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

{{/*
A Go duration string (values.schema.json "duration", e.g. 90s, 1m30s, 1.5h)
in seconds, as a float.
*/}}
{{- define "smb-mount-healer.durationSeconds" -}}
{{- $total := 0.0 }}
{{- range regexFindAll "[0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h)" (toString .) -1 }}
{{- $n := float64 (regexFind "^[0-9]+(\\.[0-9]+)?" .) }}
{{- $unit := regexReplaceAll "^[0-9]+(\\.[0-9]+)?" . "" }}
{{- $f := get (dict "h" 3600.0 "m" 60.0 "s" 1.0 "ms" 0.001 "us" 0.000001 "µs" 0.000001 "ns" 0.000000001) $unit }}
{{- $total = addf $total (mulf $n $f) }}
{{- end }}
{{- $total }}
{{- end }}

{{/*
Fail unless controlplane.remediation.verifyWait is longer than
worker.probing.slowRecoveryWindow + recheckDelay + statTimeout. A replacement
pod whose mount saw one slow probe stays Suspect for slowRecoveryWindow (it
is re-probed every recheckDelay, a probe takes up to statTimeout); a shorter
verifyWait can never verify it and ends such a remediation verify_failed.
*/}}
{{- define "smb-mount-healer.validateVerifyWait" -}}
{{- $p := .Values.worker.probing }}
{{- $verify := include "smb-mount-healer.durationSeconds" .Values.controlplane.remediation.verifyWait | float64 }}
{{- $window := include "smb-mount-healer.durationSeconds" $p.slowRecoveryWindow | float64 }}
{{- $recheck := include "smb-mount-healer.durationSeconds" $p.recheckDelay | float64 }}
{{- $stat := include "smb-mount-healer.durationSeconds" $p.statTimeout | float64 }}
{{- if and (gt $window 0.0) (le $verify (addf $window $recheck $stat)) }}
{{- fail (printf "controlplane.remediation.verifyWait (%s) must be longer than worker.probing.slowRecoveryWindow (%s) + recheckDelay (%s) + statTimeout (%s): a replacement pod with one slow probe stays Suspect that long and could never be verified" (toString .Values.controlplane.remediation.verifyWait) (toString $p.slowRecoveryWindow) (toString $p.recheckDelay) (toString $p.statTimeout)) }}
{{- end }}
{{- end }}
