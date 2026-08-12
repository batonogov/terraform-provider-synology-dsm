# WARNING: dsm_event_task runs a shell command on the NAS, here as root, every
# time the NAS boots. Write access to this file is equivalent to root shell
# access on the NAS. The provider refuses these resources unless
# allow_task_execution is enabled.
provider "dsm" {
  allow_task_execution = true
}

resource "dsm_event_task" "on_boot" {
  name    = "restore-mounts"
  user    = "root"
  event   = "bootup"
  command = "/volume1/scripts/restore-mounts.sh"
}

resource "dsm_event_task" "on_shutdown" {
  name    = "drain-queue"
  user    = "operator"
  event   = "shutdown"
  command = "/volume1/scripts/drain-queue.sh"

  # DSM waits for these tasks before starting this one.
  depends_on_tasks = [dsm_event_task.on_boot.name]

  notify_email      = "ops@example.com"
  notify_on_failure = true
}
