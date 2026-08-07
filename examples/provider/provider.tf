terraform {
  required_providers {
    dsm = {
      source  = "batonogov/dsm"
      version = "0.1.0"
    }
  }
}

provider "dsm" {
  host     = "https://diskstation:5001"
  username = "admin"
  password = var.dsm_password
  insecure = true
}

# Enable per-user home folders (/volume1/homes/<username>). This is a single
# NAS-wide setting, so declare it once. Enabling it creates the `homes` share.
#
# Destroy leaves the service running unless disable_on_destroy = true: turning it
# off would take personal folders away from every user and break Synology Drive.
resource "dsm_user_home_service" "homes" {
  location           = "/volume1"
  enable_recycle_bin = true
}

resource "dsm_user" "example" {
  name        = "john.doe"
  password    = var.user_password
  description = "John Doe - Engineering"
  email       = "john.doe@example.com"
  groups      = ["users"]
}

# A contractor account that stops working after a fixed date. expire_date and
# disabled are mutually exclusive: DSM keeps both in a single field.
resource "dsm_user" "contractor" {
  name        = "jane.contractor"
  password    = var.user_password
  description = "Contractor - access until the end of the project"
  expire_date = "2027-03-05"
}

# A personal folder exists for every user once the home service is on, so quotas
# can be placed on the `homes` share.
resource "dsm_user_quota" "example_home" {
  share_name = "homes"
  username   = dsm_user.example.name
  quota_size = 21474836480 # 20 GB

  depends_on = [dsm_user_home_service.homes]
}

# A team folder with compression and a 500 GB cap.
#
# Compression requires enable_share_cow, and both are creation-time only in DSM:
# switching either from false to true forces replacement, which destroys the
# folder and its contents. share_quota is in GIGABYTES.
resource "dsm_shared_folder" "archive" {
  name                   = "archive"
  vol_path               = "/volume1"
  description            = "Compressed team archive"
  enable_recycle_bin     = true
  recycle_bin_admin_only = true
  enable_share_compress  = true
  enable_share_cow       = true
  share_quota            = 500
}

variable "dsm_password" {
  description = "DSM administrator password"
  type        = string
  sensitive   = true
}

variable "user_password" {
  description = "Password for the new user"
  type        = string
  sensitive   = true
}
