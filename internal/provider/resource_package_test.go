package provider

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

func TestPackageResource_Metadata(t *testing.T) {
	r := NewPackageResource()
	resp := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, resp)
	if resp.TypeName != "dsm_package" {
		t.Errorf("type name = %q, want dsm_package", resp.TypeName)
	}
}

func TestPackageResource_Schema(t *testing.T) {
	r := NewPackageResource()
	resp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()
	if attr := attrs["name"]; attr == nil || !attr.IsRequired() {
		t.Error("name must be required")
	}
	for _, name := range []string{"volume", "running", "uninstall_on_destroy"} {
		attr := attrs[name]
		if attr == nil || !attr.IsOptional() || attr.IsRequired() {
			t.Errorf("%s must be optional with a default", name)
		}
	}
	for _, name := range []string{"id", "display_name", "version", "status", "description", "maintainer", "can_uninstall"} {
		attr := attrs[name]
		if attr == nil || !attr.IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}
}

func TestPackageResource_Configure(t *testing.T) {
	r := NewPackageResource().(*packageResource)
	wrong := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: "wrong"}, wrong)
	if !wrong.Diagnostics.HasError() {
		t.Fatal("expected diagnostic for wrong provider data")
	}

	r = NewPackageResource().(*packageResource)
	empty := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{}, empty)
	if empty.Diagnostics.HasError() || r.client != nil {
		t.Fatalf("nil provider data should be ignored: %v", empty.Diagnostics)
	}
}

func TestPackageDataSource_Schema(t *testing.T) {
	d := NewPackageDataSource()
	metadata := &datasource.MetadataResponse{}
	d.Metadata(t.Context(), datasource.MetadataRequest{ProviderTypeName: "dsm"}, metadata)
	if metadata.TypeName != "dsm_package" {
		t.Errorf("type name = %q, want dsm_package", metadata.TypeName)
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
	for _, name := range []string{"id", "display_name", "version", "status", "running", "description", "maintainer", "can_uninstall"} {
		if !attrs[name].IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}
}

func TestPackageErrorDetail(t *testing.T) {
	tests := []struct {
		err      error
		contains []string
	}{
		{fmtPackageNotFound(), []string{"ContainerManager", "Package Center identifier", "NAS model"}},
		{fmt.Errorf("install: %w", &client.APIError{Code: 103, API: "SYNO.Core.Package.Installation"}), []string{"103", "Virtual DSM"}},
		{fmt.Errorf("control: %w", &client.APIError{Code: 105, API: "SYNO.Core.Package.Control"}), []string{"105", "administrator"}},
		{errors.New("connection refused"), []string{"connection refused"}},
	}
	for _, tt := range tests {
		got := packageErrorDetail(tt.err)
		for _, want := range tt.contains {
			if !strings.Contains(got, want) {
				t.Errorf("detail %q does not contain %q", got, want)
			}
		}
	}
}

func fmtPackageNotFound() error {
	return errors.Join(client.ErrPackageNotFound, errors.New(`"ContainerManager"`))
}
