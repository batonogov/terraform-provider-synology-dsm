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
| `dsm_firewall` | Firewall global switch, active profile, and default policy |
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

## Before you manage the firewall

`dsm_firewall` and `dsm_firewall_rule` can make a NAS unreachable, and the only way back from that is physical access. Three things are worth knowing before the first apply.

**The provider checks, but it cannot check everything.** Before switching the firewall on, switching profiles, tightening a default policy, or writing a rule, the provider replays the resulting profile against the address its own session connects from and refuses a change that would deny it. The check is blind to NAT (it sees the local end of the TCP connection, which is not what DSM sees behind a router) and cannot replay GeoIP or DSM service-preset rules — those make it inconclusive, which warns rather than blocks. `allow_lockout = true` turns it off.

**The write side of the global switch is reconstructed, not captured.** `SYNO.Core.Security.Firewall` is undocumented. Its `set` method and its two-field shape are confirmed from primary sources — DSM's own webapi descriptor lists exactly `{set, get}` on version 1, and Synology's `synofirewall/synoFW.hpp` models the global state as a status flag plus an active profile name — and the parameter names are corroborated by the one published implementation that writes this API against DSM 7.2/7.3. But nothing here has been run against a NAS from this repository, and two details are genuine guesses: the HTTP verb, and whether `profile_name` travels plain or JSON-quoted. Both have a fallback rather than a bet — POST then GET, then a retry with the other encoding — and DSM rejecting all of them produces an error that says so. Reading the firewall is unaffected. `dsm_firewall_rule` writes through the separate `Firewall.Profile` API, which has a problem of its own — see below. Acceptance tests for `dsm_firewall` are gated behind `DSM_ACC_FIREWALL=1` and never switch the firewall on.

**Writing rules does not work on DSM 7.2.2, and the provider refuses rather than pretends.** [Issue #130](https://github.com/batonogov/terraform-provider-synology-dsm/issues/130) reported five rules created in one apply, five successful responses from DSM, and a profile that afterwards held no rules at all. Half of that is now understood, from a contract captured on a live virtual DSM 7.2.2:

- **Confirmed, and fixed.** `SYNO.Core.Security.Firewall.Profile get` answers in an *adapter-keyed* shape — `{"name": "default", "global": {"policy": "none", "rules": []}}` — where each network adapter is a top-level key and the fall-through policy is a **string** (`none` / `allow` / `drop`). There is no `rules` key and no `adapterPolicyMap` key at the top level at all. The provider used to send exactly those two, and DSM answered `success: true` and stored nothing. It now reads and writes whichever of the two shapes a given DSM answers in, chosen from the response itself rather than from a version number. Writing the default policy in the adapter-keyed shape is confirmed to round-trip, so **`dsm_firewall` works**.
- **Still unknown, and deliberately not guessed.** How a *rule* is encoded inside that shape. Every candidate form — the on-disk object from Synology's own `fwDB.hpp`, the string-enum variant two published clients send, snake_case, and even a bare `[{}]` — makes DSM's request parser crash: synoscgi answers an HTML error page instead of JSON and the DSM web interface goes down with it. The provider therefore **refuses to send a rule** on a DSM that speaks this shape, with an error that says so, rather than taking somebody's NAS down to find out. `dsm_firewall_rule` still writes on a DSM that answers in the on-disk `{rules, adapterPolicyMap}` shape, and the `dsm_firewall_rule` / `dsm_firewall_rules` data sources read rules on either.
- **What the refusal covers.** Encoding, not presence. A rule DSM sent is handed back as the very object DSM produced — nothing is rendered, which is exactly what the DSM web interface does when it saves the page — so managing `default_policy` with `dsm_firewall` keeps working on a profile full of rules created by hand, and reordering rules is fine because moving an object does not reshape it. Refused is any write that would have to *spell a rule out*: one being created or edited, or an entry the parser could not read, where sending the rest would delete it silently.

Independently of the shape, DSM's `success` is no longer treated as evidence: every write and every delete is read back from the NAS, and a change that did not take fails the apply with a diagnostic naming what DSM actually answered. A profile response in neither known shape is an error rather than an empty profile — reading it as empty and writing it back would have replaced every rule on the NAS with the one being added.

Lifting the rule limitation needs exactly one artefact: a profile that actually holds rules, from a NAS where they were created in Control Panel. Either

```
cat /usr/syno/etc/firewall.d/*.json
```

over SSH, or the raw body of

```
curl -s '<dsm>/webapi/entry.cgi?api=SYNO.Core.Security.Firewall.Profile&version=1&method=get&name=default&_sid=<sid>&SynoToken=<token>'
```

together with your DSM version, the NAS model, and whether the account is the built-in `admin`. Attach it to [issue #130](https://github.com/batonogov/terraform-provider-synology-dsm/issues/130).

The safe order is: create the rules with the firewall off, apply, confirm the rules read back as intended, and only then set `enabled = true`.

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
