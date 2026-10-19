# Development and Releases

[Project overview](../README.md) | [Українська версія](DEVELOPMENT.uk.md)

Local development, integration environments, verification, CI, and release
procedures.

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

`make coverage-check` enforces 65% total unit coverage plus package-specific floors for critical business logic. With PostgreSQL, MinIO, and fake-gcs-server available, `make coverage-integration-check` runs the unit and integration suites, merges their atomic coverage profiles without duplicating statements, and requires 70% total coverage while retaining the package-specific floors. CI produces these profiles in isolated jobs and checks their merged result.

For a self-contained local run, `make coverage-integration-docker` starts disposable PostgreSQL, MinIO, fake-gcs-server, and Go runner containers in an isolated Docker network. It uses the services' standard internal ports, waits for readiness, runs the same combined gate, and removes the containers, anonymous volumes, and network on success, failure, or interruption.

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

Before publishing, update `deploy/puppet-forge/Chart.yaml` so `version` equals the release version without the leading `v` and `appVersion` equals the complete tag. Commit that change, then run the complete local release gate with the same canonical version. The gate starts isolated PostgreSQL, MinIO, and fake-gcs-server containers for combined integration coverage and always removes them afterward:

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
