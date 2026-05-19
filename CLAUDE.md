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
- `client.go`: Auth (SYNO.API.Auth v7), session management (SID + SynoToken), GET-based request execution with retry
- `user.go`: User CRUD methods, calls `client.DoAPI()`
- All requests are **GET** to `/webapi/entry.cgi?...` with params in query string

**`internal/provider/`** — Terraform Plugin Framework wiring
- `provider.go`: Provider schema (host/username/password/insecure), Configure creates client and logs in
- `resource_user.go`: `dsm_user` resource — full CRUD + ImportState

Flow: `main.go` → `provider.New()` → `Configure()` creates `client.NewClient()` + `Login()` → resources get `*client.Client` via `ProviderData`

## Critical Synology DSM API Details

- **Developed against DSM 7.3.2** on RS4021xs+ — API behavior may differ on DSM 6.x

- **GET requests only** — DSM rejects POST for write operations (returns error 105)
- **SynoToken required** — CSRF token from login response, passed as query param in every request
- **Auth version 7** — `SYNO.API.Auth` version 7 with `enable_syno_token=yes`
- **Session via `_sid`** — Login returns SID, passed as `_sid` query param (no cookies needed with `format=sid`)
- Error 105 = "session does not have permission" — usually means wrong HTTP method or missing SynoToken

## Conventions

- Provider name: `dsm` (source: `batonogov/dsm`)
- Resource naming: `dsm_user`, `dsm_group`, etc.
- Provider env vars: `SYNOLOGY_DSM_HOST`, `SYNOLOGY_DSM_USERNAME`, `SYNOLOGY_DSM_PASSWORD`
- Tests use `httptest.NewServer` with `r.URL.Query().Get()` (matching GET-based client)
- Go 1.26, Terraform Plugin Framework v1.19.0, no third-party HTTP libs
- Repository language: Russian for docs/comments, English for code

## Adding New Resources

1. Add API methods in `internal/client/<resource>.go` (follow `user.go` pattern)
2. Create resource in `internal/provider/resource_<name>.go` (follow `resource_user.go`)
3. Register in `provider.go` → `Resources()` return list
4. Add unit tests in `internal/client/<resource>_test.go` with httptest mock
5. Update README.md features table

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

## CI/CD

- `.github/workflows/test.yml` — tests on push/PR
- `.github/workflows/release-please.yml` — release PR automation on push to main
- `.github/workflows/release.yml` — GoReleaser on GitHub Release event
- `.github/dependabot.yml` — weekly dependency updates (gomod + github-actions)

## Roadmap

`dsm_group` → `dsm_shared_folder` → `dsm_share_permission` → `dsm_user_quota` → Synology Drive → Photos
