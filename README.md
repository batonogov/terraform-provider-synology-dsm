# terraform-provider-synology-dsm

A Terraform provider for managing [Synology DSM](https://www.synology.com/en-global/dsm) as a corporate file cloud — provision users, groups, shared folders, share permissions, and user quotas as Infrastructure as Code.

Built with the Terraform Plugin Framework and the Synology DSM web API (`SYNO.API.Auth` v7 with SynoToken). Developed and tested against DSM 7.2.2 and DSM 7.3.2.

## Features

| Resource | Description |
|----------|-------------|
| [`dsm_user`](#dsm_user) | Manage local user accounts |
| [`dsm_group`](#dsm_group) | Manage groups |
| [`dsm_shared_folder`](#dsm_shared_folder) | Manage shared folders |
| [`dsm_share_permission`](#dsm_share_permission) | Manage share-level access (R/W/deny) for users and groups |
| [`dsm_user_quota`](#dsm_user_quota) | Manage per-user quotas on a shared folder |

Each resource has a matching data source (`dsm_user`, `dsm_group`, `dsm_shared_folder`, `dsm_share_permission`, `dsm_user_quota`) for reading existing objects.

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
| `id`          | string       | -        | yes      | Identifier (username)                        |
| `name`        | string       | yes      | -        | Username. Forces replacement if changed.     |
| `password`    | string       | yes      | -        | Password (sensitive). Cannot be imported.    |
| `description` | string       | no       | -        | Description                                  |
| `email`       | string       | no       | -        | Email address                                |
| `disabled`    | bool         | no       | yes      | Account disabled (default: `false`)          |
| `groups`      | list(string) | no       | -        | Group memberships                            |
| `uid`         | int          | -        | yes      | UID assigned by DSM (read-only)              |

```bash
terraform import dsm_user.john john.doe
```

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

| Attribute            | Type   | Required | Computed | Description                                          |
|----------------------|--------|----------|----------|------------------------------------------------------|
| `id`                 | string | -        | yes      | Identifier (name)                                    |
| `name`               | string | yes      | -        | Shared folder name. Forces replacement if changed.   |
| `vol_path`           | string | yes      | -        | Volume path (e.g. `/volume1`). Forces replacement.   |
| `description`        | string | no       | -        | Description                                          |
| `hidden`             | bool   | no       | yes      | Hide from network browsing (default: `false`)        |
| `enable_recycle_bin` | bool   | no       | yes      | Enable recycle bin (default: `true`)                 |
| `uuid`               | string | -        | yes      | UUID assigned by DSM (read-only)                     |

```bash
terraform import dsm_shared_folder.team_data team-data
```

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

## Data sources

Each resource has a read-only data source counterpart that takes the identifying
attributes as input and returns the remaining computed attributes:

| Data source              | Input (required)                                   | Output (computed)                                   |
|--------------------------|----------------------------------------------------|-----------------------------------------------------|
| `dsm_user`               | `name`                                             | `id`, `description`, `email`, `disabled`, `groups`, `uid` |
| `dsm_group`              | `name`                                             | `id`, `description`, `gid`                          |
| `dsm_shared_folder`      | `name`                                             | `id`, `description`, `vol_path`, `uuid`             |
| `dsm_share_permission`   | `share_name`, `user_group_type`, `principal_name`  | `id`, `permission`                                  |
| `dsm_user_quota`         | `share_name`, `username`                           | `id`, `quota_size`, `quota_used`                    |

## Development

This project uses [Task](https://taskfile.dev) (not Make).

```bash
task build          # build the provider binary to bin/
task test           # run unit tests (go test -v -count=1 ./...)
task test-acc       # run acceptance tests (TF_ACC=1, requires a reachable DSM)
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
- Sessions are short-lived; the provider re-authenticates on error 119 automatically.

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
