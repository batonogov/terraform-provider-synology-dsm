# The import ID is the certificate id DSM assigned, which is a short opaque
# string rather than the description. Find it with the data source:
#
#   data "dsm_certificates" "all" {}
#   output "ids" { value = { for c in data.dsm_certificates.all.certificates : c.description => c.id } }
terraform import dsm_certificate.wildcard K3xR9a

# DSM never returns a private key, so an imported certificate has no key
# material in state. The first apply after the import re-uploads whatever
# `certificate` and `private_key` say, in place and under the same id — the
# service assignments survive it.
