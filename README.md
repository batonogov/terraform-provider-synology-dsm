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

Most resources also have a matching data source. See [`docs/`](docs/index.md) for full reference.

## Requirements

- Terraform >= 1.0
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
