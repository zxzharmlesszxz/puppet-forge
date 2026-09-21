#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -eq 0 ]]; then
  echo "usage: $0 command [args...]" >&2
  exit 2
fi

docker_bin="${DOCKER:-docker}"
suffix="$$-${RANDOM}"
network="puppet-forge-integration-${suffix}"
postgres_container="puppet-forge-integration-postgres-${suffix}"
minio_container="puppet-forge-integration-minio-${suffix}"
gcs_container="puppet-forge-integration-gcs-${suffix}"

cleanup() {
  "$docker_bin" rm --force --volumes "$gcs_container" "$minio_container" "$postgres_container" >/dev/null 2>&1 || true
  "$docker_bin" network rm "$network" >/dev/null 2>&1 || true
}

container_logs() {
  "$docker_bin" logs "$1" >&2 || true
}

wait_healthy() {
  local container="$1"
  local status
  for _ in {1..60}; do
    status="$("$docker_bin" inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || true)"
    if [[ "$status" == "healthy" ]]; then
      return 0
    fi
    if [[ "$status" == "exited" || "$status" == "dead" ]]; then
      container_logs "$container"
      return 1
    fi
    sleep 1
  done
  container_logs "$container"
  return 1
}

wait_http() {
  local container="$1"
  local host="$2"
  local port="$3"
  local path="$4"
  "$docker_bin" run --rm \
    --network "$network" \
    --entrypoint bash \
    "${INTEGRATION_RUNNER_IMAGE:?INTEGRATION_RUNNER_IMAGE is required}" \
    -ceu "
      for _ in {1..60}; do
        if exec 3<>\"/dev/tcp/\${1}/\${2}\"; then
          printf \"GET %s HTTP/1.0\\r\\nHost: %s\\r\\n\\r\\n\" \"\$3\" \"\$1\" >&3
          IFS= read -r status <&3 || true
          exec 3<&- 3>&-
          if [[ \"\$status\" == *\" 200 \"* ]]; then
            exit 0
          fi
        fi
        sleep 1
      done
      exit 1
    " _ "$host" "$port" "$path" || {
      container_logs "$container"
      return 1
    }
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"$docker_bin" network create "$network" >/dev/null

"$docker_bin" run --detach --name "$postgres_container" \
  --network "$network" \
  --network-alias postgres \
  --env POSTGRES_DB=forge \
  --env POSTGRES_USER=forge \
  --env POSTGRES_PASSWORD=forge \
  --health-cmd 'pg_isready -U forge -d forge' \
  --health-interval 1s \
  --health-timeout 5s \
  --health-retries 60 \
  "${POSTGRES_TEST_IMAGE:?POSTGRES_TEST_IMAGE is required}" >/dev/null

"$docker_bin" run --detach --name "$minio_container" \
  --network "$network" \
  --network-alias minio \
  --env MINIO_ROOT_USER=minioadmin \
  --env MINIO_ROOT_PASSWORD=minioadmin \
  --health-cmd 'curl -f http://localhost:9000/minio/health/live || exit 1' \
  --health-interval 1s \
  --health-timeout 5s \
  --health-retries 60 \
  "${MINIO_TEST_IMAGE:?MINIO_TEST_IMAGE is required}" server /data >/dev/null

"$docker_bin" run --detach --name "$gcs_container" \
  --network "$network" \
  --network-alias gcs \
  "${GCS_TEST_IMAGE:?GCS_TEST_IMAGE is required}" \
  -scheme http -backend memory -public-host gcs:4443 >/dev/null

wait_healthy "$postgres_container"
wait_healthy "$minio_container"
wait_http "$gcs_container" gcs 4443 /storage/v1/b

"$docker_bin" run --rm \
  --network "$network" \
  --user "$(id -u):$(id -g)" \
  --volume "$PWD:/work" \
  --workdir /work \
  --env HOME=/tmp \
  --env GOCACHE=/tmp/go-build \
  --env GOMODCACHE=/tmp/go-mod \
  --env PUPPET_FORGE_TEST_POSTGRES_DSN=postgres://forge:forge@postgres:5432/forge?sslmode=disable \
  --env PUPPET_FORGE_TEST_S3_ENDPOINT=http://minio:9000 \
  --env PUPPET_FORGE_TEST_S3_ACCESS_KEY_ID=minioadmin \
  --env PUPPET_FORGE_TEST_S3_SECRET_ACCESS_KEY=minioadmin \
  --env PUPPET_FORGE_TEST_GCS_ENDPOINT=http://gcs:4443 \
  "${INTEGRATION_RUNNER_IMAGE:?INTEGRATION_RUNNER_IMAGE is required}" \
  "$@"
