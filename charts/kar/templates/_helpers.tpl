{{- define "kar.name" -}}
kar
{{- end -}}

{{- define "kar.fullname" -}}
{{- if contains "kar" .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-kar" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kar.labels" -}}
app.kubernetes.io/name: {{ include "kar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "kar.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kar.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{- define "kar.clusterRoleName" -}}
{{- if .Values.rbac.create -}}
{{- .Values.rbac.clusterRoleName | default (include "kar.fullname" .) -}}
{{- else -}}
{{- required "rbac.clusterRoleName is required when rbac.create is false" .Values.rbac.clusterRoleName -}}
{{- end -}}
{{- end -}}
