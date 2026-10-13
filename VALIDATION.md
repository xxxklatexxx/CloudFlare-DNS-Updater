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
