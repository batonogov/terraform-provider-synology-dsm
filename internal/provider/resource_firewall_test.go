package provider

import (
	"net"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestFirewallResource_Metadata(t *testing.T) {
	r := NewFirewallResource()
	resp := &resource.MetadataResponse{}
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "dsm"}, resp)
	if resp.TypeName != "dsm_firewall" {
		t.Errorf("type name = %q, want dsm_firewall", resp.TypeName)
	}
}

func TestFirewallResource_Schema(t *testing.T) {
	r := NewFirewallResource()
	resp := &resource.SchemaResponse{}
	r.Schema(t.Context(), resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned errors: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %v", diags)
	}

	attrs := resp.Schema.GetAttributes()

	// enabled has no default on purpose: switching a firewall on or off is not a
	// thing a configuration should do by omission.
	if attr := attrs["enabled"]; attr == nil || !attr.IsRequired() {
		t.Error("enabled must be required")
	}
	for _, name := range []string{"profile", "default_policy", "allow_lockout", "allow_empty_rule_set", "disable_on_destroy"} {
		if attr := attrs[name]; attr == nil || !attr.IsOptional() {
			t.Errorf("%s must be optional", name)
		}
	}
	for _, name := range []string{"id", "default_policy_effective", "active_profile"} {
		if attr := attrs[name]; attr == nil || !attr.IsComputed() {
			t.Errorf("%s must be computed", name)
		}
	}

	// DSM stores the fall-through per adapter (adapterPolicyMap), so the schema
	// says map. A scalar here would be a claim about DSM that is not true.
	if got := attrs["default_policy"].GetType(); got.String() != "types.MapType[basetypes.StringType]" {
		t.Errorf("default_policy type = %s, want a map of strings", got)
	}
}

func TestFirewallResource_Configure(t *testing.T) {
	r := NewFirewallResource().(*firewallResource)

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

// The default policy validator has to name the adapter that failed: a bare
// "invalid value" on a map tells the operator nothing about which line to fix.
func TestFirewallDefaultPolicyValidator(t *testing.T) {
	v := newMapValuesOneOfValidator(
		client.FirewallPolicyAllow, client.FirewallPolicyDeny, client.FirewallPolicyNone)

	good, diags := types.MapValueFrom(t.Context(), types.StringType, map[string]string{
		"eth0": "deny", "global": "none",
	})
	if diags.HasError() {
		t.Fatalf("building the map: %v", diags)
	}
	resp := &validator.MapResponse{}
	v.ValidateMap(t.Context(), validator.MapRequest{ConfigValue: good}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("a valid policy map was rejected: %v", resp.Diagnostics)
	}

	// "drop" is DSM's own spelling; the provider deliberately says "deny" so it
	// reads the same as a rule action, and must not silently accept both.
	bad, diags := types.MapValueFrom(t.Context(), types.StringType, map[string]string{"eth0": "drop"})
	if diags.HasError() {
		t.Fatalf("building the map: %v", diags)
	}
	resp = &validator.MapResponse{}
	v.ValidateMap(t.Context(), validator.MapRequest{ConfigValue: bad}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for an unknown policy name")
	}
	if !strings.Contains(diagnosticsText(resp.Diagnostics), "eth0") {
		t.Errorf("diagnostic does not name the offending adapter:\n%s", diagnosticsText(resp.Diagnostics))
	}
}

// refreshManagedPolicy is what keeps a partial default_policy map from becoming
// a permanent diff: only the adapters the configuration named are read back.
func TestRefreshManagedPolicy(t *testing.T) {
	profile := &client.FirewallProfile{
		AdapterPolicy: map[string]int{"eth0": 1, "eth1": 0, "global": 2},
	}

	managed, diags := types.MapValueFrom(t.Context(), types.StringType, map[string]string{"eth0": "allow"})
	if diags.HasError() {
		t.Fatalf("building the map: %v", diags)
	}

	got, diags := refreshManagedPolicy(t.Context(), managed, profile)
	if diags.HasError() {
		t.Fatalf("refreshManagedPolicy: %v", diags)
	}

	var values map[string]string
	if d := got.ElementsAs(t.Context(), &values, false); d.HasError() {
		t.Fatalf("reading back: %v", d)
	}
	if len(values) != 1 {
		t.Fatalf("unmanaged adapters leaked into state: %v", values)
	}
	if values["eth0"] != client.FirewallPolicyDeny {
		t.Errorf("eth0 = %q, want deny (DSM stores drop as 1)", values["eth0"])
	}

	// An imported resource has no managed map at all; it must stay null rather
	// than adopt every adapter DSM happens to have.
	got, diags = refreshManagedPolicy(t.Context(), types.MapNull(types.StringType), profile)
	if diags.HasError() {
		t.Fatalf("refreshManagedPolicy on null: %v", diags)
	}
	if !got.IsNull() {
		t.Errorf("a null default_policy was populated from DSM: %v", got)
	}

	// An adapter DSM no longer records is dropped, not kept at a stale value: the
	// resulting difference from the configuration is a plan that rewrites it.
	stale, _ := types.MapValueFrom(t.Context(), types.StringType, map[string]string{"eth9": "deny"})
	got, diags = refreshManagedPolicy(t.Context(), stale, profile)
	if diags.HasError() {
		t.Fatalf("refreshManagedPolicy: %v", diags)
	}
	if len(got.Elements()) != 0 {
		t.Errorf("a vanished adapter was kept in state: %v", got)
	}
}

// Each refusal has to name its own way out, and the way out of "the firewall
// would lock you out when switched on" is not the same advice as for a rule.
func TestAppendFirewallSettingsDiagnostic(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantContains []string
	}{
		{
			name: "lockout",
			err: &client.LockoutError{
				Adapter: "eth0", Profile: "default",
				Source: net.ParseIP("10.210.0.7"), Port: 5001,
				Verdict: client.FirewallVerdict{Reason: "no rule matches"},
			},
			wantContains: []string{"allow_lockout = true", "dsm_firewall_rule", "Nothing was written"},
		},
		{
			name:         "empty rule set",
			err:          &client.EmptyRuleSetError{Profile: "default"},
			wantContains: []string{"allow_empty_rule_set = true", "Nothing was written"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var diags diag.Diagnostics
			appendFirewallSettingsDiagnostic(&diags, "Failed", tt.err)
			if !diags.HasError() {
				t.Fatal("expected an error diagnostic")
			}
			joined := diagnosticsText(diags)
			for _, want := range tt.wantContains {
				if !strings.Contains(joined, want) {
					t.Errorf("diagnostic does not mention %q:\n%s", want, joined)
				}
			}
		})
	}
}
