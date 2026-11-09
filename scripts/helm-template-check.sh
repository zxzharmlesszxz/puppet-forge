#!/usr/bin/env sh

set -eu

HELM="${HELM:-helm}"
CHART_DIR="${CHART_DIR:-deploy/puppet-forge}"
GCS_VALUES="$CHART_DIR/values-gcs.yaml"
S3_VALUES="$CHART_DIR/values-s3.yaml"
PEPPER="helm-template-access-token-pepper-32-bytes"
SESSION_SECRET="helm-template-manage-session-secret-32-bytes"

render_manifest() {
  "$HELM" template puppet-forge "$CHART_DIR" \
    -f "$GCS_VALUES" \
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

expect_count() {
  actual="$(printf '%s\n' "$1" | grep -c -- "$2" || true)"
  if [ "$actual" -ne "$3" ]; then
    echo "Helm assertion failed: '$2' matched $actual times, expected $3" >&2
    exit 1
  fi
}

render
render --set autoscaling.enabled=true
render --set serviceMonitor.enabled=true --set prometheusRule.enabled=true --set networkPolicy.enabled=true
custom_telemetry_manifest="$(render_manifest --set fullnameOverride=forge --set serviceMonitor.enabled=true --set prometheusRule.enabled=true)"
expect_count "$custom_telemetry_manifest" 'job=~"forge-metrics"' 5
expect_count "$custom_telemetry_manifest" 'job=~".*puppet-forge.*"' 0
external_telemetry_manifest="$(render_manifest --set prometheusRule.enabled=true --set-string prometheusRule.targetJobRegex=corporate-forge)"
expect_count "$external_telemetry_manifest" 'job=~"corporate-forge"' 5
telemetry_manifest="$(render_manifest --set prometheusRule.enabled=true \
  --set prometheusRule.thresholds.pendingApprovalMaxAgeSeconds=7200 \
  --set prometheusRule.thresholds.authorizationRejectionMinimumRate=3 \
  --set prometheusRule.thresholds.authorizationRejectionRatio=0.4)"
for alert in PuppetForgeApprovalQueueOld PuppetForgeControlOperationErrors PuppetForgeAuthorizationErrors PuppetForgeAuthorizationRejections PuppetForgeAuditWriteErrors PuppetForgeAccessConfigRefreshErrors PuppetForgeUnknownResourceState; do
  expect_count "$telemetry_manifest" "alert: $alert$" 1
done
expect_count "$telemetry_manifest" ' > 7200$' 1
expect_count "$telemetry_manifest" ' > 3$' 1
expect_count "$telemetry_manifest" ' > 0.4$' 1
expect_count "$telemetry_manifest" 'record: job_kind_source_state:puppet_forge_resources$' 1
render --set secret.create=false --set secret.existingSecret=puppet-forge-runtime
render --set database.backend=sqlite --set replicaCount=1 --set-string secret.stringData.DATABASE_DSN=sqlite:///data/forge.db
render --set database.backend=postgres --set secret.create=false --set secret.existingSecret=puppet-forge-runtime
render --set database.backend=sqlite --set replicaCount=1 --set secret.create=false --set secret.existingSecret=puppet-forge-runtime
render --set-string config.TRUST_FORWARDED_HEADERS=true --set-string 'config.TRUSTED_PROXY_CIDRS=0.0.0.0/0\,::/0'
"$HELM" template puppet-forge "$CHART_DIR" -f "$GCS_VALUES" -f "$CHART_DIR/values-dev.yaml" >/dev/null
"$HELM" template puppet-forge "$CHART_DIR" -f "$S3_VALUES" \
  --set secret.create=false \
  --set secret.existingSecret=puppet-forge-runtime >/dev/null

if "$HELM" template puppet-forge "$CHART_DIR" \
  --set secret.create=false \
  --set secret.existingSecret=puppet-forge-runtime >/dev/null 2>&1; then
  echo "expected Helm rendering without an artifact backend profile to fail" >&2
  exit 1
fi

manifest="$(render_manifest)"
printf '%s\n' "$manifest" | grep -F 'runAsNonRoot: true' >/dev/null
printf '%s\n' "$manifest" | grep -F 'runAsUser: 10001' >/dev/null
printf '%s\n' "$manifest" | grep -F 'runAsGroup: 10001' >/dev/null
if printf '%s\n' "$manifest" | grep -Eq '^kind: (Gateway|HTTPRoute|SecurityPolicy)$'; then
  echo "gateway resources must be disabled by default" >&2
  exit 1
fi

gateway_manifest="$(render_manifest -f "$CHART_DIR/values-gateway-api.yaml")"
expect_count "$gateway_manifest" '^kind: HTTPRoute$' 2
if printf '%s\n' "$gateway_manifest" | grep -Eq '^kind: (Gateway|Ingress|SecurityPolicy)$'; then
  echo "existing Gateway mode must not create Gateway, Ingress or Envoy policy" >&2
  exit 1
fi

envoy_manifest="$(render_manifest -f "$CHART_DIR/values-gateway-api.yaml" \
  -f "$CHART_DIR/values-envoy-gateway.yaml" --namespace gateway-test \
  --set fullnameOverride=custom-forge --set service.port=8181)"
expect_count "$envoy_manifest" '^kind: HTTPRoute$' 2
expect_count "$envoy_manifest" '^kind: SecurityPolicy$' 1
for expected in 'name: custom-forge-publish' 'name: custom-forge-other' \
  'namespace: gateway-test' 'name: custom-forge' 'port: 8181' \
  'defaultAction: Deny' '192.0.2.10/32' '2001:db8:1234::/48' \
  'value: "/api/v1/modules"' 'value: "/v3/releases"' 'value: "/manage/modules"'; do
  printf '%s\n' "$envoy_manifest" | grep -F "$expected" >/dev/null
done

owned_gateway_manifest="$(render_manifest -f "$CHART_DIR/values-gateway-api-create.yaml" \
  -f "$CHART_DIR/values-envoy-gateway.yaml" --namespace owned-gateway \
  --set fullnameOverride=custom-forge)"
expect_count "$owned_gateway_manifest" '^kind: Gateway$' 1
expect_count "$owned_gateway_manifest" '^kind: HTTPRoute$' 2
expect_count "$owned_gateway_manifest" '^kind: SecurityPolicy$' 1
expect_count "$owned_gateway_manifest" 'name: custom-forge-gateway$' 3
for expected in 'namespace: owned-gateway' 'gatewayClassName: "envoy"' \
  'protocol: "HTTPS"' 'port: 443' 'mode: Terminate' 'name: forge-tls' \
  'hostname: "forge.example.com"' 'from: Same' 'sectionName: "https"'; do
  printf '%s\n' "$owned_gateway_manifest" | grep -F "$expected" >/dev/null
done

dual_listener_manifest="$(render_manifest -f "$CHART_DIR/values-gateway-api-create.yaml" \
  --set gatewayAPI.gateway.name=custom-entry \
  --set gatewayAPI.gateway.annotations.example=value \
  --set-json 'gatewayAPI.gateway.listeners=[{"name":"https","protocol":"HTTPS","port":443,"tls":{"certificateRefs":[{"name":"forge-tls"}]}},{"name":"http","protocol":"HTTP","port":80}]')"
expect_count "$dual_listener_manifest" 'name: custom-entry$' 5
expect_count "$dual_listener_manifest" 'sectionName: "http"' 2
printf '%s\n' "$dual_listener_manifest" | grep -F 'example: value' >/dev/null

reject --set gatewayAPI.gateway.create=true
reject -f "$CHART_DIR/values-gateway-api.yaml" --set gatewayAPI.gateway.create=true
reject -f "$CHART_DIR/values-gateway-api-create.yaml" --set gatewayAPI.gateway.gatewayClassName=
reject -f "$CHART_DIR/values-gateway-api-create.yaml" --set gatewayAPI.gateway.listeners=null
for listener in \
  '{"protocol":"HTTP","port":80}' \
  '{"name":"http","protocol":"HTTP","port":0}' \
  '{"name":"http","protocol":"HTTP","port":65536}' \
  '{"name":"tls","protocol":"TLS","port":443}' \
  '{"name":"https","protocol":"HTTPS","port":443}' \
  '{"name":"https","protocol":"HTTPS","port":443,"tls":{"certificateRefs":[{"name":""}]}}' \
  '{"name":"https","protocol":"HTTPS","port":443,"tls":{"mode":"Passthrough","certificateRefs":[{"name":"forge-tls"}]}}' \
  '{"name":"http","protocol":"HTTP","port":80,"tls":{"certificateRefs":[{"name":"forge-tls"}]}}'; do
  reject -f "$CHART_DIR/values-gateway-api-create.yaml" \
    --set-json "gatewayAPI.gateway.listeners=[$listener]"
done
reject -f "$CHART_DIR/values-gateway-api-create.yaml" \
  --set-json 'gatewayAPI.gateway.listeners=[{"name":"http","protocol":"HTTP","port":80},{"name":"http","protocol":"HTTP","port":8080}]'
render -f "$CHART_DIR/values-gateway-api.yaml" -f "$CHART_DIR/values-envoy-gateway.yaml" \
  --set gatewayAPI.enabled=false --set envoyGateway.enabled=false
reject -f "$CHART_DIR/values-gateway-api.yaml" --set ingress.enabled=true
reject --set envoyGateway.enabled=true
reject --set ingress.enabled=false --set gatewayAPI.enabled=true
reject -f "$CHART_DIR/values-gateway-api.yaml" --set gatewayAPI.parentRefs=null
reject -f "$CHART_DIR/values-gateway-api.yaml" --set gatewayAPI.hostnames=null
reject -f "$CHART_DIR/values-gateway-api.yaml" --set-string 'gatewayAPI.parentRefs[0].name='
reject -f "$CHART_DIR/values-gateway-api.yaml" --set-string 'gatewayAPI.hostnames[0]='
reject -f "$CHART_DIR/values-gateway-api.yaml" --set envoyGateway.enabled=true
reject -f "$CHART_DIR/values-gateway-api.yaml" -f "$CHART_DIR/values-envoy-gateway.yaml" \
  --set envoyGateway.publishAllowedCIDRs=null
reject -f "$CHART_DIR/values-gateway-api.yaml" -f "$CHART_DIR/values-envoy-gateway.yaml" \
  --set-string 'envoyGateway.publishAllowedCIDRs[0]='

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
