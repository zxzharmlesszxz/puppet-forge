# Metrics

[Українська версія](METRICS.uk.md)

> **Runtime context:** [SCHEMA.md](SCHEMA.md) explains which metrics are local to
> a replica, which gauges describe shared state, and how to aggregate them in a
> multi-instance deployment.

## Build Metric

### `puppet_forge_build_info`

- Type: gauge fixed at `1` for the running build
- Labels:
  - `version`: application version injected during the build
  - `go_version`: Go runtime version

## HTTP Metrics

### `puppet_forge_http_requests_total`

- Type: counter
- Value: total number of processed HTTP requests
- Labels:
  - `method`
  - `route`
  - `status`

### `puppet_forge_http_request_duration_seconds`

- Type: histogram
- Value: request duration in seconds
- Labels:
  - `method`
  - `route`
  - `status`
- Notes: uses Prometheus default histogram buckets

### `puppet_forge_http_panics_total`

- Type: counter
- Value: total number of recovered HTTP handler panics
- Labels:
  - `method`
  - `route`
- Notes:
  - panics before response headers are written are also recorded in `puppet_forge_http_requests_total` with status `500`
  - panics after response headers are written cannot change the HTTP status already sent to the client, but still increment this counter

### `puppet_forge_http_in_flight_requests`

- Type: gauge
- Value: current number of active HTTP requests
- Labels: none

Normalized `route` values currently include:

- `/`
- `/healthz`
- `/readyz`
- `/manage`
- `/manage/*`
- `/auth/*`
- `/api/v1/modules`
- `/api/v1/modules/`
- `/api/v1/modules/*`
- `/modules/*`
- `/v3/files/*`
- `/v3/*`
- `other`

## Inventory Metrics

### `puppet_forge_modules`

- Type: gauge
- Value: number of locally indexed modules for the owner
- Labels:
  - `owner`
- Notes:
  - per-module names and versions are not exported to avoid high-cardinality series

### `puppet_forge_module_releases`

- Type: gauge
- Value: number of locally indexed releases grouped by source
- Labels:
  - `source`
- Notes:
  - intentionally aggregated to keep `/metrics` scrape size and scrape latency bounded

### `puppet_forge_module_latest_releases`

- Type: gauge
- Value: number of latest known module releases grouped by source
- Labels:
  - `source`

### Module inventory collector health

The inventory collector refreshes its bounded snapshot in the background. Prometheus
scrapes only that in-memory snapshot and therefore never wait for SQL queries.

- `puppet_forge_module_metrics_ready`: `1` after the first successful refresh, otherwise `0`
- `puppet_forge_module_metrics_last_success_timestamp_seconds`: Unix timestamp of the last successful refresh
- `puppet_forge_module_metrics_refresh_errors_total`: failed background refresh attempts
- `puppet_forge_module_metrics_owners_total`: owners found before applying `METRICS_MODULE_LIMIT`
- `puppet_forge_module_metrics_truncated`: `1` when owner-labeled series were truncated by the configured limit

Each service process maintains its own snapshot. Use the existing Prometheus `instance`
label to diagnose a stale or failing replica.

## Operation Metrics

### `puppet_forge_publish_total`

- Type: counter
- Value: total number of module publish attempts handled by the service layer
- Labels:
  - `result`: `success` or `error`

### `puppet_forge_delete_total`

- Type: counter
- Value: total number of delete attempts
- Labels:
  - `result`: `success` or `error`
  - `kind`: `module` or `release`

### `puppet_forge_release_usage_mark_total`

- Type: counter
- Value: total number of release usage mark attempts
- Labels:
  - `result`: `success` or `error`

Release usage marks are written when clients request a concrete release or download its archive through the API or compatible `/v3/releases/*` and `/v3/files/*` routes. Listing module metadata does not mark its latest release as active. Repeated observations are coalesced to at most one SQL write per release per minute on each replica. Therefore, the counter records attempted store writes rather than every matching HTTP request. The Manage UI uses the same shared release usage data to hide delete actions for in-use releases.

### `puppet_forge_artifact_deletion_total`

- Type: counter
- Value: durable local-artifact deletion processing attempts
- Labels:
  - `result`: `deleted`, `canceled`, or `error`

`canceled` means the same content-addressed path became referenced by a republished release before cleanup; the worker removed the stale outbox task without deleting the live object.
An `error` leaves the task pending and defers its next attempt for one minute so
one failing object cannot starve later queue entries.

### `puppet_forge_artifact_deletions_pending`

- Type: gauge
- Value: local artifact deletions currently waiting in the shared SQL outbox
- Multi-replica aggregation: use `max by (job)`, because every replica reads the same durable queue

### `puppet_forge_artifact_stream_total`

- Type: counter
- Value: completed artifact response streams, including failures after HTTP headers were sent
- Labels:
  - `source`: `local` or `upstream_cache`
  - `result`: `success`, `client_cancel`, or `error`

An HTTP request can already have status `200` or `206` when object storage or the
client connection fails during streaming. Use this counter, rather than only HTTP
status metrics, to detect truncated archives and likely client checksum failures.
`PuppetForgeArtifactStreamErrors` alerts only on `result="error"`; client cancellations
remain visible in the dashboard but do not page operators.

### `puppet_forge_artifact_stream_bytes_total`

- Type: counter
- Value: artifact response bytes successfully written, grouped by `source`
- Labels:
  - `source`: `local` or `upstream_cache`

## Upstream Metrics

### `puppet_forge_upstream_sync_total`

- Type: counter
- Value: total number of upstream module sync attempts. A background refresh cycle increments this once per module it tries to sync, not once per refresh cycle.
- Labels:
  - `result`: `success` or `error`
  - `trigger`: `single` or `refresh`

### `puppet_forge_upstream_refresh_cycles_total`

- Type: counter
- Value: total number of upstream refresh cycles
- Labels:
  - `result`: `success` or `error`

### `puppet_forge_upstream_refresh_duration_seconds`

- Type: histogram
- Value: duration of upstream refresh cycles in seconds
- Notes: uses Prometheus default histogram buckets

### `puppet_forge_upstream_refresh_last_duration_seconds`

- Type: gauge
- Value: duration of the most recent upstream refresh cycle in seconds

### `puppet_forge_upstream_refresh_last_timestamp_seconds`

- Type: gauge
- Value: Unix timestamp of the most recent completed upstream refresh cycle, regardless of its result
- Multi-replica use: select the duration and module-result gauges from the replica with the greatest timestamp; alert when the timestamp remains zero or becomes older than the configured refresh-health window

### `puppet_forge_upstream_refresh_last_success_timestamp_seconds`

- Type: gauge
- Value: Unix timestamp of the last upstream refresh cycle that completed without per-module errors

### `puppet_forge_upstream_refresh_last_error_timestamp_seconds`

- Type: gauge
- Value: Unix timestamp of the last upstream refresh cycle that had at least one per-module error

### `puppet_forge_upstream_refresh_modules`

- Type: gauge
- Value: number of modules from the most recent upstream refresh cycle
- Labels:
  - `result`: `attempted`, `success`, or `error`

### `puppet_forge_upstream_cache_requests_total`

- Type: counter
- Value: total number of upstream cache decisions
- Labels:
  - `kind`: `json` or `artifact`
  - `result`: `hit`, `miss`, `stale`, or `bypass`
- Notes:
  - JSON `stale` is emitted only when the cached response is expired but still inside `UPSTREAM_PROXY_JSON_STALE_TTL`

### `puppet_forge_upstream_artifact_integrity_total`

- Type: counter
- Value: outcomes of coalesced upstream artifact cache integrity operations
- Labels:
  - `result`: `valid`, `repaired`, or `error`
- Notes:
  - `valid` means the cached object already matched the expected checksum and size
  - `repaired` means a missing or invalid object was fetched and passed the subsequent integrity check
  - concurrent checks for the same object on one replica are coalesced and counted once

### `puppet_forge_upstream_artifact_cleanup_total`

- Type: counter
- Value: outcomes of upstream artifact cache cleanup cycles
- Labels:
  - `result`: `success` or `error`
- Note: only the lease-elected worker emits a cleanup outcome

### `puppet_forge_upstream_artifact_cleanup_objects_total`

- Type: counter
- Value: number of unreferenced `upstream-cache/` objects deleted after they exceeded `UPSTREAM_ARTIFACT_ORPHAN_TTL`
- Labels: none
- Note: successful deletions are counted even if a later object makes the cleanup cycle fail

### `puppet_forge_upstream_artifact_cleanup_scanned_total`

- Type: counter
- Value: number of `upstream-cache/` objects inspected by cleanup cycles
- Labels: none
- Note: includes recent and referenced objects that were intentionally retained

### `puppet_forge_upstream_artifact_cleanup_failures_total`

- Type: counter
- Value: number of individual `upstream-cache/` objects that a cleanup cycle failed to delete
- Labels: none
- Note: the worker continues with later objects, while the cycle also increments `puppet_forge_upstream_artifact_cleanup_total{result="error"}`

### `puppet_forge_upstream_artifact_cleanup_duration_seconds`

- Type: histogram
- Value: duration of lease-elected upstream artifact cache cleanup cycles
- Labels: none
- Note: records both successful and failed cycles

## Cardinality Notes

Inventory metrics are aggregated by bounded enums or module owner. Per-module names, release versions, users, tokens, request IDs, and error text are never metric labels.
