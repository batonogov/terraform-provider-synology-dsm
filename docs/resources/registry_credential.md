# dsm_registry_credential

Manages a Container Manager registry entry: a registry URL plus the credentials the Docker daemon on the NAS uses to pull private images. Without such an entry, `dsm_container_project` referencing a private image fails with `unauthorized ... action: pull`.

## Example Usage

```terraform
resource "dsm_registry_credential" "harbor" {
  url                     = "https://registry.example.com"
  name                    = "registry.example.com"
  username                = "robot$terraform"
  password_wo             = var.registry_password
  password_wo_version     = 1
  enable_trust_self_signed = false
}
```

## Argument Reference

- `url` — (Required) Registry endpoint, for example `https://registry.example.com`. Identifies the entry; changing it replaces the resource.
- `name` — (Required) Display name in Container Manager.
- `username` — (Required) Registry login.
- `password_wo` — (Required, write-only, sensitive) Registry password. Never written to state or plan (Terraform 1.11+); requires `password_wo_version`.
- `password_wo_version` — (Required) Version counter: increment to rotate the password. Rotation replaces the entry, which matches how Container Manager's own UI applies an edit.
- `enable_trust_self_signed` — (Optional) Trust the registry's self-signed certificate. Default `false`.

## Attribute Reference

- `id` — Registry URL.

## Import

Import by registry URL:

```
terraform import dsm_registry_credential.harbor https://registry.example.com
```

The imported entry holds no password (DSM never returns one): adopt it by adding `password_wo`/`password_wo_version` to the configuration and bumping the version once.
