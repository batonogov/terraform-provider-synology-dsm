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

# A personal folder exists for every user once the home service is on, so quotas
# can be placed on the `homes` share.
resource "dsm_user_quota" "example_home" {
  share_name = "homes"
  username   = dsm_user.example.name
  quota_size = 21474836480 # 20 GB

  depends_on = [dsm_user_home_service.homes]
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
