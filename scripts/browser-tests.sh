#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture_url="${BROWSER_FIXTURE_URL:-http://127.0.0.1:18085}"
container_fixture_url="${BROWSER_CONTAINER_FIXTURE_URL:-http://host.docker.internal:18085}"
fixture_log="$(mktemp)"
fixture_pid=""

cleanup() {
  if [[ -n "$fixture_pid" ]]; then
    kill "$fixture_pid" 2>/dev/null || true
    wait "$fixture_pid" 2>/dev/null || true
  fi
  rm -f "$fixture_log"
}

trap cleanup EXIT INT TERM

cd "$repo_root"
if ! "${CURL:-curl}" --fail --silent --show-error "$fixture_url/healthz" >/dev/null 2>&1; then
  BROWSER_FIXTURE_ADDR="${BROWSER_FIXTURE_ADDR:-0.0.0.0:18085}" "${GO:-go}" run ./testdata/browser/fixture >"$fixture_log" 2>&1 &
  fixture_pid="$!"
fi

ready=false
for _ in {1..120}; do
  if "${CURL:-curl}" --fail --silent --show-error "$fixture_url/healthz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  if [[ -n "$fixture_pid" ]] && ! kill -0 "$fixture_pid" 2>/dev/null; then
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
  sh -c 'mkdir -p testdata internal/httpapi && cp /source/package.json /source/package-lock.json /source/playwright.config.js . && cp -R /source/testdata/browser testdata/browser && cp -R /source/internal/httpapi/templates internal/httpapi/templates && npm ci && npm run test:browser'; then
  cat "$fixture_log" >&2
  exit 1
fi
