package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DSM splits certificate handling across two APIs, and the split is not
// intuitive: the collection (list, set, delete) lives on the .CRT sub-API,
// while import lives on the parent. Choosing the wrong one answers 103, which
// reads like a bad method name rather than a bad API name.
//
// None of this is publicly documented. The names and parameters below follow
// acme.sh's Synology deploy hook and the python synology-api client, both of
// which are in production use against DSM 7.
const (
	certificateAPI            = "SYNO.Core.Certificate"
	certificateCRTAPI         = "SYNO.Core.Certificate.CRT"
	certificateLetsEncryptAPI = "SYNO.Core.Certificate.LetsEncrypt"
)

// ErrCertificateNotFound reports that DSM has no certificate with the given id
// or description, so a resource can drop it from state instead of failing.
var ErrCertificateNotFound = &NotFoundError{Kind: "DSM certificate"}

// DSM runs the whole ACME exchange inline inside the create request rather than
// handing back a task id, so issuance is a single long-blocking call. These are
// variables so unit tests can shorten them.
var (
	letsEncryptTimeout = 10 * time.Minute

	// After an import DSM may restart its web server (the response carries
	// restart_httpd), which drops the connection and refuses the next few
	// requests. The certificate is read back through this budget rather than
	// once, so a successful import is not reported as a failure.
	certificateSettleInterval = 2 * time.Second
	certificateSettleTimeout  = 90 * time.Second
)

// Certificate is a certificate installed on DSM.
//
// ValidFrom/ValidTill carry DSM's own rendering of the validity window. It is
// OpenSSL's asctime text ("Nov 27 09:44:00 2026 GMT", with a space-padded day),
// not a machine format, which is why callers that hold the PEM prefer
// CertificateExpiry over it.
type Certificate struct {
	ID              string
	Description     string
	Subject         string
	SubjectAltNames []string
	Issuer          string
	ValidFrom       string
	ValidTill       string
	IsDefault       bool
	SelfSigned      bool
	Renewable       bool
	Broken          bool
	KeyTypes        string
	Services        []CertificateService
}

// CertificateService is one DSM service currently served by a certificate.
// A certificate that still has services attached cannot be removed without
// leaving that service without TLS, which is what the Terraform layer refuses.
type CertificateService struct {
	Service     string
	DisplayName string
	Owner       string
	IsPackage   bool
}

// ServiceNames renders the attached services for a diagnostic message,
// preferring the display name a DSM administrator would recognise from the
// Certificate control panel over the internal identifier.
func (c Certificate) ServiceNames() []string {
	names := make([]string, 0, len(c.Services))
	for _, service := range c.Services {
		switch {
		case service.DisplayName != "" && service.Service != "" && service.DisplayName != service.Service:
			names = append(names, fmt.Sprintf("%s (%s)", service.DisplayName, service.Service))
		case service.DisplayName != "":
			names = append(names, service.DisplayName)
		default:
			names = append(names, service.Service)
		}
	}
	return names
}

// ExpiresAt returns the expiry DSM reports, normalised to UTC.
func (c Certificate) ExpiresAt() (time.Time, error) {
	return ParseDSMCertificateTime(c.ValidTill)
}

// ImportCertificateRequest is a bring-your-own certificate upload.
//
// ID is empty when installing a new certificate and set to an existing
// certificate id when replacing its material in place. DSM deduplicates on this
// id — there is no "certificate already exists" error — so passing the existing
// id is what keeps a renewed certificate attached to its services instead of
// creating a second entry beside it.
type ImportCertificateRequest struct {
	ID           string
	Description  string
	Certificate  string
	PrivateKey   string
	Intermediate string
	SetAsDefault bool
}

// LetsEncryptRequest asks DSM to obtain a certificate over ACME.
type LetsEncryptRequest struct {
	Domain      string
	AltNames    []string
	Email       string
	Description string
}

// ListCertificates returns every certificate installed on DSM.
func (c *Client) ListCertificates(ctx context.Context) ([]Certificate, error) {
	// POST rather than GET: it is the shape acme.sh uses in production against
	// this API, and DoAPIPost puts _sid and SynoToken in the query string, which
	// every certificate call requires. A certificate request without SynoToken
	// comes back as 103 and looks like a wrong method name.
	data, err := c.DoAPIPost(ctx, certificateCRTAPI, "1", "list", nil)
	if err != nil {
		return nil, fmt.Errorf("list certificates: %w", err)
	}

	certificates, err := parseCertificateList(data)
	if err != nil {
		return nil, fmt.Errorf("parse certificate list: %w", err)
	}
	return certificates, nil
}

// GetCertificate looks a certificate up by the id DSM assigned it.
//
// There is no per-certificate read: the CRT API exposes the collection only, so
// every read is a list plus a filter. That is also why service assignments come
// for free — they are part of the same list entry.
func (c *Client) GetCertificate(ctx context.Context, id string) (*Certificate, error) {
	certificates, err := c.ListCertificates(ctx)
	if err != nil {
		return nil, err
	}
	for i := range certificates {
		if certificates[i].ID == id {
			return &certificates[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrCertificateNotFound, id)
}

// GetCertificateByDescription looks a certificate up by the description shown
// in the DSM control panel. DSM does not enforce uniqueness on it, so an
// ambiguous match is an error rather than an arbitrary pick.
func (c *Client) GetCertificateByDescription(ctx context.Context, description string) (*Certificate, error) {
	certificates, err := c.ListCertificates(ctx)
	if err != nil {
		return nil, err
	}
	return matchCertificateByDescription(certificates, description)
}

// GetCertificateBySubject looks a certificate up by its subject common name.
// It exists for one specific case: DSM's own Let's Encrypt renewal can replace
// a certificate with a new object, at which point the id in Terraform state is
// stale and the common name is the only stable handle left.
func (c *Client) GetCertificateBySubject(ctx context.Context, commonName string) (*Certificate, error) {
	// An empty common name would match every certificate whose subject object
	// DSM omitted, handing back an arbitrary one under the caller's id. There is
	// no sensible answer to "find the certificate named nothing".
	if commonName == "" {
		return nil, fmt.Errorf("%w: cannot look a certificate up by an empty subject common name", ErrCertificateNotFound)
	}

	certificates, err := c.ListCertificates(ctx)
	if err != nil {
		return nil, err
	}
	for i := range certificates {
		if certificates[i].Subject == commonName {
			return &certificates[i], nil
		}
	}
	return nil, fmt.Errorf("%w: subject %q", ErrCertificateNotFound, commonName)
}

func matchCertificateByDescription(certificates []Certificate, description string) (*Certificate, error) {
	var matches []Certificate
	for _, certificate := range certificates {
		if certificate.Description == description {
			matches = append(matches, certificate)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: description %q", ErrCertificateNotFound, description)
	case 1:
		return &matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.ID)
		}
		return nil, fmt.Errorf("description %q matches %d certificates (%s); DSM does not require descriptions to be unique. "+
			"If this appeared after a failed apply, the earlier attempt probably installed a certificate before timing out — "+
			"delete the duplicate in Control Panel > Security > Certificate, or import the one to keep with `terraform import <address> <id>`",
			description, len(matches), strings.Join(ids, ", "))
	}
}

// ImportCertificate uploads certificate material and returns the resulting
// certificate.
//
// The request is multipart/form-data because DSM takes the PEM blobs as file
// parts. It reuses the transport behind UploadFile — which already gets right
// the two things that are easy to get wrong, namely repeating api/version/
// method in the query string and keeping _sid and SynoToken out of the body —
// but not its part ordering: acme.sh sends the files first and the text fields
// after, and that is the ordering with a production track record here.
func (c *Client) ImportCertificate(ctx context.Context, req ImportCertificateRequest) (*Certificate, error) {
	if strings.TrimSpace(req.Certificate) == "" {
		return nil, errors.New("import certificate: the certificate PEM is empty")
	}
	if strings.TrimSpace(req.PrivateKey) == "" {
		return nil, errors.New("import certificate: the private key PEM is empty")
	}

	parts := []multipartPart{
		filePart("key", "key.pem", []byte(req.PrivateKey)),
		filePart("cert", "cert.pem", []byte(req.Certificate)),
	}
	if strings.TrimSpace(req.Intermediate) != "" {
		parts = append(parts, filePart("inter_cert", "chain.pem", []byte(req.Intermediate)))
	}
	// An empty id means "install a new certificate"; a non-empty one replaces the
	// material of an existing certificate. DSM expects the field either way.
	parts = append(parts, textPart("id", req.ID), textPart("desc", req.Description))
	if req.SetAsDefault {
		// Only sent when true. There is no way to clear the default this way —
		// some other certificate has to claim it — so a false would be meaningless
		// at best and destructive at worst.
		parts = append(parts, textPart("as_default", "true"))
	}

	data, err := c.multipartRequest(ctx, certificateAPI, "1", "import", parts)
	if err != nil {
		return nil, fmt.Errorf("import certificate: %w", err)
	}

	id := importedCertificateID(data)
	if id == "" {
		id = req.ID
	}

	certificate, err := c.settleCertificate(ctx, id, req.Description)
	if err != nil {
		return nil, fmt.Errorf("import certificate: %w", err)
	}
	return certificate, nil
}

// settleCertificate reads an imported certificate back, tolerating the web
// server restart DSM performs when the import touched a certificate DSM itself
// serves (the import response says so with restart_httpd). During that window
// requests fail at the transport level or the certificate is briefly absent.
func (c *Client) settleCertificate(ctx context.Context, id, description string) (*Certificate, error) {
	deadline := time.NewTimer(certificateSettleTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(certificateSettleInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		certificates, err := c.ListCertificates(ctx)
		if err != nil {
			lastErr = err
		} else {
			if id != "" {
				for i := range certificates {
					if certificates[i].ID == id {
						return &certificates[i], nil
					}
				}
			}
			// A fresh import does not tell us the id on every DSM build, so the
			// description is the fallback handle. It is good enough here because the
			// caller chose it moments ago.
			match, matchErr := matchCertificateByDescription(certificates, description)
			if matchErr == nil {
				return match, nil
			}
			lastErr = matchErr
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			// DSM already accepted the material, so the certificate probably does
			// exist — this is a read-back failure, not an install failure. Saying so
			// matters: a caller that retries a create with an empty id installs a
			// second certificate under the same description, and from then on every
			// lookup is ambiguous and the resource can never be created.
			return nil, fmt.Errorf("DSM accepted the certificate but it did not become readable within %s: %w.\n\n"+
				"The certificate has most likely been installed regardless — DSM restarts its web server after an import, and this "+
				"budget covers that restart. Check Control Panel > Security > Certificate before applying again: if a certificate "+
				"named %q is there, adopt it with `terraform import <address> <id>` rather than re-running create, which would "+
				"install a second copy under the same name and make every later lookup ambiguous",
				certificateSettleTimeout, lastErr, description)
		case <-ticker.C:
		}
	}
}

// CreateLetsEncryptCertificate asks DSM to obtain a certificate over ACME.
//
// DSM does the whole exchange inside this request — DNS resolution, the inbound
// HTTP-01 challenge, and the CA round trip — and answers only when it is done
// or has failed. There is no task id to poll, so the call simply blocks; tens
// of seconds is normal and the budget here is minutes.
func (c *Client) CreateLetsEncryptCertificate(ctx context.Context, req LetsEncryptRequest) (*Certificate, error) {
	if req.Domain == "" {
		return nil, errors.New("create Let's Encrypt certificate: domain is required")
	}
	if req.Email == "" {
		return nil, errors.New("create Let's Encrypt certificate: email is required; Let's Encrypt registers the ACME account against it")
	}

	params := url.Values{}
	// Every name goes into domain_name as a semicolon-separated list with the
	// common name first. DSM has no separate parameter for subject alternative
	// names, despite what several third-party write-ups claim.
	params.Set("domain_name", strings.Join(append([]string{req.Domain}, req.AltNames...), ";"))
	params.Set("email", req.Email)
	params.Set("desc", req.Description)

	issueCtx, cancel := context.WithTimeout(ctx, letsEncryptTimeout)
	defer cancel()

	if _, err := c.DoAPIPost(issueCtx, certificateLetsEncryptAPI, "1", "create", params); err != nil {
		return nil, fmt.Errorf("create Let's Encrypt certificate for %q: %w", req.Domain, err)
	}

	// DSM does not return the new certificate, so it has to be found. The
	// description was chosen by the caller and is the reliable handle; the common
	// name is the fallback for a DSM build that ignores desc on this API.
	certificates, err := c.ListCertificates(ctx)
	if err != nil {
		return nil, fmt.Errorf("create Let's Encrypt certificate for %q: %w", req.Domain, err)
	}
	if certificate, matchErr := matchCertificateByDescription(certificates, req.Description); matchErr == nil {
		return certificate, nil
	}
	for i := range certificates {
		if certificates[i].Subject == req.Domain {
			return &certificates[i], nil
		}
	}
	return nil, fmt.Errorf("create Let's Encrypt certificate for %q: %w: DSM reported success but no matching certificate is listed",
		req.Domain, ErrCertificateNotFound)
}

// SetCertificateAttributes renames a certificate and optionally makes it the
// DSM default.
//
// DSM wants desc and id as JSON-quoted strings here — the same quirk this
// client already handles for SYNO.Docker.Project — while as_default is a raw
// value. as_default is omitted rather than sent as false when the certificate
// is not meant to become the default: DSM always has exactly one default, and
// there is no documented way to ask for none.
func (c *Client) SetCertificateAttributes(ctx context.Context, id, description string, asDefault bool) error {
	params := url.Values{}
	params.Set("id", jsonQuoted(id))
	params.Set("desc", jsonQuoted(description))
	if asDefault {
		params.Set("as_default", "true")
	}

	if _, err := c.DoAPIPost(ctx, certificateCRTAPI, "1", "set", params); err != nil {
		return fmt.Errorf("update certificate %q: %w", id, err)
	}
	return nil
}

// SetDefaultCertificate makes a certificate the one DSM serves for anything
// without an explicit assignment.
func (c *Client) SetDefaultCertificate(ctx context.Context, id, description string) error {
	return c.SetCertificateAttributes(ctx, id, description, true)
}

// DeleteCertificate removes a certificate. DSM takes a JSON array of ids even
// for a single certificate, in the same way as user and group deletion.
func (c *Client) DeleteCertificate(ctx context.Context, id string) error {
	params := url.Values{}
	params.Set("ids", jsonStringArray(id))

	if _, err := c.DoAPIPost(ctx, certificateCRTAPI, "1", "delete", params); err != nil {
		return fmt.Errorf("delete certificate %q: %w", id, err)
	}
	return nil
}

func importedCertificateID(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return ""
	}
	return result.ID
}

func parseCertificateList(data json.RawMessage) ([]Certificate, error) {
	var result struct {
		Certificates []json.RawMessage `json:"certificates"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	certificates := make([]Certificate, 0, len(result.Certificates))
	for _, raw := range result.Certificates {
		certificate, err := parseCertificate(raw)
		if err != nil {
			// A single unreadable entry must not take the rest of the list with it,
			// but it must not disappear either: an entry silently dropped here reads
			// downstream as "the certificate no longer exists", which makes Terraform
			// destroy and recreate a certificate that is in fact fine. Keeping it with
			// whatever id could be recovered means it still matches by id and the
			// resource survives.
			if id := rawCertificateID(raw); id != "" {
				certificates = append(certificates, Certificate{ID: id})
			}
			continue
		}
		if certificate.ID == "" {
			continue
		}
		certificates = append(certificates, *certificate)
	}
	return certificates, nil
}

// rawCertificateID recovers an id from an entry that failed to parse, and
// accepts the shapes DSM might use for it. stringValue alone would answer ""
// for a build that renders the id as a number, and an empty id is treated as
// "no such certificate" everywhere upstream.
func rawCertificateID(raw json.RawMessage) string {
	var entry struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return ""
	}
	return flexibleString(entry.ID)
}

// flexibleString renders a JSON scalar as a string. DSM is inconsistent about
// quoting ids, and a numeric id silently becoming "" is the kind of bug that
// only shows up on somebody else's DSM build.
func flexibleString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	return ""
}

// parseCertificate unpacks one list entry. Like the rest of this client it
// works on map[string]interface{} rather than a typed struct: the DSM response
// is loose, several service fields are absent rather than null when unset, and
// the shape differs between DSM and SRM.
func parseCertificate(raw json.RawMessage) (*Certificate, error) {
	var entry map[string]interface{}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, err
	}

	certificate := &Certificate{
		// The id is the identity of the resource, so it is read leniently: a DSM
		// build that renders it as a number would otherwise leave every entry with
		// an empty id, which upstream reads as "the certificate is gone".
		ID:          flexibleStringValue(entry, "id"),
		Description: stringValue(entry, "desc"),
		ValidFrom:   stringValue(entry, "valid_from"),
		ValidTill:   stringValue(entry, "valid_till"),
		IsDefault:   flexibleBool(entry["is_default"]),
		Renewable:   flexibleBool(entry["renewable"]),
		Broken:      flexibleBool(entry["is_broken"]),
		KeyTypes:    stringValue(entry, "key_types"),
	}

	if subject, ok := entry["subject"].(map[string]interface{}); ok {
		certificate.Subject = stringValue(subject, "common_name")
		if altNames, ok := subject["sub_alt_name"].([]interface{}); ok {
			for _, altName := range altNames {
				if name, ok := altName.(string); ok && name != "" {
					certificate.SubjectAltNames = append(certificate.SubjectAltNames, name)
				}
			}
		}
	}
	if issuer, ok := entry["issuer"].(map[string]interface{}); ok {
		certificate.Issuer = stringValue(issuer, "common_name")
	}

	// There is no self_signed flag on the wire. DSM marks its own built-in
	// certificate with a nested self_signed_cacrt_info object; for everything
	// else, a certificate that issued itself is one whose issuer and subject
	// common names agree.
	_, hasSelfSignedInfo := entry["self_signed_cacrt_info"]
	certificate.SelfSigned = hasSelfSignedInfo ||
		(certificate.Issuer != "" && certificate.Issuer == certificate.Subject)

	if services, ok := entry["services"].([]interface{}); ok {
		for _, rawService := range services {
			service, ok := rawService.(map[string]interface{})
			if !ok {
				continue
			}
			certificate.Services = append(certificate.Services, CertificateService{
				Service:     stringValue(service, "service"),
				DisplayName: stringValue(service, "display_name"),
				Owner:       stringValue(service, "owner"),
				// DSM spells this one in camelCase, unlike every neighbouring field.
				IsPackage: flexibleBool(service["isPkg"]),
			})
		}
	}

	return certificate, nil
}

// flexibleBool accepts the several ways DSM renders a boolean in this API:
// a real JSON bool, "true"/"false", and 0/1.
func flexibleBool(value interface{}) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(typed, "true") || typed == "1"
	case float64:
		return typed != 0
	default:
		return false
	}
}

// flexibleStringValue reads a map value that ought to be a string but might not
// be, rendering a number without the float formatting Go would otherwise apply.
func flexibleStringValue(object map[string]interface{}, key string) string {
	switch typed := object[key].(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

// jsonQuoted lives in event_scheduler.go — the certificate set method wants its
// id and desc in exactly the same JSON-quoted form.

// certificateServiceAPI binds certificates to individual DSM services.
// Reconstructed from the python synology-api client's set_certificate_for_service,
// which is in production use against DSM 7: the call takes a JSON array named
// "settings", one entry per binding change, each carrying the full service
// object exactly as the certificate list returned it, plus the certificate ids
// to move it from (old_id) and to (id). Sending a hand-built service object is
// rejected — DSM wants its own shape back.
const certificateServiceAPI = "SYNO.Core.Certificate.Service"

// ErrCertificateServiceNotFound reports that no installed certificate is
// currently serving the named service, so the binding resource can treat it as
// "not bound yet" rather than an error.
var ErrCertificateServiceNotFound = &NotFoundError{Kind: "DSM certificate service binding"}

// CertificateServiceBinding is the certificate currently serving a service.
type CertificateServiceBinding struct {
	// CertificateID is the DSM certificate id bound to the service.
	CertificateID string
	// Service is the raw service object exactly as DSM returned it inside the
	// certificate list. Writes must send this object back unchanged.
	Service map[string]interface{}
}

// FindCertificateServiceBinding looks through every installed certificate for
// the service with the given internal identifier (the "service" field, e.g.
// "default" for the DSM web UI or "synology.at.caddy" for a reverse-proxy
// entry). Returns the owning certificate id and the raw service object.
func (c *Client) FindCertificateServiceBinding(ctx context.Context, serviceID string) (*CertificateServiceBinding, error) {
	body, err := c.DoAPI(ctx, certificateAPI, "1", "list", url.Values{})
	if err != nil {
		return nil, fmt.Errorf("list certificates: %w", err)
	}

	var result struct {
		Certificates []struct {
			ID       string                   `json:"id"`
			Services []map[string]interface{} `json:"services"`
		} `json:"certificates"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode certificate list: %w", err)
	}

	for _, certificate := range result.Certificates {
		for _, service := range certificate.Services {
			if stringValue(service, "service") == serviceID {
				return &CertificateServiceBinding{
					CertificateID: certificate.ID,
					Service:       service,
				}, nil
			}
		}
	}
	return nil, ErrCertificateServiceNotFound
}

// SetCertificateService binds a service to a certificate.
//
// DSM takes the full service object (as returned by the certificate list) plus
// the previous owner's id in old_id — omitting a service object it recognizes,
// or sending one rebuilt by hand, is rejected. A binding change also restarts
// the affected service, so this is not a cheap call.
func (c *Client) SetCertificateService(ctx context.Context, service map[string]interface{}, oldCertificateID, newCertificateID string) error {
	settings := []map[string]interface{}{
		{
			"service": service,
			"old_id":  oldCertificateID,
			"id":      newCertificateID,
		},
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode service settings: %w", err)
	}

	params := url.Values{}
	params.Set("settings", string(encoded))

	if _, err := c.DoAPIPost(ctx, certificateServiceAPI, "1", "set", params); err != nil {
		return fmt.Errorf("bind certificate %q to service: %w", newCertificateID, err)
	}
	return nil
}
