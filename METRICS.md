# Metrics

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
- Value: current number of in-flight HTTP requests
- Labels: none

Normalized `route` values currently include:

- `/`
- `/healthz`
- `/readyz`
- `/metrics`
- `/manage`
- `/manage/*`
- `/api/v1/modules`
- `/api/v1/modules/`
- `/api/v1/modules/*`
- `/modules/*`
- `/v3/files/*`
- `/v3/*`
- `other`

## Inventory Metrics

### `puppet_forge_module_info`

- Type: gauge
- Value: always `1`
- Labels:
  - `module`
  - `owner`
  - `name`
  - `latest_version`
- Notes:
  - exported once per locally indexed module
  - `module` currently has format `<owner>-<name>`
  - `latest_version` may be empty for a module shell without indexed releases

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

## Operation Metrics

### `puppet_forge_publish_total`

- Type: counter
- Value: total number of module publish attempts handled by the service layer
- Labels:
  - `result`: `success` or `error`
  - `owner`

### `puppet_forge_delete_total`

- Type: counter
- Value: total number of delete attempts
- Labels:
  - `result`: `success` or `error`
  - `kind`: `module` or `release`
  - `owner`

### `puppet_forge_release_usage_mark_total`

- Type: counter
- Value: total number of release usage mark attempts
- Labels:
  - `result`: `success` or `error`
  - `owner`

Release usage marks are written when clients download module releases through the API or request local `/v3/*` module/release/file metadata. The Manage UI uses the same release usage data to hide delete actions for in-use releases.

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

## Compatibility Notes

`puppet_forge_module_info` is intentionally shaped close to `puppetfile_module_info` from `prometheus-puppetfile-exporter`, so dashboards and PromQL can compare:

- `current_version` from `Puppetfile`
- `latest_version` from this Forge service
- shared labels such as `owner` and `name`

The `module` label is not identical across the two services, so joins should prefer `owner` and `name`.
