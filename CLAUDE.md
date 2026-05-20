# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Development Commands

Uses [Task](https://taskfile.dev) (not Makefile):

```
task build       # Build binary to bin/
task test        # Unit tests (go test -v -count=1 ./...)
task test-acc    # Acceptance tests (TF_ACC=1, requires real DSM)
task lint        # go vet ./...
task install     # Build + install to ~/.terraform.d/plugins/ for local testing
task clean       # Remove bin/ and test cache
```

Run a single test: `go test -v -run TestClient_CreateUser ./internal/client/`

## Architecture

Terraform provider for Synology DSM using Plugin Framework. Two layers:

**`internal/client/`** — Synology DSM HTTP API client
- `client.go`: Auth (SYNO.API.Auth v7), session management (SID + SynoToken), `DoAPI()` for GET requests, `DoAPIPost()` for POST requests, retry with exponential backoff
- `user.go`, `group.go`, `share.go`: CRUD methods per resource, call `client.DoAPI()` or `client.DoAPIPost()`

**`internal/provider/`** — Terraform Plugin Framework wiring
- `provider.go`: Provider schema (host/username/password/insecure), Configure creates client and logs in
- `resource_*.go`: Resources — full CRUD + ImportState
- `datasource_*.go`: Data sources — Read only

Flow: `main.go` → `provider.New()` → `Configure()` creates `client.NewClient()` + `Login()` → resources get `*client.Client` via `ProviderData`

## Critical Synology DSM API Details

- **Developed against DSM 7.3.2** on RS4021xs+ — API behavior may differ on DSM 6.x

- **Most APIs use GET** — user/group operations send params as query string
- **Shared folder uses POST** — `SYNO.Core.Share` create/update send `shareinfo` as form-encoded POST body
- **SynoToken required** — CSRF token from login response, passed as query param in every request
- **Auth version 7** — `SYNO.API.Auth` version 7 with `enable_syno_token=yes`
- **Session via `_sid`** — Login returns SID, passed as `_sid` query param (no cookies needed with `format=sid`)
- Error 105 = "session does not have permission" — usually means wrong HTTP method or missing SynoToken

## Client patterns

- **Simple resources** (user, group): all CRUD via `DoAPI()` (GET). Delete sends name as JSON array.
- **Shared folder**: create/update via `DoAPIPost()` (POST) with `shareinfo` JSON. Update includes `name_org` so DSM recognizes it as update. Get/list/delete via `DoAPI()` (GET).
- **parseX()** helpers use `map[string]interface{}` type assertions, not typed structs — matches the loose DSM API responses.

## Resource implementation pattern

Every resource follows the same structure (see `resource_group.go` as the cleanest reference):

1. `XxxResource` struct with `client *client.Client`
2. `xxxResourceModel` struct with `types.String`/`types.Bool`/etc fields
3. Schema: `id` (computed), required fields with `RequiresReplace` for immutable attrs, optional fields with defaults via `booldefault.StaticBool()`
4. **Read must set ALL state fields** from API response (including `ID` and `Name`) — required for import to work
5. Read uses `state.ID` with fallback to `state.Name` for lookup after import
6. Import: `resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)`

## Conventions

- Provider name: `dsm` (source: `batonogov/dsm`)
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

Automated via **Release Please + GoReleaser**:

1. All commits to `main` must use **conventional commits** (`feat:`, `fix:`, `docs:`, `ci:`, `deps:`, `breaking:`)
2. Release Please automatically creates/updates a release PR with changelog and version bump
3. Merging the release PR creates a GitHub Release + git tag
4. GoReleaser picks up the release event, builds binaries for all platforms, and uploads assets

```
conventional commits → Release Please PR → merge → GitHub Release → GoReleaser → binaries
```

- **Never create tags manually** — Release Please manages versions
- **Never skip conventional commits** — changelog and versioning depend on them
- Dependabot keeps Go modules and GitHub Actions up to date (weekly, `deps:` / `ci:` prefix)

## Acceptance Test Environment

Tests run against a **virtual DSM** (QEMU via Lima VM + Docker):

```
task test-env-up      # Start Lima VM + virtual-dsm container
task test-env-down    # Stop everything
task test-env-status  # Check status
```

**Setup:** Lima VM (`.lima/dsm.yaml`, aarch64 QEMU) → Docker inside VM runs `vdsm/virtual-dsm` container (`docker-compose.test.yml`) → DSM API on `localhost:5001` → `scripts/wait-for-dsm.sh` polls until ready (~10-20 min, QEMU emulation is slow).

**Acceptance tests** (`*_acc_test.go` in repo root):
- `TestAccPreCheck` validates env vars (`TF_ACC`, `SYNOLOGY_DSM_HOST`, `SYNOLOGY_DSM_USERNAME`, `SYNOLOGY_DSM_PASSWORD`)
- `SYNOLOGY_DSM_PASSWORD` can be empty (supports DSM first-login state)
- Resource tests are currently skipped (`t.Skip`) — virtual DSM in first-login state blocks write APIs (error 3103)
- Only data source tests are active: `TestAccDataSourceGroup_basic` (reads "administrators"), `TestAccDataSourceUser_basic` (reads "admin")

**Run acceptance tests:**
```bash
export SYNOLOGY_DSM_HOST="http://localhost:5001"
export SYNOLOGY_DSM_USERNAME="admin"
export SYNOLOGY_DSM_PASSWORD=""
TF_ACC=1 go test -v -timeout 30m ./...
```

## Roadmap

`dsm_share_permission` → `dsm_user_quota` → Synology Drive → Photos
