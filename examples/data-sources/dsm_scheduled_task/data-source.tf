# Reading a task executes nothing, so this works without allow_task_execution.
data "dsm_scheduled_task" "prune_images" {
  name = "docker-prune"
}

output "prune_command" {
  description = "What the NAS is currently configured to run, and as whom."
  value       = "${data.dsm_scheduled_task.prune_images.user}: ${data.dsm_scheduled_task.prune_images.command}"
}
