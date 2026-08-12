package client

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// FirewallVerdict is the result of replaying a rule list against one packet.
//
// Indeterminate is the important state. A firewall rule can express things this
// evaluator deliberately does not model — GeoIP countries, application presets,
// interfaces we cannot resolve. When such a rule is reached before any rule has
// matched, the outcome genuinely is unknown, and saying "allowed" or "denied"
// would be a guess dressed up as an answer. The caller is expected to degrade to
// a warning in that case rather than either block or wave the change through.
type FirewallVerdict struct {
	Allowed       bool
	Indeterminate bool
	// Reason describes which rule decided the verdict, in words fit for a
	// Terraform diagnostic.
	Reason string
}

// FirewallPacket is the traffic being tested. Only the fields a DSM rule can
// discriminate on are present; the firewall is stateless-inbound only.
type FirewallPacket struct {
	Source   net.IP
	DestPort int
	Protocol string // "tcp" or "udp"
}

// EvaluateFirewall replays rules in order against pkt and reports what would
// happen. defaultAllow is the profile's "if no rules are matched" policy.
//
// Semantics confirmed only as far as DSM's own UI describes them: rules are
// evaluated top to bottom, the first match wins, disabled rules are skipped, and
// unmatched traffic falls through to the default policy. That is the model the
// DSM firewall page presents to the operator, and it is the model this function
// implements.
func EvaluateFirewall(rules []FirewallRule, pkt FirewallPacket, defaultAllow bool) FirewallVerdict {
	for i := range rules {
		rule := &rules[i]
		if !rule.Enabled {
			continue
		}

		match, ok := ruleMatches(rule, pkt)
		if !ok {
			return FirewallVerdict{
				Indeterminate: true,
				Reason: fmt.Sprintf(
					"rule %d (%q) uses a selector this provider cannot evaluate, so the effect on the management session is unknown",
					i, rule.Name),
			}
		}
		if !match {
			continue
		}

		return FirewallVerdict{
			Allowed: rule.Action == FirewallActionAllow,
			Reason: fmt.Sprintf("rule %d (%q) matches first and its action is %q",
				i, rule.Name, rule.Action),
		}
	}

	policy := "deny"
	if defaultAllow {
		policy = "allow"
	}
	return FirewallVerdict{
		Allowed: defaultAllow,
		Reason:  fmt.Sprintf("no rule matches, so the profile default policy (%s) applies", policy),
	}
}

// ruleMatches reports whether rule selects pkt. The second return value is false
// when the rule cannot be evaluated at all, which is not the same as "no match".
func ruleMatches(rule *FirewallRule, pkt FirewallPacket) (bool, bool) {
	if rule.hasUnmodelledSelector() {
		return false, false
	}

	if !protocolMatches(rule.Protocol, pkt.Protocol) {
		return false, true
	}

	portsMatch, ok := portListMatches(rule.Ports, pkt.DestPort)
	if !ok {
		return false, false
	}
	if !portsMatch {
		return false, true
	}

	sourceMatch, ok := sourceListMatches(rule.Sources, pkt.Source)
	if !ok {
		return false, false
	}
	return sourceMatch, true
}

// hasUnmodelledSelector reports whether the rule discriminates on something this
// evaluator does not understand. Rules created in the DSM UI can carry GeoIP
// country lists or built-in application presets; both are preserved verbatim on
// write-back, but neither can be replayed here.
func (r *FirewallRule) hasUnmodelledSelector() bool {
	return r.SourceKind == firewallSourceUnmodelled || r.PortKind == firewallPortUnmodelled
}

func protocolMatches(ruleProto, pktProto string) bool {
	rp := strings.ToLower(strings.TrimSpace(ruleProto))
	if rp == "" || rp == FirewallProtocolAll {
		return true
	}
	return rp == strings.ToLower(pktProto)
}

// portListMatches reports whether port is selected by the rule's port list. An
// empty list means "all ports" — that is how DSM represents a rule with no port
// restriction, and it is the dangerous case the lockout guard exists for.
func portListMatches(ports []string, port int) (bool, bool) {
	if len(ports) == 0 {
		return true, true
	}
	for _, spec := range ports {
		spec = strings.TrimSpace(spec)
		if spec == "" || strings.EqualFold(spec, "all") {
			return true, true
		}
		low, high, ok := parsePortSpec(spec)
		if !ok {
			return false, false
		}
		if port >= low && port <= high {
			return true, true
		}
	}
	return false, true
}

// parsePortSpec accepts a single port ("5001") or a range. DSM writes ranges
// with a colon in some places and a dash in others depending on where the rule
// was authored, so both are accepted.
func parsePortSpec(spec string) (int, int, bool) {
	sep := strings.IndexAny(spec, ":-")
	if sep < 0 {
		n, err := strconv.Atoi(spec)
		if err != nil || n < 0 || n > 65535 {
			return 0, 0, false
		}
		return n, n, true
	}

	low, err1 := strconv.Atoi(strings.TrimSpace(spec[:sep]))
	high, err2 := strconv.Atoi(strings.TrimSpace(spec[sep+1:]))
	if err1 != nil || err2 != nil || low > high || low < 0 || high > 65535 {
		return 0, 0, false
	}
	return low, high, true
}

// sourceListMatches reports whether ip is selected by the rule's source list. As
// with ports, an empty list means "any source".
func sourceListMatches(sources []string, ip net.IP) (bool, bool) {
	if len(sources) == 0 {
		return true, true
	}
	if ip == nil {
		return false, false
	}
	for _, spec := range sources {
		spec = strings.TrimSpace(spec)
		if spec == "" || strings.EqualFold(spec, "all") {
			return true, true
		}
		match, ok := sourceMatches(spec, ip)
		if !ok {
			return false, false
		}
		if match {
			return true, true
		}
	}
	return false, true
}

// sourceMatches understands the three source forms this provider writes: a
// single address, a CIDR, and a dashed range. Anything else is unevaluatable.
func sourceMatches(spec string, ip net.IP) (bool, bool) {
	if strings.Contains(spec, "/") {
		_, network, err := net.ParseCIDR(spec)
		if err != nil {
			return false, false
		}
		return network.Contains(ip), true
	}

	if idx := strings.Index(spec, "-"); idx > 0 {
		low := net.ParseIP(strings.TrimSpace(spec[:idx]))
		high := net.ParseIP(strings.TrimSpace(spec[idx+1:]))
		if low == nil || high == nil {
			return false, false
		}
		return ipBetween(low, high, ip), true
	}

	single := net.ParseIP(spec)
	if single == nil {
		return false, false
	}
	return single.Equal(ip), true
}

// ipBetween compares addresses of the same family byte-wise. Mixed families
// never match, which is the correct answer rather than an error: an IPv4 session
// is simply not inside an IPv6 range.
func ipBetween(low, high, ip net.IP) bool {
	l4, h4, i4 := low.To4(), high.To4(), ip.To4()
	if l4 != nil && h4 != nil && i4 != nil {
		return bytesBetween(l4, h4, i4)
	}
	l16, h16, i16 := low.To16(), high.To16(), ip.To16()
	if l16 == nil || h16 == nil || i16 == nil {
		return false
	}
	if low.To4() != nil || high.To4() != nil || ip.To4() != nil {
		return false
	}
	return bytesBetween(l16, h16, i16)
}

func bytesBetween(low, high, val []byte) bool {
	return compareBytes(val, low) >= 0 && compareBytes(val, high) <= 0
}

func compareBytes(a, b []byte) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}
