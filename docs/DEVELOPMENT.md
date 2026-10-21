# Development and Releases

[Project overview](../README.md) | [Українська версія](DEVELOPMENT.uk.md)

Local development, integration environments, verification, CI, and release
procedures.

## HTML UI Structure

The UI is server-rendered with Go `html/template`. Both templates and assets are
embedded in the binary; there is no frontend build step.

- `internal/httpapi/templates/layout.gohtml` owns the single document shell.
  `manage-layout.gohtml` and `public-layout.gohtml` supply family navigation and
  resources; the login page uses the shell without navigation.
- Each page defines its title, content, optional heading/subtitle, stylesheet,
  and scripts. Pages are parsed into separate template sets, so their named
  blocks cannot overwrite another page's blocks.
- Navigation, notices, list filters, pagination with page-size controls,
  publication forms, and module cards are shared template components.
- `internal/httpapi/assets/base.css` contains common tokens and controls;
  `manage.css` and `public.css` contain family styles. Page stylesheets contain
  only page-specific rules or overrides. Management components use shared
  `content-panel`, `list-panel`, `compact-fields`, and `report-panel` classes;
  page styles retain their spacing and responsive differences.
- JavaScript lives in `internal/httpapi/assets/*.js`. Layouts load shared
  controllers once with `defer`; section persistence reads the storage key from
  the body rather than interpolating Go values into JavaScript.
- `page-size-persistence.js` owns preference validation and cookie/local-storage
  access and loads before `async-lists.js`. All current pagination controls use
  async lists; there is no second controller that reloads the whole page.
  `clipboard.js` shares copying and temporary button feedback across token and
  module pages.
- `/assets/` serves embedded CSS and JavaScript without authentication, supports
  `GET`/`HEAD`, and uses `Cache-Control: no-cache` for deployment revalidation.
  Script tags retain the response's CSP nonce.

Keep shared behavior in components and controllers rather than copying document
heads, styles, or scripts into a new page. Template tests cover the document and
asset contract; browser tests exercise the rendered controllers.

## Local Development

```bash
go test ./...
go run ./cmd/server
```

PostgreSQL parity tests are opt-in so ordinary `go test ./...` does not depend on local Docker/Postgres. To compare SQLite and PostgreSQL behavior, run:

```bash
make test-postgres
```

Object-storage integration tests are also opt-in. With SeaweedFS and fake-gcs-server from the local Compose stack running, execute:

```bash
make test-object-storage
```

The combined target runs the S3 and GCS storage contracts. They cover immutable create-only uploads, streamed full/range reads, listing, and idempotent deletion; the S3 contract additionally verifies concurrent same-key writes and cancellation cleanup. Use `make test-s3-storage` or `make test-gcs-storage` to run one backend, and override the corresponding `PUPPET_FORGE_TEST_*_ENDPOINT` variables for other isolated emulators.

`make coverage-check` enforces 65% total unit coverage plus package-specific floors for critical business logic. With PostgreSQL, SeaweedFS, and fake-gcs-server available, `make coverage-integration-check` runs the unit and integration suites, merges their atomic coverage profiles without duplicating statements, and requires 70% total coverage while retaining the package-specific floors. CI produces these profiles in isolated jobs and checks their merged result.

For a self-contained local run, `make coverage-integration-docker` starts disposable PostgreSQL, SeaweedFS, fake-gcs-server, and Go runner containers in an isolated Docker network. It uses the services' standard internal ports, waits for readiness, runs the same combined gate, and removes the containers, anonymous volumes, and network on success, failure, or interruption.

## Docker Compose

Start the local stack:

```bash
docker compose up --build
```

The local stack exposes:

- Puppet Forge at `http://localhost:8080`;
- Prometheus at `http://localhost:9091`;
- Grafana at `http://localhost:3001`, using `admin` / `admin` by default.

Grafana starts with the Prometheus data source and Puppet Forge dashboard already provisioned. Override its port with `GRAFANA_PORT` and the local credentials with `GRAFANA_ADMIN_USER` and `GRAFANA_ADMIN_PASSWORD`.

Compose starts:

- `app` with this service;
- `postgres`, with tables created by the service on startup;
- `fake-gcs-server` as a local GCS emulator;
- `seaweedfs` for local S3-compatible object storage;
- `r10k`, based on `puppet/r10k`, running `puppetfile install` as a one-shot container.

Local storage backend switches:

- `ARTIFACT_BACKEND=gcs` uses `fake-gcs-server` through `ARTIFACT_ENDPOINT=http://gcs:4443`;
- `ARTIFACT_BACKEND=s3` uses SeaweedFS through `ARTIFACT_ENDPOINT=http://seaweedfs:8333`.

The pinned SeaweedFS image runs `weed mini` with a separate `seaweedfs-data`
volume. It creates the `puppet-forge-artifacts` bucket and local-only credentials
`forge-local` / `forge-local-secret`; the host S3 endpoint is `http://localhost:8333`.
WebDAV, the admin UI, telemetry, automatic bucket creation on upload, and recursive
deletion of nonempty buckets are disabled. Startup waits for the S3 `/readyz` probe.
`S3_TEST_IMAGE` selects the image used by disposable integration services.

This replaces MinIO only in development and CI; Forge still uses the generic S3
adapter. Existing MinIO volumes are not reused or deleted. Before switching an
existing S3-backed instance, copy its objects through S3 APIs or use a separate
development database: existing metadata does not migrate artifacts automatically.
Production GCS deployments are unchanged. Contract tests verify create-only writes
from eight independent clients, range reads, object metadata, listing, and deletion.

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

### Local OIDC

Compose reads OIDC settings from the environment or `.env`; it defaults to
`WEB_AUTH_MODE=none` so credentials are not stored in YAML. Start from
[`.env.example`](../.env.example). For a local Authentik instance, use a hostname the
browser can resolve, such as `forge.127.0.0.1.nip.io`, and register this exact
redirect URI:

```text
http://forge.127.0.0.1.nip.io:8080/auth/callback
```

The issuer must also be reachable from the `app` container. With Docker Desktop
and an identity provider running on the host, use `host.docker.internal` for the
issuer host while keeping the browser-facing Forge URL as the redirect URI.

## Authentication Diagnostics

The audit event `oidc_login` records success only after the callback verifies the
ID token and creates the local session. It uses the verified name/email as the
actor and retains `oidc_subject` for correlation. Starting a login is not audited
as successful authentication; old `oidc_login_start` records remain historical.
Login events retain the user's team memberships at login time in `actor_teams`.
The same event appears once in the global audit and in each matching team's
audit. Global administrator access alone does not grant membership in every team.
Older login events without this attribute are not retroactively assigned to teams.

OIDC storage failures log the operation, `request_id`, and underlying database
error. Expired, revoked, or missing sessions do not produce error logs.
Protocol failures log their stage, error type, upstream HTTP status when available,
and recognized OAuth error codes. Raw token responses, authorization codes, JWTs,
and session cookies are not included in these diagnostics.

Management forms and async mutations use the same server-error classification.
Unexpected failures are logged once at the response boundary. HTML responses show
a generic message with `request_id`, not the underlying database or storage error;
async responses return the corresponding status and request ID as JSON.
Validation messages remain visible without generating server-error logs.

Proxy and artifact-stream failures include the request ID and available resource
identity; stream logs also include the outcome and transferred bytes. Client
cancellations and successful `/healthz` and `/readyz` requests are logged at DEBUG.
Failed probes remain visible at INFO, with the underlying service error at ERROR.
Recovered handler panics include a server-side stack trace, never in the response.

## Makefile

Common commands:

```bash
make help
make check
make test-browser
make test-postgres
make test-object-storage
make coverage-check
make coverage-integration-check
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

`make test-browser` starts the Go rendering fixture on the host and runs the browser
suite in the pinned official Playwright Docker image. Dependencies are installed in
an ephemeral container filesystem, so no local Node.js, npm, or Chromium installation
is required and the browser and test-runner versions remain aligned locally and in CI.
Each run builds a fresh fixture binary, binds an available port, and stops that
process during cleanup; an existing server is never reused implicitly. To use an
external fixture, explicitly set both `BROWSER_FIXTURE_URL` (host access) and
`BROWSER_CONTAINER_FIXTURE_URL` (container access).

By default, the Makefile uses `go`, `gofmt`, and `golangci-lint` from `PATH`. Override tools with variables:

```bash
GO=go GOLANGCI_LINT=golangci-lint make check
```

`GOSEC_VERSION` temporarily pins upstream commit
`14ef84f6feccf40be7f5092fd2db119bf3f5f836`, which uses `golang.org/x/tools v0.52.0`.
The latest tagged gosec release, `v2.29.0`, uses `v0.49.0` and fails to decode
Go 1.27.2 export data. Keep security scans enabled; replace the commit pin with
a compatible tagged release when available. Package-loading or SSA errors mean
the scan failed, even when its summary reports zero issues.

`make helm-lint` validates unreleased chart sources with the development values
matrix; those values are not intended for production deployment.

## GitHub Actions

Workflow [`CI`](../.github/workflows/ci.yml) runs on pull requests and branch pushes. It delegates the release gates to the reusable [`Checks`](../.github/workflows/checks.yml) workflow. These gates cover formatting, static analysis, coverage, browser regressions, PostgreSQL parity, object-storage integration, packaging, security scans, and release smoke tests.

Workflow [`Release`](../.github/workflows/release.yml) runs only for tags matching `v*.*.*`. It calls the same reusable checks before publishing, without the local Docker Compose config check. For tag `v1.2.3`, it builds:

- GitHub Release assets with binary archives in `dist/*.tar.gz`, `dist/checksums.txt`, and a CycloneDX SBOM in `dist/sbom.cdx.json`;
- a signed multi-arch Docker image `ghcr.io/<owner>/<repo>:v1.2.3` with BuildKit SBOM/provenance attestations; `:latest` is promoted from that version only after the binary release, versioned image, and Helm chart all publish successfully;
- Helm chart `puppet-forge-1.2.3.tgz` with `appVersion: v1.2.3`;
- Helm repository index on GitHub Pages through `helm/chart-releaser-action`.

The workflow first verifies that committed Helm `version` and `appVersion` match the release tag. It then creates a draft GitHub Release, publishes and signs the immutable Docker version, and publishes the unmodified chart from the tagged commit. After every versioned component succeeds, it promotes `:latest` and makes the GitHub Release public.

All publication steps are safe to retry for the same tag: the GitHub Release is edited in place, the versioned image remains content-addressed, and chart-releaser skips an existing chart package. Do not recreate the tag or rebuild different artifacts under the same version.

Before publishing, update `deploy/puppet-forge/Chart.yaml` so `version` equals the release version without the leading `v` and `appVersion` equals the complete tag. Commit that change, then run the complete local release gate with the same canonical version. The gate starts isolated PostgreSQL, SeaweedFS, and fake-gcs-server containers for combined integration coverage and always removes them afterward:

```bash
make release-preflight VERSION=v1.2.3
```

After it succeeds, the guarded release target requires a clean `main` worktree, rejects existing local or remote tags, pushes `main`, verifies the remote commit, creates an annotated tag, and pushes it to start the Release workflow:

```bash
make push-release VERSION=v1.2.3
```

The release workflow publishes the chart index and package to the configured
`gh-pages` branch. Verify a published release with:

```bash
helm repo update puppet-forge
helm search repo puppet-forge/puppet-forge --versions
```

## Additional Documentation

- Runtime and multi-instance schemas: [SCHEMA.md](SCHEMA.md)
- Metrics: [METRICS.md](METRICS.md)
- Architecture: [ARCHITECTURE.md](ARCHITECTURE.md)
- Backup and restore: [BACKUP_RESTORE.md](BACKUP_RESTORE.md)
- Grafana dashboard: [examples/grafana/puppet-forge-dashboard.json](../examples/grafana/puppet-forge-dashboard.json)
- Prometheus rules: [examples/prometheus/alerts/puppet-forge.yml](../examples/prometheus/alerts/puppet-forge.yml)
