# Cloudflare homelab dynamic DNS

A small Go service with a local dashboard with no sign-in, IPv4 and optional service-host IPv6 discovery, and durable per-record applied state and history. No external Go dependencies.

## How updates work

Every five minutes (configurable), discover the current address once per configured address family. Compare it with the **persisted last successfully applied address for each zone / hostname / type**. Unchanged successful targets make **zero Cloudflare API requests**, including after restart. Failed targets stay pending and retry on the next check even when the discovered IP is unchanged. Each record succeeds independently.

On first startup, IP change, new target, or retry, look up the exact hostname and type, reject duplicates, then PATCH only `content` when necessary. A matching Cloudflare address initializes applied state without a write. Zone ID, hostname or type changes create new target identities. Removed targets never get updated and their applied markers are pruned. Changes to discovery source take effect on the next discovery. Creation settings affect only missing records; they do not change existing records.

There are **no periodic Cloudflare drift checks**. External DNS edits are not detected while the discovered IP remains unchanged. Use Force Update to correct drift, or remove the applied state intentionally to reinitialize. Force Update always discovers fresh addresses, looks up every target and PATCHes existing records even if unchanged. Manual and scheduled checks cannot overlap; a busy dashboard request returns HTTP 409. A scheduled check that encounters a manual check waits until its next interval.

The dashboard shows the public IPv4 address (without an IPv6 summary card), configured targets, progress and individual results, last successful check, next scheduled check, errors, and the latest 1,000 changes / forced updates / dry-run previews / failures. Unchanged scheduled checks do not add history. Timestamps are stored in UTC and displayed in the browser's local timezone.

## Install with Docker Compose

1. Copy `config.example.json` to `config.json`. Enter your Cloudflare zone ID and fully qualified hostnames. Create the A records in Cloudflare beforehand. A and AAAA with the same hostname are separate targets.
2. Create a Cloudflare API token with **Zone → DNS → Edit**, restricted to the specific zone. Do not use a global API key. See [Cloudflare token setup](https://developers.cloudflare.com/fundamentals/api/get-started/create-token/).
3. Create a local `secrets` directory. Store the token as a single line in `secrets/cloudflare_api_token`. The token file is gitignored and excluded from image builds. Restrict host permissions; the container UID 65532 must be able to read the mounted secrets and configuration. Compose file-backed secrets are mounts, not an encrypted secret vault.
4. Build and preview against your real configuration without DNS writes:

   ```sh
   docker compose build
   docker compose run --rm ddns -run-once -dry-run
   ```

5. Start the updater when ready:

   ```sh
   docker compose up -d
   docker compose logs -f ddns
   ```

6. Open [the dashboard](http://localhost:8080) directly; no username or password is required. Compose publishes the port only on loopback. Keep this binding for local access. For access from another machine, use an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 user@docker-host`). The dashboard has no authentication, so do not publish it directly to your LAN or the internet.

The image runs as UID/GID 65532, has no shell, and uses a read-only root filesystem in Compose. Only the named `/state` volume is writable. A newly created named volume inherits its ownership from the image. Existing or bind-mounted state directories must be writable by UID 65532. Keep exactly one updater process per state volume; state is not a multi-process database.

## Configuration

JSON is strict: unknown fields and invalid records fail startup. Native execution defaults to `127.0.0.1:8080`. The Docker example explicitly uses `:8080` inside the container; Compose limits the published host port to loopback. Legacy `dashboard_username` and `dashboard_password_file` settings are accepted but ignored, so existing configuration files continue to load without a password file. Remove those settings and the old dashboard password secret mount when updating an existing deployment. `interval` must be at least one second; normal use should keep the five-minute default. `request_timeout` is a per-request timeout between 1s and 1m. API GET/PATCH transport errors, HTTP 429 and HTTP 5xx retry at most three times with exponential backoff. In-cycle waits are capped at 30s; a longer HTTP 429 `Retry-After` defers all Cloudflare work until that cooldown expires, then pending targets retry on the next scheduled cycle or force action. Creation POSTs retry only HTTP 429 to avoid creating duplicates after an uncertain response; the next cycle resolves the record again. Other API failures do not retry within the cycle. IP discovery failures retry on the next cycle.

IPv4 defaults to `https://api.ipify.org` when A records are configured and no source is set. Any URL source must return **one plain-text IP address**, not JSON. URL sources can use HTTPS or trusted local HTTP. Discovery redirects are rejected. Private, loopback, link-local, multicast, mapped IPv4-in-IPv6 and wrong-family addresses are rejected. The Cloudflare API token is sent only to the fixed Cloudflare API base URL, never to address discovery sources; API redirects are also rejected.

For IPv6, configure a source that represents the **actual service host**, with a stable global address. A container's outbound IPv6 may differ from the host or service address, especially with Docker bridge networks and privacy addresses. There is deliberately no automatic IPv6 endpoint default.

For a host-maintained plain-text address file:

```json
"ipv6": { "file": "/sources/service-ipv6" }
```

Add an AAAA target to `records` and uncomment the file mount in `compose.yaml`. Refresh the mounted file on the host when its address changes. For atomically replaced files, mount their **parent directory** read-only instead of an individual file, so the container sees the new inode. Alternatively use a trusted router / service-host HTTP endpoint returning the address:

```json
"ipv6": { "url": "http://your-router-or-service-host/ipv6-address" }
```

If IPv6 discovery fails, AAAA targets fail without writes and A targets continue. The dashboard and health check report the overall failure. You can also configure `ipv4.file` instead of a URL, for router-maintained IPv4 discovery.

To opt in to missing-record creation for a specific target:

```json
{ "name": "new.example.com", "type": "A", "create_if_missing": true, "proxied": false, "ttl": 1 }
```

`ttl` and `proxied` apply only on creation (defaults: automatic TTL and DNS-only). Existing TTL, proxy state, comments, tags and other properties are preserved by the [content-only PATCH](https://developers.cloudflare.com/api/resources/dns/subresources/records/methods/edit/). Exact hostname/type lookup uses the [DNS list API](https://developers.cloudflare.com/api/resources/dns/subresources/records/methods/list/), including pagination and duplicate detection.

## State, health and operations

`/state/state.json` atomically stores applied state and bounded history together, after confirmed API success. Failed records lose their applied marker so they remain pending. The file is flushed before replacement; a crash after a remote write but before local persistence is safe: the next cycle looks up the actual record again. Back up the named volume. `docker compose down` preserves it; `docker compose down -v` removes applied state and history. Corrupt state fails startup rather than silently overwriting history. Stop the service and restore the file from backup or move it aside to intentionally start fresh.

Health is based on this process having completed a successful check within two polling intervals plus one minute, with no current cycle error. The unauthenticated `/healthz` endpoint exposes only `ok` or `unhealthy`. Previous-process success is not used for startup health. Compose reports unhealthy status but Docker does not automatically restart an unhealthy running container; the restart policy handles process exits. A complete success includes successful unchanged-address comparisons. Dry-run mode can also report healthy after a successful preview.

`-run-once` exits 0 on success and 1 if any record/discovery/state write fails. `-dry-run` does not modify DNS or advance applied state, but records previews/failures in history. `-config /path/config.json` selects a different configuration. Restart the service after changing config or secrets (`docker compose restart ddns`). The Cloudflare API token is still required, read at startup, and never returned to the browser. Dashboard writes require a random CSRF header and matching Origin when supplied.

Troubleshooting:

- **401 / 403:** check token validity, zone scope and DNS Edit permission. API response bodies are intentionally excluded from logs to avoid exposing sensitive values.
- **Missing record:** check the hostname/type and zone ID, create it in Cloudflare, or explicitly enable creation.
- **Ambiguous record:** remove duplicate hostname/type records before retrying. No guessed record is edited.
- **Invalid public IP:** check the source response format and network. A public IP does not solve CGNAT; inbound services also need appropriate routing/firewall configuration.
- **IPv6 unavailable:** check the host/router source and global address; avoid publishing privacy or container-only addresses.
- **State permission failure:** ensure `/state` is writable by UID/GID 65532. Checks remain unhealthy and pending if persistence fails.
- **Unexpected external DNS edit:** use Force Update; scheduled checks intentionally make no drift queries.
- **429 / temporary failure:** bounded retries happen within the cycle, then failed targets retry at the next interval. Results remain visible in history.

## Build and test

Requires Go 1.23 or later (Docker builder uses Go 1.27). Tests use local mock servers and temporary files; no real Cloudflare credentials or changes are involved.

```sh
go test ./...
go test -race ./...
go vet ./...
go build .
docker build --target race-test -t cloudflare-ddns-race-test .
docker buildx build --platform linux/amd64,linux/arm64 -t your-registry/cloudflare-ddns:latest --push .
```

The multi-stage build runs tests on the build platform and produces a static binary for the target platform. Both amd64 and arm64 are supported. Tests cover changed IPs, zero API calls on unchanged scheduled checks and restart, target changes, partial failure retry across restart, content-only PATCH, opt-in creation, duplicate/missing records, credentials failures, dry runs, absent/invalid IPv6, canonical IPv6 comparisons, throttling, bounded retries, persistent history, corrupt/unwritable state, manual serialization, direct dashboard access/CSRF/origin, force behavior and health freshness.

On Windows, `./scripts/smoke.ps1` exercises the production image with mock secrets and an invalid local discovery source, verifying non-root/read-only startup without a password file, direct access, force progress, health failure and persisted history across container restart. It makes no Cloudflare calls and cleans up its test container, volume and fixtures. Pass `-Platform linux/arm64` after loading a multi-platform image to smoke-test arm64 under Docker Desktop emulation. The Docker `race-test` build target supplies the C compiler required for race tests if local Go has CGO disabled or no compiler.
