# Saved project and active deployment fix — 16 October 2026

The initial changes existed only in the Codex worktree. The active Docker Compose container used the unchanged saved project in OneDrive/Documents/ChatGPT/CloudFlare DNS Updater. Applied the tested changes to that clean checkout, preserving config.json, API token files, and the existing state volume.

- Saved-project Go tests and go vet passed.
- Docker Compose image rebuild passed, including Go tests inside the Linux build stage.
- Updated production-image smoke script passed with mock credentials and an invalid discovery IP; no Cloudflare calls.
- Recreated the actual local Compose container with the rebuilt image. Before restart, read-only IP discovery matched every configured target in persisted applied state.
- GET http://127.0.0.1:8080/ returned HTTP 200 without credentials or a WWW-Authenticate challenge. The IPv6 card and its JavaScript reference were absent. GET /api/status also worked without credentials.
- The startup scheduled cycle completed with an unchanged result for the configured record and unchanged history, skipping Cloudflare requests. No manual Force Update was issued against the live app.
- Existing config and secrets remain intact; the old dashboard password settings are ignored, and the password secret is no longer mounted by Compose.

# Local access changes — 16 October 2026

Verified with Go 1.27.2 on Windows:

- `go test ./...` — passed outside the sandbox, which blocks loopback connections to mock servers.
- `go vet ./...`, `go build -o dist/ddns.exe .`, and `git diff --check` — passed.
- Isolated binary smoke test — passed: startup with a nonexistent legacy password file; dashboard/status access without credentials; Force Update accepted with CSRF and rejected without it; IPv6 card and its JavaScript reference absent; API token excluded from browser responses; state persisted. Mock credentials and a rejected loopback discovery address prevented all Cloudflare requests. The process and temporary fixtures were cleaned up.
- Go coverage still verifies DNS updates, unchanged-address skipping, restart state, retries, backend IPv6 support, origin checks, health and manual progress. New coverage checks configuration without dashboard credentials and compatibility with ignored legacy auth settings.
- Docker runtime and race checks were not rerun: the Docker Desktop Linux engine pipe is unavailable, and local Go has CGO disabled. The updated Docker smoke script checks direct access, absent IPv6 card and CSRF protection without provisioning a password.

The earlier results below describe the previous authenticated version.

# Validation — 14 October 2026

Verified locally with Go 1.27.2 and Docker Desktop 4.94.0 (Docker Engine 29.8.2, Linux containers).

- `go test ./...` — passed, using mock Cloudflare/discovery servers and temporary state.
- `go vet ./...` — passed.
- Docker `race-test` target (`CGO_ENABLED=1 go test -race ./...`) — passed. Local Windows Go had CGO disabled, so the Docker test stage supplied a compiler.
- Multi-platform Docker build — passed for `linux/amd64` and `linux/arm64`; loaded locally as `homelab-cloudflare-ddns:local`.
- Production-image smoke tests — passed on amd64 and arm64 (arm64 under Docker Desktop emulation). Verified non-root/read-only operation, dashboard authentication, CSRF-protected force actions, invalid discovery handling, persistent volume writes and history after restart. These runtime tests deliberately used mock secrets and a rejected local IP, preventing Cloudflare requests. Successful update behavior is exercised by the mocked Go suite inside Docker.

Key assertions include zero Cloudflare requests for unchanged scheduled checks, zero requests after restart with successful applied state, changed IP updates with content-only PATCH, independent retry of failed targets across restart, explicit record creation, absent IPv6, duplicate records, credentials failures, rate-limit cooldowns, uncertain creation responses, corrupt/unwritable state, serialization, dashboard security and health freshness.

## Docker connection diagnosis

The initial sandboxed `docker version` returned `permission denied while trying to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine`. The selected context was already `desktop-linux` and pointed to the expected Docker Desktop Linux engine pipe.

The same read-only command with approved access outside the sandbox returned both client and server details. Builds and runtime tests then succeeded through that access. The denial came from the execution sandbox's named-pipe access, not a broken Docker engine or incorrect Docker Desktop setting.

**No Docker Desktop configuration changes, service restarts, reinstalls or resets were required.** Test containers, volumes and mock fixtures were removed after smoke tests. Locally built images and build cache remain available. Nothing was pushed to a registry, deployed to the homelab, or changed in live Cloudflare DNS.
