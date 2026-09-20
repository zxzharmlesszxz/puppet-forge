#!/usr/bin/env bash

set -euo pipefail

if (( $# < 2 )); then
  printf 'usage: %s OUTPUT PROFILE...\n' "$0" >&2
  exit 2
fi

output="$1"
shift
mode=""

for profile in "$@"; do
  [[ -s "$profile" ]] || { printf 'coverage profile is missing or empty: %s\n' "$profile" >&2; exit 1; }
  profile_mode="$(sed -n '1s/^mode: //p' "$profile")"
  [[ -n "$profile_mode" ]] || { printf 'coverage profile has no mode: %s\n' "$profile" >&2; exit 1; }
  if [[ -z "$mode" ]]; then
    mode="$profile_mode"
  elif [[ "$profile_mode" != "$mode" ]]; then
    printf 'coverage mode mismatch: %s uses %s, expected %s\n' "$profile" "$profile_mode" "$mode" >&2
    exit 1
  fi
done

mkdir -p "$(dirname "$output")"
temporary="$(mktemp "${output}.XXXXXX")"
trap 'rm -f "$temporary"' EXIT

{
  printf 'mode: %s\n' "$mode"
  awk 'FNR == 1 { next } NF != 3 { exit 1 } { key = $1 " " $2; count[key] += $3 } END { for (key in count) print key, count[key] }' "$@" | LC_ALL=C sort
} >"$temporary"

mv "$temporary" "$output"
trap - EXIT
