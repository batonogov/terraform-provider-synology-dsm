package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The profile a real DSM 7 answered `Profile get&name=default` with, reported on
// issue #130 after 0.6.0 added the debug logging that could capture it. It is
// adapter-keyed -- `global` is a genuine adapter entry -- but it *also* repeats
// the on-disk shape's two key names as sibling objects carrying that same
// {policy, rules} shape.
//
// This is the response that defeated the first fix. The client decided the shape
// from the presence of `rules`/`adapterPolicyMap`, so it read this NAS as
// on-disk, parsed the keys `policy` and `rules` as adapter names (which is where
// the reported `the profile came back with adapter(s) "policy", "rules"` came
// from), and wrote the profile back in a shape this DSM does not understand --
// answered with `success: true` and discarded, which is the whole of #130.
const capturedHybridProfile = `{"data":{` +
	`"adapterPolicyMap":{"policy":"none","rules":[]},` +
	`"global":{"policy":"none","rules":[]},` +
	`"name":"default",` +
	`"rules":{"policy":"none","rules":[]}` +
	`},"success":true}`

// hybridProfileState is the same profile as a Go map, for the stateful fixture.
func hybridProfileState() map[string]interface{} {
	return map[string]interface{}{
		"adapterPolicyMap": map[string]interface{}{"policy": "none", "rules": []interface{}{}},
		"global":           map[string]interface{}{"policy": "none", "rules": []interface{}{}},
		"name":             "default",
		"rules":            map[string]interface{}{"policy": "none", "rules": []interface{}{}},
	}
}

// The shape must be decided from the values, not from the key names: `global` is
// an adapter, `rules` and `adapterPolicyMap` are not.
func TestClient_GetFirewallProfile_ParsesCapturedHybridResponse(t *testing.T) {
	c := rawProfileServer(t, capturedHybridProfile)

	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("the shape this DSM answers with must parse: %v", err)
	}

	if profile.shape != firewallShapeAdapterKeyed {
		t.Fatalf("shape = %v, want adapter-keyed — `rules` here is an adapter block, not a rules map", profile.shape)
	}
	if profile.Name != "default" {
		t.Errorf("Name = %q, want %q", profile.Name, "default")
	}

	got, ok := profile.DefaultPolicyName(FirewallAdapterGlobal)
	if !ok || got != FirewallPolicyNone {
		t.Errorf("global policy = %q (present %v), want %q", got, ok, FirewallPolicyNone)
	}

	// The two impostors must not become adapters. Reading them as such is what
	// produced the nonsense adapter names in the reported diagnostic, and it would
	// also make a write claim a policy for an interface that does not exist.
	for _, impostor := range []string{"policy", "rules", "adapterPolicyMap"} {
		if _, ok := profile.AdapterPolicy[impostor]; ok {
			t.Errorf("%q was read as an adapter carrying a policy", impostor)
		}
		if _, ok := profile.Rules[impostor]; ok {
			t.Errorf("%q was read as an adapter carrying rules", impostor)
		}
	}
	if len(profile.AdapterPolicy) != 1 {
		t.Errorf("AdapterPolicy = %v, want just the one real adapter", profile.AdapterPolicy)
	}
}

// The policy half round-trips on this NAS the same way it does on virtual DSM:
// the write goes out adapter-keyed, and the two impostor keys ride along
// untouched rather than being reinterpreted as the on-disk shape.
func TestClient_SetFirewall_HybridShapeWritesAdapterKeyed(t *testing.T) {
	c, f := newAdapterKeyedFixture(t, hybridProfileState())

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       false,
		DefaultPolicy: map[string]string{FirewallAdapterGlobal: FirewallPolicyDeny},
	}); err != nil {
		t.Fatalf("SetFirewall: %v", err)
	}
	if f.sets != 1 {
		t.Fatalf("profile writes = %d, want 1", f.sets)
	}

	var sent map[string]interface{}
	if err := json.Unmarshal([]byte(f.lastSet), &sent); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}

	global, ok := sent["global"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload has no global adapter block: %s", f.lastSet)
	}
	if global["policy"] != "drop" {
		t.Errorf("global policy = %v, want %q", global["policy"], "drop")
	}

	// Echoed, not rewritten: the provider does not know what these are, and a
	// write that rendered them as the on-disk shape is the bug being fixed.
	for _, impostor := range []string{"rules", "adapterPolicyMap"} {
		block, ok := sent[impostor].(map[string]interface{})
		if !ok {
			t.Errorf("%q was dropped or rewritten: %s", impostor, f.lastSet)
			continue
		}
		if block["policy"] != "none" {
			t.Errorf("%q was rewritten: %v", impostor, block)
		}
	}

	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetFirewallProfile after write: %v", err)
	}
	if got, _ := profile.DefaultPolicyName(FirewallAdapterGlobal); got != FirewallPolicyDeny {
		t.Errorf("global policy read back as %q, want %q", got, FirewallPolicyDeny)
	}
}

// And a rule is still refused rather than guessed at, without anything reaching
// the wire. Before this fix the same call was sent as an on-disk profile and
// silently discarded; a refusal is the honest answer until a capture of a
// profile that holds rules exists.
func TestClient_SetFirewallRule_HybridShapeRefusesRatherThanDiscards(t *testing.T) {
	shrinkFirewallVerify(t)

	c, f := newAdapterKeyedFixture(t, hybridProfileState())

	_, err := c.SetFirewallRule(context.Background(), SetFirewallRuleRequest{
		Profile: "default",
		Adapter: FirewallAdapterGlobal,
		Rule:    managedDenyRule("Deny everything else", 0),
	})

	var unsupported *FirewallRuleWriteUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected *FirewallRuleWriteUnsupportedError, got %T: %v", err, err)
	}
	if f.sets != 0 {
		t.Fatalf("a payload that crashes synoscgi was sent %d time(s)", f.sets)
	}
}

// The on-disk shape is still recognised when it is genuinely there: `rules`
// mapping adapter names to arrays, `adapterPolicyMap` mapping them to policies.
// The discriminator is the value, so tightening it for the hybrid response must
// not cost the shape it was written for.
func TestClient_GetFirewallProfile_StillReadsTheOnDiskShape(t *testing.T) {
	c := rawProfileServer(t, `{"data":{"name":"default",`+
		`"rules":{"global":[{"name":"keep","policy":1,"ruleIndex":1}],"eth0":null},`+
		`"adapterPolicyMap":{"global":0,"eth0":1}},"success":true}`)

	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetFirewallProfile: %v", err)
	}

	if profile.shape != firewallShapeRulesMap {
		t.Fatalf("shape = %v, want the on-disk rules map", profile.shape)
	}
	if got, _ := profile.DefaultPolicyName(FirewallAdapterGlobal); got != FirewallPolicyAllow {
		t.Errorf("global policy = %q, want %q", got, FirewallPolicyAllow)
	}
	if got, _ := profile.DefaultPolicyName("eth0"); got != FirewallPolicyDeny {
		t.Errorf("eth0 policy = %q, want %q", got, FirewallPolicyDeny)
	}
	if rules := profile.Rules[FirewallAdapterGlobal]; len(rules) != 1 || rules[0].Name != "keep" {
		t.Errorf("global rules = %+v, want the one rule DSM sent", rules)
	}
}

// A DSM that answers the on-disk shape with nothing in it is still on-disk:
// `{}` carries no evidence either way, and there is no adapter block to prefer.
func TestClient_GetFirewallProfile_EmptyOnDiskShapeStaysOnDisk(t *testing.T) {
	c := rawProfileServer(t, `{"data":{"name":"default","rules":{},"adapterPolicyMap":{}},"success":true}`)

	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetFirewallProfile: %v", err)
	}
	if profile.shape != firewallShapeRulesMap {
		t.Errorf("shape = %v, want the on-disk rules map", profile.shape)
	}
	if !profile.HasRulesKey() {
		t.Error("DSM sent a rules key; the profile must not report it missing")
	}
}

// A response in neither shape is still an error rather than an empty profile.
// Every write is read-modify-write, so a misread response is written straight
// back over the operator's rules.
func TestClient_GetFirewallProfile_UnrecognisedShapeStillErrors(t *testing.T) {
	c := rawProfileServer(t, `{"data":{"name":"default","somethingElse":{"unrelated":"value"}},"success":true}`)

	_, err := c.GetFirewallProfile(context.Background(), "default")
	var shape *FirewallProfileShapeError
	if !errors.As(err, &shape) {
		t.Fatalf("expected *FirewallProfileShapeError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "somethingElse") {
		t.Errorf("the error must name the keys DSM sent: %s", err.Error())
	}
}
