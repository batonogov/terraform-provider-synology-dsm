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

# A compose document that carries credentials goes through compose_yaml_wo (a
# write-only argument, Terraform 1.11 and later): it reaches Container Manager
# during apply and is never written to state or to the plan file. State keeps
# compose_yaml_checksum, which is what makes an edit made in Container Manager
# visible on the next plan.
#
# Increment compose_yaml_wo_version to send an edited document to DSM — without
# the stored document there is nothing for Terraform to diff against.
resource "dsm_container_project" "database" {
  name       = "database"
  share_path = "/containers/database"

  compose_yaml_wo = <<-YAML
    services:
      db:
        image: postgres:17
        environment:
          POSTGRES_PASSWORD: ${var.database_password}
        volumes:
          - ./data:/var/lib/postgresql/data
        restart: unless-stopped
  YAML

  compose_yaml_wo_version = 1
}
