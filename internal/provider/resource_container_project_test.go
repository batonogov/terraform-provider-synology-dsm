package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func containerProjectSchema(t *testing.T) resource.SchemaResponse {
	t.Helper()
	r := NewContainerProjectResource()
	resp := resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	return resp
}

func TestContainerProjectResource_Schema(t *testing.T) {
	resp := containerProjectSchema(t)
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()
	for _, name := range []string{"name", "share_path"} {
		if attr := attrs[name]; attr == nil || !attr.IsRequired() {
			t.Errorf("%s must be required", name)
		}
	}
	// compose_yaml stopped being required when compose_yaml_wo arrived; exactly
	// one of the two is enforced in ValidateConfig instead.
	for _, name := range []string{"compose_yaml", "compose_yaml_wo", "compose_yaml_wo_version"} {
		if attr := attrs[name]; attr == nil || !attr.IsOptional() {
			t.Errorf("%s must be optional", name)
		}
	}
	if attr := attrs["compose_yaml_wo"]; attr == nil || !attr.IsWriteOnly() || attr.IsComputed() {
		t.Error("compose_yaml_wo must be write-only and not computed — that is what keeps the document out of state")
	}
	if attrs["compose_yaml_wo_version"].IsWriteOnly() {
		t.Error("compose_yaml_wo_version must survive in state: it is the marker Read keys off")
	}
	for _, name := range []string{"running", "delete_on_destroy"} {
		if attr := attrs[name]; attr == nil || !attr.IsOptional() || !attr.IsComputed() {
			t.Errorf("%s must be optional and computed", name)
		}
	}
	for _, name := range []string{"id", "path", "status", "container_ids", "compose_yaml_checksum"} {
		if attr := attrs[name]; attr == nil || !attr.IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}
	for _, name := range []string{"compose_yaml", "compose_yaml_wo"} {
		if !attrs[name].IsSensitive() {
			t.Errorf("%s must be sensitive", name)
		}
	}
}

func TestContainerProjectResource_MetadataAndConfigure(t *testing.T) {
	r := NewContainerProjectResource().(*containerProjectResource)
	metadata := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, metadata)
	if metadata.TypeName != "dsm_container_project" {
		t.Errorf("type name = %q, want dsm_container_project", metadata.TypeName)
	}

	wrong := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: "wrong"}, wrong)
	if !wrong.Diagnostics.HasError() {
		t.Fatal("expected diagnostic for wrong provider data")
	}

	r = NewContainerProjectResource().(*containerProjectResource)
	empty := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{}, empty)
	if empty.Diagnostics.HasError() || r.client != nil {
		t.Fatalf("nil provider data should be ignored: %v", empty.Diagnostics)
	}
}

func TestContainerProjectDataSource_Schema(t *testing.T) {
	d := NewContainerProjectDataSource()
	metadata := &datasource.MetadataResponse{}
	d.Metadata(t.Context(), datasource.MetadataRequest{ProviderTypeName: "dsm"}, metadata)
	if metadata.TypeName != "dsm_container_project" {
		t.Errorf("type name = %q, want dsm_container_project", metadata.TypeName)
	}

	resp := &datasource.SchemaResponse{}
	d.Schema(t.Context(), datasource.SchemaRequest{}, resp)
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}
	attrs := resp.Schema.GetAttributes()
	if !attrs["name"].IsRequired() {
		t.Error("name must be required")
	}
	for _, name := range []string{"id", "share_path", "compose_yaml", "running", "path", "status", "container_ids"} {
		if !attrs[name].IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}
	if !attrs["compose_yaml"].IsSensitive() {
		t.Error("compose_yaml must be sensitive")
	}
}

// containerProjectObject builds a full resource object, leaving every attribute
// that is not named in values as null.
func containerProjectObject(t *testing.T, sch schema.Schema, values map[string]tftypes.Value) tftypes.Value {
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

func containerProjectConfig(t *testing.T, name, sharePath, compose string) tfsdk.Config {
	t.Helper()
	sch := containerProjectSchema(t).Schema
	return tfsdk.Config{Schema: sch, Raw: containerProjectObject(t, sch, map[string]tftypes.Value{
		"name":         tftypes.NewValue(tftypes.String, name),
		"share_path":   tftypes.NewValue(tftypes.String, sharePath),
		"compose_yaml": tftypes.NewValue(tftypes.String, compose),
	})}
}

func TestContainerProjectResource_ValidateConfig(t *testing.T) {
	tests := []struct {
		name      string
		project   string
		sharePath string
		compose   string
		wantError bool
	}{
		{name: "valid", project: "s3-storage", sharePath: "/docker/s3-storage", compose: "services: {}\n"},
		{name: "empty project", project: " ", sharePath: "/docker/s3-storage", compose: "services: {}\n", wantError: true},
		{name: "slash in project", project: "team/storage", sharePath: "/docker/s3-storage", compose: "services: {}\n", wantError: true},
		{name: "project with surrounding space", project: " s3-storage", sharePath: "/docker/s3-storage", compose: "services: {}\n", wantError: true},
		{name: "shared folder root", project: "s3-storage", sharePath: "/docker", compose: "services: {}\n", wantError: true},
		{name: "absolute volume path", project: "s3-storage", sharePath: "/volume1/docker/s3-storage", compose: "services: {}\n", wantError: true},
		{name: "shared folder beginning with volume is valid", project: "s3-storage", sharePath: "/volume-data/s3-storage", compose: "services: {}\n"},
		{name: "relative path", project: "s3-storage", sharePath: "docker/s3-storage", compose: "services: {}\n", wantError: true},
		{name: "trailing slash", project: "s3-storage", sharePath: "/docker/s3-storage/", compose: "services: {}\n", wantError: true},
		{name: "empty compose", project: "s3-storage", sharePath: "/docker/s3-storage", compose: "\n", wantError: true},
	}
	r := NewContainerProjectResource().(*containerProjectResource)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &resource.ValidateConfigResponse{}
			r.ValidateConfig(t.Context(), resource.ValidateConfigRequest{
				Config: containerProjectConfig(t, tt.project, tt.sharePath, tt.compose),
			}, resp)
			if tt.wantError != resp.Diagnostics.HasError() {
				t.Fatalf("HasError = %v, want %v: %v", resp.Diagnostics.HasError(), tt.wantError, resp.Diagnostics)
			}
		})
	}
}

func TestContainerProjectResource_ValidateConfig_WriteOnlyCompose(t *testing.T) {
	sch := containerProjectSchema(t).Schema
	base := map[string]tftypes.Value{
		"name":       tftypes.NewValue(tftypes.String, "s3-storage"),
		"share_path": tftypes.NewValue(tftypes.String, "/docker/s3-storage"),
	}
	with := func(extra map[string]tftypes.Value) map[string]tftypes.Value {
		values := map[string]tftypes.Value{}
		for name, value := range base {
			values[name] = value
		}
		for name, value := range extra {
			values[name] = value
		}
		return values
	}

	tests := []struct {
		name      string
		values    map[string]tftypes.Value
		wantError bool
	}{
		{
			name: "write-only compose with version",
			values: with(map[string]tftypes.Value{
				"compose_yaml_wo":         tftypes.NewValue(tftypes.String, "services: {}\n"),
				"compose_yaml_wo_version": tftypes.NewValue(tftypes.Number, 1),
			}),
		},
		{
			name:      "write-only compose without version",
			values:    with(map[string]tftypes.Value{"compose_yaml_wo": tftypes.NewValue(tftypes.String, "services: {}\n")}),
			wantError: true,
		},
		{
			name: "version without write-only compose",
			values: with(map[string]tftypes.Value{
				"compose_yaml":            tftypes.NewValue(tftypes.String, "services: {}\n"),
				"compose_yaml_wo_version": tftypes.NewValue(tftypes.Number, 1),
			}),
			wantError: true,
		},
		{
			name: "both compose forms",
			values: with(map[string]tftypes.Value{
				"compose_yaml":            tftypes.NewValue(tftypes.String, "services: {}\n"),
				"compose_yaml_wo":         tftypes.NewValue(tftypes.String, "services: {}\n"),
				"compose_yaml_wo_version": tftypes.NewValue(tftypes.Number, 1),
			}),
			wantError: true,
		},
		{
			name:      "neither compose form",
			values:    with(nil),
			wantError: true,
		},
		{
			name: "empty write-only compose",
			values: with(map[string]tftypes.Value{
				"compose_yaml_wo":         tftypes.NewValue(tftypes.String, "\n"),
				"compose_yaml_wo_version": tftypes.NewValue(tftypes.Number, 1),
			}),
			wantError: true,
		},
	}

	r := NewContainerProjectResource().(*containerProjectResource)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &resource.ValidateConfigResponse{}
			r.ValidateConfig(t.Context(), resource.ValidateConfigRequest{
				Config: tfsdk.Config{Schema: sch, Raw: containerProjectObject(t, sch, tt.values)},
			}, resp)
			if tt.wantError != resp.Diagnostics.HasError() {
				t.Fatalf("HasError = %v, want %v: %v", resp.Diagnostics.HasError(), tt.wantError, resp.Diagnostics)
			}
		})
	}
}

// TestApplyContainerProject_WriteOnlyComposeStaysOutOfState covers the refresh
// side of issue #104: DSM returns the compose document on every read, and a
// project tracked write-only must keep it out of state while still recording
// the checksum drift detection needs.
func TestApplyContainerProject_WriteOnlyComposeStaysOutOfState(t *testing.T) {
	const compose = "services:\n  db:\n    environment:\n      POSTGRES_PASSWORD: s3cr3t\n"
	project := &client.ContainerProject{
		ID: "uuid", Name: "nextcloud", SharePath: "/containers/nextcloud",
		ComposeYAML: compose, Status: "RUNNING",
	}

	writeOnly := containerProjectResourceModel{ComposeYAMLWOVersion: types.Int64Value(1)}
	var diags diag.Diagnostics
	applyContainerProjectToResourceModel(t.Context(), &writeOnly, project, &diags)
	if diags.HasError() {
		t.Fatalf("apply returned errors: %v", diags)
	}
	if !writeOnly.ComposeYAML.IsNull() || !writeOnly.ComposeYAMLWO.IsNull() {
		t.Errorf("no form of the compose document may reach state, got %+v", writeOnly)
	}
	if writeOnly.ComposeChecksum.ValueString() != sha256Hex(compose) {
		t.Errorf("compose_yaml_checksum = %q, want the checksum of the remote document", writeOnly.ComposeChecksum.ValueString())
	}

	plain := containerProjectResourceModel{}
	applyContainerProjectToResourceModel(t.Context(), &plain, project, &diags)
	if plain.ComposeYAML.ValueString() != compose {
		t.Error("without the write-only marker the document must still be tracked in state")
	}
	if plain.ComposeChecksum.ValueString() != sha256Hex(compose) {
		t.Error("the checksum is reported in both modes")
	}
}

func TestComposeWillChange(t *testing.T) {
	writeOnly := containerProjectResourceModel{
		ComposeYAMLWOVersion: types.Int64Value(1),
		ComposeChecksum:      types.StringValue(sha256Hex("written")),
	}
	tests := []struct {
		name        string
		state       containerProjectResourceModel
		plan        containerProjectResourceModel
		lastWritten string
		want        bool
	}{
		{
			name:  "compose edited in configuration",
			state: containerProjectResourceModel{ComposeYAML: types.StringValue("old")},
			plan:  containerProjectResourceModel{ComposeYAML: types.StringValue("new")},
			want:  true,
		},
		{
			name:  "nothing changed",
			state: containerProjectResourceModel{ComposeYAML: types.StringValue("same")},
			plan:  containerProjectResourceModel{ComposeYAML: types.StringValue("same")},
			want:  false,
		},
		{
			name:  "write-only version bumped",
			state: writeOnly,
			plan:  containerProjectResourceModel{ComposeYAMLWOVersion: types.Int64Value(2)},
			want:  true,
		},
		{
			name: "project edited outside terraform",
			state: containerProjectResourceModel{
				ComposeYAMLWOVersion: types.Int64Value(1),
				ComposeChecksum:      types.StringValue(sha256Hex("edited in Container Manager")),
			},
			plan:        writeOnly,
			lastWritten: sha256Hex("written"),
			want:        true,
		},
		{
			name:        "project still matches the last write",
			state:       writeOnly,
			plan:        writeOnly,
			lastWritten: sha256Hex("written"),
			want:        false,
		},
		{
			name:  "no remembered checksum",
			state: writeOnly,
			plan:  writeOnly,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := composeWillChange(tt.state, tt.plan, tt.lastWritten); got != tt.want {
				t.Errorf("composeWillChange = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestContainerProjectResource_ModifyPlan_MarksRebuiltAttributesUnknown: a
// rebuild restarts containers, so carrying the previous status and container ids
// into the plan both hides the change and risks "inconsistent result after
// apply".
func TestContainerProjectResource_ModifyPlan_MarksRebuiltAttributesUnknown(t *testing.T) {
	sch := containerProjectSchema(t).Schema
	containerIDs := tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{tftypes.NewValue(tftypes.String, "abc")})
	base := map[string]tftypes.Value{
		"id":                      tftypes.NewValue(tftypes.String, "uuid"),
		"name":                    tftypes.NewValue(tftypes.String, "nextcloud"),
		"share_path":              tftypes.NewValue(tftypes.String, "/containers/nextcloud"),
		"compose_yaml_wo_version": tftypes.NewValue(tftypes.Number, 1),
		"compose_yaml_checksum":   tftypes.NewValue(tftypes.String, sha256Hex("written")),
		"status":                  tftypes.NewValue(tftypes.String, "RUNNING"),
		"container_ids":           containerIDs,
	}
	base["running"] = tftypes.NewValue(tftypes.Bool, true)
	variant := func(attribute string, value tftypes.Value) map[string]tftypes.Value {
		values := map[string]tftypes.Value{}
		for name, base := range base {
			values[name] = base
		}
		values[attribute] = value
		return values
	}
	bumped := variant("compose_yaml_wo_version", tftypes.NewValue(tftypes.Number, 2))
	stopped := variant("running", tftypes.NewValue(tftypes.Bool, false))

	state := containerProjectObject(t, sch, base)
	tests := []struct {
		name                string
		state               tftypes.Value
		plan                tftypes.Value
		wantChecksumUnknown bool
		wantStatusUnknown   bool
	}{
		{name: "version bumped", state: state, plan: containerProjectObject(t, sch, bumped), wantChecksumUnknown: true, wantStatusUnknown: true},
		{
			// Stopping the project changes its status and container ids without
			// touching the compose document; Update is called for it all the same.
			name: "project stopped", state: state, plan: containerProjectObject(t, sch, stopped),
			wantStatusUnknown: true,
		},
		{name: "no change", state: state, plan: state},
		{name: "create", state: tftypes.NewValue(sch.Type().TerraformType(t.Context()), nil), plan: state},
		{name: "destroy", state: state, plan: tftypes.NewValue(sch.Type().TerraformType(t.Context()), nil)},
	}

	r := NewContainerProjectResource().(*containerProjectResource)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: tt.plan}}
			r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{
				State: tfsdk.State{Schema: sch, Raw: tt.state},
				Plan:  tfsdk.Plan{Schema: sch, Raw: tt.plan},
			}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("ModifyPlan returned errors: %v", resp.Diagnostics)
			}
			if resp.Plan.Raw.IsNull() {
				return
			}

			var planned containerProjectResourceModel
			if diags := resp.Plan.Get(t.Context(), &planned); diags.HasError() {
				t.Fatalf("reading plan failed: %v", diags)
			}
			if planned.ComposeChecksum.IsUnknown() != tt.wantChecksumUnknown {
				t.Errorf("compose_yaml_checksum unknown = %v, want %v", planned.ComposeChecksum.IsUnknown(), tt.wantChecksumUnknown)
			}
			if planned.Status.IsUnknown() != tt.wantStatusUnknown || planned.ContainerIDs.IsUnknown() != tt.wantStatusUnknown {
				t.Errorf("status/container_ids unknown = %v/%v, want %v",
					planned.Status.IsUnknown(), planned.ContainerIDs.IsUnknown(), tt.wantStatusUnknown)
			}
		})
	}
}

func TestContainerProjectErrorDetail(t *testing.T) {
	tests := []struct {
		err      error
		contains []string
	}{
		{errors.Join(client.ErrContainerProjectNotFound, errors.New(`"missing"`)), []string{"missing", "imported"}},
		{errors.New("project already exists"), []string{"already exists", "Import"}},
		{&client.APIError{Code: 103, API: "SYNO.Docker.Project"}, []string{"ContainerManager", "Virtual DSM"}},
		{&client.APIError{Code: 105, API: "SYNO.Docker.Project"}, []string{"administrator", "shared folder"}},
		{errors.New("connection refused"), []string{"connection refused"}},
	}
	for _, tt := range tests {
		got := containerProjectErrorDetail(tt.err)
		for _, want := range tt.contains {
			if !strings.Contains(got, want) {
				t.Errorf("detail %q does not contain %q", got, want)
			}
		}
	}
}
