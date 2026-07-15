{{/* Common helpers */}}

{{- define "llm-gateway.name" -}}
llm-gateway
{{- end -}}

{{- define "llm-gateway.fullname" -}}
{{ .Release.Name }}-{{ include "llm-gateway.name" . }}
{{- end -}}

{{- define "llm-gateway.labels" -}}
app.kubernetes.io/name: {{ include "llm-gateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "llm-gateway.gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llm-gateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: gateway
{{- end -}}

{{- define "llm-gateway.secretName" -}}
{{- default (printf "%s-secrets" (include "llm-gateway.fullname" .)) .Values.secrets.existingSecret -}}
{{- end -}}

{{- define "llm-gateway.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
