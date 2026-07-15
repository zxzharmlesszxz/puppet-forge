# Architecture

## Overview

`puppet-forge` is a Go service that stores internal Puppet module releases, serves a small HTML UI, exposes an API for module publishing and admin-only deletion, and proxies `/v3/*` requests to the upstream public Puppet Forge.

The service separates:

- module metadata in SQL
- release artifacts in object storage
- upstream Forge proxy/cache behavior in a dedicated proxy layer

## Package Layout

- `cmd/server`
  Process entrypoint, config loading, logger setup, and HTTP server startup.
- `internal/app`
  Application wiring for storage backends, SQL store, auth, upstream proxy, background refresh loop, and router construction.
- `internal/httpapi`
  HTTP routes, JSON API handlers, HTML pages, health/readiness endpoints, and auth enforcement.
- `internal/service`
  Publishing normalization, archive inspection, artifact upload orchestration, upstream indexing, and release read logic.
- `internal/store`
  SQL persistence for modules, releases, refresh leases, and DB-backed access configuration with PostgreSQL and SQLite implementations.
- `internal/storage`
  Artifact backends for GCS and S3-compatible storage.
- `internal/proxy`
  Reverse proxy for upstream Puppet Forge API plus GCS/object-storage-backed cache for file downloads.
- `internal/auth`
  Token-based API authorization for read, publish, and admin-scoped delete flows.
- `internal/webauth`
  Optional OIDC session auth for the HTML UI.
- `internal/observability`
  Prometheus metrics and HTTP middleware logging.

## Data Flow

### Module Publishing Flow

1. Client sends `POST /api/v1/modules` with multipart form data and a module archive.
2. `internal/httpapi` reads the uploaded file and optional form fields.
3. `internal/service` inspects the archive and fills missing `owner`, `name`, `version`, `description`, `README`, and metadata from `metadata.json`.
4. The service validates owner/name slugs and version presence.
5. Artifact storage writes the archive to `<prefix>/<owner>/<name>/<version>/<file>`.
6. SQL store upserts the module and persists the release metadata.
7. API returns the created release payload.

### Read Flow

1. Client requests module or release metadata through `/api/v1/modules/...` or the HTML UI.
2. SQL store returns module and release records.
3. For local releases, download URLs are built from the configured artifact backend.
4. For upstream-indexed releases, the service may enrich missing fields by querying the upstream proxy integration.

### Upstream Proxy Flow

1. Client requests `/v3/*` on this service.
2. The proxy forwards the request to the public Puppet Forge.
3. JSON GET and HEAD responses are cached in memory for `UPSTREAM_PROXY_JSON_CACHE_TTL`.
4. If upstream fails after cache expiry, the proxy may serve stale JSON only within `UPSTREAM_PROXY_JSON_STALE_TTL`.
5. `/v3/files/*` downloads are cached into object storage under `upstream-cache/`.
6. Indexed upstream module metadata is observed and stored locally for UI and metrics usage.

### Background Refresh Flow

1. If `UPSTREAM_SYNC_INTERVAL` is configured, the app starts a background refresh loop.
2. The process acquires a lease in the SQL store to avoid concurrent refresh leaders.
3. The leader refreshes cached upstream modules on the configured interval.

## Storage Model

The service uses two persistence layers:

- SQL for modules, releases, and refresh leases
- object storage for module archives and cached upstream files

Supported SQL backends:

- PostgreSQL via `postgres://...`
- SQLite via `sqlite:///...`

Supported artifact backends:

- Google Cloud Storage
- S3-compatible storage

## Routing Model

Important route groups:

- `/api/v1/modules`
  publish and list modules
- `/api/v1/modules/{owner}/{name}`
  module lookup and delete
- `/api/v1/modules/{owner}/{name}/versions/{version}`
  release lookup and delete
- `/api/v1/modules/{owner}/{name}/versions/{version}/download`
  release download redirect or backend URL
- `/`
  HTML index
- `/modules/...`
  HTML module and extracted file views
- `/v3/*`
  upstream Forge proxy
- `/metrics`
  Prometheus endpoint
- `/healthz`
  process health
- `/readyz`
  readiness backed by service dependencies

## Auth Model

API auth is token-based through access configuration stored in SQL. There is no JSON seed path: the database is the source of truth for teams, tokens, publishing spaces, and OIDC mappings.
Both SQLite and PostgreSQL stores create the required operational schema in code during startup; Docker Compose does not rely on PostgreSQL initdb SQL files.

- `read_tokens` allow read APIs, downloads, and `/v3/*`
- `publish_tokens` allow read, publish, and update access within configured publishing spaces
- deleting modules and releases is allowed for global admins across all spaces and for delegated team admins only inside their managed primary team spaces; extra publishing spaces allow publishing but do not grant delete ownership
- `ADMIN_TOKEN` is a runtime-only bootstrap/break-glass admin token and is not persisted in SQL
- The informational HTML catalog (`/` and `/modules/...`) stays publicly viewable regardless of `PUBLIC_MODULE_ACCESS`
- `PUBLIC_MODULE_ACCESS=true` bypasses read auth only for install/API routes: module metadata APIs, downloads, extracted module files, and `/v3/*`, mainly for r10k-style consumers without an Authorization header
- `PUBLIC_MODULE_ACCESS=false` requires read/publish/admin auth for those same install/API routes

HTML UI can additionally use OIDC session auth when `WEB_AUTH_MODE=oidc`. Structured team access maps OIDC identities to team principals with publishing rights through `oidc_groups`. Delegated team admin principals map through `oidc_team_admin_emails` or `oidc_team_admin_groups` and may edit only their own team's tokens and OIDC groups, and may delete modules/releases only inside their own publishing spaces. Global admin principals are managed separately through `oidc_admin_groups`, `oidc_admin_emails`, or `oidc_admin_subjects`.

When a single OIDC identity matches several mappings, global admin wins over team admin, and team admin wins over ordinary publishing rights. This keeps admin access usable for users who are also members of ordinary team groups.

`/manage/access` is the access-management UI for live access changes. Global admins can create, rename, delete, and bulk-replace teams; edit optional extra publishing spaces; and manage global OIDC admin mappings. Team admins can open the same page but see only their own team and can edit only read tokens, publishing tokens, OIDC publishing groups, OIDC team admin emails, and OIDC team admin groups. In the structured form, each team can publish to the space matching `team` by default; extra spaces extend that list and remain global-admin-only. Delete is allowed for global admins across all spaces and for team admins only inside their managed primary team spaces; extra publishing spaces allow publishing/updating but not deletion. Ordinary publishing tokens cannot delete. Latest releases and releases active within `ACTIVE_RELEASE_TTL` cannot be deleted through API or `/manage`; module deletion is rejected while the module contains protected releases. If SQL access config is empty, startup requires `ADMIN_TOKEN`; the operator logs in with that token and creates the initial access model through `/manage/access`.

## Failure Semantics

- If SQL store initialization fails, the service does not start.
- If artifact storage initialization fails, the service does not start.
- If upstream proxy initialization fails, the service does not start.
- If a publishing archive cannot be parsed, publishing returns `400`.
- If auth is enabled and the token is missing or insufficient, handlers return `401` or `403`.
- `/healthz` reflects process liveness.
- `/readyz` checks service readiness through the module service and its dependencies.

## Observability

The service exposes:

- structured HTTP request logs
- Prometheus HTTP metrics
- module inventory metrics

The canonical metric contract is documented in [METRICS.md](METRICS.md).
