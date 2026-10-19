{{- define "acm-hub.namespace" -}}
{{- .Values.namespace -}}
{{- end -}}

{{- define "acm-hub.labels" -}}
app.kubernetes.io/name: acm-hub
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}
