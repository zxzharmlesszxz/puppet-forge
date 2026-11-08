#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${OPENVOX_TEST_IMAGE:?Set an official OpenVox 8 agent image, preferably pinned by digest}"
: "${OPENVOX_READ_TOKEN:?OPENVOX_READ_TOKEN is required}"
export OPENVOX_FORGE_URL="${OPENVOX_FORGE_URL:-http://host.docker.internal:8080}"

"${DOCKER:-docker}" run --rm --init \
  --add-host=host.docker.internal:host-gateway \
  --mount "type=bind,src=$repo_root/scripts/openvox-install.sh,dst=/openvox-install.sh,readonly" \
  --env OPENVOX_FORGE_URL --env OPENVOX_READ_TOKEN \
  --env OPENVOX_COMMON_MODULE --env OPENVOX_COMMON_VERSION \
  --entrypoint /bin/bash "$OPENVOX_TEST_IMAGE" /openvox-install.sh
