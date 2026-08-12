resource "dsm_shared_folder" "team_data" {
  name                   = "team-data"
  vol_path               = "/volume1"
  description            = "Team datasets and model artifacts"
  enable_recycle_bin     = true
  recycle_bin_admin_only = true
  share_quota            = 500
}
