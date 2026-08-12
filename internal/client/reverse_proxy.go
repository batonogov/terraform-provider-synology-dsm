package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Reverse proxy entries live behind Control Panel → Login Portal → Advanced →
// Reverse Proxy. The API is not part of Synology's published developer guide;
// the wire contract below was reconstructed from four independent sources that
// agree with each other:
//
//   - a verified DSM 7.x capture of list/create published as API reference
//     (pmilano1/synology-dsm-api, docs/api-reference/system-services/login-portal.md)
//   - a DSM 7.3 wizard capture exported into an Ansible role (KastnerRG/krg-infra)
//   - two working Go clients (phoeluga/synology-proxy-operator,
//     ironashram/terraform-provider-synology)
//   - Synology's own header /usr/include/synow3/synow3.h, which defines the JSON
//     key names ("fqdn", "hsts", "http2", "https", "frontend", "backend", "UUID")
//
// Anything below that is inferred rather than observed on the wire is called out
// with an explicit comment.
const (
	reverseProxyAPI      = "SYNO.Core.AppPortal.ReverseProxy"
	accessControlAPI     = "SYNO.Core.AppPortal.AccessControl"
	reverseProxyProtoNum = "protocol"
)

// DSM encodes the endpoint protocol as an integer, not a string.
const (
	reverseProxyProtocolHTTP  int64 = 0
	reverseProxyProtocolHTTPS int64 = 1
)

// Canonical protocol spellings used by this provider. DSM's UI shows them
// upper-case, and the resource schema accepts either case.
const (
	ReverseProxyProtocolHTTP  = "HTTP"
	ReverseProxyProtocolHTTPS = "HTTPS"
)

var (
	ErrReverseProxyNotFound         = errors.New("reverse proxy entry not found")
	ErrAccessControlProfileNotFound = errors.New("Login Portal access control profile not found")
)

// ReverseProxyEndpoint is one side of a reverse proxy entry: DSM calls them
// "frontend" (the public source) and "backend" (the destination).
type ReverseProxyEndpoint struct {
	Protocol string
	Hostname string
	Port     int64
}

// ReverseProxyHeader is one entry of DSM's `customize_headers` array. DSM
// renders each as an nginx `proxy_set_header` directive, so values may contain
// nginx variables such as `$http_upgrade` or `$scheme`.
type ReverseProxyHeader struct {
	Name  string
	Value string
}

// ReverseProxy is a single DSM reverse proxy entry.
//
// DSM has no separate name field: the "Description" shown in the UI is stored as
// `description`, and it is the only human-readable handle an entry has. The
// stable identifier is UUID, assigned by DSM on create.
type ReverseProxy struct {
	UUID        string
	Description string
	Source      ReverseProxyEndpoint
	Destination ReverseProxyEndpoint
	HSTS        bool
	// HTTP2 is a pointer because the key is *inferred*, not observed. Synology's
	// own header defines an "http2" key next to "hsts", but no captured payload
	// contains it, so a DSM build that does not persist it must be
	// distinguishable from one that persists it as false. nil means "DSM did not
	// report this field"; callers preserve their configured value in that case
	// instead of reporting drift.
	HTTP2           *bool
	ACLProfileUUID  string
	CustomHeaders   []ReverseProxyHeader
	ConnectTimeout  int64
	ReadTimeout     int64
	SendTimeout     int64
	HTTPVersion     int64
	InterceptErrors bool

	// raw is the entry exactly as DSM returned it. Updates are applied on top of
	// it so that fields this provider does not model — DSM's internal `_key`, and
	// whatever a future DSM adds — survive a round trip instead of being dropped.
	raw map[string]interface{}
}

// ReverseProxyWebSocketHeaders returns the two headers DSM's own "WebSocket"
// custom-header preset inserts. Confirmed by phoeluga/synology-proxy-operator
// and by DSM's Application Portal UI, which has offered this preset since 6.2.1.
func ReverseProxyWebSocketHeaders() []ReverseProxyHeader {
	return []ReverseProxyHeader{
		{Name: "Upgrade", Value: "$http_upgrade"},
		{Name: "Connection", Value: "$connection_upgrade"},
	}
}

// AccessControlProfile is a Login Portal access control profile. Reverse proxy
// entries reference one by UUID through `frontend.acl`.
type AccessControlProfile struct {
	UUID string
	Name string
}

// EncodeReverseProxyProtocol maps a protocol name to DSM's integer encoding.
// Matching is case-insensitive so that both `HTTPS` and `https` are accepted.
func EncodeReverseProxyProtocol(protocol string) (int64, error) {
	switch strings.ToUpper(strings.TrimSpace(protocol)) {
	case ReverseProxyProtocolHTTP:
		return reverseProxyProtocolHTTP, nil
	case ReverseProxyProtocolHTTPS:
		return reverseProxyProtocolHTTPS, nil
	default:
		return 0, fmt.Errorf("unsupported reverse proxy protocol %q: must be HTTP or HTTPS", protocol)
	}
}

// DecodeReverseProxyProtocol maps DSM's integer encoding back to a name. An
// unknown code is an error rather than a silent default: mislabelling HTTPS as
// HTTP would silently downgrade a listener.
func DecodeReverseProxyProtocol(code int64) (string, error) {
	switch code {
	case reverseProxyProtocolHTTP:
		return ReverseProxyProtocolHTTP, nil
	case reverseProxyProtocolHTTPS:
		return ReverseProxyProtocolHTTPS, nil
	default:
		return "", fmt.Errorf("DSM reported an unknown reverse proxy protocol code %d", code)
	}
}

// ListReverseProxies returns every reverse proxy entry DSM knows about.
func (c *Client) ListReverseProxies(ctx context.Context) ([]ReverseProxy, error) {
	data, err := c.DoAPIPost(ctx, reverseProxyAPI, "1", "list", nil)
	if err != nil {
		return nil, fmt.Errorf("list reverse proxy entries: %w", err)
	}

	var result struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse reverse proxy entry list: %w", err)
	}

	entries := make([]ReverseProxy, 0, len(result.Entries))
	for _, raw := range result.Entries {
		entry, err := parseReverseProxy(raw)
		if err != nil {
			return nil, fmt.Errorf("parse reverse proxy entry: %w", err)
		}
		entries = append(entries, *entry)
	}
	return entries, nil
}

// GetReverseProxy looks an entry up by UUID. DSM exposes no per-entry `get`
// method, so this filters the list.
func (c *Client) GetReverseProxy(ctx context.Context, uuid string) (*ReverseProxy, error) {
	return c.getReverseProxy(ctx, uuid)
}

// GetReverseProxyByDescription looks an entry up by its DSM description, which
// is what the UI labels "Description" and what a data source can address.
func (c *Client) GetReverseProxyByDescription(ctx context.Context, description string) (*ReverseProxy, error) {
	return c.findReverseProxyByDescription(ctx, description)
}

func (c *Client) getReverseProxy(ctx context.Context, uuid string) (*ReverseProxy, error) {
	if uuid == "" {
		return nil, fmt.Errorf("%w: empty UUID", ErrReverseProxyNotFound)
	}
	entries, err := c.ListReverseProxies(ctx)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].UUID == uuid {
			return &entries[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrReverseProxyNotFound, uuid)
}

func (c *Client) findReverseProxyByDescription(ctx context.Context, description string) (*ReverseProxy, error) {
	entries, err := c.ListReverseProxies(ctx)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].Description == description {
			return &entries[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrReverseProxyNotFound, description)
}

// CreateReverseProxy adds an entry and returns it as DSM stored it.
//
// DSM's create answers `{"success": true}` and does not reliably echo the new
// UUID, so the entry is read back afterwards. That read-after-write, plus the
// duplicate check before it, is why the whole sequence runs under
// reverseProxyMu — see the field comment in client.go.
func (c *Client) CreateReverseProxy(ctx context.Context, entry ReverseProxy) (*ReverseProxy, error) {
	c.reverseProxyMu.Lock()
	defer c.reverseProxyMu.Unlock()

	if existing, err := c.findReverseProxyByDescription(ctx, entry.Description); err == nil {
		return nil, fmt.Errorf("reverse proxy entry %q already exists (UUID %s); import it instead", entry.Description, existing.UUID)
	} else if !errors.Is(err, ErrReverseProxyNotFound) {
		return nil, err
	}

	entry.UUID = ""
	entry.raw = nil
	payload, err := entry.encodeEntry()
	if err != nil {
		return nil, err
	}

	data, err := c.DoAPIPost(ctx, reverseProxyAPI, "1", "create", reverseProxyEntryParams(payload))
	if err != nil {
		// 4154 means the entry already exists. A create whose response was lost
		// (proxy timeout, dropped connection) can still have landed, and the retry
		// then reports 4154 for an entry this call itself created — so adopt it
		// rather than failing an apply that actually succeeded.
		if IsAPIError(err, 4154) {
			if created, findErr := c.findReverseProxyByDescription(ctx, entry.Description); findErr == nil {
				return created, nil
			}
		}
		return nil, fmt.Errorf("create reverse proxy entry %q: %w", entry.Description, err)
	}

	if uuid := reverseProxyUUIDFromResponse(data); uuid != "" {
		return c.getReverseProxy(ctx, uuid)
	}
	created, err := c.findReverseProxyByDescription(ctx, entry.Description)
	if err != nil {
		return nil, fmt.Errorf("read back reverse proxy entry %q after create: %w", entry.Description, err)
	}
	return created, nil
}

// UpdateReverseProxy rewrites an existing entry in place. DSM identifies the
// target by the UUID carried inside the entry payload.
func (c *Client) UpdateReverseProxy(ctx context.Context, entry ReverseProxy) (*ReverseProxy, error) {
	c.reverseProxyMu.Lock()
	defer c.reverseProxyMu.Unlock()

	if entry.UUID == "" {
		return nil, errors.New("update reverse proxy entry: UUID is required")
	}

	// Read the stored entry first so unmodelled DSM fields (`_key` and friends)
	// are carried into the payload instead of being dropped.
	current, err := c.getReverseProxy(ctx, entry.UUID)
	if err != nil {
		return nil, err
	}
	entry.raw = current.raw

	payload, err := entry.encodeEntry()
	if err != nil {
		return nil, err
	}

	if _, err := c.DoAPIPost(ctx, reverseProxyAPI, "1", "update", reverseProxyEntryParams(payload)); err != nil {
		return nil, fmt.Errorf("update reverse proxy entry %q: %w", entry.UUID, err)
	}
	return c.getReverseProxy(ctx, entry.UUID)
}

// DeleteReverseProxy removes an entry. DSM's delete takes a JSON array of
// UUIDs even when only one entry is removed.
func (c *Client) DeleteReverseProxy(ctx context.Context, uuid string) error {
	c.reverseProxyMu.Lock()
	defer c.reverseProxyMu.Unlock()

	if _, err := c.getReverseProxy(ctx, uuid); errors.Is(err, ErrReverseProxyNotFound) {
		return nil
	} else if err != nil {
		return err
	}

	uuids, err := json.Marshal([]string{uuid})
	if err != nil {
		return fmt.Errorf("encode reverse proxy UUID list: %w", err)
	}
	params := url.Values{}
	params.Set("uuids", string(uuids))

	if _, err := c.DoAPIPost(ctx, reverseProxyAPI, "1", "delete", params); err != nil {
		return fmt.Errorf("delete reverse proxy entry %q: %w", uuid, err)
	}
	return nil
}

// ListAccessControlProfiles returns the Login Portal access control profiles a
// reverse proxy entry can reference.
func (c *Client) ListAccessControlProfiles(ctx context.Context) ([]AccessControlProfile, error) {
	data, err := c.DoAPIPost(ctx, accessControlAPI, "1", "list", nil)
	if err != nil {
		return nil, fmt.Errorf("list access control profiles: %w", err)
	}

	var result struct {
		Entries []map[string]interface{} `json:"entries"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse access control profile list: %w", err)
	}

	profiles := make([]AccessControlProfile, 0, len(result.Entries))
	for _, object := range result.Entries {
		profile := AccessControlProfile{
			UUID: stringValue(object, "UUID"),
			Name: stringValue(object, "name"),
		}
		if profile.UUID == "" {
			profile.UUID = stringValue(object, "uuid")
		}
		if profile.UUID != "" {
			profiles = append(profiles, profile)
		}
	}
	return profiles, nil
}

// FindAccessControlProfileByName resolves the profile name shown in the DSM UI
// to the UUID a reverse proxy entry stores.
func (c *Client) FindAccessControlProfileByName(ctx context.Context, name string) (*AccessControlProfile, error) {
	profiles, err := c.ListAccessControlProfiles(ctx)
	if err != nil {
		return nil, err
	}
	for i := range profiles {
		if profiles[i].Name == name {
			return &profiles[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrAccessControlProfileNotFound, name)
}

// FindAccessControlProfileByUUID resolves a stored UUID back to its name so
// that Read can report the profile the way the configuration names it.
func (c *Client) FindAccessControlProfileByUUID(ctx context.Context, uuid string) (*AccessControlProfile, error) {
	profiles, err := c.ListAccessControlProfiles(ctx)
	if err != nil {
		return nil, err
	}
	for i := range profiles {
		if profiles[i].UUID == uuid {
			return &profiles[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrAccessControlProfileNotFound, uuid)
}

// reverseProxyEntryParams wraps the entry as DSM expects it: a single form
// value holding the JSON document. Flattening the object into separate
// parameters is rejected with error 4151.
func reverseProxyEntryParams(payload map[string]interface{}) url.Values {
	raw, _ := json.Marshal(payload)
	params := url.Values{}
	params.Set("entry", string(raw))
	return params
}

func reverseProxyUUIDFromResponse(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var object map[string]interface{}
	if err := json.Unmarshal(data, &object); err != nil {
		return ""
	}
	if uuid := stringValue(object, "UUID"); uuid != "" {
		return uuid
	}
	return stringValue(object, "uuid")
}

// encodeEntry renders the entry into DSM's JSON shape, layering the managed
// fields over whatever DSM previously stored.
func (p ReverseProxy) encodeEntry() (map[string]interface{}, error) {
	sourceProtocol, err := EncodeReverseProxyProtocol(p.Source.Protocol)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	destinationProtocol, err := EncodeReverseProxyProtocol(p.Destination.Protocol)
	if err != nil {
		return nil, fmt.Errorf("destination: %w", err)
	}

	entry := copyJSONObject(p.raw)
	if p.UUID != "" {
		entry["UUID"] = p.UUID
	} else {
		delete(entry, "UUID")
	}
	entry["description"] = p.Description
	entry["proxy_connect_timeout"] = p.ConnectTimeout
	entry["proxy_read_timeout"] = p.ReadTimeout
	entry["proxy_send_timeout"] = p.SendTimeout
	// proxy_http_version is the HTTP version DSM speaks to the destination — not
	// an HTTP/2 toggle for the listener, despite the name. This provider does not
	// model it, so keep whatever DSM already stored and fall back to 1 (the value
	// in every captured payload) for a new entry.
	httpVersion := p.HTTPVersion
	if httpVersion == 0 {
		httpVersion = int64Value(entry, "proxy_http_version")
	}
	if httpVersion == 0 {
		httpVersion = 1
	}
	entry["proxy_http_version"] = httpVersion
	entry["proxy_intercept_errors"] = p.InterceptErrors

	frontend := copyJSONObject(objectValue(entry, "frontend"))
	frontend["fqdn"] = p.Source.Hostname
	frontend["port"] = p.Source.Port
	frontend[reverseProxyProtoNum] = sourceProtocol
	// DSM stores the access control profile as a UUID under `frontend.acl`, and
	// uses JSON null when no profile is attached.
	if p.ACLProfileUUID != "" {
		frontend["acl"] = p.ACLProfileUUID
	} else {
		frontend["acl"] = nil
	}
	https := copyJSONObject(objectValue(frontend, "https"))
	https["hsts"] = p.HSTS
	// INFERRED: "http2" is defined as a JSON key by Synology's synow3.h next to
	// "hsts" and "https", but no captured reverse proxy payload contains it, so
	// the nesting under frontend.https is deduction rather than observation. It
	// is only sent when the caller set it, so a DSM build that rejects or ignores
	// the key never sees it unless the user asks for HTTP/2.
	if p.HTTP2 != nil {
		https["http2"] = *p.HTTP2
	}
	frontend["https"] = https
	entry["frontend"] = frontend

	backend := copyJSONObject(objectValue(entry, "backend"))
	backend["fqdn"] = p.Destination.Hostname
	backend["port"] = p.Destination.Port
	backend[reverseProxyProtoNum] = destinationProtocol
	entry["backend"] = backend

	headers := make([]map[string]interface{}, 0, len(p.CustomHeaders))
	for _, header := range p.CustomHeaders {
		headers = append(headers, map[string]interface{}{"name": header.Name, "value": header.Value})
	}
	entry["customize_headers"] = headers

	return entry, nil
}

func parseReverseProxy(raw json.RawMessage) (*ReverseProxy, error) {
	var object map[string]interface{}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	return parseReverseProxyMap(object)
}

func parseReverseProxyMap(object map[string]interface{}) (*ReverseProxy, error) {
	frontend := objectValue(object, "frontend")
	backend := objectValue(object, "backend")

	sourceProtocol, err := DecodeReverseProxyProtocol(int64Value(frontend, reverseProxyProtoNum))
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	destinationProtocol, err := DecodeReverseProxyProtocol(int64Value(backend, reverseProxyProtoNum))
	if err != nil {
		return nil, fmt.Errorf("destination: %w", err)
	}

	uuid := stringValue(object, "UUID")
	if uuid == "" {
		uuid = stringValue(object, "uuid")
	}

	https := objectValue(frontend, "https")
	entry := &ReverseProxy{
		UUID:        uuid,
		Description: stringValue(object, "description"),
		Source: ReverseProxyEndpoint{
			Protocol: sourceProtocol,
			Hostname: stringValue(frontend, "fqdn"),
			Port:     int64Value(frontend, "port"),
		},
		Destination: ReverseProxyEndpoint{
			Protocol: destinationProtocol,
			Hostname: stringValue(backend, "fqdn"),
			Port:     int64Value(backend, "port"),
		},
		HSTS:            boolValue(https, "hsts"),
		HTTP2:           boolPointerValue(https, "http2"),
		ACLProfileUUID:  parseAccessControlReference(frontend["acl"]),
		ConnectTimeout:  int64Value(object, "proxy_connect_timeout"),
		ReadTimeout:     int64Value(object, "proxy_read_timeout"),
		SendTimeout:     int64Value(object, "proxy_send_timeout"),
		HTTPVersion:     int64Value(object, "proxy_http_version"),
		InterceptErrors: boolValue(object, "proxy_intercept_errors"),
		raw:             object,
	}

	if values, ok := object["customize_headers"].([]interface{}); ok {
		for _, value := range values {
			header, ok := value.(map[string]interface{})
			if !ok {
				continue
			}
			name := stringValue(header, "name")
			if name == "" {
				continue
			}
			entry.CustomHeaders = append(entry.CustomHeaders, ReverseProxyHeader{
				Name:  name,
				Value: stringValue(header, "value"),
			})
		}
	}

	return entry, nil
}

// parseAccessControlReference tolerates the shapes `frontend.acl` has been seen
// in: JSON null when unset, and a UUID string when a profile is attached. The
// object form is defensive — DSM has not been observed returning one, but
// accepting it costs nothing and avoids losing the reference if it appears.
func parseAccessControlReference(value interface{}) string {
	switch acl := value.(type) {
	case string:
		return acl
	case map[string]interface{}:
		if uuid := stringValue(acl, "UUID"); uuid != "" {
			return uuid
		}
		return stringValue(acl, "uuid")
	default:
		return ""
	}
}

func copyJSONObject(object map[string]interface{}) map[string]interface{} {
	copied := make(map[string]interface{}, len(object)+8)
	for key, value := range object {
		copied[key] = value
	}
	return copied
}

func objectValue(object map[string]interface{}, key string) map[string]interface{} {
	nested, _ := object[key].(map[string]interface{})
	return nested
}

// int64Value reads a JSON number. encoding/json decodes every number into a
// float64 when the target is interface{}, which is why this is not a plain
// type assertion to int64.
func int64Value(object map[string]interface{}, key string) int64 {
	switch value := object[key].(type) {
	case float64:
		return int64(value)
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

// boolPointerValue distinguishes "DSM reported false" from "DSM did not report
// this field at all".
func boolPointerValue(object map[string]interface{}, key string) *bool {
	value, ok := object[key].(bool)
	if !ok {
		return nil
	}
	return &value
}
