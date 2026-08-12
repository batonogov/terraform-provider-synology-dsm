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

  # dsm_scheduled_task and dsm_event_task run shell commands on the NAS as root.
  # They are refused at plan time unless this is enabled, so that write access to
  # this configuration does not silently mean root on the NAS.
  # allow_task_execution = true
}

variable "dsm_password" {
  description = "DSM administrator password"
  type        = string
  sensitive   = true
}
