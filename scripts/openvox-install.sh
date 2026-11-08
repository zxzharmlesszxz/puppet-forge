#!/usr/bin/env bash
set -euo pipefail

: "${OPENVOX_FORGE_URL:?OPENVOX_FORGE_URL is required}"
: "${OPENVOX_READ_TOKEN:?OPENVOX_READ_TOKEN is required}"

export PATH="/opt/puppetlabs/bin:/opt/puppetlabs/puppet/bin:$PATH"
if ! command -v puppet >/dev/null 2>&1; then
  printf '%s\n' 'OpenVox puppet executable was not found in the installer image' >&2
  exit 1
fi

case "$(puppet --version)" in
  8.*) ;;
  *) printf '%s\n' 'OpenVox 8 is required' >&2; exit 1 ;;
esac

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
umask 077
export OPENVOX_CONFIG="$workdir/puppet.conf"
mkdir -p "$workdir/var" "$workdir/code" "$workdir/conf"
/opt/puppetlabs/puppet/bin/ruby -rnet/http -ruri -e '
  base = URI(ENV.fetch("OPENVOX_FORGE_URL"))
  abort "Forge URL must be an HTTP(S) origin" unless %w[http https].include?(base.scheme) && base.host && !base.userinfo && !base.query && !base.fragment && [nil, "", "/"].include?(base.path)
  response = Net::HTTP.get_response(URI.join(base.to_s + "/", "v3/releases?limit=1"))
  abort "Forge must reject anonymous installer requests with HTTP 401" unless response.code == "401"
  token = ENV.fetch("OPENVOX_READ_TOKEN")
  abort "Invalid multiline token" if token.match?(/[\r\n]/)
  File.write(ENV.fetch("OPENVOX_CONFIG"), "[main]\nforge_authorization = Bearer #{token}\n", perm: 0600)
'

for pass in cold warm; do
  target="$workdir/$pass"
  mkdir -p "$target"
  for spec in "puppetlabs-stdlib:10.1.0" "puppetlabs-apt:11.4.0" "${OPENVOX_COMMON_MODULE:-}:${OPENVOX_COMMON_VERSION:-1.0.0}"; do
    module="${spec%:*}"
    version="${spec##*:}"
    [[ -n "$module" ]] || continue
    puppet module install "$module" --version "$version" --target-dir "$target" \
      --config "$OPENVOX_CONFIG" --module_repository "$OPENVOX_FORGE_URL" \
      --vardir "$workdir/var" --codedir "$workdir/code" --confdir "$workdir/conf"
    /opt/puppetlabs/puppet/bin/ruby -rjson -e '
      module_name, version, target = ARGV
      metadata = JSON.parse(File.read(File.join(target, module_name.split("-", 2).last, "metadata.json")))
      abort "Installed module identity mismatch" unless metadata["name"] == module_name && metadata["version"] == version
    ' "$module" "$version" "$target"
  done
done

if [[ -z "${OPENVOX_COMMON_MODULE:-}" ]]; then
  printf '%s\n' 'Private Common acceptance skipped: OPENVOX_COMMON_MODULE was not supplied.'
fi
printf '%s\n' 'OpenVox 8 installer smoke passed; project-specific sshd checks are separate.'
