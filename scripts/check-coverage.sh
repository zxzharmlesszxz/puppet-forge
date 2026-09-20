#!/usr/bin/env bash

set -euo pipefail

report="${1:?coverage report is required}"
threshold="${2:?coverage threshold is required}"
coverage="$(awk '/^total:/ {gsub(/%/, "", $3); print $3}' "$report")"

[[ -n "$coverage" ]] || { printf 'total coverage is missing from %s\n' "$report" >&2; exit 1; }

awk -v coverage="$coverage" -v threshold="$threshold" 'BEGIN {
  if (coverage + 0 < threshold + 0) {
    printf "coverage %.1f%% is below %.1f%%\n", coverage, threshold
    exit 1
  }
  printf "coverage %.1f%% meets threshold %.1f%%\n", coverage, threshold
}'
