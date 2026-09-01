{{/*
Resource name: the release name (or fullnameOverride), truncated to the label limit.
*/}}
{{- define "netbox-zone-labeler.fullname" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels. Deliberately name-only: a Deployment selector is immutable,
and this is the selector the chart has shipped since 0.1.0.
*/}}
{{- define "netbox-zone-labeler.selectorLabels" -}}
app.kubernetes.io/name: netbox-zone-labeler
{{- end }}

{{/*
Common labels.
*/}}
{{- define "netbox-zone-labeler.labels" -}}
{{ include "netbox-zone-labeler.selectorLabels" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{/*
Container security context shared by both containers. 65532 is
nonroot:nonroot of distroless, which both images run as.
*/}}
{{- define "netbox-zone-labeler.containerSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
readOnlyRootFilesystem: true
allowPrivilegeEscalation: false
seccompProfile:
  type: RuntimeDefault
capabilities:
  drop: ["ALL"]
{{- end }}
