package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func sharedFolderSchema(t *testing.T) resource.SchemaResponse {
	t.Helper()

	r := NewSharedFolderResource()
	resp := resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	return resp
}

func TestSharedFolderResource_Schema(t *testing.T) {
	resp := sharedFolderSchema(t)

	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()

	for _, name := range []string{"name", "vol_path"} {
		if !attrs[name].IsRequired() {
			t.Errorf("attribute %q must be required", name)
		}
	}

	for _, name := range []string{
		"hidden", "enable_recycle_bin", "recycle_bin_admin_only",
		"enable_share_compress", "enable_share_cow", "share_quota",
	} {
		attr, ok := attrs[name]
		if !ok {
			t.Errorf("missing attribute %q", name)
			continue
		}
		if attr.IsRequired() {
			t.Errorf("attribute %q must not be required", name)
		}
		if !attr.IsComputed() {
			t.Errorf("attribute %q must be computed so its default applies", name)
		}
	}

	for _, name := range []string{"id", "uuid"} {
		if !attrs[name].IsComputed() {
			t.Errorf("attribute %q must be computed", name)
		}
	}
}

// TestSharedFolderResource_Read_ReportsPosixPermissions covers issue #94 for a
// shared folder: SYNO.Core.Share reports nothing about the POSIX mode the
// folder lands on disk with, so Read asks File Station for it — by the bare
// share name, which is how File Station addresses a share root.
func TestSharedFolderResource_Read_ReportsPosixPermissions(t *testing.T) {
	var permissionPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "SYNO.Core.Share":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"data": map[string]interface{}{
					"name": "containers", "vol_path": "/volume1", "uuid": "uuid-1",
				},
			})
		case "SYNO.FileStation.List":
			permissionPath = r.URL.Query().Get("path")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"data": map[string]interface{}{"files": []map[string]interface{}{{
					"path": "/containers", "name": "containers", "isdir": true,
					"additional": map[string]interface{}{
						"perm":  map[string]interface{}{"posix": 0, "is_acl_mode": true},
						"owner": map[string]interface{}{"user": "root", "group": "root", "uid": 0, "gid": 0},
					},
				}}},
			})
		default:
			t.Errorf("unexpected API call: %s", r.URL.RawQuery)
		}
	}))
	t.Cleanup(server.Close)

	sch := sharedFolderSchema(t).Schema
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	attrs := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		attrs[name] = tftypes.NewValue(typ, nil)
	}
	attrs["id"] = tftypes.NewValue(tftypes.String, "containers")
	raw := tftypes.NewValue(objType, attrs)

	r := &sharedFolderResource{client: client.NewClient(server.URL, "admin", "", false)}
	resp := &resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
	r.Read(t.Context(), resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: raw}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read returned errors: %v", resp.Diagnostics)
	}

	var model sharedFolderResourceModel
	if diags := resp.State.Get(t.Context(), &model); diags.HasError() {
		t.Fatalf("reading state failed: %v", diags)
	}

	if permissionPath != `["/containers"]` {
		t.Errorf("permission lookup used path %q, want the File Station share root", permissionPath)
	}
	if model.PosixMode.ValueString() != "000" || !model.ACLMode.ValueBool() {
		t.Errorf("posix_mode/acl_mode = %q/%v, want \"000\"/true", model.PosixMode.ValueString(), model.ACLMode.ValueBool())
	}
	if model.PosixOwner.ValueString() != "root" || model.PosixUID.ValueInt64() != 0 {
		t.Errorf("ownership not reported: %+v", model.posixPermissionsModel)
	}
}

// TestSharedFolderResource_Read_SurvivesMissingFileStation keeps the extra read
// best effort: File Station is a package, and a share this provider read
// successfully must not fail to refresh because an informational attribute
// could not be filled in.
func TestSharedFolderResource_Read_SurvivesMissingFileStation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api") == "SYNO.Core.Share" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"data":    map[string]interface{}{"name": "containers", "vol_path": "/volume1"},
			})
			return
		}
		// 103: the File Station API is not installed on this NAS.
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false, "error": map[string]int{"code": 103},
		})
	}))
	t.Cleanup(server.Close)

	sch := sharedFolderSchema(t).Schema
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	attrs := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		attrs[name] = tftypes.NewValue(typ, nil)
	}
	attrs["id"] = tftypes.NewValue(tftypes.String, "containers")
	raw := tftypes.NewValue(objType, attrs)

	r := &sharedFolderResource{client: client.NewClient(server.URL, "admin", "", false)}
	resp := &resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
	r.Read(t.Context(), resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: raw}}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read must not fail when File Station is unavailable: %v", resp.Diagnostics)
	}

	var model sharedFolderResourceModel
	if diags := resp.State.Get(t.Context(), &model); diags.HasError() {
		t.Fatalf("reading state failed: %v", diags)
	}
	if !model.PosixMode.IsNull() || !model.ACLMode.IsNull() {
		t.Errorf("permissions must stay null when File Station cannot answer: %+v", model.posixPermissionsModel)
	}
}

// TestReplaceIfTurnedOn covers the predicate behind the plan modifier that
// guards the creation-time-only flags: only switching one ON may destroy the
// folder, since DSM ignores an in-place enable.
func TestReplaceIfTurnedOn(t *testing.T) {
	tests := []struct {
		name  string
		state bool
		plan  bool
		want  bool
	}{
		{name: "off to on replaces", state: false, plan: true, want: true},
		{name: "on to off is in place", state: true, plan: false, want: false},
		{name: "unchanged on", state: true, plan: true, want: false},
		{name: "unchanged off", state: false, plan: false, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := planmodifier.BoolRequest{
				StateValue: types.BoolValue(tt.state),
				PlanValue:  types.BoolValue(tt.plan),
			}
			resp := &boolplanmodifier.RequiresReplaceIfFuncResponse{}
			replaceIfTurnedOn(context.Background(), req, resp)

			if resp.RequiresReplace != tt.want {
				t.Errorf("RequiresReplace = %v, want %v", resp.RequiresReplace, tt.want)
			}
		})
	}
}

// sharedFolderConfig builds a tfsdk.Config for ValidateConfig from the values
// that matter to the validator; everything else is null.
func sharedFolderConfig(t *testing.T, compress, cow *bool) tfsdk.Config {
	t.Helper()

	sch := sharedFolderSchema(t).Schema
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)

	boolValue := func(v *bool) tftypes.Value {
		if v == nil {
			return tftypes.NewValue(tftypes.Bool, nil)
		}
		return tftypes.NewValue(tftypes.Bool, *v)
	}

	values := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		switch name {
		case "enable_share_compress":
			values[name] = boolValue(compress)
		case "enable_share_cow":
			values[name] = boolValue(cow)
		case "name":
			values[name] = tftypes.NewValue(tftypes.String, "team-data")
		case "vol_path":
			values[name] = tftypes.NewValue(tftypes.String, "/volume1")
		default:
			values[name] = tftypes.NewValue(typ, nil)
		}
	}

	return tfsdk.Config{
		Schema: sch,
		Raw:    tftypes.NewValue(objType, values),
	}
}

// TestSharedFolderResource_ValidateConfig pins the rule DSM enforces itself:
// compression cannot be enabled without copy-on-write.
func TestSharedFolderResource_ValidateConfig(t *testing.T) {
	ptr := func(b bool) *bool { return &b }

	tests := []struct {
		name      string
		compress  *bool
		cow       *bool
		wantError bool
	}{
		{name: "compress with cow is valid", compress: ptr(true), cow: ptr(true)},
		{name: "compress without cow is rejected", compress: ptr(true), cow: ptr(false), wantError: true},
		{name: "compress with cow unset is rejected", compress: ptr(true), cow: nil, wantError: true},
		{name: "no compress, no cow", compress: ptr(false), cow: ptr(false)},
		{name: "cow alone is fine", compress: ptr(false), cow: ptr(true)},
		{name: "both unset", compress: nil, cow: nil},
	}

	r := NewSharedFolderResource().(*sharedFolderResource)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &resource.ValidateConfigResponse{}
			r.ValidateConfig(
				t.Context(),
				resource.ValidateConfigRequest{Config: sharedFolderConfig(t, tt.compress, tt.cow)},
				resp,
			)

			if tt.wantError && !resp.Diagnostics.HasError() {
				t.Fatal("expected a validation error")
			}
			if !tt.wantError && resp.Diagnostics.HasError() {
				t.Fatalf("unexpected validation error: %v", resp.Diagnostics)
			}
		})
	}
}

func TestSharedFolderResource_Metadata(t *testing.T) {
	r := NewSharedFolderResource()

	resp := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, resp)

	if resp.TypeName != "dsm_shared_folder" {
		t.Errorf("expected dsm_shared_folder, got %q", resp.TypeName)
	}
}

func TestSharedFolderResource_Configure_WrongType(t *testing.T) {
	r := NewSharedFolderResource().(*sharedFolderResource)

	resp := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: "not-a-client"}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error for wrong ProviderData type")
	}
	if r.client != nil {
		t.Error("client should remain nil for wrong type")
	}
}
