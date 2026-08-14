# The transport for every notification DSM sends: task failures, storage
# degradation, security advisories. Without this, notify_email on a scheduled
# task points at an outgoing mail server nobody configured.
resource "dsm_notification_mail" "this" {
  smtp_server = "smtp.example.com"
  smtp_port   = 587
  sender      = "nas@example.com"

  use_tls       = true
  smtp_auth     = true
  smtp_username = "nas@example.com"
  smtp_password = var.smtp_password
}
