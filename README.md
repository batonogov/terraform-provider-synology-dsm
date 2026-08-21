# terraform-provider-synology-dsm

A Terraform provider for [Synology DSM](https://www.synology.com/en-global/dsm) — manage users, groups, shared folders, permissions, quotas, packages, Container Manager projects, files, TLS certificates, reverse proxy, firewall, Task Scheduler, and system settings as Infrastructure as Code.

Built with the Terraform Plugin Framework and the Synology DSM web API (`SYNO.API.Auth` v7 with SynoToken). Developed and tested against DSM 7.2.2 and DSM 7.3.2.

## Quick start

```hcl
terraform {
  required_providers {
    dsm = {
      source = "batonogov/synology-dsm"
    }
  }
}

provider "dsm" {
  host     = "https://diskstation:5001"
  username = "admin"
  password = var.dsm_password
  insecure = true
}
```

All provider attributes can also be set via environment variables: `SYNOLOGY_DSM_HOST`, `SYNOLOGY_DSM_USERNAME`, `SYNOLOGY_DSM_PASSWORD`, `SYNOLOGY_DSM_INSECURE`, `SYNOLOGY_DSM_ALLOW_TASK_EXECUTION`.

## Resources

| Resource | Description |
|----------|-------------|
| `dsm_user` | Local user accounts |
| `dsm_group` | Groups |
| `dsm_shared_folder` | Shared folders |
| `dsm_share_permission` | Share-level access (R/W/deny) for users and groups |
| `dsm_user_quota` | Per-user quotas on a shared folder |
| `dsm_user_home_service` | User home folders |
| `dsm_package` | Package Center packages |
| `dsm_container_project` | Docker Compose projects in Container Manager |
| `dsm_file` | Upload configuration files into a shared folder |
| `dsm_system_settings` | NAS time zone and NTP |
| `dsm_reverse_proxy` | Login Portal reverse proxy entries |
| `dsm_firewall_rule` | Firewall profile rules |
| `dsm_scheduled_task` | Task Scheduler script tasks |
| `dsm_event_task` | Boot/shutdown tasks |
| `dsm_certificate` | Import externally issued TLS certificates |
| `dsm_certificate_lets_encrypt` | Let's Encrypt certificates via DSM ACME |
| `dsm_certificate_service` | Bind a certificate to an individual DSM service |
| `dsm_notification_mail` | Outgoing SMTP transport for DSM notifications |

Most resources also have a matching data source. See [`docs/`](docs/index.md) for full reference.

## Scope

Everything above is managed over the DSM web API. Three things are *not*, for
three different reasons — the distinction matters when a bootstrap checklist has
to say whether a step will ever become automatable:

- **Not implemented yet, contributions welcome** — group membership as its own
  object, the SMB/NFS/FTP file services, shared folder encryption, Login Portal
  access control profiles, joining a directory service. The usual blocker is an
  undocumented DSM API whose write contract has to be captured from hardware
  first.
- **Not expressible through the DSM API** — POSIX mode and ownership (reported,
  never writable), fields DSM accepts but never returns, one-shot scheduled
  tasks. These stay manual until Synology changes the API.
- **The NAS's own network settings** (static address, gateway, DNS) — not
  implemented, and the interesting case: an apply that moves the address of the
  host the provider is talking to severs the session that would confirm the
  result, and a wrong address is recoverable only with physical access. It is
  not ruled out, but it needs a lockout guard like the firewall's plus a
  confirmation path at the new address.

The full breakdown, including what an implementation of the network resources
would have to carry, is in [the provider documentation](docs/index.md#scope-what-this-provider-manages-and-what-it-does-not).

## Keeping secrets out of Terraform state

`dsm_file` and `dsm_container_project` write content to the NAS, and that content is normally kept in Terraform state — which makes access to state equal to access to every credential it carries. Both resources therefore accept [write-only arguments](https://developer.hashicorp.com/terraform/language/resources/ephemeral#write-only-arguments) (Terraform 1.11 and later): `content_wo` / `content_base64_wo` and `compose_yaml_wo`. The value reaches DSM during apply and is stored neither in state nor in the plan file.

```hcl
resource "dsm_file" "database_password" {
  share_path = "/containers/nextcloud/conf"
  name       = "db-password"

  content_wo         = var.database_password
  content_wo_version = 1 # increment to write the current value again
}
```

Terraform cannot diff a value it does not store, so the `_wo_version` counter is what asks for a rewrite. Drift is still detected in the other direction: state keeps a checksum (`checksum`, `compose_yaml_checksum`), and a file edited on the NAS no longer matches the last write, which plans an apply that restores the configured content.

## Requirements

- Terraform >= 1.0 (>= 1.11 for the write-only arguments above)
- Synology DSM 7.2+ (DSM 6.x may differ)

## Development

```bash
task build     # build
task test      # unit tests
task test-acc  # acceptance tests (requires virtual DSM or real hardware)
task docs      # regenerate registry docs
```

Releases are automated via Release Please + GoReleaser. Use [conventional commits](https://www.conventionalcommits.org/); never create tags manually.

## License

[MIT](LICENSE)
