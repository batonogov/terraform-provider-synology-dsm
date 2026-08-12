resource "dsm_container_project" "object_storage" {
  name         = "s3-storage"
  share_path   = "/containers/s3-storage"
  compose_yaml = <<-YAML
    services:
      object-storage:
        image: minio/minio:latest
        command: server /data --console-address ":9001"
        volumes:
          - ./data:/data
        restart: unless-stopped
  YAML

  running = true

  # Keep the project and workloads if the Terraform resource is removed.
  delete_on_destroy = false
}
