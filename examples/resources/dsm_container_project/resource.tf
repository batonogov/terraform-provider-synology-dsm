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

# compose_yaml_wo is a write-only argument (Terraform 1.11 and later): the
# document reaches Container Manager during apply and is never written to state
# or to the plan file. State keeps compose_yaml_checksum, which is what makes an
# edit made in Container Manager visible on the next plan. Increment
# compose_yaml_wo_version to send an edited document — without the stored
# document there is nothing for Terraform to diff against.
#
# It keeps the document out of Terraform state, not off the NAS: Container
# Manager writes the resolved document into the project directory. So the
# password still goes in a dsm_file and reaches the container through env_file,
# and the compose document only names it.
resource "dsm_file" "database_env" {
  share_path = "/containers/database/conf"
  name       = "db.env"

  content_wo         = "POSTGRES_PASSWORD=${var.database_password}"
  content_wo_version = 1
}

resource "dsm_container_project" "database" {
  name       = "database"
  share_path = "/containers/database"

  # Bind mounts must point at a path that already exists — Container Manager
  # does not create host directories, and a relative mount fails the build.
  compose_yaml_wo = <<-YAML
    services:
      db:
        image: postgres:17
        env_file:
          - /volume1/containers/database/conf/db.env
        volumes:
          - database-data:/var/lib/postgresql/data
        restart: unless-stopped
    volumes:
      database-data:
  YAML

  compose_yaml_wo_version = 1
  depends_on              = [dsm_file.database_env]
}
