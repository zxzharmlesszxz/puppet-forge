# Puppet Forge Service

[Українська версія](./README.uk.md)

Go service for running an internal Puppet Forge-compatible module registry.

It provides:

- publishing Puppet modules and versions;
- team-owned publishing spaces;
- token and OIDC access for teams;
- proxy/cache support for the official Puppet Forge API;
- artifact storage in GCS or S3-compatible object storage;
- SQL metadata storage with PostgreSQL or SQLite;
- Kubernetes deployment through Helm;
- Prometheus metrics and Grafana dashboards.

## Features

Implemented:

- `POST /api/v1/modules` uploads a new module version;
- `GET /api/v1/modules?limit=20&offset=0` lists modules and returns `items`, `limit`, `offset`, and `total`;
- `GET /api/v1/modules/{owner}/{name}` returns a module card;
- `DELETE /api/v1/modules/{owner}/{name}` deletes a module through global-admin or team-admin access;
- `GET /api/v1/modules/{owner}/{name}/versions/{version}` returns a specific release;
- `DELETE /api/v1/modules/{owner}/{name}/versions/{version}` deletes a specific release through global-admin or team-admin access;
- `GET /api/v1/modules/{owner}/{name}/versions/{version}/download` redirects to the stored artifact;
- `GET /` renders the public HTML module index;
- `GET /modules/{owner}/{name}` renders a module page with README Markdown, version selector, and install snippets;
- `GET /v3/*` and `HEAD /v3/*` reverse-proxy the official Puppet Forge API;
- `/manage` provides the module management UI for publishing, deleting, and importing upstream modules;
- `/manage/access` provides DB-backed access configuration;
- `ADMIN_TOKEN` provides bootstrap/break-glass access;
- OIDC login supports global admins, team admins, and OIDC groups that grant publishing rights;
- `PUBLIC_MODULE_ACCESS` controls anonymous machine-readable install/API access;
- active release tracking blocks backend deletion of latest/in-use releases and hides unsafe delete actions in `/manage`;
- `GET /healthz` and `GET /readyz` expose health probes.

Not implemented:

- advanced search with filters, ratings, dependencies, and verified publishers;
- full compatibility with every official Puppet Forge API behavior;
- deep Puppet module archive validation;
- CDN or signed URLs with TTL.

## Architecture

- HTTP API: standard `net/http`;
- Service layer: validation, upload orchestration, metadata updates;
- Artifact storage: GCS or S3-compatible object storage;
- Metadata store: PostgreSQL or SQLite selected by `DATABASE_DSN`;
- Upstream proxy: reverse proxy to the official Forge API with an in-memory TTL cache for JSON GET/HEAD responses.

Publish flow:

1. A client sends a multipart request containing a `.tar.gz` module artifact.
2. The service validates `owner`, `name`, and `version`.
3. The artifact is uploaded under `modules/<owner>/<name>/<version>/`.
4. Release metadata is written to the selected SQL backend.
5. The API returns the created release description.

Proxy flow:

1. A client calls `/v3/...` on this service.
2. The service proxies the request to the official Forge API.
3. JSON responses are cached in memory for `UPSTREAM_PROXY_JSON_CACHE_TTL`.
4. File downloads through `/v3/files/...` are cached under `upstream-cache/` on first use, then served from object storage by this service.

## Configuration

See [`.env.example`](./.env.example).

Runtime parameters can be passed either through environment variables or command-line flags. Environment variables are still supported, and command-line flags override environment values when both are set.

The binary prints its build version and exits with:

```bash
puppet-forge --version
```

Flag names are the lowercase kebab-case form of the environment variable name:

```text
DATABASE_DSN -> --database-dsn
MANAGE_SESSION_SECRET -> --manage-session-secret
PUBLIC_MODULE_ACCESS -> --public-module-access
UPSTREAM_PROXY_JSON_CACHE_TTL -> --upstream-proxy-json-cache-ttl
UPSTREAM_PROXY_JSON_STALE_TTL -> --upstream-proxy-json-stale-ttl
```

Example:

```bash
puppet-forge \
  --database-dsn "postgres://forge:forge@postgres:5432/forge?sslmode=disable" \
  --admin-token "replace-me-bootstrap-token" \
  --artifact-backend s3 \
  --artifact-endpoint "http://minio:9000" \
  --artifact-bucket "puppet-forge-artifacts" \
  --public-module-access=false
```

## Environment Variables

| Variable                        | Default                           | Required                       | Description                                                                                                                                                                                                                                  |
|---------------------------------|-----------------------------------|--------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `APP_ENV`                       | `dev`                             | no                             | Runtime environment name for logs and diagnostics.                                                                                                                                                                                           |
| `HTTP_ADDR`                     | `:8080`                           | no                             | HTTP listen address inside the process or container.                                                                                                                                                                                         |
| `READ_TIMEOUT`                  | `10s`                             | no                             | Maximum time to read an HTTP request. Uses Go duration syntax, for example `10s` or `1m`.                                                                                                                                                    |
| `WRITE_TIMEOUT`                 | `30s`                             | no                             | Maximum time to write an HTTP response.                                                                                                                                                                                                      |
| `SHUTDOWN_TIMEOUT`              | `10s`                             | no                             | Graceful shutdown timeout.                                                                                                                                                                                                                   |
| `DATABASE_DSN`                  | empty                             | yes                            | Metadata store DSN. `postgres://...` enables PostgreSQL, `sqlite:///path/file.db` enables SQLite.                                                                                                                                            |
| `ADMIN_TOKEN`                   | empty                             | when DB access config is empty | Runtime bootstrap/break-glass admin token. It is not stored in the database and is used to bootstrap `/manage/access`.                                                                                                                       |
| `MANAGE_SESSION_SECRET`         | empty                             | no                             | Shared secret for encrypted `/manage` token sessions. Set it for multi-replica deployments. If empty, the service falls back to `OIDC_COOKIE_SECRET`, then `ADMIN_TOKEN`, then a per-process random secret.                                  |
| `ARTIFACT_BACKEND`              | `gcs`                             | no                             | Artifact backend for module tarballs: `gcs` or `s3`.                                                                                                                                                                                         |
| `ARTIFACT_ENDPOINT`             | `https://storage.googleapis.com`  | for `s3`; optional for `gcs`   | Object storage endpoint. For `gcs`, this may point to an emulator/custom host. For `s3`, this is the S3-compatible endpoint.                                                                                                                 |
| `ARTIFACT_BUCKET`               | empty                             | yes                            | Bucket/container for module tarballs and upstream artifact cache.                                                                                                                                                                            |
| `ARTIFACT_PROJECT`              | empty                             | for `gcs`                      | GCP project used for GCS bucket operations. Not used by `s3`.                                                                                                                                                                                |
| `ARTIFACT_PREFIX`               | `modules`                         | no                             | Bucket prefix for locally published modules.                                                                                                                                                                                                 |
| `ARTIFACT_REGION`               | `us-east-1`                       | no                             | Region used by the S3-compatible client.                                                                                                                                                                                                     |
| `ARTIFACT_ACCESS_KEY_ID`        | empty                             | for private `s3`               | Access key for S3-compatible storage.                                                                                                                                                                                                        |
| `ARTIFACT_SECRET_ACCESS_KEY`    | empty                             | for private `s3`               | Secret key for S3-compatible storage.                                                                                                                                                                                                        |
| `ARTIFACT_PATH_STYLE`           | `true`                            | no                             | Enables path-style S3 URLs. Useful for MinIO, GCS interoperability, and local endpoints.                                                                                                                                                     |
| `PUBLIC_BASE_URL`               | empty                             | no                             | Optional fallback for building absolute URLs. If empty, the service derives URLs from `Host`, `X-Forwarded-*`, or RFC `Forwarded` headers on the current request.                                                                            |
| `PUBLIC_MODULE_ACCESS`          | `false`                           | no                             | If `true`, read APIs, downloads, and `/v3/*` are open without a token. If `false`, install/API routes require a read/publish/admin token. HTML catalog pages stay informationally public. Publish/delete/manage routes are always protected. |
| `ACTIVE_RELEASE_TTL`            | `720h`                            | no                             | How long a release is considered active/in use after an r10k or `puppet module install` request. Active/latest versions cannot be deleted through API or `/manage`.                                                                          |
| `SECURITY_HSTS_ENABLED`         | `false`                           | no                             | Enables the `Strict-Transport-Security` response header. Keep disabled for local HTTP and enable only when the public endpoint is always HTTPS.                                                                                              |
| `WEB_AUTH_MODE`                 | `none`                            | no                             | Web auth mode: `none` or `oidc`. API token auth is independent from this setting.                                                                                                                                                            |
| `OIDC_ISSUER_URL`               | empty                             | for `WEB_AUTH_MODE=oidc`       | OIDC issuer discovery URL, for example an Authentik application provider URL.                                                                                                                                                                |
| `OIDC_CLIENT_ID`                | empty                             | for `WEB_AUTH_MODE=oidc`       | OIDC client ID.                                                                                                                                                                                                                              |
| `OIDC_CLIENT_SECRET`            | empty                             | for `WEB_AUTH_MODE=oidc`       | OIDC client secret.                                                                                                                                                                                                                          |
| `OIDC_REDIRECT_URL`             | empty                             | no                             | Explicit callback URL. If empty, the callback URL is built from the current request base URL as `/auth/callback`; this is recommended for multi-ingress deployments.                                                                         |
| `OIDC_LOGOUT_URL`               | auto-discovery/empty              | no                             | Provider end-session URL. If unset, the service tries to read `end_session_endpoint` from OIDC discovery.                                                                                                                                    |
| `OIDC_COOKIE_SECRET`            | empty                             | for `WEB_AUTH_MODE=oidc`       | Secret used to sign web session/state cookies. Must remain stable across pod restarts.                                                                                                                                                       |
| `UPSTREAM_URL`                  | `https://forgeapi.puppetlabs.com` | no                             | Upstream Puppet Forge API used when a module is not available locally.                                                                                                                                                                       |
| `UPSTREAM_PROXY_JSON_CACHE_TTL` | `5m`                              | no                             | In-memory cache TTL for upstream JSON GET/HEAD responses such as `/v3/modules/...` and `/v3/releases/...`. It does not control object-storage tarball caching for `/v3/files/...`. `0s` effectively disables the JSON response cache.        |
| `UPSTREAM_PROXY_JSON_STALE_TTL` | `1h`                              | no                             | Maximum time after `UPSTREAM_PROXY_JSON_CACHE_TTL` during which stale JSON may be served if upstream Forge fails or is unavailable. `0s` disables stale fallback.                                                                            |
| `FORGE_CACHE_MAX_BODY_BYTES`    | `1048576`                         | no                             | Maximum upstream JSON response body size that may be stored in the in-memory proxy cache.                                                                                                                                                    |
| `MODULE_UPLOAD_MAX_BYTES`       | `134217728`                       | no                             | Maximum publish upload request size in bytes. Oversized requests return `413 Request Entity Too Large` and are not fully read into memory.                                                                                                   |
| `UPSTREAM_ARTIFACT_MAX_BYTES`   | `134217728`                       | no                             | Maximum upstream tarball size accepted from `/v3/files/...`, both for object-storage cache and bypass mode. Oversized artifacts return `413 Request Entity Too Large`.                                                                       |
| `UPSTREAM_SYNC_INTERVAL`        | `0s`                              | no                             | Background refresh interval for already cached upstream modules. `0s` disables background refresh.                                                                                                                                           |
| `UPSTREAM_SYNC_LIMIT`           | `1000`                            | no                             | Maximum number of upstream modules processed by one refresh cycle.                                                                                                                                                                           |
| `METRICS_MODULE_LIMIT`          | `10000`                           | no                             | Maximum number of modules exported by inventory metrics during one collection pass.                                                                                                                                                          |

## Artifact Storage

- `ARTIFACT_BACKEND=gcs` uses Google Cloud Storage.
- `ARTIFACT_ENDPOINT` defaults to `https://storage.googleapis.com`; override it for a custom GCS host, emulator, or S3-compatible endpoint.
- `ARTIFACT_BUCKET` sets the bucket for artifacts.
- `ARTIFACT_PROJECT` sets the GCP project for GCS bucket operations.
- `ARTIFACT_PREFIX` sets the prefix for locally published module artifacts.
- `ARTIFACT_BACKEND=s3` uses S3-compatible object storage, including GCS interoperability, MinIO, or Ceph.
- For `s3`, set `ARTIFACT_ENDPOINT`, `ARTIFACT_BUCKET`, `ARTIFACT_ACCESS_KEY_ID`, and `ARTIFACT_SECRET_ACCESS_KEY`.
- `ARTIFACT_PATH_STYLE=true` is useful for GCS/MinIO-style endpoints.

## Database Backend

- `DATABASE_DSN=postgres://...` enables PostgreSQL.
- `DATABASE_DSN=sqlite:///data/puppet-forge.db` enables SQLite.
- The database backend is selected from the DSN scheme; no separate `DB_BACKEND` setting is required.
- PostgreSQL is the production-oriented backend for multi-replica deployments. SQLite is suitable for local and single-writer deployments; its refresh lease uses SQLite transaction locking rather than PostgreSQL `FOR UPDATE NOWAIT`.

## Access Control

Access configuration is stored only in SQL tables:

- `access_teams`;
- `access_tokens`;
- `access_publish_owners`;
- `access_oidc_mappings`.

`ACCESS_JSON` is no longer used.

Important rules:

- `ADMIN_TOKEN` is a runtime-only bootstrap/break-glass admin token and is not persisted in SQL.
- If the DB is empty and `ADMIN_TOKEN` is not set, the service refuses to start.
- On a clean start, log in to `/manage` with `ADMIN_TOKEN`, open `/manage/access`, and configure teams, tokens, OIDC groups, and OIDC team admins.
- The web catalog (`/` and `/modules/...`) remains informationally public regardless of `PUBLIC_MODULE_ACCESS`.
- `read_tokens` allow read API, download, and `/v3/*` access.
- `publish_tokens` allow read, publish, and update access only within permitted publishing spaces; they do not allow module deletion.
- Deleting modules and versions is allowed for global admins in any namespace and for OIDC team admins only in their primary team space.
- Extra publishing spaces allow publishing/updating, but do not grant delete ownership.
- OIDC team-admin mappings allow editing tokens and OIDC groups only for the mapped team.
- OIDC admin mappings grant global access to `/manage/access` and delete access in any namespace.
- `PUBLIC_MODULE_ACCESS=false` requires a read/publish/admin token for install/API routes: read API, release downloads, extracted module files, and `/v3/*`.
- `PUBLIC_MODULE_ACCESS=true` opens those install/API routes without a token so r10k and `puppet module install` can fetch modules without an Authorization header.
- `PUBLIC_MODULE_ACCESS=true` does not open publish, delete, `/manage`, or `/manage/access`.

## Web Auth and OIDC

- `WEB_AUTH_MODE=oidc` enables session login for the web UI through OIDC/Authenik.
- API access remains token-based.
- Required settings are `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, and `OIDC_COOKIE_SECRET`.
- `OIDC_REDIRECT_URL` may be set explicitly, but for Kubernetes/multi-ingress deployments it is usually better to leave it empty so the service derives the callback URL from the current request host/proto and appends `/auth/callback`.
- `PUBLIC_BASE_URL` is optional and is only used as fallback when the request does not contain `Host`, `Forwarded`, or `X-Forwarded-*`.
- Kubernetes ingress should forward the real host and scheme through `X-Forwarded-Host`/`X-Forwarded-Proto` or RFC `Forwarded`.
- Team web UI access maps OIDC users to teams through `oidc_groups`.
- Delegated team-admin access maps OIDC users through `oidc_team_admin_emails` or `oidc_team_admin_groups`.

Enable OIDC in Docker Compose with environment variables:

```bash
export WEB_AUTH_MODE=oidc
export PUBLIC_MODULE_ACCESS="false"
export OIDC_ISSUER_URL="https://auth.example.com/application/o/puppet-forge/"
export OIDC_CLIENT_ID="puppet-forge"
export OIDC_CLIENT_SECRET="replace-me"
export OIDC_COOKIE_SECRET="32-byte-random-secret"
# Optional: use this when discovery does not expose end_session_endpoint.
export OIDC_LOGOUT_URL="https://auth.example.com/application/o/puppet-forge/end-session/"
export ADMIN_TOKEN="replace-me-bootstrap-token"
docker compose up
```

For local Authentik setups, avoid `localhost` in redirect URIs. Use a local DNS hostname that opens in the browser, for example:

```bash
open "http://forge.127.0.0.1.nip.io:8080"
```

Add the exact redirect URI in the Authentik provider:

```text
http://forge.127.0.0.1.nip.io:8080/auth/callback
```

For multiple ingress hostnames with an empty `OIDC_REDIRECT_URL`, register a callback for each hostname:

```text
https://forge.example.com/auth/callback
https://forge.dev.example.com/auth/callback
```

Logout from `/manage` clears local token/OIDC cookies. If OIDC discovery exposes `end_session_endpoint`, or if `OIDC_LOGOUT_URL` is set, the service also redirects the browser to provider logout so the provider does not silently log the user back in with an old session.

For groups, prefer OIDC groups over individual emails:

```json
{
  "team": "teamname",
  "oidc_groups": ["teamname-devops"]
}
```

In Authentik, add users to the `teamname-devops` group and ensure the application/provider sends the `groups` claim in the ID token. For team access, `oidc_groups` grants publishing access to the publishing space named after `Team`. If the team also needs other spaces, add them as extra publishing spaces.

To let a team manage its own tokens and OIDC groups, add `OIDC team admins`. For one or two people, emails are convenient; when the list grows, use a group:

```json
{
  "team": "teamname",
  "oidc_groups": ["teamname-devops"],
  "oidc_team_admin_emails": ["owner@example.com"],
  "oidc_team_admin_groups": ["teamname-admins"]
}
```

A user with email `owner@example.com` or group `teamname-admins` can open `/manage/access`, but sees only the `teamname` team and cannot edit other teams, global admins, JSON config, or extra publishing spaces. In `/manage`, that team admin can delete modules and versions only in the team's primary space. Extra publishing spaces remain publishing and update scope only; deletion there is global-admin-only.

The same OIDC team-admin email or group can be added to multiple teams. The user will be able to edit all mapped teams in `/manage/access`, but still cannot access unrelated teams or global admin settings.

Global OIDC admin access is configured in the `Global OIDC Admins` block:

```json
{
  "team": "platform-admin",
  "oidc_admin_groups": ["forge-admins"]
}
```

Users in `forge-admins` can open `/manage/access` and delete modules/versions in any namespace. Publishing access is not automatically granted by global admin groups.

If an OIDC user belongs to a team group such as `teamname-devops` and also matches `oidc_admin_groups`, `oidc_admin_emails`, or `oidc_admin_subjects`, the admin mapping wins.

To add yourself as an admin after the first startup:

1. Log in to `/manage` with `ADMIN_TOKEN`.
2. Open `/manage/access`.
3. In `Global OIDC Admins`, add your email to `OIDC admin emails` or your group to `OIDC admin groups`.
4. Save global admins and log in again through OIDC.

`ADMIN_TOKEN` should be kept for bootstrap/break-glass access, for example when OIDC is unavailable. Use OIDC admin groups or emails for daily access.

Example DB-backed access config that can be entered through `/manage/access`:

```json
[
  {
    "team": "teamname",
    "read_tokens": ["teamname-read-token"],
    "publish_tokens": ["teamname-publish-token"],
    "oidc_groups": ["teamname-devops"],
    "oidc_team_admin_emails": ["owner@example.com"],
    "oidc_team_admin_groups": ["teamname-admins"]
  },
  {
    "team": "carbon",
    "read_tokens": ["carbon-read-token"],
    "publish_tokens": [],
    "oidc_groups": ["carbon-devops"]
  },
  {
    "team": "platform-admin",
    "oidc_admin_groups": ["forge-admins"]
  }
]
```

## Module Publishing

The service accepts Puppet module `.tar.gz` archives through `POST /api/v1/modules` as `multipart/form-data`.

Minimum module requirements:

- the archive root contains a valid `metadata.json`;
- `metadata.json.name` uses the `<owner>-<name>` format, for example `teamname-apache`;
- `version` is read from `metadata.json.version` unless overridden by form fields;
- putting `README.md` in the module is recommended because it is rendered on the HTML module page.

Example layout:

```text
teamname-apache/
  metadata.json
  README.md
  manifests/
    init.pp
```

Example `metadata.json`:

```json
{
  "name": "teamname-apache",
  "version": "1.2.3",
  "summary": "Apache module",
  "author": "Example Team"
}
```

Build the archive with Puppet:

```bash
cd /path/to/teamname-apache
puppet module build
```

Puppet usually writes the artifact to `pkg/teamname-apache-1.2.3.tar.gz`.

Recommended upload: let the service read `owner`, `name`, `version`, and `description` from the archive:

```bash
export FORGE_URL="https://forge.example.com"
export PUBLISH_TOKEN="replace-me"

curl -X POST "${FORGE_URL}/api/v1/modules" \
  -H "Authorization: Bearer ${PUBLISH_TOKEN}" \
  -F "file=@pkg/teamname-apache-1.2.3.tar.gz" \
  -F 'metadata={"source":"internal-ci"}'
```

Important details:

- normal publishing only needs the `file` field;
- `owner`, `name`, `version`, and `description` are read from the archive;
- form fields override archive values when provided;
- the token must be a `publish_token` with access to the target publishing space. In the structured UI, the space named after `Team` is added automatically, and additional spaces are configured as extra publishing spaces.

Manual upload with explicit overrides:

```bash
curl -X POST "${FORGE_URL}/api/v1/modules" \
  -H "Authorization: Bearer ${PUBLISH_TOKEN}" \
  -F "file=@pkg/teamname-apache-1.2.3.tar.gz" \
  -F "owner=teamname" \
  -F "name=apache" \
  -F "version=1.2.3" \
  -F "description=Apache module" \
  -F 'metadata={"source":"internal-ci","git_sha":"abc123"}'
```

Quick verification:

```bash
curl "${FORGE_URL}/api/v1/modules/teamname/apache"
curl -I "${FORGE_URL}/api/v1/modules/teamname/apache/versions/1.2.3/download"
```

The module HTML page is available at:

```text
https://forge.example.com/modules/teamname/apache
```

The UI includes:

- live filtering on `/`;
- quick upstream module page navigation;
- README Markdown rendering;
- module version selector;
- ready-to-copy install snippets for Puppet and r10k.

## Active Releases and Deletion

- Releases requested by r10k or `puppet module install` within `ACTIVE_RELEASE_TTL` are marked as `in use`.
- Latest and active releases cannot be deleted through API or `/manage`.
- Delete buttons are hidden for protected releases in `/manage`.
- `ACTIVE_RELEASE_TTL` defaults to `720h` / 30 days.
- Release usage rows older than the active window are pruned when active releases are listed.
- Deleted upstream releases are stored in the `deleted_releases` tombstone table so the next upstream sync does not recreate them.
- Deleting the whole module clears tombstones and release usage for that module, so a later upstream sync can create it again with all available upstream releases.

## Observability

The service provides:

- structured HTTP logging for all requests;
- Prometheus metrics on `GET /metrics`;
- Grafana dashboard example in [examples/grafana/puppet-forge-dashboard.json](./examples/grafana/puppet-forge-dashboard.json);
- Prometheus alert rules in [examples/prometheus/alerts/puppet-forge.yml](./examples/prometheus/alerts/puppet-forge.yml).

Inventory metrics:

- `puppet_forge_module_info{module,owner,name,latest_version} 1`
- `puppet_forge_module_releases{source}`
- `puppet_forge_module_latest_releases{source}`

Operational metrics:

- `puppet_forge_publish_total{result,owner}`
- `puppet_forge_delete_total{result,kind,owner}`
- `puppet_forge_release_usage_mark_total{result,owner}`
- `puppet_forge_upstream_sync_total{result,trigger}`
- `puppet_forge_upstream_refresh_cycles_total{result}`
- `puppet_forge_upstream_refresh_duration_seconds`
- `puppet_forge_upstream_refresh_last_duration_seconds`
- `puppet_forge_upstream_refresh_last_success_timestamp_seconds`
- `puppet_forge_upstream_refresh_last_error_timestamp_seconds`
- `puppet_forge_upstream_refresh_modules{result}`
- `puppet_forge_upstream_cache_requests_total{kind,result}`

Metric notes:

- `puppet_forge_module_info` exports one series per locally available module.
- `module` uses the `<owner>-<name>` format.
- `owner` is the module namespace/publisher.
- `name` is the short module name without owner prefix.
- `latest_version` is the latest version known locally.
- `puppet_forge_module_releases` exports locally available release counts grouped by `source`.
- `puppet_forge_module_latest_releases` exports latest-release counts grouped by `source`.
- Release-level inventory metrics are intentionally aggregated so `/metrics` remains fast with many module versions.

`puppet_forge_module_info` is intentionally aligned with `puppetfile_module_info` from `prometheus-puppetfile-exporter`, so Grafana/PromQL can compare:

- `current_version` from Puppetfile;
- `latest_version` from the local Forge;
- shared `owner` and `name` labels.

See [METRICS.md](./METRICS.md) for the full metrics reference.

## Local Development

```bash
go test ./...
go run ./cmd/server
```

PostgreSQL parity tests are opt-in so ordinary `go test ./...` does not depend on local Docker/Postgres. To compare SQLite and PostgreSQL behavior, run:

```bash
make test-postgres
```

## Docker Compose

Start the local stack:

```bash
docker compose up --build
```

The service is available at `http://localhost:8080`.

Compose starts:

- `app` with this service;
- `postgres`, with tables created by the service on startup;
- `fake-gcs-server` as a local GCS emulator;
- `minio` and `minio-init` for local S3-compatible object storage;
- `r10k`, based on `puppet/r10k`, running `puppetfile install` as a one-shot container.

Local storage backend switches:

- `ARTIFACT_BACKEND=gcs` uses `fake-gcs-server` through `ARTIFACT_ENDPOINT=http://gcs:4443`;
- `ARTIFACT_BACKEND=s3` uses MinIO through `ARTIFACT_ENDPOINT=http://minio:9000`.

Standalone r10k smoke with Compose:

1. Start the stack:

    ```bash
    docker compose up --build
    ```

2. Ensure the required modules are available locally. For upstream modules, r10k requests to `/v3/*` index metadata and cache artifacts automatically.

3. Keep upstream-only modules in `testdata/r10k/Puppetfile` for smoke testing. Local private modules must be published first; otherwise r10k should fail with "module does not exist".

4. Run the one-shot `r10k` container:

   ```bash
   docker compose up r10k
   ```

After it exits:

```bash
docker compose logs r10k
```

The default `docker-compose.yml` r10k config talks to the service at `http://app` inside the Compose network and passes the local `ADMIN_TOKEN` as `authorization_token`, so smoke tests work with `PUBLIC_MODULE_ACCESS=false`.

## Makefile

Common commands:

```bash
make help
make check
make test-postgres
make compose-up
make http-smoke
make compose-smoke
make r10k
make oidc-preflight
make helm-package
```

`make http-smoke` checks a running HTTP instance: `/healthz`, `/readyz`, `/metrics`, public HTML catalog, token login form, read API policy for `PUBLIC_MODULE_ACCESS`, and admin-token read/login. Override `SMOKE_BASE_URL`, `SMOKE_PUBLIC_MODULE_ACCESS`, and `SMOKE_ADMIN_TOKEN` for non-default instances.

`make compose-smoke` starts the local Compose stack in detached mode, runs HTTP smoke, runs r10k one-shot, and returns the r10k exit code.

By default, the Makefile uses `go`, `gofmt`, and `golangci-lint` from `PATH`. Override tools with variables:

```bash
GO=go GOLANGCI_LINT=golangci-lint make check
```

## Kubernetes

The Helm chart is in [deploy/puppet-forge](./deploy/puppet-forge).

Example:

```bash
helm upgrade --install puppet-forge ./deploy/puppet-forge
```

By default the chart uses a fixed `replicaCount`. To let Kubernetes manage the replica count, enable HPA:

```bash
helm upgrade --install puppet-forge ./deploy/puppet-forge \
  --set autoscaling.enabled=true \
  --set autoscaling.minReplicas=2 \
  --set autoscaling.maxReplicas=6
```

When `autoscaling.enabled=true`, the Deployment does not render `spec.replicas`; the `HorizontalPodAutoscaler` owns it. Do not enable autoscaling with a `sqlite://` `DATABASE_DSN`; use PostgreSQL and a shared `MANAGE_SESSION_SECRET` for multi-replica deployments.

The chart supports Prometheus Operator resources, disabled by default:

- `serviceMonitor.enabled=true` creates a `ServiceMonitor`;
- `prometheusRule.enabled=true` creates a `PrometheusRule` with the same recording and alert rules as [examples/prometheus/alerts/puppet-forge.yml](./examples/prometheus/alerts/puppet-forge.yml).

## GitHub Actions

Workflow [`CI`](./.github/workflows/ci.yml) runs on pull requests and branch pushes. It calls reusable workflow [`Checks`](./.github/workflows/checks.yml) to run formatting, `go vet`, `golangci-lint`, coverage threshold, Docker Compose config, Helm lint/package, release archive smoke, Docker image dry-build, and race tests.

Workflow [`Release`](./.github/workflows/release.yml) runs only for tags matching `v*.*.*`. It calls the same reusable checks before publishing, without the local Docker Compose config check. For tag `v1.2.3`, it builds:

- GitHub Release assets with binary archives in `dist/*.tar.gz` and `dist/checksums.txt`;
- multi-arch Docker images `ghcr.io/<owner>/<repo>:v1.2.3` and `:latest`;
- Helm chart `puppet-forge-1.2.3.tgz` with `appVersion: v1.2.3`;
- Helm repository index on GitHub Pages through `helm/chart-releaser-action`.

For the Helm repository, create or allow the `gh-pages` branch and configure GitHub Pages to serve it. After a release:

```bash
helm repo add puppet-forge https://<owner>.github.io/<repo>
helm repo update
helm upgrade --install puppet-forge puppet-forge/puppet-forge
```

## Additional Documentation

- Metrics: [METRICS.md](./METRICS.md)
- Architecture: [ARCHITECTURE.md](./ARCHITECTURE.md)
- Grafana dashboard: [examples/grafana/puppet-forge-dashboard.json](./examples/grafana/puppet-forge-dashboard.json)
- Prometheus rules: [examples/prometheus/alerts/puppet-forge.yml](./examples/prometheus/alerts/puppet-forge.yml)
