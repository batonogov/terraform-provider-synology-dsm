# WARNING: dsm_scheduled_task runs a shell command on the NAS, here as root.
# Write access to this file is equivalent to root shell access on the NAS.
# The provider refuses these resources unless allow_task_execution is enabled.
provider "dsm" {
  allow_task_execution = true
}

# Weekly cleanup of unused container images.
resource "dsm_scheduled_task" "prune_images" {
  name    = "docker-prune"
  user    = "root"
  command = "/usr/local/bin/docker system prune -af --filter until=168h"

  schedule {
    frequency   = "weekly"
    day_of_week = ["sunday"]
    hour        = 4
    minute      = 30
  }

  enabled           = true
  notify_on_failure = true
  notify_email      = "ops@example.com"
}

# A health check that runs every 15 minutes during the working day, as an
# ordinary account rather than root.
resource "dsm_scheduled_task" "health_check" {
  name    = "health-check"
  user    = "operator"
  command = "/volume1/scripts/health-check.sh"

  schedule {
    frequency               = "daily"
    hour                    = 8
    minute                  = 0
    repeat_interval_minutes = 15
    repeat_until_hour       = 20
  }
}

# The first Monday of every month.
resource "dsm_scheduled_task" "monthly_report" {
  name    = "monthly-report"
  user    = "operator"
  command = "/volume1/scripts/report.sh"

  schedule {
    frequency     = "monthly"
    day_of_week   = ["monday"]
    week_of_month = ["first"]
    hour          = 6
  }
}
