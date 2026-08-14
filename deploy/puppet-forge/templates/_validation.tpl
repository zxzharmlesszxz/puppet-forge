{{- define "chart.validate" -}}
{{- $dsn := default "" .Values.secret.stringData.DATABASE_DSN -}}
{{- $backend := default "" .Values.database.backend -}}
{{- if not (has $backend (list "postgres" "sqlite")) -}}
{{- fail "database.backend must be postgres or sqlite" -}}
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
