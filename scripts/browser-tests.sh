#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture_url="${BROWSER_FIXTURE_URL:-}"
container_fixture_url="${BROWSER_CONTAINER_FIXTURE_URL:-}"
fixture_dir="$(mktemp -d)"
fixture_log="$fixture_dir/fixture.log"
fixture_pid=""

cleanup() {
  if [[ -n "$fixture_pid" ]]; then
    kill "$fixture_pid" 2>/dev/null || true
    wait "$fixture_pid" 2>/dev/null || true
  fi
  rm -rf "$fixture_dir"
}

trap cleanup EXIT INT TERM

cd "$repo_root"
if [[ -z "$fixture_url" ]]; then
  "${GO:-go}" build -o "$fixture_dir/fixture" ./testdata/browser/fixture
  BROWSER_FIXTURE_ADDR="${BROWSER_FIXTURE_ADDR:-0.0.0.0:0}" \
    "$fixture_dir/fixture" >"$fixture_dir/port" 2>"$fixture_log" &
  fixture_pid="$!"
else
  : >"$fixture_log"
  if [[ -z "$container_fixture_url" ]]; then
    printf '%s\n' 'BROWSER_CONTAINER_FIXTURE_URL is required with BROWSER_FIXTURE_URL' >&2
    exit 1
  fi
fi

ready=false
for _ in {1..120}; do
  if [[ -z "$fixture_url" && -s "$fixture_dir/port" ]]; then
    port="$(<"$fixture_dir/port")"
    fixture_url="http://127.0.0.1:$port"
    container_fixture_url="${BROWSER_CONTAINER_FIXTURE_URL:-http://host.docker.internal:$port}"
  fi
  if [[ -n "$fixture_pid" ]] && ! kill -0 "$fixture_pid" 2>/dev/null; then
    break
  fi
  if [[ -n "$fixture_url" ]] && "${CURL:-curl}" --fail --silent --show-error "$fixture_url/healthz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done

if [[ "$ready" != true ]]; then
  cat "$fixture_log" >&2
  exit 1
fi

if ! "${DOCKER:-docker}" run --rm --init --ipc=host \
  --add-host=host.docker.internal:host-gateway \
  --env BROWSER_EXTERNAL_SERVER=1 \
  --env "BROWSER_BASE_URL=$container_fixture_url" \
  --env PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 \
  --mount "type=bind,src=$repo_root,dst=/source,readonly" \
  --tmpfs /work:rw,exec,nosuid,size=768m \
  --workdir /work \
  "${PLAYWRIGHT_IMAGE:?PLAYWRIGHT_IMAGE is required}" \
  sh -c 'mkdir -p testdata internal/httpapi && cp /source/package.json /source/package-lock.json /source/playwright.config.js . && cp -R /source/testdata/browser testdata/browser && cp -R /source/internal/httpapi/assets internal/httpapi/assets && npm ci && npm run test:browser'; then
  cat "$fixture_log" >&2
  exit 1
fi
