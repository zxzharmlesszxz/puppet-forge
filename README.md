# Puppet Forge Service

> **Operational map:** [SCHEMA.md](SCHEMA.md) documents single-instance and
> multi-instance runtime flows, shared-state boundaries, safety mechanisms, and
> point-by-point verification procedures.

[Українська версія](README.uk.md)

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
- `GET /api/v1/manage/publish-spaces` lists spaces available to the authenticated principal with publishing rights;
- `GET /api/v1/modules?limit=20&offset=0` lists modules and returns `items`, `limit`, `offset`, and `total`;
- `GET /api/v1/modules/{owner}/{name}` returns a module card;
- `DELETE /api/v1/modules/{owner}/{name}` deletes a module through global-admin or team-admin access;
- `GET /api/v1/modules/{owner}/{name}/versions/{version}` returns a specific release;
- `DELETE /api/v1/modules/{owner}/{name}/versions/{version}` deletes a specific release through global-admin or team-admin access;
- `GET /api/v1/modules/{owner}/{name}/versions/{version}/download` serves the stored artifact;
- `GET /` renders the public HTML module index;
- `GET /modules/{owner}/{name}` renders a module page with README Markdown, version selector, and install snippets;
- `GET /v3/*` and `HEAD /v3/*` reverse-proxy the official Puppet Forge API;
- `/manage` provides a read-only overview of modules and manageable teams;
- `/manage/modules` provides cross-team module publishing, deletion, search, pagination, and upstream imports;
- `/manage/teams` lists manageable teams; `/manage/teams/{team}/access`, `/tokens`, and `/modules` provide separate team-scoped management pages;
- paginated public and Manage lists update only their own rows, counters, and controls through progressive HTML fragment requests; filters and browser history use the same mechanism, ordinary navigation remains the fallback, and page size persists with a choice of 10, 20, 50, or 100 items;
- `/manage/admin/access` and `/manage/admin/spaces` provide structured global DB-backed access configuration;
- `ADMIN_TOKEN` provides bootstrap/break-glass access;
- OIDC login supports global admins, team admins, and OIDC groups that grant publishing rights;
- `PUBLIC_MODULE_ACCESS` controls anonymous machine-readable install/API access;
- active release tracking blocks backend deletion of latest/in-use releases and hides unsafe delete actions in `/manage/modules`;
- `GET /healthz` and `GET /readyz` expose health probes.

Not implemented:

- advanced search with filters, ratings, dependencies, and verified publishers;
- full compatibility with every official Puppet Forge API behavior;
- CDN or signed URLs with TTL.

## Architecture

- HTTP API: standard `net/http`;
- Service layer: validation, upload orchestration, metadata updates;
- Artifact storage: GCS or S3-compatible object storage;
- Metadata store: PostgreSQL or SQLite selected by `DATABASE_DSN`;
- Upstream proxy: reverse proxy to the official Forge API with an in-memory TTL cache for JSON GET/HEAD responses.

Publishing flow:

1. A client sends a multipart request containing the selected `space` and a `.tar.gz` module artifact.
2. The service verifies that the authenticated principal may publish to that space.
3. The service reads identity and metadata from the archive and requires its namespace to match the selected space.
4. MD5, SHA-256, and size are calculated once. The archive is created without overwrite at `modules/<owner>/<name>/<version>/<sha256>.tar.gz`.
5. Local release identity is immutable: retrying identical bytes is idempotent, while different bytes for an existing module/version return `409 Conflict`.
6. Release metadata is created in the selected SQL backend and returned to the client. Definite persistence failures compensate the uncommitted object.

Proxy flow:

1. A client calls `/v3/...` on this service.
2. The service proxies the request to the official Forge API.
3. JSON responses are cached in memory for `UPSTREAM_PROXY_JSON_CACHE_TTL`.
4. File downloads through `/v3/files/...` stream into a create-only object under `upstream-cache/` on first use, then stream back through this service only after the shared object is complete. The proxy does not retain the full tarball in memory.

## Configuration

See [`.env.example`](.env.example).

Runtime parameters can be passed either through environment variables or command-line flags. Environment variables are still supported, and command-line flags override environment values when both are set.

The binary prints its build version and exits with:

```bash
puppet-forge --version
```

Flag names are the lowercase kebab-case form of the environment variable name:

```text
DATABASE_DSN -> --database-dsn
LOG_LEVEL -> --log-level
MANAGE_SESSION_SECRET -> --manage-session-secret
ACCESS_TOKEN_PEPPER -> --access-token-pepper
TRUSTED_PROXY_CIDRS -> --trusted-proxy-cidrs
TRUST_FORWARDED_HEADERS -> --trust-forwarded-headers
ALLOWED_PUBLIC_HOSTS -> --allowed-public-hosts
PUBLIC_MODULE_ACCESS -> --public-module-access
OIDC_SCOPES -> --oidc-scopes
OIDC_SIGNING_ALGORITHMS -> --oidc-signing-algorithms
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

| Variable                        | Default                           | Required                       | Description                                                                                                                                                                                                                                                                                          |
| ------------------------------- | --------------------------------- | ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `APP_ENV`                       | `dev`                             | no                             | Runtime environment name for logs and diagnostics.                                                                                                                                                                                                                                                   |
| `LOG_LEVEL`                     | `info`                            | no                             | Minimum structured log level: `debug`, `info`, `warn`, or `error`. At `debug`, the service emits request-start, authorization/session/CSRF, OIDC flow, upstream request/cache, and refresh-lease diagnostics without logging credentials or session secrets. Command-line equivalent: `--log-level`. |
| `HTTP_ADDR`                     | `:8080`                           | no                             | HTTP listen address inside the process or container.                                                                                                                                                                                                                                                 |
| `METRICS_ADDR`                  | `:9090`                           | no                             | Separate Prometheus listener. It must differ from `HTTP_ADDR` and should be network-restricted to Prometheus.                                                                                                                                                                                        |
| `READ_HEADER_TIMEOUT`           | `10s`                             | no                             | Maximum time to read request headers. Must be greater than zero.                                                                                                                                                                                                                                     |
| `READ_TIMEOUT`                  | `0s`                              | no                             | Maximum time to read an entire HTTP request. The default disables the whole-request deadline so large or slow module uploads can stream; header and body-size limits remain enforced. Set a positive Go duration only when the deployment can guarantee a sufficient upload rate.                    |
| `WRITE_TIMEOUT`                 | `0s`                              | no                             | Maximum time to write an entire HTTP response. The default disables the whole-response deadline so large or slow artifact downloads can stream; enforce client transfer limits at the ingress when needed.                                                                                           |
| `IDLE_TIMEOUT`                  | `60s`                             | no                             | Maximum keep-alive idle time between HTTP requests. Must be greater than zero.                                                                                                                                                                                                                       |
| `HTTP_MAX_HEADER_BYTES`         | `1048576`                         | no                             | Maximum request-header size in bytes; accepted range is 4096 through 16777216.                                                                                                                                                                                                                       |
| `SHUTDOWN_TIMEOUT`              | `10s`                             | no                             | Graceful shutdown timeout.                                                                                                                                                                                                                                                                           |
| `DATABASE_BACKEND`              | inferred from `DATABASE_DSN`      | no                             | Metadata backend: `postgres` or `sqlite`. Set it explicitly in Helm so topology validation does not depend on reading Secret contents.                                                                                                                                                               |
| `DATABASE_DSN`                  | empty                             | yes                            | Metadata store DSN. `postgres://...` enables PostgreSQL, `sqlite:///path/file.db` enables SQLite.                                                                                                                                                                                                    |
| `DATABASE_MAX_CONNS`            | `10`                              | no                             | Maximum PostgreSQL pool connections per replica. Size this together with the HPA maximum so all replicas remain within the database connection budget.                                                                                                                                               |
| `DATABASE_MIN_CONNS`            | `0`                               | no                             | Minimum PostgreSQL pool connections kept by each replica.                                                                                                                                                                                                                                            |
| `DATABASE_MAX_CONN_LIFETIME`    | `1h`                              | no                             | Maximum lifetime of a PostgreSQL pooled connection; zero disables lifetime expiry.                                                                                                                                                                                                                   |
| `DATABASE_MAX_CONN_IDLE_TIME`   | `30m`                             | no                             | Maximum idle time of a PostgreSQL pooled connection; zero disables idle expiry.                                                                                                                                                                                                                      |
| `ADMIN_TOKEN`                   | empty                             | when DB access config is empty | Runtime bootstrap/break-glass admin token. It is not stored in the database and is used to bootstrap access through `/manage/teams` and `/manage/admin/access`.                                                                                                                                      |
| `ACCESS_TOKEN_PEPPER`           | empty                             | yes                            | Shared secret of at least 32 bytes used for HMAC-SHA256 hashing of persisted tokens with `read` or `publish` roles. Keep it stable across all replicas and include it in backup/restore procedures; losing it invalidates stored tokens.                                                             |
| `ACCESS_TOKEN_HISTORY_TTL`      | `2160h`                           | no                             | Retention period for metadata of expired and revoked access tokens. Cleanup runs asynchronously under a cross-replica SQL lease immediately after startup and daily; `0` keeps inactive token history indefinitely. Active tokens are never removed by this cleanup.                                 |
| `DELETED_RELEASE_TTL`           | `2160h`                           | no                             | Retention period for deleted upstream-release tombstones. Cleanup runs asynchronously under a cross-replica SQL lease immediately after startup and daily; after expiry, refresh may restore the release. `0` retains tombstones indefinitely.                                                       |
| `MANAGE_SESSION_SECRET`         | empty                             | yes                            | Independent shared secret of at least 32 bytes for HMAC-hashing opaque server-side `/manage` session IDs. Keep it stable and identical on every replica. It must differ from `ACCESS_TOKEN_PEPPER`; rotating either secret invalidates only its own credential class.                                |
| `ARTIFACT_BACKEND`              | `gcs`                             | no                             | Artifact backend for module tarballs: `gcs` or `s3`.                                                                                                                                                                                                                                                 |
| `ARTIFACT_ENDPOINT`             | `https://storage.googleapis.com`  | for `s3`; optional for `gcs`   | Object storage endpoint. For `gcs`, this may point to an emulator/custom host. For `s3`, this is the S3-compatible endpoint.                                                                                                                                                                         |
| `ARTIFACT_BUCKET`               | empty                             | yes                            | Bucket/container for module tarballs and upstream artifact cache.                                                                                                                                                                                                                                    |
| `ARTIFACT_PROJECT`              | empty                             | when GCS provisioning enabled  | GCP project used only to create a missing GCS bucket when `ARTIFACT_ENSURE_BUCKET=true`. Not used by normal object operations or `s3`.                                                                                                                                                               |
| `ARTIFACT_ENSURE_BUCKET`        | `false`                           | no                             | When `true`, checks for the configured GCS bucket at startup and creates it when absent. Keep this disabled in production so the runtime identity needs only object permissions; enable it for local emulators or an explicitly privileged provisioning identity.                                    |
| `ARTIFACT_GCS_ANONYMOUS`        | `false`                           | no                             | Disables GCS authentication. Enable only for a local emulator; custom production endpoints use normal application-default credentials unless this is explicitly set.                                                                                                                                 |
| `ARTIFACT_PREFIX`               | `modules`                         | no                             | Bucket prefix for locally published modules.                                                                                                                                                                                                                                                         |
| `ARTIFACT_REGION`               | `us-east-1`                       | no                             | Region used by the S3-compatible client.                                                                                                                                                                                                                                                             |
| `ARTIFACT_ACCESS_KEY_ID`        | empty                             | for private `s3`               | Access key for S3-compatible storage.                                                                                                                                                                                                                                                                |
| `ARTIFACT_SECRET_ACCESS_KEY`    | empty                             | for private `s3`               | Secret key for S3-compatible storage.                                                                                                                                                                                                                                                                |
| `ARTIFACT_PATH_STYLE`           | `true`                            | no                             | Enables path-style S3 URLs. Useful for MinIO, GCS interoperability, and local endpoints.                                                                                                                                                                                                             |
| `PUBLIC_BASE_URL`               | empty                             | no                             | Optional fallback for building absolute URLs. If empty, the service derives URLs from the validated request host and, when explicitly trusted, forwarded host/proto.                                                                                                                                 |
| `ALLOWED_PUBLIC_HOSTS`          | empty                             | no                             | Optional comma- or space-separated allowlist of accepted public hosts, with optional ports. Disallowed hosts receive `421`; an entry without a port accepts that hostname on any port.                                                                                                               |
| `TRUSTED_PROXY_CIDRS`           | empty                             | no                             | Comma- or space-separated direct ingress/reverse-proxy CIDRs. These CIDRs may supply forwarded client identity and, only with `TRUST_FORWARDED_HEADERS=true`, forwarded host/proto.                                                                                                                  |
| `TRUST_FORWARDED_HEADERS`       | `false`                           | no                             | Accept validated `Forwarded`/`X-Forwarded-*` values only when the direct peer belongs to `TRUSTED_PROXY_CIDRS`. Otherwise every forwarded variant is removed before routing.                                                                                                                         |
| `PUBLIC_MODULE_ACCESS`          | `false`                           | no                             | If `true`, read APIs, downloads, and `/v3/*` are open without a token. If `false`, install/API routes require a token with read, publish, or admin privileges. HTML catalog pages stay informationally public. Publishing, deletion, and management routes are always protected.                     |
| `ACTIVE_RELEASE_TTL`            | `720h`                            | no                             | How long a release is considered active/in use after an r10k or `puppet module install` request. Active/latest versions cannot be deleted through API or `/manage/modules`.                                                                                                                          |
| `SECURITY_HSTS_ENABLED`         | `false`                           | no                             | Enables the `Strict-Transport-Security` response header. Keep disabled for local HTTP and enable only when the public endpoint is always HTTPS.                                                                                                                                                      |
| `WEB_AUTH_MODE`                 | `none`                            | no                             | Web auth mode: `none` or `oidc`. API token auth is independent from this setting.                                                                                                                                                                                                                    |
| `OIDC_ISSUER_URL`               | empty                             | for `WEB_AUTH_MODE=oidc`       | OIDC issuer discovery URL, for example an Authentik application provider URL.                                                                                                                                                                                                                        |
| `OIDC_CLIENT_ID`                | empty                             | for `WEB_AUTH_MODE=oidc`       | OIDC client ID.                                                                                                                                                                                                                                                                                      |
| `OIDC_CLIENT_SECRET`            | empty                             | for `WEB_AUTH_MODE=oidc`       | OIDC client secret.                                                                                                                                                                                                                                                                                  |
| `OIDC_REDIRECT_URL`             | empty                             | no                             | Explicit callback URL. If empty, the callback URL is built from the current request base URL as `/auth/callback`; this is recommended for multi-ingress deployments.                                                                                                                                 |
| `OIDC_LOGOUT_URL`               | auto-discovery/empty              | no                             | Provider end-session URL. If unset, the service tries to read `end_session_endpoint` from OIDC discovery.                                                                                                                                                                                            |
| `OIDC_COOKIE_SECRET`            | empty                             | for `WEB_AUTH_MODE=oidc`       | Secret of at least 32 bytes used to encrypt the transient state cookie and HMAC opaque SQL-backed OIDC session IDs. Must remain stable and identical across replicas.                                                                                                                                |
| `OIDC_SCOPES`                   | `openid profile email`            | no                             | Space-separated OIDC scopes. `openid` is always included; add provider-specific scopes such as `groups` when group claims require one.                                                                                                                                                               |
| `OIDC_SIGNING_ALGORITHMS`       | `RS256`                           | no                             | Space-separated allowlist of asymmetric ID-token signing algorithms. Supported values: `RS*`, `PS*`, `ES*`, and `EdDSA`; symmetric algorithms and `none` are rejected.                                                                                                                               |
| `UPSTREAM_URL`                  | `https://forgeapi.puppetlabs.com` | no                             | Upstream Puppet Forge API used when a module is not available locally.                                                                                                                                                                                                                               |
| `UPSTREAM_PROXY_JSON_CACHE_TTL` | `5m`                              | no                             | In-memory cache TTL for upstream JSON GET/HEAD responses such as `/v3/modules/...` and `/v3/releases/...`. It does not control object-storage tarball caching for `/v3/files/...`. `0s` effectively disables the JSON response cache.                                                                |
| `UPSTREAM_PROXY_JSON_STALE_TTL` | `1h`                              | no                             | Maximum time after `UPSTREAM_PROXY_JSON_CACHE_TTL` during which stale JSON may be served if upstream Forge fails or is unavailable. `0s` disables stale fallback.                                                                                                                                    |
| `FORGE_CACHE_MAX_BODY_BYTES`    | `1048576`                         | no                             | Maximum upstream JSON response body size that may be stored in the in-memory proxy cache.                                                                                                                                                                                                            |
| `MODULE_UPLOAD_MAX_BYTES`       | `134217728`                       | no                             | Maximum Puppet module archive size in bytes. Multipart framing has a separate bounded allowance; oversized archives return `413 Request Entity Too Large` and are not fully read into memory.                                                                                                        |
| `UPSTREAM_ARTIFACT_MAX_BYTES`   | `134217728`                       | no                             | Maximum upstream tarball size accepted from `/v3/files/...`, both for object-storage cache and bypass mode. An oversized upstream response is treated as an invalid gateway response and returns `502 Bad Gateway`.                                                                                  |
| `UPSTREAM_ARTIFACT_ORPHAN_TTL`  | `24h`                             | no                             | Grace period before a lease-elected worker removes an unreferenced object under `upstream-cache/`. Referenced objects are retained. Zero disables this cleanup.                                                                                                                                      |
| `UPSTREAM_SYNC_INTERVAL`        | `0s`                              | no                             | Background refresh interval for already cached upstream modules. `0s` disables background refresh.                                                                                                                                                                                                   |
| `UPSTREAM_SYNC_LIMIT`           | `1000`                            | no                             | Maximum number of upstream modules processed by one refresh cycle.                                                                                                                                                                                                                                   |
| `UPSTREAM_SYNC_CONCURRENCY`     | `8`                               | no                             | Maximum number of upstream modules refreshed concurrently within the elected refresh replica.                                                                                                                                                                                                        |
| `METRICS_MODULE_LIMIT`          | `10000`                           | no                             | Maximum number of owner series exported by inventory metrics during one collection pass.                                                                                                                                                                                                             |
| `METRICS_REFRESH_INTERVAL`      | `30s`                             | no                             | Base interval for rebuilding each replica's inventory-metric snapshot from shared SQL. A small random jitter prevents replicas from querying SQL simultaneously.                                                                                                                                     |
| `RECONCILE_ARTIFACTS`           | `false`                           | no                             | Run one-shot SQL/object-storage reconciliation, emit JSON, and exit without starting listeners.                                                                                                                                                                                                      |
| `RECONCILE_REPAIR`              | `false`                           | no                             | With reconciliation enabled, delete only orphan objects; missing/corrupt release records remain untouched.                                                                                                                                                                                           |

## Artifact Storage

- `ARTIFACT_BACKEND=gcs` uses Google Cloud Storage.
- `ARTIFACT_ENDPOINT` defaults to `https://storage.googleapis.com`; override it for a custom GCS host, emulator, or S3-compatible endpoint. GCS API requests and artifact URLs both use this endpoint without changing process-global emulator environment variables.
- `ARTIFACT_BUCKET` sets the bucket for artifacts.
- `ARTIFACT_PROJECT` identifies the project only when startup bucket provisioning is enabled.
- Production GCS runtime access requires object create/get/delete/list permissions on the pre-provisioned bucket (for example `roles/storage.objectAdmin` scoped to that bucket). `ARTIFACT_ENSURE_BUCKET=true` additionally requires `storage.buckets.get` and `storage.buckets.create`; keep it disabled for the workload identity unless bucket provisioning is intentional.
- `ARTIFACT_PREFIX` sets the prefix for locally published module artifacts.
- `ARTIFACT_BACKEND=s3` uses S3-compatible object storage, including GCS interoperability, MinIO, or Ceph. The backend must atomically enforce `If-None-Match: *` on `PutObject`; this is required for immutable create-only artifact writes and is covered by the integration test for supported backends.
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
- Persisted access tokens are generated as opaque credentials, returned only once, and stored as HMAC-SHA256 digests with a visible prefix and lifecycle metadata. Each new token has a required operator-facing name and a `read` or `publish` role; an active name/role pair must be unique within its team. Set the same `ACCESS_TOKEN_PEPPER` on every replica. Every protected bearer request checks the token ID's active state in SQL, so revocation and expiration apply across replicas immediately after commit. The management page uses one creation form and paginates the unified active-token list and inactive history independently; each list defaults to 20 records and supports a persisted choice of 10, 20, 50, or 100. `ACCESS_TOKEN_HISTORY_TTL` removes old expired/revoked metadata.
- If the DB contains no effective access credential and `ADMIN_TOKEN` is not set, the service refuses to start.
- On a clean start, log in to `/manage` with `ADMIN_TOKEN`, open `/manage/teams`, and configure teams, tokens, OIDC groups, and OIDC team admins. Global administrators assign extra publishing spaces under `/manage/admin/spaces`; global OIDC admins are configured under `/manage/admin/access`.
- Every token value must be globally unique across teams and access roles. Reusing a token with the `read` or `publish` role, or reusing an admin token, is rejected without exposing the token in the error.
- `platform-admin` is a reserved configuration identity for global admin tokens and OIDC mappings. It cannot be created or used as a publishing team or module space.
- The web catalog (`/` and `/modules/...`) remains informationally public regardless of `PUBLIC_MODULE_ACCESS`.
- Read tokens allow read API, download, and `/v3/*` access.
- Tokens with the `publish` role allow read, publish, and update access only within permitted publishing spaces; they do not allow module deletion.
- Deleting modules and versions is allowed for global admins in any namespace and for OIDC team admins only in their primary team space.
- Extra publishing spaces are assigned or unassigned only by global administrators under `/manage/admin/spaces`. They allow publishing/updating, but do not grant delete ownership.
- OIDC team-admin mappings allow editing tokens and OIDC groups only for the mapped team.
- OIDC admin mappings grant global access to `/manage/admin/*` and delete access in any namespace.
- `PUBLIC_MODULE_ACCESS=false` requires a token with read, publish, or admin privileges for install/API routes: read API, release downloads, extracted module files, and `/v3/*`.
- `PUBLIC_MODULE_ACCESS=true` opens those install/API routes without a token so r10k and `puppet module install` can fetch modules without an Authorization header.
- `PUBLIC_MODULE_ACCESS=true` does not open publish, delete, `/manage`, or `/manage/admin/*`.

## Web Auth and OIDC

- `WEB_AUTH_MODE=oidc` enables session login for the web UI through OIDC/Authenik.
- API access remains token-based.
- Required settings are `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, and `OIDC_COOKIE_SECRET`.
- Authorization state is single-use and expires after five minutes. The flow uses nonce binding, PKCE S256, strict issuer/audience/signature/expiry verification, and the `OIDC_SIGNING_ALGORITHMS` allowlist.
- The browser receives only an opaque OIDC session ID. Identity claims, session-bound CSRF secret, expiry, last-seen time, and revocation state are stored in SQL; logout revokes the shared session record.
- `OIDC_SCOPES` defaults to `openid profile email`; add `groups` when the provider only includes group claims for that scope.
- `OIDC_REDIRECT_URL` may be set explicitly, but for Kubernetes/multi-ingress deployments it is usually better to leave it empty so the service derives the callback URL from the current request host/proto and appends `/auth/callback`.
- `PUBLIC_BASE_URL` is optional and is only a fallback. The request `Host` is used by default, so multiple ingress hostnames work without a canonical URL.
- Forwarded host/proto are ignored unless `TRUST_FORWARDED_HEADERS=true` and the direct peer matches `TRUSTED_PROXY_CIDRS`. Configure both behind an ingress when HTTPS termination requires the protocol to be forwarded; use `ALLOWED_PUBLIC_HOSTS` to restrict accepted ingress names.
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
export ACCESS_TOKEN_PEPPER="replace-with-at-least-32-random-bytes"
export MANAGE_SESSION_SECRET="replace-with-a-different-32-byte-secret"
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

Logout from `/manage` revokes the SQL-backed token/OIDC session and clears local cookies. If OIDC discovery exposes `end_session_endpoint`, or if `OIDC_LOGOUT_URL` is set, the service also redirects the browser to provider logout so the provider does not silently log the user back in with an old session.

Login and publishing rate limits are stored in bounded memory per replica. Expired client buckets are removed through a priority queue; when the bound is reached, the bucket nearest to reset is evicted so new clients remain serviceable. `TRUSTED_PROXY_CIDRS` lets a replica distinguish clients behind a trusted ingress; leave it empty unless the proxy CIDRs are known. For a strict cluster-wide or adversarial limit, enforce an additional shared limit at the ingress or API gateway.

For groups, prefer OIDC groups over individual emails:

```json
{
  "team": "teamname",
  "oidc_groups": ["teamname-devops"]
}
```

In Authentik, add users to the `teamname-devops` group and ensure the application/provider sends the `groups` claim in the ID token. For team access, `oidc_groups` grants publishing access to the publishing space named after `Team`. If the team also needs other spaces, a global administrator assigns them under `/manage/admin/spaces`.

To let a team manage its own tokens and OIDC groups, add `OIDC team admins`. For one or two people, emails are convenient; when the list grows, use a group:

```json
{
  "team": "teamname",
  "oidc_groups": ["teamname-devops"],
  "oidc_team_admin_emails": ["owner@example.com"],
  "oidc_team_admin_groups": ["teamname-admins"]
}
```

A user with email `owner@example.com` or group `teamname-admins` can open `/manage/teams` and the separate Access, Tokens, and Modules pages for `teamname`. The same user cannot edit other teams, global admins, JSON config, or extra publishing spaces. Team admins can delete modules and versions only in the team's primary space. Extra publishing spaces remain publishing and update scope only; deletion there is global-admin-only.

The same OIDC team-admin email or group can be added to multiple teams. The user will be able to edit all mapped teams under `/manage/teams`, but still cannot access unrelated teams or global admin settings.

Global OIDC admin access is configured in the `Global OIDC Admins` block:

```json
{
  "team": "platform-admin",
  "oidc_admin_groups": ["forge-admins"]
}
```

Users in `forge-admins` can open `/manage/admin/access` and delete modules/versions in any namespace. Publishing access is not automatically granted by global admin groups.

If an OIDC user belongs to a team group such as `teamname-devops` and also matches `oidc_admin_groups`, `oidc_admin_emails`, or `oidc_admin_subjects`, the service combines both mappings. Global administration therefore does not remove team publishing or team-management capabilities.

To add yourself as an admin after the first startup:

1. Log in to `/manage` with `ADMIN_TOKEN`.
2. Open `/manage/admin/access`.
3. In `Global OIDC Admins`, add your email to `OIDC admin emails` or your group to `OIDC admin groups`.
4. Save global admins and log in again through OIDC.

`ADMIN_TOKEN` should be kept for bootstrap/break-glass access, for example when OIDC is unavailable. Use OIDC admin groups or emails for daily access.

Create and edit teams through `/manage/teams`, assign extra publishing spaces through `/manage/admin/spaces`, and configure global OIDC administrators through `/manage/admin/access`. The service intentionally provides no full-config JSON replacement endpoint.

Access tokens are intentionally absent from this JSON contract. Create, rotate, and revoke them in `/manage/teams/{team}/tokens`; a newly generated raw token is displayed exactly once.

When upgrading a database that still has the legacy plaintext `access_tokens.token` column, startup migrates each credential to the HMAC-backed schema. Configure and retain `ACCESS_TOKEN_PEPPER` before the first upgraded startup; existing raw credentials continue to authenticate, but losing or changing the pepper invalidates them.

## Module Publishing

The service accepts Puppet module `.tar.gz` archives through `POST /api/v1/modules` as `multipart/form-data`.

Minimum module requirements:

- the archive contains exactly one valid `metadata.json`;
- every archive entry is contained by one canonical `<owner>-<name>-<version>` root directory;
- `metadata.json` and an optional README are direct children of that root directory;
- absolute paths, parent traversal, links, devices, and other special tar entries are rejected;
- both individual entry size and total expanded archive size are bounded;
- `metadata.json.name` uses the `<owner>-<name>` format, for example `teamname-apache`;
- `owner` and the short module `name` use lowercase letters, digits, and underscores; hyphens are reserved as the unambiguous separator between those two parts;
- `metadata.json.version` is present;
- the namespace in `metadata.json.name` matches the selected `space`;
- putting `README.md` in the module is recommended because it is rendered on the HTML module page.

Example layout:

```text
teamname-apache-1.2.3/
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

Publishing accepts only the target space and archive:

```bash
export FORGE_URL="https://forge.example.com"
export PUBLISH_TOKEN="replace-me"

curl -X POST "${FORGE_URL}/api/v1/modules" \
  -H "Authorization: Bearer ${PUBLISH_TOKEN}" \
  -F "space=teamname" \
  -F "file=@pkg/teamname-apache-1.2.3.tar.gz"
```

Important details:

- publishing requires only the `space` and `file` fields;
- owner, module name, version, summary, description, and stored metadata are read exclusively from `metadata.json`;
- manual identity or metadata override fields are rejected;
- the archive namespace must equal the selected space;
- the token must have the `publish` role and access to the target publishing space. In the structured UI, the space named after `Team` is added automatically, and global administrators assign additional spaces under `/manage/admin/spaces`.
- multipart uploads, checksum calculation, object-storage writes, and release downloads are streamed; large archives are not copied into a single in-memory buffer.

List the spaces available to a token with the `publish` role:

```bash
curl "${FORGE_URL}/api/v1/manage/publish-spaces" \
  -H "Authorization: Bearer ${PUBLISH_TOKEN}"
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
- README Markdown rendering;
- module version selector;
- ready-to-copy install snippets for Puppet and r10k.

## Active Releases and Deletion

- Releases requested by r10k or `puppet module install` within `ACTIVE_RELEASE_TTL` are marked as `in use`.
- Latest and active releases cannot be deleted through API or `/manage/modules`.
- Delete buttons are hidden for protected releases in `/manage/modules`.
- `ACTIVE_RELEASE_TTL` defaults to `720h` / 30 days.
- Release usage rows older than the active window are pruned by a daily lease-elected worker, including one asynchronous pass after startup.
- Delete policy checks and SQL mutation run under the same module-scoped lock across replicas. The SQL transaction removes metadata and enqueues each local object in the durable `artifact_deletions` outbox. A singleton worker retries idempotent object deletion every minute; a storage error leaves only the object and outbox task, never metadata that points to a missing archive. Republish safety is checked under the same module lock before cleanup.
- Deleting indexed upstream metadata does not enqueue a shared `upstream-cache/` object for immediate deletion. A daily lease-elected worker removes only unreferenced objects older than `UPSTREAM_ARTIFACT_ORPHAN_TTL`.
- Deleted upstream releases are stored in the `deleted_releases` tombstone table so background synchronization does not immediately recreate them. A direct request for that exact release from r10k, Puppet, or another client deliberately restores it on demand; this keeps old environments deployable without repopulating every deleted version during refresh. `DELETED_RELEASE_TTL` controls background suppression when no client requests the release; after expiry, refresh may restore it as well.
- Deleting the whole module clears tombstones and release usage for that module, so a later upstream sync can create it again with all available upstream releases.

## Observability

The service provides:

- structured HTTP logging for all requests;
- Prometheus metrics on `GET /metrics` through the separate `METRICS_ADDR` listener;
- Grafana dashboard example in [examples/grafana/puppet-forge-dashboard.json](examples/grafana/puppet-forge-dashboard.json);
- Prometheus alert rules in [examples/prometheus/alerts/puppet-forge.yml](examples/prometheus/alerts/puppet-forge.yml).

Inventory metrics:

- `puppet_forge_modules{owner}`
- `puppet_forge_module_releases{source}`
- `puppet_forge_module_latest_releases{source}`
- `puppet_forge_module_metrics_ready`
- `puppet_forge_module_metrics_last_success_timestamp_seconds`
- `puppet_forge_module_metrics_refresh_errors_total`
- `puppet_forge_module_metrics_owners_total`
- `puppet_forge_module_metrics_truncated`

Operational metrics:

- `puppet_forge_build_info{version,go_version}`
- `puppet_forge_publish_total{result}`
- `puppet_forge_delete_total{result,kind}`
- `puppet_forge_release_usage_mark_total{result}`
- `puppet_forge_artifact_stream_total{source,result}`
- `puppet_forge_artifact_stream_bytes_total{source}`
- `puppet_forge_upstream_sync_total{result,trigger}`
- `puppet_forge_upstream_refresh_cycles_total{result}`
- `puppet_forge_upstream_refresh_duration_seconds`
- `puppet_forge_upstream_refresh_last_duration_seconds`
- `puppet_forge_upstream_refresh_last_success_timestamp_seconds`
- `puppet_forge_upstream_refresh_last_error_timestamp_seconds`
- `puppet_forge_upstream_refresh_modules{result}`
- `puppet_forge_upstream_cache_requests_total{kind,result}`

Metric notes:

- `puppet_forge_modules` exports module counts grouped by the bounded `owner` label.
- `puppet_forge_module_releases` exports locally available release counts grouped by `source`.
- `puppet_forge_module_latest_releases` exports latest-release counts grouped by `source`.
- Release-level inventory metrics are intentionally aggregated so `/metrics` remains fast with many module versions.

Per-module names and versions are deliberately not exported as Prometheus labels. Use the catalog API or Manage UI for release-level inventory.

See [METRICS.md](METRICS.md) for the full metrics reference.

Backup ordering, restore validation, and the one-shot artifact reconciliation/repair modes are documented in [BACKUP_RESTORE.md](BACKUP_RESTORE.md).

## Local Development

```bash
go test ./...
go run ./cmd/server
```

PostgreSQL parity tests are opt-in so ordinary `go test ./...` does not depend on local Docker/Postgres. To compare SQLite and PostgreSQL behavior, run:

```bash
make test-postgres
```

Object-storage integration tests are also opt-in. With MinIO and fake-gcs-server from the local Compose stack running, execute:

```bash
make test-object-storage
```

The combined target runs the S3 and GCS storage contracts. They cover immutable create-only uploads, streamed full/range reads, listing, and idempotent deletion; the S3 contract additionally verifies concurrent same-key writes and cancellation cleanup. Use `make test-s3-storage` or `make test-gcs-storage` to run one backend, and override the corresponding `PUPPET_FORGE_TEST_*_ENDPOINT` variables for other isolated emulators.

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
make test-browser
make test-postgres
make test-object-storage
make compose-up
make http-smoke
make docker-smoke
make compose-smoke
make r10k
make oidc-preflight
make helm-package
```

`make http-smoke` checks a running HTTP instance: `/healthz`, `/readyz`, the separate metrics listener, public HTML catalog, token login form, read API policy for `PUBLIC_MODULE_ACCESS`, and admin-token read/login. Override `SMOKE_BASE_URL`, `SMOKE_METRICS_URL`, `SMOKE_PUBLIC_MODULE_ACCESS`, and `SMOKE_ADMIN_TOKEN` for non-default instances.

`make docker-smoke` builds the final image and verifies its non-root UID, embedded version, help output, and OCI version label. `make compose-smoke` starts the local Compose stack in detached mode, runs HTTP smoke, runs r10k one-shot, and returns the r10k exit code.

Run `npm ci` once before `make test-browser`. CI executes the browser suite in the
pinned Playwright container, so its browser and test-runner versions remain aligned.

By default, the Makefile uses `go`, `gofmt`, and `golangci-lint` from `PATH`. Override tools with variables:

```bash
GO=go GOLANGCI_LINT=golangci-lint make check
```

## Kubernetes

The Helm chart is in [deploy/puppet-forge](deploy/puppet-forge).

Example:

```bash
helm upgrade --install puppet-forge ./deploy/puppet-forge \
  --set secret.existingSecret=puppet-forge-runtime
```

The chart requires exactly one secret mode: either provide `secret.existingSecret`,
or explicitly set `secret.create=true` and provide its required values. The default
chart does not create credentials. Create the referenced Secret through your secret
manager/GitOps controller with at least `DATABASE_DSN`, `ACCESS_TOKEN_PEPPER`, and an
independent `MANAGE_SESSION_SECRET`; add artifact and OIDC credentials required by
your configuration. For an isolated local chart evaluation, use
`-f deploy/puppet-forge/values-dev.yaml`.

When an external Secret manager updates that Secret without changing the Deployment manifest, set `secret.rolloutChecksum` to the external Secret version or digest. Changing it updates the pod template and triggers a controlled rollout.

By default, the chart uses a fixed `replicaCount`. To let Kubernetes manage the replica count, enable HPA:

```bash
helm upgrade --install puppet-forge ./deploy/puppet-forge \
  --set autoscaling.enabled=true \
  --set autoscaling.minReplicas=2 \
  --set autoscaling.maxReplicas=6
```

When `autoscaling.enabled=true`, the Deployment does not render `spec.replicas`; the `HorizontalPodAutoscaler` owns it. Do not enable autoscaling with a `sqlite://` `DATABASE_DSN`; use PostgreSQL and identical `ACCESS_TOKEN_PEPPER` and `MANAGE_SESSION_SECRET` values on every replica.

The chart runs the container as non-root with a read-only root filesystem, RuntimeDefault seccomp, no Linux capabilities, and an `emptyDir` mounted at `/tmp`. Service-account token mounting is disabled by default. A PDB and topology spreading are enabled; `networkPolicy.enabled` remains off until ingress and egress peers are explicitly supplied through `networkPolicy.ingress` and `networkPolicy.egress`.

The chart supports Prometheus Operator resources, disabled by default:

- `serviceMonitor.enabled=true` creates a `ServiceMonitor` that selects the internal metrics-only Service;
- `prometheusRule.enabled=true` creates a `PrometheusRule` with the same recording and alert rules as [examples/prometheus/alerts/puppet-forge.yml](examples/prometheus/alerts/puppet-forge.yml). Runtime thresholds can be tuned under `prometheusRule.thresholds`; the defaults match the standalone example.

## GitHub Actions

Workflow [`CI`](.github/workflows/ci.yml) runs on pull requests and branch pushes. It delegates the release gates to the reusable [`Checks`](.github/workflows/checks.yml) workflow. These gates cover formatting, static analysis, coverage, browser regressions, PostgreSQL parity, object-storage integration, packaging, security scans, and release smoke tests.

Workflow [`Release`](.github/workflows/release.yml) runs only for tags matching `v*.*.*`. It calls the same reusable checks before publishing, without the local Docker Compose config check. For tag `v1.2.3`, it builds:

- GitHub Release assets with binary archives in `dist/*.tar.gz`, `dist/checksums.txt`, and a CycloneDX SBOM in `dist/sbom.cdx.json`;
- a signed multi-arch Docker image `ghcr.io/<owner>/<repo>:v1.2.3` with BuildKit SBOM/provenance attestations; `:latest` is promoted from that version only after the binary release, versioned image, and Helm chart all publish successfully;
- Helm chart `puppet-forge-1.2.3.tgz` with `appVersion: v1.2.3`;
- Helm repository index on GitHub Pages through `helm/chart-releaser-action`.

The workflow first creates a draft GitHub Release, then publishes and signs the immutable Docker version, then publishes the Helm chart. After every versioned component succeeds, it updates source chart metadata, promotes `:latest`, and finally makes the GitHub Release public. The follow-up metadata commit uses `[skip ci]` so the normal branch CI workflow is not started just for release metadata.

For the Helm repository, create or allow the `gh-pages` branch and configure GitHub Pages to serve it. After a release:

```bash
helm repo add puppet-forge https://<owner>.github.io/<repo>
helm repo update
helm upgrade --install puppet-forge puppet-forge/puppet-forge
```

## Additional Documentation

- Runtime and multi-instance schemas: [SCHEMA.md](SCHEMA.md)
- Metrics: [METRICS.md](METRICS.md)
- Architecture: [ARCHITECTURE.md](ARCHITECTURE.md)
- Backup and restore: [BACKUP_RESTORE.md](BACKUP_RESTORE.md)
- Grafana dashboard: [examples/grafana/puppet-forge-dashboard.json](examples/grafana/puppet-forge-dashboard.json)
- Prometheus rules: [examples/prometheus/alerts/puppet-forge.yml](examples/prometheus/alerts/puppet-forge.yml)
