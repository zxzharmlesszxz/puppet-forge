{{- define "chart.validate" -}}
{{- if and .Values.gatewayAPI.enabled .Values.ingress.enabled -}}
{{- fail "gatewayAPI.enabled and ingress.enabled are mutually exclusive" -}}
{{- end -}}
{{- if and .Values.gatewayAPI.gateway.create (not .Values.gatewayAPI.enabled) -}}
{{- fail "gatewayAPI.gateway.create requires gatewayAPI.enabled=true" -}}
{{- end -}}
{{- if .Values.gatewayAPI.enabled -}}
{{- if .Values.gatewayAPI.gateway.create -}}
{{- if not (empty .Values.gatewayAPI.parentRefs) -}}
{{- fail "gatewayAPI.parentRefs must be empty when gatewayAPI.gateway.create=true" -}}
{{- end -}}
{{- if empty (trim .Values.gatewayAPI.gateway.gatewayClassName) -}}
{{- fail "gatewayAPI.gateway.gatewayClassName is required when creating a Gateway" -}}
{{- end -}}
{{- if empty .Values.gatewayAPI.gateway.listeners -}}
{{- fail "gatewayAPI.gateway.listeners is required when creating a Gateway" -}}
{{- end -}}
{{- $listenerNames := dict -}}
{{- range .Values.gatewayAPI.gateway.listeners -}}
{{- if empty .name -}}
{{- fail "each Gateway listener requires a name" -}}
{{- end -}}
{{- if hasKey $listenerNames .name -}}
{{- fail "Gateway listener names must be unique" -}}
{{- end -}}
{{- $_ := set $listenerNames .name true -}}
{{- if not (has .protocol (list "HTTP" "HTTPS")) -}}
{{- fail "Gateway listeners must use HTTP or HTTPS for HTTPRoute" -}}
{{- end -}}
{{- if or (lt (int .port) 1) (gt (int .port) 65535) -}}
{{- fail "Gateway listener port must be between 1 and 65535" -}}
{{- end -}}
{{- if eq .protocol "HTTPS" -}}
{{- $tls := default dict .tls -}}
{{- if ne (default "Terminate" $tls.mode) "Terminate" -}}
{{- fail "HTTPS Gateway listeners require TLS Terminate mode" -}}
{{- end -}}
{{- if empty $tls.certificateRefs -}}
{{- fail "HTTPS Gateway listeners require tls.certificateRefs" -}}
{{- end -}}
{{- range $tls.certificateRefs -}}
{{- if empty .name -}}
{{- fail "each Gateway TLS certificate reference requires a name" -}}
{{- end -}}
{{- end -}}
{{- else if not (empty .tls) -}}
{{- fail "HTTP Gateway listeners must not configure TLS" -}}
{{- end -}}
{{- end -}}
{{- else -}}
{{- if empty .Values.gatewayAPI.parentRefs -}}
{{- fail "gatewayAPI.parentRefs is required when gatewayAPI.enabled=true" -}}
{{- end -}}
{{- range .Values.gatewayAPI.parentRefs -}}
{{- if empty .name -}}
{{- fail "each gatewayAPI.parentRefs entry requires a name" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if empty .Values.gatewayAPI.hostnames -}}
{{- fail "gatewayAPI.hostnames is required when gatewayAPI.enabled=true" -}}
{{- end -}}
{{- range .Values.gatewayAPI.hostnames -}}
{{- if empty (trim .) -}}
{{- fail "gatewayAPI.hostnames entries must not be empty" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if .Values.envoyGateway.enabled -}}
{{- if not .Values.gatewayAPI.enabled -}}
{{- fail "envoyGateway.enabled requires gatewayAPI.enabled=true" -}}
{{- end -}}
{{- if empty .Values.envoyGateway.publishAllowedCIDRs -}}
{{- fail "envoyGateway.publishAllowedCIDRs is required when envoyGateway.enabled=true" -}}
{{- end -}}
{{- range .Values.envoyGateway.publishAllowedCIDRs -}}
{{- if empty (trim .) -}}
{{- fail "envoyGateway.publishAllowedCIDRs entries must not be empty" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $dsn := default "" .Values.secret.stringData.DATABASE_DSN -}}
{{- $backend := default "" .Values.database.backend -}}
{{- $artifactBackend := default "" (index .Values.config "ARTIFACT_BACKEND") -}}
{{- if not (has $backend (list "postgres" "sqlite")) -}}
{{- fail "database.backend must be postgres or sqlite" -}}
{{- end -}}
{{- if not (has $artifactBackend (list "gcs" "s3")) -}}
{{- fail "config.ARTIFACT_BACKEND must be gcs or s3; apply values-gcs.yaml or values-s3.yaml" -}}
{{- end -}}
{{- if empty (index .Values.config "ARTIFACT_BUCKET") -}}
{{- fail "config.ARTIFACT_BUCKET is required" -}}
{{- end -}}
{{- if and (eq $artifactBackend "s3") (empty (index .Values.config "ARTIFACT_ENDPOINT")) -}}
{{- fail "config.ARTIFACT_ENDPOINT is required for the s3 backend" -}}
{{- end -}}
{{- if and .Values.secret.create (not (empty .Values.secret.existingSecret)) -}}
{{- fail "secret.create and secret.existingSecret are mutually exclusive" -}}
{{- end -}}
{{- if and (not .Values.secret.create) (empty .Values.secret.existingSecret) -}}
{{- fail "set secret.create=true or provide secret.existingSecret" -}}
{{- end -}}
{{- if and .Values.secret.create (lt (len (default "" .Values.secret.stringData.ACCESS_TOKEN_PEPPER)) 32) -}}
{{- fail "secret.stringData.ACCESS_TOKEN_PEPPER must contain at least 32 bytes when secret.create=true" -}}
{{- end -}}
{{- if and .Values.secret.create (lt (len (default "" .Values.secret.stringData.MANAGE_SESSION_SECRET)) 32) -}}
{{- fail "secret.stringData.MANAGE_SESSION_SECRET must contain at least 32 bytes when secret.create=true" -}}
{{- end -}}
{{- if and .Values.secret.create (not (empty .Values.secret.stringData.ADMIN_TOKEN)) (lt (len .Values.secret.stringData.ADMIN_TOKEN) 32) -}}
{{- fail "secret.stringData.ADMIN_TOKEN must contain at least 32 bytes when set and secret.create=true" -}}
{{- end -}}
{{- if and .Values.secret.create (eq (default "" .Values.secret.stringData.MANAGE_SESSION_SECRET) (default "" .Values.secret.stringData.ACCESS_TOKEN_PEPPER)) -}}
{{- fail "secret.stringData.MANAGE_SESSION_SECRET must differ from secret.stringData.ACCESS_TOKEN_PEPPER" -}}
{{- end -}}
{{- if and (eq (default "false" .Values.config.TRUST_FORWARDED_HEADERS) "true") (empty .Values.config.TRUSTED_PROXY_CIDRS) -}}
{{- fail "config.TRUSTED_PROXY_CIDRS is required when config.TRUST_FORWARDED_HEADERS=true" -}}
{{- end -}}
{{- if and .Values.secret.create (eq $backend "postgres") (not (or (hasPrefix "postgres://" $dsn) (hasPrefix "postgresql://" $dsn))) -}}
{{- fail "secret.stringData.DATABASE_DSN must use postgres:// or postgresql:// when database.backend=postgres" -}}
{{- end -}}
{{- if and .Values.secret.create (eq $backend "sqlite") (not (hasPrefix "sqlite://" $dsn)) -}}
{{- fail "secret.stringData.DATABASE_DSN must use sqlite:// when database.backend=sqlite" -}}
{{- end -}}
{{- if and (eq $backend "sqlite") .Values.autoscaling.enabled -}}
{{- fail "sqlite DATABASE_DSN cannot be used with autoscaling.enabled=true because SQLite is a single-writer local database" -}}
{{- end -}}
{{- if and (eq $backend "sqlite") (ne (int .Values.replicaCount) 1) -}}
{{- fail "sqlite DATABASE_DSN requires replicaCount=1 because SQLite is a single-writer local database" -}}
{{- end -}}
{{- if .Values.autoscaling.enabled -}}
{{- if gt (int .Values.autoscaling.minReplicas) (int .Values.autoscaling.maxReplicas) -}}
{{- fail "autoscaling.minReplicas must be less than or equal to autoscaling.maxReplicas" -}}
{{- end -}}
{{- if and (empty .Values.autoscaling.targetCPUUtilizationPercentage) (empty .Values.autoscaling.targetMemoryUtilizationPercentage) -}}
{{- fail "autoscaling requires targetCPUUtilizationPercentage, targetMemoryUtilizationPercentage, or both" -}}
{{- end -}}
{{- end -}}
{{- end -}}
