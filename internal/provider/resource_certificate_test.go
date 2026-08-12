package provider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// testCertificatePEM mints a real certificate so the expiry assertions run
// against DER that a CA could actually have produced.
func testCertificatePEM(t *testing.T, commonName string, notAfter time.Time) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     []string{commonName},
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func certificateResourceSchema(t *testing.T) schema.Schema {
	t.Helper()
	resp := resource.SchemaResponse{}
	NewCertificateResource().Schema(t.Context(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func certificateObject(t *testing.T, sch schema.Schema, values map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	attrs := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		if value, ok := values[name]; ok {
			attrs[name] = value
			continue
		}
		attrs[name] = tftypes.NewValue(typ, nil)
	}
	return tftypes.NewValue(objType, attrs)
}

// certificateServer answers list requests with the supplied entry and records
// whether a delete was attempted, which is the property the destroy guard is
// about.
func certificateServer(t *testing.T, entry map[string]interface{}, deleted *bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Query().Get("method")
		if method == "" {
			_ = r.ParseForm()
			method = r.FormValue("method")
		}

		if method == "delete" {
			if deleted != nil {
				*deleted = true
			}
			writeProviderAPIResponse(w, map[string]interface{}{})
			return
		}

		certificates := []map[string]interface{}{}
		if entry != nil {
			certificates = append(certificates, entry)
		}
		writeProviderAPIResponse(w, map[string]interface{}{"certificates": certificates})
	}))
	t.Cleanup(server.Close)
	return server
}

func writeProviderAPIResponse(w http.ResponseWriter, data interface{}) {
	raw, _ := json.Marshal(data)
	_ = json.NewEncoder(w).Encode(struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}{Success: true, Data: raw})
}

func assignedCertificateEntry() map[string]interface{} {
	return map[string]interface{}{
		"id":          "K3xR9a",
		"desc":        "wildcard.example.com",
		"is_default":  true,
		"self_signed": false,
		"subject":     map[string]interface{}{"common_name": "*.example.com"},
		"issuer":      map[string]interface{}{"common_name": "Example CA R3"},
		"valid_till":  "Nov 10 23:59:59 2026 GMT",
		"services": []interface{}{
			map[string]interface{}{"service": "default", "display_name": "DSM Desktop Service"},
			map[string]interface{}{"service": "ftpd", "display_name": "FTPS"},
		},
	}
}

func deleteCertificateResource(t *testing.T, server *httptest.Server, state map[string]tftypes.Value) *resource.DeleteResponse {
	t.Helper()
	sch := certificateResourceSchema(t)
	raw := certificateObject(t, sch, state)
	r := &certificateResource{client: client.NewClient(server.URL, "admin", "", false)}

	resp := &resource.DeleteResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
	r.Delete(t.Context(), resource.DeleteRequest{State: tfsdk.State{Schema: sch, Raw: raw}}, resp)
	return resp
}

func readCertificateResource(t *testing.T, server *httptest.Server, state map[string]tftypes.Value) (certificateResourceModel, *resource.ReadResponse) {
	t.Helper()
	sch := certificateResourceSchema(t)
	raw := certificateObject(t, sch, state)
	r := &certificateResource{client: client.NewClient(server.URL, "admin", "", false)}

	resp := &resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
	r.Read(t.Context(), resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: raw}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read returned errors: %v", resp.Diagnostics)
	}

	var model certificateResourceModel
	if resp.State.Raw.IsNull() {
		return model, resp
	}
	if diags := resp.State.Get(t.Context(), &model); diags.HasError() {
		t.Fatalf("reading state failed: %v", diags)
	}
	return model, resp
}

// TestCertificateResource_Delete_RefusesWhileServicesAreAssigned is the guard
// the issue asks for: removing a certificate a service still depends on would
// leave DSM serving that service without one, so destroy must stop and say
// which services are in the way.
func TestCertificateResource_Delete_RefusesWhileServicesAreAssigned(t *testing.T) {
	deleted := false
	server := certificateServer(t, assignedCertificateEntry(), &deleted)

	resp := deleteCertificateResource(t, server, map[string]tftypes.Value{
		"id":            tftypes.NewValue(tftypes.String, "K3xR9a"),
		"description":   tftypes.NewValue(tftypes.String, "wildcard.example.com"),
		"force_destroy": tftypes.NewValue(tftypes.Bool, false),
	})

	if !resp.Diagnostics.HasError() {
		t.Fatal("destroy must fail while services are still assigned")
	}
	if deleted {
		t.Error("no delete request may reach DSM once the guard has tripped")
	}

	detail := resp.Diagnostics.Errors()[0].Detail()
	// Naming the services is the whole point: "in use" alone is not actionable.
	for _, want := range []string{"DSM Desktop Service", "default", "FTPS", "ftpd"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the refusal must name %q, got:\n%s", want, detail)
		}
	}
	// And so is saying how to get out of it.
	for _, want := range []string{"Control Panel", "terraform state rm", "force_destroy = true"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the refusal must explain the way out (%q), got:\n%s", want, detail)
		}
	}
	if !strings.Contains(detail, "default certificate") {
		t.Errorf("a certificate that is also the DSM default must say so, got:\n%s", detail)
	}
}

func TestCertificateResource_Delete_ProceedsWhenNoServiceUsesIt(t *testing.T) {
	entry := assignedCertificateEntry()
	entry["services"] = []interface{}{}
	entry["is_default"] = false

	deleted := false
	server := certificateServer(t, entry, &deleted)

	resp := deleteCertificateResource(t, server, map[string]tftypes.Value{
		"id":            tftypes.NewValue(tftypes.String, "K3xR9a"),
		"force_destroy": tftypes.NewValue(tftypes.Bool, false),
	})

	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy of an unused certificate must succeed: %v", resp.Diagnostics)
	}
	if !deleted {
		t.Error("DSM should have been asked to delete the certificate")
	}
}

func TestCertificateResource_Delete_ForceDestroyOverridesTheGuard(t *testing.T) {
	deleted := false
	server := certificateServer(t, assignedCertificateEntry(), &deleted)

	resp := deleteCertificateResource(t, server, map[string]tftypes.Value{
		"id":            tftypes.NewValue(tftypes.String, "K3xR9a"),
		"force_destroy": tftypes.NewValue(tftypes.Bool, true),
	})

	if resp.Diagnostics.HasError() {
		t.Fatalf("force_destroy must be honoured: %v", resp.Diagnostics)
	}
	if !deleted {
		t.Error("force_destroy must actually delete the certificate")
	}
}

// TestCertificateResource_Delete_ChecksDSMRatherThanState covers the case the
// guard is really for: the assignment was made in the DSM UI after the last
// refresh, so state says the certificate is unused and DSM disagrees.
func TestCertificateResource_Delete_ChecksDSMRatherThanState(t *testing.T) {
	deleted := false
	server := certificateServer(t, assignedCertificateEntry(), &deleted)

	resp := deleteCertificateResource(t, server, map[string]tftypes.Value{
		"id":            tftypes.NewValue(tftypes.String, "K3xR9a"),
		"force_destroy": tftypes.NewValue(tftypes.Bool, false),
		// State believes nothing uses this certificate.
		"services": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{}),
	})

	if !resp.Diagnostics.HasError() {
		t.Fatal("a stale, empty services list in state must not let the delete through")
	}
	if deleted {
		t.Error("no delete request may reach DSM once the guard has tripped")
	}
}

func TestCertificateResource_Delete_MissingCertificateIsNotAnError(t *testing.T) {
	server := certificateServer(t, nil, nil)

	resp := deleteCertificateResource(t, server, map[string]tftypes.Value{
		"id":            tftypes.NewValue(tftypes.String, "gone"),
		"force_destroy": tftypes.NewValue(tftypes.Bool, false),
	})
	if resp.Diagnostics.HasError() {
		t.Fatalf("destroying an already-deleted certificate must be a no-op: %v", resp.Diagnostics)
	}
}

// TestCertificateResource_Read_ExpiresAtComesFromTheCertificate is the other
// half of the alerting requirement. DSM here reports a date this provider
// cannot parse; the value in state must still be right, because it is read from
// the certificate rather than from DSM.
func TestCertificateResource_Read_ExpiresAtComesFromTheCertificate(t *testing.T) {
	expiry := time.Date(2027, time.February, 3, 4, 5, 6, 0, time.UTC)
	certPEM := testCertificatePEM(t, "wildcard.example.com", expiry)

	entry := assignedCertificateEntry()
	entry["valid_till"] = "whenever DSM feels like it"

	server := certificateServer(t, entry, nil)

	model, resp := readCertificateResource(t, server, map[string]tftypes.Value{
		"id":          tftypes.NewValue(tftypes.String, "K3xR9a"),
		"description": tftypes.NewValue(tftypes.String, "wildcard.example.com"),
		"certificate": tftypes.NewValue(tftypes.String, certPEM),
	})

	if got, want := model.ExpiresAt.ValueString(), expiry.Format(time.RFC3339); got != want {
		t.Errorf("expires_at = %q, want %q read from the certificate itself", got, want)
	}
	if resp.Diagnostics.WarningsCount() != 0 {
		t.Errorf("DSM's own date must not even be consulted when the PEM is available: %v", resp.Diagnostics.Warnings())
	}
}

// TestCertificateResource_Read_FallsBackToDSMDateAfterImport covers the import
// path, where state has no PEM at all until the configuration supplies one.
func TestCertificateResource_Read_FallsBackToDSMDateAfterImport(t *testing.T) {
	server := certificateServer(t, assignedCertificateEntry(), nil)

	model, _ := readCertificateResource(t, server, map[string]tftypes.Value{
		"id": tftypes.NewValue(tftypes.String, "K3xR9a"),
	})

	if got, want := model.ExpiresAt.ValueString(), "2026-11-10T23:59:59Z"; got != want {
		t.Errorf("expires_at = %q, want %q parsed from DSM's valid_till", got, want)
	}
	// Everything else has to land too, or the imported resource is unusable.
	if model.Description.ValueString() != "wildcard.example.com" || model.Subject.ValueString() != "*.example.com" {
		t.Errorf("import did not populate the identity fields: %+v", model)
	}
	if !model.IsDefault.ValueBool() {
		t.Error("is_default must be populated on import")
	}
	if len(model.Services.Elements()) != 2 {
		t.Errorf("services = %v, want both assignments", model.Services)
	}
}

// TestCertificateResource_Read_WarnsOnAnUnparseableDate makes sure a date this
// provider cannot read is visible rather than silently absent — alerting on a
// null expires_at fails quietly otherwise.
func TestCertificateResource_Read_WarnsOnAnUnparseableDate(t *testing.T) {
	entry := assignedCertificateEntry()
	entry["valid_till"] = "sometime next year"
	server := certificateServer(t, entry, nil)

	model, resp := readCertificateResource(t, server, map[string]tftypes.Value{
		"id": tftypes.NewValue(tftypes.String, "K3xR9a"),
	})

	if !model.ExpiresAt.IsNull() {
		t.Errorf("expires_at = %q, want null when the date cannot be read", model.ExpiresAt.ValueString())
	}
	if resp.Diagnostics.WarningsCount() == 0 {
		t.Fatal("an unreadable expiry must produce a warning")
	}
	if !strings.Contains(resp.Diagnostics.Warnings()[0].Detail(), "sometime next year") {
		t.Errorf("the warning must quote the raw value, got %v", resp.Diagnostics.Warnings()[0].Detail())
	}
}

func TestCertificateResource_Read_RemovesMissingCertificate(t *testing.T) {
	server := certificateServer(t, nil, nil)

	_, resp := readCertificateResource(t, server, map[string]tftypes.Value{
		"id": tftypes.NewValue(tftypes.String, "gone"),
	})
	if !resp.State.Raw.IsNull() {
		t.Error("a certificate deleted in DSM must be dropped from state so it gets recreated")
	}
}

// TestCertificateResource_Read_WarnsWhenDSMHoldsADifferentCertificate covers
// the blind spot of preferring the configured PEM: if the certificate behind
// this id was replaced on the NAS out of band, `expires_at` would keep
// reporting the configured certificate's date forever — a date nobody is being
// served, and an alert that never fires.
func TestCertificateResource_Read_WarnsWhenDSMHoldsADifferentCertificate(t *testing.T) {
	configured := time.Date(2027, time.February, 3, 4, 5, 6, 0, time.UTC)
	certPEM := testCertificatePEM(t, "wildcard.example.com", configured)

	// DSM reports a different, much sooner expiry for the same id.
	entry := assignedCertificateEntry()
	entry["valid_till"] = "Sep 10 23:59:59 2026 GMT"
	server := certificateServer(t, entry, nil)

	model, resp := readCertificateResource(t, server, map[string]tftypes.Value{
		"id":          tftypes.NewValue(tftypes.String, "K3xR9a"),
		"certificate": tftypes.NewValue(tftypes.String, certPEM),
	})

	if got, want := model.ExpiresAt.ValueString(), configured.Format(time.RFC3339); got != want {
		t.Errorf("expires_at = %q, want the configured certificate's expiry %q", got, want)
	}
	if resp.Diagnostics.WarningsCount() == 0 {
		t.Fatal("a disagreement between the configured certificate and DSM must be surfaced")
	}
	detail := resp.Diagnostics.Warnings()[0].Detail()
	for _, want := range []string{"2027-02-03T04:05:06Z", "2026-09-10T23:59:59Z", "outside Terraform"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the warning must mention %q, got:\n%s", want, detail)
		}
	}
}

// TestCertificateResource_Update_DoesNotReuploadUnchangedMaterial: an import
// makes DSM reload the certificate and can restart its web server, which is a
// heavy price for renaming a certificate.
func TestCertificateResource_Update_DoesNotReuploadUnchangedMaterial(t *testing.T) {
	expiry := time.Date(2027, time.February, 3, 4, 5, 6, 0, time.UTC)
	certPEM := testCertificatePEM(t, "wildcard.example.com", expiry)

	var methods []string
	var multipart int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			multipart++
			writeProviderAPIResponse(w, map[string]interface{}{"id": "K3xR9a"})
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		methods = append(methods, r.FormValue("method"))
		if r.FormValue("method") == "list" {
			entry := assignedCertificateEntry()
			entry["desc"] = "renamed"
			writeProviderAPIResponse(w, map[string]interface{}{"certificates": []map[string]interface{}{entry}})
			return
		}
		writeProviderAPIResponse(w, map[string]interface{}{})
	}))
	t.Cleanup(server.Close)

	sch := certificateResourceSchema(t)
	values := func(description string) map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"id":             tftypes.NewValue(tftypes.String, "K3xR9a"),
			"description":    tftypes.NewValue(tftypes.String, description),
			"certificate":    tftypes.NewValue(tftypes.String, certPEM),
			"private_key":    tftypes.NewValue(tftypes.String, "key"),
			"set_as_default": tftypes.NewValue(tftypes.Bool, false),
			"force_destroy":  tftypes.NewValue(tftypes.Bool, false),
			"is_default":     tftypes.NewValue(tftypes.Bool, true),
		}
	}
	state := certificateObject(t, sch, values("wildcard.example.com"))
	plan := certificateObject(t, sch, values("renamed"))

	r := &certificateResource{client: client.NewClient(server.URL, "admin", "", false)}
	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: sch, Raw: state}}
	r.Update(t.Context(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: plan},
		State: tfsdk.State{Schema: sch, Raw: state},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update returned errors: %v", resp.Diagnostics)
	}

	if multipart != 0 {
		t.Errorf("the key material was re-uploaded %d time(s) for a rename; use the set method instead", multipart)
	}
	if !slices.Contains(methods, "set") {
		t.Errorf("a rename must go through the set method, saw %v", methods)
	}
}

// TestCertificateResource_Update_ReuploadsRotatedMaterial is the other half:
// when the certificate really did change, it has to go back to DSM — under the
// same id, so the service assignments survive.
func TestCertificateResource_Update_ReuploadsRotatedMaterial(t *testing.T) {
	oldPEM := testCertificatePEM(t, "wildcard.example.com", time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC))
	newPEM := testCertificatePEM(t, "wildcard.example.com", time.Date(2027, time.September, 1, 0, 0, 0, 0, time.UTC))

	var importedID, importedCert string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatalf("parse multipart: %v", err)
			}
			importedID = r.FormValue("id")
			if files := r.MultipartForm.File["cert"]; len(files) == 1 {
				file, err := files[0].Open()
				if err != nil {
					t.Fatalf("open cert part: %v", err)
				}
				content, _ := io.ReadAll(file)
				importedCert = string(content)
			}
			writeProviderAPIResponse(w, map[string]interface{}{"id": "K3xR9a"})
			return
		}
		writeProviderAPIResponse(w, map[string]interface{}{"certificates": []map[string]interface{}{assignedCertificateEntry()}})
	}))
	t.Cleanup(server.Close)

	sch := certificateResourceSchema(t)
	values := func(pem string) map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"id":             tftypes.NewValue(tftypes.String, "K3xR9a"),
			"description":    tftypes.NewValue(tftypes.String, "wildcard.example.com"),
			"certificate":    tftypes.NewValue(tftypes.String, pem),
			"private_key":    tftypes.NewValue(tftypes.String, "key"),
			"set_as_default": tftypes.NewValue(tftypes.Bool, false),
			"force_destroy":  tftypes.NewValue(tftypes.Bool, false),
			"is_default":     tftypes.NewValue(tftypes.Bool, true),
		}
	}

	r := &certificateResource{client: client.NewClient(server.URL, "admin", "", false)}
	state := certificateObject(t, sch, values(oldPEM))
	plan := certificateObject(t, sch, values(newPEM))
	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: sch, Raw: state}}
	r.Update(t.Context(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: plan},
		State: tfsdk.State{Schema: sch, Raw: state},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update returned errors: %v", resp.Diagnostics)
	}

	// Rotating under the existing id is what keeps the services attached.
	if importedID != "K3xR9a" {
		t.Errorf("import id = %q, want the existing certificate id so the service assignments survive", importedID)
	}
	if importedCert != newPEM {
		t.Error("the rotated certificate was not the one sent to DSM")
	}
}

func TestCertificateResource_Schema(t *testing.T) {
	sch := certificateResourceSchema(t)
	if diags := sch.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := sch.GetAttributes()
	for _, name := range []string{"description", "certificate", "private_key"} {
		if attr := attrs[name]; attr == nil || !attr.IsRequired() {
			t.Errorf("%s must be required", name)
		}
	}
	// Key material must never show up in plan output.
	for _, name := range []string{"certificate", "private_key", "intermediate"} {
		if attr := attrs[name]; attr == nil || !attr.IsSensitive() {
			t.Errorf("%s must be marked sensitive", name)
		}
	}
	for _, name := range []string{"id", "expires_at", "subject", "issuer", "is_default", "self_signed", "services", "subject_alt_names"} {
		if attr := attrs[name]; attr == nil || !attr.IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}
	// The private key lands in state; the documentation has to say so out loud.
	if !strings.Contains(sch.GetDescription(), "state") {
		t.Error("the resource description must warn that the private key is stored in state")
	}
	if !strings.Contains(attrs["private_key"].GetDescription(), "state") {
		t.Error("private_key must carry the state warning on the attribute itself")
	}
}

func TestCertificateLetsEncryptResource_Schema(t *testing.T) {
	resp := resource.SchemaResponse{}
	NewCertificateLetsEncryptResource().Schema(t.Context(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()
	for _, name := range []string{"domain", "email"} {
		if attr := attrs[name]; attr == nil || !attr.IsRequired() {
			t.Errorf("%s must be required", name)
		}
	}
	for _, name := range []string{"id", "expires_at", "subject", "issuer", "is_default", "renewable", "services"} {
		if attr := attrs[name]; attr == nil || !attr.IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}
	// Nothing secret ever reaches Terraform here — DSM keeps the key.
	for name, attr := range attrs {
		if attr.IsSensitive() {
			t.Errorf("%s is marked sensitive, but this resource handles no key material", name)
		}
	}

	metadata := &resource.MetadataResponse{}
	NewCertificateLetsEncryptResource().Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, metadata)
	if metadata.TypeName != "dsm_certificate_lets_encrypt" {
		t.Errorf("type name = %q", metadata.TypeName)
	}
}

func TestCertificatesDataSource_Schema(t *testing.T) {
	resp := datasource.SchemaResponse{}
	NewCertificatesDataSource().Schema(t.Context(), datasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()
	if attr := attrs["description"]; attr == nil || !attr.IsOptional() {
		t.Error("description must be an optional filter")
	}
	if attr := attrs["certificates"]; attr == nil || !attr.IsComputed() {
		t.Error("certificates must be computed")
	}

	metadata := &datasource.MetadataResponse{}
	NewCertificatesDataSource().Metadata(t.Context(), datasource.MetadataRequest{ProviderTypeName: "dsm"}, metadata)
	if metadata.TypeName != "dsm_certificates" {
		t.Errorf("type name = %q", metadata.TypeName)
	}
}

func TestCertificateErrorDetail(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		contains []string
	}{
		{
			name:     "102 explains where the API is missing",
			err:      &client.APIError{Code: 102, API: "SYNO.Core.Certificate"},
			contains: []string{"DSM 7", "Virtual DSM"},
		},
		{
			name:     "101 lists the usual import mistakes",
			err:      &client.APIError{Code: 101, API: "SYNO.Core.Certificate"},
			contains: []string{"does not match", "passphrase", "chain"},
		},
		{
			name:     "105 names the account requirement",
			err:      &client.APIError{Code: 105, API: "SYNO.Core.Certificate"},
			contains: []string{"administrators"},
		},
		{
			// 103 on these APIs is a missing SynoToken far more often than a
			// missing method, and reading it literally sends people hunting for
			// the wrong thing.
			name:     "103 corrects the obvious misreading",
			err:      &client.APIError{Code: 103, API: "SYNO.Core.Certificate"},
			contains: []string{"SynoToken"},
		},
		{
			name:     "5514 points at the key pair",
			err:      &client.APIError{Code: 5514, API: "SYNO.Core.Certificate"},
			contains: []string{"does not match", "pair"},
		},
		{
			name:     "5517 points at the chain",
			err:      &client.APIError{Code: 5517, API: "SYNO.Core.Certificate"},
			contains: []string{"intermediate", "chain"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := certificateErrorDetail(tt.err)
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("detail should mention %q, got:\n%s", want, got)
				}
			}
			if !strings.Contains(got, tt.err.Error()) {
				t.Errorf("the original message must survive, got:\n%s", got)
			}
		})
	}
}

// TestLetsEncryptErrorDetail checks that an issuance failure is explained
// instead of being handed over as a bare DSM code.
func TestLetsEncryptErrorDetail(t *testing.T) {
	err := &client.APIError{Code: 101, API: "SYNO.Core.Certificate.LetsEncrypt"}
	got := letsEncryptErrorDetail(err, "cloud.example.com", []string{"s3.example.com"})

	for _, want := range []string{
		"cloud.example.com",
		"s3.example.com",
		"resolves",
		"TCP/80",
		"rate limit",
		"acme-challenge",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the diagnostic should mention %q, got:\n%s", want, got)
		}
	}
	if !strings.Contains(got, err.Error()) {
		t.Error("the DSM code must still be visible")
	}
}
