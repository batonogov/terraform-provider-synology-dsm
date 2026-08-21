# The rules come first. A firewall switched on before the rule that lets you
# reach DSM exists is a locked NAS, and the provider will refuse it — so write
# the allow rules, apply, then flip the switch.

resource "dsm_firewall_rule" "dsm_admin_from_vpn" {
  profile = "default"
  adapter = "eth0"

  name     = "DSM admin from the VPN"
  priority = 0
  action   = "allow"
  protocol = "tcp"
  ports    = ["5000", "5001"]
  source   = ["10.210.0.0/16"]
}

# The profile-level switch: whether the firewall runs at all, which profile is
# in force, and what happens to traffic no rule matched.
#
# `default_policy` is a map because DSM stores it per network adapter
# (`adapterPolicyMap`), not once per profile — the same way the DSM firewall page
# asks the "Allow access / Deny access" question separately for each interface.
# Adapters left out of the map keep whatever DSM has; `default_policy_effective`
# reports all of them.
resource "dsm_firewall" "this" {
  profile = "default"
  enabled = true

  default_policy = {
    # `deny` here is what turns the rules above into a whitelist. Without it the
    # meaning of the whole rule set depends on a dropdown somebody last touched
    # in the UI.
    eth0 = "deny"

    # `global` is DSM's pre-table rather than a real interface, so it has no
    # fall-through of its own.
    global = "none"
  }

  depends_on = [dsm_firewall_rule.dsm_admin_from_vpn]
}

# What DSM actually has, including adapters not managed above.
output "firewall_default_policy" {
  value = dsm_firewall.this.default_policy_effective
}
