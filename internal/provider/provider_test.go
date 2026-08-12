package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

var testProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"dsm": providerserver.NewProtocol6WithError(New("test")()),
}

func TestProviderMetadataIncludesBuildVersion(t *testing.T) {
	p := New("1.2.3")()
	resp := &provider.MetadataResponse{}

	p.Metadata(context.Background(), provider.MetadataRequest{}, resp)

	if resp.TypeName != "dsm" {
		t.Fatalf("provider type = %q, want dsm", resp.TypeName)
	}
	if resp.Version != "1.2.3" {
		t.Fatalf("provider version = %q, want 1.2.3", resp.Version)
	}
}
