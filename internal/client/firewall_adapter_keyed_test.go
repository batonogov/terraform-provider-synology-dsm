package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The adapter-keyed profile, captured from a live virtual DSM 7.2.2 for issue
// #130 (.pi/recon-firewall-vdsm-2026-08-24.md). This is the exact body DSM
// answered `Profile get&name=default` with, and it is what made the old client
// fail: it looks for `rules` and `adapterPolicyMap`, finds neither, and DSM
// answers `success: true` to the shape it does not understand.
const capturedAdapterKeyedProfile = `{"data":{"global":{"policy":"none","rules":[]},"name":"default"},"success":true}`

// rawProfileServer answers Firewall.Profile `get` with a literal body, so a
// captured response can be replayed byte for byte rather than round-tripped
// through a Go map first.
func rawProfileServer(t *testing.T, body string) *Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("api") == "SYNO.Core.Security.Firewall.Profile" && r.FormValue("method") == "get" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
	}))
	t.Cleanup(server.Close)

	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")
	return c
}

// The response a real DSM gives, read verbatim. `global` carries its policy as
// the string "none" and its rules as an empty array; there is no `rules` key and
// no `adapterPolicyMap` key at the top level at all.
func TestClient_GetFirewallProfile_ParsesCapturedAdapterKeyedResponse(t *testing.T) {
	c := rawProfileServer(t, capturedAdapterKeyedProfile)

	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("the shape a live DSM answers with must parse: %v", err)
	}

	if profile.Name != "default" {
		t.Errorf("Name = %q, want %q", profile.Name, "default")
	}
	if profile.shape != firewallShapeAdapterKeyed {
		t.Errorf("shape = %v, want adapter-keyed", profile.shape)
	}
	if got, ok := profile.AdapterPolicy[FirewallAdapterGlobal]; !ok || got != fwPolicyNone {
		t.Errorf("policy for %q = %d (present %t), want none (%d)", FirewallAdapterGlobal, got, ok, fwPolicyNone)
	}
	if rules, ok := profile.Rules[FirewallAdapterGlobal]; !ok || len(rules) != 0 {
		t.Errorf("rules for %q = %v (present %t), want an empty list", FirewallAdapterGlobal, rules, ok)
	}
	// The rule list was empty, not absent: DSM did mention rules. Getting this
	// backwards would point the "DSM stored nothing" diagnostic at the wrong
	// hypothesis.
	if !profile.HasRulesKey() {
		t.Error("HasRulesKey() = false, but the captured response carries a rules key")
	}
}

// DSM's per-adapter policy is a string in this shape, and "drop" is the spelling
// the provider calls "deny". Reading it as anything else would report a locked
// down interface as open.
func TestClient_GetFirewallProfile_ParsesAdapterKeyedPolicyStrings(t *testing.T) {
	c := rawProfileServer(t, `{"data":{"name":"custom",`+
		`"eth0":{"policy":"drop","rules":[]},`+
		`"wlan0":{"policy":"allow","rules":[]},`+
		`"global":{"policy":"none","rules":[]}},"success":true}`)

	profile, err := c.GetFirewallProfile(context.Background(), "custom")
	if err != nil {
		t.Fatalf("GetFirewallProfile: %v", err)
	}

	want := map[string]string{
		"eth0":                FirewallPolicyDeny,
		"wlan0":               FirewallPolicyAllow,
		FirewallAdapterGlobal: FirewallPolicyNone,
	}
	if got := profile.DefaultPolicyNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultPolicyNames() = %v, want %v", got, want)
	}
}

// adapterKeyedFixture is a stateful mock DSM speaking the adapter-keyed shape,
// so a write can be inspected as it was actually sent.
type adapterKeyedFixture struct {
	enabled       bool
	activeProfile string
	profile       map[string]interface{}

	// lastSet is the raw `profile` form value of the most recent write, and sets
	// counts how many arrived. A write that must never happen is checked with
	// sets rather than with an error alone: an error after the payload was on the
	// wire is exactly the thing that crashes synoscgi.
	lastSet string
	sets    int
	applies int

	// switchError makes SYNO.Core.Security.Firewall `set` answer a DSM error
	// code, so the client's handling of it can be asserted.
	switchError int
}

func newAdapterKeyedFixture(t *testing.T, profile map[string]interface{}) (*Client, *adapterKeyedFixture) {
	t.Helper()

	f := &adapterKeyedFixture{enabled: false, activeProfile: "default", profile: profile}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		api, method := r.FormValue("api"), r.FormValue("method")

		switch {
		case api == "SYNO.API.Auth" && method == "login":
			writeAPIData(w, map[string]interface{}{"sid": "test-sid", "synotoken": "test-token"})

		case api == "SYNO.Core.Security.Firewall" && method == "get":
			writeAPIData(w, map[string]interface{}{"enable_firewall": f.enabled, "profile_name": f.activeProfile})

		case api == "SYNO.Core.Security.Firewall" && method == "set":
			if f.switchError != 0 {
				_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: f.switchError}})
				return
			}
			f.enabled = r.FormValue("enable_firewall") == "true"
			f.activeProfile = strings.Trim(r.FormValue("profile_name"), `"`)
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.Security.Firewall.Profile" && method == "get":
			writeAPIData(w, f.profile)

		case api == "SYNO.Core.Security.Firewall.Profile" && method == "set":
			f.sets++
			f.lastSet = r.FormValue("profile")
			var incoming map[string]interface{}
			if err := json.Unmarshal([]byte(f.lastSet), &incoming); err != nil {
				_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
				return
			}
			f.profile = incoming
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.Security.Firewall.Profile.Apply" && method == "start":
			f.applies++
			writeAPIData(w, map[string]interface{}{"task_id": "task-1"})

		case api == "SYNO.Core.Security.Firewall.Profile.Apply" && method == "status":
			writeAPIData(w, map[string]interface{}{"finish": true})

		default:
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})
		}
	}))
	t.Cleanup(server.Close)

	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")
	return c, f
}

// capturedAdapterKeyedState is the profile the fixture starts from: the captured
// vDSM response plus a physical adapter and two keys the provider knows nothing
// about.
func capturedAdapterKeyedState() map[string]interface{} {
	return map[string]interface{}{
		"name":              "default",
		"global":            map[string]interface{}{"policy": "none", "rules": []interface{}{}},
		"eth0":              map[string]interface{}{"policy": "allow", "rules": []interface{}{}, "unknownAdapterKey": "keep-me"},
		"unknownProfileKey": "keep-me-too",
	}
}

// The write must go out in the shape the read came back in. This is the fix for
// issue #130: the old client sent `rules` + `adapterPolicyMap` to a DSM that
// speaks neither, and DSM answered success and stored nothing.
func TestClient_SetFirewall_WritesTheAdapterKeyedShapeItRead(t *testing.T) {
	c, f := newAdapterKeyedFixture(t, capturedAdapterKeyedState())

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       false,
		DefaultPolicy: map[string]string{"eth0": FirewallPolicyDeny},
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

	// Neither of the on-disk keys may appear: they are what DSM ignores.
	for _, forbidden := range []string{"rules", "adapterPolicyMap"} {
		if _, ok := sent[forbidden]; ok {
			t.Errorf("payload carries the on-disk key %q, which this DSM ignores: %s", forbidden, f.lastSet)
		}
	}

	eth0, ok := sent["eth0"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload has no eth0 adapter block: %s", f.lastSet)
	}
	// "drop" is DSM's spelling; "deny" is the provider's, and sending it would be
	// an unrecognised policy word.
	if eth0["policy"] != "drop" {
		t.Errorf("eth0 policy = %v, want %q", eth0["policy"], "drop")
	}
	if rules, ok := eth0["rules"].([]interface{}); !ok || len(rules) != 0 {
		t.Errorf("eth0 rules = %v, want an empty array", eth0["rules"])
	}
	if sent["name"] != "default" {
		t.Errorf("name = %v, want %q", sent["name"], "default")
	}

	// And the round trip closes: reading it back yields what was asked for.
	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetFirewallProfile after write: %v", err)
	}
	if got, _ := profile.DefaultPolicyName("eth0"); got != FirewallPolicyDeny {
		t.Errorf("eth0 policy read back as %q, want %q", got, FirewallPolicyDeny)
	}
	if got, _ := profile.DefaultPolicyName(FirewallAdapterGlobal); got != FirewallPolicyNone {
		t.Errorf("global policy read back as %q, want %q — an untouched adapter must not move", got, FirewallPolicyNone)
	}
}

// Keys the provider does not model must survive a write, at both levels. DSM's
// profile object is not fully known, and a write that dropped what it did not
// recognise would quietly discard settings on every apply.
func TestClient_SetFirewall_AdapterKeyedPreservesUnknownKeys(t *testing.T) {
	c, f := newAdapterKeyedFixture(t, capturedAdapterKeyedState())

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       false,
		DefaultPolicy: map[string]string{"eth0": FirewallPolicyDeny},
	}); err != nil {
		t.Fatalf("SetFirewall: %v", err)
	}

	var sent map[string]interface{}
	if err := json.Unmarshal([]byte(f.lastSet), &sent); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}

	if sent["unknownProfileKey"] != "keep-me-too" {
		t.Errorf("an unknown profile key was dropped: %s", f.lastSet)
	}
	eth0, _ := sent["eth0"].(map[string]interface{})
	if eth0["unknownAdapterKey"] != "keep-me" {
		t.Errorf("an unknown adapter key was dropped: %s", f.lastSet)
	}
}

// A DSM that answers in the on-disk shape must still be read and written in it.
// The choice is made from the response, not from a version number, because the
// webapi shim that renders the profile is not published and nothing says every
// build renders it the same way.
func TestClient_SetFirewall_RulesMapShapeIsWrittenBackUnchanged(t *testing.T) {
	c, f := newAdapterKeyedFixture(t, map[string]interface{}{
		"name":             "default",
		"rules":            map[string]interface{}{FirewallAdapterGlobal: []interface{}{allowAllRule("keep")}},
		"adapterPolicyMap": map[string]interface{}{"eth0": float64(fwPolicyAllow)},
	})

	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetFirewallProfile: %v", err)
	}
	if profile.shape != firewallShapeRulesMap {
		t.Fatalf("shape = %v, want the on-disk rules map", profile.shape)
	}

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       false,
		DefaultPolicy: map[string]string{"eth0": FirewallPolicyDeny},
	}); err != nil {
		t.Fatalf("SetFirewall on the on-disk shape must still work: %v", err)
	}

	var sent map[string]interface{}
	if err := json.Unmarshal([]byte(f.lastSet), &sent); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	policies, ok := sent["adapterPolicyMap"].(map[string]interface{})
	if !ok {
		t.Fatalf("the on-disk shape lost adapterPolicyMap: %s", f.lastSet)
	}
	if policies["eth0"] != float64(fwPolicyDrop) {
		t.Errorf("eth0 policy = %v, want the integer %d", policies["eth0"], fwPolicyDrop)
	}
	// The integer encoding, not the string one: mixing them would be the same
	// class of mistake in the other direction.
	if _, isString := policies["eth0"].(string); isString {
		t.Error("the on-disk shape must carry the policy as an integer, not a string")
	}
	rules, ok := sent["rules"].(map[string]interface{})
	if !ok || len(rules[FirewallAdapterGlobal].([]interface{})) != 1 {
		t.Errorf("the existing rule did not survive the write: %s", f.lastSet)
	}
}

// A rule written into the adapter-keyed shape goes out in DSM's own encoding:
// ten snake_case fields with string values, exactly what DSM's web client sends
// and what a physical NAS accepted and read back (issue #130).
func TestClient_SetFirewallRule_AdapterKeyedWritesDSMsEncoding(t *testing.T) {
	shrinkFirewallVerify(t)

	c, f := newAdapterKeyedFixture(t, capturedAdapterKeyedState())

	if _, err := c.SetFirewallRule(context.Background(), SetFirewallRuleRequest{
		Profile: "default",
		Adapter: "eth0",
		Rule: FirewallRule{
			Name: "web", Enabled: true, Action: FirewallActionAllow,
			Protocol: FirewallProtocolTCP, Ports: []string{"443", "8000-8100"},
			Sources: []string{"10.0.0.0/24"}, Priority: 0,
		},
		AllowLockout: true,
	}); err != nil {
		t.Fatalf("SetFirewallRule: %v", err)
	}
	if f.sets != 1 {
		t.Fatalf("profile writes = %d, want 1", f.sets)
	}

	var sent map[string]interface{}
	if err := json.Unmarshal([]byte(f.lastSet), &sent); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	block, _ := sent["eth0"].(map[string]interface{})
	rules, _ := block["rules"].([]interface{})
	if len(rules) != 1 {
		t.Fatalf("eth0 carries %d rule(s), want 1: %s", len(rules), f.lastSet)
	}
	rule, _ := rules[0].(map[string]interface{})

	for key, want := range map[string]interface{}{
		"name": "web", "enable": true, "policy": "allow", "protocol": "tcp",
		"port_group": "custom", "ports": "443,8000-8100", "port_direction": "destination",
		"source_ip_group": "netmask", "source_ip": "10.0.0.0/24",
	} {
		if rule[key] != want {
			t.Errorf("rule[%q] = %#v, want %#v", key, rule[key], want)
		}
	}

	// The round trip closes through the fixture, which stores what it was sent.
	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetFirewallProfile: %v", err)
	}
	got := profile.Rules["eth0"]
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("rules read back = %+v, want the rule just written", got)
	}
	if !reflect.DeepEqual(got[0].Ports, []string{"443", "8000-8100"}) {
		t.Errorf("ports read back as %v", got[0].Ports)
	}
	if !reflect.DeepEqual(got[0].Sources, []string{"10.0.0.0/24"}) {
		t.Errorf("sources read back as %v", got[0].Sources)
	}
}

// A profile that already holds rules created in the DSM UI keeps them, and the
// selectors this provider cannot model — a service preset, a GeoIP country —
// go back exactly as DSM sent them rather than being replaced with a guess.
//
// A `set` carries the whole profile, so this is what decides whether the
// resource is usable on a NAS that actually uses its firewall.
func TestClient_SetFirewall_AdapterKeyedPreservesRulesItDidNotAuthor(t *testing.T) {
	manual := map[string]interface{}{
		"enable": true, "log": false, "name": "made in the DSM UI", "policy": "allow",
		"port_direction": "destination", "port_group": "service", "ports": "ssh,cifs",
		"protocol": "tcp", "source_ip": "RU", "source_ip_group": "geoip",
		"someFutureField": "keep-me",
	}

	state := capturedAdapterKeyedState()
	state["eth0"] = map[string]interface{}{
		"policy": "allow",
		"rules":  []interface{}{manual},
	}

	c, f := newAdapterKeyedFixture(t, state)

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
	block, ok := sent["eth0"].(map[string]interface{})
	if !ok {
		t.Fatalf("eth0 block missing from the payload: %#v", sent)
	}
	rules, ok := block["rules"].([]interface{})
	if !ok || len(rules) != 1 {
		t.Fatalf("eth0 rules = %#v, want the one rule DSM sent", block["rules"])
	}
	rule, _ := rules[0].(map[string]interface{})

	// The unmodelled selectors must be untouched: guessing at a service preset or
	// a country list would rewrite somebody's rule into a different rule.
	for key, want := range map[string]interface{}{
		"port_group": "service", "ports": "ssh,cifs",
		"source_ip_group": "geoip", "source_ip": "RU",
		"someFutureField": "keep-me",
	} {
		if rule[key] != want {
			t.Errorf("rule[%q] = %#v, want %#v — an unmodelled field was rewritten", key, rule[key], want)
		}
	}
	if policy, _ := sent["global"].(map[string]interface{}); policy["policy"] != "drop" {
		t.Errorf("global policy = %v, want drop — the change this write existed for", policy["policy"])
	}
}

// An entry the parser could not read is the one case where presence alone is
// enough to refuse: nothing was kept to hand back, so writing the rest would
// delete it quietly.
func TestClient_SetFirewall_AdapterKeyedRefusesWhenAnEntryWasNotParsed(t *testing.T) {
	state := capturedAdapterKeyedState()
	state["eth0"] = map[string]interface{}{
		"policy": "allow",
		"rules":  []interface{}{"not an object at all"},
	}

	c, f := newAdapterKeyedFixture(t, state)

	_, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       false,
		DefaultPolicy: map[string]string{FirewallAdapterGlobal: FirewallPolicyDeny},
	})

	var unsupported *FirewallRuleWriteUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected *FirewallRuleWriteUnsupportedError, got %T: %v", err, err)
	}
	if f.sets != 0 {
		t.Fatalf("a payload that would have dropped an unread entry was sent %d time(s)", f.sets)
	}
	if len(unsupported.Adapters) != 1 || unsupported.Adapters[0] != "eth0" {
		t.Errorf("Adapters = %v, want [eth0]", unsupported.Adapters)
	}
}

// An adapter DSM never mentioned still gets a `rules` key when the provider
// creates its block, because the write that was actually captured carries
// `"rules": []` on every block and there is nothing else to be faithful to.
func TestClient_SetFirewall_AdapterKeyedAddsRulesKeyToANewAdapter(t *testing.T) {
	c, f := newAdapterKeyedFixture(t, map[string]interface{}{
		"name":   "default",
		"global": map[string]interface{}{"policy": "none", "rules": []interface{}{}},
	})

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       false,
		DefaultPolicy: map[string]string{"ovs_eth0": FirewallPolicyDeny},
	}); err != nil {
		t.Fatalf("SetFirewall: %v", err)
	}

	var sent map[string]interface{}
	if err := json.Unmarshal([]byte(f.lastSet), &sent); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	block, ok := sent["ovs_eth0"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload has no block for the new adapter: %s", f.lastSet)
	}
	if block["policy"] != "drop" {
		t.Errorf("policy = %v, want %q", block["policy"], "drop")
	}
	if rules, ok := block["rules"].([]interface{}); !ok || len(rules) != 0 {
		t.Errorf("rules = %v, want an empty array", block["rules"])
	}
}

// A profile with no rules at all is written without complaint: that is the
// captured, confirmed-working payload, and it is what makes `dsm_firewall`
// usable on a DSM that speaks this shape.
func TestClient_SetFirewall_AdapterKeyedPolicyWriteIsNotRefused(t *testing.T) {
	c, f := newAdapterKeyedFixture(t, capturedAdapterKeyedState())

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       false,
		DefaultPolicy: map[string]string{FirewallAdapterGlobal: FirewallPolicyDeny, "eth0": FirewallPolicyDeny},
	}); err != nil {
		t.Fatalf("a policy-only write on a rule-free profile must go through: %v", err)
	}
	if f.sets != 1 {
		t.Errorf("profile writes = %d, want 1", f.sets)
	}
}

// Removing the last rule an adapter has leaves an empty array, which is the
// payload DSM is confirmed to accept. Refusing it would strand an imported rule
// in state with no way to remove it.
func TestClient_DeleteFirewallRule_AdapterKeyedEmptiesTheList(t *testing.T) {
	shrinkFirewallVerify(t)

	state := capturedAdapterKeyedState()
	state[FirewallAdapterGlobal] = map[string]interface{}{
		"policy": "none",
		"rules":  []interface{}{allowAllRule("the only one")},
	}

	c, f := newAdapterKeyedFixture(t, state)

	if _, err := c.DeleteFirewallRule(context.Background(), "default", FirewallAdapterGlobal, "the only one",
		DeleteFirewallRuleOptions{}); err != nil {
		t.Fatalf("DeleteFirewallRule: %v", err)
	}
	if f.sets != 1 {
		t.Fatalf("profile writes = %d, want 1", f.sets)
	}

	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetFirewallProfile: %v", err)
	}
	if got := len(profile.Rules[FirewallAdapterGlobal]); got != 0 {
		t.Errorf("%d rule(s) left after the delete, want 0", got)
	}
}

// An entry DSM sent that this client could not read still occupies a slot, and
// writing an empty array over it would delete a rule nobody asked to delete.
// That is the one case where the model alone is not enough to decide.
func TestClient_SetFirewall_AdapterKeyedRefusesWhenAnEntryDidNotParse(t *testing.T) {
	state := capturedAdapterKeyedState()
	state["eth0"] = map[string]interface{}{
		"policy": "allow",
		"rules":  []interface{}{"not an object"},
	}

	c, f := newAdapterKeyedFixture(t, state)

	_, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       false,
		DefaultPolicy: map[string]string{FirewallAdapterGlobal: FirewallPolicyDeny},
	})
	if err == nil {
		t.Fatal("expected a refusal: an unreadable rule entry must not be written over")
	}

	var unsupported *FirewallRuleWriteUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected *FirewallRuleWriteUnsupportedError, got %T: %v", err, err)
	}
	if f.sets != 0 {
		t.Errorf("the profile was written %d time(s) despite the refusal", f.sets)
	}
}

// TestClient_SetFirewall_Answers114WithWhatWasLearned covers the finding that
// came out of running this client against a live virtual DSM 7.2.2: the global
// switch answers 114 to every parameter set anyone has published, while `get`
// works in the same session. A bare code reads like a transient failure and
// invites a retry loop, so the client has to say what is actually known.
func TestClient_SetFirewall_Answers114WithWhatWasLearned(t *testing.T) {
	c, f := newAdapterKeyedFixture(t, capturedAdapterKeyedState())
	f.switchError = 114

	_, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:           "default",
		Enabled:           true,
		AllowLockout:      true,
		AllowEmptyRuleSet: true,
	})

	if err == nil {
		t.Fatal("expected the 114 to surface")
	}
	for _, want := range []string{"114", "required parameter is missing", "Control Panel", "devtools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
