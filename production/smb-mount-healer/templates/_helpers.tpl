{{/*
Chart name (nameOverride wins).
*/}}
{{- define "smb-mount-healer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Resource name prefix. With release name "smb-mount-healer" this is
"smb-mount-healer" (the e2e suite and the alerts rely on these names).
Truncated to 45 characters so "<fullname>-controlplane-metrics" stays a valid
Service name (63).
*/}}
{{- define "smb-mount-healer.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 45 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 45 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 45 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart label value.
*/}}
{{- define "smb-mount-healer.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels (cluster-scoped and shared objects).
*/}}
{{- define "smb-mount-healer.labels" -}}
helm.sh/chart: {{ include "smb-mount-healer.chart" . }}
app.kubernetes.io/name: {{ include "smb-mount-healer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: smb-mount-healer
{{- end }}

{{/*
Selector labels of a component. Pods are selected by
app.kubernetes.io/component (controlplane|worker).
Usage: include "smb-mount-healer.selectorLabels" (dict "context" . "component" "worker")
*/}}
{{- define "smb-mount-healer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "smb-mount-healer.name" .context }}
app.kubernetes.io/instance: {{ .context.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Common labels plus the component label.
Usage: include "smb-mount-healer.componentLabels" (dict "context" . "component" "worker")
*/}}
{{- define "smb-mount-healer.componentLabels" -}}
{{ include "smb-mount-healer.labels" .context }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Per-component name: <fullname>-<component>.
Usage: include "smb-mount-healer.componentName" (dict "context" . "component" "worker")
*/}}
{{- define "smb-mount-healer.componentName" -}}
{{- printf "%s-%s" (include "smb-mount-healer.fullname" .context) .component }}
{{- end }}

{{/*
ServiceAccount name of a component (created when serviceAccount.create).
Usage: include "smb-mount-healer.serviceAccountName" (dict "context" . "component" "worker")
*/}}
{{- define "smb-mount-healer.serviceAccountName" -}}
{{- include "smb-mount-healer.componentName" . }}
{{- end }}

{{/*
Image reference: image.reference, else repository:tag[@digest] with tag
defaulting to the chart appVersion.
*/}}
{{- define "smb-mount-healer.image" -}}
{{- if .Values.image.reference }}
{{- .Values.image.reference }}
{{- else }}
{{- $ref := printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- if .Values.image.digest }}
{{- $ref = printf "%s@%s" $ref .Values.image.digest }}
{{- end }}
{{- $ref }}
{{- end }}
{{- end }}

{{/*
Quarantine directory on the nodes.
*/}}
{{- define "smb-mount-healer.quarantineDir" -}}
{{- default (printf "%s/smb-mount-healer-quarantine" (trimSuffix "/" .Values.kubeletRoot)) .Values.quarantineDir }}
{{- end }}

{{/*
Fail when a PodMonitor is requested but the Prometheus Operator CRD is missing.
*/}}
{{- define "smb-mount-healer.requirePodMonitor" -}}
{{- if not (.Capabilities.APIVersions.Has "monitoring.coreos.com/v1/PodMonitor") }}
{{- fail "metrics.podMonitor.enabled=true needs the monitoring.coreos.com/v1 PodMonitor CRD (Prometheus Operator); install it or set metrics.podMonitor.enabled=false" }}
{{- end }}
{{- end }}

{{/*
Fail when a PrometheusRule is requested but the Prometheus Operator CRD is missing.
*/}}
{{- define "smb-mount-healer.requirePrometheusRule" -}}
{{- if not (.Capabilities.APIVersions.Has "monitoring.coreos.com/v1/PrometheusRule") }}
{{- fail "monitoring.prometheusRule.enabled=true needs the monitoring.coreos.com/v1 PrometheusRule CRD (Prometheus Operator); install it or set monitoring.prometheusRule.enabled=false" }}
{{- end }}
{{- end }}
