# Useful for auditing what a NAS already runs unattended on every restart.
data "dsm_event_task" "on_boot" {
  name = "restore-mounts"
}

output "boot_task_runs_as_root" {
  description = "DSM records an event task's owner by uid; 0 is root."
  value       = data.dsm_event_task.on_boot.owner_uid == 0
}
