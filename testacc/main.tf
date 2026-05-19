terraform {
  required_providers {
    dsm = {
      source  = "registry.terraform.io/batonogov/dsm"
      version = "0.1.0"
    }
  }
}

provider "dsm" {
  host     = var.dsm_host
  username = var.dsm_username
  password = var.dsm_password
  insecure = true
}

resource "dsm_user" "test_user" {
  name        = "tf-test-user"
  password    = "TestPass123!"
  description = "Created by Terraform provider test"
}

variable "dsm_host" {
  type = string
}

variable "dsm_username" {
  type = string
}

variable "dsm_password" {
  type      = string
  sensitive = true
}
