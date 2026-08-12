# terraform-provider-synology-dsm

A Terraform provider for managing [Synology DSM](https://www.synology.com/en-global/dsm) as Infrastructure as Code — provision packages, Container Manager projects, files in shared folders, users, groups, shared folders, permissions, quotas, and per-user home folders.

Built with the Terraform Plugin Framework and the Synology DSM web API (`SYNO.API.Auth` v7 with SynoToken). Developed and tested against DSM 7.2.2 and DSM 7.3.2.

## Features

| Resource | Description |
|----------|-------------|
| [`dsm_user`](#dsm_user) | Manage local user accounts |
| [`dsm_group`](#dsm_group) | Manage groups |
| [`dsm_shared_folder`](#dsm_shared_folder) | Manage shared folders |
| [`dsm_share_permission`](#dsm_share_permission) | Manage share-level access (R/W/deny) for users and groups |
| [`dsm_user_quota`](#dsm_user_quota) | Manage per-user quotas on a shared folder |
| [`dsm_user_home_service`](#dsm_user_home_service) | Enable per-user home folders (the `homes` shared folder) |
| [`dsm_package`](#dsm_package) | Install and control Package Center packages |
| [`dsm_container_project`](#dsm_container_project) | Manage Docker Compose projects in Container Manager |
| [`dsm_file`](#dsm_file) | Upload configuration files into a shared folder |
| [`dsm_system_settings`](#dsm_system_settings) | Manage the NAS time zone and NTP synchronisation |

Every resource except `dsm_file` has a matching data source (`dsm_user`, `dsm_group`, `dsm_shared_folder`, `dsm_share_permission`, `dsm_user_quota`, `dsm_user_home_service`, `dsm_package`, `dsm_container_project`, `dsm_system_settings`) for reading existing objects.
| [`dsm_reverse_proxy`](#dsm_reverse_proxy) | Publish a service through the DSM Login Portal reverse proxy |

Each resource has a matching data source (`dsm_user`, `dsm_group`, `dsm_shared_folder`, `dsm_share_permission`, `dsm_user_quota`, `dsm_user_home_service`, `dsm_package`, `dsm_container_project`, `dsm_reverse_proxy`) for reading existing objects.

Full generated reference documentation is available in [`docs/`](docs/index.md).

## Requirements

- Terraform >= 1.0
- Synology DSM 7.2+ (tested on 7.2.2 and 7.3.2; behavior on DSM 6.x may differ)
- Go >= 1.26 (for development)

## Installation

### From the Terraform Registry

```hcl
terraform {
  required_providers {
    dsm = {
      source  = "batonogov/synology-dsm"
      version = "0.1.0"
    }
  }
}
```

### Local development

```bash
git clone https://github.com/batonogov/terraform-provider-synology-dsm.git
cd terraform-provider-synology-dsm
task install   # builds and installs into ~/.terraform.d/plugins/
```

## Usage

```hcl
terraform {
  required_providers {
    dsm = {
      source  = "batonogov/synology-dsm"
      version = "0.1.0"
    }
  }
}

variable "dsm_password" {}
variable "user_password" {}

provider "dsm" {
  host     = "https://diskstation:5001"
  username = "admin"
  password = var.dsm_password
  insecure = true # skip TLS verification for self-signed certs
}

resource "dsm_package" "container_manager" {
  name   = "ContainerManager"
  volume = "/volume1"
}

resource "dsm_shared_folder" "containers" {
  name     = "containers"
  vol_path = "/volume1"
}

resource "dsm_container_project" "object_storage" {
  name       = "s3-storage"
  share_path = "/${dsm_shared_folder.containers.name}/s3-storage"
  compose_yaml = <<-YAML
    services:
      object-storage:
        image: minio/minio:latest
        command: server /data --console-address ":9001"
        ports:
          - "9000:9000"
          - "9001:9001"
        volumes:
          - ./data:/data
        restart: unless-stopped
  YAML

  depends_on = [dsm_package.container_manager]
}

resource "dsm_group" "developers" {
  name        = "developers"
  description = "Development team"
}

resource "dsm_user" "john" {
  name        = "john.doe"
  password    = var.user_password
  description = "John Doe - Engineering"
  email       = "john.doe@example.com"
  groups      = [dsm_group.developers.name]
}

resource "dsm_shared_folder" "team_data" {
  name               = "team-data"
  vol_path           = "/volume1"
  description        = "Team shared data"
  enable_recycle_bin = true
}

resource "dsm_share_permission" "developers_rw" {
  share_name      = dsm_shared_folder.team_data.name
  user_group_type = "local_group"
  principal_name  = dsm_group.developers.name
  permission      = "read_write"
}

resource "dsm_share_permission" "john_rw" {
  share_name      = dsm_shared_folder.team_data.name
  user_group_type = "local_user"
  principal_name  = dsm_user.john.name
  permission      = "read_write"
}

resource "dsm_user_quota" "john_quota" {
  share_name = dsm_shared_folder.team_data.name
  username   = dsm_user.john.name
  quota_size = 10737418240 # 10 GB
}
```

## Provider configuration

| Attribute   | Type   | Required | Description                                            |
|-------------|--------|----------|--------------------------------------------------------|
| `host`      | string | yes      | DSM URL (e.g. `https://diskstation:5001`)              |
| `username`  | string | yes      | DSM administrator username                             |
| `password`  | string | yes      | DSM password (sensitive)                               |
| `insecure`  | bool   | no       | Skip TLS certificate verification (self-signed certs)  |

All attributes can be supplied via environment variables: `SYNOLOGY_DSM_HOST`, `SYNOLOGY_DSM_USERNAME`, `SYNOLOGY_DSM_PASSWORD`. `SYNOLOGY_DSM_PASSWORD` may be empty to support a DSM in first-login state.

## Resources

### `dsm_user`

Manages a local user account.

| Attribute     | Type         | Required | Computed | Description                                  |
|---------------|--------------|----------|----------|----------------------------------------------|
| `id`                 | string       | -        | yes      | Identifier (username)                        |
| `name`               | string       | yes      | -        | Username. Forces replacement if changed.     |
| `password`           | string       | no*      | -        | Password (sensitive). *Required when creating; may be omitted for an imported user. |
| `description`        | string       | no       | -        | Description                                  |
| `email`              | string       | no       | -        | Email address                                |
| `disabled`           | bool         | no       | yes      | Account disabled (default: `false`). Mutually exclusive with `expire_date`. |
| `expire_date`        | string       | no       | -        | Account expiry as `YYYY-MM-DD`. Omit for an account that never expires. |
| `groups`             | list(string) | no       | -        | Group memberships                            |
| `two_factor_enabled` | bool         | -        | yes      | Whether 2FA is on (read-only)                |
| `uid`                | int          | -        | yes      | UID assigned by DSM (read-only)              |

```hcl
resource "dsm_user" "contractor" {
  name        = "jane.contractor"
  password    = var.contractor_password
  description = "Contractor — access until the end of the project"
  expire_date = "2027-03-05"
}
```

```bash
terraform import dsm_user.john john.doe
```

> **`disabled` and `expire_date` are mutually exclusive.** DSM keeps the account
> state in a single field, so an account cannot be both switched off and carry
> an expiry date. The provider rejects that combination at plan time.

> **2FA is read-only here.** DSM manages two-factor authentication through a
> separate API (`SYNO.Core.OTP`), not through user attributes.

### `dsm_group`

Manages a group.

| Attribute     | Type   | Required | Computed | Description                              |
|---------------|--------|----------|----------|------------------------------------------|
| `id`          | string | -        | yes      | Identifier (group name)                  |
| `name`        | string | yes      | -        | Group name. Forces replacement if changed. |
| `description` | string | no       | -        | Description                              |
| `gid`         | int    | -        | yes      | GID assigned by DSM (read-only)          |

```bash
terraform import dsm_group.developers developers
```

### `dsm_shared_folder`

Manages a shared folder.

| Attribute                | Type   | Required | Computed | Description                                          |
|--------------------------|--------|----------|----------|------------------------------------------------------|
| `id`                     | string | -        | yes      | Identifier (name)                                    |
| `name`                   | string | yes      | -        | Shared folder name. Forces replacement if changed.   |
| `vol_path`               | string | yes      | -        | Volume path (e.g. `/volume1`). Forces replacement.   |
| `description`            | string | no       | -        | Description                                          |
| `hidden`                 | bool   | no       | yes      | Hide from network browsing (default: `false`)        |
| `enable_recycle_bin`     | bool   | no       | yes      | Enable recycle bin (default: `true`)                 |
| `recycle_bin_admin_only` | bool   | no       | yes      | Restrict recycle bin to administrators (default: `true`) |
| `enable_share_compress`  | bool   | no       | yes      | File compression, Btrfs (default: `false`). See caveats below. |
| `enable_share_cow`       | bool   | no       | yes      | Data checksum for advanced data integrity / copy-on-write, Btrfs (default: `false`) |
| `share_quota`            | int    | no       | yes      | Quota for the whole folder **in gigabytes**; `0` is unlimited (default: `0`) |
| `uuid`                   | string | -        | yes      | UUID assigned by DSM (read-only)                     |

```hcl
resource "dsm_shared_folder" "archive" {
  name                   = "archive"
  vol_path               = "/volume1"
  description            = "Compressed archive with a 500 GB cap"
  enable_recycle_bin     = true
  recycle_bin_admin_only = true
  enable_share_compress  = true
  enable_share_cow       = true
  share_quota            = 500
}
```

```bash
terraform import dsm_shared_folder.team_data team-data
```

> **Compression requires copy-on-write.** DSM refuses to create a compressed
> folder unless `enable_share_cow` is also `true`; the provider rejects that
> combination at plan time.

> **`enable_share_compress` and `enable_share_cow` are creation-time only.**
> DSM accepts them when the folder is created and silently ignores them
> afterwards — a `set` call reports success while the value stays `false`.
> Switching either from `false` to `true` therefore forces replacement, **which
> destroys the folder and everything in it**. Turning them off is applied in
> place. Plan output will show the replacement; check it before applying.

> **`share_quota` is in gigabytes**, not bytes — unlike `dsm_user_quota.quota_size`.
> That matches DSM's own API, which takes `share_quota` and reports it back as
> `quota_value`.

### `dsm_share_permission`

Manages share-level access for a user or group. DSM stores permissions as a
whole-list, so concurrent changes to permissions on the same share are serialized
by the provider to avoid lost updates.

| Attribute         | Type   | Required | Computed | Description                                              |
|-------------------|--------|----------|----------|----------------------------------------------------------|
| `id`              | string | -        | yes      | `share_name:user_group_type:principal_name`              |
| `share_name`      | string | yes      | -        | Shared folder name. Forces replacement if changed.       |
| `user_group_type` | string | yes      | -        | `local_user` or `local_group`. Forces replacement if changed. |
| `principal_name`  | string | yes      | -        | User or group name. Forces replacement if changed.       |
| `permission`      | string | yes      | -        | `read_only`, `read_write`, or `no_access`                |

```bash
terraform import dsm_share_permission.john_rw team-data:local_user:john.doe
```

### `dsm_user_quota`

Manages a per-user quota on a shared folder.

| Attribute    | Type | Required | Computed | Description                              |
|--------------|------|----------|----------|------------------------------------------|
| `id`         | string | -      | yes      | `share_name:username`                    |
| `share_name` | string | yes    | -        | Shared folder name. Forces replacement if changed. |
| `username`   | string | yes    | -        | Username. Forces replacement if changed. |
| `quota_size` | int    | yes    | -        | Quota in bytes. `0` means unlimited.     |
| `quota_used` | int    | -      | yes      | Current usage in bytes (read-only)       |

```bash
terraform import dsm_user_quota.john_quota team-data:john.doe
```

> **Note:** The user quota API (`SYNO.Core.Share.Quota`) returns error 102 (not
> supported) on the virtual DSM used for acceptance testing. It works on real
> hardware running DSM 7.2+/7.3+.

### `dsm_user_home_service`

Enables the DSM user home service, which gives every user a personal folder
under the `homes` shared folder (`/volume1/homes/<username>`). This underpins
personal storage and Synology Drive's `/home/drive` space.

The service is a single NAS-wide setting, so declare **at most one** instance of
this resource per DSM host. Enabling it creates the `homes` shared folder.

| Attribute            | Type   | Required | Computed | Description                                                        |
|----------------------|--------|----------|----------|--------------------------------------------------------------------|
| `id`                 | string | -        | yes      | Always `user_home_service`                                          |
| `location`           | string | yes      | -        | Volume **path** hosting `homes`, e.g. `/volume1`                    |
| `enable`             | bool   | -        | yes      | Whether the service is on. Defaults to `true`                       |
| `enable_recycle_bin` | bool   | -        | yes      | Recycle bin on the `homes` folder. Defaults to `false`              |
| `force`              | bool   | -        | yes      | Pass DSM's `force` flag to override soft warnings. Defaults to `false` |
| `disable_on_destroy` | bool   | -        | yes      | Whether `destroy` turns the service off. Defaults to `false`        |

```hcl
resource "dsm_user_home_service" "homes" {
  location           = "/volume1"
  enable_recycle_bin = true
}

# Per-user quotas can then be placed on the homes share.
resource "dsm_user_quota" "john_home" {
  share_name = "homes"
  username   = dsm_user.john.name
  quota_size = 21474836480 # 20 GB
  depends_on = [dsm_user_home_service.homes]
}
```

```bash
terraform import dsm_user_home_service.homes user_home_service
```

> **`location` must be a path.** DSM rejects a bare volume name such as
> `volume1` with error 3101; use `/volume1`.

> **Destroy is a no-op by default.** Turning the service off is a NAS-wide
> action that takes personal folders away from every user and breaks Synology
> Drive and Photos. Set `disable_on_destroy = true` to opt in. Files under
> `homes` are never deleted by this resource either way.

> **Requires the built-in `admin` account.** `SYNO.Core.User.Home` answers error
> 119 for other administrator accounts even when the session is valid.

### `dsm_package`

Installs a package from the repositories configured in DSM Package Center and
controls whether it is running. If the package is already installed, the
resource adopts it instead of attempting another installation.

| Attribute              | Type   | Required | Computed | Description                                                      |
|------------------------|--------|----------|----------|------------------------------------------------------------------|
| `id`                   | string | -        | yes      | Package Center identifier                                        |
| `name`                 | string | yes      | -        | Package identifier, e.g. `ContainerManager`. Forces replacement. |
| `volume`               | string | -        | yes      | Installation volume path. Defaults to `/volume1`.                |
| `running`              | bool   | -        | yes      | Desired running state. Defaults to `true`.                       |
| `uninstall_on_destroy` | bool   | -        | yes      | Uninstall on destroy. Defaults to `false`.                       |
| `display_name`         | string | -        | yes      | Human-readable package name                                      |
| `version`              | string | -        | yes      | Installed version                                                |
| `status`               | string | -        | yes      | Raw DSM lifecycle status                                         |
| `description`          | string | -        | yes      | Package description                                              |
| `maintainer`           | string | -        | yes      | Package maintainer                                               |
| `can_uninstall`        | bool   | -        | yes      | Whether DSM allows the package to be uninstalled                 |

```hcl
resource "dsm_package" "container_manager" {
  name   = "ContainerManager"
  volume = "/volume1"

  # The safe default: removing this block does not uninstall the package.
  uninstall_on_destroy = false
}
```

```bash
terraform import dsm_package.container_manager ContainerManager
```

> **Destroy is non-destructive by default.** With `uninstall_on_destroy = false`,
> Terraform only removes the resource from state. Set it to `true` only when
> package removal — including any package-specific configuration/data cleanup —
> is intended. DSM may refuse to uninstall system packages.

> **Package compatibility is model-specific.** A package must be visible in
> Package Center for the exact NAS model and DSM version. Virtual DSM exposes
> the package APIs and catalog but blocks package installation with error 103;
> install acceptance testing therefore requires physical hardware.

### `dsm_container_project`

Creates a Docker Compose project in Synology Container Manager, manages its
compose document, and controls whether it is running. On compose changes, the
provider stops a running project, updates it, rebuilds it, and restores the
requested state.

| Attribute           | Type         | Required | Computed | Description                                                      |
|---------------------|--------------|----------|----------|------------------------------------------------------------------|
| `id`                | string       | -        | yes      | Project UUID assigned by Container Manager                       |
| `name`              | string       | yes      | -        | Unique project name. Forces replacement.                         |
| `share_path`        | string       | yes      | -        | File Station directory, e.g. `/containers/s3-storage`. Forces replacement. |
| `compose_yaml`      | string       | yes      | -        | Docker Compose YAML (sensitive)                                  |
| `running`           | bool         | -        | yes      | Desired running state. Defaults to `true`.                       |
| `delete_on_destroy` | bool         | -        | yes      | Delete the DSM project on destroy. Defaults to `false`.          |
| `path`              | string       | -        | yes      | Absolute volume path reported by DSM                             |
| `status`            | string       | -        | yes      | Raw Container Manager lifecycle status                           |
| `container_ids`     | list(string) | -        | yes      | Containers associated with the project                           |

```hcl
resource "dsm_package" "container_manager" {
  name = "ContainerManager"
}

resource "dsm_shared_folder" "containers" {
  name     = "containers"
  vol_path = "/volume1"
}

resource "dsm_container_project" "s3_storage" {
  name       = "s3-storage"
  share_path = "/${dsm_shared_folder.containers.name}/s3-storage"
  compose_yaml = <<-YAML
    services:
      object-storage:
        image: minio/minio:latest
        command: server /data --console-address ":9001"
        ports:
          - "9000:9000"
          - "9001:9001"
        volumes:
          - ./data:/data
        restart: unless-stopped
  YAML

  running           = true
  delete_on_destroy = false
  depends_on        = [dsm_package.container_manager]
}
```

```bash
# UUID and project name are both accepted.
terraform import dsm_container_project.s3_storage s3-storage
```

> **`share_path` is a File Station path.** Use `/containers/s3-storage`, not
> `/volume1/containers/s3-storage`. The shared folder itself must already exist;
> the provider creates the project directory beneath it.

> **Destroy is non-destructive by default.** With `delete_on_destroy = false`,
> Terraform removes only its state entry. If explicitly enabled, the provider
> asks DSM to preserve the project directory, but Container Manager may still
> remove project containers, networks, and related data.

> **Sensitive is not encrypted state.** Terraform redacts `compose_yaml` in UI
> output, but still stores it in state. Use an encrypted remote state backend
> and inject application secrets through a separate secret-management path.

> **Requires physical hardware.** Container Manager is model-specific and is
> unavailable on Virtual DSM. Declare the `dsm_package.container_manager`
> dependency explicitly so the package is running before project creation.

### `dsm_file`

Uploads a file into a shared folder through File Station. This is what keeps
configuration out of `compose_yaml`: an S3 credentials file, a `Caddyfile`, or a
`config.toml` becomes a plain file on disk instead of a `printf` in a throwaway
`busybox` service — and no `$` has to be escaped for Compose interpolation.

| Attribute        | Type   | Required | Computed | Description                                                                 |
|------------------|--------|----------|----------|-----------------------------------------------------------------------------|
| `id`             | string | -        | yes      | Absolute File Station path of the file                                      |
| `share_path`     | string | yes      | -        | File Station directory, e.g. `/containers/seaweedfs/conf`. Forces replacement. |
| `name`           | string | yes      | -        | File name inside `share_path`. Forces replacement.                          |
| `content`        | string | -        | -        | UTF-8 content (sensitive). Conflicts with `content_base64`.                 |
| `content_base64` | string | -        | -        | Base64 content for binary files (sensitive). Conflicts with `content`.      |
| `checksum`       | string | -        | yes      | SHA-256 of the content stored on DSM                                        |
| `size`           | number | -        | yes      | File size in bytes                                                          |

```hcl
resource "dsm_shared_folder" "containers" {
  name     = "containers"
  vol_path = "/volume1"
}

resource "dsm_file" "s3_identities" {
  share_path = "/${dsm_shared_folder.containers.name}/seaweedfs/conf"
  name       = "s3.json"

  content = jsonencode({
    identities = [{
      name        = "nextcloud"
      credentials = [{ accessKey = var.s3_access_key, secretKey = var.s3_secret_key }]
      actions     = ["Read", "Write", "List"]
    }]
  })
}

resource "dsm_file" "keystore" {
  share_path     = "/${dsm_shared_folder.containers.name}/caddy/conf"
  name           = "keystore.p12"
  content_base64 = filebase64("${path.module}/keystore.p12")
### `dsm_system_settings`

Manages the NAS date and time configuration — Control Panel → Regional Options →
Time. A wrong clock is not a cosmetic problem: it desynchronises logs from the
containers running on the same box, breaks certificate validation, and shifts
every scheduled task.

These are NAS-wide settings, so declare **at most one** instance of this resource
per DSM host.

| Attribute     | Type   | Required | Computed | Description                                                     |
|---------------|--------|----------|----------|-----------------------------------------------------------------|
| `id`          | string | -        | yes      | Always `system_settings`                                        |
| `timezone`    | string | -        | yes      | DSM time zone name, e.g. `Moscow`. Synology's own naming        |
| `ntp_enabled` | bool   | -        | yes      | Whether the clock is synchronised with an NTP server            |
| `ntp_server`  | string | -        | yes      | NTP server, e.g. `time.google.com`                              |

Every attribute is optional. One left out of the configuration keeps whatever DSM
currently has and is recorded in state, so it does not turn into a perpetual
diff — which makes it possible to pin only the time zone and leave the NTP
configuration to whoever set it up.

```hcl
resource "dsm_system_settings" "this" {
  timezone    = "Moscow"
  ntp_enabled = true
  ntp_server  = "time.google.com"
}

# Or manage the time zone alone and leave NTP untouched.
resource "dsm_system_settings" "timezone_only" {
  timezone = "Amsterdam"
### `dsm_reverse_proxy`

Manages an entry in Control Panel → Login Portal → Advanced → Reverse Proxy —
the natural way to publish a Container Manager workload without running a second
proxy alongside DSM.

| Attribute                   | Type        | Required | Computed | Description                                                       |
|-----------------------------|-------------|----------|----------|-------------------------------------------------------------------|
| `id`                        | string      | -        | yes      | Entry UUID assigned by DSM                                        |
| `description`               | string      | yes      | -        | Entry description; DSM's only human-readable handle, must be unique |
| `source`                    | block       | yes      | -        | Public listener: `protocol`, `hostname`, `port`                    |
| `destination`               | block       | yes      | -        | Upstream service: `protocol`, `hostname`, `port`                   |
| `websocket`                 | bool        | -        | yes      | Add the `Upgrade`/`Connection` header pair. Defaults to `false`.    |
| `http2`                     | bool        | -        | yes      | Enable HTTP/2 on the listener. Defaults to `false`.                |
| `hsts`                      | bool        | -        | yes      | Send `Strict-Transport-Security`. Defaults to `false`.             |
| `custom_headers`            | map(string) | -        | -        | Extra request headers, e.g. the `X-Forwarded-*` family             |
| `access_control_profile`    | string      | -        | -        | Name of an existing Login Portal access control profile            |
| `access_control_profile_id` | string      | -        | yes      | UUID of the applied profile                                        |
| `proxy_connect_timeout`     | number      | -        | yes      | Connect timeout in seconds. Defaults to `60`.                      |
| `proxy_read_timeout`        | number      | -        | yes      | Read timeout in seconds. Defaults to `60`.                         |
| `proxy_send_timeout`        | number      | -        | yes      | Send timeout in seconds. Defaults to `60`.                         |
| `proxy_intercept_errors`    | bool        | -        | yes      | Replace upstream errors with DSM error pages. Defaults to `false`. |

```hcl
resource "dsm_reverse_proxy" "nextcloud" {
  description = "Nextcloud"

  source {
    protocol = "HTTPS"
    hostname = "cloud.example.com"
    port     = 443
  }

  destination {
    protocol = "HTTP"
    hostname = "localhost"
    port     = 8080
  }

  websocket = true
  http2     = true
  hsts      = true

  custom_headers = {
    "X-Forwarded-Proto" = "$scheme"
    "X-Forwarded-Host"  = "$host"
    "X-Forwarded-For"   = "$proxy_add_x_forwarded_for"
    "X-Real-IP"         = "$remote_addr"
  }

  access_control_profile = "internal-only"
}
```

```bash
terraform import dsm_file.s3_identities /containers/seaweedfs/conf/s3.json
```

> **`share_path` is a File Station path.** Use `/containers/seaweedfs/conf`, not
> `/volume1/containers/seaweedfs/conf`. Missing subdirectories are created; the
> shared folder itself must already exist (declare a `dsm_shared_folder` for it).

> **Drift is detected by reading the file back.** Every refresh downloads the
> file and recomputes `checksum`, so an edit made outside Terraform shows up as a
> plan. Because the content therefore lives in state, this resource is meant for
> configuration-sized files; anything above 16 MiB is refused.

> **Sensitive is not encrypted state.** `content` and `content_base64` are
> redacted in UI output but stored in state — use an encrypted remote state
> backend for credentials files.

> **POSIX permissions are not managed.** File Station exposes no API for a file
> mode, so there is no `mode` attribute; files are created with the DSM defaults
> of the destination shared folder.
terraform import dsm_system_settings.this system_settings
```

> **Time zones use Synology's names, not IANA identifiers.** DSM expects
> `Moscow`, not `Europe/Moscow`; the spelling is the one shown in Control Panel →
> Regional Options → Time. An unknown name is rejected with error 5701, and the
> provider then suggests the likely DSM equivalent in the diagnostic.

> **Destroy is always a no-op.** There is no meaningful value to reset a time
> zone or NTP server to, so removing the resource only drops it from state and
> leaves the NAS clock configuration alone.

> **DSM rejects partial writes.** `SYNO.Core.Region.NTP` answers error 5701 to a
> `set` that carries only the field being changed, so the provider reads the
> current settings and writes the complete set back — `timezone`, `enable_ntp`
> and `server`, and deliberately *not* the clock readings `get` returns
> alongside them, since sending those is DSM's "set the time manually" path.
> This API is undocumented and the exact contract is inferred, so the write path
> is gated in acceptance tests behind `DSM_ACC_SYSTEM_SETTINGS=1`.

> **Writes may require the built-in `admin` account.** `SYNO.Core.Region.NTP` has
> been reported to answer error 119 for other administrator accounts, the same
> restriction as `dsm_user_home_service`.
# UUID and description are both accepted.
terraform import dsm_reverse_proxy.nextcloud 1b0d0c30-9e1f-4a2b-8f7e-2c9d1a5b7e40
```

> **`description` is the identity, `id` is the key.** DSM has no name field for
> reverse proxy entries: the UI's "Description" is the `description` field, and
> DSM identifies entries by the UUID it assigns. Descriptions must be unique —
> the provider refuses to create a second entry with one that already exists.

> **`websocket` and `custom_headers` share one DSM list.** DSM stores both in
> `customize_headers`; `websocket = true` prepends the same pair DSM's own
> WebSocket preset uses. Do not also set `Upgrade` or `Connection` by hand.

> **Certificates are a separate concern.** An `HTTPS` source needs a certificate
> bound to it in Control Panel → Security → Certificate. This provider does not
> manage certificates.

> **Access control profiles are not created here.** Reference an existing
> profile by name; the provider resolves it to the UUID DSM stores.

> **The API is undocumented.** `SYNO.Core.AppPortal.ReverseProxy` is not in
> Synology's developer guide. The wire contract used here was reconstructed from
> published DSM 7.x captures and independent working clients, and is covered by
> unit tests that assert the exact request payload. The `http2` flag in
> particular is inferred rather than observed: it is only sent when enabled, and
> a DSM build that does not report it back keeps the configured value instead of
> producing a permanent diff. Acceptance tests for this resource are opt-in
> behind `DSM_ACC_REVERSE_PROXY=1`.

## Data sources

Each resource has a read-only data source counterpart that takes the identifying
attributes as input and returns the remaining computed attributes:

| Data source              | Input (required)                                   | Output (computed)                                   |
|--------------------------|----------------------------------------------------|-----------------------------------------------------|
| `dsm_user`               | `name`                                             | `id`, `description`, `email`, `disabled`, `expire_date`, `two_factor_enabled`, `groups`, `uid` |
| `dsm_group`              | `name`                                             | `id`, `description`, `gid`                          |
| `dsm_shared_folder`      | `name`                                             | `id`, `description`, `vol_path`, `hidden`, `enable_recycle_bin`, `recycle_bin_admin_only`, `enable_share_compress`, `enable_share_cow`, `share_quota`, `uuid` |
| `dsm_share_permission`   | `share_name`, `user_group_type`, `principal_name`  | `id`, `permission`                                  |
| `dsm_user_quota`         | `share_name`, `username`                           | `id`, `quota_size`, `quota_used`                    |
| `dsm_user_home_service`  | — (singleton)                                      | `id`, `enable`, `location`, `enable_recycle_bin`, `enable_domain`, `enable_ldap`, `encryption`, `personal_photo_enable` |
| `dsm_package`            | `name`                                             | `id`, `display_name`, `version`, `status`, `running`, `description`, `maintainer`, `can_uninstall` |
| `dsm_container_project`  | `name`                                             | `id`, `share_path`, `compose_yaml`, `running`, `path`, `status`, `container_ids` |
| `dsm_system_settings`    | — (singleton)                                      | `id`, `timezone`, `ntp_enabled`, `ntp_server`, `current_date`, `current_time`, `timestamp` |
| `dsm_reverse_proxy`      | `description`                                      | `id`, `source_protocol`, `source_hostname`, `source_port`, `destination_protocol`, `destination_hostname`, `destination_port`, `websocket`, `http2`, `hsts`, `custom_headers`, `access_control_profile`, `access_control_profile_id`, `proxy_connect_timeout`, `proxy_read_timeout`, `proxy_send_timeout`, `proxy_intercept_errors` |

## Development

This project uses [Task](https://taskfile.dev) (not Make).

```bash
task build          # build the provider binary to bin/
task test           # run unit tests (go test -v -count=1 ./...)
task test-acc       # sweep leftovers, then run acceptance tests (requires a reachable DSM)
task sweep          # delete leftover acceptance-test resources without running tests
task lint           # go vet ./...
task docs           # format examples and regenerate Terraform Registry docs
task docs:check     # validate examples and fail on stale generated docs
task install        # build + install into ~/.terraform.d/plugins/ for local use
task clean          # remove build artifacts and test cache
```

### Acceptance tests

Acceptance tests run against a **virtual DSM** (`vdsm/virtual-dsm`) inside a Lima VM:

```bash
task test-env-up      # start the Lima VM + virtual-dsm container
task test-env-status  # check status
task test-env-down    # stop everything
```

Run the tests:

```bash
export SYNOLOGY_DSM_HOST="http://localhost:5001"
export SYNOLOGY_DSM_USERNAME="admin"
export SYNOLOGY_DSM_PASSWORD=""
TF_ACC=1 go test -v -timeout 30m ./...
```

**Virtual DSM specifics:**

- Login with an empty password (`admin` / `""`) while in first-login state.
- The user quota API returns error 102 — not supported on virtual DSM. The
  three `dsm_user_quota` acceptance tests are skipped unless `DSM_ACC_QUOTA=1`
  is set; run them against real hardware with that env var enabled.
- Leftovers from a failed or interrupted run make the next one fail on creation
  with error 3301. `task test-acc` sweeps first; run `task sweep` on its own to
  clean up manually. Sweepers only delete objects whose name starts with
  `tfacctest`, so anything else on the NAS is left alone. Container Manager
  projects are swept before their backing shared folders.
- Sessions are short-lived; the provider re-authenticates on error 119 automatically
  (once per call — a 119 that survives a fresh session is reported as is, since
  some APIs use that code for "not the built-in admin").
- The user home service (`SYNO.Core.User.Home`) works fully on virtual DSM, so
  its acceptance tests run unconditionally.
- Installed-package lookup/import can be tested on virtual DSM, but installing
  a Package Center package is blocked there with error 103 and must be verified
  on compatible physical hardware.
- Container Manager projects are unavailable on virtual DSM. Their acceptance
  tests require physical hardware and explicit `DSM_ACC_CONTAINER_PROJECT=1`
  plus an image in `DSM_ACC_CONTAINER_IMAGE` that the NAS can pull.

### Release flow

Releases are automated via **Release Please** + **GoReleaser**:

1. All commits to `main` use [conventional commits](https://www.conventionalcommits.org/)
   (`feat:`, `fix:`, `docs:`, `ci:`, `deps:`, `breaking:`).
2. Release Please opens and maintains a release PR with the changelog and version bump.
3. Merging the release PR creates a draft GitHub Release and a git tag.
4. GoReleaser builds, signs, verifies, and uploads the Registry artifacts.
5. The workflow publishes the release only after verification succeeds.

```
conventional commits → Release Please PR → merge → draft release → signed artifacts → publish
```

Never create tags manually — Release Please manages versions.

## License

[MIT](LICENSE)
