#!/usr/bin/env sh

set -eu

HELM="${HELM:-helm}"
CHART_DIR="${CHART_DIR:-deploy/puppet-forge}"
PEPPER="helm-template-access-token-pepper-32-bytes"
SESSION_SECRET="helm-template-manage-session-secret-32-bytes"

render_manifest() {
  "$HELM" template puppet-forge "$CHART_DIR" \
    --set secret.create=true \
    --set-string secret.stringData.DATABASE_DSN=postgres://forge:forge@postgres:5432/forge \
    --set-string secret.stringData.ACCESS_TOKEN_PEPPER="$PEPPER" \
    --set-string secret.stringData.MANAGE_SESSION_SECRET="$SESSION_SECRET" \
    "$@"
}

render() {
  render_manifest "$@" >/dev/null
}

reject() {
  if render "$@" 2>/dev/null; then
    echo "expected Helm rendering to fail: $*" >&2
    exit 1
  fi
}

render
render --set autoscaling.enabled=true
render --set serviceMonitor.enabled=true --set prometheusRule.enabled=true --set networkPolicy.enabled=true
render --set secret.create=false --set secret.existingSecret=puppet-forge-runtime
render --set database.backend=sqlite --set replicaCount=1 --set-string secret.stringData.DATABASE_DSN=sqlite:///data/forge.db
render --set database.backend=postgres --set secret.create=false --set secret.existingSecret=puppet-forge-runtime
render --set database.backend=sqlite --set replicaCount=1 --set secret.create=false --set secret.existingSecret=puppet-forge-runtime
render --set-string config.TRUST_FORWARDED_HEADERS=true --set-string 'config.TRUSTED_PROXY_CIDRS=0.0.0.0/0\,::/0'
"$HELM" template puppet-forge "$CHART_DIR" -f "$CHART_DIR/values-dev.yaml" >/dev/null

manifest="$(render_manifest)"
printf '%s\n' "$manifest" | grep -F 'runAsNonRoot: true' >/dev/null
printf '%s\n' "$manifest" | grep -F 'runAsUser: 10001' >/dev/null
printf '%s\n' "$manifest" | grep -F 'runAsGroup: 10001' >/dev/null

reject --set autoscaling.enabled=true --set autoscaling.minReplicas=3 --set autoscaling.maxReplicas=2
reject --set autoscaling.enabled=true --set autoscaling.targetCPUUtilizationPercentage=null --set autoscaling.targetMemoryUtilizationPercentage=null
reject --set database.backend=sqlite --set autoscaling.enabled=true --set-string secret.stringData.DATABASE_DSN=sqlite:///data/forge.db
reject --set database.backend=sqlite --set replicaCount=2 --set-string secret.stringData.DATABASE_DSN=sqlite:///data/forge.db
reject --set database.backend=sqlite --set secret.create=false --set secret.existingSecret=puppet-forge-runtime
reject --set database.backend=postgres --set-string secret.stringData.DATABASE_DSN=sqlite:///data/forge.db
reject --set-string config.TRUST_FORWARDED_HEADERS=true --set-string config.TRUSTED_PROXY_CIDRS=
reject --set secret.create=false --set secret.existingSecret=
reject --set secret.create=true --set secret.existingSecret=puppet-forge-runtime
reject --set-string secret.stringData.ADMIN_TOKEN=too-short

echo "Helm template matrix passed"
