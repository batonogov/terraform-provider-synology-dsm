data "dsm_reverse_proxy" "nextcloud" {
  description = "Nextcloud"
}

output "nextcloud_upstream" {
  value = "${lower(data.dsm_reverse_proxy.nextcloud.destination_protocol)}://${data.dsm_reverse_proxy.nextcloud.destination_hostname}:${data.dsm_reverse_proxy.nextcloud.destination_port}"
}
