{{- define "chart.validate" -}}
{{- $dsn := default "" .Values.secret.stringData.DATABASE_DSN -}}
{{- if and (hasPrefix "sqlite://" $dsn) (ne (int .Values.replicaCount) 1) -}}
{{- fail "sqlite DATABASE_DSN requires replicaCount=1 because SQLite is a single-writer local database" -}}
{{- end -}}
{{- end -}}
