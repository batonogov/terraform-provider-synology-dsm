package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
)

func TestCertificateServiceResource_Metadata(t *testing.T) {
	r := NewCertificateServiceResource()

	req := resource.MetadataRequest{
		ProviderTypeName: "dsm",
	}
	resp := &resource.MetadataResponse{}

	r.Metadata(t.Context(), req, resp)

	if resp.TypeName != "dsm_certificate_service" {
		t.Errorf("expected type name dsm_certificate_service, got %q", resp.TypeName)
	}
}

func TestCertificateServiceResource_Schema(t *testing.T) {
	r := NewCertificateServiceResource()

	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}

	r.Schema(t.Context(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}

	attrs := resp.Schema.GetAttributes()

	required := []string{"service", "certificate_id"}
	for _, attr := range required {
		a, ok := attrs[attr]
		if !ok {
			t.Errorf("missing required attribute %q", attr)
			continue
		}
		if !a.IsRequired() {
			t.Errorf("attribute %q should be required", attr)
		}
	}

	if a, ok := attrs["id"]; !ok {
		t.Error("missing computed attribute id")
	} else if !a.IsComputed() {
		t.Error("id should be computed")
	}
}

func TestCertificateServiceResource_Configure_NilProviderData(t *testing.T) {
	r := NewCertificateServiceResource().(*certificateServiceResource)

	req := resource.ConfigureRequest{}
	resp := &resource.ConfigureResponse{}

	r.Configure(t.Context(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("nil provider data must not error: %v", resp.Diagnostics)
	}

	if r.client != nil {
		t.Error("client must stay nil with nil provider data")
	}
}
