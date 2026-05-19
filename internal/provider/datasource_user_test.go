package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
)

func TestUserDataSource_Metadata(t *testing.T) {
	d := NewUserDataSource()

	req := datasource.MetadataRequest{
		ProviderTypeName: "dsm",
	}
	resp := &datasource.MetadataResponse{}

	d.Metadata(t.Context(), req, resp)

	if resp.TypeName != "dsm_user" {
		t.Errorf("expected type name dsm_user, got %q", resp.TypeName)
	}
}

func TestUserDataSource_Schema(t *testing.T) {
	d := NewUserDataSource()

	req := datasource.SchemaRequest{}
	resp := &datasource.SchemaResponse{}

	d.Schema(t.Context(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}

	attrs := resp.Schema.GetAttributes()
	requiredAttrs := []string{"name"}
	for _, attr := range requiredAttrs {
		if _, ok := attrs[attr]; !ok {
			t.Errorf("missing required attribute %q", attr)
		}
	}

	computedAttrs := []string{"id", "description", "email", "disabled", "groups", "uid"}
	for _, attr := range computedAttrs {
		if _, ok := attrs[attr]; !ok {
			t.Errorf("missing computed attribute %q", attr)
		}
	}
}

func TestUserDataSource_Configure_NilProviderData(t *testing.T) {
	ds := NewUserDataSource().(*userDataSource)

	req := datasource.ConfigureRequest{}
	resp := &datasource.ConfigureResponse{}

	ds.Configure(t.Context(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("should not error on nil ProviderData: %v", resp.Diagnostics)
	}
	if ds.client != nil {
		t.Error("client should remain nil when ProviderData is nil")
	}
}
