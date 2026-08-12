package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
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
	for _, name := range []string{"name", "share_path", "compose_yaml"} {
		if attr := attrs[name]; attr == nil || !attr.IsRequired() {
			t.Errorf("%s must be required", name)
		}
	}
	for _, name := range []string{"running", "delete_on_destroy"} {
		if attr := attrs[name]; attr == nil || !attr.IsOptional() || !attr.IsComputed() {
			t.Errorf("%s must be optional and computed", name)
		}
	}
	for _, name := range []string{"id", "path", "status", "container_ids"} {
		if attr := attrs[name]; attr == nil || !attr.IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}
	if !attrs["compose_yaml"].IsSensitive() {
		t.Error("compose_yaml must be sensitive")
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

func containerProjectConfig(t *testing.T, name, sharePath, compose string) tfsdk.Config {
	t.Helper()
	sch := containerProjectSchema(t).Schema
	objType := sch.Type().TerraformType(context.Background()).(tftypes.Object)
	values := map[string]tftypes.Value{}
	for attrName, typ := range objType.AttributeTypes {
		switch attrName {
		case "name":
			values[attrName] = tftypes.NewValue(tftypes.String, name)
		case "share_path":
			values[attrName] = tftypes.NewValue(tftypes.String, sharePath)
		case "compose_yaml":
			values[attrName] = tftypes.NewValue(tftypes.String, compose)
		default:
			values[attrName] = tftypes.NewValue(typ, nil)
		}
	}
	return tfsdk.Config{Schema: sch, Raw: tftypes.NewValue(objType, values)}
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

func TestContainerProjectErrorDetail(t *testing.T) {
	tests := []struct {
		err      error
		contains []string
	}{
		{errors.Join(client.ErrContainerProjectNotFound, errors.New(`"missing"`)), []string{"missing", "imported"}},
		{errors.New("project already exists"), []string{"already exists", "Import"}},
		{errors.New("api error 103: method unavailable"), []string{"ContainerManager", "Virtual DSM"}},
		{errors.New("api error 105: denied"), []string{"administrator", "shared folder"}},
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
