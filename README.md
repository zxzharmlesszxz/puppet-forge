# Puppet Forge Service

> **Operational map:** [SCHEMA.md](docs/SCHEMA.md) documents single-instance and
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
- `POST /v3/releases` accepts the Puppet Forge V3 JSON/base64 upload used by PDK 3.4 and later;
- `GET /v3/modules` and `GET /v3/releases` expose the combined local and indexed-upstream catalog with Forge-compatible filtering and pagination;
- `GET /api/v1/manage/publish-spaces` lists spaces available to the authenticated principal with publishing rights;
- `GET /api/v1/modules?limit=20&offset=0` lists modules and returns `items`, `limit`, `offset`, and `total`;
- `GET /api/v1/modules/{owner}/{name}` returns a module card;
- `DELETE /api/v1/modules/{owner}/{name}` deletes a module through global-admin or team-admin access;
- `GET /api/v1/modules/{owner}/{name}/versions/{version}` returns a specific release;
- `DELETE /api/v1/modules/{owner}/{name}/versions/{version}` deletes a specific release through global-admin or team-admin access;
- `GET /api/v1/modules/{owner}/{name}/versions/{version}/download` serves the stored artifact;
- `GET /` renders the public HTML catalog of readable local and indexed-upstream modules;
- `GET /modules/{owner}/{name}` renders a module page with README Markdown, version selector, and install snippets;
- V3 reads use the local catalog and archive cache where supported; remaining `GET /v3/*` and `HEAD /v3/*` routes proxy the official Puppet Forge API;
- `/manage` provides a read-only overview of modules and manageable teams;
- `/manage/modules` provides cross-team module publishing, deletion, search, pagination, and upstream imports;
- administrative release review provides archive browsing, syntax highlighting, and server-side comparison with SHA256-bound approval; see [Release Review](docs/RELEASE_REVIEW.md);
- `/manage/teams` lists manageable teams; `/manage/teams/{team}/access`, `/tokens`, `/modules`, `/legacy`, and `/audit` provide separate team-scoped management pages;
- paginated public and Manage lists update only their own rows, counters, and controls through progressive HTML fragment requests; filters and browser history use the same mechanism, ordinary navigation remains the fallback, and page size persists with a choice of 10, 20, 50, or 100 items;
- `/manage/admin/access`, `/manage/admin/spaces`, and `/manage/admin/audit` provide structured global configuration and the complete persistent security audit log; each team page exposes only events targeting that team or its publish spaces;
- `ADMIN_TOKEN` provides bootstrap/break-glass access;
- OIDC login supports global admins, team admins, and team readers; CI publishes new Pending releases using publish tokens;
- The HTML catalog and module descriptions are public; `PUBLIC_MODULE_ACCESS` controls anonymous install/API access;
- active release tracking blocks accidental deletion of recently used releases while administrators retain an explicit audited override; inactive latest releases may be deleted;
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

1. CI uploads a `.tar.gz` module artifact through multipart `POST /api/v1/modules` with an optional `space`, or through PDK's JSON/base64 `POST /v3/releases`.
2. The service authenticates the principal and reads identity and metadata from the archive.
3. The service verifies that the principal may publish to the metadata owner. An explicit `space` must match that owner.
4. MD5, SHA-256, and size are calculated once. The archive is created without overwrite at `modules/<owner>/<name>/<version>/<sha256>.tar.gz`.
5. Bearer-token publishing keeps local release identity immutable: retrying identical bytes is idempotent, while different bytes for an existing module/version return `409 Conflict`. A global or owning team administrator may explicitly select **Replace existing release** in the management UI to correct a bad archive without disabling active-release protection.
6. A new API upload creates a Pending release in SQL, excluded from ordinary catalogs and downloads until a team or global administrator approves it. The first release's approval also makes its Pending module Available. Administrators retain an emergency web upload with an explicit allow decision and mandatory reason. Definite persistence failures compensate the uncommitted object.

Proxy flow:

1. A client calls `/v3/...` on this service.
2. Supported catalog and release routes read local SQL first. A concrete unknown upstream module is indexed on demand; the original filters and pagination are applied locally. Required metadata is repaired only for releases on the returned page, without downloading every archive. Pending and Blocked restrictions still apply.
3. Routes requiring the official Forge API use the upstream proxy and its in-memory JSON cache for `UPSTREAM_PROXY_JSON_CACHE_TTL`. Known local catalog reads do not unconditionally contact upstream. V3 artifact and pagination links use relative paths.
4. File downloads through `/v3/files/...` stream into a create-only object under `upstream-cache/` on first use, then stream back through this service only after the shared object is complete. The proxy does not retain the full tarball in memory.
5. If Forge advertises a release before its `/v3/releases/...` metadata becomes available, the service verifies `/v3/files/...tar.gz`, derives both checksums from that artifact, and serves a complete local release response instead of exposing the transient metadata 404 to r10k.

## Documentation

- Deployment and operations: [DEPLOYMENT.md](docs/DEPLOYMENT.md)
- Development, testing, CI, and releases: [DEVELOPMENT.md](docs/DEVELOPMENT.md)
- Runtime and multi-instance schemas: [SCHEMA.md](docs/SCHEMA.md)
- Metrics: [METRICS.md](docs/METRICS.md)
- Architecture: [ARCHITECTURE.md](docs/ARCHITECTURE.md)
- Release review and comparison: [RELEASE_REVIEW.md](docs/RELEASE_REVIEW.md)
- Backup and restore: [BACKUP_RESTORE.md](docs/BACKUP_RESTORE.md)
