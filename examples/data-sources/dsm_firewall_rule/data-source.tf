data "dsm_firewall_rule" "s3_from_vpn" {
  profile = "default"
  adapter = "eth0"
  name    = "S3 and HTTPS from the VPN only"
}

# A rule only enforces anything when the firewall is on and its profile is the
# active one, so both are exposed for assertions.
output "s3_rule_is_enforced" {
  value = data.dsm_firewall_rule.s3_from_vpn.firewall_enabled && data.dsm_firewall_rule.s3_from_vpn.profile_active
}
