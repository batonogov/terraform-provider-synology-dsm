terraform {
  required_providers {
    dsm = {
      source  = "batonogov/synology-dsm"
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

variable "dsm_password" {
  description = "DSM administrator password"
  type        = string
  sensitive   = true
}
