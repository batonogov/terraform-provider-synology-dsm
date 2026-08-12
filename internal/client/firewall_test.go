package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// firewallFixture is a stateful mock DSM. It keeps one profile in memory and
// answers the four APIs a rule write touches, so the tests exercise the real
// read-modify-write path rather than a stubbed one.
type firewallFixture struct {
	mu            sync.Mutex
	enabled       bool
	activeProfile string
	profile       map[string]interface{}

	applies atomic.Int64
	sets    atomic.Int64
}

func newFirewallFixture(t *testing.T, enabled bool, rules map[string][]map[string]interface{}) (*Client, *firewallFixture, *httptest.Server) {
	t.Helper()

	wireRules := map[string]interface{}{}
	for adapter, list := range rules {
		items := make([]interface{}, len(list))
		for i, rule := range list {
			items[i] = rule
		}
		wireRules[adapter] = items
	}

	f := &firewallFixture{
		enabled:       enabled,
		activeProfile: "default",
		profile: map[string]interface{}{
			"name":  "default",
			"rules": wireRules,
			"adapterPolicyMap": map[string]interface{}{
				FirewallAdapterGlobal: float64(fwPolicyNone),
				"eth0":                float64(fwPolicyDrop),
			},
			// A key the provider knows nothing about, to prove writes preserve it.
			"unknownProfileKey": "keep-me",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webapi/entry.cgi", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		api := r.FormValue("api")
		method := r.FormValue("method")

		switch {
		case api == "SYNO.API.Auth" && method == "login":
			writeAPIData(w, map[string]interface{}{"sid": "test-sid", "synotoken": "test-token"})

		case api == "SYNO.Core.Security.Firewall" && method == "get":
			f.mu.Lock()
			data := map[string]interface{}{"enable_firewall": f.enabled, "profile_name": f.activeProfile}
			f.mu.Unlock()
			writeAPIData(w, data)

		case api == "SYNO.Core.Security.Firewall.Profile" && method == "get":
			f.mu.Lock()
			raw, _ := json.Marshal(f.profile)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})

		case api == "SYNO.Core.Security.Firewall.Profile" && method == "set":
			var incoming map[string]interface{}
			if err := json.Unmarshal([]byte(r.FormValue("profile")), &incoming); err != nil {
				json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
				return
			}
			f.sets.Add(1)
			f.mu.Lock()
			f.profile = incoming
			f.mu.Unlock()
			json.NewEncoder(w).Encode(APIResponse{Success: true})

		case api == "SYNO.Core.Security.Firewall.Profile.Apply" && method == "start":
			f.applies.Add(1)
			writeAPIData(w, map[string]interface{}{"task_id": "task-1"})

		case api == "SYNO.Core.Security.Firewall.Profile.Apply" && method == "status":
			writeAPIData(w, map[string]interface{}{"finish": true})

		case api == "SYNO.Core.Security.Firewall.Profile.Apply" && method == "stop":
			json.NewEncoder(w).Encode(APIResponse{Success: true})

		default:
			json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
		}
	})

	server := httptest.NewServer(mux)
	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")

	// Force one request so the transport dials and the client learns the source
	// address its own session uses; the lockout guard is built on that.
	if _, err := c.GetFirewallSettings(context.Background()); err != nil {
		t.Fatalf("priming request failed: %v", err)
	}

	return c, f, server
}

func writeAPIData(w http.ResponseWriter, data interface{}) {
	raw, _ := json.Marshal(data)
	json.NewEncoder(w).Encode(APIResponse{Success: true, Data: raw})
}

// allowAllRule is the wire form of "allow anything from anywhere", the rule that
// keeps a fixture reachable so lockout tests measure the change under test.
func allowAllRule(name string) map[string]interface{} {
	return map[string]interface{}{
		"name":          name,
		"enable":        true,
		"policy":        float64(fwPolicyAllow),
		"protocol":      float64(fwProtoAll),
		"ipGroup":       float64(fwIPGroupAll),
		"ipList":        []interface{}{},
		"portGroup":     float64(fwPortGroupAll),
		"portList":      []interface{}{},
		"ruleIndex":     float64(1),
		"table":         "filter",
		"adapterDirect": float64(fwDirectSrc),
		"ipDirect":      float64(fwDirectSrc),
		"portDirect":    float64(fwDirectDest),
		"labelList":     []interface{}{"keep-me"},
	}
}

func ruleNames(t *testing.T, c *Client, profile, adapter string) []string {
	t.Helper()
	p, err := c.GetFirewallProfile(context.Background(), profile)
	if err != nil {
		t.Fatalf("GetFirewallProfile: %v", err)
	}
	names := make([]string, 0, len(p.Rules[adapter]))
	for _, rule := range p.Rules[adapter] {
		names = append(names, rule.Name)
	}
	return names
}

func setRule(t *testing.T, c *Client, adapter string, rule FirewallRule, allowLockout bool) (*FirewallWriteResult, error) {
	t.Helper()
	return c.SetFirewallRule(context.Background(), SetFirewallRuleRequest{
		Profile:      "default",
		Adapter:      adapter,
		Rule:         rule,
		AllowLockout: allowLockout,
	})
}

func TestClient_GetFirewallProfile(t *testing.T) {
	c, _, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		"eth0": {
			{
				"name":      "vpn only",
				"enable":    true,
				"policy":    float64(fwPolicyAllow),
				"protocol":  float64(fwProtoTCP),
				"ipGroup":   float64(fwIPGroupNetmask),
				"ipList":    []interface{}{"10.210.0.0", "255.255.0.0"},
				"portGroup": float64(fwPortGroupCustom),
				"portList":  []interface{}{"8443", "8000-8100"},
				"ruleIndex": float64(4),
			},
		},
	})
	defer server.Close()

	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetFirewallProfile: %v", err)
	}

	if profile.Name != "default" {
		t.Errorf("name = %q, want default", profile.Name)
	}
	if got := profile.AdapterPolicy["eth0"]; got != fwPolicyDrop {
		t.Errorf("eth0 policy = %d, want %d", got, fwPolicyDrop)
	}

	rules := profile.Rules["eth0"]
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	rule := rules[0]
	if rule.Action != FirewallActionAllow {
		t.Errorf("action = %q, want allow", rule.Action)
	}
	if rule.Protocol != FirewallProtocolTCP {
		t.Errorf("protocol = %q, want tcp", rule.Protocol)
	}
	if len(rule.Sources) != 1 || rule.Sources[0] != "10.210.0.0/16" {
		t.Errorf("sources = %v, want [10.210.0.0/16]", rule.Sources)
	}
	if len(rule.Ports) != 2 || rule.Ports[0] != "8443" || rule.Ports[1] != "8000-8100" {
		t.Errorf("ports = %v, want [8443 8000-8100]", rule.Ports)
	}
	if rule.Priority != 0 {
		t.Errorf("priority = %d, want 0", rule.Priority)
	}
}

// TestClient_SetFirewallRule_Order is the ordering contract: a rule lands at the
// index its priority names, and inserting into the middle pushes the rest down
// rather than overwriting them. Order is the policy for a firewall, so this is a
// correctness test, not a cosmetic one.
func TestClient_SetFirewallRule_Order(t *testing.T) {
	c, _, server := newFirewallFixture(t, false, nil)
	defer server.Close()

	for i, name := range []string{"first", "second", "third"} {
		if _, err := setRule(t, c, "eth0", FirewallRule{
			Name: name, Enabled: true, Action: FirewallActionAllow,
			Protocol: FirewallProtocolTCP, Priority: i,
		}, false); err != nil {
			t.Fatalf("set %q: %v", name, err)
		}
	}

	if got := ruleNames(t, c, "default", "eth0"); !equalStrings(got, []string{"first", "second", "third"}) {
		t.Fatalf("order after appends = %v, want [first second third]", got)
	}

	// Insert at the front: everything else must shift, nothing may be lost.
	if _, err := setRule(t, c, "eth0", FirewallRule{
		Name: "zeroth", Enabled: true, Action: FirewallActionDeny,
		Protocol: FirewallProtocolTCP, Priority: 0,
	}, false); err != nil {
		t.Fatalf("set zeroth: %v", err)
	}

	if got := ruleNames(t, c, "default", "eth0"); !equalStrings(got, []string{"zeroth", "first", "second", "third"}) {
		t.Fatalf("order after front insert = %v, want [zeroth first second third]", got)
	}

	// Move an existing rule: it is removed from its old slot before being placed,
	// so it must appear exactly once.
	if _, err := setRule(t, c, "eth0", FirewallRule{
		Name: "third", Enabled: true, Action: FirewallActionAllow,
		Protocol: FirewallProtocolTCP, Priority: 1,
	}, false); err != nil {
		t.Fatalf("move third: %v", err)
	}

	if got := ruleNames(t, c, "default", "eth0"); !equalStrings(got, []string{"zeroth", "third", "first", "second"}) {
		t.Fatalf("order after move = %v, want [zeroth third first second]", got)
	}
}

// TestClient_SetFirewallRule_Concurrent is the lost-update test: ten rules
// written in parallel must all survive. Without c.mu each goroutine reads the
// same profile and writes back a copy missing the other nine.
//
// Run with -race to also cover the session fields the writes touch.
func TestClient_SetFirewallRule_Concurrent(t *testing.T) {
	c, fixture, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		"eth0": {allowAllRule("baseline allow")},
	})
	defer server.Close()

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	errs := make([]error, n)

	for i := range n {
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = setRule(t, c, "eth0", FirewallRule{
				Name:     fmt.Sprintf("rule-%d", i),
				Enabled:  true,
				Action:   FirewallActionAllow,
				Protocol: FirewallProtocolTCP,
				Sources:  []string{fmt.Sprintf("10.%d.0.0/16", i)},
				Ports:    []string{"5001"},
				Priority: i,
			}, false)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent set %d failed: %v", i, err)
		}
	}

	got := ruleNames(t, c, "default", "eth0")
	present := map[string]int{}
	for _, name := range got {
		present[name]++
	}
	for i := range n {
		name := fmt.Sprintf("rule-%d", i)
		switch present[name] {
		case 1:
		case 0:
			t.Errorf("lost update: %q missing after concurrent writes (have %d rules: %v)", name, len(got), got)
		default:
			t.Errorf("duplicate: %q appears %d times", name, present[name])
		}
	}
	if present["baseline allow"] != 1 {
		t.Errorf("pre-existing rule was clobbered: %v", got)
	}

	if fixture.applies.Load() < 1 {
		t.Errorf("profile was saved but never applied; a saved profile is not live")
	}

	// One more pass, now that every rule exists, settles the order: each write
	// places its own rule at exactly the index its priority names.
	for i := range n {
		if _, err := setRule(t, c, "eth0", FirewallRule{
			Name:     fmt.Sprintf("rule-%d", i),
			Enabled:  true,
			Action:   FirewallActionAllow,
			Protocol: FirewallProtocolTCP,
			Sources:  []string{fmt.Sprintf("10.%d.0.0/16", i)},
			Ports:    []string{"5001"},
			Priority: i,
		}, false); err != nil {
			t.Fatalf("reconcile set %d: %v", i, err)
		}
	}

	want := make([]string, 0, n+1)
	for i := range n {
		want = append(want, fmt.Sprintf("rule-%d", i))
	}
	want = append(want, "baseline allow")
	if got := ruleNames(t, c, "default", "eth0"); !equalStrings(got, want) {
		t.Errorf("order after reconcile = %v, want %v", got, want)
	}
}

// TestClient_SetFirewallRule_RefusesLockout covers the guard: a deny rule that
// would swallow the provider's own session is refused, and the profile on the
// server is left untouched.
func TestClient_SetFirewallRule_RefusesLockout(t *testing.T) {
	c, fixture, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		"eth0": {allowAllRule("baseline allow")},
	})
	defer server.Close()

	writesBefore := fixture.sets.Load()

	_, err := setRule(t, c, "eth0", FirewallRule{
		Name:     "deny everything",
		Enabled:  true,
		Action:   FirewallActionDeny,
		Protocol: FirewallProtocolAll,
		Priority: 0,
	}, false)

	var lockout *LockoutError
	if !errors.As(err, &lockout) {
		t.Fatalf("expected *LockoutError, got %v", err)
	}
	if lockout.Adapter != "eth0" {
		t.Errorf("lockout adapter = %q, want eth0", lockout.Adapter)
	}
	if fixture.sets.Load() != writesBefore {
		t.Errorf("a refused write still saved the profile")
	}

	// The escape hatch must actually work, or the guard is a wall.
	if _, err := setRule(t, c, "eth0", FirewallRule{
		Name:     "deny everything",
		Enabled:  true,
		Action:   FirewallActionDeny,
		Protocol: FirewallProtocolAll,
		Priority: 0,
	}, true); err != nil {
		t.Fatalf("allow_lockout did not override the guard: %v", err)
	}
	if got := ruleNames(t, c, "default", "eth0"); !equalStrings(got, []string{"deny everything", "baseline allow"}) {
		t.Errorf("rules after override = %v", got)
	}
}

// A rule that denies a subnet the provider is not on must not be mistaken for a
// lockout — a guard that cries wolf gets switched off.
func TestClient_SetFirewallRule_AllowsUnrelatedDeny(t *testing.T) {
	c, _, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		"eth0": {allowAllRule("baseline allow")},
	})
	defer server.Close()

	if _, err := setRule(t, c, "eth0", FirewallRule{
		Name:     "block guests",
		Enabled:  true,
		Action:   FirewallActionDeny,
		Protocol: FirewallProtocolTCP,
		Sources:  []string{"192.168.99.0/24"},
		Priority: 0,
	}, false); err != nil {
		t.Fatalf("unrelated deny was refused: %v", err)
	}
}

// A profile that is not the active one cannot drop a packet, so the guard must
// stay out of the way even for a deny-everything rule.
func TestClient_SetFirewallRule_SkipsGuardOnStandbyProfile(t *testing.T) {
	c, fixture, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		"eth0": {allowAllRule("baseline allow")},
	})
	defer server.Close()

	fixture.mu.Lock()
	fixture.activeProfile = "some other profile"
	fixture.mu.Unlock()

	if _, err := setRule(t, c, "eth0", FirewallRule{
		Name: "deny everything", Enabled: true,
		Action: FirewallActionDeny, Protocol: FirewallProtocolAll, Priority: 0,
	}, false); err != nil {
		t.Fatalf("guard fired on a standby profile: %v", err)
	}
	if fixture.applies.Load() != 0 {
		t.Errorf("a standby profile was applied, which would switch DSM over to it")
	}
}

// A disabled rule enforces nothing, so adding one must never trip the guard.
func TestClient_SetFirewallRule_DisabledDenyIsNotLockout(t *testing.T) {
	c, _, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		"eth0": {allowAllRule("baseline allow")},
	})
	defer server.Close()

	if _, err := setRule(t, c, "eth0", FirewallRule{
		Name: "staged deny", Enabled: false,
		Action: FirewallActionDeny, Protocol: FirewallProtocolAll, Priority: 0,
	}, false); err != nil {
		t.Fatalf("a disabled deny rule was treated as a lockout: %v", err)
	}
}

// TestClient_DeleteFirewallRule_RefusesEmptyRuleSet covers the destroy guard:
// emptying the active profile while the firewall is on locks everyone out.
func TestClient_DeleteFirewallRule_RefusesEmptyRuleSet(t *testing.T) {
	c, fixture, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		"eth0": {allowAllRule("only rule")},
	})
	defer server.Close()

	writesBefore := fixture.sets.Load()

	_, err := c.DeleteFirewallRule(context.Background(), "default", "eth0", "only rule", DeleteFirewallRuleOptions{})
	var empty *EmptyRuleSetError
	if !errors.As(err, &empty) {
		t.Fatalf("expected *EmptyRuleSetError, got %v", err)
	}
	if fixture.sets.Load() != writesBefore {
		t.Errorf("a refused delete still saved the profile")
	}
	if got := ruleNames(t, c, "default", "eth0"); len(got) != 1 {
		t.Errorf("rule was removed despite the refusal: %v", got)
	}

	if _, err := c.DeleteFirewallRule(context.Background(), "default", "eth0", "only rule",
		DeleteFirewallRuleOptions{AllowEmptyRuleSet: true, AllowLockout: true}); err != nil {
		t.Fatalf("allow_empty_rule_set did not override the guard: %v", err)
	}
	if got := ruleNames(t, c, "default", "eth0"); len(got) != 0 {
		t.Errorf("rules after override = %v, want none", got)
	}
}

// With the firewall switched off nothing is enforced, so neither guard applies.
func TestClient_DeleteFirewallRule_EmptyAllowedWhenFirewallOff(t *testing.T) {
	c, _, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		"eth0": {allowAllRule("only rule")},
	})
	defer server.Close()

	if _, err := c.DeleteFirewallRule(context.Background(), "default", "eth0", "only rule", DeleteFirewallRuleOptions{}); err != nil {
		t.Fatalf("delete with the firewall off was refused: %v", err)
	}
}

// Deleting the rule that keeps the session reachable is a lockout even when
// other rules remain.
func TestClient_DeleteFirewallRule_RefusesLockout(t *testing.T) {
	c, _, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		"eth0": {
			allowAllRule("baseline allow"),
			{
				"name": "allow lan", "enable": true,
				"policy": float64(fwPolicyAllow), "protocol": float64(fwProtoTCP),
				"ipGroup": float64(fwIPGroupNetmask), "ipList": []interface{}{"10.0.0.0", "255.0.0.0"},
				"portGroup": float64(fwPortGroupAll), "portList": []interface{}{},
				"ruleIndex": float64(2),
			},
		},
	})
	defer server.Close()

	_, err := c.DeleteFirewallRule(context.Background(), "default", "eth0", "baseline allow", DeleteFirewallRuleOptions{})
	var lockout *LockoutError
	if !errors.As(err, &lockout) {
		t.Fatalf("expected *LockoutError, got %v", err)
	}
}

// TestClient_DeleteFirewallRule_ConcurrentKeepsSurvivors mirrors the set test on
// the delete path, which does the same read-modify-write and needs the same lock.
func TestClient_DeleteFirewallRule_ConcurrentKeepsSurvivors(t *testing.T) {
	seed := map[string][]map[string]interface{}{"eth0": {allowAllRule("baseline allow")}}
	c, _, server := newFirewallFixture(t, false, seed)
	defer server.Close()

	const n = 10
	for i := range n {
		if _, err := setRule(t, c, "eth0", FirewallRule{
			Name: fmt.Sprintf("rule-%d", i), Enabled: true,
			Action: FirewallActionAllow, Protocol: FirewallProtocolTCP, Priority: i,
		}, false); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := range n {
		go func() {
			defer wg.Done()
			<-start
			_, _ = c.DeleteFirewallRule(context.Background(), "default", "eth0",
				fmt.Sprintf("rule-%d", i), DeleteFirewallRuleOptions{})
		}()
	}
	close(start)
	wg.Wait()

	got := ruleNames(t, c, "default", "eth0")
	if !equalStrings(got, []string{"baseline allow"}) {
		t.Errorf("rules after concurrent deletes = %v, want [baseline allow]", got)
	}
}

// A write must not quietly drop the parts of a rule, or of the profile, that
// this provider does not model.
func TestClient_SetFirewallRule_PreservesUnknownFields(t *testing.T) {
	c, fixture, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		"eth0": {allowAllRule("baseline allow")},
	})
	defer server.Close()

	if _, err := setRule(t, c, "eth0", FirewallRule{
		Name: "new rule", Enabled: true,
		Action: FirewallActionAllow, Protocol: FirewallProtocolTCP, Priority: 1,
	}, false); err != nil {
		t.Fatalf("set: %v", err)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()

	if got := fixture.profile["unknownProfileKey"]; got != "keep-me" {
		t.Errorf("profile-level unknown key was dropped: %v", got)
	}

	rules := fixture.profile["rules"].(map[string]interface{})["eth0"].([]interface{})
	existing := rules[0].(map[string]interface{})
	labels, _ := existing["labelList"].([]interface{})
	if len(labels) != 1 || labels[0] != "keep-me" {
		t.Errorf("rule-level unknown key was dropped: %v", existing["labelList"])
	}

	// A brand new rule must carry the structural fields DSM expects, and a
	// ruleIndex that does not collide with the one already in the profile.
	created := rules[1].(map[string]interface{})
	if created["table"] != "filter" {
		t.Errorf("new rule table = %v, want filter", created["table"])
	}
	if created["ruleIndex"] == existing["ruleIndex"] {
		t.Errorf("new rule reused ruleIndex %v", created["ruleIndex"])
	}
	if created["policy"] != float64(fwPolicyAllow) {
		t.Errorf("new rule policy = %v, want %d", created["policy"], fwPolicyAllow)
	}
	if created["protocol"] != float64(fwProtoTCP) {
		t.Errorf("new rule protocol = %v, want %d", created["protocol"], fwProtoTCP)
	}
}

// A rule the provider cannot replay must make the guard say so rather than
// assume the traffic is fine.
func TestClient_SetFirewallRule_WarnsOnUnmodelledNeighbour(t *testing.T) {
	c, _, server := newFirewallFixture(t, true, map[string][]map[string]interface{}{
		"eth0": {
			{
				"name": "block china", "enable": true,
				"policy": float64(fwPolicyDrop), "protocol": float64(fwProtoAll),
				"ipGroup": float64(fwIPGroupGeoIP), "ipList": []interface{}{"CN"},
				"portGroup": float64(fwPortGroupAll), "portList": []interface{}{},
				"ruleIndex": float64(1),
			},
			allowAllRule("baseline allow"),
		},
	})
	defer server.Close()

	result, err := setRule(t, c, "eth0", FirewallRule{
		Name: "allow vpn", Enabled: true, Action: FirewallActionAllow,
		Protocol: FirewallProtocolTCP, Sources: []string{"10.210.0.0/16"}, Priority: 2,
	}, false)
	if err != nil {
		t.Fatalf("an unmodelled neighbour rule blocked the write: %v", err)
	}
	if result.LockoutWarning == nil {
		t.Fatalf("expected an inconclusive-guard warning, got none")
	}
}

func TestClient_SetFirewallRule_RejectsUnstorableSources(t *testing.T) {
	c, _, server := newFirewallFixture(t, false, nil)
	defer server.Close()

	_, err := setRule(t, c, "eth0", FirewallRule{
		Name: "two subnets", Enabled: true, Action: FirewallActionAllow,
		Protocol: FirewallProtocolTCP,
		Sources:  []string{"10.0.0.0/8", "192.168.0.0/16"},
		Priority: 0,
	}, false)
	if err == nil {
		t.Fatal("expected an error: DSM cannot store two networks in one rule")
	}
}

func TestParseFirewallRuleID(t *testing.T) {
	profile, adapter, name, err := ParseFirewallRuleID("default:eth0:allow: vpn")
	if err != nil {
		t.Fatalf("ParseFirewallRuleID: %v", err)
	}
	if profile != "default" || adapter != "eth0" || name != "allow: vpn" {
		t.Errorf("got %q/%q/%q", profile, adapter, name)
	}

	if _, _, _, err := ParseFirewallRuleID("default:eth0"); err == nil {
		t.Error("expected an error for a two-part ID")
	}
}

func TestSourcesToWire(t *testing.T) {
	tests := []struct {
		name      string
		sources   []string
		wantGroup int
		wantList  []string
		wantType  int
	}{
		{"any", nil, fwIPGroupAll, []string{}, fwIPTypeAll},
		{"single ip", []string{"10.0.0.5"}, fwIPGroupIP, []string{"10.0.0.5"}, fwIPTypeV4},
		{"cidr", []string{"10.210.0.0/16"}, fwIPGroupNetmask, []string{"10.210.0.0", "255.255.0.0"}, fwIPTypeV4},
		{"range", []string{"10.0.0.1-10.0.0.9"}, fwIPGroupRange, []string{"10.0.0.1", "10.0.0.9"}, fwIPTypeV4},
		{"set", []string{"10.0.0.1", "10.0.0.2"}, fwIPGroupIPSet, []string{"10.0.0.1", "10.0.0.2"}, fwIPTypeV4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group, list, ipType, err := sourcesToWire(tt.sources)
			if err != nil {
				t.Fatalf("sourcesToWire: %v", err)
			}
			if group != tt.wantGroup {
				t.Errorf("group = %d, want %d", group, tt.wantGroup)
			}
			if !equalStrings(list, tt.wantList) {
				t.Errorf("list = %v, want %v", list, tt.wantList)
			}
			if ipType != tt.wantType {
				t.Errorf("ipType = %d, want %d", ipType, tt.wantType)
			}
		})
	}

	if _, _, _, err := sourcesToWire([]string{"2001:db8::/32"}); err == nil {
		t.Error("expected an error: DSM has no IPv6 netmask form")
	}
}

// TestEvaluateFirewall pins the matching semantics the guard rests on: rules are
// read top to bottom, the first match decides, disabled rules are skipped, and
// anything unmatched falls through to the adapter's default policy.
func TestEvaluateFirewall(t *testing.T) {
	pkt := FirewallPacket{Source: net.ParseIP("10.210.0.7"), DestPort: 5001, Protocol: FirewallProtocolTCP}

	allowVPN := FirewallRule{
		Name: "allow vpn", Enabled: true, Action: FirewallActionAllow,
		Protocol: FirewallProtocolTCP, Ports: []string{"5001"}, Sources: []string{"10.210.0.0/16"},
	}
	denyAll := FirewallRule{
		Name: "deny all", Enabled: true, Action: FirewallActionDeny, Protocol: FirewallProtocolAll,
	}

	if v := EvaluateFirewall([]FirewallRule{allowVPN, denyAll}, pkt, false); !v.Allowed {
		t.Errorf("allow before deny should allow: %s", v.Reason)
	}
	if v := EvaluateFirewall([]FirewallRule{denyAll, allowVPN}, pkt, false); v.Allowed {
		t.Errorf("deny before allow should deny: %s", v.Reason)
	}

	disabled := allowVPN
	disabled.Enabled = false
	if v := EvaluateFirewall([]FirewallRule{disabled, denyAll}, pkt, false); v.Allowed {
		t.Errorf("a disabled allow rule must not match: %s", v.Reason)
	}

	if v := EvaluateFirewall(nil, pkt, false); v.Allowed {
		t.Errorf("empty rule set with a deny default must deny: %s", v.Reason)
	}
	if v := EvaluateFirewall(nil, pkt, true); !v.Allowed {
		t.Errorf("empty rule set with an allow default must allow: %s", v.Reason)
	}

	// A rule for a different port must not decide this packet.
	otherPort := denyAll
	otherPort.Ports = []string{"22"}
	otherPort.Protocol = FirewallProtocolTCP
	if v := EvaluateFirewall([]FirewallRule{otherPort}, pkt, true); !v.Allowed {
		t.Errorf("a rule on another port must not match: %s", v.Reason)
	}

	// A port range must be read as a range, not as a literal.
	ranged := denyAll
	ranged.Protocol = FirewallProtocolTCP
	ranged.Ports = []string{"5000-5010"}
	if v := EvaluateFirewall([]FirewallRule{ranged}, pkt, true); v.Allowed {
		t.Errorf("port 5001 should fall inside 5000-5010: %s", v.Reason)
	}

	unmodelled := FirewallRule{Name: "geoip", Enabled: true, Action: FirewallActionDeny, SourceKind: firewallSourceUnmodelled}
	if v := EvaluateFirewall([]FirewallRule{unmodelled, allowVPN}, pkt, false); !v.Indeterminate {
		t.Errorf("an unmodelled rule must produce an indeterminate verdict, got %+v", v)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
