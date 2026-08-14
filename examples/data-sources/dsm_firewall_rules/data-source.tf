data "dsm_firewall_rules" "current" {
  profile = "default"
}

# The whole picture for a security audit: every allow, across every adapter,
# in evaluation order — including rules nobody remembers creating.
output "open_doors" {
  value = [
    for r in data.dsm_firewall_rules.current.rules : {
      adapter = r.adapter
      name    = r.name
      ports   = r.ports
      source  = r.source
    }
    if r.enabled && r.action == "allow"
  ]
}

# A rule only enforces anything when the firewall is on and its profile is the
# active one, so both are exposed for assertions.
output "profile_is_enforced" {
  value = data.dsm_firewall_rules.current.firewall_enabled && data.dsm_firewall_rules.current.profile_active
}
