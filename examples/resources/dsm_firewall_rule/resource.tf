# Rules are matched top to bottom and the first match wins, so `priority` is the
# policy. Keep the allow rules above the catch-all deny, and keep the rule that
# lets Terraform reach DSM above everything.

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

resource "dsm_firewall_rule" "s3_from_vpn" {
  profile = "default"
  adapter = "eth0"

  name     = "S3 and HTTPS from the VPN only"
  priority = 1
  action   = "allow"
  protocol = "tcp"
  ports    = ["8333", "8443"]
  source   = ["10.210.0.0/16"]
}

# The catch-all goes last. The provider replays the resulting rule set against
# its own DSM session before writing, and refuses the change if that session
# would stop being reachable — so this rule is only accepted while the allow
# rule above still covers the address Terraform connects from.
resource "dsm_firewall_rule" "deny_everything_else" {
  profile = "default"
  adapter = "eth0"

  name     = "Deny everything else"
  priority = 2
  action   = "deny"
  protocol = "all"
}
