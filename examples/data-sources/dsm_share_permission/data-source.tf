data "dsm_share_permission" "developers" {
  share_name      = "team-data"
  user_group_type = "local_group"
  principal_name  = "developers"
}
