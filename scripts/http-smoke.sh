#!/usr/bin/env sh
set -eu

base_url="${1:-${SMOKE_BASE_URL:-http://forge.127.0.0.1.nip.io:8080}}"
public_module_access="${SMOKE_PUBLIC_MODULE_ACCESS:-${PUBLIC_MODULE_ACCESS:-false}}"
admin_token="${SMOKE_ADMIN_TOKEN:-${ADMIN_TOKEN:-forge-admin-token-local}}"
retries="${SMOKE_RETRIES:-30}"
retry_delay="${SMOKE_RETRY_DELAY:-2}"
curl_bin="${CURL:-curl}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

body="$tmpdir/body"
headers="$tmpdir/headers"
cookies="$tmpdir/cookies"

status_code() {
	method="$1"
	url="$2"
	token="${3:-}"

	if [ -n "$token" ]; then
		"$curl_bin" -sS -o "$body" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $token" "$url"
	else
		"$curl_bin" -sS -o "$body" -w '%{http_code}' -X "$method" "$url"
	fi
}

expect_status() {
	name="$1"
	got="$2"
	want="$3"

	if [ "$got" != "$want" ]; then
		echo "${name}: expected HTTP ${want}, got ${got}"
		cat "$body"
		exit 1
	fi
}

wait_for_json_status() {
	path="$1"
	field="$2"
	value="$3"
	name="$4"

	attempt=1
	while [ "$attempt" -le "$retries" ]; do
		if "$curl_bin" -fsS "${base_url}${path}" -o "$body" 2>/dev/null && grep -q "\"${field}\":\"${value}\"" "$body"; then
			return 0
		fi
		if [ "$attempt" -lt "$retries" ]; then
			sleep "$retry_delay"
		fi
		attempt=$((attempt + 1))
	done

	echo "${name} did not return ${value} after ${retries} attempts"
	cat "$body" 2>/dev/null || true
	exit 1
}

wait_for_json_status "/healthz" "status" "ok" "healthz"
wait_for_json_status "/readyz" "status" "ready" "readyz"

"$curl_bin" -fsS "${base_url}/metrics" -o "$body"
grep -q '^puppet_forge_build_info' "$body" || {
	echo "metrics missing puppet_forge_build_info"
	exit 1
}

"$curl_bin" -fsS "${base_url}/" -o "$body"
grep -q 'href="/manage"' "$body" || {
	echo "catalog page missing Manage link"
	exit 1
}

"$curl_bin" -fsS "${base_url}/manage/login" -o "$body"
grep -q 'name="token"' "$body" || {
	echo "manage login page missing token input"
	exit 1
}

api_status="$(status_code GET "${base_url}/api/v1/modules?limit=1")"
case "$public_module_access" in
	true)
		expect_status "public API read" "$api_status" "200"
		;;
	false)
		expect_status "private API read without token" "$api_status" "401"
		;;
	*)
		echo "SMOKE_PUBLIC_MODULE_ACCESS must be true or false, got ${public_module_access}"
		exit 1
		;;
esac

if [ -n "$admin_token" ]; then
	api_status="$(status_code GET "${base_url}/api/v1/modules?limit=1" "$admin_token")"
	expect_status "admin token API read" "$api_status" "200"

	login_status="$(
		"$curl_bin" -sS -o "$body" -D "$headers" -c "$cookies" -w '%{http_code}' \
			-X POST \
			--data-urlencode "token=${admin_token}" \
			"${base_url}/manage/login"
	)"
	expect_status "admin token manage login" "$login_status" "302"
	grep -qi '^location: /manage' "$headers" || {
		echo "manage login did not redirect to /manage"
		cat "$headers"
		exit 1
	}
	grep -q 'puppet_forge_manage_token' "$cookies" || {
		echo "manage login did not set manage token cookie"
		exit 1
	}
fi

echo "HTTP smoke passed for ${base_url}"
