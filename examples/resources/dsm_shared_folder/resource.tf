resource "dsm_shared_folder" "team_data" {
  name                   = "team-data"
  vol_path               = "/volume1"
  description            = "Team datasets and model artifacts"
  enable_recycle_bin     = true
  recycle_bin_admin_only = true
  share_quota            = 500
}

# Shared folders outlive Terraform: one may predate the configuration, or have
# been created by DSM itself — Container Manager creates `docker` when it is
# installed. Set adopt_existing to take such a folder over instead of failing
# with DSM error 3301.
#
# Adoption applies the settings below to the existing folder and puts it under
# full management, so `terraform destroy` will later delete it and its contents.
# Leave adopt_existing at its default and use `terraform import` instead if you
# would rather keep creation strict.
resource "dsm_shared_folder" "docker" {
  name           = "docker"
  vol_path       = "/volume1"
  description    = "Container Manager projects"
  adopt_existing = true
}
