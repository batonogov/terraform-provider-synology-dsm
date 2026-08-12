# The import ID is profile:adapter:rule_name, where rule_name is the rule's
# Description as shown in Control Panel > Security > Firewall.
terraform import dsm_firewall_rule.s3_from_vpn "default:eth0:S3 and HTTPS from the VPN only"
