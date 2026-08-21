# The import ID is the firewall profile name, as shown in
# Control Panel > Security > Firewall > Firewall Profile.
terraform import dsm_firewall.this "default"

# `default_policy` is left null by an import: DSM cannot say which adapters a
# configuration meant to manage. Read `default_policy_effective` after the
# import and copy the adapters you want to manage into `default_policy`.
