data "dsm_system_settings" "current" {}

output "nas_timezone" {
  value = data.dsm_system_settings.current.timezone
}

output "nas_clock" {
  value = "${data.dsm_system_settings.current.current_date} ${data.dsm_system_settings.current.current_time}"
}
