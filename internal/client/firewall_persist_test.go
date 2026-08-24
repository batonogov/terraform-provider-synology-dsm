package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// shrinkFirewallVerify makes the read-after-write retry cheap enough for a unit
// test, and puts it back afterwards.
func shrinkFirewallVerify(t *testing.T) {
	t.Helper()
	attempts, interval := firewallVerifyAttempts, firewallVerifyInterval
	firewallVerifyAttempts, firewallVerifyInterval = 3, time.Millisecond
	t.Cleanup(func() {
		firewallVerifyAttempts, firewallVerifyInterval = attempts, interval
	})
}

func managedDenyRule(name string, priority int) FirewallRule {
	return FirewallRule{
		Name:     name,
		Enabled:  true,
		Action:   FirewallActionDeny,
		Protocol: FirewallProtocolAll,
		Priority: priority,
	}
}

// Issue #130: DSM answers `{"success": true}` to the profile write and stores
// nothing. Before the read-after-write check the provider reported that as a
// created resource, so Terraform state held five rules the NAS did not have and
// every later plan said "no changes" about an unconfigured firewall.
func TestClient_SetFirewallRule_SuccessWithoutPersistenceIsAnError(t *testing.T) {
	shrinkFirewallVerify(t)

	c, f, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {},
	})
	defer server.Close()

	f.discardWrites()

	_, err := setRule(t, c, FirewallAdapterGlobal, managedDenyRule("Deny everything else", 0), false)
	if err == nil {
		t.Fatal("expected an error when DSM reports success and stores nothing")
	}

	var notPersisted *FirewallNotPersistedError
	if !errors.As(err, &notPersisted) {
		t.Fatalf("expected *FirewallNotPersistedError, got %T: %v", err, err)
	}
	if notPersisted.Operation != "write" {
		t.Errorf("Operation = %q, want %q", notPersisted.Operation, "write")
	}
	if notPersisted.Rule != "Deny everything else" {
		t.Errorf("Rule = %q", notPersisted.Rule)
	}
	if len(notPersisted.Found) != 0 {
		t.Errorf("Found = %v, want empty", notPersisted.Found)
	}
	if msg := err.Error(); !strings.Contains(msg, "reported success") {
		t.Errorf("message does not say DSM reported success: %s", msg)
	}
	if f.sets.Load() == 0 {
		t.Error("the write was never attempted, so the test proves nothing")
	}
}

// A write the provider had to disown must not leave its priority behind. The
// record exists to reproduce a layout DSM accepted (issue #122); remembering a
// position from a write that vanished would let a rejected change reach the NAS
// later, through some other rule's write.
func TestClient_SetFirewallRule_NotPersistedLeavesNoPriorityRecord(t *testing.T) {
	shrinkFirewallVerify(t)

	c, f, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {},
	})
	defer server.Close()

	f.discardWrites()

	if _, err := setRule(t, c, FirewallAdapterGlobal, managedDenyRule("ghost", 3), false); err == nil {
		t.Fatal("expected an error")
	}

	c.mu.Lock()
	remembered := c.firewallPriorities("default", FirewallAdapterGlobal)
	c.mu.Unlock()

	if len(remembered) != 0 {
		t.Fatalf("priority record kept %v after a write that did not land", remembered)
	}
}

// DSM storing a rule under the right name but with the wrong policy is the same
// class of failure as storing nothing: state would claim a deny rule and the NAS
// would hold an allow rule.
func TestClient_SetFirewallRule_FieldMismatchIsAnError(t *testing.T) {
	shrinkFirewallVerify(t)

	c, f, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {},
	})
	defer server.Close()

	f.mu.Lock()
	f.onSet = func(incoming map[string]interface{}) map[string]interface{} {
		rules, _ := incoming["rules"].(map[string]interface{})
		list, _ := rules[FirewallAdapterGlobal].([]interface{})
		for _, item := range list {
			if rule, ok := item.(map[string]interface{}); ok {
				rule["policy"] = float64(fwPolicyAllow)
			}
		}
		return incoming
	}
	f.mu.Unlock()

	_, err := setRule(t, c, FirewallAdapterGlobal, managedDenyRule("Deny everything else", 0), false)
	if err == nil {
		t.Fatal("expected an error when DSM stores a different rule than the one written")
	}

	var notPersisted *FirewallNotPersistedError
	if !errors.As(err, &notPersisted) {
		t.Fatalf("expected *FirewallNotPersistedError, got %T: %v", err, err)
	}
	if len(notPersisted.Mismatches) == 0 {
		t.Fatal("expected the mismatching fields to be named")
	}
	if !strings.Contains(err.Error(), "action") {
		t.Errorf("message does not name the action mismatch: %s", err.Error())
	}
}

// The same rule written back unchanged must not be reported as a mismatch. The
// selectors go through DSM's own encoding on the way out and come back in a
// different spelling -- a CIDR is stored as an address plus a dotted netmask --
// so a naive string comparison would fail every write with a source.
func TestClient_SetFirewallRule_RoundTrippedSelectorsAreNotAMismatch(t *testing.T) {
	shrinkFirewallVerify(t)

	c, _, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {},
	})
	defer server.Close()

	rule := FirewallRule{
		Name:     "vpn",
		Enabled:  true,
		Action:   FirewallActionAllow,
		Protocol: FirewallProtocolTCP,
		Ports:    []string{"5001", "8000-8100"},
		Sources:  []string{"10.210.0.0/16"},
	}
	if _, err := setRule(t, c, FirewallAdapterGlobal, rule, false); err != nil {
		t.Fatalf("SetFirewallRule: %v", err)
	}
}

// A NAS that has just reloaded its packet filter may serve the previous profile
// for a moment. That is not a failed write, so the check retries before it
// gives up.
func TestClient_SetFirewallRule_VerificationRetriesAStaleRead(t *testing.T) {
	shrinkFirewallVerify(t)

	c, f, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {},
	})
	defer server.Close()

	stale := 1
	f.mu.Lock()
	f.onGet = func(stored map[string]interface{}) map[string]interface{} {
		if stale > 0 {
			stale--
			return map[string]interface{}{
				"name":             "default",
				"rules":            map[string]interface{}{FirewallAdapterGlobal: []interface{}{}},
				"adapterPolicyMap": map[string]interface{}{FirewallAdapterGlobal: float64(fwPolicyNone)},
			}
		}
		return stored
	}
	f.mu.Unlock()

	if _, err := setRule(t, c, FirewallAdapterGlobal, managedDenyRule("late", 0), false); err != nil {
		t.Fatalf("expected the stale read to be retried, got: %v", err)
	}
}

// A delete DSM accepts and does not perform is the more dangerous direction:
// Terraform drops the resource from state while the rule keeps filtering.
func TestClient_DeleteFirewallRule_SuccessWithoutRemovalIsAnError(t *testing.T) {
	shrinkFirewallVerify(t)

	c, f, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {allowAllRule("keep me")},
	})
	defer server.Close()

	f.discardWrites()

	_, err := c.DeleteFirewallRule(context.Background(), "default", FirewallAdapterGlobal, "keep me",
		DeleteFirewallRuleOptions{})
	if err == nil {
		t.Fatal("expected an error when DSM reports success and keeps the rule")
	}

	var notPersisted *FirewallNotPersistedError
	if !errors.As(err, &notPersisted) {
		t.Fatalf("expected *FirewallNotPersistedError, got %T: %v", err, err)
	}
	if notPersisted.Operation != "delete" {
		t.Errorf("Operation = %q, want %q", notPersisted.Operation, "delete")
	}
	if !strings.Contains(err.Error(), "still in profile") {
		t.Errorf("message does not say the rule survived: %s", err.Error())
	}
}

// Deleting a rule that was never there stays a no-op: there is nothing to write
// and therefore nothing to verify.
func TestClient_DeleteFirewallRule_AbsentRuleIsStillANoOp(t *testing.T) {
	shrinkFirewallVerify(t)

	c, f, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {allowAllRule("other")},
	})
	defer server.Close()

	if _, err := c.DeleteFirewallRule(context.Background(), "default", FirewallAdapterGlobal, "never existed",
		DeleteFirewallRuleOptions{}); err != nil {
		t.Fatalf("DeleteFirewallRule: %v", err)
	}
	if f.sets.Load() != 0 {
		t.Errorf("a no-op delete wrote the profile %d time(s)", f.sets.Load())
	}
}

// newProfileResponseServer answers Firewall.Profile `get` with a fixed body and
// nothing else, so a response shape can be tested on its own.
func newProfileResponseServer(t *testing.T, body interface{}) (*Client, *httptest.Server) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("api") == "SYNO.Core.Security.Firewall.Profile" && r.FormValue("method") == "get" {
			writeAPIData(w, body)
			return
		}
		_ = json.NewEncoder(w).Encode(APIResponse{Success: false, Error: &APIError{Code: 101}})
	}))
	t.Cleanup(server.Close)

	c := NewClient(server.URL, "admin", "password", false)
	c.setSession("test-sid", "test-token")
	return c, server
}

// A successful response that is not a profile must not be read as an empty
// profile. Every rule write is read-modify-write, so an empty profile taken at
// face value would be written straight back and would replace every rule the
// operator has.
func TestClient_GetFirewallProfile_RejectsUnrecognisedResponse(t *testing.T) {
	c, _ := newProfileResponseServer(t, map[string]interface{}{
		"total":  1,
		"offset": 0,
	})

	_, err := c.GetFirewallProfile(context.Background(), "default")
	if err == nil {
		t.Fatal("expected an error for a response that is not a profile")
	}

	var shape *FirewallProfileShapeError
	if !errors.As(err, &shape) {
		t.Fatalf("expected *FirewallProfileShapeError, got %T: %v", err, err)
	}
	for _, key := range []string{"offset", "total"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("message does not report the key %q DSM actually returned: %s", key, err.Error())
		}
	}
}

// The nested envelope is still accepted -- which of the two shapes DSM uses is
// unconfirmed, so both must keep working.
func TestClient_GetFirewallProfile_AcceptsNestedEnvelope(t *testing.T) {
	c, _ := newProfileResponseServer(t, map[string]interface{}{
		"profile": map[string]interface{}{
			"name":             "default",
			"rules":            map[string]interface{}{FirewallAdapterGlobal: []interface{}{allowAllRule("only")}},
			"adapterPolicyMap": map[string]interface{}{"eth0": float64(fwPolicyDrop)},
		},
	})

	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetFirewallProfile: %v", err)
	}
	if got := len(profile.Rules[FirewallAdapterGlobal]); got != 1 {
		t.Fatalf("nested envelope yielded %d rules, want 1", got)
	}
	if profile.AdapterPolicy["eth0"] != fwPolicyDrop {
		t.Errorf("adapter policy lost through the nested envelope")
	}
}

// A profile that genuinely holds no rules is not a shape error: `rules` is
// there, it is simply empty.
func TestClient_GetFirewallProfile_AcceptsGenuinelyEmptyProfile(t *testing.T) {
	c, _ := newProfileResponseServer(t, map[string]interface{}{
		"name":  "default",
		"rules": map[string]interface{}{},
	})

	profile, err := c.GetFirewallProfile(context.Background(), "default")
	if err != nil {
		t.Fatalf("an empty profile must not be an error: %v", err)
	}
	if profile.ruleCount() != 0 {
		t.Errorf("ruleCount = %d, want 0", profile.ruleCount())
	}
}

// The adapter-keyed shape is the leading explanation for issue #130: two
// published DSM clients send and read {"name": ..., "<adapter>": {"policy": ...,
// "rules": [...]}} over HTTP, rather than the {"rules": {...},
// "adapterPolicyMap": {...}} shape Synology's own fwDB.hpp uses on disk. This
// client cannot write that shape without a capture to go on, but it must say so
// by name instead of reading the profile as empty.
func TestClient_GetFirewallProfile_NamesTheAdapterKeyedShape(t *testing.T) {
	c, _ := newProfileResponseServer(t, map[string]interface{}{
		"name": "default",
		"global": map[string]interface{}{
			"policy": "deny",
			"rules": []interface{}{
				map[string]interface{}{"name": "allow lan", "policy": "allow"},
			},
		},
	})

	_, err := c.GetFirewallProfile(context.Background(), "default")
	if err == nil {
		t.Fatal("expected an error for the adapter-keyed profile shape")
	}

	var shape *FirewallProfileShapeError
	if !errors.As(err, &shape) {
		t.Fatalf("expected *FirewallProfileShapeError, got %T: %v", err, err)
	}
	if len(shape.AdapterKeyed) != 1 || shape.AdapterKeyed[0] != FirewallAdapterGlobal {
		t.Fatalf("AdapterKeyed = %v, want [%s]", shape.AdapterKeyed, FirewallAdapterGlobal)
	}
	if !strings.Contains(err.Error(), "#130") {
		t.Errorf("message does not point at the issue: %s", err.Error())
	}
}

// "DSM returned no rules" and "DSM never mentioned rules" parse the same and
// mean different things. The second is the shape that would follow if rules
// lived behind SYNO.Core.Security.Firewall.Rules rather than inside the profile
// object, which is the other candidate explanation for issue #130 -- so the
// diagnostic has to be able to tell the reader which one it saw.
func TestClient_SetFirewallRule_ReportsAMissingRulesKey(t *testing.T) {
	shrinkFirewallVerify(t)

	c, f, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {},
	})
	defer server.Close()

	f.mu.Lock()
	f.onSet = func(map[string]interface{}) map[string]interface{} { return nil }
	f.onGet = func(map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"name":             "default",
			"adapterPolicyMap": map[string]interface{}{"eth0": float64(fwPolicyDrop)},
		}
	}
	f.mu.Unlock()

	_, err := setRule(t, c, FirewallAdapterGlobal, managedDenyRule("nowhere", 0), false)
	if err == nil {
		t.Fatal("expected an error")
	}

	var notPersisted *FirewallNotPersistedError
	if !errors.As(err, &notPersisted) {
		t.Fatalf("expected *FirewallNotPersistedError, got %T: %v", err, err)
	}
	if !notPersisted.NoRulesKey {
		t.Fatal("NoRulesKey was not set for a profile response with no rules key")
	}
	if !strings.Contains(err.Error(), "Firewall.Rules") {
		t.Errorf("message does not point at the separate Rules API: %s", err.Error())
	}
}

// The same profile with an empty rules map must not raise that flag: there is
// nothing surprising about a profile with no rules in it.
func TestClient_SetFirewallRule_EmptyRulesKeyIsNotAMissingOne(t *testing.T) {
	shrinkFirewallVerify(t)

	c, f, server := newFirewallFixture(t, false, map[string][]map[string]interface{}{
		FirewallAdapterGlobal: {},
	})
	defer server.Close()

	f.discardWrites()

	_, err := setRule(t, c, FirewallAdapterGlobal, managedDenyRule("nowhere", 0), false)
	if err == nil {
		t.Fatal("expected an error")
	}

	var notPersisted *FirewallNotPersistedError
	if !errors.As(err, &notPersisted) {
		t.Fatalf("expected *FirewallNotPersistedError, got %T: %v", err, err)
	}
	if notPersisted.NoRulesKey {
		t.Fatal("NoRulesKey was set for a profile that does carry a rules key")
	}
}
