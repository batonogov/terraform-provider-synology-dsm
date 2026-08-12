package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func letsEncryptSchema(t *testing.T) schema.Schema {
	t.Helper()
	resp := resource.SchemaResponse{}
	NewCertificateLetsEncryptResource().Schema(t.Context(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func letsEncryptObject(t *testing.T, sch schema.Schema, values map[string]tftypes.Value) tftypes.Value {
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

// letsEncryptEntry is a DSM list entry for an ACME-issued certificate. DSM
// repeats the common name inside sub_alt_name, which is why alt_names has to be
// derived rather than copied.
func letsEncryptEntry() map[string]interface{} {
	return map[string]interface{}{
		"id":         "LeAbc1",
		"desc":       "cloud.example.com",
		"is_default": false,
		"renewable":  true,
		"subject": map[string]interface{}{
			"common_name":  "cloud.example.com",
			"sub_alt_name": []interface{}{"cloud.example.com", "s3.example.com", "www.example.com"},
		},
		"issuer":     map[string]interface{}{"common_name": "R3"},
		"valid_till": "Nov 10 23:59:59 2026 GMT",
		"services":   []interface{}{},
	}
}

func letsEncryptServer(t *testing.T, entry map[string]interface{}, calls *[]recordedCall) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if calls != nil {
			*calls = append(*calls, recordedCall{api: r.FormValue("api"), method: r.FormValue("method"), form: r.PostForm})
		}

		certificates := []map[string]interface{}{}
		if entry != nil {
			certificates = append(certificates, entry)
		}
		switch r.FormValue("method") {
		case "list":
			writeProviderAPIResponse(w, map[string]interface{}{"certificates": certificates})
		default:
			writeProviderAPIResponse(w, map[string]interface{}{})
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func readLetsEncryptResource(t *testing.T, server *httptest.Server, state map[string]tftypes.Value) (certificateLetsEncryptResourceModel, *resource.ReadResponse) {
	t.Helper()
	sch := letsEncryptSchema(t)
	raw := letsEncryptObject(t, sch, state)
	r := &certificateLetsEncryptResource{client: client.NewClient(server.URL, "admin", "", false)}

	resp := &resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
	r.Read(t.Context(), resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: raw}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read returned errors: %v", resp.Diagnostics)
	}

	var model certificateLetsEncryptResourceModel
	if resp.State.Raw.IsNull() {
		return model, resp
	}
	if diags := resp.State.Get(t.Context(), &model); diags.HasError() {
		t.Fatalf("reading state failed: %v", diags)
	}
	return model, resp
}

func setElements(t *testing.T, model certificateLetsEncryptResourceModel) []string {
	t.Helper()
	var values []string
	if diags := model.AltNames.ElementsAs(t.Context(), &values, false); diags.HasError() {
		t.Fatalf("reading alt_names failed: %v", diags)
	}
	slices.Sort(values)
	return values
}

// TestLetsEncryptResource_Read_PopulatesEveryFieldAfterImport is the regression
// for the worst failure this resource can have.
//
// After `terraform import` state holds nothing but the id. `domain` and
// `alt_names` are RequiresReplace, so leaving them null makes the very next plan
// read `domain = null -> "cloud.example.com" # forces replacement`: Terraform
// destroys a working certificate and asks Let's Encrypt for another one,
// spending a rate-limit slot to end up where it started. If the certificate is
// assigned to a service the destroy guard refuses as well, and the resource is
// wedged with no way out but force_destroy — which strips TLS from those
// services. Import exists precisely to avoid all of that.
func TestLetsEncryptResource_Read_PopulatesEveryFieldAfterImport(t *testing.T) {
	server := letsEncryptServer(t, letsEncryptEntry(), nil)

	model, _ := readLetsEncryptResource(t, server, map[string]tftypes.Value{
		"id": tftypes.NewValue(tftypes.String, "LeAbc1"),
	})

	if got := model.Domain.ValueString(); got != "cloud.example.com" {
		t.Errorf("domain = %q, want it restored from the subject common name; a null here forces a replacement", got)
	}
	// The common name is not an alternative name as far as the configuration is
	// concerned, even though DSM repeats it in sub_alt_name.
	if got := setElements(t, model); !slices.Equal(got, []string{"s3.example.com", "www.example.com"}) {
		t.Errorf("alt_names = %v, want the SAN list without the common name", got)
	}
	if got := model.Description.ValueString(); got != "cloud.example.com" {
		t.Errorf("description = %q", got)
	}
	if got := model.ExpiresAt.ValueString(); got != "2026-11-10T23:59:59Z" {
		t.Errorf("expires_at = %q", got)
	}
	if !model.Renewable.ValueBool() {
		t.Error("renewable must be populated on import")
	}
	if len(model.SubjectAltNames.Elements()) != 3 {
		t.Errorf("subject_alt_names = %v, want everything DSM reports", model.SubjectAltNames)
	}
}

// TestLetsEncryptResource_Read_LeavesEmailAlone covers the one attribute DSM
// does not report. It must survive a refresh untouched rather than be blanked,
// and — since it cannot be recovered after an import — it must not be able to
// force a replacement either.
func TestLetsEncryptResource_Read_LeavesEmailAlone(t *testing.T) {
	server := letsEncryptServer(t, letsEncryptEntry(), nil)

	model, _ := readLetsEncryptResource(t, server, map[string]tftypes.Value{
		"id":    tftypes.NewValue(tftypes.String, "LeAbc1"),
		"email": tftypes.NewValue(tftypes.String, "admin@example.com"),
	})

	if got := model.Email.ValueString(); got != "admin@example.com" {
		t.Errorf("email = %q, want the configured value preserved: DSM never reports it", got)
	}

	attr := letsEncryptSchema(t).GetAttributes()["email"]
	if attr.(schema.StringAttribute).PlanModifiers != nil && len(attr.(schema.StringAttribute).PlanModifiers) > 0 {
		t.Error("email must carry no RequiresReplace: it is null after an import, and a forced replacement there destroys a working certificate")
	}
}

// TestLetsEncryptResource_Read_AdoptsARenewedCertificate covers DSM renewing on
// its own and replacing the certificate object: the id in state is stale, but
// the domain still resolves to a certificate DSM holds. Dropping the resource
// would make Terraform reissue against the rate limit.
func TestLetsEncryptResource_Read_AdoptsARenewedCertificate(t *testing.T) {
	entry := letsEncryptEntry()
	entry["id"] = "LeNew2"
	server := letsEncryptServer(t, entry, nil)

	model, resp := readLetsEncryptResource(t, server, map[string]tftypes.Value{
		"id":     tftypes.NewValue(tftypes.String, "LeAbc1"),
		"domain": tftypes.NewValue(tftypes.String, "cloud.example.com"),
	})

	if resp.State.Raw.IsNull() {
		t.Fatal("a certificate DSM renewed under a new id must be adopted, not dropped and reissued")
	}
	if got := model.ID.ValueString(); got != "LeNew2" {
		t.Errorf("id = %q, want the new id DSM assigned", got)
	}
}

// TestLetsEncryptResource_Read_DoesNotAdoptOnAnEmptyDomain is the guard against
// the lookup degenerating into "match anything". A certificate whose subject
// DSM omitted would otherwise be adopted under this resource's id.
func TestLetsEncryptResource_Read_DoesNotAdoptOnAnEmptyDomain(t *testing.T) {
	entry := letsEncryptEntry()
	entry["id"] = "Other1"
	delete(entry, "subject")
	server := letsEncryptServer(t, entry, nil)

	_, resp := readLetsEncryptResource(t, server, map[string]tftypes.Value{
		"id": tftypes.NewValue(tftypes.String, "LeAbc1"),
		// domain is null, as it is immediately after an import
	})

	if !resp.State.Raw.IsNull() {
		t.Error("with no domain to match on, the resource must be dropped rather than bound to an arbitrary certificate")
	}
}

func TestLetsEncryptResource_Read_RemovesMissingCertificate(t *testing.T) {
	server := letsEncryptServer(t, nil, nil)

	_, resp := readLetsEncryptResource(t, server, map[string]tftypes.Value{
		"id":     tftypes.NewValue(tftypes.String, "LeAbc1"),
		"domain": tftypes.NewValue(tftypes.String, "cloud.example.com"),
	})
	if !resp.State.Raw.IsNull() {
		t.Error("a certificate that is gone from DSM must be dropped from state")
	}
}

// TestLetsEncryptResource_Create_SendsTheRequestAndClaimsTheDefault checks the
// two-step create: DSM's create method is not known to take as_default, so the
// default is claimed with a follow-up set.
func TestLetsEncryptResource_Create_SendsTheRequestAndClaimsTheDefault(t *testing.T) {
	var calls []recordedCall
	server := letsEncryptServer(t, letsEncryptEntry(), &calls)

	sch := letsEncryptSchema(t)
	plan := letsEncryptObject(t, sch, map[string]tftypes.Value{
		"domain": tftypes.NewValue(tftypes.String, "cloud.example.com"),
		"email":  tftypes.NewValue(tftypes.String, "admin@example.com"),
		"alt_names": tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, []tftypes.Value{
			tftypes.NewValue(tftypes.String, "s3.example.com"),
			tftypes.NewValue(tftypes.String, "www.example.com"),
		}),
		"set_as_default": tftypes.NewValue(tftypes.Bool, true),
		"force_destroy":  tftypes.NewValue(tftypes.Bool, false),
	})

	r := &certificateLetsEncryptResource{client: client.NewClient(server.URL, "admin", "", false)}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: plan}}
	r.Create(t.Context(), resource.CreateRequest{Plan: tfsdk.Plan{Schema: sch, Raw: plan}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create returned errors: %v", resp.Diagnostics)
	}

	create := findCall(t, calls, "SYNO.Core.Certificate.LetsEncrypt", "create")
	if got := create.form.Get("domain_name"); got != "cloud.example.com;s3.example.com;www.example.com" {
		t.Errorf("domain_name = %q, want the common name first and the alternative names after it", got)
	}
	if got := create.form.Get("desc"); got != "cloud.example.com" {
		t.Errorf("desc = %q, want it to default to the domain", got)
	}

	// The default is claimed separately, on the CRT sub-API.
	setCall := findCall(t, calls, "SYNO.Core.Certificate.CRT", "set")
	if got := setCall.form.Get("as_default"); got != "true" {
		t.Errorf("as_default = %q on the follow-up set", got)
	}

	var model certificateLetsEncryptResourceModel
	if diags := resp.State.Get(t.Context(), &model); diags.HasError() {
		t.Fatalf("reading state failed: %v", diags)
	}
	if model.ID.ValueString() != "LeAbc1" || model.Email.ValueString() != "admin@example.com" {
		t.Errorf("state after create: %+v", model)
	}
}

// TestLetsEncryptResource_Update_WarnsAboutTheContactAddress: DSM cannot change
// the ACME contact address of an issued certificate, and reissuing to apply one
// would cost a rate-limit slot. Silently doing nothing is the wrong answer, so
// the change is recorded and explained.
func TestLetsEncryptResource_Update_WarnsAboutTheContactAddress(t *testing.T) {
	var calls []recordedCall
	server := letsEncryptServer(t, letsEncryptEntry(), &calls)

	sch := letsEncryptSchema(t)
	state := letsEncryptObject(t, sch, map[string]tftypes.Value{
		"id":             tftypes.NewValue(tftypes.String, "LeAbc1"),
		"domain":         tftypes.NewValue(tftypes.String, "cloud.example.com"),
		"description":    tftypes.NewValue(tftypes.String, "cloud.example.com"),
		"email":          tftypes.NewValue(tftypes.String, "old@example.com"),
		"set_as_default": tftypes.NewValue(tftypes.Bool, false),
		"force_destroy":  tftypes.NewValue(tftypes.Bool, false),
		"is_default":     tftypes.NewValue(tftypes.Bool, false),
	})
	plan := letsEncryptObject(t, sch, map[string]tftypes.Value{
		"id":             tftypes.NewValue(tftypes.String, "LeAbc1"),
		"domain":         tftypes.NewValue(tftypes.String, "cloud.example.com"),
		"description":    tftypes.NewValue(tftypes.String, "cloud.example.com"),
		"email":          tftypes.NewValue(tftypes.String, "new@example.com"),
		"set_as_default": tftypes.NewValue(tftypes.Bool, false),
		"force_destroy":  tftypes.NewValue(tftypes.Bool, false),
	})

	r := &certificateLetsEncryptResource{client: client.NewClient(server.URL, "admin", "", false)}
	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: sch, Raw: state}}
	r.Update(t.Context(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: plan},
		State: tfsdk.State{Schema: sch, Raw: state},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update returned errors: %v", resp.Diagnostics)
	}

	if resp.Diagnostics.WarningsCount() == 0 {
		t.Fatal("changing email must not look like it took effect")
	}
	detail := resp.Diagnostics.Warnings()[0].Detail()
	for _, want := range []string{"old@example.com", "new@example.com", "next time"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the warning must mention %q, got:\n%s", want, detail)
		}
	}

	var model certificateLetsEncryptResourceModel
	if diags := resp.State.Get(t.Context(), &model); diags.HasError() {
		t.Fatalf("reading state failed: %v", diags)
	}
	if model.Email.ValueString() != "new@example.com" {
		t.Errorf("email = %q, want the new value recorded in state", model.Email.ValueString())
	}
}

// TestLetsEncryptResource_ModifyPlan_ReclaimsTheDefault: set_as_default would
// otherwise be a one-shot. If something takes the default away outside
// Terraform, nothing in the configuration changes and no drift is reported.
func TestLetsEncryptResource_ModifyPlan_ReclaimsTheDefault(t *testing.T) {
	sch := letsEncryptSchema(t)

	build := func(setAsDefault, isDefault bool) (tftypes.Value, tftypes.Value) {
		values := map[string]tftypes.Value{
			"id":             tftypes.NewValue(tftypes.String, "LeAbc1"),
			"domain":         tftypes.NewValue(tftypes.String, "cloud.example.com"),
			"email":          tftypes.NewValue(tftypes.String, "admin@example.com"),
			"set_as_default": tftypes.NewValue(tftypes.Bool, setAsDefault),
			"force_destroy":  tftypes.NewValue(tftypes.Bool, false),
			"is_default":     tftypes.NewValue(tftypes.Bool, isDefault),
		}
		return letsEncryptObject(t, sch, values), letsEncryptObject(t, sch, values)
	}

	tests := []struct {
		name         string
		setAsDefault bool
		isDefault    bool
		wantUnknown  bool
	}{
		{"default was taken away out of band", true, false, true},
		{"still the default", true, true, false},
		{"never asked to be the default", false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			planRaw, stateRaw := build(tt.setAsDefault, tt.isDefault)
			r := &certificateLetsEncryptResource{}
			resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: planRaw}}
			r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{
				Plan:   tfsdk.Plan{Schema: sch, Raw: planRaw},
				State:  tfsdk.State{Schema: sch, Raw: stateRaw},
				Config: tfsdk.Config{Schema: sch, Raw: planRaw},
			}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("ModifyPlan returned errors: %v", resp.Diagnostics)
			}

			var model certificateLetsEncryptResourceModel
			if diags := resp.Plan.Get(t.Context(), &model); diags.HasError() {
				t.Fatalf("reading plan failed: %v", diags)
			}
			if model.IsDefault.IsUnknown() != tt.wantUnknown {
				t.Errorf("is_default unknown = %v, want %v (unknown is what produces a plan and gets Update called)",
					model.IsDefault.IsUnknown(), tt.wantUnknown)
			}
		})
	}
}

// TestLetsEncryptResource_ModifyPlan_IgnoresCreateAndDestroy: there is no prior
// state on create and no plan on destroy, and touching either would panic.
func TestLetsEncryptResource_ModifyPlan_IgnoresCreateAndDestroy(t *testing.T) {
	sch := letsEncryptSchema(t)
	populated := letsEncryptObject(t, sch, map[string]tftypes.Value{
		"set_as_default": tftypes.NewValue(tftypes.Bool, true),
	})
	null := tftypes.NewValue(sch.Type().TerraformType(context.Background()), nil)

	r := &certificateLetsEncryptResource{}
	for _, tt := range []struct {
		name        string
		plan, state tftypes.Value
	}{
		{"create", populated, null},
		{"destroy", null, populated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: tt.plan}}
			r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{
				Plan:  tfsdk.Plan{Schema: sch, Raw: tt.plan},
				State: tfsdk.State{Schema: sch, Raw: tt.state},
			}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("ModifyPlan returned errors: %v", resp.Diagnostics)
			}
		})
	}
}

// recordedCall is one request the fake DSM saw, so a test can assert on what
// actually went over the wire rather than only on the resulting state.
type recordedCall struct {
	api    string
	method string
	form   url.Values
}

func findCall(t *testing.T, calls []recordedCall, api, method string) recordedCall {
	t.Helper()
	for _, call := range calls {
		if call.api == api && call.method == method {
			return call
		}
	}
	t.Fatalf("no %s %s call was made; saw %v", api, method, calls)
	return recordedCall{}
}
