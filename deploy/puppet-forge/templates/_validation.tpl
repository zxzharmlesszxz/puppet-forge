{{- define "chart.validate" -}}
{{- $dsn := default "" .Values.secret.stringData.DATABASE_DSN -}}
{{- if and (hasPrefix "sqlite://" $dsn) .Values.autoscaling.enabled -}}
{{- fail "sqlite DATABASE_DSN cannot be used with autoscaling.enabled=true because SQLite is a single-writer local database" -}}
{{- end -}}
{{- if and (hasPrefix "sqlite://" $dsn) (ne (int .Values.replicaCount) 1) -}}
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
