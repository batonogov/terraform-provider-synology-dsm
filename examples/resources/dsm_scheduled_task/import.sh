# Import by the numeric task id shown in DSM Task Scheduler.
terraform import dsm_scheduled_task.prune_images 42

# The task name also works, as long as it is unique on the NAS.
terraform import dsm_scheduled_task.prune_images docker-prune
