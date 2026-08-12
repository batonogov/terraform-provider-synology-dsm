# The import ID is the certificate id DSM assigned. List them with the
# dsm_certificates data source to find it.
terraform import dsm_certificate_lets_encrypt.cloud LeAbc1

# Importing rather than creating is the safe way to adopt a certificate DSM
# already obtained: creating it again would spend a Let's Encrypt rate-limit
# slot for a certificate that already exists.
#
# `domain` and `alt_names` are restored from the certificate itself, so the
# first plan after an import is clean. `email` is the exception — DSM does not
# report the ACME contact address, so put it in the configuration and the first
# apply records it (it does not force a reissue).
