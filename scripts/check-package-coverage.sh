#!/usr/bin/env bash

set -euo pipefail

profile="${1:?coverage profile is required}"
shift

for specification in "$@"; do
  package="${specification%%=*}"
  threshold="${specification#*=}"
  [[ "$package" != "$threshold" ]] || { printf 'invalid package threshold: %s\n' "$specification" >&2; exit 2; }
  coverage="$(awk -v package="$package" '
    NR == 1 { next }
    {
      file = $1
      sub(/:.*/, "", file)
      if (index(file, "/" package "/") > 0 || index(file, package "/") == 1) {
        total += $2
        if ($3 > 0) covered += $2
      }
    }
    END {
      if (total == 0) exit 2
      printf "%.1f", covered * 100 / total
    }
  ' "$profile")" || { printf 'package is missing from coverage profile: %s\n' "$package" >&2; exit 1; }
  awk -v package="$package" -v coverage="$coverage" -v threshold="$threshold" 'BEGIN {
    if (coverage + 0 < threshold + 0) {
      printf "%s coverage %.1f%% is below %.1f%%\n", package, coverage, threshold
      exit 1
    }
    printf "%s coverage %.1f%% meets %.1f%%\n", package, coverage, threshold
  }'
done
