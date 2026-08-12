# Let DSM obtain the certificate. Nothing secret reaches Terraform: the key is
# generated on the NAS and never leaves it, and DSM renews on its own schedule.
#
# Every name below is validated by Let's Encrypt from the public internet, so
# each one must resolve to this NAS and inbound TCP/80 must reach it. DSM runs
# the whole exchange inside one request, so this apply blocks for tens of
# seconds and occasionally minutes.
resource "dsm_certificate_lets_encrypt" "cloud" {
  description = "cloud.example.com"
  domain      = "cloud.example.com"
  alt_names   = ["s3.example.com"]
  email       = "admin@example.com"

  set_as_default = true
}

# Issuance is rate limited by Let's Encrypt (a handful of certificates per
# registered domain per week), so a failed apply is expensive. Iterate with
# -target on this one resource until DNS and the firewall are right.

# expires_at moves forward every time DSM renews. An alert on "expires_at is
# less than 30 days away" is therefore an alert on renewal having stopped
# working, which is the failure worth waking up for.
output "cloud_expires_at" {
  value = dsm_certificate_lets_encrypt.cloud.expires_at
}
