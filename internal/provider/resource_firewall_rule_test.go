package provider

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestFirewallRuleResource_Metadata(t *testing.T) {
	r := NewFirewallRuleResource()
	resp := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, resp)
	if resp.TypeName != "dsm_firewall_rule" {
		t.Errorf("type name = %q, want dsm_firewall_rule", resp.TypeName)
	}
}

func TestFirewallRuleResource_Schema(t *testing.T) {
	r := NewFirewallRuleResource()
	resp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()

	// priority is required rather than computed on purpose: order is the policy,
	// so it must be stated in configuration, not inferred from apply order.
	for _, name := range []string{"name", "priority", "action"} {
		if attr := attrs[name]; attr == nil || !attr.IsRequired() {
			t.Errorf("%s must be required", name)
		}
	}
	for _, name := range []string{"profile", "adapter", "protocol", "ports", "source", "enabled", "allow_lockout", "allow_empty_rule_set"} {
		if attr := attrs[name]; attr == nil || !attr.IsOptional() {
			t.Errorf("%s must be optional", name)
		}
	}
	if attr := attrs["id"]; attr == nil || !attr.IsComputed() {
		t.Error("id must be computed")
	}
}

func TestFirewallRuleDataSource_Schema(t *testing.T) {
	d := NewFirewallRuleDataSource()
	resp := &datasource.SchemaResponse{}
	d.Schema(t.Context(), datasource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()
	for _, name := range []string{"profile", "adapter", "name"} {
		if attr := attrs[name]; attr == nil || !attr.IsRequired() {
			t.Errorf("%s must be required", name)
		}
	}
	for _, name := range []string{"id", "priority", "action", "protocol", "ports", "source", "enabled", "firewall_enabled", "profile_active"} {
		if attr := attrs[name]; attr == nil || !attr.IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}
}

func TestFirewallRuleResource_Configure(t *testing.T) {
	r := NewFirewallRuleResource().(*firewallRuleResource)
	wrong := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: "wrong"}, wrong)
	if !wrong.Diagnostics.HasError() {
		t.Fatal("expected diagnostic for wrong provider data")
	}

	ok := &resource.ConfigureResponse{}
	r.Configure(t.Context(), resource.ConfigureRequest{
		ProviderData: &dsmProviderData{client: client.NewClient("http://dsm:5000", "admin", "", false)},
	}, ok)
	if ok.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", ok.Diagnostics)
	}
	if r.client == nil {
		t.Fatal("client was not stored")
	}
}

// A refusal is only useful if it says what to do next. Each safety error must
// name its own escape hatch, because an operator who cannot find the override
// will reach for something worse.
func TestAppendFirewallDiagnostic_ExplainsTheWayOut(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantContains string
	}{
		{
			name: "lockout",
			err: &client.LockoutError{
				Adapter: "eth0", Profile: "default",
				Source: net.ParseIP("10.210.0.7"), Port: 5001,
				Verdict: client.FirewallVerdict{Reason: "rule 0 denies it"},
			},
			wantContains: "allow_lockout = true",
		},
		{
			name:         "empty rule set",
			err:          &client.EmptyRuleSetError{Profile: "default"},
			wantContains: "allow_empty_rule_set = true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var diags diag.Diagnostics
			appendFirewallDiagnostic(&diags, "Failed", tt.err)

			if !diags.HasError() {
				t.Fatal("expected an error diagnostic")
			}
			joined := diagnosticsText(diags)
			if !strings.Contains(joined, tt.wantContains) {
				t.Errorf("diagnostic does not mention %q:\n%s", tt.wantContains, joined)
			}
			if !strings.Contains(joined, "Nothing was written") {
				t.Errorf("diagnostic does not say the change was not applied:\n%s", joined)
			}
		})
	}
}

// An inconclusive check must warn without failing: the write already happened,
// and a silent success would read as "the provider checked and it is fine".
func TestAppendLockoutWarning(t *testing.T) {
	var diags diag.Diagnostics
	appendLockoutWarning(&diags, nil)
	if len(diags) != 0 {
		t.Fatalf("a nil warning produced diagnostics: %v", diags)
	}

	appendLockoutWarning(&diags, &client.IndeterminateLockoutError{
		Profile: "default",
		Verdict: client.FirewallVerdict{Indeterminate: true, Reason: "rule 0 uses GeoIP"},
	})
	if diags.HasError() {
		t.Fatalf("an inconclusive check must not fail the apply: %v", diags)
	}
	if diags.WarningsCount() != 1 {
		t.Fatalf("expected exactly one warning, got %v", diags)
	}
	if !strings.Contains(diagnosticsText(diags), "applied anyway") {
		t.Errorf("warning does not say the change went through:\n%s", diagnosticsText(diags))
	}
}

// A priority the profile is too short to hold must be said out loud on refresh.
//
// The write path deliberately stays quiet about it: mid-apply it cannot tell an
// unreachable priority from one whose neighbours have not been created yet. A
// refresh has the whole list and no concurrent creates, so the same check is
// unambiguous there — and it has to be made somewhere, because the resulting
// diff never converges on its own.
func TestAppendUnreachablePriorityWarning(t *testing.T) {
	tests := []struct {
		name       string
		configured types.Int64
		actual     int
		ruleCount  int
		wantWarn   bool
	}{
		{name: "within reach", configured: types.Int64Value(2), actual: 2, ruleCount: 3},
		{name: "last position", configured: types.Int64Value(2), actual: 2, ruleCount: 3},
		{
			name: "moved in DSM is ordinary drift, not this warning",
			// The rule was configured for 0 and sits at 2: a real diff, but one an
			// apply does close, so this warning must stay out of it.
			configured: types.Int64Value(0), actual: 2, ruleCount: 3,
		},
		{name: "not yet applied", configured: types.Int64Null(), actual: 0, ruleCount: 3},
		{name: "unknown", configured: types.Int64Unknown(), actual: 0, ruleCount: 3},
		{name: "empty adapter", configured: types.Int64Value(4), actual: 0, ruleCount: 0},
		{
			name: "sparse numbering", configured: types.Int64Value(20), actual: 1, ruleCount: 3,
			wantWarn: true,
		},
		{
			name: "one past the end", configured: types.Int64Value(3), actual: 2, ruleCount: 3,
			wantWarn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var diags diag.Diagnostics
			appendUnreachablePriorityWarning(&diags, "default", "eth0", "web", tt.configured, tt.actual, tt.ruleCount)

			if diags.HasError() {
				t.Fatalf("refresh must not fail over ordering: %v", diags)
			}
			if !tt.wantWarn {
				if len(diags) != 0 {
					t.Fatalf("unexpected warning: %s", diagnosticsText(diags))
				}
				return
			}
			if diags.WarningsCount() != 1 {
				t.Fatalf("expected one warning, got %v", diags)
			}

			text := diagnosticsText(diags)
			for _, want := range []string{
				"web",                                   // which rule
				fmt.Sprint(tt.configured.ValueInt64()),  // what it asked for
				fmt.Sprintf("position %d", tt.actual),   // where it actually is
				fmt.Sprintf("%d rule(s)", tt.ruleCount), // how long the list is
				"will not settle by itself",             // that the diff is permanent
			} {
				if !strings.Contains(text, want) {
					t.Errorf("warning does not mention %q:\n%s", want, text)
				}
			}
		})
	}
}

// diagnosticsText lives in resource_reverse_proxy_test.go — the two resources
// landed in the same release and both need it.
