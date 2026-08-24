package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// Default-policy spellings this provider uses for DSM's per-adapter
// "if no rules are matched" setting.
//
// allow and deny are the two choices the DSM firewall page offers. none is the
// third value the data model carries (FW_POLICY_NONE) and is what DSM stores for
// the `global` pseudo-interface, which is a pre-table rather than an interface
// and therefore has no fall-through of its own. It is accepted so a profile can
// be round-tripped without the provider inventing a policy DSM never had.
const (
	FirewallPolicyAllow = "allow"
	FirewallPolicyDeny  = "deny"
	FirewallPolicyNone  = "none"
)

// SetFirewallRequest is a write to the firewall's profile-level configuration:
// the global on/off switch, which profile is in force, and that profile's
// per-adapter default policy.
//
// It is deliberately a whole-object request rather than a set of optional
// pointers. All three settings interact — a default policy only matters while
// the firewall is on, and only for the profile in force — so a caller that could
// change one without stating the others would be asking the provider to guard a
// state it has not been told about.
type SetFirewallRequest struct {
	// Profile is the profile that must be in force, and whose default policy
	// DefaultPolicy applies to.
	Profile string
	// Enabled is the global firewall switch.
	Enabled bool
	// DefaultPolicy maps an adapter name to allow, deny, or none. Adapters absent
	// from the map are left exactly as DSM has them; a nil map changes no policy
	// at all.
	DefaultPolicy map[string]string

	// AllowLockout disables the guard that refuses a change which would stop this
	// client's own management session from reaching DSM.
	AllowLockout bool
	// AllowEmptyRuleSet permits switching the firewall on while the profile that
	// would be in force has no rules. Left false, that is refused for the same
	// reason deleting the last rule of an enabled profile is.
	AllowEmptyRuleSet bool
}

// FirewallSettingsResult is the state DSM reports after a profile-level write,
// plus an inconclusive lockout check if there was one.
type FirewallSettingsResult struct {
	Settings *FirewallSettings
	Profile  *FirewallProfile
	// LockoutWarning is non-nil when the write happened but the provider could
	// not prove its own session survives it.
	LockoutWarning *IndeterminateLockoutError
}

// SetFirewall writes the global switch, the active profile, and the active
// profile's default policy, in that order of consequence.
//
// The whole sequence runs under c.mu — the same lock the per-rule writes take.
// Without it a `dsm_firewall_rule` applied in parallel would read the profile
// this call is about to overwrite, and one of the two writes would be lost.
//
// Nothing is written when nothing differs from what DSM already has. That
// matters more here than usual: applying a profile makes DSM re-render and
// reload its iptables chains, which drops in-flight connections, and a refresh
// that quietly did that on every plan would be worse than the problem it solves.
func (c *Client) SetFirewall(ctx context.Context, req SetFirewallRequest) (*FirewallSettingsResult, error) {
	if req.Profile == "" {
		return nil, fmt.Errorf("firewall profile name must not be empty")
	}
	policy, err := firewallPolicyMapToWire(req.DefaultPolicy)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	current, err := c.GetFirewallSettings(ctx)
	if err != nil {
		return nil, err
	}

	before, err := c.GetFirewallProfile(ctx, req.Profile)
	if err != nil {
		return nil, err
	}

	after := before.clone()
	policyChanged := false
	for adapter, value := range policy {
		if existing, ok := after.AdapterPolicy[adapter]; !ok || existing != value {
			policyChanged = true
		}
		after.AdapterPolicy[adapter] = value
	}

	desired := FirewallSettings{Enabled: req.Enabled, ActiveProfile: req.Profile}
	settingsChanged := current.Enabled != desired.Enabled || current.ActiveProfile != desired.ActiveProfile

	// Reported separately from the lockout guard, and before it, because the two
	// say different things: an enabled profile with no rules denies everybody, not
	// merely this session, and it does so whether or not the guard can replay it.
	if desired.Enabled && after.ruleCount() == 0 && !req.AllowEmptyRuleSet {
		return nil, &EmptyRuleSetError{Profile: after.Name}
	}

	warning, err := c.guardFirewallSettings(ctx, current, desired, before, after, req.AllowLockout)
	if err != nil {
		return nil, err
	}

	if policyChanged {
		if err := c.saveFirewallProfile(ctx, after); err != nil {
			return nil, err
		}
	}
	if settingsChanged {
		if err := c.setFirewallSwitch(ctx, desired); err != nil {
			return nil, err
		}
	}
	// A saved profile is not a live one: DSM only re-renders its chains on an
	// explicit apply. Nothing is applied while the firewall is off, because there
	// is nothing to make live.
	if desired.Enabled && (policyChanged || settingsChanged) {
		if err := c.applyFirewallProfile(ctx, desired.ActiveProfile); err != nil {
			return nil, err
		}
	}

	settings, err := c.GetFirewallSettings(ctx)
	if err != nil {
		return nil, err
	}
	profile, err := c.GetFirewallProfile(ctx, req.Profile)
	if err != nil {
		return nil, err
	}

	// The read-back is here anyway, so it may as well be believed rather than
	// merely returned. Both writes above are reconstructed rather than captured
	// -- the switch's parameter names are inferred from what `get` answers with,
	// and the profile write is the same call that silently dropped every rule in
	// issue #130 -- so a success that changed nothing is a shape this code has to
	// be able to report. Reporting it as a completed apply would leave Terraform
	// claiming a firewall configuration the NAS does not have.
	if err := verifyFirewallSettings(req, desired, policy, settings, profile); err != nil {
		return nil, err
	}

	return &FirewallSettingsResult{Settings: settings, Profile: profile, LockoutWarning: warning}, nil
}

// verifyFirewallSettings compares the state DSM reports after the write with the
// state that was asked for.
//
// Only what was requested is checked. An adapter the caller said nothing about
// is not this write's business, and a profile that was not switched to is not
// evidence of anything.
func verifyFirewallSettings(
	req SetFirewallRequest,
	desired FirewallSettings,
	policy map[string]int,
	settings *FirewallSettings,
	profile *FirewallProfile,
) error {
	var mismatches []string

	if settings.Enabled != desired.Enabled {
		mismatches = append(mismatches,
			fmt.Sprintf("enabled: asked for %t, DSM reports %t", desired.Enabled, settings.Enabled))
	}
	if settings.ActiveProfile != desired.ActiveProfile {
		mismatches = append(mismatches,
			fmt.Sprintf("active profile: asked for %q, DSM reports %q", desired.ActiveProfile, settings.ActiveProfile))
	}

	for _, adapter := range sortedPolicyAdapters(policy) {
		want := policy[adapter]
		got, ok := profile.AdapterPolicy[adapter]
		switch {
		case !ok:
			mismatches = append(mismatches,
				fmt.Sprintf("default policy for adapter %q: asked for %q, DSM reports no policy for that adapter at all",
					adapter, FirewallPolicyName(want)))
		case got != want:
			mismatches = append(mismatches,
				fmt.Sprintf("default policy for adapter %q: asked for %q, DSM reports %q",
					adapter, FirewallPolicyName(want), FirewallPolicyName(got)))
		}
	}

	if len(mismatches) == 0 {
		return nil
	}
	return &FirewallSettingsNotPersistedError{Profile: req.Profile, Mismatches: mismatches}
}

func sortedPolicyAdapters(policy map[string]int) []string {
	out := make([]string, 0, len(policy))
	for adapter := range policy {
		out = append(out, adapter)
	}
	sort.Strings(out)
	return out
}

// FirewallSettingsNotPersistedError reports a profile-level firewall write DSM
// accepted and did not perform. It is the SetFirewall counterpart of
// FirewallNotPersistedError; see issue #130.
type FirewallSettingsNotPersistedError struct {
	Profile    string
	Mismatches []string
}

func (e *FirewallSettingsNotPersistedError) Error() string {
	return fmt.Sprintf(
		"DSM reported success but the firewall settings read back differently for profile %q: %s",
		e.Profile, strings.Join(e.Mismatches, "; "))
}

// setFirewallSwitch writes SYNO.Core.Security.Firewall itself: the global on/off
// flag and the name of the profile in force.
//
// Sources, and how far each of them goes:
//
//   - **CONFIRMED — the method is `set`.** DSM's own webapi descriptor for this
//     API lists exactly two methods on version 1, `set` and `get`, with
//     `authLevel: 1` and `allowUser: [admin.local, admin.domain, admin.ldap]`,
//     served by `lib/SYNO.Core.Security.Firewall.so`. (Dumped verbatim in
//     LeoMartinDev/synology-api `definitions.6.x.json`; that same dump
//     independently reproduces five quirks this provider learned the hard way —
//     `SYNO.Core.User` having no `update`, `SYNO.Core.Share` updating through
//     `set`, `AppPortal.ReverseProxy` using `update` rather than `set`,
//     `Region.NTP`'s method list, and `Firewall.Rules` having no `get`/`set`.)
//     Three independent DSM 7.x `SYNO.API.Info` dumps confirm the API is still
//     min = max = version 1 there. There is therefore no alternative method name
//     worth trying: `save`, `set_enable` and friends do not exist.
//
//   - **CONFIRMED — the object has exactly these two fields.** Synology's own
//     `synofirewall/synoFW.hpp` models the global state as a status flag plus an
//     active profile name, persisted to `firewall.d/firewall_settings.json` under
//     the keys `status` and `profile`. The webapi renames them, and `get` answers
//     `{"enable_firewall": bool, "profile_name": string}` — nothing else.
//
//   - **INFERRED (strong) — the parameter names on the write are the same two
//     keys `get` answers with.** One production Ansible deployment against DSM
//     7.2.2 and 7.3 writes exactly `enable_firewall` + `profile_name` to this API
//     (KastnerRG/krg-infra `apply_security.py`), which is the only published
//     implementation of the write that could be found anywhere.
//
//   - **INFERRED — the write is whole-object.** The same deployment reports that
//     DSM's settings APIs answer 2001 to a partial `set` and therefore always
//     read-modify-write. Both fields are sent here regardless of which one
//     changed; with a two-field object that costs nothing either way.
//
//   - **INFERRED — POST.** `get` on this API has been probed over POST, and the
//     DSM UI posts to entry.cgi; it is also the verb `Firewall.Profile set`
//     already uses in this package.
//
// Two things are still guesses, so both have a fallback rather than a bet:
// whether DSM wants the verb as POST or GET, and whether `profile_name` travels
// plain or JSON-quoted. DSM is inconsistent about the latter across APIs —
// `SYNO.Core.User` and `SYNO.Core.Share` take plain values, `SYNO.Docker.Project`
// and `Certificate.CRT` need quoting — and the one implementation that writes
// this API quotes it. sendFirewallSwitch tries the verbs; this function retries
// the encoding on the codes that mean "I did not like these parameters".
func (c *Client) setFirewallSwitch(ctx context.Context, s FirewallSettings) error {
	err := c.sendFirewallSwitch(ctx, firewallSwitchParams(s, false))
	if err == nil {
		return nil
	}

	// 120 / 2001 / 5701 are the codes DSM uses for "the parameters are wrong",
	// as opposed to "I cannot do this at all". Only those are worth a second
	// encoding; a 105 has already been answered by the verb fallback below.
	if IsAPIError(err, 120, 2001, 5701) {
		if quotedErr := c.sendFirewallSwitch(ctx, firewallSwitchParams(s, true)); quotedErr == nil {
			return nil
		}
	}

	if IsAPIError(err, 103) {
		return fmt.Errorf(
			"set firewall switch: DSM answered 103 to SYNO.Core.Security.Firewall `set` over both POST and GET. "+
				"That method is listed in DSM's own webapi descriptor for this API, so a 103 here is a real finding "+
				"rather than a wrong method name — switch the firewall in Control Panel -> Security -> Firewall and "+
				"please report the DSM version: %w", err)
	}
	return fmt.Errorf("set firewall switch: %w", err)
}

// sendFirewallSwitch issues one encoding of the call, POST first and GET on the
// two codes that mean "wrong shape of call" rather than "bad values" — 103 (no
// such method) and 105 (no permission for this operation, which DSM also returns
// for the wrong HTTP method). This mirrors setRegionNTP, for the same reason:
// this provider needs both verbs elsewhere, and no capture of DSM's own request
// exists to settle it.
func (c *Client) sendFirewallSwitch(ctx context.Context, params url.Values) error {
	_, err := c.DoAPIPost(ctx, "SYNO.Core.Security.Firewall", "1", "set", params)
	if err == nil {
		return nil
	}
	if !IsAPIError(err, 103, 105) {
		return err
	}

	if _, getErr := c.DoAPI(ctx, "SYNO.Core.Security.Firewall", "1", "set", params); getErr == nil {
		return nil
	}
	// Report the POST failure: it reflects the intended call, and surfacing the
	// fallback's error would send the reader after the wrong problem.
	return err
}

// firewallSwitchParams renders the two fields, optionally JSON-quoting the
// profile name. The boolean is never quoted: every DSM API on entry.cgi takes it
// as the bare string "true"/"false".
func firewallSwitchParams(s FirewallSettings, quoteStrings bool) url.Values {
	name := s.ActiveProfile
	if quoteStrings {
		if encoded, err := json.Marshal(name); err == nil {
			name = string(encoded)
		}
	}

	params := url.Values{}
	params.Set("enable_firewall", boolParam(s.Enabled))
	params.Set("profile_name", name)
	return params
}

// guardFirewallSettings decides whether a profile-level change would cut this
// client off, and refuses it if so.
//
// Two different comparisons live here, because the two transitions are not the
// same question.
//
// Switching the firewall ON has no real "before" to compare with: with the
// firewall off every adapter admits everything, so a per-adapter comparison would
// refuse any profile that denies on any interface — including the interface
// nobody uses — and the guard would be useless on the very change it exists for.
// What can be proved instead is the strong statement: after the change, *no*
// adapter would admit this session. That is a lockout with no room for doubt.
//
// While the firewall is already on there is a real before-state, so the same
// comparison the per-rule writes use applies: an adapter that admits the session
// today and would not afterwards is a lockout, and an adapter that already denied
// it is not this change's doing.
func (c *Client) guardFirewallSettings(
	ctx context.Context,
	current *FirewallSettings,
	desired FirewallSettings,
	before, after *FirewallProfile,
	allowLockout bool,
) (*IndeterminateLockoutError, error) {
	// Switching the firewall off, or leaving it off, cannot deny a packet.
	if allowLockout || !desired.Enabled {
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

	if current.Enabled {
		beforeProfile := before
		if current.ActiveProfile != after.Name {
			// A different profile is in force today; that one is the before-state.
			p, err := c.GetFirewallProfile(ctx, current.ActiveProfile)
			if err != nil {
				return &IndeterminateLockoutError{
					Profile: after.Name,
					Verdict: FirewallVerdict{
						Indeterminate: true,
						Reason: fmt.Sprintf("the profile in force (%q) could not be read, so the change cannot be compared with it: %v",
							current.ActiveProfile, err),
					},
				}, nil
			}
			beforeProfile = p
		}
		return compareFirewallReachability(beforeProfile, after, adapters, pkt, src, port)
	}

	return provesFirewallLockout(after, adapters, pkt, src, port)
}

// compareFirewallReachability is the per-adapter before/after comparison used
// while the firewall is already on.
func compareFirewallReachability(
	before, after *FirewallProfile,
	adapters []string,
	pkt FirewallPacket,
	src net.IP,
	port int,
) (*IndeterminateLockoutError, error) {
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

// provesFirewallLockout is the stricter test used when the firewall is being
// switched on: it refuses only when every adapter would deny this session, which
// is the one conclusion a replay can reach without knowing which interface the
// session actually arrives on.
func provesFirewallLockout(
	after *FirewallProfile,
	adapters []string,
	pkt FirewallPacket,
	src net.IP,
	port int,
) (*IndeterminateLockoutError, error) {
	denied := make([]string, 0, len(adapters))
	var indeterminate *IndeterminateLockoutError
	var lastDenial FirewallVerdict

	for _, adapter := range adapters {
		verdict := after.evaluateAccess(adapter, pkt)
		switch {
		case verdict.Indeterminate:
			if indeterminate == nil {
				indeterminate = &IndeterminateLockoutError{Profile: after.Name, Verdict: verdict}
			}
		case verdict.Allowed:
			// One adapter that admits the session is enough: the firewall cannot be
			// proved to cut it off, so the change goes ahead.
			return nil, nil
		default:
			denied = append(denied, adapter)
			lastDenial = verdict
		}
	}

	// Some adapter could not be replayed, so "every adapter denies" was never
	// established. Warn rather than block; the uncertainty comes from rules the
	// operator wrote elsewhere.
	if indeterminate != nil {
		return indeterminate, nil
	}

	if len(denied) > 0 {
		sort.Strings(denied)
		return nil, &LockoutError{
			Adapter: strings.Join(denied, ", "),
			Profile: after.Name,
			Source:  src,
			Port:    port,
			Verdict: lastDenial,
		}
	}
	return nil, nil
}

// firewallPolicyMapToWire validates a caller's adapter policy map and converts
// it to DSM's integers.
func firewallPolicyMapToWire(policies map[string]string) (map[string]int, error) {
	if len(policies) == 0 {
		return nil, nil
	}

	out := make(map[string]int, len(policies))
	for adapter, name := range policies {
		if adapter == "" {
			return nil, fmt.Errorf("firewall default policy: adapter name must not be empty")
		}
		value, ok := firewallPolicyValue(name)
		if !ok {
			return nil, fmt.Errorf(
				"unknown firewall default policy %q for adapter %q: must be %s, %s, or %s",
				name, adapter, FirewallPolicyAllow, FirewallPolicyDeny, FirewallPolicyNone)
		}
		out[adapter] = value
	}
	return out, nil
}

func firewallPolicyValue(name string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case FirewallPolicyAllow:
		return fwPolicyAllow, true
	case FirewallPolicyDeny:
		return fwPolicyDrop, true
	case FirewallPolicyNone:
		return fwPolicyNone, true
	default:
		return 0, false
	}
}

// FirewallPolicyName renders DSM's per-adapter default policy integer.
//
// An unrecognised value is reported as none rather than guessed at: reading a
// future DSM's fourth policy as "allow" or "deny" would put a wrong answer in a
// security audit, which is worse than an obviously unhelpful one.
func FirewallPolicyName(value int) string {
	switch value {
	case fwPolicyAllow:
		return FirewallPolicyAllow
	case fwPolicyDrop:
		return FirewallPolicyDeny
	default:
		return FirewallPolicyNone
	}
}

// DefaultPolicyNames renders the whole adapterPolicyMap for display.
func (p *FirewallProfile) DefaultPolicyNames() map[string]string {
	out := make(map[string]string, len(p.AdapterPolicy))
	for adapter, value := range p.AdapterPolicy {
		out[adapter] = FirewallPolicyName(value)
	}
	return out
}

// DefaultPolicyName reports one adapter's default policy, and whether DSM has
// one recorded for that adapter at all.
func (p *FirewallProfile) DefaultPolicyName(adapter string) (string, bool) {
	value, ok := p.AdapterPolicy[adapter]
	if !ok {
		return "", false
	}
	return FirewallPolicyName(value), true
}
