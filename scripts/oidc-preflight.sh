#!/usr/bin/env sh
set -eu

base_url="${1:-http://forge.127.0.0.1.nip.io:8080}"
expected_redirect="${2:-${base_url}/auth/callback}"
curl_bin="${CURL:-curl}"
python_bin="${PYTHON:-python3}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

headers="$tmpdir/headers"
cookies="$tmpdir/cookies"
login="$tmpdir/login.html"

"$curl_bin" -sS -o /dev/null -D "$headers" -c "$cookies" "${base_url}/auth/login?next=%2Fmanage"

"$python_bin" - "$headers" "$expected_redirect" <<'PY'
import sys
import urllib.parse

headers_path, expected_redirect = sys.argv[1:3]
status = ""
location = ""

with open(headers_path, encoding="utf-8", errors="replace") as fh:
    for line in fh:
        line = line.rstrip("\r\n")
        if line.startswith("HTTP/"):
            parts = line.split()
            status = parts[1] if len(parts) > 1 else ""
        if line.lower().startswith("location:"):
            location = line.split(":", 1)[1].strip()

if status != "302":
    raise SystemExit(f"expected /auth/login to return 302, got {status}")
if not location:
    raise SystemExit("missing OIDC redirect Location header")

parsed = urllib.parse.urlparse(location)
query = urllib.parse.parse_qs(parsed.query)
redirect_uri = query.get("redirect_uri", [""])[0]

if redirect_uri != expected_redirect:
    raise SystemExit(f"unexpected redirect_uri: {redirect_uri!r}, want {expected_redirect!r}")
if not query.get("client_id", [""])[0]:
    raise SystemExit("missing OIDC client_id")
if not query.get("state", [""])[0]:
    raise SystemExit("missing OIDC state")

print(f"OIDC authorize endpoint: {parsed.scheme}://{parsed.netloc}{parsed.path}")
print(f"OIDC redirect_uri: {redirect_uri}")
PY

grep -q 'puppet_forge_state' "$cookies" || {
    echo "missing puppet_forge_state cookie"
    exit 1
}

"$curl_bin" -fsS "${base_url}/manage/login" -o "$login"
grep -q 'href="/auth/login?next=%2Fmanage"' "$login" || {
    echo "missing OIDC login link"
    exit 1
}
grep -q 'name="token"' "$login" || {
    echo "missing token fallback input"
    exit 1
}

echo "OIDC preflight passed for ${base_url}"
