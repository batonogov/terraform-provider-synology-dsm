package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newCertificateTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")
	return c
}

func writeCertAPIResponse(w http.ResponseWriter, data interface{}) {
	raw, _ := json.Marshal(data)
	_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
}

func writeCertAPIError(w http.ResponseWriter, code int) {
	_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: code}})
}

// isCertificateListPost recognises the form-encoded list call, which the import
// tests have to answer alongside the multipart import itself.
func isCertificateListPost(r *http.Request) bool {
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		return false
	}
	if err := r.ParseForm(); err != nil {
		return false
	}
	return r.PostForm.Get("api") == certificateCRTAPI
}

// generateTestCertificate mints a real, self-signed certificate so the expiry
// tests exercise actual DER parsing rather than a hand-written string. Anything
// less would test the fixture, not the code that has to survive whatever a CA
// emits.
func generateTestCertificate(t *testing.T, commonName string, notAfter time.Time) (certPEM, keyPEM string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     []string{commonName},
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// TestCertificateExpiry_ReadsNotAfterFromRealCertificate is the core of the
// "alert on expires_at" requirement: the date has to come out of the DER, so it
// stays correct no matter how DSM chooses to render it.
func TestCertificateExpiry_ReadsNotAfterFromRealCertificate(t *testing.T) {
	// Whole seconds only: ASN.1 UTCTime has no sub-second precision.
	want := time.Date(2027, time.March, 14, 1, 59, 26, 0, time.UTC)
	certPEM, _ := generateTestCertificate(t, "wildcard.example.com", want)

	got, err := CertificateExpiry(certPEM)
	if err != nil {
		t.Fatalf("CertificateExpiry failed: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("expiry = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Location() != time.UTC {
		t.Errorf("expiry location = %s, want UTC so the value is comparable across machines", got.Location())
	}
}

// testCA is a working certificate authority, so the chain tests exercise real
// issuer/subject relationships instead of two unrelated self-signed blobs.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newTestCA(t *testing.T, commonName string, notAfter time.Time) testCA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notAfter.Add(-3650 * 24 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return testCA{
		cert: parsed,
		key:  key,
		pem:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

// issue mints an end-entity certificate signed by the CA.
func (ca testCA) issue(t *testing.T, commonName string, notAfter time.Time) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: commonName},
		DNSNames:              []string{commonName},
		NotBefore:             notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("issue leaf certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestCertificateExpiry_PicksTheLeafInEitherOrder is the reason the leaf is
// identified structurally rather than positionally.
//
// `cat cert.pem chain.pem` and `cat chain.pem cert.pem` are both common ways to
// build a bundle, and the second one would report the CA's expiry — nine years
// out here — if the first block were simply trusted. `expires_at` is the value
// the README tells people to alert on, so a silent nine-year answer means the
// alert never fires.
func TestCertificateExpiry_PicksTheLeafInEitherOrder(t *testing.T) {
	leafExpiry := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	caExpiry := time.Date(2035, time.January, 1, 12, 0, 0, 0, time.UTC)

	ca := newTestCA(t, "Example CA R3", caExpiry)
	leafPEM := ca.issue(t, "cloud.example.com", leafExpiry)

	tests := []struct {
		name   string
		bundle string
	}{
		{"leaf first, as fullchain.pem is written", leafPEM + ca.pem},
		{"chain first, as cat chain.pem cert.pem produces", ca.pem + leafPEM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CertificateExpiry(tt.bundle)
			if err != nil {
				t.Fatalf("CertificateExpiry failed: %v", err)
			}
			if !got.Equal(leafExpiry) {
				t.Errorf("expiry = %s, want the leaf expiry %s; reporting the CA's date makes an expiry alert useless",
					got.Format(time.RFC3339), leafExpiry.Format(time.RFC3339))
			}
		})
	}
}

// TestLeafCertificate_SingleSelfSignedCertificate checks the degenerate case
// the structural rule could get wrong: a self-signed certificate is its own
// issuer, and must not exclude itself from being the leaf.
func TestLeafCertificate_SingleSelfSignedCertificate(t *testing.T) {
	expiry := time.Date(2030, time.June, 1, 0, 0, 0, 0, time.UTC)
	certPEM, _ := generateTestCertificate(t, "nas.internal", expiry)

	got, err := CertificateExpiry(certPEM)
	if err != nil {
		t.Fatalf("CertificateExpiry failed: %v", err)
	}
	if !got.Equal(expiry) {
		t.Errorf("expiry = %s, want %s", got, expiry)
	}
}

func TestCertificateExpiry_SkipsNonCertificateBlocks(t *testing.T) {
	expiry := time.Date(2026, time.December, 25, 0, 0, 0, 0, time.UTC)
	certPEM, keyPEM := generateTestCertificate(t, "nas.example.com", expiry)

	// A key pasted ahead of the certificate must not derail the search.
	got, err := CertificateExpiry(keyPEM + certPEM)
	if err != nil {
		t.Fatalf("CertificateExpiry failed: %v", err)
	}
	if !got.Equal(expiry) {
		t.Errorf("expiry = %s, want %s", got, expiry)
	}
}

func TestCertificateExpiry_RejectsGarbage(t *testing.T) {
	for _, input := range []string{"", "not a pem", "-----BEGIN CERTIFICATE-----\nzzz\n-----END CERTIFICATE-----\n"} {
		if _, err := CertificateExpiry(input); err == nil {
			t.Errorf("CertificateExpiry(%q) succeeded, want an error", input)
		}
	}
}

func TestParseDSMCertificateTime(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Time
	}{
		{
			// The shape DSM returns: OpenSSL's asctime rendering, always GMT.
			name:  "openssl text",
			value: "Nov 27 09:44:00 2026 GMT",
			want:  time.Date(2026, time.November, 27, 9, 44, 0, 0, time.UTC),
		},
		{
			// OpenSSL pads a single-digit day with a space, producing a double
			// space that no Go reference layout models directly.
			name:  "openssl text with a space-padded day",
			value: "May  6 00:00:00 2024 GMT",
			want:  time.Date(2024, time.May, 6, 0, 0, 0, 0, time.UTC),
		},
		{
			name:  "rfc3339",
			value: "2026-11-27T09:44:00Z",
			want:  time.Date(2026, time.November, 27, 9, 44, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDSMCertificateTime(tt.value)
			if err != nil {
				t.Fatalf("ParseDSMCertificateTime(%q) failed: %v", tt.value, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("= %s, want %s", got, tt.want)
			}
		})
	}

	if _, err := ParseDSMCertificateTime("whenever"); err == nil {
		t.Error("an unparseable timestamp must be reported, not silently zeroed")
	}
}

// certificateListPayload mirrors a captured DSM 7 CRT.list response: subject
// and issuer are nested objects, services carry both the internal id and a
// display name, and there is no self_signed field.
func certificateListPayload() map[string]interface{} {
	return map[string]interface{}{
		"certificates": []map[string]interface{}{
			{
				"id":         "K3xR9a",
				"desc":       "wildcard.example.com",
				"is_default": true,
				"is_broken":  false,
				"renewable":  false,
				"key_types":  "ECC",
				"subject": map[string]interface{}{
					"common_name":  "*.example.com",
					"sub_alt_name": []interface{}{"*.example.com", "example.com"},
				},
				"issuer":              map[string]interface{}{"common_name": "Example CA R3", "organization": "Example", "country": "US"},
				"signature_algorithm": "sha256WithRSAEncryption",
				"user_deletable":      true,
				"valid_from":          "Aug 12 00:00:00 2026 GMT",
				"valid_till":          "Nov 10 23:59:59 2026 GMT",
				"services": []interface{}{
					map[string]interface{}{
						"service": "default", "display_name": "DSM Desktop Service",
						"display_name_i18n": "common:web_desktop", "owner": "root",
						"isPkg": false, "subscriber": "system", "user_setable": true,
					},
					map[string]interface{}{"service": "ftpd", "display_name": "FTPS", "owner": "root", "isPkg": false},
				},
			},
			{
				"id":         "SelfSg",
				"desc":       "synology.com",
				"is_default": false,
				"subject":    map[string]interface{}{"common_name": "synology.com"},
				"issuer":     map[string]interface{}{"common_name": "Synology Inc. CA"},
				// DSM marks its own built-in certificate with this nested object
				// instead of a boolean.
				"self_signed_cacrt_info": map[string]interface{}{"common_name": "Synology Inc. CA"},
				"valid_till":             "Jan 27 07:15:52 2034 GMT",
				"services":               []interface{}{},
			},
		},
	}
}

func TestClient_ListCertificates(t *testing.T) {
	var form url.Values
	var query string
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		form = r.PostForm
		writeCertAPIResponse(w, certificateListPayload())
	})

	certificates, err := c.ListCertificates(context.Background())
	if err != nil {
		t.Fatalf("ListCertificates failed: %v", err)
	}

	if form.Get("api") != "SYNO.Core.Certificate.CRT" || form.Get("method") != "list" || form.Get("version") != "1" {
		t.Errorf("unexpected list request: %v", form)
	}
	// Every certificate call needs SynoToken alongside _sid; without it DSM
	// answers 103, which reads like a wrong method name.
	if !strings.Contains(query, "SynoToken=test-token") || !strings.Contains(query, "_sid=test-sid") {
		t.Errorf("query %q must carry both _sid and SynoToken", query)
	}

	if len(certificates) != 2 {
		t.Fatalf("got %d certificates, want 2", len(certificates))
	}

	first := certificates[0]
	if first.ID != "K3xR9a" || first.Description != "wildcard.example.com" {
		t.Errorf("unexpected identity: %+v", first)
	}
	if first.Subject != "*.example.com" || first.Issuer != "Example CA R3" {
		t.Errorf("subject/issuer not unpacked from their nested objects: %+v", first)
	}
	if len(first.SubjectAltNames) != 2 || first.SubjectAltNames[1] != "example.com" {
		t.Errorf("sub_alt_name = %v", first.SubjectAltNames)
	}
	if !first.IsDefault || first.Broken || first.KeyTypes != "ECC" {
		t.Errorf("flags = default:%v broken:%v key_types:%q", first.IsDefault, first.Broken, first.KeyTypes)
	}
	if len(first.Services) != 2 || first.Services[0].Service != "default" || first.Services[0].DisplayName != "DSM Desktop Service" {
		t.Errorf("services = %+v", first.Services)
	}

	expiry, err := first.ExpiresAt()
	if err != nil {
		t.Fatalf("ExpiresAt failed: %v", err)
	}
	if want := time.Date(2026, time.November, 10, 23, 59, 59, 0, time.UTC); !expiry.Equal(want) {
		t.Errorf("expiry = %s, want %s", expiry, want)
	}
}

// TestClient_ListCertificates_DerivesSelfSigned covers the field DSM does not
// send: there is no self_signed boolean, only a nested object on DSM's own
// certificate, so it has to be inferred.
func TestClient_ListCertificates_DerivesSelfSigned(t *testing.T) {
	c := newCertificateTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCertAPIResponse(w, certificateListPayload())
	})

	certificates, err := c.ListCertificates(context.Background())
	if err != nil {
		t.Fatalf("ListCertificates failed: %v", err)
	}
	if certificates[0].SelfSigned {
		t.Error("a CA-issued certificate must not be reported as self-signed")
	}
	if !certificates[1].SelfSigned {
		t.Error("self_signed_cacrt_info marks DSM's own certificate as self-signed")
	}
}

func TestClient_ListCertificates_DerivesSelfSignedFromMatchingNames(t *testing.T) {
	c := newCertificateTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCertAPIResponse(w, map[string]interface{}{"certificates": []map[string]interface{}{{
			"id":      "Own001",
			"desc":    "internal",
			"subject": map[string]interface{}{"common_name": "nas.internal"},
			"issuer":  map[string]interface{}{"common_name": "nas.internal"},
		}}})
	})

	certificates, err := c.ListCertificates(context.Background())
	if err != nil {
		t.Fatalf("ListCertificates failed: %v", err)
	}
	if !certificates[0].SelfSigned {
		t.Error("a certificate that issued itself is self-signed")
	}
}

func TestClient_GetCertificate_NotFound(t *testing.T) {
	c := newCertificateTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCertAPIResponse(w, certificateListPayload())
	})

	if _, err := c.GetCertificate(context.Background(), "K3xR9a"); err != nil {
		t.Fatalf("GetCertificate failed: %v", err)
	}
	_, err := c.GetCertificate(context.Background(), "nope")
	if !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("expected ErrCertificateNotFound, got %v", err)
	}
}

func TestClient_GetCertificateBySubject(t *testing.T) {
	c := newCertificateTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCertAPIResponse(w, certificateListPayload())
	})

	certificate, err := c.GetCertificateBySubject(context.Background(), "*.example.com")
	if err != nil {
		t.Fatalf("GetCertificateBySubject failed: %v", err)
	}
	if certificate.ID != "K3xR9a" {
		t.Errorf("id = %q", certificate.ID)
	}
	if _, err := c.GetCertificateBySubject(context.Background(), "absent.example.com"); !errors.Is(err, ErrCertificateNotFound) {
		t.Fatalf("expected ErrCertificateNotFound, got %v", err)
	}
}

func TestClient_GetCertificateByDescription_AmbiguousMatchIsAnError(t *testing.T) {
	c := newCertificateTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCertAPIResponse(w, map[string]interface{}{"certificates": []map[string]interface{}{
			{"id": "aaa", "desc": "cloud.example.com"},
			{"id": "bbb", "desc": "cloud.example.com"},
		}})
	})

	_, err := c.GetCertificateByDescription(context.Background(), "cloud.example.com")
	if err == nil {
		t.Fatal("an ambiguous description must not resolve to an arbitrary certificate")
	}
	// Both ids belong in the message: the user has to pick one.
	if !strings.Contains(err.Error(), "aaa") || !strings.Contains(err.Error(), "bbb") {
		t.Errorf("error must name the candidates, got %v", err)
	}
}

// TestClient_ImportCertificate_SendsMultipartMaterial pins down the wire format
// of the import, the part of this API with no public documentation: three PEM
// blobs as file parts under fixed field names, the text fields after them, and
// the dispatch plus session in the query string.
func TestClient_ImportCertificate_SendsMultipartMaterial(t *testing.T) {
	certPEM, keyPEM := generateTestCertificate(t, "wildcard.example.com", time.Now().Add(90*24*time.Hour))
	chainPEM, _ := generateTestCertificate(t, "Example CA R3", time.Now().Add(3650*24*time.Hour))

	var parts []uploadedPart
	var query string
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if isCertificateListPost(r) {
			writeCertAPIResponse(w, certificateListPayload())
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "multipart/form-data") {
			t.Errorf("Content-Type = %q, want multipart/form-data", contentType)
		}
		query = r.URL.RawQuery
		parts = readUploadParts(t, r)
		writeCertAPIResponse(w, map[string]interface{}{"id": "K3xR9a", "restart_httpd": true})
	})

	certificate, err := c.ImportCertificate(context.Background(), ImportCertificateRequest{
		Description:  "wildcard.example.com",
		Certificate:  certPEM,
		PrivateKey:   keyPEM,
		Intermediate: chainPEM,
		SetAsDefault: true,
	})
	if err != nil {
		t.Fatalf("ImportCertificate failed: %v", err)
	}
	if certificate.ID != "K3xR9a" {
		t.Errorf("returned certificate id = %q", certificate.ID)
	}

	// Import lives on SYNO.Core.Certificate, not the .CRT sub-API that carries
	// list/set/delete. Getting that wrong answers 103.
	if !strings.Contains(query, "api=SYNO.Core.Certificate&") && !strings.HasSuffix(query, "api=SYNO.Core.Certificate") {
		t.Errorf("query %q must dispatch to SYNO.Core.Certificate, not the CRT sub-API", query)
	}
	for _, want := range []string{"method=import", "version=1", "_sid=test-sid", "SynoToken=test-token"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q missing %q", query, want)
		}
	}

	if got := partValue(parts, "desc"); got != "wildcard.example.com" {
		t.Errorf("desc = %q", got)
	}
	// An empty id tells DSM to install a new certificate rather than replace the
	// material of an existing one; the field must still be present.
	if !hasPart(parts, "id") {
		t.Error("the id field must be sent even when empty")
	}
	if got := partValue(parts, "id"); got != "" {
		t.Errorf("id = %q, want empty for a new certificate", got)
	}
	if got := partValue(parts, "as_default"); got != "true" {
		t.Errorf("as_default = %q", got)
	}

	if got := partValue(parts, "cert"); got != certPEM {
		t.Error("the cert part does not carry the certificate PEM")
	}
	if got := partValue(parts, "key"); got != keyPEM {
		t.Error("the key part does not carry the private key PEM")
	}
	if got := partValue(parts, "inter_cert"); got != chainPEM {
		t.Error("the inter_cert part does not carry the intermediate PEM")
	}

	// acme.sh — the reference implementation with a production track record
	// against this API — sends the files first and the text fields after, the
	// opposite of File Station's ordering.
	if lastFileIndex(parts) > firstTextIndex(parts) {
		t.Errorf("file parts must come before every text field, got order %v", partNames(parts))
	}
	// And unlike File Station, the dispatch triple is not repeated in the body.
	for _, unwanted := range []string{"api", "version", "method"} {
		if hasPart(parts, unwanted) {
			t.Errorf("%q must not be repeated in the import body", unwanted)
		}
	}
}

func TestClient_ImportCertificate_OmitsChainWhenNotSupplied(t *testing.T) {
	certPEM, keyPEM := generateTestCertificate(t, "nas.example.com", time.Now().Add(24*time.Hour))

	var parts []uploadedPart
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if isCertificateListPost(r) {
			writeCertAPIResponse(w, certificateListPayload())
			return
		}
		parts = readUploadParts(t, r)
		writeCertAPIResponse(w, map[string]interface{}{"id": "K3xR9a"})
	})

	if _, err := c.ImportCertificate(context.Background(), ImportCertificateRequest{
		Description: "nas.example.com",
		Certificate: certPEM,
		PrivateKey:  keyPEM,
	}); err != nil {
		t.Fatalf("ImportCertificate failed: %v", err)
	}

	if hasPart(parts, "inter_cert") {
		t.Error("an empty intermediate must not be sent as an empty file part")
	}
	if hasPart(parts, "as_default") {
		t.Error("as_default must be absent unless the certificate is meant to become the default")
	}
}

// TestClient_ImportCertificate_ReplacesInPlace covers the rotation path: the
// existing id goes on the wire, which is what keeps DSM's service assignments
// attached to the renewed certificate instead of stranding them on the old one.
func TestClient_ImportCertificate_ReplacesInPlace(t *testing.T) {
	certPEM, keyPEM := generateTestCertificate(t, "nas.example.com", time.Now().Add(24*time.Hour))

	var parts []uploadedPart
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if isCertificateListPost(r) {
			writeCertAPIResponse(w, certificateListPayload())
			return
		}
		parts = readUploadParts(t, r)
		// DSM does not always echo an id back on an in-place replacement.
		writeCertAPIResponse(w, map[string]interface{}{})
	})

	certificate, err := c.ImportCertificate(context.Background(), ImportCertificateRequest{
		ID:          "K3xR9a",
		Description: "wildcard.example.com",
		Certificate: certPEM,
		PrivateKey:  keyPEM,
	})
	if err != nil {
		t.Fatalf("ImportCertificate failed: %v", err)
	}
	if got := partValue(parts, "id"); got != "K3xR9a" {
		t.Errorf("id = %q, want the existing certificate id", got)
	}
	if certificate.ID != "K3xR9a" {
		t.Errorf("resolved certificate id = %q", certificate.ID)
	}
}

// TestClient_ImportCertificate_SurvivesTheHttpdRestart covers what DSM does
// after an import that touches a certificate it serves itself: it restarts the
// web server, so the read-back briefly fails at the transport level.
func TestClient_ImportCertificate_SurvivesTheHttpdRestart(t *testing.T) {
	original := certificateSettleInterval
	certificateSettleInterval = time.Millisecond
	defer func() { certificateSettleInterval = original }()

	certPEM, keyPEM := generateTestCertificate(t, "nas.example.com", time.Now().Add(24*time.Hour))

	lists := 0
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if isCertificateListPost(r) {
			lists++
			if lists < 3 {
				// The certificate is not visible yet while httpd comes back.
				writeCertAPIResponse(w, map[string]interface{}{"certificates": []map[string]interface{}{}})
				return
			}
			writeCertAPIResponse(w, certificateListPayload())
			return
		}
		readUploadParts(t, r)
		writeCertAPIResponse(w, map[string]interface{}{"restart_httpd": true})
	})

	certificate, err := c.ImportCertificate(context.Background(), ImportCertificateRequest{
		Description: "wildcard.example.com",
		Certificate: certPEM,
		PrivateKey:  keyPEM,
	})
	if err != nil {
		t.Fatalf("ImportCertificate failed: %v", err)
	}
	if certificate.ID != "K3xR9a" {
		t.Errorf("id = %q, want the certificate that appeared once DSM settled", certificate.ID)
	}
}

func TestClient_ImportCertificate_RejectsEmptyMaterialBeforeSending(t *testing.T) {
	c := newCertificateTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("empty material must be refused before any request is sent")
	})

	if _, err := c.ImportCertificate(context.Background(), ImportCertificateRequest{Certificate: "x"}); err == nil {
		t.Error("a missing private key must be an error")
	}
	if _, err := c.ImportCertificate(context.Background(), ImportCertificateRequest{PrivateKey: "x"}); err == nil {
		t.Error("a missing certificate must be an error")
	}
}

// TestClient_ImportCertificate_SurfacesAPIError checks that the 55xx family
// survives with its code, so 5514 can be rendered as "the key does not match
// the certificate" rather than a bare number.
func TestClient_ImportCertificate_SurfacesAPIError(t *testing.T) {
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = readUploadParts(t, r)
		writeCertAPIError(w, 5514)
	})

	_, err := c.ImportCertificate(context.Background(), ImportCertificateRequest{
		Description: "x", Certificate: "cert", PrivateKey: "key",
	})
	if !IsAPIError(err, 5514) {
		t.Fatalf("expected API error 5514, got %v", err)
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("5514 must render as a sentence, got %v", err)
	}
}

func TestClient_DeleteCertificate_SendsJSONArrayOfIDs(t *testing.T) {
	var ids, api, method string
	var query string
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		ids, api, method = r.FormValue("ids"), r.FormValue("api"), r.FormValue("method")
		writeCertAPIResponse(w, map[string]interface{}{})
	})

	if err := c.DeleteCertificate(context.Background(), "K3xR9a"); err != nil {
		t.Fatalf("DeleteCertificate failed: %v", err)
	}
	if api != "SYNO.Core.Certificate.CRT" || method != "delete" {
		t.Errorf("api/method = %q/%q", api, method)
	}
	if ids != `["K3xR9a"]` {
		t.Errorf("ids = %q, want a JSON array even for one certificate", ids)
	}
	// The session belongs in the query string on a POST, never in the body.
	if !strings.Contains(query, "_sid=test-sid") || !strings.Contains(query, "SynoToken=test-token") {
		t.Errorf("query %q must carry the session", query)
	}
}

// TestClient_SetCertificateAttributes pins the quoting quirk: DSM wants id and
// desc as JSON strings on this method, while as_default is a raw value.
func TestClient_SetCertificateAttributes(t *testing.T) {
	var api, method, id, desc, asDefault string
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		api, method = r.FormValue("api"), r.FormValue("method")
		id, desc, asDefault = r.FormValue("id"), r.FormValue("desc"), r.FormValue("as_default")
		writeCertAPIResponse(w, map[string]interface{}{})
	})

	if err := c.SetDefaultCertificate(context.Background(), "K3xR9a", "wildcard.example.com"); err != nil {
		t.Fatalf("SetDefaultCertificate failed: %v", err)
	}
	if api != "SYNO.Core.Certificate.CRT" || method != "set" {
		t.Errorf("api/method = %q/%q, want the CRT sub-API", api, method)
	}
	if id != `"K3xR9a"` {
		t.Errorf("id = %q, want a JSON-quoted string", id)
	}
	if desc != `"wildcard.example.com"` {
		t.Errorf("desc = %q, want a JSON-quoted string", desc)
	}
	if asDefault != "true" {
		t.Errorf("as_default = %q, want the raw value true", asDefault)
	}
}

// TestClient_SetCertificateAttributes_OmitsAsDefaultForARename keeps a rename
// from quietly stealing the default: DSM keeps exactly one default certificate
// and offers no way to ask for none.
func TestClient_SetCertificateAttributes_OmitsAsDefaultForARename(t *testing.T) {
	var seen bool
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		_, seen = r.PostForm["as_default"]
		writeCertAPIResponse(w, map[string]interface{}{})
	})

	if err := c.SetCertificateAttributes(context.Background(), "K3xR9a", "renamed", false); err != nil {
		t.Fatalf("SetCertificateAttributes failed: %v", err)
	}
	if seen {
		t.Error("as_default must be absent for a plain rename")
	}
}

func TestClient_CreateLetsEncryptCertificate(t *testing.T) {
	var domainName, email, desc, api, method string
	var unexpected []string
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.PostForm.Get("api") == certificateCRTAPI {
			writeCertAPIResponse(w, map[string]interface{}{"certificates": []map[string]interface{}{{
				"id": "LeAbc1", "desc": "cloud.example.com", "renewable": true,
				"subject":    map[string]interface{}{"common_name": "cloud.example.com"},
				"issuer":     map[string]interface{}{"common_name": "R3"},
				"valid_till": "Nov 10 23:59:59 2026 GMT",
			}}})
			return
		}
		api, method = r.FormValue("api"), r.FormValue("method")
		domainName = r.FormValue("domain_name")
		email, desc = r.FormValue("email"), r.FormValue("desc")
		// These spellings appear in third-party write-ups but not in DSM.
		for _, name := range []string{"alt_domain", "alt_names", "domain_list", "server_url", "as_default"} {
			if _, ok := r.PostForm[name]; ok {
				unexpected = append(unexpected, name)
			}
		}
		writeCertAPIResponse(w, map[string]interface{}{})
	})

	certificate, err := c.CreateLetsEncryptCertificate(context.Background(), LetsEncryptRequest{
		Domain:      "cloud.example.com",
		AltNames:    []string{"s3.example.com", "www.example.com"},
		Email:       "admin@example.com",
		Description: "cloud.example.com",
	})
	if err != nil {
		t.Fatalf("CreateLetsEncryptCertificate failed: %v", err)
	}

	if api != "SYNO.Core.Certificate.LetsEncrypt" || method != "create" {
		t.Errorf("api/method = %q/%q", api, method)
	}
	if email != "admin@example.com" || desc != "cloud.example.com" {
		t.Errorf("email=%q desc=%q", email, desc)
	}
	// Every name goes into domain_name, common name first, semicolon separated:
	// DSM has no separate parameter for subject alternative names.
	if domainName != "cloud.example.com;s3.example.com;www.example.com" {
		t.Errorf("domain_name = %q, want the common name followed by the alternative names", domainName)
	}
	if len(unexpected) > 0 {
		t.Errorf("parameters DSM does not accept were sent: %v", unexpected)
	}
	if certificate.ID != "LeAbc1" || !certificate.Renewable {
		t.Errorf("unexpected certificate: %+v", certificate)
	}
}

func TestClient_CreateLetsEncryptCertificate_RequiresDomainAndEmail(t *testing.T) {
	c := newCertificateTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("an incomplete request must not reach DSM")
	})

	if _, err := c.CreateLetsEncryptCertificate(context.Background(), LetsEncryptRequest{Email: "a@b.c"}); err == nil {
		t.Error("a missing domain must be an error")
	}
	if _, err := c.CreateLetsEncryptCertificate(context.Background(), LetsEncryptRequest{Domain: "a.example.com"}); err == nil {
		t.Error("a missing email must be an error")
	}
}

// TestClient_CreateLetsEncryptCertificate_SurfacesIssuanceFailure checks that a
// refusal reaches the caller with its code intact, so the provider can turn
// 5521 into "inbound TCP/80 does not reach the NAS" rather than a bare number.
func TestClient_CreateLetsEncryptCertificate_SurfacesIssuanceFailure(t *testing.T) {
	c := newCertificateTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCertAPIError(w, 5521)
	})

	_, err := c.CreateLetsEncryptCertificate(context.Background(), LetsEncryptRequest{
		Domain: "cloud.example.com", Email: "admin@example.com",
	})
	if !IsAPIError(err, 5521) {
		t.Fatalf("expected the DSM error to survive, got %v", err)
	}
	if !strings.Contains(err.Error(), "cloud.example.com") {
		t.Errorf("the failure must name the domain, got %v", err)
	}
	if !strings.Contains(err.Error(), "TCP/80") {
		t.Errorf("5521 must render as a sentence, got %v", err)
	}
}

// TestClient_CreateLetsEncryptCertificate_FallsBackToTheCommonName covers a DSM
// build that ignores desc on this API: the certificate still has to be found.
func TestClient_CreateLetsEncryptCertificate_FallsBackToTheCommonName(t *testing.T) {
	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.PostForm.Get("api") == certificateCRTAPI {
			writeCertAPIResponse(w, map[string]interface{}{"certificates": []map[string]interface{}{{
				"id": "LeAbc1", "desc": "cloud.example.com - Let's Encrypt",
				"subject":    map[string]interface{}{"common_name": "cloud.example.com"},
				"valid_till": "Nov 10 23:59:59 2026 GMT",
			}}})
			return
		}
		writeCertAPIResponse(w, map[string]interface{}{})
	})

	certificate, err := c.CreateLetsEncryptCertificate(context.Background(), LetsEncryptRequest{
		Domain: "cloud.example.com", Email: "admin@example.com", Description: "cloud",
	})
	if err != nil {
		t.Fatalf("CreateLetsEncryptCertificate failed: %v", err)
	}
	if certificate.ID != "LeAbc1" {
		t.Errorf("id = %q, want the certificate matching the common name", certificate.ID)
	}
}

// TestClient_ListCertificates_KeepsEntriesWithAnUnquotedID guards a silent
// failure mode: an id rendered as a number would read as "" and the entry would
// be dropped, which upstream looks exactly like "this certificate is gone" —
// so Terraform would destroy and recreate a certificate that is perfectly fine.
// For a Let's Encrypt certificate that also spends a rate-limit slot.
func TestClient_ListCertificates_KeepsEntriesWithAnUnquotedID(t *testing.T) {
	c := newCertificateTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// Deliberately not a JSON string.
		_, _ = w.Write([]byte(`{"success":true,"data":{"certificates":[
			{"id":12345,"desc":"numeric","subject":{"common_name":"nas.example.com"},"valid_till":"Nov 10 23:59:59 2026 GMT"}
		]}}`))
	})

	certificates, err := c.ListCertificates(context.Background())
	if err != nil {
		t.Fatalf("ListCertificates failed: %v", err)
	}
	if len(certificates) != 1 {
		t.Fatalf("got %d certificates, want the numeric-id entry to survive", len(certificates))
	}
	if certificates[0].ID != "12345" {
		t.Errorf("id = %q, want %q rendered without float formatting", certificates[0].ID, "12345")
	}
	if certificates[0].Description != "numeric" {
		t.Errorf("the rest of the entry must still parse, got %+v", certificates[0])
	}
}

// TestClient_ListCertificates_KeepsUnparseableEntriesByID covers the harsher
// case: the entry itself cannot be understood, but its id can. Losing it would
// still read as a deleted certificate.
func TestClient_ListCertificates_KeepsUnparseableEntriesByID(t *testing.T) {
	c := newCertificateTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// A string where an object belongs makes the entry unparseable as a map.
		_, _ = w.Write([]byte(`{"success":true,"data":{"certificates":[
			"K3xR9a",
			{"id":"Good01","desc":"fine"}
		]}}`))
	})

	certificates, err := c.ListCertificates(context.Background())
	if err != nil {
		t.Fatalf("ListCertificates failed: %v", err)
	}
	if len(certificates) != 1 || certificates[0].ID != "Good01" {
		t.Fatalf("the readable entry must survive a malformed neighbour, got %+v", certificates)
	}
}

// TestClient_ImportCertificate_ExplainsADuplicateAfterATimeout covers the trap
// in the failure path: DSM has already installed the certificate, so a caller
// that simply retries the create installs a second one under the same
// description and every later lookup becomes ambiguous. The error has to say so.
func TestClient_ImportCertificate_ExplainsADuplicateAfterATimeout(t *testing.T) {
	originalInterval, originalTimeout := certificateSettleInterval, certificateSettleTimeout
	certificateSettleInterval, certificateSettleTimeout = time.Millisecond, 20*time.Millisecond
	defer func() {
		certificateSettleInterval, certificateSettleTimeout = originalInterval, originalTimeout
	}()

	c := newCertificateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if isCertificateListPost(r) {
			writeCertAPIResponse(w, map[string]interface{}{"certificates": []map[string]interface{}{}})
			return
		}
		readUploadParts(t, r)
		writeCertAPIResponse(w, map[string]interface{}{"restart_httpd": true})
	})

	_, err := c.ImportCertificate(context.Background(), ImportCertificateRequest{
		Description: "wildcard.example.com", Certificate: "cert", PrivateKey: "key",
	})
	if err == nil {
		t.Fatal("a certificate that never becomes readable must be an error")
	}
	for _, want := range []string{"wildcard.example.com", "terraform import", "second copy", "installed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must mention %q so the operator knows a certificate may exist on the NAS, got:\n%s", want, err)
		}
	}
}

// TestClient_CreateLetsEncryptCertificate_IsBounded checks that a DSM which
// never answers eventually gives the caller its turn back. Issuance has no task
// to poll, so this timeout is the only thing between a wedged NAS and an apply
// that hangs forever.
func TestClient_CreateLetsEncryptCertificate_IsBounded(t *testing.T) {
	original := letsEncryptTimeout
	letsEncryptTimeout = 50 * time.Millisecond
	defer func() { letsEncryptTimeout = original }()

	// The handler never answers. It is released only when the test returns, so
	// the only thing that can end the call is letsEncryptTimeout.
	wedged := make(chan struct{})
	defer close(wedged)

	c := newCertificateTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		<-wedged
	})

	start := time.Now()
	_, err := c.CreateLetsEncryptCertificate(context.Background(), LetsEncryptRequest{
		Domain: "cloud.example.com", Email: "admin@example.com",
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a create that never completes must fail rather than hang")
	}
	if elapsed > 10*time.Second {
		t.Errorf("create took %s; letsEncryptTimeout did not bound it", elapsed)
	}
	if !strings.Contains(err.Error(), "cloud.example.com") {
		t.Errorf("the failure must name the domain, got %v", err)
	}
}

func hasPart(parts []uploadedPart, name string) bool {
	for _, part := range parts {
		if part.name == name {
			return true
		}
	}
	return false
}

func partNames(parts []uploadedPart) []string {
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		names = append(names, part.name)
	}
	return names
}

func lastFileIndex(parts []uploadedPart) int {
	last := -1
	for i, part := range parts {
		if part.filename != "" {
			last = i
		}
	}
	return last
}

func firstTextIndex(parts []uploadedPart) int {
	for i, part := range parts {
		if part.filename == "" {
			return i
		}
	}
	return len(parts)
}
