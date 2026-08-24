package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
)

func TestFirewallRulesDataSource_Metadata(t *testing.T) {
	d := NewFirewallRulesDataSource()

	req := datasource.MetadataRequest{
		ProviderTypeName: "dsm",
	}
	resp := &datasource.MetadataResponse{}

	d.Metadata(t.Context(), req, resp)

	if resp.TypeName != "dsm_firewall_rules" {
		t.Errorf("expected type name dsm_firewall_rules, got %q", resp.TypeName)
	}
}

func TestFirewallRulesDataSource_Schema(t *testing.T) {
	d := NewFirewallRulesDataSource()

	req := datasource.SchemaRequest{}
	resp := &datasource.SchemaResponse{}

	d.Schema(t.Context(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}

	attrs := resp.Schema.GetAttributes()

	topLevel := []string{"id", "profile", "firewall_enabled", "profile_active", "default_policy", "rules"}
	for _, attr := range topLevel {
		if _, ok := attrs[attr]; !ok {
			t.Errorf("missing top-level attribute %q", attr)
		}
	}

	if attr, ok := attrs["profile"]; ok {
		if !attr.IsOptional() {
			t.Errorf("profile should be optional, required=%v computed=%v", attr.IsRequired(), attr.IsComputed())
		}
	}

	// #123: the fall-through policy decides what a rule list means, so an audit
	// read has to report it.
	if attr := attrs["default_policy"]; attr == nil || !attr.IsComputed() {
		t.Error("default_policy must be computed")
	} else if got := attr.GetType().String(); got != "types.MapType[basetypes.StringType]" {
		t.Errorf("default_policy type = %s, want a map of strings", got)
	}

	rulesAttr, ok := attrs["rules"]
	if !ok {
		t.Fatal("missing rules attribute")
	}
	// A ListNestedAttribute exposes its nested object; verify through the
	// schema type description rather than depending on framework internals.
	if rulesAttr.GetType().String() == "" {
		t.Error("rules attribute has no type")
	}
}

func TestFirewallRulesDataSource_Configure_NilProviderData(t *testing.T) {
	ds := NewFirewallRulesDataSource().(*firewallRulesDataSource)

	req := datasource.ConfigureRequest{}
	resp := &datasource.ConfigureResponse{}

	ds.Configure(t.Context(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("nil provider data must not error: %v", resp.Diagnostics)
	}

	if ds.client != nil {
		t.Error("client must stay nil with nil provider data")
	}
}
