package client

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Rules exactly as a physical DSM 7 answered `SYNO.Core.Security.Firewall.Profile
// get` with, after each was written through `set` and read back (issue #130).
// Every field name, every value spelling, and every normalisation below was
// observed on that NAS rather than inferred.
const capturedRuleset = `[
 {"enable":true,"log":false,"name":"svc","policy":"allow","port_direction":"destination",
  "port_group":"service","ports":"ssh","protocol":"tcp","source_ip":"all","source_ip_group":"all"},
 {"enable":true,"log":false,"name":"subnet","policy":"allow","port_direction":"destination",
  "port_group":"all","ports":"all","protocol":"all","source_ip":"10.210.102.0/24","source_ip_group":"netmask"},
 {"enable":true,"log":false,"name":"range","policy":"allow","port_direction":"destination",
  "port_group":"custom","ports":"20000:20010","protocol":"udp",
  "source_ip":"10.210.102.5-10.210.102.9","source_ip_group":"iprange"},
 {"enable":false,"log":false,"name":"icmp","policy":"allow","port_direction":"destination",
  "port_group":"all","ports":"all","protocol":"icmp","source_ip":"10.210.102.7","source_ip_group":"ip"},
 {"enable":true,"log":false,"name":"geo","policy":"allow","port_direction":"destination",
  "port_group":"all","ports":"all","protocol":"all","source_ip":"RU","source_ip_group":"geoip"},
 {"enable":true,"log":false,"name":"set","policy":"drop","port_direction":"destination",
  "port_group":"custom","ports":"1234,5678","protocol":"tcp",
  "source_ip":"10.210.102.5,10.210.102.6","source_ip_group":"ipset"}
]`

func parseCapturedRules(t *testing.T) []FirewallRule {
	t.Helper()

	var list []interface{}
	if err := json.Unmarshal([]byte(capturedRuleset), &list); err != nil {
		t.Fatalf("captured ruleset is not JSON: %v", err)
	}
	rules, skipped := parseFirewallRuleList(list)
	if skipped != 0 {
		t.Fatalf("%d captured rule(s) did not parse at all", skipped)
	}
	if len(rules) != 6 {
		t.Fatalf("parsed %d rules, want 6", len(rules))
	}
	return rules
}

func TestParseAdapterKeyedRule_CapturedFromHardware(t *testing.T) {
	rules := parseCapturedRules(t)

	tests := []struct {
		rule     FirewallRule
		action   string
		protocol string
		enabled  bool
		ports    []string
		sources  []string
		portKind firewallSelectorKind
		srcKind  firewallSelectorKind
	}{
		// A service preset names DSM applications, which the provider cannot expand
		// into port numbers — recorded as unmodelled so the lockout replay refuses
		// to guess rather than reading it as "no ports, therefore no match".
		{rules[0], FirewallActionAllow, FirewallProtocolTCP, true, nil, nil, firewallPortUnmodelled, firewallSelectorModelled},
		// A network arrives in prefix form here, unlike the on-disk encoding, which
		// splits it into an address and a dotted netmask.
		{rules[1], FirewallActionAllow, FirewallProtocolAll, true, nil, []string{"10.210.102.0/24"}, firewallSelectorModelled, firewallSelectorModelled},
		// DSM's UI writes a port range with a colon; the provider models the dashed
		// spelling and folds the other into it.
		{rules[2], FirewallActionAllow, FirewallProtocolUDP, true, []string{"20000-20010"}, []string{"10.210.102.5-10.210.102.9"}, firewallSelectorModelled, firewallSelectorModelled},
		{rules[3], FirewallActionAllow, FirewallProtocolICMP, false, nil, []string{"10.210.102.7"}, firewallSelectorModelled, firewallSelectorModelled},
		// GeoIP names countries, which cannot be expanded into addresses.
		{rules[4], FirewallActionAllow, FirewallProtocolAll, true, nil, nil, firewallSelectorModelled, firewallSourceUnmodelled},
		// "drop" is DSM's spelling of what this provider calls deny. Reading it as
		// the default (allow) would make the lockout replay believe a deny rule lets
		// this session through.
		{rules[5], FirewallActionDeny, FirewallProtocolTCP, true, []string{"1234", "5678"}, []string{"10.210.102.5", "10.210.102.6"}, firewallSelectorModelled, firewallSelectorModelled},
	}

	for _, tc := range tests {
		t.Run(tc.rule.Name, func(t *testing.T) {
			if tc.rule.Action != tc.action {
				t.Errorf("Action = %q, want %q", tc.rule.Action, tc.action)
			}
			if tc.rule.Protocol != tc.protocol {
				t.Errorf("Protocol = %q, want %q", tc.rule.Protocol, tc.protocol)
			}
			if tc.rule.Enabled != tc.enabled {
				t.Errorf("Enabled = %v, want %v", tc.rule.Enabled, tc.enabled)
			}
			if !reflect.DeepEqual(tc.rule.Ports, tc.ports) {
				t.Errorf("Ports = %#v, want %#v", tc.rule.Ports, tc.ports)
			}
			if !reflect.DeepEqual(tc.rule.Sources, tc.sources) {
				t.Errorf("Sources = %#v, want %#v", tc.rule.Sources, tc.sources)
			}
			if tc.rule.PortKind != tc.portKind {
				t.Errorf("PortKind = %v, want %v", tc.rule.PortKind, tc.portKind)
			}
			if tc.rule.SourceKind != tc.srcKind {
				t.Errorf("SourceKind = %v, want %v", tc.rule.SourceKind, tc.srcKind)
			}
		})
	}
}

// Every captured rule must survive a write unchanged in the fields DSM keeps.
// This is the property the resource depends on: a refresh that re-rendered a
// rule differently from how DSM stores it would plan a diff on every run.
func TestFirewallRule_AdapterKeyedRoundTrip(t *testing.T) {
	for _, rule := range parseCapturedRules(t) {
		t.Run(rule.Name, func(t *testing.T) {
			rendered, err := rule.toWireAdapterKeyed()
			if err != nil {
				t.Fatalf("toWireAdapterKeyed: %v", err)
			}

			for _, key := range []string{"name", "enable", "policy", "protocol",
				"port_group", "ports", "port_direction", "source_ip", "source_ip_group"} {
				if !reflect.DeepEqual(rendered[key], rule.raw[key]) {
					t.Errorf("%s: rendered %#v, DSM stored %#v", key, rendered[key], rule.raw[key])
				}
			}
		})
	}
}

// The provider spells the deny policy "deny"; DSM spells it "drop" and treats
// "deny" as an unknown word, storing the rule as "none" — which matches nothing,
// so the rule silently stops denying. CONFIRMED on hardware: a rule written with
// policy "deny" came back with policy "none".
func TestFirewallRule_DenyIsWrittenAsDrop(t *testing.T) {
	rule := FirewallRule{Name: "deny all", Enabled: true, Action: FirewallActionDeny, Protocol: FirewallProtocolAll}

	rendered, err := rule.toWireAdapterKeyed()
	if err != nil {
		t.Fatalf("toWireAdapterKeyed: %v", err)
	}
	if rendered["policy"] != "drop" {
		t.Fatalf("policy = %#v, want \"drop\" — DSM stores anything else as \"none\" and the rule stops denying", rendered["policy"])
	}
}

func TestSourcesToWireAdapterKeyed(t *testing.T) {
	tests := []struct {
		name    string
		sources []string
		group   string
		source  string
		wantErr bool
	}{
		{"none", nil, "all", "all", false},
		{"single address", []string{"10.0.0.1"}, "ip", "10.0.0.1", false},
		{"network", []string{"10.0.0.0/24"}, "netmask", "10.0.0.0/24", false},
		{"range", []string{"10.0.0.1-10.0.0.9"}, "iprange", "10.0.0.1-10.0.0.9", false},
		{"several addresses", []string{"10.0.0.1", "10.0.0.2"}, "ipset", "10.0.0.1,10.0.0.2", false},
		// DSM stores one selector per rule, so a list may only hold plain addresses.
		{"several networks", []string{"10.0.0.0/24", "10.0.1.0/24"}, "", "", true},
		{"nonsense", []string{"not-an-address"}, "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			group, source, err := sourcesToWireAdapterKeyed(tc.sources)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q/%q", group, source)
				}
				return
			}
			if err != nil {
				t.Fatalf("sourcesToWireAdapterKeyed: %v", err)
			}
			if group != tc.group || source != tc.source {
				t.Errorf("got %q/%q, want %q/%q", group, source, tc.group, tc.source)
			}
		})
	}
}

func TestPortsToWireAdapterKeyed(t *testing.T) {
	if group, ports := portsToWireAdapterKeyed(nil); group != "all" || ports != "all" {
		t.Errorf("no ports rendered as %q/%q, want all/all", group, ports)
	}
	if group, ports := portsToWireAdapterKeyed([]string{"443", "8000-8100"}); group != "custom" || ports != "443,8000-8100" {
		t.Errorf("rendered %q/%q, want custom/\"443,8000-8100\"", group, ports)
	}
}

// The two encodings must never be confused for one another: reading an on-disk
// rule as adapter-keyed (or the reverse) silently changes what a rule means,
// because `policy` is an integer in one and a string in the other.
func TestLooksLikeAdapterKeyedRule(t *testing.T) {
	onDisk := map[string]interface{}{
		"name": "x", "policy": float64(1), "portGroup": float64(3),
		"portList": []interface{}{}, "ipGroup": float64(5), "ruleIndex": float64(1),
	}
	if looksLikeAdapterKeyedRule(onDisk) {
		t.Error("an on-disk rule was read as adapter-keyed")
	}

	adapterKeyed := map[string]interface{}{
		"name": "x", "policy": "drop", "port_group": "all", "source_ip_group": "all",
	}
	if !looksLikeAdapterKeyedRule(adapterKeyed) {
		t.Error("an adapter-keyed rule was read as on-disk")
	}
}
