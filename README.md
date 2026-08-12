# terraform-provider-synology-dsm

A Terraform provider for managing [Synology DSM](https://www.synology.com/en-global/dsm) as Infrastructure as Code — provision packages, users, groups, shared folders, permissions, quotas, and per-user home folders.

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

Each resource has a matching data source (`dsm_user`, `dsm_group`, `dsm_shared_folder`, `dsm_share_permission`, `dsm_user_quota`, `dsm_user_home_service`, `dsm_package`) for reading existing objects.

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
      source  = "batonogov/dsm"
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
      source  = "batonogov/dsm"
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

## Development

This project uses [Task](https://taskfile.dev) (not Make).

```bash
task build          # build the provider binary to bin/
task test           # run unit tests (go test -v -count=1 ./...)
task test-acc       # sweep leftovers, then run acceptance tests (requires a reachable DSM)
task sweep          # delete leftover acceptance-test resources without running tests
task lint           # go vet ./...
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
  `tfacctest`, so anything else on the NAS is left alone.
- Sessions are short-lived; the provider re-authenticates on error 119 automatically
  (once per call — a 119 that survives a fresh session is reported as is, since
  some APIs use that code for "not the built-in admin").
- The user home service (`SYNO.Core.User.Home`) works fully on virtual DSM, so
  its acceptance tests run unconditionally.
- Installed-package lookup/import can be tested on virtual DSM, but installing
  a Package Center package is blocked there with error 103 and must be verified
  on compatible physical hardware.

### Release flow

Releases are automated via **Release Please** + **GoReleaser**:

1. All commits to `main` use [conventional commits](https://www.conventionalcommits.org/)
   (`feat:`, `fix:`, `docs:`, `ci:`, `deps:`, `breaking:`).
2. Release Please opens and maintains a release PR with the changelog and version bump.
3. Merging the release PR creates a GitHub Release and a git tag.
4. GoReleaser builds binaries for all platforms and uploads them.

```
conventional commits → Release Please PR → merge → GitHub Release → GoReleaser → binaries
```

Never create tags manually — Release Please manages versions.

## License

[MIT](LICENSE)
