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

resource "dsm_user" "example" {
  name        = "john.doe"
  password    = var.user_password
  description = "John Doe - Engineering"
  email       = "john.doe@example.com"
  groups      = ["users"]
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
