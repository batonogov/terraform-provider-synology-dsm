resource "dsm_package" "container_manager" {
  name    = "ContainerManager"
  volume  = "/volume1"
  running = true

  # Keep the package installed if the Terraform resource is removed.
  uninstall_on_destroy = false
}
