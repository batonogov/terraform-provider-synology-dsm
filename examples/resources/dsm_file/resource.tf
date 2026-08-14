resource "dsm_shared_folder" "containers" {
  name     = "containers"
  vol_path = "/volume1"
}

# Configuration files live on disk next to the Compose project, so credentials
# stay out of compose_yaml. Missing directories (here: seaweedfs/conf) are
# created automatically; the shared folder itself must already exist.
resource "dsm_file" "s3_identities" {
  share_path = "/${dsm_shared_folder.containers.name}/seaweedfs/conf"
  name       = "s3.json"

  content = jsonencode({
    identities = [{
      name = "nextcloud"
      credentials = [{
        accessKey = var.s3_access_key
        secretKey = var.s3_secret_key
      }]
      actions = ["Read", "Write", "List"]
    }]
  })
}

resource "dsm_file" "caddyfile" {
  share_path = "/${dsm_shared_folder.containers.name}/caddy/conf"
  name       = "Caddyfile"

  # No $ escaping needed: the content never passes through Compose interpolation.
  content = <<-CADDY
    cloud.example.com {
      reverse_proxy nextcloud:80
    }
  CADDY
}

# Binary files go through content_base64.
resource "dsm_file" "keystore" {
  share_path     = "/${dsm_shared_folder.containers.name}/caddy/conf"
  name           = "keystore.p12"
  content_base64 = filebase64("${path.module}/keystore.p12")
}

# Credentials that must never appear in Terraform state go through content_wo
# instead (a write-only argument, Terraform 1.11 and later). The value is sent
# to DSM during apply and stored nowhere else; state keeps only the checksum.
#
# Terraform cannot diff a value it does not store, so editing the content alone
# produces no plan — increment content_wo_version to have it written again. An
# edit made on the NAS is still noticed: the refreshed checksum no longer
# matches the last write, and the next apply restores the configured content.
resource "dsm_file" "database_password" {
  share_path = "/${dsm_shared_folder.containers.name}/nextcloud/conf"
  name       = "db-password"

  content_wo         = var.database_password
  content_wo_version = 1
}
