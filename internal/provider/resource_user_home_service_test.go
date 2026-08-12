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

func TestUserHomeServiceResource_Metadata(t *testing.T) {
	r := NewUserHomeServiceResource()

	resp := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, resp)

	if resp.TypeName != "dsm_user_home_service" {
		t.Errorf("expected type name dsm_user_home_service, got %q", resp.TypeName)
	}
}

func TestUserHomeServiceResource_Schema(t *testing.T) {
	r := NewUserHomeServiceResource()

	resp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}

	// The framework's own validation catches structural mistakes such as an
	// attribute that is neither optional, required, nor computed.
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()

	locationAttr, ok := attrs["location"]
	if !ok {
		t.Fatal("missing attribute location")
	}
	if !locationAttr.IsRequired() {
		t.Error("location must be required: DSM rejects an enable call without it (error 3103)")
	}

	// Everything else is optional with a default, so a minimal config is just
	// location — and none of these may be Required.
	for _, name := range []string{"enable", "enable_recycle_bin", "force", "disable_on_destroy"} {
		attr, ok := attrs[name]
		if !ok {
			t.Errorf("missing attribute %q", name)
			continue
		}
		if attr.IsRequired() {
			t.Errorf("attribute %q must not be required", name)
		}
		if !attr.IsOptional() {
			t.Errorf("attribute %q must be optional", name)
		}
	}

	idAttr, ok := attrs["id"]
	if !ok {
		t.Fatal("missing attribute id")
	}
	if !idAttr.IsComputed() {
		t.Error("id must be computed")
	}
}

func TestUserHomeServiceResource_Configure_NilProviderData(t *testing.T) {
	r := NewUserHomeServiceResource().(*userHomeServiceResource)

	resp := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("should not error on nil ProviderData: %v", resp.Diagnostics)
	}
	if r.client != nil {
		t.Error("client should remain nil when ProviderData is nil")
	}
}

func TestUserHomeServiceResource_Configure_WrongType(t *testing.T) {
	r := NewUserHomeServiceResource().(*userHomeServiceResource)

	resp := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: "not-a-client"}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error for wrong ProviderData type")
	}
	if r.client != nil {
		t.Error("client should remain nil for wrong type")
	}
}

func TestUserHomeServiceDataSource_Metadata(t *testing.T) {
	d := NewUserHomeServiceDataSource()

	resp := &datasource.MetadataResponse{}
	d.Metadata(t.Context(), datasource.MetadataRequest{ProviderTypeName: "dsm"}, resp)

	if resp.TypeName != "dsm_user_home_service" {
		t.Errorf("expected type name dsm_user_home_service, got %q", resp.TypeName)
	}
}

func TestUserHomeServiceDataSource_Schema(t *testing.T) {
	d := NewUserHomeServiceDataSource()

	resp := &datasource.SchemaResponse{}
	d.Schema(t.Context(), datasource.SchemaRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}

	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()
	// A singleton data source takes no arguments: every attribute is computed.
	expected := []string{
		"id", "enable", "location", "enable_recycle_bin",
		"enable_domain", "enable_ldap", "encryption", "personal_photo_enable",
	}
	for _, name := range expected {
		attr, ok := attrs[name]
		if !ok {
			t.Errorf("missing attribute %q", name)
			continue
		}
		if !attr.IsComputed() {
			t.Errorf("attribute %q must be computed", name)
		}
		if attr.IsRequired() {
			t.Errorf("attribute %q must not be required: the data source takes no arguments", name)
		}
	}
}

func TestUserHomeServiceDataSource_Configure_WrongType(t *testing.T) {
	d := NewUserHomeServiceDataSource().(*userHomeServiceDataSource)

	resp := &datasource.ConfigureResponse{}
	d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: 42}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error for wrong ProviderData type")
	}
	if d.client != nil {
		t.Error("client should remain nil for wrong type")
	}
}

// TestUserHomeErrorDetail checks that bare DSM error codes get the explanation
// that turns them into something actionable.
func TestUserHomeErrorDetail(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantContain []string
	}{
		{
			name:        "3101 points at the volume path format",
			err:         fmt.Errorf("set user home service: %w", &client.APIError{Code: 3101, API: "SYNO.Core.User.Home"}),
			wantContain: []string{"3101", "/volume1", "volume path"},
		},
		{
			name:        "3103 names the missing parameter",
			err:         fmt.Errorf("set user home service: %w", &client.APIError{Code: 3103, API: "SYNO.Core.User.Home"}),
			wantContain: []string{"3103", "`location`", "required"},
		},
		{
			name:        "119 mentions the built-in admin requirement",
			err:         fmt.Errorf("get user home service: %w", &client.APIError{Code: 119, API: "SYNO.Core.User.Home"}),
			wantContain: []string{"119", "built-in `admin`", "Control Panel"},
		},
		{
			name:        "unknown errors pass through unchanged",
			err:         errors.New("http request: connection refused"),
			wantContain: []string{"connection refused"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := userHomeErrorDetail(tt.err)
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("detail should mention %q, got:\n%s", want, got)
				}
			}
			// The original message must always survive.
			if !strings.Contains(got, tt.err.Error()) {
				t.Errorf("detail must retain the original error, got:\n%s", got)
			}
		})
	}
}

// TestUserHomeErrorDetail_UnknownErrorIsVerbatim guards against accidentally
// appending advice to errors we know nothing about.
func TestUserHomeErrorDetail_UnknownErrorIsVerbatim(t *testing.T) {
	err := errors.New("api error 999: synology api error: code 999")
	if got := userHomeErrorDetail(err); got != err.Error() {
		t.Errorf("unknown errors must be returned verbatim, got:\n%s", got)
	}
}
