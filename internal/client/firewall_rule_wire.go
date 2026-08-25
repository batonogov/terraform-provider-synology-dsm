package client

import (
	"fmt"
	"net"
	"strings"
)

// The adapter-keyed encoding of a firewall rule.
//
// CONFIRMED against physical DSM 7 (issue #130): every field below was written
// through SYNO.Core.Security.Firewall.Profile `set` and read back unchanged, and
// the same names appear both in DSM's own web client
// (webman/modules/AdminCenter/admin_center.js, which builds exactly this object)
// and as string literals in the webapi shim
// (webapi/lib/SYNO.Core.Security.Firewall.so).
//
// A rule has exactly these ten fields. The provider used to send the *on-disk*
// encoding instead -- `ruleIndex`, `ipType`, `ipGroup`, `ipList`, `portGroup`,
// `portList`, `direct` -- which is what libsynofirewall writes to
// /usr/syno/etc/firewall.d/*.json but not what the shim speaks. Those names mean
// nothing to the shim's parser, and feeding them to it is what crashed synoscgi
// and took the DSM web interface down with it. Getting this list right is
// therefore not a matter of tidiness.
const (
	fwRuleKeyName          = "name"
	fwRuleKeyEnable        = "enable"
	fwRuleKeyLog           = "log"
	fwRuleKeyPolicy        = "policy"
	fwRuleKeyPortGroup     = "port_group"
	fwRuleKeyPorts         = "ports"
	fwRuleKeyPortDirection = "port_direction"
	fwRuleKeyProtocol      = "protocol"
	fwRuleKeySourceIP      = "source_ip"
	fwRuleKeySourceIPGroup = "source_ip_group"
)

// Values of `port_group` and `source_ip_group`, and the two defaults DSM fills
// in for an empty field. All CONFIRMED by round trip.
const (
	fwPortGroupWireAll     = "all"
	fwPortGroupWireService = "service"
	fwPortGroupWireCustom  = "custom"

	fwSourceWireAll     = "all"
	fwSourceWireIP      = "ip"
	fwSourceWireIPSet   = "ipset"
	fwSourceWireRange   = "iprange"
	fwSourceWireNetmask = "netmask"
	fwSourceWireGeoIP   = "geoip"

	// DSM answers `port_direction` as "destination" on every rule captured, and
	// normalises an empty one to it. "src" exists in the shim's string table and
	// in the web client, and is kept when DSM sends it.
	fwPortDirectionDest = "destination"

	fwWireAll = "all"
)

// looksLikeAdapterKeyedRule reports whether a rule object is in the adapter-keyed
// encoding rather than the on-disk one.
//
// The two share `name`, `policy`, `enable` and `protocol` but disagree on
// everything that matters, and they disagree on *type* as well as on name:
// `policy` is a string here and an integer on disk, `ports` is a string here and
// `portList` an array on disk. Either selector key is enough to tell them apart.
func looksLikeAdapterKeyedRule(m map[string]interface{}) bool {
	for _, key := range []string{fwRuleKeyPortGroup, fwRuleKeySourceIPGroup, fwRuleKeySourceIP} {
		if _, ok := m[key]; ok {
			return true
		}
	}
	// A rule with no selector at all: fall back to the type of `policy`, which is
	// a string in this encoding and an integer on disk.
	_, isString := m[fwRuleKeyPolicy].(string)
	return isString
}

// parseAdapterKeyedRule reads one rule in the adapter-keyed encoding.
func parseAdapterKeyedRule(m map[string]interface{}) *FirewallRule {
	rule := &FirewallRule{
		raw:      m,
		Action:   FirewallActionAllow,
		Protocol: FirewallProtocolAll,
		Enabled:  true,
	}

	if v, ok := m[fwRuleKeyName].(string); ok {
		rule.Name = v
	}
	if v, ok := toBool(m[fwRuleKeyEnable]); ok {
		rule.Enabled = v
	}
	if v, ok := firewallAdapterPolicyFromWire(m[fwRuleKeyPolicy]); ok {
		rule.Action = firewallPolicyFromWire(v)
	}
	if v, ok := m[fwRuleKeyProtocol].(string); ok {
		rule.Protocol = firewallProtocolFromWireString(v)
	}

	rule.Ports, rule.PortKind = parseAdapterKeyedPorts(m)
	rule.Sources, rule.SourceKind = parseAdapterKeyedSources(m)
	return rule
}

// parseAdapterKeyedPorts reads port_group/ports.
//
// A `service` rule names DSM applications ("ssh", "cifs") rather than port
// numbers, and the provider cannot resolve those to ports -- it is recorded as
// unmodelled so the lockout replay refuses to guess rather than reading it as
// "no ports, therefore no match".
func parseAdapterKeyedPorts(m map[string]interface{}) ([]string, firewallSelectorKind) {
	group, ok := m[fwRuleKeyPortGroup].(string)
	if !ok {
		return nil, firewallPortUnmodelled
	}

	switch group {
	case fwPortGroupWireAll:
		return nil, firewallSelectorModelled

	case fwPortGroupWireCustom:
		spec, _ := m[fwRuleKeyPorts].(string)
		list := splitWireList(spec)
		if len(list) == 0 {
			return nil, firewallSelectorModelled
		}
		out := make([]string, 0, len(list))
		for _, v := range list {
			// DSM's own UI writes a range as "20000:20010" and accepts "8000-8100"
			// unchanged; the provider models one spelling, so the other is folded
			// into it on the way in.
			v = strings.ReplaceAll(v, ":", "-")
			if _, _, ok := parsePortSpec(v); !ok {
				return nil, firewallPortUnmodelled
			}
			out = append(out, v)
		}
		return out, firewallSelectorModelled

	default:
		// "service", and anything a future DSM adds.
		return nil, firewallPortUnmodelled
	}
}

// parseAdapterKeyedSources reads source_ip/source_ip_group. GeoIP names
// countries, which the provider cannot expand into addresses.
func parseAdapterKeyedSources(m map[string]interface{}) ([]string, firewallSelectorKind) {
	group, ok := m[fwRuleKeySourceIPGroup].(string)
	if !ok {
		return nil, firewallSourceUnmodelled
	}
	spec, _ := m[fwRuleKeySourceIP].(string)

	switch group {
	case fwSourceWireAll:
		return nil, firewallSelectorModelled

	case fwSourceWireIP, fwSourceWireIPSet:
		list := splitWireList(spec)
		if len(list) == 0 {
			return nil, firewallSourceUnmodelled
		}
		for _, v := range list {
			if net.ParseIP(v) == nil {
				return nil, firewallSourceUnmodelled
			}
		}
		return list, firewallSelectorModelled

	case fwSourceWireRange:
		if _, _, ok := splitIPRange(spec); !ok {
			return nil, firewallSourceUnmodelled
		}
		return []string{spec}, firewallSelectorModelled

	case fwSourceWireNetmask:
		// DSM stores the network in prefix form here ("10.0.0.0/24"), unlike the
		// on-disk encoding, which splits it into an address and a dotted netmask.
		if _, _, err := net.ParseCIDR(spec); err != nil {
			return nil, firewallSourceUnmodelled
		}
		return []string{spec}, firewallSelectorModelled

	default:
		// "geoip", and anything a future DSM adds.
		return nil, firewallSourceUnmodelled
	}
}

// toWireAdapterKeyed renders one rule in the adapter-keyed encoding.
//
// The render starts from whatever DSM sent, so a field this provider does not
// model rides along unchanged, and only the managed keys are overwritten. A
// selector the provider could not read (a service preset, a GeoIP country) is
// left exactly as DSM sent it rather than replaced with a guess.
func (r *FirewallRule) toWireAdapterKeyed() (map[string]interface{}, error) {
	out := map[string]interface{}{}
	for k, v := range r.raw {
		out[k] = v
	}

	out[fwRuleKeyName] = r.Name
	out[fwRuleKeyEnable] = r.Enabled
	// `policy` must be DSM's spelling. Sending the provider's own word for the
	// same value -- "deny" -- is not rejected: DSM stores the rule with policy
	// "none", which matches nothing, so a deny rule silently stops denying.
	// CONFIRMED on hardware.
	out[fwRuleKeyPolicy] = firewallPolicyToWireString(firewallPolicyToWire(r.Action))
	out[fwRuleKeyProtocol] = firewallProtocolToWireString(r.Protocol)

	// DSM normalises an empty port_direction to "destination"; sending it that way
	// keeps the write and the read-back agreeing.
	setIfAbsent(out, fwRuleKeyPortDirection, fwPortDirectionDest)
	if _, ok := out[fwRuleKeyPortDirection].(string); !ok {
		out[fwRuleKeyPortDirection] = fwPortDirectionDest
	}
	// `log` is write-ignored: DSM accepts true and always answers false. It is
	// carried through rather than managed, so nothing plans a diff on it.
	setIfAbsent(out, fwRuleKeyLog, false)

	// A selector is only re-rendered when it actually changed. DSM's own UI writes
	// a port range as "20000:20010" while this provider models it as
	// "20000-20010", and both mean the same ports — rewriting one into the other
	// on every profile write would silently edit rules the operator created by
	// hand, because a `set` carries the whole profile and not just the rule being
	// managed.
	if r.PortKind == firewallSelectorModelled && !r.portsUnchanged() {
		group, ports := portsToWireAdapterKeyed(r.Ports)
		out[fwRuleKeyPortGroup] = group
		out[fwRuleKeyPorts] = ports
	}
	if r.SourceKind == firewallSelectorModelled && !r.sourcesUnchanged() {
		group, source, err := sourcesToWireAdapterKeyed(r.Sources)
		if err != nil {
			return nil, err
		}
		out[fwRuleKeySourceIPGroup] = group
		out[fwRuleKeySourceIP] = source
	}

	// The on-disk encoding's keys must never travel in this shape. A rule adopted
	// from a profile read in the other shape would otherwise carry them along and
	// hand the shim's parser exactly the payload that crashes it.
	for _, key := range []string{"ruleIndex", "ipType", "ipGroup", "ipList", "portGroup", "portList", "direct",
		"adapterDirect", "ipDirect", "portDirect", "table", "chainList", "labelList", "blLog"} {
		delete(out, key)
	}

	return out, nil
}

// portsUnchanged reports whether the model still says what the object DSM sent
// says, so the write can leave DSM's own spelling alone.
func (r *FirewallRule) portsUnchanged() bool {
	if r.raw == nil {
		return false
	}
	was, kind := parseAdapterKeyedPorts(r.raw)
	return kind == firewallSelectorModelled && equalStrings(was, r.Ports)
}

// sourcesUnchanged is the same question for the source selector. DSM accepts a
// list with spaces after the commas and stores it verbatim, so this keeps a
// cosmetic difference from being written back as a change.
func (r *FirewallRule) sourcesUnchanged() bool {
	if r.raw == nil {
		return false
	}
	was, kind := parseAdapterKeyedSources(r.raw)
	return kind == firewallSelectorModelled && equalStrings(was, r.Sources)
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

// portsToWireAdapterKeyed maps the provider's port list onto port_group/ports.
func portsToWireAdapterKeyed(ports []string) (string, string) {
	if len(ports) == 0 {
		return fwPortGroupWireAll, fwWireAll
	}
	return fwPortGroupWireCustom, strings.Join(ports, ",")
}

// sourcesToWireAdapterKeyed maps the provider's source list onto
// source_ip_group/source_ip.
//
// One rule carries one kind of selector, which is a real constraint on what a
// rule can say: a network rule holds one network, a range rule one range, and
// only the ipset form holds several entries -- and then every entry must be a
// plain address. Rejecting what DSM cannot store beats writing a rule that
// quietly means something else.
func sourcesToWireAdapterKeyed(sources []string) (string, string, error) {
	if len(sources) == 0 {
		return fwSourceWireAll, fwWireAll, nil
	}

	if len(sources) == 1 {
		spec := strings.TrimSpace(sources[0])

		if strings.Contains(spec, "/") {
			if _, _, err := net.ParseCIDR(spec); err != nil {
				return "", "", fmt.Errorf("invalid source %q: not a valid CIDR", spec)
			}
			return fwSourceWireNetmask, spec, nil
		}
		if low, high, ok := splitIPRange(spec); ok {
			return fwSourceWireRange, fmt.Sprintf("%s-%s", low, high), nil
		}
		if net.ParseIP(spec) == nil {
			return "", "", fmt.Errorf(
				"invalid source %q: expected an address, a CIDR such as 10.0.0.0/16, or a range such as 10.0.0.1-10.0.0.9", spec)
		}
		return fwSourceWireIP, spec, nil
	}

	list := make([]string, 0, len(sources))
	for _, spec := range sources {
		spec = strings.TrimSpace(spec)
		if net.ParseIP(spec) == nil {
			return "", "", fmt.Errorf(
				"invalid source %q: a rule with several sources can only list plain addresses, because DSM stores multiple "+
					"sources as an address set; put a CIDR or a range in a rule of its own", spec)
		}
		list = append(list, spec)
	}
	return fwSourceWireIPSet, strings.Join(list, ","), nil
}

// firewallProtocolToWireString renders a protocol for the adapter-keyed shape.
// DSM spells them out; the provider's own words already match for the four it
// models.
func firewallProtocolToWireString(protocol string) string {
	switch protocol {
	case FirewallProtocolTCP, FirewallProtocolUDP, FirewallProtocolICMP:
		return protocol
	default:
		return fwWireAll
	}
}

// firewallProtocolFromWireString reads DSM's spelling. Protocols the provider
// does not model (igmp, gre, esp — all present in the shim's string table) are
// reported as "all" only for the lockout replay's benefit; the raw object is
// what gets written back, so nothing is lost.
func firewallProtocolFromWireString(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case FirewallProtocolTCP:
		return FirewallProtocolTCP
	case FirewallProtocolUDP:
		return FirewallProtocolUDP
	case FirewallProtocolICMP:
		return FirewallProtocolICMP
	default:
		return FirewallProtocolAll
	}
}

// splitWireList splits DSM's comma-separated list fields, tolerating spaces.
func splitWireList(spec string) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == fwWireAll {
		return nil
	}
	parts := strings.Split(spec, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
