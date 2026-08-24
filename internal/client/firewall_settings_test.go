package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// firewallSwitchCall records one SYNO.Core.Security.Firewall `set`, so the tests
// can assert the exact wire contract rather than only its effect. The write side
// of that API is reconstructed rather than captured, which makes pinning the
// payload the only defence against it drifting silently.
type firewallSwitchCall struct {
	Method         string // HTTP verb
	EnableFirewall string
	ProfileName    string
}

// settingsFixture is a stateful mock DSM covering the profile-level APIs.
//
// It is deliberately separate from firewallFixture: this one has to answer the
// global switch's `set`, record every call, and be able to fail a verb on demand.
type settingsFixture struct {
	mu            sync.Mutex
	enabled       bool
	activeProfile string
	profiles      map[string]map[string]interface{}

	switchCalls []firewallSwitchCall
	profileSets atomic.Int64
	applies     atomic.Int64

	// rejectPOST makes the switch answer 103 to a POST, so the GET fallback can
	// be exercised.
	rejectPOST bool
	// requireQuotedProfileName makes the switch answer 2001 to a bare
	// profile_name, modelling a DSM that wants the value JSON-quoted.
	requireQuotedProfileName bool
	// switchMethod is the method name the fixture accepts for the global switch.
	switchMethod string
	// discardSwitch makes the global switch record the call, answer success, and
	// change nothing -- the silent no-op of issue #130.
	discardSwitch bool
	// discardProfileSet does the same for the profile write.
	discardProfileSet bool
}

func newSettingsFixture(t *testing.T, enabled bool, profiles map[string]map[string]interface{}) (*Client, *settingsFixture, *httptest.Server) {
	t.Helper()

	f := &settingsFixture{
		enabled:       enabled,
		activeProfile: "default",
		profiles:      profiles,
		switchMethod:  "set",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		api := r.FormValue("api")
		method := r.FormValue("method")

		f.mu.Lock()
		switchMethod, rejectPOST := f.switchMethod, f.rejectPOST
		requireQuoted := f.requireQuotedProfileName
		f.mu.Unlock()

		switch {
		case api == "SYNO.API.Auth" && method == "login":
			writeAPIData(w, map[string]interface{}{"sid": "test-sid", "synotoken": "test-token"})

		case api == "SYNO.Core.Security.Firewall" && method == "get":
			f.mu.Lock()
			data := map[string]interface{}{"enable_firewall": f.enabled, "profile_name": f.activeProfile}
			f.mu.Unlock()
			writeAPIData(w, data)

		case api == "SYNO.Core.Security.Firewall" && method == switchMethod:
			if rejectPOST && r.Method == http.MethodPost {
				_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 103}})
				return
			}
			if requireQuoted && !strings.HasPrefix(r.FormValue("profile_name"), `"`) {
				_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 2001}})
				return
			}

			f.mu.Lock()
			f.switchCalls = append(f.switchCalls, firewallSwitchCall{
				Method:         r.Method,
				EnableFirewall: r.FormValue("enable_firewall"),
				ProfileName:    r.FormValue("profile_name"),
			})
			if !f.discardSwitch {
				f.enabled = r.FormValue("enable_firewall") == "true"
				if name := unquoteFormValue(r.FormValue("profile_name")); name != "" {
					f.activeProfile = name
				}
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.Security.Firewall" && method == "set":
			// Reached only when switchMethod was changed, i.e. the "DSM does not
			// know `set`" case.
			_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 103}})

		case api == "SYNO.Core.Security.Firewall.Profile" && method == "get":
			f.mu.Lock()
			profile, ok := f.profiles[r.FormValue("name")]
			raw, _ := json.Marshal(profile)
			f.mu.Unlock()
			if !ok {
				_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
				return
			}
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Security.Firewall.Profile" && method == "set":
			var incoming map[string]interface{}
			if err := json.Unmarshal([]byte(r.FormValue("profile")), &incoming); err != nil {
				_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
				return
			}
			f.profileSets.Add(1)
			f.mu.Lock()
			if !f.discardProfileSet {
				f.profiles[incoming["name"].(string)] = incoming
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.Security.Firewall.Profile.Apply" && method == "start":
			f.applies.Add(1)
			writeAPIData(w, map[string]interface{}{"task_id": "task-1"})

		case api == "SYNO.Core.Security.Firewall.Profile.Apply" && method == "status":
			writeAPIData(w, map[string]interface{}{"finish": true})

		case api == "SYNO.Core.Security.Firewall.Profile.Apply" && method == "stop":
			_ = json.NewEncoder(w).Encode(APIResponse{Success: true})

		default:
			_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
		}
	})

	server := httptest.NewServer(mux)
	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")

	// Prime the transport so the lockout guard knows the session's own address.
	if _, err := c.GetFirewallSettings(context.Background()); err != nil {
		t.Fatalf("priming request failed: %v", err)
	}

	return c, f, server
}

// unquoteFormValue models DSM storing the decoded value: whichever encoding it
// accepted on the wire, `get` answers with a bare name.
func unquoteFormValue(v string) string {
	var decoded string
	if err := json.Unmarshal([]byte(v), &decoded); err == nil {
		return decoded
	}
	return v
}

// profileDoc builds one profile document for the fixture.
func profileDoc(name string, policy map[string]int, rules map[string][]map[string]interface{}) map[string]interface{} {
	wireRules := map[string]interface{}{}
	for adapter, list := range rules {
		items := make([]interface{}, len(list))
		for i, rule := range list {
			items[i] = rule
		}
		wireRules[adapter] = items
	}
	wirePolicy := map[string]interface{}{}
	for adapter, value := range policy {
		wirePolicy[adapter] = float64(value)
	}
	return map[string]interface{}{
		"name":              name,
		"rules":             wireRules,
		"adapterPolicyMap":  wirePolicy,
		"unknownProfileKey": "keep-me",
	}
}

func (f *settingsFixture) calls() []firewallSwitchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]firewallSwitchCall, len(f.switchCalls))
	copy(out, f.switchCalls)
	return out
}

// TestClient_SetFirewall_EnablePayload pins the reconstructed write contract of
// SYNO.Core.Security.Firewall: both fields always travel together, as strings,
// on a POST — and the profile is applied afterwards, because a switch flipped on
// a profile DSM has not rendered enforces nothing.
func TestClient_SetFirewall_EnablePayload(t *testing.T) {
	c, f, server := newSettingsFixture(t, false, map[string]map[string]interface{}{
		"default": profileDoc("default",
			map[string]int{FirewallAdapterGlobal: fwPolicyNone, "eth0": fwPolicyDrop},
			map[string][]map[string]interface{}{"eth0": {allowAllRule("baseline allow")}}),
	})
	defer server.Close()

	result, err := c.SetFirewall(context.Background(), SetFirewallRequest{Profile: "default", Enabled: true})
	if err != nil {
		t.Fatalf("SetFirewall: %v", err)
	}
	if !result.Settings.Enabled {
		t.Errorf("firewall not reported as enabled after the write")
	}

	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one switch write, got %d", len(calls))
	}
	got := calls[0]
	if got.Method != http.MethodPost {
		t.Errorf("HTTP method = %s, want POST", got.Method)
	}
	if got.EnableFirewall != "true" {
		t.Errorf("enable_firewall = %q, want \"true\"", got.EnableFirewall)
	}
	// The whole object travels even though only the switch changed: DSM's
	// neighbouring settings APIs reject a partial write.
	if got.ProfileName != "default" {
		t.Errorf("profile_name = %q, want \"default\"", got.ProfileName)
	}

	if f.applies.Load() == 0 {
		t.Errorf("the firewall was switched on but the profile was never applied")
	}
	// Nothing about the profile changed, so it must not have been rewritten.
	if f.profileSets.Load() != 0 {
		t.Errorf("the profile was saved although no default policy changed")
	}
}

// The verb is a guess, so the fallback has to work. DSM answering 103 to the
// POST must be followed by a GET carrying the same parameters.
func TestClient_SetFirewall_FallsBackToGET(t *testing.T) {
	c, f, server := newSettingsFixture(t, false, map[string]map[string]interface{}{
		"default": profileDoc("default",
			map[string]int{"eth0": fwPolicyDrop},
			map[string][]map[string]interface{}{"eth0": {allowAllRule("baseline allow")}}),
	})
	defer server.Close()

	f.mu.Lock()
	f.rejectPOST = true
	f.mu.Unlock()

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{Profile: "default", Enabled: true}); err != nil {
		t.Fatalf("SetFirewall did not fall back to GET: %v", err)
	}

	calls := f.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodGet {
		t.Fatalf("expected one GET after the POST was refused, got %+v", calls)
	}
	if calls[0].EnableFirewall != "true" || calls[0].ProfileName != "default" {
		t.Errorf("the fallback dropped parameters: %+v", calls[0])
	}
}

// `set` is listed in DSM's own webapi descriptor for this API, so a 103 over
// both verbs is a real finding rather than a wrong method name. The error has to
// say that much, or the next reader spends an afternoon guessing method names
// that the descriptor says do not exist.
func TestClient_SetFirewall_ReportsUnknownMethod(t *testing.T) {
	c, f, server := newSettingsFixture(t, false, map[string]map[string]interface{}{
		"default": profileDoc("default",
			map[string]int{"eth0": fwPolicyDrop},
			map[string][]map[string]interface{}{"eth0": {allowAllRule("baseline allow")}}),
	})
	defer server.Close()

	f.mu.Lock()
	f.switchMethod = "save" // DSM knows something else, not `set`
	f.mu.Unlock()

	_, err := c.SetFirewall(context.Background(), SetFirewallRequest{Profile: "default", Enabled: true})
	if err == nil {
		t.Fatal("expected an error when DSM knows neither verb")
	}
	if !strings.Contains(err.Error(), "webapi descriptor") {
		t.Errorf("error does not say a 103 here is a real finding: %v", err)
	}
}

// Whether DSM wants profile_name plain or JSON-quoted is the one part of this
// call that is a guess rather than a reconstruction: DSM is inconsistent about
// it across APIs, and the only published implementation of this write quotes it.
// A rejection of the parameters must therefore be retried in the other encoding
// before the call is given up on.
func TestClient_SetFirewall_RetriesQuotedProfileName(t *testing.T) {
	c, f, server := newSettingsFixture(t, false, map[string]map[string]interface{}{
		"default": profileDoc("default",
			map[string]int{"eth0": fwPolicyDrop},
			map[string][]map[string]interface{}{"eth0": {allowAllRule("baseline allow")}}),
	})
	defer server.Close()

	f.mu.Lock()
	f.requireQuotedProfileName = true
	f.mu.Unlock()

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{Profile: "default", Enabled: true}); err != nil {
		t.Fatalf("SetFirewall did not retry with a JSON-quoted profile name: %v", err)
	}

	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("expected one accepted write, got %+v", calls)
	}
	if calls[0].ProfileName != `"default"` {
		t.Errorf("profile_name = %q, want a JSON-quoted name", calls[0].ProfileName)
	}
	// The boolean is never quoted: entry.cgi takes it as a bare string
	// everywhere, and quoting it would be a second guess layered on the first.
	if calls[0].EnableFirewall != "true" {
		t.Errorf("enable_firewall = %q, want bare \"true\"", calls[0].EnableFirewall)
	}
}

// TestClient_SetFirewall_RefusesLockoutOnEnable is the guard issue #121 asks for:
// switching the firewall on with a profile that denies this session is refused,
// and nothing is written.
func TestClient_SetFirewall_RefusesLockoutOnEnable(t *testing.T) {
	c, f, server := newSettingsFixture(t, false, map[string]map[string]interface{}{
		"default": profileDoc("default",
			map[string]int{"eth0": fwPolicyDrop},
			map[string][]map[string]interface{}{
				"eth0": {denyAllRule("deny everything")},
			}),
	})
	defer server.Close()

	_, err := c.SetFirewall(context.Background(), SetFirewallRequest{Profile: "default", Enabled: true})
	var lockout *LockoutError
	if !errors.As(err, &lockout) {
		t.Fatalf("expected *LockoutError, got %v", err)
	}
	if len(f.calls()) != 0 {
		t.Errorf("a refused enable still wrote the switch")
	}
	if f.applies.Load() != 0 {
		t.Errorf("a refused enable still applied the profile")
	}

	// The escape hatch must work, or the guard is a wall.
	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile: "default", Enabled: true, AllowLockout: true,
	}); err != nil {
		t.Fatalf("allow_lockout did not override the guard: %v", err)
	}
	if len(f.calls()) != 1 {
		t.Errorf("the override did not write the switch")
	}
}

// One adapter that still admits the session is enough. The provider cannot know
// which interface its own connection arrives on, so refusing whenever *some*
// adapter denies would block almost every real profile.
func TestClient_SetFirewall_OneReachableAdapterIsEnough(t *testing.T) {
	c, _, server := newSettingsFixture(t, false, map[string]map[string]interface{}{
		"default": profileDoc("default",
			map[string]int{"eth0": fwPolicyDrop, "eth1": fwPolicyDrop},
			map[string][]map[string]interface{}{
				"eth0": {denyAllRule("deny everything")},
				"eth1": {allowAllRule("baseline allow")},
			}),
	})
	defer server.Close()

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{Profile: "default", Enabled: true}); err != nil {
		t.Fatalf("an enable with one reachable adapter was refused: %v", err)
	}
}

// Switching the firewall on with an empty profile is refused by the same rule
// that refuses deleting the last rule of an enabled one, and for the same reason.
func TestClient_SetFirewall_RefusesEmptyRuleSet(t *testing.T) {
	c, f, server := newSettingsFixture(t, false, map[string]map[string]interface{}{
		"default": profileDoc("default", map[string]int{"eth0": fwPolicyDrop}, nil),
	})
	defer server.Close()

	_, err := c.SetFirewall(context.Background(), SetFirewallRequest{Profile: "default", Enabled: true})
	var empty *EmptyRuleSetError
	if !errors.As(err, &empty) {
		t.Fatalf("expected *EmptyRuleSetError, got %v", err)
	}
	if len(f.calls()) != 0 {
		t.Errorf("a refused enable still wrote the switch")
	}

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile: "default", Enabled: true, AllowEmptyRuleSet: true, AllowLockout: true,
	}); err != nil {
		t.Fatalf("allow_empty_rule_set did not override the guard: %v", err)
	}
}

// Switching the firewall off cannot deny anything, so neither guard may stand in
// the way — including when the profile in force denies everything, which is
// exactly the state somebody would be trying to escape.
func TestClient_SetFirewall_DisableIsNeverGuarded(t *testing.T) {
	c, f, server := newSettingsFixture(t, true, map[string]map[string]interface{}{
		"default": profileDoc("default", map[string]int{"eth0": fwPolicyDrop}, nil),
	})
	defer server.Close()

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{Profile: "default", Enabled: false}); err != nil {
		t.Fatalf("disabling the firewall was refused: %v", err)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].EnableFirewall != "false" {
		t.Fatalf("switch write = %+v, want one call with enable_firewall=false", calls)
	}
	if f.applies.Load() != 0 {
		t.Errorf("a disabled firewall was applied; there is nothing to make live")
	}
}

// TestClient_SetFirewall_DefaultPolicy covers issue #123's write half: the
// per-adapter fall-through is written into adapterPolicyMap, other adapters are
// left alone, and the rest of the profile document survives.
func TestClient_SetFirewall_DefaultPolicy(t *testing.T) {
	c, f, server := newSettingsFixture(t, false, map[string]map[string]interface{}{
		"default": profileDoc("default",
			map[string]int{FirewallAdapterGlobal: fwPolicyNone, "eth0": fwPolicyAllow, "eth1": fwPolicyAllow},
			map[string][]map[string]interface{}{"eth0": {allowAllRule("baseline allow")}}),
	})
	defer server.Close()

	result, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       false,
		DefaultPolicy: map[string]string{"eth0": FirewallPolicyDeny},
	})
	if err != nil {
		t.Fatalf("SetFirewall: %v", err)
	}

	if f.profileSets.Load() != 1 {
		t.Fatalf("profile writes = %d, want 1", f.profileSets.Load())
	}

	names := result.Profile.DefaultPolicyNames()
	if names["eth0"] != FirewallPolicyDeny {
		t.Errorf("eth0 default policy = %q, want deny", names["eth0"])
	}
	if names["eth1"] != FirewallPolicyAllow {
		t.Errorf("an unmanaged adapter was rewritten: eth1 = %q, want allow", names["eth1"])
	}
	if names[FirewallAdapterGlobal] != FirewallPolicyNone {
		t.Errorf("global default policy = %q, want none", names[FirewallAdapterGlobal])
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.profiles["default"]["unknownProfileKey"] != "keep-me" {
		t.Errorf("a profile-level key the provider does not model was dropped by a policy write")
	}
}

// Tightening the default policy of the profile in force is a lockout in its own
// right: no rule changed, but traffic that used to fall through to allow now
// falls through to deny.
func TestClient_SetFirewall_DenyDefaultPolicyIsGuarded(t *testing.T) {
	c, f, server := newSettingsFixture(t, true, map[string]map[string]interface{}{
		"default": profileDoc("default",
			map[string]int{"eth0": fwPolicyAllow},
			map[string][]map[string]interface{}{
				// A rule that matches somebody else, so the profile is not empty but
				// this session still relies on the fall-through.
				"eth0": {denySubnetRule("block guests", "192.168.99.0", "255.255.255.0")},
			}),
	})
	defer server.Close()

	_, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       true,
		DefaultPolicy: map[string]string{"eth0": FirewallPolicyDeny},
	})
	var lockout *LockoutError
	if !errors.As(err, &lockout) {
		t.Fatalf("expected *LockoutError, got %v", err)
	}
	if f.profileSets.Load() != 0 {
		t.Errorf("a refused policy change still saved the profile")
	}

	if _, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       true,
		DefaultPolicy: map[string]string{"eth0": FirewallPolicyDeny},
		AllowLockout:  true,
	}); err != nil {
		t.Fatalf("allow_lockout did not override the policy guard: %v", err)
	}
}

// Switching the active profile is guarded too: the profile being switched away
// from is the before-state, not the one being written.
func TestClient_SetFirewall_GuardsProfileSwitch(t *testing.T) {
	c, f, server := newSettingsFixture(t, true, map[string]map[string]interface{}{
		"default": profileDoc("default",
			map[string]int{"eth0": fwPolicyDrop},
			map[string][]map[string]interface{}{"eth0": {allowAllRule("baseline allow")}}),
		"strict": profileDoc("strict",
			map[string]int{"eth0": fwPolicyDrop},
			map[string][]map[string]interface{}{"eth0": {denyAllRule("deny everything")}}),
	})
	defer server.Close()

	_, err := c.SetFirewall(context.Background(), SetFirewallRequest{Profile: "strict", Enabled: true})
	var lockout *LockoutError
	if !errors.As(err, &lockout) {
		t.Fatalf("expected *LockoutError when switching to an unreachable profile, got %v", err)
	}
	if len(f.calls()) != 0 {
		t.Errorf("a refused profile switch still wrote the switch")
	}
}

// TestClient_SetFirewall_Idempotent is the reason a plain refresh-and-apply is
// safe to run repeatedly: applying a profile makes DSM reload its chains and
// drop connections, so a call that changes nothing must touch nothing.
func TestClient_SetFirewall_Idempotent(t *testing.T) {
	c, f, server := newSettingsFixture(t, true, map[string]map[string]interface{}{
		"default": profileDoc("default",
			map[string]int{FirewallAdapterGlobal: fwPolicyNone, "eth0": fwPolicyAllow},
			map[string][]map[string]interface{}{"eth0": {allowAllRule("baseline allow")}}),
	})
	defer server.Close()

	req := SetFirewallRequest{
		Profile:       "default",
		Enabled:       true,
		DefaultPolicy: map[string]string{"eth0": FirewallPolicyAllow},
	}
	if _, err := c.SetFirewall(context.Background(), req); err != nil {
		t.Fatalf("SetFirewall: %v", err)
	}

	if got := len(f.calls()); got != 0 {
		t.Errorf("switch writes = %d, want 0: nothing differed from DSM", got)
	}
	if got := f.profileSets.Load(); got != 0 {
		t.Errorf("profile writes = %d, want 0", got)
	}
	if got := f.applies.Load(); got != 0 {
		t.Errorf("profile applies = %d, want 0: an apply drops live connections", got)
	}
}

func TestFirewallPolicyMapToWire(t *testing.T) {
	got, err := firewallPolicyMapToWire(map[string]string{
		"eth0":                FirewallPolicyDeny,
		"eth1":                FirewallPolicyAllow,
		FirewallAdapterGlobal: FirewallPolicyNone,
	})
	if err != nil {
		t.Fatalf("firewallPolicyMapToWire: %v", err)
	}
	if got["eth0"] != fwPolicyDrop || got["eth1"] != fwPolicyAllow || got[FirewallAdapterGlobal] != fwPolicyNone {
		t.Errorf("wire policies = %v", got)
	}

	if _, err := firewallPolicyMapToWire(map[string]string{"eth0": "drop"}); err == nil {
		t.Error("expected an error for a policy name DSM does not have")
	}

	// DSM's own spelling is "drop"; the provider says "deny" so the value reads
	// the same way as a rule's action. Reading must agree with writing.
	if name := FirewallPolicyName(fwPolicyDrop); name != FirewallPolicyDeny {
		t.Errorf("FirewallPolicyName(drop) = %q, want deny", name)
	}
	// An unknown future value must not be read as allow or deny.
	if name := FirewallPolicyName(99); name != FirewallPolicyNone {
		t.Errorf("FirewallPolicyName(99) = %q, want none", name)
	}
}

// denyAllRule is the wire form of "deny anything from anywhere".
func denyAllRule(name string) map[string]interface{} {
	rule := allowAllRule(name)
	rule["policy"] = float64(fwPolicyDrop)
	return rule
}

// denySubnetRule denies one network and leaves everything else to fall through.
func denySubnetRule(name, network, mask string) map[string]interface{} {
	rule := allowAllRule(name)
	rule["policy"] = float64(fwPolicyDrop)
	rule["ipGroup"] = float64(fwIPGroupNetmask)
	rule["ipList"] = []interface{}{network, mask}
	return rule
}

// The profile-level write has the same problem the rule write had (issue #130):
// SYNO.Core.Security.Firewall `set` is reconstructed rather than captured, so a
// success that changed nothing has to be reported rather than recorded.
func TestClient_SetFirewall_SwitchWithoutEffectIsAnError(t *testing.T) {
	c, f, server := newSettingsFixture(t, false, map[string]map[string]interface{}{
		"default": {
			"name":             "default",
			"rules":            map[string]interface{}{"eth0": []interface{}{allowAllRule("allow all")}},
			"adapterPolicyMap": map[string]interface{}{"eth0": float64(fwPolicyDrop)},
		},
	})
	defer server.Close()

	f.mu.Lock()
	f.discardSwitch = true
	f.mu.Unlock()

	_, err := c.SetFirewall(context.Background(), SetFirewallRequest{Profile: "default", Enabled: true})
	if err == nil {
		t.Fatal("expected an error when DSM reports success and leaves the firewall off")
	}

	var notPersisted *FirewallSettingsNotPersistedError
	if !errors.As(err, &notPersisted) {
		t.Fatalf("expected *FirewallSettingsNotPersistedError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "enabled") {
		t.Errorf("message does not name the field that did not take: %s", err.Error())
	}
}

// A default policy DSM accepts and does not store is the same failure in the
// other half of the write.
func TestClient_SetFirewall_DefaultPolicyWithoutEffectIsAnError(t *testing.T) {
	c, f, server := newSettingsFixture(t, false, map[string]map[string]interface{}{
		"default": {
			"name":             "default",
			"rules":            map[string]interface{}{"eth0": []interface{}{allowAllRule("allow all")}},
			"adapterPolicyMap": map[string]interface{}{"eth0": float64(fwPolicyAllow)},
		},
	})
	defer server.Close()

	f.mu.Lock()
	f.discardProfileSet = true
	f.mu.Unlock()

	_, err := c.SetFirewall(context.Background(), SetFirewallRequest{
		Profile:       "default",
		Enabled:       false,
		DefaultPolicy: map[string]string{"eth0": FirewallPolicyDeny},
	})
	if err == nil {
		t.Fatal("expected an error when DSM reports success and keeps the old default policy")
	}

	var notPersisted *FirewallSettingsNotPersistedError
	if !errors.As(err, &notPersisted) {
		t.Fatalf("expected *FirewallSettingsNotPersistedError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "eth0") {
		t.Errorf("message does not name the adapter whose policy did not take: %s", err.Error())
	}
}
