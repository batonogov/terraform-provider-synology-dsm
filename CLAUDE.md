# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Development Commands

Uses [Task](https://taskfile.dev) (not Makefile):

```
task build       # Build binary to bin/
task test        # Unit tests (go test -v -count=1 ./...)
task test-acc    # Acceptance tests (TF_ACC=1, requires real DSM)
task lint        # go vet ./...
task docs        # Generate Terraform Registry docs from provider schemas
task docs:check  # Validate examples and generated docs
task install     # Build + install to ~/.terraform.d/plugins/ for local testing
task clean       # Remove bin/ and test cache
```

Run a single test: `go test -v -run TestClient_CreateUser ./internal/client/`

## Architecture

Terraform provider for Synology DSM using Plugin Framework. Two layers:

**`internal/client/`** — Synology DSM HTTP API client
- `client.go`: Auth (SYNO.API.Auth v7), session management (SID + SynoToken), `DoAPI()` for GET requests, `DoAPIPost()` for POST requests (SID/SynoToken in query string, other params in body), retry with exponential backoff
- `user.go`, `group.go`, `share.go`, `share_permission.go`, `user_quota.go`, `package.go`, `container_project.go`, `task_scheduler.go`, `event_scheduler.go`: CRUD/lifecycle methods per resource

**`internal/provider/`** — Terraform Plugin Framework wiring
- `provider.go`: Provider schema (host/username/password/insecure), Configure creates client and logs in
- `resource_*.go`: Resources — full CRUD + ImportState
- `datasource_*.go`: Data sources — Read only

Flow: `main.go` → `provider.New(version)` → `Configure()` creates `client.NewClient()` + `Login()` → resources get `*client.Client` via `ProviderData`

## Critical Synology DSM API Details

- **Developed against DSM 7.2.2** (virtual DSM) and DSM 7.3.2 on RS4021xs+ — API behavior may differ on DSM 6.x

- **Most APIs use GET** — user/group operations send params as query string
- **`SYNO.Core.User` update is `method=set`** — there is no `update` method; it answers 103 ("method not exist"). Account state is **not** the `disabled` param (accepted and ignored on DSM 7) but `expired`: `normal` / `now` / a `YYYY/M/D` date. DSM always answers dates without leading zeros regardless of input, so the client normalises to `YYYY-MM-DD`. Full findings: `.pi/recon-user-fields-2026-08-07.md`
- **Shared folder uses POST** — `SYNO.Core.Share` create/update send `shareinfo` as form-encoded POST body. **Update is `method=set`, not `create` with `name_org`** — the latter is rejected with 3301. `share_quota` is written under that name but read back as `quota_value`, and its unit is **gigabytes**. `enable_share_compress`/`enable_share_cow` are creation-time only (a later `set` reports success and changes nothing), and compression requires cow. Full findings: `.pi/recon-share-attributes-2026-08-07.md`
- **Task Scheduler is versioned per method** — `SYNO.Core.TaskScheduler` uses version **3** for `list`, **4** for `get`/`create`/`set`, and **2** for `delete`/`run`/`set_enable`. Version 4 on `list` returns nothing usable. `schedule` and `extra` are bare JSON objects inside form values (not JSON-quoted); `name`/`owner`/`real_owner`/`type`/`enable`/`id` are plain values. `real_owner` is a lookup key, not decoration: `get`, `set`, `delete`, and `run` all require it, and only `list`/`get` report it — hence `GetScheduledTask` lists first. The schedule document nests `"version": 4`, and `repeat_date` is `1001`/`1002`/`1003` for daily/weekly/monthly with `date_type: 0` (a plain `0` is rejected on DSM 7). `week_day` is `"0,1,2"` with 0 = Sunday, not a bitmask. `set` replaces the whole task; there is no partial update.
- **Event Scheduler is shaped differently from Task Scheduler** — `SYNO.Core.EventScheduler` is version **1** throughout, is addressed by `task_name` (no numeric id, so rename = replace), has **no** `schedule` or `extra` object (the command is `operation` with `operation_type`, and the notify fields are top level), takes `owner` as a `{uid: name}` JSON map rather than a username, and JSON-quotes its string parameters. `event` is `bootup` or `shutdown`. DSM has no list method here; `SYNO.Core.TaskScheduler.list` returns event tasks mixed in, distinguishable only by having no `id`.
- **`.Root` is a separate API namespace, not a flag** — `SYNO.Core.TaskScheduler.Root` / `SYNO.Core.EventScheduler.Root` are used *instead of* the base namespace when the owner is `root`, for `create`/`set` only, and additionally require `SynoConfirmPWToken` from `SYNO.Core.User.PasswordConfirm` v2 `auth` (POST, param `password`). Reads and deletes always use the base namespace. The token is fetched per call; its lifetime is undocumented. DSM's own client RSA-encrypts the password over plain HTTP through `SYNO.API.Encryption`; this provider sends it plain and warns at plan time instead.
- **`_sid` and `SynoToken` must be in query string for POST requests** — DSM validates session from URL params, not POST body. `DoAPIPost()` handles this by moving them from body to query string.
- **Auth version 7** — `SYNO.API.Auth` version 7 with `enable_syno_token=yes`
- **Session via `_sid`** — Login returns SID, passed as `_sid` query param (no cookies needed with `format=sid`)
- **User home service is asynchronous-capable** — `SYNO.Core.User.Home` `set` is POST. On virtual DSM it completes synchronously (`{"success":true}`), but the API can return a `task_id`; poll `status` with that id until `finish: true`. `location` must be a volume **path** (`/volume1`) — a bare `volume1` gives error 3101. When disabling, send `enable=false` alone; extra fields trigger 3103. Full findings: `.pi/recon-user-home-2026-08-07.md`
- **`SYNO.FileStation.Upload` is the only multipart API** — `multipart/form-data` POST to `entry.cgi`. The **file part must be written last**: DSM's uploader consumes the stream sequentially and ignores every field after the file content. The destination *name* comes from the multipart filename, the destination *directory* from `path` — and that `path` is a **raw** form value, unlike `SYNO.FileStation.List`/`.Delete`, which take a JSON array of paths. `api`/`version`/`method` are repeated in the query string as well as the body, because DSM dispatches from the URL before parsing the body. Without `overwrite=true` an existing file fails with 414, and a refused upload is reported as `blSkip: true` inside a *successful* envelope
- **Certificates are split across two APIs, and the split is counter-intuitive** — `list`/`set`/`delete` live on `SYNO.Core.Certificate.CRT`, but `import` lives on the parent `SYNO.Core.Certificate`. Picking the wrong one answers 103. All are version 1. **Every certificate call needs `SynoToken` as well as `_sid`**; without it DSM answers 103, which reads like a bad method name. `import` is `multipart/form-data` with `key`/`cert`/`inter_cert` as file parts and `id`/`desc`/`as_default` as text — and unlike File Station the **files come first** and `api`/`version`/`method` are *not* repeated in the body. An empty `id` creates; an existing `id` replaces the material in place and keeps the service bindings (there is no "already exists" error — `import` deduplicates on the caller-supplied id). `CRT.set` wants `id` and `desc` as **JSON-quoted strings** (the same quirk as `SYNO.Docker.Project`) while `as_default` is a raw value. `CRT.delete` takes `ids` as a JSON array. Sources: acme.sh's `deploy/synology_dsm.sh`, the python `synology-api` client, and a captured DSM response in Tailscale's `configure-synology-cert_test.go`
- **`CRT.list` has no `self_signed` field** — DSM marks only its own built-in certificate, with a nested `self_signed_cacrt_info` object. Everything else has to be inferred from `issuer.common_name == subject.common_name`. `subject` and `issuer` are nested objects, not strings; the date fields are `valid_from`/`valid_till` in OpenSSL asctime with a **space-padded day** (`"May  6 00:00:00 2024 GMT"`), always GMT. `isPkg` is the only camelCase key. Several `services[]` fields are absent rather than null when unset
- **Let's Encrypt issuance is synchronous and long-blocking** — `SYNO.Core.Certificate.LetsEncrypt` v1 `create` takes `domain_name`, `email`, `desc` and runs the whole ACME exchange inline. There is **no task id and nothing to poll**. **SANs go into `domain_name` as a `;`-joined list, common name first** — `alt_domain`, `alt_names`, `domain_list`, and `server_url` do not exist, whatever third-party write-ups claim, and the production ACME directory is hardcoded. `as_default` on `create` is unverified, so the provider claims the default with a follow-up `CRT.set`
- **DSM restarts httpd after a certificate import** (`restart_httpd: true` in the response), dropping the connection and refusing the next few requests — `settleCertificate` reads the certificate back through a ~90s budget rather than once
- **No DSM API writes POSIX mode or ownership** — `SYNO.FileStation.Property` `set` accepts `posix`, `mode`, `unix_perm`, `posix_perm`, `permission`, an `owner` object, and any combination of them, answers `{"running":1,"taskid":…}` every time, and changes nothing. `SYNO.Core.ACL` (`get`/`set`/`status`/`stop`, v1 only despite `maxVersion: 2`) manages the **Synology ACL**, not mode bits. `SYNO.FileStation.Property.ACLOwner` exposes `get` only — `set` is 103. `SYNO.FileStation.CreateFolder` ignores a `mode` parameter. The only knob DSM offers is the global `use_unix_default_perm` in `SYNO.FileStation.Settings`. Reading works: `SYNO.FileStation.List` `getinfo` with `additional=["perm","owner"]` returns `perm.posix`, `perm.is_acl_mode` and `owner.{user,group,uid,gid}`. **`perm.posix` is not a bitmask** — DSM prints the octal digits as a decimal number, so 755 means `rwxr-xr-x`; and `perm.acl` describes the *calling session's* rights, not the path's ACL. A shared folder is addressed in File Station by its bare name (`/containers`), never by volume path. Full findings: `.pi/recon-posix-mode-2026-08-13.md` (issue #94)
- **`SYNO.FileStation.Download` returns raw bytes, not an envelope** — a failure comes back as JSON instead. A real download is served with `Content-Disposition`, so that header is what distinguishes a file that merely contains JSON from an error
- **`SYNO.Core.Region.NTP` `set` is a whole-object write** — a `set` carrying only `timezone` is rejected with 5701 ("parameter bad"), confirmed on DSM 7.4-90075 (issue #57). The client therefore does read-modify-write and sends `timezone` + `enable_ntp` + `server` together. Two details that are easy to get wrong: the parameter is **`server`, not `ntp_server`** (the symbol table of `lib/SYNO.Core.Region.so` has no such name), and the clock readings `get` returns alongside the config (`date`/`hour`/`minute`/`second`/`now`/`timestamp`) **must not be echoed back** — DSM exports `SYNONtpSet` and `SYNONtpSetWithModifiedTime` as separate entry points, so sending them is the "set the clock manually" path. `enable_ntp` is a mode **string** (`"ntp"`), not a boolean; only the enabled spelling has ever been captured, so the client echoes DSM's own value wherever it can and falls back to `"manual"` only when forced. Whether `set` wants GET or POST is unknown — no public capture exists — so `setRegionNTP` tries POST and falls back to GET on 103/105. `listzone` returns `{"zonedata":[{"value","offset"}]}` and is used to make an unknown-timezone error actionable.
- Error 105 = "session does not have permission" — usually means wrong HTTP method or missing SynoToken
- Error 119 = "SID not found or invalid" — typically means SID not in query string for POST, or session expired. **Also returned by `SYNO.Core.User.Home` when the account is not the built-in `admin`** — hence `doRequestWithRetry` re-logins at most once per call and then surfaces the 119 verbatim
- Error 3101 = "invalid location" for `SYNO.Core.User.Home` (bare volume name instead of a path)
- Error 3103 = "location parameter missing" for `SYNO.Core.User.Home`
- Error 3301 = "share already exists"
- Error 3106 = "user not found" (from `get` method with `additional` param)
- Error 5701 = "parameter bad" for `SYNO.Core.Region.NTP` — an incomplete or unrecognised time-settings payload. DSM renders it as "please sign in to DSM again", which is misleading: the session is fine.
- Error 4151 = `SYNO.Core.AppPortal.ReverseProxy` `entry` missing or not sent as a JSON string (flattening the object into separate params triggers it)
- Error 4154 = a reverse proxy entry with that description already exists
- **Reverse proxy is an undocumented API** — `SYNO.Core.AppPortal.ReverseProxy` v1 (POST). `list` returns `{entries: [...]}`; `create`/`update` take the whole rule as one form value `entry=<json>`; `delete` takes `uuids=<json array>`. `update` targets the entry by the `UUID` inside the payload. Protocol is an **integer**: `0` = HTTP, `1` = HTTPS. There is no name field — the UI's "Description" is `description`, and `UUID` is the identity. Reconstructed from published DSM 7.x captures and independent clients, not verified by this project; source list is at the top of `internal/client/reverse_proxy.go`.
- **Firewall is a whole-profile API, and it is undocumented** — `SYNO.Core.Security.Firewall*` are all version 1 on `entry.cgi` and admin-only. `Firewall get` answers `{"enable_firewall", "profile_name"}`; `Firewall.Profile get` reads a profile as `{"name", "rules": {<adapter>: [...]}, "adapterPolicyMap": {<adapter>: int}}`; `Firewall.Profile set` takes the whole profile as compact JSON in `profile` plus `profile_applying=false`, and only rewrites `/usr/syno/etc/firewall.d/*.json` — the change is not live until `Firewall.Profile.Apply start` → poll `status` → `stop`. `profile_applying=true` answers error 117 on DSM 7. **Never use `Firewall.Rules save_start`**: it crashes synoscgi (HTTP 502) on DSM 7.2.2 when given a rule with concrete fields. Rule and enum field names come from Synology's `synofirewall/fwDB.hpp`; see `internal/client/firewall.go`. None of this is verified against physical hardware.
- The **55xx family belongs to certificates**: 5510-5519 and 5534 are import failures (5514 = key does not match the certificate, 5517 = chain does not verify), 5520-5530 are Let's Encrypt failures (5521 = domain validation failed / port 80 unreachable, 5524 = rate limit, 5529 = not publicly resolvable). Wording extracted from DSM's own webman i18n strings by two independent clients; 5510/5512/5517 additionally confirmed by acme.sh bug reports

## Client patterns

- **Share mutations are serialised and retried** — DSM processes `SYNO.Core.Share` create/update/delete one at a time: overlapping calls answer 3328, and a call issued while an earlier mutation is still settling answers 3300. `shareMu` queues mutations client-side and `mutateShare` retries those two codes with exponential backoff (~15s budget); reads stay outside the lock. Permanent answers such as 3301 are never retried.
- **Error codes are the contract, messages are presentation** — `internal/client/errors.go` renders a DSM code as a sentence (`a share with this name already exists (code 3301)`) using a common table plus per-API overrides, because codes above 3000 mean different things in different APIs. `executeRequest` tags each `*APIError` with the API it came from. Never match on the rendered text: use `client.IsAPIError(err, codes...)`. Only add a description for a code whose meaning was verified against real DSM.

- **User fields DSM will not round-trip** — `cannot_chg_passwd` and `allow_ip` are accepted by `set` but never returned by `list`; `passwd_never_expire` is ignored outright. All three are left out of the schema. Per-user IP restrictions on DSM 7 live in the firewall (`SYNO.Core.Security.Firewall.*`), not the account.
- **User `get` returns minimal data without `additional`** — only `name` and `uid`. To get `description`, `email`, `disabled`, `groups` use `list` method with `additional=["description","email","disabled","groups"]` and filter by name.
- **`get` API returns arrays** — `SYNO.Core.User.get` returns `{users: [...]}`, `SYNO.Core.Group.get` returns `{groups: [...]}` — not a bare object. `parseUser`/`parseGroup` must unpack the array wrapper first.
- **Simple resources** (user, group): all CRUD via `DoAPI()` (GET). Delete sends name as JSON array.
- **Packages**: installed state comes from `SYNO.Core.Package.list` v2 with lifecycle fields nested under `additional`. Installation follows Package Center's queue flow (`SYNO.Core.Package.Server.list` → `SYNO.Core.Package.Installation.get_queue` → `install` → poll); start/stop uses `SYNO.Core.Package.Control`. Package removal is opt-in in the Terraform resource.
- **Container Manager projects**: `SYNO.Docker.Project` v1 uses POST and returns `list` as an object keyed by project UUID. Creation follows DSM's sequence: create the directory with `SYNO.FileStation.CreateFolder` → create an empty project → `update` the raw compose content → `build` → optional `start`. `name`, `share_path`, and `get_share_info.path` are JSON-quoted strings; `update.content` and direct-action `id` are raw form values. Current DSM builds expose direct `build`/`start`/`stop`; the client falls back on error 103 to the older GET streaming variants with a JSON-quoted id. A project `share_path` is a File Station path such as `/containers/s3-storage`, not `/volume1/containers/s3-storage`. Project deletion is opt-in and always sends `preserve_content=true`.
- **Certificates**: `internal/client/certificate.go` plus `certificate_pem.go`. The leaf of a PEM bundle is identified **structurally** — the certificate nothing else in the bundle was issued by — not as the first block: `cat chain.pem cert.pem` is common enough, and reading the intermediate as the leaf would report the CA's expiry years out and silently break every alert built on it. `expires_at` is parsed from that leaf's DER with `crypto/x509` whenever the configuration holds a PEM (`dsm_certificate`), and only falls back to DSM's `valid_till` text when there is none (`dsm_certificate_lets_encrypt`, the data source, an imported resource). Destroy re-reads the certificate from DSM — not from state — and refuses while `services` is non-empty, naming the services and the four ways out; `force_destroy` is the opt-out. Rotation goes through `import` with the existing id so the service bindings survive. Per-service binding (`SYNO.Core.Certificate.Service` `set`, which wants the whole service object plus the correct `old_id` echoed back) is deliberately not implemented yet.
- **Files**: `internal/client/file.go` carries the two request paths that cannot go through `executeRequest` — a multipart upload and a raw download. Both reuse the backoff and single-re-login policy via `retryFileTransfer`. `GetFileInfo` must inspect the per-entry `code` in a *successful* getinfo response, because DSM reports a missing path that way as well as through a failed envelope. `dsm_file` reads content back on every refresh (that is what makes an out-of-band edit a plan), so transfers are capped at 16 MiB to keep state sane. File Station has no API for a POSIX file mode, hence no `mode` attribute.
- **Shared folder**: create (`method=create`) and update (`method=set`) via `DoAPIPost()` (POST) with `shareinfo` JSON — no `name_org`, which DSM rejects with 3301. Get/list/delete via `DoAPI()` (GET). API returns `enable_recycle_bin` (not `recyclebin`) and `quota_value` (written as `share_quota`).
- **Reverse proxy entries**: mutations run under `reverseProxyMu`. The API is per-entry rather than a whole-list `set`, but DSM keeps every entry in one datastore it rewrites and re-renders into nginx on each change, and `CreateReverseProxy` is a non-atomic list → create → list sequence (DSM's create returns no usable UUID). `TestClient_CreateReverseProxy_Concurrent` models a lost-update-prone datastore and fails without the lock. Updates are layered onto the raw entry DSM returned, so unmodelled fields such as `_key` survive the round trip. `frontend.https.http2` is **inferred** from Synology's `synow3.h` and is only sent when enabled; `ReverseProxy.HTTP2` is a `*bool` so "DSM omitted it" stays distinguishable from "DSM said false".
- **Firewall rules**: `dsm_firewall_rule` manages one rule, but DSM has no per-rule API — every write reads the profile, splices one rule into `rules[<adapter>]`, and writes and applies the whole profile, all under `c.mu` (same lock as share permissions; without it parallel rules clobber each other). Order **is** the policy: DSM matches top to bottom and the array index is the only priority there is (`ruleIndex` is a max+1 counter, not a sort key), so `priority` is a required argument and Read reports the actual index. Rules round-trip through their original JSON map so GeoIP and service-preset selectors survive a write. Two guards live in the client: a lockout check that replays the profile against the client's own source address (recorded from the transport's dialer) and refuses a change that flips reachability from allowed to denied, and a refusal to leave the active profile empty while the firewall is on.
- **parseX()** helpers use `map[string]interface{}` type assertions, not typed structs — matches the loose DSM API responses.

## Provider data

`Configure` hands resources and data sources a `*dsmProviderData` (not a bare `*client.Client`), because some resources need provider-level configuration as well as an API client. Use the helpers in `helpers.go`:

- `clientFromProviderData(req.ProviderData, &resp.Diagnostics)` — the common case; returns nil when configuration cannot proceed (including the normal nil-ProviderData case)
- `providerDataFrom(...)` — when the resource also needs a policy flag, currently only `allowTaskExecution`

## Task execution opt-in

`dsm_scheduled_task` and `dsm_event_task` create DSM tasks that run shell commands, normally as root, which makes write access to a Terraform configuration equivalent to root shell access on the NAS. They are therefore gated behind the provider attribute `allow_task_execution` (default `false`, also readable from `SYNOLOGY_DSM_ALLOW_TASK_EXECUTION`; an explicit `false` in HCL wins over the env var).

- The gate lives in `ModifyPlan`, not `ValidateConfig` — Terraform validates resource config *before* configuring the provider, so `ValidateConfig` cannot see the flag. Failing during plan means a team that leaves it off never reaches apply.
- Destroy is deliberately not gated: removing a task executes nothing, and a configuration that turned the flag back off must still be able to clean up.
- Data sources are never gated: reading a task executes nothing.
- `user` has no default so the privilege is always stated in the open, and `user = "root"` adds a plan-time warning (plus a second warning about the password confirmation when the host is plain HTTP).
- `command` is intentionally **not** `Sensitive` — hiding a root command from plan output removes the only place a reviewer can see what will run.
- `user` is `RequiresReplace` on both resources. The owner selects the API namespace (`.Root` or not), so editing it in place would send the modification to the wrong namespace for the task DSM actually holds.
- The gate is enforced in `ModifyPlan` **and** re-checked in `Create`/`Update`. The duplication is deliberate: a privilege boundary should not rest on one call site, and `resource_task_lifecycle_test.go` fails if either check is removed.

## Task resource hazards worth preserving

Three failure modes here are specific to tasks and each has a regression test in `resource_task_lifecycle_test.go` or the client test files:

- **Never build a client request in argument position.** Go evaluates arguments before the call, so `client.CreateEventTask(ctx, buildRequest(...))` reaches DSM *before* any diagnostic the builder raised can be inspected — creating a root-capable task that no state entry tracks and destroy cannot remove. Build first, check `ok`, then call.
- **A create that succeeds but cannot be read back must still return a task.** `CreateScheduledTask`/`CreateEventTask` return a non-nil task *alongside* an error in that case, and the resources write state before reporting it. Same reason: DSM has already created the thing.
- **`real_owner` is never guessed.** The create path reads back through `GetScheduledTask`, which lists first. Assuming `real_owner == owner` breaks exactly when it matters — a root-owned task created from an admin session — and DSM answers a get with the wrong `real_owner` by returning an empty task, i.e. "not found" for something it just created.

Two smaller invariants: `repeat_until_hour` is a `*int` in the client because unset ("end the window at the start hour") and an explicit `0` are different, and an explicit value earlier than `hour` is rejected rather than rewritten (rewriting produced "provider produced inconsistent result after apply"). And `applyScheduledTaskToModel` takes an `applyMode`: an unrepresentable schedule is an error after a write but only a warning during refresh, because failing on refresh would break every later plan including the destroy that would clean it up.

## Resource implementation pattern

Every resource follows the same structure (see `resource_group.go` as the cleanest reference):

1. `XxxResource` struct with `client *client.Client`
2. `xxxResourceModel` struct with `types.String`/`types.Bool`/etc fields
3. Schema: `id` (computed), required fields with `RequiresReplace` for immutable attrs, optional fields with defaults via `booldefault.StaticBool()`
4. **Read must set ALL state fields** from API response (including `ID` and `Name`) — required for import to work
5. Read uses `state.ID` with fallback to `state.Name` for lookup after import
6. Import: `resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)`

## Conventions

- Preferred local provider name: `dsm`; public source address: `batonogov/synology-dsm`
- Resource naming: `dsm_user`, `dsm_group`, `dsm_shared_folder`, etc.
- Provider env vars: `SYNOLOGY_DSM_HOST`, `SYNOLOGY_DSM_USERNAME`, `SYNOLOGY_DSM_PASSWORD`
- Tests use `httptest.NewServer` — GET tests check `r.URL.Query().Get()`, POST tests read body and call `r.ParseForm()` then `r.FormValue()`
- Go 1.26, Terraform Plugin Framework v1.19.0, no third-party HTTP libs
- Repository language: Russian for docs/README, English for code

## Adding New Resources

1. Add API methods in `internal/client/<resource>.go` (follow existing patterns)
2. Create resource in `internal/provider/resource_<name>.go` (follow `resource_group.go`)
3. Create data source in `internal/provider/datasource_<name>.go` (follow `datasource_group.go`)
4. Register both in `provider.go` → `Resources()` and `DataSources()` return lists
5. Add unit tests in `internal/client/<resource>_test.go` with httptest mock
6. Update README.md features table and add attribute documentation

## Release Flow

Automated via **Release Please + GoReleaser**, split across two workflows so each release PR carries only the delta since the last release (not the whole history):

1. All commits to `main` must use **conventional commits** (`feat:`, `fix:`, `docs:`, `ci:`, `deps:`, `breaking:`)
2. `release-please.yml` (workflow 1) opens/updates the release PR with the changelog and version bump. It runs on `push` to `main` (skipping the release merge commit itself) and `workflow_dispatch`, and only manages the PR — it never tags (`skip-github-release: true`)
3. Merging the release PR triggers `release-publish.yml` (workflow 2) via `pull_request: closed` of a PR labelled **`autorelease: pending`** — the label Release Please puts on a release PR when it opens it. It tags the release and creates a draft GitHub Release (`skip-github-pull-request: true`), imports the Registry GPG key, runs GoReleaser, and verifies the artifacts. **Do not gate this on `autorelease: tagged`**: that label is applied *after* the release exists, which is workflow 2's own output, so a closed-PR event never carries it and every release is silently skipped (this is what left 0.3.0 merged but untagged). Workflow 2 also has `workflow_dispatch` so a release stranded that way can be finished by hand — the merged PR is still labelled `pending`, which is how Release Please finds it
4. After verification it publishes the draft (archives, manifest, checksums, and signature must pass), flips the PR label to `autorelease: published`, then re-runs workflow 1. Because the previous release is now `published`, the next release PR is computed as a delta

```
conventional commits → release PR → merge → tag + draft release → verified signed artifacts → publish → recompute next PR
```

- **A release cannot be created while `main`'s head commit touches `.github/workflows/`** — GitHub refuses `POST /repos/{owner}/{repo}/releases` from `GITHUB_TOKEN` when the target commit adds or modifies a workflow file, because that needs the `workflows` scope, which an Actions token cannot hold. It surfaces as `release-please failed: Resource not accessible by integration` right after `Creating 1 releases for pull #NN`, with `contents: write` plainly granted in the job — so it reads like a permissions misconfiguration and is not one. The normal flow never hits it: merging a release PR makes the release commit (CHANGELOG + manifest only) the head. It bites when workflow 2 is re-run by hand after a CI change has landed on top. Fix: land any commit that does not touch `.github/workflows/`, then re-run — do not go hunting for a missing permission
- **Never create tags manually** — Release Please manages versions
- **Never skip conventional commits** — changelog and versioning depend on them
- Dependabot keeps Go modules and GitHub Actions up to date (weekly, `deps:` / `ci:` prefix)

## CI/CD

- `.github/workflows/test.yml` — unit tests on push/PR
- `.github/workflows/release-please.yml` — opens/updates the release PR only (`skip-github-release`)
- `.github/workflows/release-publish.yml` — tags, signs, verifies, and publishes a release on `pull_request: closed` of a tagged release PR, then recomputes the release PR
- `.github/dependabot.yml` — weekly dependency updates (gomod + github-actions)

## Acceptance Test Environment

Tests run against a **virtual DSM** (QEMU via Lima VM + Docker):

```
task sweep            # Delete leftover tfacctest* resources from the target DSM
task test-env-up      # Start Lima VM + virtual-dsm container
task test-env-down    # Stop everything
task test-env-status  # Check status
```

**Setup:** Lima VM (`.lima/dsm.yaml`, aarch64 QEMU) → Docker inside VM runs `vdsm/virtual-dsm` container (`docker-compose.test.yml`) → DSM API on `localhost:5001` → `scripts/wait-for-dsm.sh` polls until ready (~10-20 min on cold start, seconds on restart with saved state).

**Virtual DSM specifics:**
- Login with empty password (`admin`/`""`)
- No shared folders by default — tests must create them
- No `homes` share — don't reference it in tests
- User quota API returns error 102 — not supported on virtual DSM. The three `dsm_user_quota` acceptance tests skip unless `DSM_ACC_QUOTA=1` is set; run them against real hardware with that env var enabled.
- Container Manager is unavailable on virtual DSM. `dsm_container_project` acceptance tests skip unless `DSM_ACC_CONTAINER_PROJECT=1` and `DSM_ACC_CONTAINER_IMAGE` are set against physical hardware.
- Sessions are short-lived; the client re-authenticates automatically on error 119

**Acceptance tests** (`*_acc_test.go` in repo root):
- `TestAccPreCheck` validates env vars (`TF_ACC`, `SYNOLOGY_DSM_HOST`, `SYNOLOGY_DSM_USERNAME`, `SYNOLOGY_DSM_PASSWORD`)
- `TestAccPreCheckQuota` additionally requires `DSM_ACC_QUOTA=1` (gates the quota tests; `SYNO.Core.Share.Quota` is error 102 on virtual DSM)
- `TestAccPreCheckContainerProject` additionally requires `DSM_ACC_CONTAINER_PROJECT=1` and `DSM_ACC_CONTAINER_IMAGE` (gates project tests that build and run a real container on physical hardware)
- `SYNOLOGY_DSM_PASSWORD` can be empty (supports DSM first-login state)
- Tests use `internal/acctest` package — `ComposeTestResourceConfig()` wraps config with provider block
- Each resource has: basic create, import (two-step: create then import), and data source tests
- Tests that need a shared folder (share_permission, user_quota) create `dsm_shared_folder` as a dependency

**Current virtual-DSM baseline (29 PASS / 0 FAIL / 6 SKIP):**
- PASS: all `*_basic` and `*_import` tests for group, user, shared_folder, share_permission (two-step create→import), plus the data source tests
- PASS: the six `dsm_user` tests — create, update, disabled, expiry date, import, and the data source
- PASS: the six `dsm_shared_folder` tests — create, extended attributes, in-place update, replacement when compression is switched on, import, and the data source
- PASS: the eight `dsm_user_home_service` tests — create, recycle-bin update, import, the `homes` share side effect, both destroy modes (`disable_on_destroy` on and off), the bad-location diagnostic, and the data source
- PASS: the three `dsm_package` tests — adopt/import/data source for the built-in File Station package
- SKIP: `TestAccUserQuota_basic`, `TestAccUserQuota_import`, `TestAccDataSourceUserQuota_basic` — gated behind `DSM_ACC_QUOTA=1`; `SYNO.Core.Share.Quota` is error 102 on virtual DSM, works on real hardware
- SKIP: the `dsm_container_project` tests — gated behind `DSM_ACC_CONTAINER_PROJECT=1` and `DSM_ACC_CONTAINER_IMAGE`; Container Manager requires compatible physical hardware

**Run acceptance tests:**
```bash
export SYNOLOGY_DSM_HOST="http://localhost:5001"
export SYNOLOGY_DSM_USERNAME="admin"
export SYNOLOGY_DSM_PASSWORD=""
TF_ACC=1 go test -v -timeout 30m ./...
```

## Known Issues

- ~~**Test state pollution**~~ — resolved: sweepers in `sweeper_test.go` delete leftovers named `tfacctest*` before a run. Container projects are swept before their backing shared folders; unsupported project APIs are skipped on virtual DSM. `task test-acc` sweeps automatically; `task sweep` runs it standalone. Only the root package registers sweepers, so the command is `go test . -sweep=all` (not `./...`, which fails with "flag provided but not defined").
- ~~**`dsm_user.password` blocks clean import**~~ — resolved: `password` is now `Optional` + `Sensitive`, with `ModifyPlan` requiring it only when the user is actually being created. An imported account can be managed without putting the password in config.
- **Explicit `""` on optional strings** — `nullableString` normalizes empty descriptions/emails to null on Read (fixing the omitted-attribute drift). Setting `description = ""` explicitly still produces a perpetual diff because DSM cannot represent an intentional empty string; see `internal/provider/helpers.go`.
- **Quota untested on hardware** — the quota resource only validates on a real NAS (`DSM_ACC_QUOTA=1`); it is skipped on the virtual DSM.
- **Shared folder fields DSM will not round-trip** — `hide_unreadable` can be written but is never returned by `get`, and `unite_permission` is accepted and ignored. Both are left out of the schema rather than exposed as permanent drift.
- **POSIX mode is reported but cannot be managed** — `dsm_shared_folder`, its data source, and `dsm_file` expose `posix_mode`/`posix_owner`/`posix_group`/`posix_uid`/`posix_gid`/`acl_mode` as computed attributes, filled from File Station `getinfo`. They are read-only because DSM has no API to write them (see the API note above). The practical consequence, reported in issue #94: a shared folder created through DSM lands in ACL mode with mode `000`, which DSM, SMB and File Station honour through the ACL and a Docker bind mount does not — so a container not running as root cannot read the path. Fixing it needs a `chmod` on the NAS. The extra `getinfo` call is best effort: File Station is a package that can be absent, so a failure leaves the attributes null instead of failing the read. Virtual DSM reports 777 everywhere, so the symptom itself can only be reproduced on hardware.
- **Share encryption not implemented** — `encryption` / `encrypt_pwd` plus the `SYNO.Core.Share.Crypto*` family (key management, mount/unmount) need their own design; deliberately out of scope for the extended-attributes work.
- **`personal_photo_enable` is read-only in practice** — `SYNO.Core.User.Home` `set` accepts the parameter and answers `success:true`, but `get` keeps reporting `false` (likely needs the Synology Photos package). It is therefore exposed only on the `dsm_user_home_service` data source, not the resource, to avoid a perpetual diff.
- **User home service needs the built-in `admin`** — other administrator accounts get error 119 from `SYNO.Core.User.Home` even with a valid session.
- **Container projects unverified on hardware** — the wire contract is covered by stateful HTTP tests, but project create/build/start/update/delete acceptance requires compatible physical hardware and a pullable test image.
- **Certificates unverified against a real DSM** — the wire contract is covered by `httptest` and comes from three independent production implementations (acme.sh, python `synology-api`, a captured DSM response in Tailscale's test data), but no call has been made against hardware. The soft spots are: the `import` part ordering (acme.sh's files-first layout is copied verbatim; File Station needs the opposite), whether `CRT.set` tolerates a rename without `as_default`, whether `LetsEncrypt.create` echoes `desc` back into the certificate, and the exact behaviour of the httpd restart after an import. There are no acceptance tests: issuance would spend a real Let's Encrypt rate-limit slot and `SYNO.Core.Certificate` is not expected to work on virtual DSM.
- **The certificate destroy guard trusts `CRT.list` to report service assignments** — it refuses to delete a certificate whose `services` array is non-empty. If some DSM build returns assignments only when asked for them (the way `SYNO.Core.User.get` needs `additional`, in this same project), `services` would arrive empty, the guard would wave the delete through, and the affected services would be left without TLS. Nothing in the captured responses suggests this, and none of the three reference implementations passes an extra parameter here — but it has not been checked on hardware, and it is the one way this guard fails silently rather than loudly. Confirm it on a real NAS by assigning a certificate to a service and checking that `services` is populated in a plain `CRT.list`.
- **`dsm_certificate_lets_encrypt.email` cannot be read back** — DSM does not report the ACME contact address, so it is absent from state after an import and deliberately carries no `RequiresReplace`: an attribute that can never be refreshed must not be able to trigger a destroy. Changing it warns and records the value rather than reissuing.
- **A DSM-side Let's Encrypt renewal may replace the certificate object** rather than update it, invalidating the id in state. `dsm_certificate_lets_encrypt`'s Read falls back to matching the subject common name and adopts the new id, because dropping the resource would make Terraform request another certificate against the rate limit.
- **`dsm_file` unverified against a real DSM** — the multipart upload, the raw download, and the getinfo/delete contracts are covered by `httptest`, and `file_acc_test.go` should run on virtual DSM (File Station is present there), but no acceptance run has happened yet. The wire details came from Synology's File Station API guide, so the per-code hints in `fileErrorDetail` live in the provider layer rather than the client's verified error table.
- **`dsm_system_settings` write path is inferred, not verified** — only `SYNO.Core.Region.NTP` `get` has been captured from hardware (issue #57), plus the negative result that a `timezone`-only `set` answers 5701. The parameter *names* (`timezone`/`enable_ntp`/`server`) come from the DSM library symbol table and the `get` payload; the requirement that all three travel together, the GET/POST verb, and the `enable_ntp` value for the disabled state are all inferences. Reads are ungated; the write acceptance tests skip unless `DSM_ACC_SYSTEM_SETTINGS=1`. Setting `ntp_enabled = false` on a NAS that currently has NTP on emits a Terraform warning because the value is a guess. `SYNO.Core.Region.NTP` also exposes `setzone`/`sync`/`status`/`ensure_ntp_sync_and_enable`; `setzone` is likely the clean timezone-only path but its parameters are undocumented and it is not used.
- **Reverse proxy unverified on hardware** — the wire contract is reconstructed from third-party DSM 7.x captures and covered by unit tests that assert the exact payload, but nothing here has talked to a real DSM. `frontend.https.http2` is the weakest link: the key name comes from Synology's own header, its nesting under `https` is deduction. Acceptance tests are gated behind `DSM_ACC_REVERSE_PROXY=1` because they rewrite live nginx configuration.
- **Reverse proxy certificate *binding* and access control profiles are out of scope** — `dsm_certificate`/`dsm_certificate_lets_encrypt` can install the certificate an HTTPS source needs, but attaching it to a specific reverse proxy entry is `SYNO.Core.Certificate.Service`, which is not implemented; only the DSM-wide default is settable. `access_control_profile` references a profile that must already exist.
- **Firewall rules unverified on hardware, and there are no acceptance tests** — `dsm_firewall_rule` is covered only by stateful HTTP tests. An acceptance test would have to enable a real firewall on the box running the test suite, which is exactly the thing the resource's own guards exist to prevent; it needs a NAS reachable by console before it can be written safely. Two specifics still to confirm on hardware: whether `Firewall.Profile get` returns the profile bare or nested under a `profile` key (both are accepted), and whether `adapterPolicyMap` keeps that name over HTTP as it does on disk.
- **Lockout guard sees the local source address** — it uses the local end of the TCP connection to DSM. Behind NAT, DSM sees a different address and the guard can be wrong in both directions. It also cannot replay GeoIP or service-preset rules; those make the check inconclusive, which warns rather than blocks.
- **Task Scheduler wire contract unverified on hardware** — `SYNO.Core.TaskScheduler` and `SYNO.Core.EventScheduler` are undocumented. The encoding was transcribed from three agreeing implementations (python `synology-api`, `synology-community/go-synology`, 007revad's `task_setup.sh`, the last with real DS218/DSM 7 findings) and is covered by request-asserting httptest cases, but nothing has been run against a NAS from this repository. The parts that are inference rather than transcription are flagged `INFERRED` in the client: `limit=-1` on `list`, and the assumption that event tasks are the list entries carrying no `id`. `SYNO.Core.EventScheduler.get` in particular has no captured real response anywhere, so `parseEventTask` accepts both `task_name`/`name` and `operation`/`script`. There are no acceptance tests for either resource yet.
- **One-shot scheduled tasks are not modelled** — DSM's `date_type: 1` schedules (a single date, yearly, every 3/6 months) are deliberately unsupported: a fixed past date is not an ongoing desired state. Reading such a task yields an empty `Frequency`, which the resource reports as an error and the data source surfaces as null rather than guessing.

## Roadmap

Remaining gaps from `.pi/audit-scenario-gap.md`: a `dsm_group_member` resource for atomic membership, per-share NFS rules (`SYNO.Core.FileServ.NFS.SharePrivilege`) and the global protocol services (`SYNO.Core.FileServ.SMB`/`NFS`/`FTP`), and share encryption. Reverse proxy is implemented (`dsm_reverse_proxy`); access control profiles themselves (`SYNO.Core.AppPortal.AccessControl` create/update/delete) are still read-only here. Certificates are implemented (`dsm_certificate`, `dsm_certificate_lets_encrypt`); per-service certificate binding (`SYNO.Core.Certificate.Service`) is not. Then: Synology Drive → Photos.
