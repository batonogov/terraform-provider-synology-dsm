resource "dsm_user_quota" "john_home" {
  share_name = "homes"
  username   = "john.doe"
  quota_size = 21474836480 # 20 GiB in bytes
}
