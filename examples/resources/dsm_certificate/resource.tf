# Bring your own certificate: from a file on disk, an internal CA, Vault, ACM —
# anywhere Terraform can already reach.
#
# The private key ends up in Terraform state in clear text. Terraform redacts it
# from plan output, but state is not encrypted: use an encrypted remote backend
# and restrict who can read it.
resource "dsm_certificate" "wildcard" {
  description = "wildcard.example.com"

  certificate  = file("${path.module}/cert.pem")
  private_key  = file("${path.module}/key.pem")
  intermediate = file("${path.module}/chain.pem")

  set_as_default = true
}

# Rotation is an in-place update, not a replacement: the DSM certificate id
# stays the same, so every service already pointed at it keeps working. Feeding
# the material in from a secret store makes that a one-line change.
resource "dsm_certificate" "internal" {
  description = "nas.internal"

  certificate  = var.internal_certificate
  private_key  = var.internal_private_key
  intermediate = var.internal_chain
}

# expires_at is parsed out of the certificate itself, so it is exactly what
# clients will see — alert on it rather than on a renewal job's exit code.
output "wildcard_expires_at" {
  value = dsm_certificate.wildcard.expires_at
}

# Services DSM is currently serving with this certificate. While this list is
# non-empty, destroy refuses unless force_destroy is set.
output "wildcard_services" {
  value = dsm_certificate.wildcard.services
}
