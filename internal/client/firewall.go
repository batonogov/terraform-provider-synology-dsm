package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Poll cadence for SYNO.Core.Security.Firewall.Profile.Apply. Variables rather
// than constants so tests can shrink them.
var (
	firewallApplyPollInterval = 2 * time.Second
	firewallApplyTimeout      = 2 * time.Minute
)

// Read-after-write verification cadence. A DSM that has just re-rendered its
// iptables chains may serve the profile back a moment later than it accepted it,
// so the check is retried a few times before it is called a failure. Variables
// rather than constants so tests can shrink them.
var (
	firewallVerifyAttempts = 3
	firewallVerifyInterval = 500 * time.Millisecond
)

// Actions, protocols and the global adapter name as this provider spells them.
const (
	FirewallActionAllow = "allow"
	FirewallActionDeny  = "deny"

	FirewallProtocolTCP  = "tcp"
	FirewallProtocolUDP  = "udp"
	FirewallProtocolAll  = "all"
	FirewallProtocolICMP = "icmp"

	// FirewallAdapterGlobal is DSM's pseudo-interface for rules that apply to
	// every network adapter. DSM evaluates this table *before* the table of the
	// interface a connection actually arrived on, which is why it takes part in
	// every reachability calculation below.
	FirewallAdapterGlobal = "global"
)

// Wire enumerations. CONFIRMED against Synology's own synofirewall/fwDB.hpp
// (FW_POLICY, FW_IP_TYPE, FW_IP_GROUP, FW_PORT_GROUP, FW_PORT_PROTOCOL) and
// cross-checked against a rule object written to a live DSM 7.x profile file.
const (
	fwPolicyAllow = 0
	fwPolicyDrop  = 1
	fwPolicyNone  = 2
	// fwPolicyPartial is FW_POLICY_PARTIAL. DSM never answers with it in any
	// captured response; it exists here so the string mapping is total.
	fwPolicyPartial = 3

	fwIPTypeV4  = 0
	fwIPTypeV6  = 1
	fwIPTypeAll = 2

	fwIPGroupIP      = 0 // ipList: ["1.1.1.1"]
	fwIPGroupNetmask = 1 // ipList: ["1.1.1.0", "255.255.255.0"]
	fwIPGroupIPSet   = 2 // ipList: ["1.1.1.1", "2.2.2.2", ...]
	fwIPGroupGeoIP   = 3 // ipList: ["CN", "TW", ...]
	fwIPGroupRange   = 4 // ipList: ["1.1.1.1", "1.1.1.9"] (begin, end)
	fwIPGroupAll     = 5 // ipList: []

	fwPortGroupService  = 0 // portList: ["nfs", "afp", ...]
	fwPortGroupCustom   = 1 // portList: ["123", "1234-1240", ...]
	fwPortGroupReserved = 2 // portList: ["nfs", "afp", ...]
	fwPortGroupAll      = 3 // portList: []

	fwProtoTCP  = 1
	fwProtoUDP  = 2
	fwProtoAll  = 3 // TCP|UDP — note this does *not* include ICMP
	fwProtoICMP = 4

	fwDirectDest = 0
	fwDirectSrc  = 1
)

// firewallSelectorKind records how much of a rule's selector this provider can
// reason about. Rules authored in the DSM UI can select on things the provider
// does not model — a GeoIP country list, one of DSM's built-in service presets —
// and those rules are carried through writes untouched but cannot be replayed by
// the lockout evaluator.
type firewallSelectorKind int

const (
	firewallSelectorModelled firewallSelectorKind = iota
	firewallSourceUnmodelled
	firewallPortUnmodelled
)

// FirewallRule is one entry in a DSM firewall profile's per-adapter rule list.
//
// Priority is the rule's zero-based position in that list. DSM has no ordering
// field: the array position *is* the priority. The `ruleIndex` key DSM stores is
// a bare unique counter (max+1 on insert, never renumbered on delete), and
// Synology's own header calls the equivalent struct member "useless for now" —
// so it must not be mistaken for a sort key.
type FirewallRule struct {
	// Name is the rule's Description in the DSM UI. This provider also uses it as
	// the rule's identity within a profile and adapter.
	Name     string
	Enabled  bool
	Action   string
	Protocol string
	// Ports are destination ports or ranges ("5001", "8000-8100"). Empty means
	// every port — the case that makes a deny rule capable of locking everyone out.
	Ports []string
	// Sources are source addresses, CIDRs, or dashed ranges. Empty means any source.
	Sources  []string
	Priority int

	SourceKind firewallSelectorKind
	PortKind   firewallSelectorKind

	// raw is the rule object exactly as DSM returned it. Writes start from this
	// map and overwrite only the keys the provider manages, so everything it does
	// not model — labelList, chainList, blLog, whatever a future DSM adds —
	// survives a write-back. Rebuilding rules from scratch would silently rewrite
	// rules the operator created by hand.
	raw map[string]interface{}
}

// firewallProfileShape records which of the two wire forms a profile was read
// in, so a write can answer in the same one.
//
// The provider's internal model is the same either way — rule lists and a policy
// per adapter — but the two forms are not interchangeable on the wire, and DSM
// answers `success: true` to the one it does not understand while storing
// nothing. That silent no-op is issue #130, so the shape a profile arrived in is
// carried through to the write rather than assumed.
type firewallProfileShape int

const (
	// firewallShapeAdapterKeyed is what a live DSM answers with:
	// {"name": "default", "global": {"policy": "none", "rules": []},
	//  "eth0": {"policy": "drop", "rules": [...]}}.
	// Adapters are top-level keys and the policy is a string. CONFIRMED against
	// virtual DSM 7.2.2, including a write round trip for the policy
	// (.pi/recon-firewall-vdsm-2026-08-24.md).
	firewallShapeAdapterKeyed firewallProfileShape = iota

	// firewallShapeRulesMap is the shape Synology's own fwDB.hpp uses on disk:
	// {"name": ..., "rules": {<adapter>: [...]}, "adapterPolicyMap": {<adapter>: int}}.
	// CONFIRMED as the on-disk form and as what libsynofirewall serialises; never
	// observed over HTTP. Kept because the webapi shim that translates between the
	// two is not published, so another DSM build may well speak it.
	firewallShapeRulesMap
)

// FirewallProfile is a whole DSM firewall profile: rule lists keyed by network
// adapter, plus the fall-through policy for each adapter.
type FirewallProfile struct {
	Name string
	// Rules is adapter name to that adapter's ordered rule list.
	Rules map[string][]FirewallRule
	// AdapterPolicy is the "if no rule matches" action per adapter: allow (0),
	// drop (1), or none (2). There is no global default — this map is it.
	AdapterPolicy map[string]int

	// shape is the wire form DSM answered in, and the one a write will use.
	shape firewallProfileShape

	// unparsedRules counts, per adapter, the entries of DSM's rule array that
	// were not JSON objects and so did not become rules.
	//
	// It exists so a write cannot quietly drop them. The number is normally zero,
	// and the alternative — comparing the model against the raw array at write
	// time — cannot work, because a delete legitimately makes the model shorter
	// than what DSM sent.
	unparsedRules map[string]int

	// rulesKeyPresent records whether DSM's response mentioned rules at all, as
	// opposed to mentioning an empty list. The two look identical once parsed and
	// mean very different things: an empty profile, or a DSM that does not keep
	// rules inside the profile object. DSM's own webapi descriptor lists a
	// separate SYNO.Core.Security.Firewall.Rules API with `load` / `save_start` /
	// `save_status` / `save_stop`, so the second reading is not far-fetched, and
	// a diagnostic that cannot tell them apart sends the reader looking in the
	// wrong place. In the adapter-keyed shape the key lives inside each adapter
	// block rather than at the top level.
	rulesKeyPresent bool

	raw map[string]interface{}
}

// HasRulesKey reports whether the profile as DSM returned it carried a `rules`
// key. False means DSM never mentioned rules, not that it reported none.
func (p *FirewallProfile) HasRulesKey() bool { return p.rulesKeyPresent }

// FirewallSettings is the global firewall switch plus the profile currently in
// force. CONFIRMED: SYNO.Core.Security.Firewall v1 `get` answers exactly
// {"enable_firewall": bool, "profile_name": string}.
type FirewallSettings struct {
	Enabled       bool
	ActiveProfile string
}

// SetFirewallRuleRequest is a single rule write. The profile is read, this one
// rule is inserted or replaced, and the whole profile is written back — DSM has
// no per-rule API, so read-modify-write is the only shape available.
type SetFirewallRuleRequest struct {
	Profile string
	Adapter string
	Rule    FirewallRule
	// AllowLockout disables the guard that refuses writes which would cut off
	// this client's own management session.
	AllowLockout bool
}

// DeleteFirewallRuleOptions controls the two safety behaviours of a delete.
type DeleteFirewallRuleOptions struct {
	AllowLockout bool
	// AllowEmptyRuleSet permits removing the last rule of an active profile while
	// the firewall is enabled. Left false, that delete is refused: DSM's
	// fall-through policy on a real interface is drop, so a profile with no rules
	// denies every connection including Terraform's.
	AllowEmptyRuleSet bool
}

// FirewallWriteResult is what a rule write did, plus any safety check that came
// back inconclusive. A non-nil LockoutWarning means the write happened but the
// provider could not prove the management session survives it.
type FirewallWriteResult struct {
	Rule           *FirewallRule
	LockoutWarning *IndeterminateLockoutError
	// OrderConflict is set when two rules of the same profile and adapter were
	// configured with the same priority. Both cannot occupy one position, so one
	// of them silently ends up under the other — and for a firewall, under is a
	// different policy.
	OrderConflict *FirewallOrderConflict
}

// FirewallOrderConflict reports that several rules this provider manages were
// configured with the same priority in the same profile and adapter.
//
// Position is the only thing DSM has that expresses precedence, so a tie has to
// be broken somehow. It is broken by rule name, which is at least the same on
// every run — but it is a guess at what the configuration meant, and a rule that
// ends up below a deny rule never matches, so the guess is reported rather than
// made quietly.
type FirewallOrderConflict struct {
	Profile  string
	Adapter  string
	Rule     string
	Priority int
	// Index is where the rule actually ended up.
	Index int
	// Tied names the other rules configured with the same priority, sorted.
	Tied []string
}

func (e *FirewallOrderConflict) Error() string {
	return fmt.Sprintf(
		"firewall rule %q shares priority %d with %s in profile %q adapter %q; DSM stores precedence as position, so the tie "+
			"was broken by rule name and %q ended up at position %d",
		e.Rule, e.Priority, strings.Join(quoteAll(e.Tied), ", "), e.Profile, e.Adapter, e.Rule, e.Index)
}

func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}

// LockoutError reports a refused write: replaying the resulting profile said
// this client's own session, which reaches DSM today, would stop reaching it.
type LockoutError struct {
	Adapter string
	Profile string
	Source  net.IP
	Port    int
	Verdict FirewallVerdict
}

func (e *LockoutError) Error() string {
	return fmt.Sprintf(
		"this change would lock the provider out of DSM: traffic from %s to port %d arriving on adapter %q is allowed today "+
			"but would be denied by profile %q afterwards (%s)",
		e.Source, e.Port, e.Adapter, e.Profile, e.Verdict.Reason)
}

// EmptyRuleSetError reports a refused delete that would have left the active
// profile of an enabled firewall with no rules at all.
type EmptyRuleSetError struct {
	Profile string
}

func (e *EmptyRuleSetError) Error() string {
	return fmt.Sprintf(
		"removing this rule would leave the active firewall profile %q with no rules while the firewall is enabled; "+
			"DSM then falls through to each adapter's default policy, which is drop, and denies every connection including this one",
		e.Profile)
}

// IndeterminateLockoutError reports that the lockout guard could not decide.
//
// This accompanies a successful write rather than replacing one. Refusing every
// change to a profile that merely contains a GeoIP rule would make the resource
// unusable, and the uncertainty comes from a neighbouring rule rather than from
// the change being made — so the write proceeds and the operator is told loudly
// that the guard could not vouch for it.
type IndeterminateLockoutError struct {
	Profile string
	Verdict FirewallVerdict
}

func (e *IndeterminateLockoutError) Error() string {
	return fmt.Sprintf(
		"could not verify that this change keeps the provider's own DSM session reachable in profile %q: %s",
		e.Profile, e.Verdict.Reason)
}

// FirewallNotPersistedError reports that DSM accepted a firewall profile write
// with success = true and then did not store the change.
//
// This is the failure mode of issue #130: five rules created in one apply, five
// successful responses, and a profile that afterwards holds no rules at all.
// DSM's `set` on SYNO.Core.Security.Firewall.Profile answers success for a
// payload it goes on to ignore, so the response cannot be trusted as evidence
// that anything happened -- only a read-back can. Reporting the write as a
// success would put a rule in Terraform state that does not exist on the NAS,
// and every later plan would say "no changes" about a firewall that is not
// configured.
type FirewallNotPersistedError struct {
	Profile string
	Adapter string
	Rule    string
	// Operation is "write" or "delete", so the message can say which direction
	// failed to take.
	Operation string
	// Mismatches names the fields that came back different when the rule is
	// present but wrong. Empty when the rule is missing (or, for a delete, still
	// present) altogether.
	Mismatches []string
	// Found lists the rule names the adapter holds after the write, so the
	// message can distinguish "DSM stored nothing" from "DSM stored something
	// else". Nil when the adapter has no rules.
	Found []string
	// Adapters lists the adapter keys the profile came back with, which is the
	// other thing that can be wrong: a rule written under an adapter name DSM
	// does not keep would vanish exactly like this.
	Adapters []string
	// NoRulesKey records that the profile DSM returned did not mention rules at
	// all. See FirewallProfile.rulesKeyPresent for why that is worth saying out
	// loud rather than rendering as "zero rules".
	NoRulesKey bool
}

func (e *FirewallNotPersistedError) Error() string {
	var b strings.Builder

	switch {
	case e.Operation == "delete":
		fmt.Fprintf(&b, "DSM reported success but firewall rule %q is still in profile %q adapter %q after the delete",
			e.Rule, e.Profile, e.Adapter)
	case len(e.Mismatches) > 0:
		fmt.Fprintf(&b, "DSM reported success but firewall rule %q came back different in profile %q adapter %q (%s)",
			e.Rule, e.Profile, e.Adapter, strings.Join(e.Mismatches, "; "))
	default:
		fmt.Fprintf(&b, "DSM reported success but firewall rule %q is not in profile %q adapter %q after the write",
			e.Rule, e.Profile, e.Adapter)
	}

	if len(e.Found) > 0 {
		fmt.Fprintf(&b, "; that adapter now holds %s", strings.Join(quoteAll(e.Found), ", "))
	} else if e.Operation != "delete" {
		b.WriteString("; that adapter now holds no rules at all")
	}
	if len(e.Adapters) > 0 {
		fmt.Fprintf(&b, "; the profile came back with adapter(s) %s", strings.Join(quoteAll(e.Adapters), ", "))
	} else {
		b.WriteString("; the profile came back with no adapters at all")
	}

	if e.NoRulesKey {
		b.WriteString(". Note that the profile DSM returned carries no \"rules\" key at all, rather than an empty one: " +
			"this DSM may not keep rules inside the profile object, in which case SYNO.Core.Security.Firewall.Profile " +
			"`set` would have no reason to store them. DSM's own webapi descriptor lists a separate " +
			"SYNO.Core.Security.Firewall.Rules API (`load`, `save_start`, `save_status`, `save_stop`) which this client " +
			"deliberately does not use")
	}

	return b.String()
}

// FirewallProfileShapeError reports that SYNO.Core.Security.Firewall.Profile
// `get` answered something that is not a profile.
//
// It is a refusal to guess, and the refusal is the safety property. The rule
// writes are read-modify-write: they take the profile DSM returned, splice one
// rule into it, and send the whole thing back. A response this code fails to
// recognise parses into a profile with no rules and no adapter policies -- and
// writing *that* back would replace every rule the operator has with the single
// rule being added. Erroring on the read is the difference between "the provider
// could not read your firewall" and "the provider deleted your firewall".
//
// Two shapes are recognised, and neither of them is a guess any more: the
// adapter-keyed one a live DSM 7.2.2 answers with, and the
// {rules, adapterPolicyMap} one Synology's own fwDB.hpp stores on disk. A third
// would surface here.
type FirewallProfileShapeError struct {
	Profile string
	// Keys is what DSM actually returned at the top level, sorted.
	Keys []string
}

func (e *FirewallProfileShapeError) Error() string {
	keys := "nothing"
	if len(e.Keys) > 0 {
		keys = strings.Join(quoteAll(e.Keys), ", ")
	}

	return fmt.Sprintf(
		"SYNO.Core.Security.Firewall.Profile `get` answered success for profile %q but the response is not in either shape "+
			"this provider recognises; it carries %s. Expected either the adapter-keyed shape a live DSM answers with "+
			"({\"name\": ..., \"global\": {\"policy\": \"none\", \"rules\": []}}) or the on-disk shape "+
			"({\"name\": ..., \"rules\": {...}, \"adapterPolicyMap\": {...}}). The provider cannot tell an empty profile "+
			"apart from a response it is misreading, and writing back a profile it reconstructed from an unrecognised "+
			"response would replace every rule the profile holds",
		e.Profile, keys)
}

// FirewallRuleWriteUnsupportedError reports a refused write: the profile was
// read in the adapter-keyed shape, and no rule encoding is known for that shape.
//
// This is a deliberate dead end rather than an oversight. Writing the *policy*
// of an adapter-keyed profile is confirmed to work -- that round trip was
// captured on virtual DSM 7.2.2 -- but every candidate encoding of a rule inside
// one crashes synoscgi outright: the on-disk object from fwDB.hpp, the
// string-enum variant two published clients send, snake_case, a rule carrying
// only a name, and even a bare `[{}]` all make DSM answer with its HTML error
// page instead of JSON, because the request never survives the parser. A crashed
// synoscgi takes DSM's whole web interface down with it.
//
// So the provider does the one honest thing available and refuses to send the
// payload. Guessing again on somebody else's NAS is not a debugging strategy,
// and this is a security control: a wrong guess that *did* parse would be worse
// than one that did not.
type FirewallRuleWriteUnsupportedError struct {
	Profile string
	// Adapters names the adapters that would carry at least one rule, sorted.
	Adapters []string
	// Rules names the rules that would have been sent, sorted.
	Rules []string
}

func (e *FirewallRuleWriteUnsupportedError) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b,
		"this DSM answers SYNO.Core.Security.Firewall.Profile `get` in the adapter-keyed shape "+
			"({\"name\": ..., \"<adapter>\": {\"policy\": \"drop\", \"rules\": [...]}}), and no encoding of a firewall rule "+
			"inside that shape is known to work: every published and probed form makes DSM's own request parser crash, so "+
			"synoscgi answers an HTML error page rather than JSON and the DSM web interface goes down with it. The provider "+
			"therefore refuses to send it. Nothing was written to profile %q",
		e.Profile)

	if len(e.Rules) > 0 {
		fmt.Fprintf(&b, "; the write would have carried %s", strings.Join(quoteAll(e.Rules), ", "))
	}
	if len(e.Adapters) > 0 {
		fmt.Fprintf(&b, " under adapter(s) %s", strings.Join(quoteAll(e.Adapters), ", "))
	}

	b.WriteString(". Managing the firewall's default policy (`dsm_firewall`) is unaffected: that write is confirmed to " +
		"round-trip in this shape. What is missing is one capture of a profile that actually holds rules -- either " +
		"`cat /usr/syno/etc/firewall.d/*.json` over SSH, or the raw response of " +
		"SYNO.Core.Security.Firewall.Profile `get` from a NAS where a rule was created in Control Panel -> Security -> " +
		"Firewall. With that, the encoding stops being a guess. See issue #130")

	return b.String()
}

// firewallAdapterBlocks picks out the top-level keys of an adapter-keyed profile:
// the ones whose value is an object carrying a policy, a rule list, or both.
//
// `name` is a string and so never matches. `rules` and `adapterPolicyMap` are
// skipped by name, and that is load-bearing rather than tidiness: a DSM 7
// captured for issue #130 answers with *both* a real adapter block and two
// sibling keys carrying the same {policy, rules} shape --
//
//	{"adapterPolicyMap": {"policy": "none", "rules": []},
//	 "global":           {"policy": "none", "rules": []},
//	 "name":             "default",
//	 "rules":            {"policy": "none", "rules": []}}
//
// Those two are the on-disk shape's own key names leaking into the adapter-keyed
// rendering, not interfaces. Reading them as adapters invents two of them, which
// is where the reported "the profile came back with adapter(s) \"policy\",
// \"rules\"" came from. They stay in `raw` and ride back out of a write
// untouched; nothing here is a name a network interface can have.
func firewallAdapterBlocks(m map[string]interface{}) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	for key, value := range m {
		if key == firewallRulesKey || key == firewallPolicyMapKey {
			continue
		}
		block, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		_, hasPolicy := block["policy"]
		_, hasRules := block["rules"]
		if !hasPolicy && !hasRules {
			continue
		}
		out[key] = block
	}
	return out
}

// GetFirewallSettings reports the global firewall switch and the active profile.
func (c *Client) GetFirewallSettings(ctx context.Context) (*FirewallSettings, error) {
	data, err := c.DoAPI(ctx, "SYNO.Core.Security.Firewall", "1", "get", url.Values{})
	if err != nil {
		return nil, fmt.Errorf("get firewall settings: %w", err)
	}

	var result struct {
		EnableFirewall bool   `json:"enable_firewall"`
		ProfileName    string `json:"profile_name"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse firewall settings: %w", err)
	}
	return &FirewallSettings{Enabled: result.EnableFirewall, ActiveProfile: result.ProfileName}, nil
}

// GetFirewallProfile reads one profile whole.
//
// CONFIRMED against virtual DSM 7.2.2: `name` is the parameter, and the profile
// arrives bare, adapter-keyed --
// {"global": {"policy": "none", "rules": []}, "name": "default"}. The nested
// "profile" envelope and the on-disk {rules, adapterPolicyMap} shape are still
// accepted, because the webapi shim that renders the profile is not published
// and another DSM build may well answer differently. Which shape a response is
// in is decided from the response itself, never from a version number, and the
// same shape is used for the write (see FirewallProfile.toWire).
func (c *Client) GetFirewallProfile(ctx context.Context, name string) (*FirewallProfile, error) {
	params := url.Values{}
	params.Set("name", name)

	data, err := c.DoAPI(ctx, "SYNO.Core.Security.Firewall.Profile", "1", "get", params)
	if err != nil {
		return nil, fmt.Errorf("get firewall profile %q: %w", name, err)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse firewall profile %q: %w", name, err)
	}

	body := data
	if nested, ok := envelope["profile"]; ok {
		body = nested
	}

	profile, err := parseFirewallProfile(body, name)
	if err != nil {
		// A shape error already names the profile and every key DSM sent; wrapping
		// it would only repeat that back.
		if _, ok := err.(*FirewallProfileShapeError); ok {
			return nil, err
		}
		return nil, fmt.Errorf("parse firewall profile %q: %w", name, err)
	}
	if profile.Name == "" {
		profile.Name = name
	}
	return profile, nil
}

// FirewallRulePlacement is one rule together with the length of the list it sits
// in.
//
// The length is not decoration: a priority is a position, so a rule configured
// for position 7 in a list of three has asked for something the profile cannot
// express. Without the count that is indistinguishable from an ordinary
// reordering, and a caller can only report the position it found — which reads
// as drift that another apply will fix, and it will not.
type FirewallRulePlacement struct {
	Rule *FirewallRule
	// RuleCount is the number of rules in this profile and adapter, so the last
	// position available is RuleCount-1.
	RuleCount int
}

// GetFirewallRule returns one rule by profile, adapter and name.
func (c *Client) GetFirewallRule(ctx context.Context, profileName, adapter, name string) (*FirewallRule, error) {
	placement, err := c.GetFirewallRulePlacement(ctx, profileName, adapter, name)
	if err != nil {
		return nil, err
	}
	return placement.Rule, nil
}

// GetFirewallRulePlacement returns one rule along with the size of the list it
// belongs to. Both come out of the same profile read, so it costs no extra call.
func (c *Client) GetFirewallRulePlacement(ctx context.Context, profileName, adapter, name string) (*FirewallRulePlacement, error) {
	profile, err := c.GetFirewallProfile(ctx, profileName)
	if err != nil {
		return nil, err
	}

	rules := profile.Rules[adapter]
	for i := range rules {
		if rules[i].Name == name {
			return &FirewallRulePlacement{Rule: &rules[i], RuleCount: len(rules)}, nil
		}
	}
	// A rule that is simply absent from a profile DSM returned is established
	// absence, not a failed request: the profile read succeeded and the rule was
	// not in it. Read turns this into a state removal (issue #131), which is why
	// it must not be reachable from a transport or session failure — those come
	// out of GetFirewallProfile above, untouched.
	return nil, &NotFoundError{
		Kind:  "firewall rule",
		Name:  name,
		Scope: fmt.Sprintf("in profile %q adapter %q", profileName, adapter),
	}
}

// SetFirewallRule inserts or replaces one rule and writes the whole profile back.
//
// Ordering: the write does not merely place this one rule, it lays out the whole
// adapter list. Every rule this client has written during the life of the process
// is put at the position its own configuration asked for, and the rules the
// provider has never written — created in the DSM UI, or by an earlier apply —
// keep their relative order and fill the slots that are left. Position is the
// only thing DSM has that expresses priority, and Terraform creates independent
// resources concurrently and in arbitrary order: placing just the rule at hand
// would clamp it to the end of whichever list happened to exist at that moment,
// so five rules created in one apply would settle in scheduling order rather than
// in priority order (issue #122). Laying out the whole list makes each write
// idempotent with respect to order, so the final write of an apply always leaves
// the configured order behind whatever order the writes arrived in.
//
// A priority past the end of the list still clamps — there is no position 7 in a
// list of three — but the rules that are present keep their configured order, and
// the achieved index is returned so Read can turn the gap into an ordinary diff.
//
// The whole read-modify-write runs under c.mu, the same lock share permissions
// and quotas take. Terraform applies resources in parallel by default; without
// the lock two rules that read the same profile would each write back a version
// missing the other. The lockout check runs inside the lock too, against the
// exact profile about to be written — checking outside it would be checking a
// state that no longer exists by the time the write lands.
func (c *Client) SetFirewallRule(ctx context.Context, req SetFirewallRuleRequest) (*FirewallWriteResult, error) {
	if err := validateFirewallRule(req.Rule); err != nil {
		return nil, err
	}
	if req.Adapter == "" {
		return nil, fmt.Errorf("firewall rule adapter must not be empty")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	settings, err := c.GetFirewallSettings(ctx)
	if err != nil {
		return nil, err
	}

	before, err := c.GetFirewallProfile(ctx, req.Profile)
	if err != nil {
		return nil, err
	}

	// The layout is computed from the record plus this rule, but the record
	// itself is only updated once the write has landed. A refused or failed write
	// must leave nothing behind: for a rule that already exists in DSM — created
	// by an earlier apply, and so unmanaged in this process — recording the new
	// priority up front would promote it to managed, and the next write of any
	// *other* rule would then lay the list out using a position that was never
	// accepted. Part of a rejected change would arrive later through somebody
	// else's write, which is not what "nothing was written" means.
	desired := c.firewallPriorities(req.Profile, req.Adapter)
	desired[req.Rule.Name] = normalizeFirewallPriority(req.Rule.Priority)

	after := before.clone()
	after.Rules[req.Adapter] = arrangeFirewallRules(
		upsertFirewallRule(after.Rules[req.Adapter], req.Rule), desired)

	warning, err := c.guardFirewallLockout(before, after, settings, req.AllowLockout)
	if err != nil {
		return nil, err
	}

	if err := c.writeFirewallProfile(ctx, after, settings); err != nil {
		return nil, fmt.Errorf("set firewall rule %q in profile %q: %w", req.Rule.Name, req.Profile, err)
	}

	// DSM's success is not evidence (issue #130). Read the profile back and
	// insist the rule is really in it before anything downstream treats the write
	// as done -- including this client's own priority record, which exists to
	// reproduce a layout DSM accepted and must not remember one it discarded.
	if err := c.verifyFirewallRuleWritten(ctx, req.Profile, req.Adapter, req.Rule); err != nil {
		return nil, err
	}

	// The position is now DSM's, so it becomes the provider's to maintain.
	c.rememberFirewallPriority(req.Profile, req.Adapter, req.Rule.Name, req.Rule.Priority)

	for _, rule := range after.Rules[req.Adapter] {
		if rule.Name == req.Rule.Name {
			written := rule
			result := &FirewallWriteResult{Rule: &written, LockoutWarning: warning}
			if tied := firewallPriorityTies(after.Rules[req.Adapter], desired, req.Rule.Name); len(tied) > 0 {
				result.OrderConflict = &FirewallOrderConflict{
					Profile:  req.Profile,
					Adapter:  req.Adapter,
					Rule:     req.Rule.Name,
					Priority: desired[req.Rule.Name],
					Index:    written.Priority,
					Tied:     tied,
				}
			}
			return result, nil
		}
	}
	return nil, fmt.Errorf("firewall rule %q missing from the profile just written", req.Rule.Name)
}

// normalizeFirewallPriority folds a nonsensical priority onto the first position
// rather than rejecting it, so the record and the layout always agree on what a
// given request meant.
func normalizeFirewallPriority(priority int) int {
	if priority < 0 {
		return 0
	}
	return priority
}

// rememberFirewallPriority records the position a rule was configured to take.
// Callers must hold c.mu, and must only call it for a write DSM has accepted.
func (c *Client) rememberFirewallPriority(profile, adapter, name string, priority int) {
	priority = normalizeFirewallPriority(priority)
	if c.firewallRuleOrder == nil {
		c.firewallRuleOrder = map[string]map[string]map[string]int{}
	}
	adapters, ok := c.firewallRuleOrder[profile]
	if !ok {
		adapters = map[string]map[string]int{}
		c.firewallRuleOrder[profile] = adapters
	}
	rules, ok := adapters[adapter]
	if !ok {
		rules = map[string]int{}
		adapters[adapter] = rules
	}
	rules[name] = priority
}

// forgetFirewallPriority drops a rule from the record, so a deleted rule stops
// influencing the layout of the ones that remain. Callers must hold c.mu.
func (c *Client) forgetFirewallPriority(profile, adapter, name string) {
	if adapters, ok := c.firewallRuleOrder[profile]; ok {
		if rules, ok := adapters[adapter]; ok {
			delete(rules, name)
		}
	}
}

// firewallPriorities copies the record for one profile and adapter. Callers must
// hold c.mu; the copy is what keeps the layout functions free of the lock.
func (c *Client) firewallPriorities(profile, adapter string) map[string]int {
	out := map[string]int{}
	for name, priority := range c.firewallRuleOrder[profile][adapter] {
		out[name] = priority
	}
	return out
}

// firewallPriorityTies lists the other rules present in the list that were
// configured with the same priority as name.
//
// Detected from the configured priorities rather than from "the rule did not
// land where it asked", because only one side of a tie is displaced: of two
// rules that both ask for position 0 one of them does get position 0, and if
// that is the rule being written the collision would otherwise go unreported.
func firewallPriorityTies(rules []FirewallRule, desired map[string]int, name string) []string {
	priority, ok := desired[name]
	if !ok {
		return nil
	}

	var tied []string
	for _, rule := range rules {
		if rule.Name == name {
			continue
		}
		if other, ok := desired[rule.Name]; ok && other == priority {
			tied = append(tied, rule.Name)
		}
	}
	sort.Strings(tied)
	return tied
}

// DeleteFirewallRule removes one rule by name, refusing by default both a write
// that would lock this session out and one that would empty an active profile.
func (c *Client) DeleteFirewallRule(ctx context.Context, profileName, adapter, name string, opts DeleteFirewallRuleOptions) (*FirewallWriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	settings, err := c.GetFirewallSettings(ctx)
	if err != nil {
		return nil, err
	}

	before, err := c.GetFirewallProfile(ctx, profileName)
	if err != nil {
		return nil, err
	}

	rules := before.Rules[adapter]
	remaining := make([]FirewallRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Name != name {
			remaining = append(remaining, rule)
		}
	}
	if len(remaining) == len(rules) {
		// Already gone. Nothing to write, and nothing to guard against.
		return &FirewallWriteResult{}, nil
	}
	// The rule stops counting towards the layout the moment it stops existing;
	// leaving it in the record would reserve a position for a rule that is gone.
	c.forgetFirewallPriority(profileName, adapter, name)
	remaining = arrangeFirewallRules(remaining, c.firewallPriorities(profileName, adapter))

	after := before.clone()
	after.Rules[adapter] = remaining

	// Checked before the lockout guard and reported separately, because the two
	// say different things. The guard would also refuse this, but its message
	// would talk about the session rather than about the profile the operator
	// just emptied — and emptying an active profile is dangerous whether or not
	// this particular session happens to notice.
	if settings.Enabled && settings.ActiveProfile == after.Name && after.ruleCount() == 0 && !opts.AllowEmptyRuleSet {
		return nil, &EmptyRuleSetError{Profile: after.Name}
	}

	warning, err := c.guardFirewallLockout(before, after, settings, opts.AllowLockout)
	if err != nil {
		return nil, err
	}

	if err := c.writeFirewallProfile(ctx, after, settings); err != nil {
		return nil, fmt.Errorf("delete firewall rule %q from profile %q: %w", name, profileName, err)
	}

	// Same reason as the write path: a delete DSM accepted and did not perform
	// would drop the resource out of Terraform state and leave the rule in force.
	// For a firewall rule that is the worse direction of the two -- an allow rule
	// nobody knows about any more, or a deny rule that keeps denying.
	if err := c.verifyFirewallRuleRemoved(ctx, profileName, adapter, name); err != nil {
		return nil, err
	}
	return &FirewallWriteResult{LockoutWarning: warning}, nil
}

// verifyFirewallRuleWritten re-reads the profile and insists the rule is in it,
// with the fields it was written with.
func (c *Client) verifyFirewallRuleWritten(ctx context.Context, profileName, adapter string, want FirewallRule) error {
	return c.verifyFirewallProfile(ctx, profileName, func(profile *FirewallProfile) error {
		rules := profile.Rules[adapter]
		for i := range rules {
			if rules[i].Name != want.Name {
				continue
			}
			mismatches := firewallRuleMismatches(want, rules[i])
			if len(mismatches) == 0 {
				return nil
			}
			return &FirewallNotPersistedError{
				Profile:    profileName,
				Adapter:    adapter,
				Rule:       want.Name,
				Operation:  "write",
				Mismatches: mismatches,
				Found:      firewallRuleNames(rules),
				Adapters:   sortedAdapters(profile),
				NoRulesKey: !profile.HasRulesKey(),
			}
		}
		return &FirewallNotPersistedError{
			Profile:    profileName,
			Adapter:    adapter,
			Rule:       want.Name,
			Operation:  "write",
			Found:      firewallRuleNames(rules),
			Adapters:   sortedAdapters(profile),
			NoRulesKey: !profile.HasRulesKey(),
		}
	})
}

// verifyFirewallRuleRemoved re-reads the profile and insists the rule is gone.
func (c *Client) verifyFirewallRuleRemoved(ctx context.Context, profileName, adapter, name string) error {
	return c.verifyFirewallProfile(ctx, profileName, func(profile *FirewallProfile) error {
		rules := profile.Rules[adapter]
		for i := range rules {
			if rules[i].Name == name {
				return &FirewallNotPersistedError{
					Profile:    profileName,
					Adapter:    adapter,
					Rule:       name,
					Operation:  "delete",
					Found:      firewallRuleNames(rules),
					Adapters:   sortedAdapters(profile),
					NoRulesKey: !profile.HasRulesKey(),
				}
			}
		}
		return nil
	})
}

// verifyFirewallProfile re-reads the profile and runs check against it, retrying
// a few times before giving up.
//
// The retry is not decoration. A rule write ends with Profile.Apply, which makes
// DSM re-render and reload its packet filter, and a read issued in the middle of
// that is entitled to answer with what was there a moment ago. Failing on the
// first disagreement would turn a slow NAS into a broken apply. Failing after a
// bounded number of tries is the point of the whole check.
//
// A read that errors is reported as itself rather than folded into "the rule is
// missing": a firewall the provider cannot read is a different problem from a
// firewall that discarded the write, and conflating them would send the reader
// after the wrong one.
func (c *Client) verifyFirewallProfile(ctx context.Context, profileName string, check func(*FirewallProfile) error) error {
	attempts := firewallVerifyAttempts
	if attempts < 1 {
		attempts = 1
	}

	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(firewallVerifyInterval):
			}
		}

		profile, err := c.GetFirewallProfile(ctx, profileName)
		if err != nil {
			return fmt.Errorf("read firewall profile %q back after writing it: %w", profileName, err)
		}
		last = check(profile)
		if last == nil {
			return nil
		}
	}
	return last
}

// firewallRuleMismatches names the managed fields on which a rule read back from
// DSM disagrees with the rule that was written.
//
// The selectors are compared through the same encoder that wrote them rather
// than as the strings the operator typed, so a source that DSM stores as an
// address plus a netmask and returns as a CIDR compares equal, and a normalised
// IPv6 literal does not read as drift. A selector neither side can model is
// skipped: this function exists to catch a write DSM ignored, not to second-guess
// a GeoIP rule the provider never claimed to understand.
func firewallRuleMismatches(want, got FirewallRule) []string {
	var out []string

	if want.Enabled != got.Enabled {
		out = append(out, fmt.Sprintf("enabled: wrote %t, read back %t", want.Enabled, got.Enabled))
	}
	if want.Action != got.Action {
		out = append(out, fmt.Sprintf("action: wrote %q, read back %q", want.Action, got.Action))
	}
	if want.Protocol != got.Protocol {
		out = append(out, fmt.Sprintf("protocol: wrote %q, read back %q", want.Protocol, got.Protocol))
	}

	if a, aok := firewallPortFingerprint(want); aok {
		if b, bok := firewallPortFingerprint(got); bok && a != b {
			out = append(out, fmt.Sprintf("ports: wrote %v, read back %v", want.Ports, got.Ports))
		}
	}
	if a, aok := firewallSourceFingerprint(want); aok {
		if b, bok := firewallSourceFingerprint(got); bok && a != b {
			out = append(out, fmt.Sprintf("source: wrote %v, read back %v", want.Sources, got.Sources))
		}
	}

	return out
}

func firewallPortFingerprint(rule FirewallRule) (string, bool) {
	if rule.PortKind != firewallSelectorModelled {
		return "", false
	}
	group, list, err := portsToWire(rule.Ports)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d|%s", group, strings.Join(list, ",")), true
}

func firewallSourceFingerprint(rule FirewallRule) (string, bool) {
	if rule.SourceKind != firewallSelectorModelled {
		return "", false
	}
	group, list, ipType, err := sourcesToWire(rule.Sources)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d|%d|%s", group, ipType, strings.Join(list, ",")), true
}

func firewallRuleNames(rules []FirewallRule) []string {
	if len(rules) == 0 {
		return nil
	}
	out := make([]string, 0, len(rules))
	for i := range rules {
		out = append(out, rules[i].Name)
	}
	return out
}

// sortedAdapters lists every adapter key the profile came back with, rules and
// policies alike, so a diagnostic can show what DSM answered rather than only
// what was looked for.
func sortedAdapters(profile *FirewallProfile) []string { return profile.adapterKeys() }

func sortedAnyKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// guardFirewallLockout compares reachability of this client's own session before
// and after the change.
//
// Comparing rather than checking the result in isolation is what keeps the guard
// usable. A profile can legitimately deny a great deal already — an interface
// nobody uses, a stricter posture the operator chose — and refusing every write
// that leaves *some* path denied would block routine work. What matters is
// whether this change takes away a path that works today.
//
// It returns an error only when the answer is a definite yes. When the replay
// cannot reach a verdict it returns a warning and no error, and the caller
// proceeds: the uncertainty comes from rules the operator created elsewhere, and
// blocking on it would make such profiles unmanageable.
func (c *Client) guardFirewallLockout(before, after *FirewallProfile, settings *FirewallSettings, allowLockout bool) (*IndeterminateLockoutError, error) {
	// Nothing to guard when the firewall is off, or when the profile being edited
	// is not the one in force. Editing a standby profile cannot drop a packet.
	if allowLockout || !settings.Enabled || settings.ActiveProfile != after.Name {
		return nil, nil
	}

	src := c.LocalIP()
	port := c.ManagementPort()
	if src == nil || port == 0 {
		return &IndeterminateLockoutError{
			Profile: after.Name,
			Verdict: FirewallVerdict{
				Indeterminate: true,
				Reason:        "the provider could not determine the source address or port of its own DSM session",
			},
		}, nil
	}

	pkt := FirewallPacket{Source: src, DestPort: port, Protocol: FirewallProtocolTCP}

	adapters := after.physicalAdapters()
	if len(adapters) == 0 {
		return &IndeterminateLockoutError{
			Profile: after.Name,
			Verdict: FirewallVerdict{
				Indeterminate: true,
				Reason:        "the profile lists no network adapter besides " + FirewallAdapterGlobal + ", so there is no rule chain to replay",
			},
		}, nil
	}

	var indeterminate *IndeterminateLockoutError
	for _, adapter := range adapters {
		wasAllowed := before.evaluateAccess(adapter, pkt)
		willBeAllowed := after.evaluateAccess(adapter, pkt)

		if wasAllowed.Indeterminate || willBeAllowed.Indeterminate {
			if indeterminate == nil {
				verdict := willBeAllowed
				if !verdict.Indeterminate {
					verdict = wasAllowed
				}
				indeterminate = &IndeterminateLockoutError{Profile: after.Name, Verdict: verdict}
			}
			continue
		}

		// Only a change from reachable to unreachable is a lockout. An adapter
		// that already denied this session is not this change's doing.
		if wasAllowed.Allowed && !willBeAllowed.Allowed {
			return nil, &LockoutError{
				Adapter: adapter,
				Profile: after.Name,
				Source:  src,
				Port:    port,
				Verdict: willBeAllowed,
			}
		}
	}

	return indeterminate, nil
}

// evaluateAccess replays a packet against one adapter's effective chain: the
// global table first, then the adapter's own table, then the adapter's
// fall-through policy. CONFIRMED order of precedence, per Synology's firewall
// documentation.
func (p *FirewallProfile) evaluateAccess(adapter string, pkt FirewallPacket) FirewallVerdict {
	global := p.Rules[FirewallAdapterGlobal]
	own := p.Rules[adapter]

	chain := make([]FirewallRule, 0, len(global)+len(own))
	chain = append(chain, global...)
	chain = append(chain, own...)

	policy, ok := p.AdapterPolicy[adapter]
	if !ok {
		// An adapter with no recorded policy is treated as drop. That is DSM's
		// conservative posture, and because the guard compares before with after,
		// guessing conservatively here cannot invent a lockout — it makes both
		// sides deny, which is not a change.
		policy = fwPolicyDrop
	}
	return EvaluateFirewall(chain, pkt, policy == fwPolicyAllow)
}

// physicalAdapters lists the adapters whose chains can actually receive traffic,
// sorted so diagnostics are stable.
func (p *FirewallProfile) physicalAdapters() []string {
	seen := map[string]bool{}
	for adapter := range p.Rules {
		if adapter != FirewallAdapterGlobal {
			seen[adapter] = true
		}
	}
	for adapter := range p.AdapterPolicy {
		if adapter != FirewallAdapterGlobal {
			seen[adapter] = true
		}
	}

	out := make([]string, 0, len(seen))
	for adapter := range seen {
		out = append(out, adapter)
	}
	sort.Strings(out)
	return out
}

func (p *FirewallProfile) ruleCount() int {
	total := 0
	for _, rules := range p.Rules {
		total += len(rules)
	}
	return total
}

func (p *FirewallProfile) clone() *FirewallProfile {
	out := &FirewallProfile{
		Name:            p.Name,
		Rules:           make(map[string][]FirewallRule, len(p.Rules)),
		AdapterPolicy:   make(map[string]int, len(p.AdapterPolicy)),
		shape:           p.shape,
		unparsedRules:   p.unparsedRules,
		rulesKeyPresent: p.rulesKeyPresent,
		raw:             p.raw,
	}
	for adapter, rules := range p.Rules {
		copied := make([]FirewallRule, len(rules))
		copy(copied, rules)
		out.Rules[adapter] = copied
	}
	for adapter, policy := range p.AdapterPolicy {
		out.AdapterPolicy[adapter] = policy
	}
	return out
}

// writeFirewallProfile saves the profile and then applies it.
//
// CONFIRMED from a capture of the DSM control panel: the UI writes with
// SYNO.Core.Security.Firewall.Profile `set` (params `profile` carrying the whole
// profile as compact JSON, and `profile_applying=false`), then commits with
// Profile.Apply `start` / `status` / `stop`. A `set` on its own only rewrites
// /usr/syno/etc/firewall.d/*.json and does not take effect.
//
// Deliberately NOT used: SYNO.Core.Security.Firewall.Rules `save_start`, which
// is reported to crash synoscgi (HTTP 502) on DSM 7.2.2 when handed a rule
// object with concrete fields.
func (c *Client) writeFirewallProfile(ctx context.Context, profile *FirewallProfile, settings *FirewallSettings) error {
	if err := c.saveFirewallProfile(ctx, profile); err != nil {
		return err
	}

	// Applying a profile that is not the active one would switch DSM over to it.
	// Saving is the whole job in that case.
	if settings.ActiveProfile != profile.Name {
		return nil
	}
	return c.applyFirewallProfile(ctx, profile.Name)
}

// saveFirewallProfile is the `set` half of writeFirewallProfile on its own.
//
// Split out because the profile-level writes in firewall_settings.go may change
// the active profile in the same operation: they have to decide for themselves
// which profile to apply, and applying here as well would either commit the
// wrong profile or commit the right one twice.
func (c *Client) saveFirewallProfile(ctx context.Context, profile *FirewallProfile) error {
	// The render can refuse. This is the single choke point every profile write
	// passes through, which is exactly where the refusal belongs: a payload that
	// crashes DSM's request parser must be impossible to send, not merely
	// discouraged at the call sites that happen to remember.
	wire, err := profile.toWire()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("encode firewall profile: %w", err)
	}

	params := url.Values{}
	params.Set("profile", string(encoded))
	params.Set("profile_applying", boolParam(false))

	if _, err := c.DoAPIPost(ctx, "SYNO.Core.Security.Firewall.Profile", "1", "set", params); err != nil {
		return fmt.Errorf("save firewall profile %q: %w", profile.Name, err)
	}
	return nil
}

// applyFirewallProfile runs the two-phase commit that makes a saved profile live.
//
// profile_applying is sent as false: DSM 7 answers error 117 to true, and the
// control panel sends false.
func (c *Client) applyFirewallProfile(ctx context.Context, name string) error {
	params := url.Values{}
	params.Set("name", name)
	params.Set("profile_applying", boolParam(false))

	data, err := c.DoAPIPost(ctx, "SYNO.Core.Security.Firewall.Profile.Apply", "1", "start", params)
	if err != nil {
		return fmt.Errorf("apply firewall profile %q: %w", name, err)
	}

	taskID := parseFirewallTaskID(data)
	if taskID == "" {
		// Some builds finish synchronously and return no task. Nothing to poll.
		return nil
	}

	if err := c.waitFirewallApply(ctx, taskID); err != nil {
		return err
	}

	stopParams := url.Values{}
	stopParams.Set("task_id", taskID)
	if _, err := c.DoAPIPost(ctx, "SYNO.Core.Security.Firewall.Profile.Apply", "1", "stop", stopParams); err != nil {
		// The profile is already live at this point; failing to tear down the task
		// handle is not worth undoing a successful apply over.
		return nil
	}
	return nil
}

func (c *Client) waitFirewallApply(ctx context.Context, taskID string) error {
	deadline := time.Now().Add(firewallApplyTimeout)

	for {
		params := url.Values{}
		params.Set("task_id", taskID)

		data, err := c.DoAPI(ctx, "SYNO.Core.Security.Firewall.Profile.Apply", "1", "status", params)
		if err != nil {
			return fmt.Errorf("poll firewall apply task %q: %w", taskID, err)
		}

		var result struct {
			Finish bool `json:"finish"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("parse firewall apply status: %w", err)
		}
		if result.Finish {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("firewall apply task %q did not finish within %s", taskID, firewallApplyTimeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(firewallApplyPollInterval):
		}
	}
}

func parseFirewallTaskID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var result struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return ""
	}
	return result.TaskID
}

// upsertFirewallRule returns rules with rule replacing any existing entry of the
// same name, or appended when there is none. Where it lands is not decided here:
// arrangeFirewallRules positions the whole list afterwards. Appending is what
// keeps this step from disturbing the relative order of the rules the provider
// does not manage.
func upsertFirewallRule(rules []FirewallRule, rule FirewallRule) []FirewallRule {
	out := make([]FirewallRule, 0, len(rules)+1)
	for _, existing := range rules {
		if existing.Name == rule.Name {
			// Carry over the wire fields of the rule being replaced so selectors
			// this provider does not model are not dropped by an update.
			if rule.raw == nil {
				rule.raw = existing.raw
			}
			continue
		}
		out = append(out, existing)
	}
	return append(out, rule)
}

// arrangeFirewallRules lays out one adapter's rule list from the priorities the
// provider was asked for, and returns it renumbered so every rule's Priority
// agrees with where it actually sits.
//
// The layout is a function of (list contents, desired priorities) alone — never
// of the order the writes arrived in. That is the whole point: Terraform writes
// independent resources concurrently, so anything that depends on arrival order
// makes the resulting policy depend on scheduling.
//
// Two properties hold and both are load-bearing:
//
//   - A rule the provider has written takes the lowest free position at or after
//     its configured priority, so with distinct priorities every managed rule
//     lands exactly on its number, and their relative order always matches the
//     configuration even when the list is too short for the numbers themselves.
//   - Rules the provider has never written — created in the DSM UI, or by an
//     earlier apply — are never reordered among themselves. They fill the
//     positions no managed rule claims, in the order DSM returned them.
//
// Equal priorities are broken by rule name so the result stays stable across
// runs; the caller reports that tie rather than resolving it silently.
func arrangeFirewallRules(rules []FirewallRule, desired map[string]int) []FirewallRule {
	type placed struct {
		rule     FirewallRule
		priority int
	}

	managed := make([]placed, 0, len(rules))
	unmanaged := make([]FirewallRule, 0, len(rules))
	for _, rule := range rules {
		if priority, ok := desired[rule.Name]; ok {
			managed = append(managed, placed{rule: rule, priority: priority})
			continue
		}
		unmanaged = append(unmanaged, rule)
	}

	if len(managed) == 0 {
		out := make([]FirewallRule, len(rules))
		copy(out, rules)
		reindexFirewallRules(out)
		return out
	}

	sort.SliceStable(managed, func(i, j int) bool {
		if managed[i].priority != managed[j].priority {
			return managed[i].priority < managed[j].priority
		}
		return managed[i].rule.Name < managed[j].rule.Name
	})

	out := make([]FirewallRule, 0, len(rules))
	next, other := 0, 0
	for slot := range rules {
		switch {
		case next < len(managed) && managed[next].priority <= slot:
			out = append(out, managed[next].rule)
			next++
		case other < len(unmanaged):
			out = append(out, unmanaged[other])
			other++
		default:
			// Only reached when the remaining managed rules ask for positions past
			// the end of the list. They keep their relative order and take the
			// last slots; the caller sees the achieved index.
			out = append(out, managed[next].rule)
			next++
		}
	}

	reindexFirewallRules(out)
	return out
}

func reindexFirewallRules(rules []FirewallRule) {
	for i := range rules {
		rules[i].Priority = i
	}
}

func validateFirewallRule(rule FirewallRule) error {
	if rule.Name == "" {
		return fmt.Errorf("firewall rule name must not be empty: it is the rule's identity within a profile")
	}
	switch rule.Action {
	case FirewallActionAllow, FirewallActionDeny:
	default:
		return fmt.Errorf("unknown firewall action %q: must be %s or %s", rule.Action, FirewallActionAllow, FirewallActionDeny)
	}
	switch rule.Protocol {
	case FirewallProtocolTCP, FirewallProtocolUDP, FirewallProtocolAll, FirewallProtocolICMP:
	default:
		return fmt.Errorf("unknown firewall protocol %q: must be %s, %s, %s, or %s",
			rule.Protocol, FirewallProtocolTCP, FirewallProtocolUDP, FirewallProtocolAll, FirewallProtocolICMP)
	}
	if _, _, err := portsToWire(rule.Ports); err != nil {
		return err
	}
	if _, _, _, err := sourcesToWire(rule.Sources); err != nil {
		return err
	}
	return nil
}

// toWire renders the profile for a `set` call, in the same shape it was read in.
//
// Round-trip fidelity is the point. DSM answers `success: true` to a profile
// object it does not understand and stores nothing (issue #130), so the write
// cannot pick a shape on its own: it has to answer in the one the read came
// back in. Either way the render starts from whatever DSM returned, so keys this
// provider does not model ride along unchanged.
func (p *FirewallProfile) toWire() (map[string]interface{}, error) {
	if p.shape == firewallShapeAdapterKeyed {
		return p.toWireAdapterKeyed()
	}
	return p.toWireRulesMap(), nil
}

// toWireRulesMap renders {"rules": {...}, "adapterPolicyMap": {...}}, the shape
// Synology's own fwDB.hpp uses on disk.
func (p *FirewallProfile) toWireRulesMap() map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range p.raw {
		out[k] = v
	}
	out["name"] = p.Name

	nextIndex := p.nextRuleIndex()
	rules := map[string]interface{}{}
	for adapter, list := range p.Rules {
		wire := make([]map[string]interface{}, len(list))
		for i := range list {
			wire[i] = list[i].toWire(&nextIndex)
		}
		rules[adapter] = wire
	}
	out["rules"] = rules

	policy := map[string]interface{}{}
	for adapter, value := range p.AdapterPolicy {
		policy[adapter] = value
	}
	out["adapterPolicyMap"] = policy

	return out
}

// toWireAdapterKeyed renders {"name": ..., "<adapter>": {"policy": "drop",
// "rules": []}}, the shape a live DSM answers with — and refuses outright to
// render a rule into it.
//
// The refusal is the safety property, not a limitation being worked around. The
// policy half of this shape is confirmed to round-trip on virtual DSM 7.2.2; the
// rule half has no known encoding, and every candidate crashes DSM's request
// parser rather than being rejected by it, which takes the NAS's web interface
// down. "I do not know how to write this" is the only true thing the provider
// can say, and saying it costs an operator an error message where guessing costs
// them a firewall.
//
// The refusal is drawn around *encoding*, not around the presence of rules. A
// rule DSM sent and this write hands back unchanged goes out as the very object
// DSM produced, which is by construction a form DSM can read — that is what its
// own web interface does every time somebody saves the firewall page. A rule the
// provider had to build from its model is the one nobody knows how to spell, and
// that is what gets refused. Drawing the line around presence instead would take
// `dsm_firewall` away from every NAS that has a rule, which is every NAS that
// uses its firewall, in exchange for no extra safety.
func (p *FirewallProfile) toWireAdapterKeyed() (map[string]interface{}, error) {
	if err := p.refuseConstructedRules(); err != nil {
		return nil, err
	}

	out := map[string]interface{}{}
	for k, v := range p.raw {
		out[k] = v
	}
	out["name"] = p.Name

	for _, adapter := range p.adapterKeys() {
		block := map[string]interface{}{}
		existing, known := p.raw[adapter].(map[string]interface{})
		for k, v := range existing {
			block[k] = v
		}

		if policy, ok := p.AdapterPolicy[adapter]; ok {
			block["policy"] = firewallPolicyToWireString(policy)
		}

		rules, hadRulesKey := p.Rules[adapter]
		if hadRulesKey || !known {
			// Every rule here is one DSM sent, verified unchanged by
			// refuseConstructedRules, so each goes back as the object DSM produced
			// rather than as anything this provider rendered. Order is this
			// provider's to decide (issue #122): the array index is the priority,
			// and reordering objects does not reshape them.
			//
			// An adapter DSM never mentioned gets the key anyway, because the write
			// that was actually captured carries `"rules": []` on every adapter block
			// and there is nothing to be faithful to. An adapter DSM *did* send
			// without a rules key keeps it that way.
			wire := make([]interface{}, 0, len(rules))
			for i := range rules {
				wire = append(wire, rules[i].raw)
			}
			block["rules"] = wire
		}
		out[adapter] = block
	}

	return out, nil
}

// refuseConstructedRules refuses a write that would have to spell a rule out.
//
// Three things make a rule un-echoable, and each is a different way of losing
// somebody's firewall:
//
//   - it carries no raw object, so it was built from the model — a rule being
//     created or replaced. Its encoding is the unknown one, and every candidate
//     tried so far crashes DSM's request parser rather than being rejected by
//     it, taking the web interface down with it;
//   - the model no longer agrees with the object DSM sent, so an edit is being
//     written. Same problem: the changed field has to be rendered;
//   - the parser could not read an entry at all (unparsedRules). Nothing was
//     kept to hand back, and sending the rest would delete that entry quietly.
//
// A rule that fails none of these goes back exactly as it arrived, which needs
// no encoding at all.
func (p *FirewallProfile) refuseConstructedRules() error {
	var adapters, names []string
	seen := map[string]bool{}

	for _, adapter := range p.adapterKeys() {
		refused := false

		if p.unparsedRules[adapter] > 0 {
			refused = true
		}
		for _, rule := range p.Rules[adapter] {
			if firewallRuleIsEcho(rule) {
				continue
			}
			refused = true
			if !seen[rule.Name] {
				seen[rule.Name] = true
				names = append(names, rule.Name)
			}
		}

		if refused {
			adapters = append(adapters, adapter)
		}
	}

	if len(adapters) == 0 {
		return nil
	}
	sort.Strings(adapters)
	sort.Strings(names)
	return &FirewallRuleWriteUnsupportedError{Profile: p.Name, Adapters: adapters, Rules: names}
}

// firewallRuleIsEcho reports whether a rule can be handed back to DSM as the
// object DSM sent, with nothing rendered.
//
// The comparison runs the raw object back through the parser and asks whether
// the model still agrees with it, reusing the same field comparison the
// read-after-write check uses. Comparing the parsed forms rather than the maps
// is what keeps a rule DSM merely re-serialised — a reordered key, a number
// written differently — from being mistaken for an edit.
func firewallRuleIsEcho(rule FirewallRule) bool {
	if rule.raw == nil {
		return false
	}
	original := parseFirewallRule(rule.raw)
	if original == nil || original.Name != rule.Name {
		return false
	}
	return len(firewallRuleMismatches(rule, *original)) == 0
}

// adapterKeys lists every adapter the profile knows about, from either map,
// sorted so a rendered payload is stable across runs.
func (p *FirewallProfile) adapterKeys() []string {
	seen := map[string]bool{}
	for adapter := range p.Rules {
		seen[adapter] = true
	}
	for adapter := range p.AdapterPolicy {
		seen[adapter] = true
	}
	out := make([]string, 0, len(seen))
	for adapter := range seen {
		out = append(out, adapter)
	}
	sort.Strings(out)
	return out
}

// nextRuleIndex returns one past the highest ruleIndex in the profile. DSM
// assigns ruleIndex as max+1 and never renumbers, so new rules must not reuse a
// value even though nothing sorts by it.
func (p *FirewallProfile) nextRuleIndex() int {
	max := -1
	for _, rules := range p.Rules {
		for i := range rules {
			if idx, ok := rules[i].wireRuleIndex(); ok && idx > max {
				max = idx
			}
		}
	}
	return max + 1
}

func (r *FirewallRule) wireRuleIndex() (int, bool) {
	v, ok := r.raw["ruleIndex"].(float64)
	if !ok {
		return 0, false
	}
	return int(v), true
}

// toWire renders one rule, allocating a ruleIndex from next when the rule does
// not already carry one.
func (r *FirewallRule) toWire(next *int) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range r.raw {
		out[k] = v
	}

	// Defaults for a rule this provider is creating, taken from the FWRULE
	// constructor in Synology's own header. Existing rules keep whatever DSM had.
	setIfAbsent(out, "table", "filter")
	setIfAbsent(out, "chainList", []interface{}{"INPUT_FIREWALL", "FORWARD_FIREWALL"})
	setIfAbsent(out, "labelList", []interface{}{})
	setIfAbsent(out, "blLog", false)
	setIfAbsent(out, "adapterDirect", fwDirectSrc)
	setIfAbsent(out, "ipDirect", fwDirectSrc)
	setIfAbsent(out, "portDirect", fwDirectDest)

	if _, ok := out["ruleIndex"]; !ok {
		out["ruleIndex"] = *next
		*next++
	}

	out["name"] = r.Name
	out["enable"] = r.Enabled
	out["policy"] = firewallPolicyToWire(r.Action)
	out["protocol"] = firewallProtocolToWire(r.Protocol)

	// Only rewrite a selector the provider actually models. A GeoIP or service
	// selector is left exactly as DSM sent it.
	if r.PortKind == firewallSelectorModelled {
		group, list, err := portsToWire(r.Ports)
		if err == nil {
			out["portGroup"] = group
			out["portList"] = toInterfaceSlice(list)
		}
	}
	if r.SourceKind == firewallSelectorModelled {
		group, list, ipType, err := sourcesToWire(r.Sources)
		if err == nil {
			out["ipGroup"] = group
			out["ipList"] = toInterfaceSlice(list)
			out["ipType"] = ipType
		}
	}

	return out
}

func setIfAbsent(m map[string]interface{}, key string, value interface{}) {
	if _, ok := m[key]; !ok {
		m[key] = value
	}
}

func toInterfaceSlice(values []string) []interface{} {
	out := make([]interface{}, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// portsToWire maps the provider's port list onto DSM's portGroup/portList pair.
func portsToWire(ports []string) (int, []string, error) {
	if len(ports) == 0 {
		return fwPortGroupAll, []string{}, nil
	}

	out := make([]string, 0, len(ports))
	for _, spec := range ports {
		spec = strings.TrimSpace(spec)
		if _, _, ok := parsePortSpec(spec); !ok {
			return 0, nil, fmt.Errorf(
				"invalid port %q: expected a port such as 5001 or a range such as 8000-8100", spec)
		}
		out = append(out, spec)
	}
	return fwPortGroupCustom, out, nil
}

// sourcesToWire maps the provider's source list onto DSM's ipGroup/ipList/ipType
// triple.
//
// A DSM rule carries exactly one kind of source selector, which is a real
// constraint on what a single rule can express: a netmask rule holds one network,
// a range rule holds one range, and only the ipset form holds several entries —
// and then every entry must be a plain address. Rejecting the combinations DSM
// cannot store is better than writing a rule that silently means something else.
func sourcesToWire(sources []string) (int, []string, int, error) {
	if len(sources) == 0 {
		return fwIPGroupAll, []string{}, fwIPTypeAll, nil
	}

	if len(sources) == 1 {
		spec := strings.TrimSpace(sources[0])

		if strings.Contains(spec, "/") {
			ip, network, err := net.ParseCIDR(spec)
			if err != nil {
				return 0, nil, 0, fmt.Errorf("invalid source %q: not a valid CIDR", spec)
			}
			if ip.To4() == nil {
				return 0, nil, 0, fmt.Errorf(
					"invalid source %q: DSM stores a network rule as an address plus a dotted netmask, which has no IPv6 form; "+
						"use a range such as 2001:db8::1-2001:db8::ff instead", spec)
			}
			mask := net.IP(network.Mask).String()
			return fwIPGroupNetmask, []string{network.IP.String(), mask}, fwIPTypeV4, nil
		}

		if low, high, ok := splitIPRange(spec); ok {
			return fwIPGroupRange, []string{low.String(), high.String()}, ipTypeOf(low), nil
		}

		ip := net.ParseIP(spec)
		if ip == nil {
			return 0, nil, 0, fmt.Errorf(
				"invalid source %q: expected an address, a CIDR such as 10.0.0.0/16, or a range such as 10.0.0.1-10.0.0.9", spec)
		}
		return fwIPGroupIP, []string{ip.String()}, ipTypeOf(ip), nil
	}

	list := make([]string, 0, len(sources))
	ipType := -1
	for _, spec := range sources {
		spec = strings.TrimSpace(spec)
		ip := net.ParseIP(spec)
		if ip == nil {
			return 0, nil, 0, fmt.Errorf(
				"invalid source %q: a rule with several sources can only list plain addresses, because DSM stores multiple "+
					"sources as an address set; put a CIDR or a range in a rule of its own", spec)
		}
		if t := ipTypeOf(ip); ipType == -1 {
			ipType = t
		} else if ipType != t {
			return 0, nil, 0, fmt.Errorf("firewall rule sources must all be IPv4 or all be IPv6, not a mix")
		}
		list = append(list, ip.String())
	}
	return fwIPGroupIPSet, list, ipType, nil
}

func ipTypeOf(ip net.IP) int {
	if ip.To4() != nil {
		return fwIPTypeV4
	}
	return fwIPTypeV6
}

func splitIPRange(spec string) (net.IP, net.IP, bool) {
	idx := strings.Index(spec, "-")
	if idx <= 0 {
		return nil, nil, false
	}
	low := net.ParseIP(strings.TrimSpace(spec[:idx]))
	high := net.ParseIP(strings.TrimSpace(spec[idx+1:]))
	if low == nil || high == nil {
		return nil, nil, false
	}
	return low, high, true
}

// Policy spellings DSM uses on the wire in the adapter-keyed shape. CONFIRMED
// on virtual DSM 7.2.2: `global` reads back as "none" and a written "drop"
// round-trips. They are the lowercase names of FW_POLICY from fwDB.hpp, so
// "partial" is included for completeness even though nothing has ever answered
// with it.
//
// Note that DSM's spelling of FW_POLICY_DROP is "drop" while this provider
// spells the same value "deny" (FirewallPolicyDeny), so the two must not be used
// interchangeably. allow and none coincide.
const (
	fwPolicyWireDrop    = "drop"
	fwPolicyWirePartial = "partial"
)

// firewallAdapterPolicyFromWire reads a per-adapter policy DSM sent, in either
// encoding: the string of the adapter-keyed shape or the FW_POLICY integer of
// the on-disk one.
func firewallAdapterPolicyFromWire(value interface{}) (int, bool) {
	if s, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case FirewallPolicyAllow:
			return fwPolicyAllow, true
		case fwPolicyWireDrop:
			return fwPolicyDrop, true
		case FirewallPolicyNone:
			return fwPolicyNone, true
		case fwPolicyWirePartial:
			return fwPolicyPartial, true
		default:
			// Not a policy word. Falling through to toInt would read "deny" as a
			// failed number rather than as the provider's own spelling reaching the
			// wire by mistake, so say no here instead.
			return 0, false
		}
	}
	return toInt(value)
}

// firewallPolicyToWireString renders a policy for the adapter-keyed shape.
//
// An unrecognised value becomes "none", matching FirewallPolicyName: rendering a
// future DSM's fifth policy as "allow" or "drop" would write a security setting
// nobody asked for.
func firewallPolicyToWireString(value int) string {
	switch value {
	case fwPolicyAllow:
		return FirewallPolicyAllow
	case fwPolicyDrop:
		return fwPolicyWireDrop
	case fwPolicyPartial:
		return fwPolicyWirePartial
	default:
		return FirewallPolicyNone
	}
}

func firewallPolicyToWire(action string) int {
	if action == FirewallActionDeny {
		return fwPolicyDrop
	}
	return fwPolicyAllow
}

func firewallPolicyFromWire(policy int) string {
	if policy == fwPolicyAllow {
		return FirewallActionAllow
	}
	return FirewallActionDeny
}

func firewallProtocolToWire(protocol string) int {
	switch protocol {
	case FirewallProtocolTCP:
		return fwProtoTCP
	case FirewallProtocolUDP:
		return fwProtoUDP
	case FirewallProtocolICMP:
		return fwProtoICMP
	default:
		return fwProtoAll
	}
}

func firewallProtocolFromWire(protocol int) string {
	switch protocol {
	case fwProtoTCP:
		return FirewallProtocolTCP
	case fwProtoUDP:
		return FirewallProtocolUDP
	case fwProtoICMP:
		return FirewallProtocolICMP
	default:
		return FirewallProtocolAll
	}
}

// parseFirewallProfile turns a `Profile get` response into the provider's model.
//
// The shape is decided from the response itself, and from the *values* rather
// than from the key names. A top-level `rules` whose entries are rule arrays, or
// an `adapterPolicyMap` whose entries are policies, means the on-disk shape from
// Synology's fwDB.hpp; top-level keys whose value is an object carrying `policy`
// or `rules` mean the adapter-keyed shape a live DSM answers with. A response
// that is neither is a FirewallProfileShapeError rather than an empty profile:
// every write here is read-modify-write, so an unrecognised response taken at
// face value would be written straight back and would replace the operator's
// rules.
//
// Keying the decision on the presence of `rules`/`adapterPolicyMap` was the
// second half of issue #130. A DSM 7 answers adapter-keyed *and* repeats those
// two names as sibling {policy, rules} objects (see firewallAdapterBlocks), so
// the old test picked the on-disk shape for a NAS that speaks the other one --
// and toWire then wrote back a profile DSM answers `success: true` to and
// discards. Hence: both keys must look the part, not merely be present.
func parseFirewallProfile(raw json.RawMessage, name string) (*FirewallProfile, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	profile := &FirewallProfile{
		Rules:         map[string][]FirewallRule{},
		AdapterPolicy: map[string]int{},
		raw:           m,
	}
	if v, ok := m["name"].(string); ok {
		profile.Name = v
	}

	rulesValue, hasRulesKey := m[firewallRulesKey]
	policyValue, hasPolicyMapKey := m[firewallPolicyMapKey]
	onDisk := looksLikeFirewallRulesMap(rulesValue) || looksLikeFirewallPolicyMap(policyValue)

	// An adapter block outranks a bare `rules`/`adapterPolicyMap` key, because
	// only one of the two readings can be right and a real adapter entry is
	// evidence while a key name is not.
	if blocks := firewallAdapterBlocks(m); !onDisk && len(blocks) > 0 {
		profile.shape = firewallShapeAdapterKeyed
		parseFirewallAdapterKeyedProfile(profile, blocks)
		return profile, nil
	}

	// Either the on-disk shape carrying content, or an empty profile in it:
	// `{"rules": {}, "adapterPolicyMap": {}}` says nothing about its entries, and
	// there are no adapter blocks to prefer over it.
	if onDisk || hasRulesKey || hasPolicyMapKey {
		profile.shape = firewallShapeRulesMap
		profile.rulesKeyPresent = hasRulesKey
		parseFirewallRulesMapProfile(profile, m)
		return profile, nil
	}

	return nil, &FirewallProfileShapeError{Profile: name, Keys: sortedAnyKeys(m)}
}

// The two top-level key names of the on-disk shape. They are also the two names
// a DSM 7 repeats as sibling adapter blocks in the adapter-keyed shape, which is
// why every use of them is guarded by a look at the value.
const (
	firewallRulesKey     = "rules"
	firewallPolicyMapKey = "adapterPolicyMap"
)

// looksLikeFirewallAdapterBlock reports whether an object is one adapter's
// {"policy": ..., "rules": [...]} entry rather than a map keyed by adapter name.
//
// The distinction is only ever needed for the two objects DSM sends under the
// on-disk shape's own key names; everywhere else the key tells you.
func looksLikeFirewallAdapterBlock(m map[string]interface{}) bool {
	if policy, ok := m["policy"]; ok {
		if _, ok := firewallAdapterPolicyFromWire(policy); ok {
			return true
		}
	}
	if rules, ok := m["rules"]; ok {
		if rules == nil {
			return true
		}
		if _, ok := rules.([]interface{}); ok {
			return true
		}
	}
	return false
}

// looksLikeFirewallRulesMap reports whether a value is the on-disk `rules`:
// an object mapping adapter names to rule arrays.
//
// Empty is not enough to go on and answers no -- `{}` is the same object in both
// shapes, and the caller falls back to the key name when there is nothing else.
func looksLikeFirewallRulesMap(value interface{}) bool {
	m, ok := value.(map[string]interface{})
	if !ok || len(m) == 0 || looksLikeFirewallAdapterBlock(m) {
		return false
	}
	for _, v := range m {
		if v == nil {
			// DSM answers an adapter with no rules as null as readily as [].
			continue
		}
		if _, ok := v.([]interface{}); !ok {
			return false
		}
	}
	return true
}

// looksLikeFirewallPolicyMap reports whether a value is the on-disk
// `adapterPolicyMap`: an object mapping adapter names to policies.
func looksLikeFirewallPolicyMap(value interface{}) bool {
	m, ok := value.(map[string]interface{})
	if !ok || len(m) == 0 || looksLikeFirewallAdapterBlock(m) {
		return false
	}
	for _, v := range m {
		if _, ok := firewallAdapterPolicyFromWire(v); !ok {
			return false
		}
	}
	return true
}

// parseFirewallRulesMapProfile reads {"rules": {<adapter>: [...]},
// "adapterPolicyMap": {<adapter>: int}}.
func parseFirewallRulesMapProfile(profile *FirewallProfile, m map[string]interface{}) {
	if rules, ok := m["rules"].(map[string]interface{}); ok {
		for adapter, value := range rules {
			list, ok := value.([]interface{})
			if !ok {
				continue
			}
			var skipped int
			profile.Rules[adapter], skipped = parseFirewallRuleList(list)
			profile.noteUnparsedRules(adapter, skipped)
		}
	}

	if policies, ok := m["adapterPolicyMap"].(map[string]interface{}); ok {
		for adapter, value := range policies {
			if n, ok := firewallAdapterPolicyFromWire(value); ok {
				profile.AdapterPolicy[adapter] = n
			}
		}
	}
}

// parseFirewallAdapterKeyedProfile reads {"<adapter>": {"policy": "drop",
// "rules": [...]}}, the shape captured from virtual DSM 7.2.2.
func parseFirewallAdapterKeyedProfile(profile *FirewallProfile, blocks map[string]map[string]interface{}) {
	for adapter, block := range blocks {
		if policy, ok := block["policy"]; ok {
			if n, ok := firewallAdapterPolicyFromWire(policy); ok {
				profile.AdapterPolicy[adapter] = n
			}
		}

		value, ok := block["rules"]
		if !ok {
			// Policy but no rule list. Recorded as such rather than as an empty
			// list, so a write does not invent a `rules` key DSM never sent.
			continue
		}
		profile.rulesKeyPresent = true
		// DSM answers `"rules": null` as readily as `[]` (Firewall.Rules `load`
		// does exactly that), and both mean the same thing.
		list, _ := value.([]interface{})
		var skipped int
		profile.Rules[adapter], skipped = parseFirewallRuleList(list)
		profile.noteUnparsedRules(adapter, skipped)
	}
}

// noteUnparsedRules records rule entries DSM sent that the parser could not
// read. See FirewallProfile.unparsedRules.
func (p *FirewallProfile) noteUnparsedRules(adapter string, skipped int) {
	if skipped == 0 {
		return
	}
	if p.unparsedRules == nil {
		p.unparsedRules = map[string]int{}
	}
	p.unparsedRules[adapter] += skipped
}

// parseFirewallRuleList reads one adapter's rule array, returning the rules and
// the number of entries it could not read.
func parseFirewallRuleList(list []interface{}) ([]FirewallRule, int) {
	parsed := make([]FirewallRule, 0, len(list))
	skipped := 0
	for _, item := range list {
		obj, ok := item.(map[string]interface{})
		if !ok {
			skipped++
			continue
		}
		rule := parseFirewallRule(obj)
		rule.Priority = len(parsed)
		parsed = append(parsed, *rule)
	}
	return parsed, skipped
}

// parseFirewallRule turns one DSM rule object into the provider's model, keeping
// the original map so a later write can put back everything it did not touch.
func parseFirewallRule(m map[string]interface{}) *FirewallRule {
	rule := &FirewallRule{
		raw:      m,
		Action:   FirewallActionAllow,
		Protocol: FirewallProtocolAll,
		Enabled:  true,
	}

	if v, ok := m["name"].(string); ok {
		rule.Name = v
	}
	if v, ok := toBool(m["enable"]); ok {
		rule.Enabled = v
	}
	// A rule's policy is read through the same converter as an adapter's, so the
	// string spelling is understood as well as the FW_POLICY integer. Nothing has
	// captured a rule out of an adapter-keyed profile, but if one arrives with
	// `"policy": "drop"` the dangerous direction is reading it as the default,
	// allow: the lockout replay would then believe a deny rule lets this session
	// through.
	if v, ok := firewallAdapterPolicyFromWire(m["policy"]); ok {
		rule.Action = firewallPolicyFromWire(v)
	}
	if v, ok := toInt(m["protocol"]); ok {
		rule.Protocol = firewallProtocolFromWire(v)
	}

	rule.Ports, rule.PortKind = parseFirewallPorts(m)
	rule.Sources, rule.SourceKind = parseFirewallSources(m)

	return rule
}

// parseFirewallPorts reads portGroup/portList. A rule that selects ports through
// one of DSM's service presets names services rather than numbers; that is
// recorded as unmodelled so the lockout evaluator refuses to guess instead of
// reading it as "no ports, therefore no match".
func parseFirewallPorts(m map[string]interface{}) ([]string, firewallSelectorKind) {
	group, ok := toInt(m["portGroup"])
	if !ok {
		return nil, firewallPortUnmodelled
	}

	switch group {
	case fwPortGroupAll:
		return nil, firewallSelectorModelled
	case fwPortGroupCustom:
		list := stringList(m["portList"])
		for _, v := range list {
			if _, _, ok := parsePortSpec(v); !ok {
				return nil, firewallPortUnmodelled
			}
		}
		return list, firewallSelectorModelled
	default:
		// SERVICE and RESERVED name DSM applications, which this provider cannot
		// resolve to port numbers.
		return nil, firewallPortUnmodelled
	}
}

// parseFirewallSources reads ipGroup/ipList, marking GeoIP country rules
// unmodelled for the same reason.
func parseFirewallSources(m map[string]interface{}) ([]string, firewallSelectorKind) {
	group, ok := toInt(m["ipGroup"])
	if !ok {
		return nil, firewallSourceUnmodelled
	}
	list := stringList(m["ipList"])

	switch group {
	case fwIPGroupAll:
		return nil, firewallSelectorModelled

	case fwIPGroupIP:
		if len(list) != 1 || net.ParseIP(list[0]) == nil {
			return nil, firewallSourceUnmodelled
		}
		return list, firewallSelectorModelled

	case fwIPGroupNetmask:
		if len(list) != 2 {
			return nil, firewallSourceUnmodelled
		}
		ip := net.ParseIP(list[0]).To4()
		mask := net.ParseIP(list[1]).To4()
		if ip == nil || mask == nil {
			return nil, firewallSourceUnmodelled
		}
		ones, bits := net.IPMask(mask).Size()
		if bits == 0 {
			return nil, firewallSourceUnmodelled
		}
		return []string{fmt.Sprintf("%s/%d", ip.String(), ones)}, firewallSelectorModelled

	case fwIPGroupRange:
		if len(list) != 2 || net.ParseIP(list[0]) == nil || net.ParseIP(list[1]) == nil {
			return nil, firewallSourceUnmodelled
		}
		return []string{list[0] + "-" + list[1]}, firewallSelectorModelled

	case fwIPGroupIPSet:
		for _, v := range list {
			if net.ParseIP(v) == nil {
				return nil, firewallSourceUnmodelled
			}
		}
		return list, firewallSelectorModelled

	default:
		// GEOIP and anything a future DSM adds.
		return nil, firewallSourceUnmodelled
	}
}

func toInt(v interface{}) (int, bool) {
	switch val := v.(type) {
	case float64:
		return int(val), true
	case int:
		return val, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func toBool(v interface{}) (bool, bool) {
	switch val := v.(type) {
	case bool:
		return val, true
	case float64:
		return val != 0, true
	case string:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return false, false
		}
		return b, true
	}
	return false, false
}

func stringList(v interface{}) []string {
	list, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		switch val := item.(type) {
		case string:
			if s := strings.TrimSpace(val); s != "" {
				out = append(out, s)
			}
		case float64:
			out = append(out, strconv.FormatFloat(val, 'f', -1, 64))
		}
	}
	return out
}

// BuildFirewallRuleID renders the composite identity used by the Terraform
// resource: profile, adapter, and rule name.
func BuildFirewallRuleID(profile, adapter, name string) string {
	return fmt.Sprintf("%s:%s:%s", profile, adapter, name)
}

// ParseFirewallRuleID splits an ID produced by BuildFirewallRuleID. The name is
// taken as the remainder, so rule names may contain colons.
func ParseFirewallRuleID(id string) (profile, adapter, name string, err error) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("invalid firewall rule ID %q: expected format profile:adapter:rule_name", id)
	}
	return parts[0], parts[1], parts[2], nil
}
