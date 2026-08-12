resource "dsm_user_home_service" "homes" {
  location           = "/volume1"
  enable_recycle_bin = true

  # Keep this NAS-wide service enabled if the Terraform resource is removed.
  disable_on_destroy = false
}
