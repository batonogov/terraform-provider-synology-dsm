# DSM date and time settings are NAS-wide, so declare this resource once per
# host. Every attribute is optional: whatever you leave out keeps the value DSM
# already has.
resource "dsm_system_settings" "this" {
  # Synology's own zone name, not an IANA identifier:
  # "Moscow", not "Europe/Moscow".
  timezone = "Moscow"

  ntp_enabled = true
  ntp_server  = "time.google.com"
}
