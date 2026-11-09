# Metrics

[Українська версія](METRICS.uk.md)

> **Runtime context:** [SCHEMA.md](SCHEMA.md) explains which metrics are local to
> a replica, which gauges describe shared state, and how to aggregate them in a
> multi-instance deployment.

## Lifecycle, Access, and Audit

### `puppet_forge_resources`

- Type: cached gauge; labels: `kind` (`module`, `release`), `source` (`local`,
  `upstream`, `unknown`), and `state` (`available`, `pending`, `deprecated`, `blocked`, `unknown`).
- Counts existing catalog rows, including resources hidden from public reads.
  Deleted rows and retained blocks without catalog data are not counted.
- A module has its own moderation state. A release includes parent restrictions:
  Blocked wins, then Pending, then Deprecated, otherwise Available. Module
  source follows recorded origin, preferring local if historical origins overlap.
- `latest` and observed usage are independent of lifecycle. Neither creates a new
  state. A deprecated release is still readable; Pending and Blocked are not.
- Empty combinations are exported as zero after a successful refresh. Unexpected
  label values are collapsed to `unknown`; no owner/module/version labels are added.
- Shares `METRICS_REFRESH_INTERVAL`, readiness, staleness, and last-good-cache
  behavior with existing module metrics. SQL aggregation occurs during refresh,
  never during a Prometheus scrape. Existing inventory metrics remain unchanged.
- Deduplicate shared gauges across replicas; sum counters across replicas:

```promql
sum by (kind, state) (
  max by (job, kind, source, state) (puppet_forge_resources)
)
```

### `puppet_forge_resource_oldest_pending_timestamp_seconds`

Cached gauge with `kind` and `source` labels. Unix timestamp, at second precision,
of the oldest Pending state in each group; zero means an empty approval queue.
Parent Pending timestamps apply to releases restricted by that parent. Across
replicas, use the minimum nonzero timestamp and check the current pending count.
This is state age, not time since the last archive download. Timestamp zero must
not be subtracted from `time()` as if it described a pending resource.

### `puppet_forge_control_operations_total`

Counter with bounded `operation`, `kind`, and `result` labels. Operations are
`approve`, `deprecate`, `restore`, `block`, `unblock`, `clear_cache`, `delete`,
`force_delete`, `review`, `compare`, and `review_download`; unexpected values map
to `other`. Results are `success`, `rejected`, or `error`.

Records completed service calls, not requests rejected before the service is
invoked. Permission, validation, lifecycle, conflict, review capacity/limit,
missing-resource, and client-cancellation refusals are `rejected`; SQL/storage
failures and timeouts are `error`. Mutations count success after commit. Review
counts cover archive inspection, not later HTML highlighting or response streams;
`review_download` covers opening a Pending archive for administrator review.
Publish attempts retain the existing `puppet_forge_publish_total` metric.

Clear cache success means paths were detached and cleanup tasks committed, not
that objects were physically removed. Use `puppet_forge_artifact_deletion_total`
and `puppet_forge_artifact_deletions_pending` for actual worker outcomes.
Shared or re-referenced archive objects may be preserved intentionally.

### `puppet_forge_authorization_checks_total`

Counter with bounded `scope` and `result` (`success`, `rejected`, `error`). Scopes
are `read`, `publish`, `delete`, `manage`, `token_active`, and shared rate-limit
gates prefixed with `rate_limit_`: `publish`, `manage-login`, `manage-publish`,
`auth-login`, and `auth-callback`. Unknown scopes map to `other`.
Counts gate decisions, not unique requests: basic token authentication and the
active-token SQL check are separate gates. Public read and successful OIDC read
checks are included; optional token attribution on public reads is not another gate.
Ordinary denials and rate-limit exhaustion are not internal failures. Backend
failures remain fail-closed. Route-level 401/403/429 and other permission refusals
remain available through HTTP metrics; not every handler-specific permission
condition has an additional gate counter.

### `puppet_forge_access_config_refresh_total`

Counter with `result=success|error`. Counts actual SQL/config refresh attempts,
excluding TTL cache hits, coalesced requests, and lock waits that never refresh.
It exposes backend failures even while a usable cached authorization snapshot
still allows requests.

### `puppet_forge_audit_writes_total`

Counter with `result=success|error`. Counts standalone audit insert attempts and
transactional audit persistence through the enclosing commit. Rollback/commit
failure is not success, and successful transactional writes are not counted again
when the handler logs the audit event. It does not count historical audit rows.
Nontransactional login/security actions may succeed even if their audit insert
fails; inspect logs when this counter reports errors.

### `puppet_forge_security_events_total`

Counter with bounded `action` and `result=success|failure` labels. Covers audit
events for login/logout, team/access/token changes, publication/replacement,
moderation, review, deletion, upstream import, and Clear cache. Unknown action or
result values map to `other`. This counts events independently of audit persistence;
actors, raw tokens, token IDs, email, IP, reasons, and diffs remain in the audit
log, never in these labels. It is not a complete count of all HTTP rejections.

### Alerts and Dashboard

Standalone rules and the Helm `PrometheusRule` include replica-deduplicated
resource recording rules, an informational approval reminder after 24 hours
(plus 30 minutes pending), internal administrative/auth backend failures,
sustained authorization rejections, audit persistence failures, access-config
refresh failures, and unexpected lifecycle states. Available, Deprecated, and
Blocked counts alone do not imply a service outage.

Helm target/scrape alerts match the chart's metrics Service job name, including
`nameOverride` and `fullnameOverride`. Set `prometheusRule.targetJobRegex` only
when an external scrape configuration assigns a different job name. Standalone
rules default to `job=~".*puppet-forge.*"`; adapt that selector to your scrape
jobs. Grafana discovers Forge jobs from its HTTP metric and expands **All** to
the discovered names, without requiring `forge` in the name.

Helm thresholds are `prometheusRule.thresholds.pendingApprovalMaxAgeSeconds`
(86400), `authorizationRejectionMinimumRate` (1 gate/s), and
`authorizationRejectionRatio` (0.2). Both rejection thresholds must be exceeded
for 10 minutes. An audit persistence error is critical; ordinary validation or
permission refusals do not trigger the internal-operation error alert.

The Grafana `Lifecycle & Access` tab shows current state counts, approval age,
administrative outcomes, Clear cache and forced deletions, authorization gates,
audit persistence, access-config refresh, security events, and inventory freshness.
State colors are green/amber/purple/red for Available/Pending/Deprecated/Blocked.
Current inventory and legacy-consumer panels use instant gauges, not historical
`last_over_time` samples that could resurrect removed label sets. Counters still
show activity over the selected time range. Refresh lag remains bounded by the
configured collector interval; a replica with a stale snapshot can temporarily
retain old counts, so monitor freshness alongside inventory.

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

### Release consumers

- `puppet_forge_release_consumer_last_seen_timestamp_seconds{consumer_team,consumer,consumer_role,owner,module,version,latest_version,status}` records the latest concrete archive request attributed to a named stored access token.
- `puppet_forge_release_consumer_observations{consumer_team,consumer,consumer_role,owner,module,version,latest_version,status}` exposes retained observations coalesced to at most one write per minute on each replica. It is a gauge backed by shared SQL, not an exact HTTP request counter.
- `puppet_forge_release_consumer_mark_total{result}` counts SQL mark attempts made after throttling.
- `puppet_forge_release_consumer_series_total` reports all retained consumer records before the export limit.
- `puppet_forge_release_consumer_metrics_truncated` is `1` when `METRICS_RELEASE_CONSUMER_LIMIT` omits older records from the snapshot.

`status` is `latest`, `legacy`, or `unknown` when the module no longer has a known latest release. Only the most recently observed version for each named token and module is exported, so downloading the latest release clears an older legacy signal; downloading an old release again restores it. This model assumes that token names identify distinct projects or environments. `consumer_team` is the team owning the access token, `consumer` is its operator-assigned name, and `consumer_role` is `read` or `publish`. Raw token values, hashes, prefixes, OIDC subjects, and email addresses are never exported. Anonymous public access, OIDC, bootstrap-admin, unnamed, and legacy plaintext credentials are intentionally not attributed. A valid named bearer token is still attributed when `PUBLIC_MODULE_ACCESS=true`; an invalid token does not block otherwise public access. Rows are retained for `RELEASE_CONSUMER_TTL` (180 days by default), while the collector refreshes an in-memory snapshot and performs no SQL during Prometheus scrapes.

The Grafana `Legacy Usage` tab lists these records with used and latest versions. Use the dashboard's ad hoc `Filters` variable for `consumer_team`, `consumer`, `consumer_role`, `owner`, `module`, or `version`; a uniquely named token gives its team the direct project/environment lookup without adding a separate project identity to Forge.

Global administrators can inspect current legacy signals at `/manage/admin/legacy`. Team readers and administrators have the same view scoped to tokens owned by their team at `/manage/teams/<team>/legacy`. Each view selects the most recently observed version for every named token and module directly from retained SQL history, uses server-side filtering and pagination, and is not constrained by `METRICS_RELEASE_CONSUMER_LIMIT`, which only bounds the Prometheus snapshot.

`legacy` means different from the current latest readable release, not
necessarily a lower semantic version. Blocking a newer version may move latest
backwards. It is an observation, not evidence that a project still deploys it.

### `puppet_forge_artifact_deletion_total`

- Type: counter
- Value: durable artifact deletion processing attempts for local and upstream paths
- Labels:
  - `result`: `deleted`, `canceled`, or `error`

`canceled` means the same content-addressed path became referenced by a republished release before cleanup; the worker removed the stale outbox task without deleting the live object.
An `error` leaves the task pending and defers its next attempt for one minute so
one failing object cannot starve later queue entries.

### `puppet_forge_artifact_deletions_pending`

- Type: gauge
- Value: artifact deletions currently waiting in the shared SQL outbox, including cleared upstream archive copies
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

### `puppet_forge_upstream_refresh_enabled`

- Type: gauge; `1` when this replica has background upstream refresh enabled,
  `0` when disabled (`UPSTREAM_SYNC_INTERVAL=0s`). This is configuration, not
  current lease ownership or proof of a successful cycle.
- The stalled-worker alert considers only enabled replicas and uses their
  greatest completion timestamp. A healthy elected worker prevents duplicate
  alerts from idle followers; disabled replicas cannot hide an enabled worker's
  failure. Disabled workers do not alert. Keep the maximum-age threshold longer
  than the configured refresh interval.

### `puppet_forge_upstream_refresh_last_duration_seconds`

- Type: gauge
- Value: duration of the most recent upstream refresh cycle in seconds

### `puppet_forge_upstream_refresh_last_timestamp_seconds`

- Type: gauge
- Value: Unix timestamp of the most recent completed upstream refresh cycle, regardless of its result
- Multi-replica use: select the duration and module-result gauges from the replica with the greatest timestamp. For worker-health alerts, first filter to `puppet_forge_upstream_refresh_enabled == 1`; then alert on zero or stale timestamps.

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

Most inventory metrics are aggregated by bounded enums or module owner. Release-consumer metrics are the deliberate bounded exception: they label operator-visible token names and module versions, cap the cached export at `METRICS_RELEASE_CONSUMER_LIMIT`, expose truncation explicitly, and retain SQL rows only for `RELEASE_CONSUMER_TTL`. Raw credentials, token IDs/digests/prefixes, users, request IDs, and error text are never metric labels.
