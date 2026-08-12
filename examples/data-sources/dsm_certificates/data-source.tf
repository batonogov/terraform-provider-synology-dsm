# Every certificate on the NAS, including the ones Terraform does not manage.
data "dsm_certificates" "all" {}

# Or narrow it to one description.
data "dsm_certificates" "wildcard" {
  description = "wildcard.example.com"
}

# The description-to-id map is what `terraform import` needs: DSM identifies a
# certificate by a short opaque id, not by its description.
output "certificate_ids" {
  value = { for certificate in data.dsm_certificates.all.certificates : certificate.description => certificate.id }
}

# Feed this into whatever raises the alert. It covers the self-signed
# certificate DSM ships with and anything installed by hand, which is exactly
# the set most likely to expire unnoticed.
output "expiring_within_30_days" {
  value = [
    for certificate in data.dsm_certificates.all.certificates :
    {
      description = certificate.description
      subject     = certificate.subject
      expires_at  = certificate.expires_at
      services    = certificate.services
    }
    if certificate.expires_at != null && timecmp(certificate.expires_at, timeadd(timestamp(), "720h")) < 0
  ]
}
