{{/* Common labels (part-of comes with the selector). Names stay plain: one release per namespace. */}}
{{- define "keeper.labels" -}}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{- define "keeper.selector" -}}
app.kubernetes.io/name: {{ . }}
app.kubernetes.io/part-of: keeper
{{- end }}

{{/* Image of a Keeper app: <repository>/<name>:<tag> */}}
{{- define "keeper.image" -}}
{{ .root.Values.image.repository }}/{{ .name }}:{{ .root.Values.image.tag }}
{{- end }}

{{/* OpenTelemetry env every Keeper service gets. */}}
{{- define "keeper.otelEnv" -}}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: http://otel-collector:4318
- name: OTEL_EXPORTER_OTLP_PROTOCOL
  value: http/protobuf
- name: OTEL_RESOURCE_ATTRIBUTES
  value: deployment.environment.name={{ .Release.Namespace }}
{{- end }}

{{/* Pod annotation that rolls a pod when the secrets it reads change. */}}
{{- define "keeper.secretsChecksum" -}}
{{- with .Values.secrets.checksum }}
checksum/secrets: {{ . | quote }}
{{- end }}
{{- end }}

{{/* Optional key from keeper-secrets. */}}
{{- define "keeper.secretEnv" -}}
- name: {{ .key }}
  valueFrom: { secretKeyRef: { name: {{ .root.Values.secrets.name }}, key: {{ .key }}, optional: true } }
{{- end }}

{{/* Postgres connection: password from the secret, read by pgx as PGPASSWORD. */}}
{{- define "keeper.dbEnv" -}}
- name: DATABASE_URL
  value: postgres://keeper@postgres:5432/keeper
- name: PGPASSWORD
  valueFrom: { secretKeyRef: { name: {{ .Values.secrets.name }}, key: POSTGRES_PASSWORD } }
{{- end }}
