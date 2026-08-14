# The missing piece of the TLS pipeline: DSM issues the certificate over ACME,
# the reverse proxy listens, and this binding selects which certificate the
# listener serves — the step that otherwise has to be done by hand in
# Control Panel > Security > Certificate > Settings.
resource "dsm_certificate_service" "caddy" {
  service        = "synology.at.caddy" # as listed by the dsm_certificates data source
  certificate_id = dsm_certificate_lets_encrypt.cloud.id
}
