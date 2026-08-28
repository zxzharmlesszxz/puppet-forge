# Architecture

[Українська версія](ARCHITECTURE.uk.md)

> **Runtime diagrams and verification map:** [SCHEMA.md](SCHEMA.md) shows the
> single-instance and multi-instance topologies, request sequences, shared-state
> coordination, safety mechanisms, and node-level checks.

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

1. Client sends `POST /api/v1/modules` with multipart `space` and `file` fields.
2. `internal/httpapi` authenticates the principal and verifies publish permission for the selected space.
3. `internal/service` validates one canonical module root, safe regular archive entries, bounded expanded size, and exactly one root-level `metadata.json`, then reads owner, name, version, description, README, and metadata from the archive.
4. The service verifies that the archive namespace and root match the selected space and resulting identity.
5. Multipart input, checksums, object-storage upload, and artifact responses use streaming readers so archive size does not become per-request heap usage.
6. The service calculates MD5, SHA-256, and size, then serializes publication for the module identity across replicas.
7. Artifact storage creates `<prefix>/<owner>/<name>/<version>/<sha256>.tar.gz` with a backend precondition that prevents overwriting an existing object.
8. Bearer-token publishing creates the local release only if that module/version does not exist. Retrying the same bytes is idempotent, and different bytes return `409 Conflict`. An explicit management-only replacement requires global or owning-team delete capability; SQL atomically switches the release to the new content-addressed path and queues the superseded local object for durable deletion.
9. A definite SQL failure removes the uncommitted object and any empty module row. If commit status cannot be verified, the content-addressed object is preserved for reconciliation instead of risking deletion of committed data.
10. API returns the created release payload. Manual identity and metadata form overrides are rejected.

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
5. A cold `/v3/files/*` body streams directly from upstream into a create-only
   object under `upstream-cache/`. Size and `Content-Length` validation happen
   during that stream; only after the object is committed does the service open
   it and stream the response to the client. The artifact is therefore never
   retained as a request-sized in-memory byte slice, and clients cannot observe
   a partial cache object.
6. Fresh upstream module metadata is observed and stored locally for UI and metrics usage. JSON cache hits do not repeat module/release indexing or write release-usage state. Exact v3 release and file requests track usage, while all local and upstream usage paths coalesce SQL writes to at most once per release per minute on each replica through a bounded 10,000-entry in-memory throttle. SQL remains the shared active-release source of truth.
7. On a cold module response, observation and SQL indexing finish before response delivery. The observer detaches client cancellation while retaining request values and applies an independent 30-second timeout, preventing both canceled indexing and partially indexed releases from racing the client's next request.

### Background Refresh Flow

1. If `UPSTREAM_SYNC_INTERVAL` is configured, the app starts a background refresh loop.
2. The process acquires a lease in the SQL store to avoid concurrent refresh leaders.
3. The leader refreshes cached upstream modules on the configured interval through a bounded worker pool controlled by `UPSTREAM_SYNC_CONCURRENCY`.

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

The GCS client and generated artifact URLs use the configured HTTP(S)
`ARTIFACT_ENDPOINT` without mutating process-global emulator environment state.
The S3 backend's immutable create-only contract requires the selected service to
atomically enforce `If-None-Match: *` for `PutObject`; supported backends are
verified by the object-storage integration test.

Local release deletion atomically removes SQL metadata and writes the content-addressed object path to the durable `artifact_deletions` outbox. A lease-elected worker retries idempotent object deletion every minute. It acquires the module lock and checks for a current SQL reference before deleting, so republishing the same immutable path cancels the stale task instead of deleting live content. Indexed upstream releases may reference the shared `upstream-cache/` namespace; deleting their metadata does not enqueue that shared object. A separate daily lease-elected worker removes only unreferenced cache objects older than `UPSTREAM_ARTIFACT_ORPHAN_TTL`.

Periodic refresh and cleanup workers are critical process components. Expected
operational errors are logged and retried, but an unexpected worker panic is not
recovered locally: the process exits so the runtime restarts a fully wired
instance instead of leaving a Ready API with permanently stopped maintenance.

## Routing Model

Important route groups:

- `/api/v1/modules`
  publish and list modules
- `/api/v1/modules/{owner}/{name}`
  module lookup and delete
- `/api/v1/modules/{owner}/{name}/versions/{version}`
  release lookup and delete
- `/api/v1/modules/{owner}/{name}/versions/{version}/download`
  service-streamed release download with HEAD, single-range, ETag, and conditional-request support
- `/`
  HTML index
- `/modules/...`
  HTML module and extracted file views
- `/v3/*`
  upstream Forge proxy
- `/metrics` on the separate `METRICS_ADDR` listener; it is not registered on the application listener
  Prometheus endpoint
- `/healthz`
  process health
- `/readyz`
  readiness backed by service dependencies
- `/manage`
  authenticated read-only overview of modules and manageable teams
- `/manage/modules`
  authenticated cross-team module publishing, search, deletion, and upstream imports
- `/manage/teams` and `/manage/teams/{team}/{access,tokens,modules}`
  authenticated team list and separate team-scoped access, token, and module management pages
- `/manage/admin/{access,spaces}`
  authenticated structured global access and publishing-space configuration available only to global admins

## Auth Model

API auth is token-based through access configuration stored in SQL. There is no JSON seed path: the database is the source of truth for teams, tokens, publishing spaces, and OIDC mappings.
Both SQLite and PostgreSQL stores create the required operational schema in code during startup; Docker Compose does not rely on PostgreSQL initdb SQL files.

- generated read tokens allow read APIs, downloads, and `/v3/*`
- generated publish tokens allow read, publish, and update access within configured publishing spaces
- persisted token values are never stored or returned after creation: SQL contains an opaque token ID, visible prefix, HMAC-SHA256 digest keyed by `ACCESS_TOKEN_PEPPER`, operator-facing name, role, and lifecycle timestamps
- successful token authentication updates `last_used_at` at most once per token per minute per service replica; SQL also rejects stale timestamp updates. This telemetry throttle is bounded to 10,000 recent token IDs per replica and evicts the oldest entry under higher cardinality; it is not used to authorize requests
- legacy plaintext token rows are migrated in place on startup after `ACCESS_TOKEN_PEPPER` is configured
- browser management authentication uses opaque cookie IDs backed by SQL sessions. Token sessions persist only an HMAC of the ID, credential digest/ID, CSRF secret, timestamps, and revocation state; OIDC sessions persist the mapped identity claims and the same lifecycle/CSRF data. Session IDs and raw API tokens never enter SQL
- OIDC authorization state is HMAC-indexed, expires after five minutes, and is atomically consumed once across replicas. Authorization requests use nonce and PKCE S256; callbacks enforce nonce plus strict signed ID-token issuer, audience, algorithm, and expiry validation
- the independent `MANAGE_SESSION_SECRET` protects token-session IDs. `ACCESS_TOKEN_PEPPER` remains dedicated to access-token digests, while `OIDC_COOKIE_SECRET` protects transient OIDC state cookies and OIDC session IDs. Each value must be stable and identical across replicas
- an early request boundary strips every forwarded-header variant from untrusted peers. Forwarded host/proto/client addresses are accepted only after explicit opt-in, direct-peer CIDR matching, syntax validation, and conflict checks; `ALLOWED_PUBLIC_HOSTS` can additionally constrain all effective request hosts
- deleting modules and releases is allowed for global admins across all spaces and for delegated team admins only inside their managed primary team spaces; extra publishing spaces allow publishing but do not grant delete ownership
- `ADMIN_TOKEN` is a runtime-only bootstrap/break-glass admin token and is not persisted in SQL
- The informational HTML catalog (`/` and `/modules/...`) stays publicly viewable regardless of `PUBLIC_MODULE_ACCESS`
- `PUBLIC_MODULE_ACCESS=true` bypasses read auth only for install/API routes: module metadata APIs, downloads, extracted module files, and `/v3/*`, mainly for r10k-style consumers without an Authorization header
- `PUBLIC_MODULE_ACCESS=false` requires read/publish/admin auth for those same install/API routes

HTML UI can additionally use OIDC session auth when `WEB_AUTH_MODE=oidc`. Structured team access maps OIDC identities to team principals with publishing rights through `oidc_groups`. Delegated team admin principals map through `oidc_team_admin_emails` or `oidc_team_admin_groups` and may edit only their own team's tokens and OIDC groups. They may delete modules/releases only inside their managed primary team spaces; extra publishing spaces permit publishing and updates but do not grant delete ownership. Global admin principals are managed separately through `oidc_admin_groups`, `oidc_admin_emails`, or `oidc_admin_subjects`.

`platform-admin` is reserved as the global-admin configuration identity. It may contain only global admin credentials and cannot act as a publishing team or module namespace.

When a single OIDC identity matches several mappings, the service unions all capabilities and team scopes. Global administration therefore does not remove team administration or publishing rights. A global-admin mapping alone does not grant publishing rights; content permissions still come from publisher or team-admin mappings.

`/manage/teams` is the team-centric management entry point. It lists only teams available to the principal and summarizes spaces, modules, tokens, and OIDC mappings. Team access settings, token lifecycle controls, and module publishing/catalog operations live on distinct `/manage/teams/{team}/access`, `/tokens`, and `/modules` pages. The legacy team root redirects to Access, while backend authorization remains independent for every operation.

Paginated public and Manage lists use one progressive-enhancement contract. The server always renders the canonical list HTML, while the shared browser controller fetches the requested page and replaces only the matching list boundary, including its rows, counters, page-size control, and Previous/Next links. Filters, independent lists on the same page, and browser Back/Forward use the same mechanism; ordinary navigation remains the fallback when JavaScript or a fragment request fails. Page-size preferences persist in a same-site cookie and browser storage with a choice of 10, 20, 50, or 100 rows. Manage module catalogs apply owner/search filtering in SQL and load release/activity data only for the current page, with active usage resolved through one bounded batch query.

`/manage/admin/access` is the global OIDC access-management UI, and `/manage/admin/spaces` is the global-admin-only publishing-space assignment UI. There is no HTTP endpoint for full access-config replacement. Global admins create teams from `/manage/teams/new`, assign or unassign extra publishing spaces, manage global OIDC mappings, and may edit or remove every team. Team admins create, rotate, or revoke team access tokens and edit OIDC publishing groups and OIDC team admin mappings on their scoped team Access and Tokens pages; forged team updates cannot change publishing-space assignments. Token creation uses one form with a required operator-facing name and an explicit `read` or `publish` role. Active name/role pairs are unique within a team, while immutable token IDs remain the authorization and lifecycle identity. Raw generated tokens are shown once on a `no-store` response; regular pages expose only token metadata. Active roles share one newest-first actionable list; expired and revoked records move into a collapsed, newest-first history. Both lists are independently paginated, default to 20 records, and support a persisted choice of 10, 20, 50, or 100, so accidental bulk creation cannot expand the whole page. `ACCESS_TOKEN_HISTORY_TTL` controls SQL retention for inactive records. Cleanup runs asynchronously under a shared SQL lease immediately after startup and once per day, so readiness does not wait for retention work. Each team can publish to the space matching `team` by default; that primary assignment is automatic and cannot be unassigned. Extra spaces extend the list and may be changed only through structured global administration. Delete is allowed for global admins across all spaces and for team admins only inside their managed primary team spaces; extra publishing spaces allow publishing/updating but not deletion. Ordinary publishing tokens cannot delete. Token digests are globally unique, so one bearer credential can never resolve to multiple principals. Bearer authorization resolves capabilities from an atomically loaded SQL snapshot and then checks the immutable token ID against SQL on every protected request. Snapshot refresh failures tolerate at most 30 seconds of stale access configuration before protected requests fail with `503`; forced manage refreshes fail immediately. Revocation and expiration therefore fail closed across replicas immediately after the database commit, without reloading every team. Latest releases and releases active within `ACTIVE_RELEASE_TTL` cannot be deleted through API or `/manage/modules`; module deletion is rejected while the module contains protected releases. Release-usage retention runs asynchronously under the shared `release-usage-cleanup` SQL lease immediately after startup and daily, keeping read paths free of cleanup writes. Failed retention cycles retry after a stable per-replica 25–35 second delay; successful cycles wait for their complete configured interval. Deleted upstream releases remain suppressed from background refresh by SQL tombstones for `DELETED_RELEASE_TTL`; direct requests for an exact version restore it on demand for legacy clients. Tombstone cleanup runs asynchronously under a shared SQL lease immediately after startup and daily, while `0` keeps unrequested tombstones indefinitely. If SQL access config contains no effective credential, startup requires `ADMIN_TOKEN`; the operator logs in with that token and creates the initial access model through `/manage/teams`, `/manage/admin/spaces`, and `/manage/admin/access`.

## Failure Semantics

Periodic upstream refresh is a singleton job coordinated through the shared SQL `app_leases` table. Only one replica can hold the refresh lease; another replica can acquire it after expiry or explicit release. Each cycle has a bounded context and uses idempotent module/release upserts, so a partial cycle can be retried safely.

Sensitive OIDC login/callback, token login, and publish limits use atomic SQL counters shared by every replica and fail closed when the shared limiter is unavailable. High-volume search, download, and Forge v3 read limits remain bounded per process. Their expiration uses a priority queue instead of scanning every client bucket, and at capacity the bucket nearest to reset is evicted. Configure ingress-level protection as an additional adversarial-traffic boundary; read limits are intentionally not a distributed quota.

- If SQL store initialization fails, the service does not start.
- If artifact storage initialization fails, the service does not start.
- If upstream proxy initialization fails, the service does not start.
- If a publishing archive cannot be parsed, publishing returns `400`.
- If auth is enabled and the token is missing or insufficient, handlers return `401` or `403`.
- `/healthz` reflects process liveness.
- `/readyz` checks metadata SQL and performs a cached object-storage capability probe: create-if-absent, read, and delete of a unique object under `.readiness-probes/`. A successful capability result is reused for five minutes to keep probes bounded.

## Observability

The service exposes:

- structured HTTP request logs
- Prometheus HTTP metrics
- module inventory metrics

The canonical metric contract is documented in [METRICS.md](METRICS.md).
