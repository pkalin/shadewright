{{/*
Expand the name of the chart.
*/}}
{{- define "shadewright.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "shadewright.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Namespace for generated references.
Always uses the Helm release namespace.
*/}}
{{- define "shadewright.namespaceName" -}}
{{- .Release.Namespace }}
{{- end }}

{{/*
Resource name with proper truncation for Kubernetes 63-character limit.
Takes a dict with:
  - .suffix: Resource name suffix (e.g., "metrics", "webhook")
  - .context: Template context (root context with .Values, .Release, etc.)
Dynamically calculates safe truncation to ensure total name length <= 63 chars.
*/}}
{{- define "shadewright.resourceName" -}}
{{- $fullname := include "shadewright.fullname" .context }}
{{- $suffix := .suffix }}
{{- $maxLen := sub 62 (len $suffix) | int }}
{{- if gt (len $fullname) $maxLen }}
{{- printf "%s-%s" (trunc $maxLen $fullname | trimSuffix "-") $suffix | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" $fullname $suffix | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Selector labels of one component (the Deployment selector, the Service and NetworkPolicy selectors).
Takes a dict with .component ("executor" or "analyzer") and .context.
*/}}
{{- define "shadewright.selectorLabels" -}}
app.kubernetes.io/name: {{ include "shadewright.name" .context }}
app.kubernetes.io/component: {{ .component }}
control-plane: {{ .component }}
{{- end }}

{{/*
Common labels, on every object of the chart (test/chart checks it). Takes a dict with .component (may be empty)
and .context.
*/}}
{{- define "shadewright.labels" -}}
app.kubernetes.io/name: {{ include "shadewright.name" .context }}
app.kubernetes.io/instance: {{ .context.Release.Name }}
app.kubernetes.io/version: {{ .context.Chart.AppVersion | quote }}
app.kubernetes.io/part-of: shadewright
app.kubernetes.io/managed-by: {{ .context.Release.Service }}
helm.sh/chart: {{ .context.Chart.Name }}-{{ .context.Chart.Version | replace "+" "_" }}
{{- if .component }}
app.kubernetes.io/component: {{ .component }}
{{- end }}
{{- end }}

{{/*
Image reference of a component. The chart published on a release tag carries the digests of that release's
signed images in values.yaml (the Chart workflow writes them before packaging), so it runs repository@digest. A
source checkout has no digest and falls back to the appVersion tag. Takes a dict with .component and .context.
*/}}
{{- define "shadewright.image" -}}
{{- $c := index .context.Values .component -}}
{{- if $c.image.digest -}}
{{- printf "%s@%s" $c.image.repository $c.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $c.image.repository .context.Chart.AppVersion -}}
{{- end -}}
{{- end }}

{{/*
Cluster ID hashed into every Incident fingerprint (--cluster-id). An explicit clusterId wins. Otherwise the UID
of the kube-system namespace: stable for the life of the cluster and unique, read with lookup at install and
upgrade time. helm template, helm lint and a client-side --dry-run ask no cluster, so lookup finds nothing there
and the chart fails with a clear message instead of rendering an ID that would change at install. It is an ID,
never a display name.
*/}}
{{- define "shadewright.clusterId" -}}
{{- if .Values.clusterId -}}
{{- .Values.clusterId -}}
{{- else -}}
{{- $ns := lookup "v1" "Namespace" "" "kube-system" | default dict -}}
{{- $uid := dig "metadata" "uid" "" $ns -}}
{{- if not $uid -}}
{{- fail "clusterId is not set and the UID of the kube-system namespace could not be read (helm template, helm lint and --dry-run ask no cluster): set clusterId, for example --set clusterId=<your cluster>" -}}
{{- end -}}
{{- $uid -}}
{{- end -}}
{{- end }}

{{/*
Image pull secrets of one component: the global list and the component's own, without duplicates. Takes a dict
with .component and .context. Renders nothing when both are empty.
*/}}
{{- define "shadewright.imagePullSecrets" -}}
{{- $c := index .context.Values .component -}}
{{- $names := list -}}
{{- range concat (.context.Values.imagePullSecrets | default list) ($c.imagePullSecrets | default list) -}}
{{- $names = append $names .name -}}
{{- end -}}
{{- with $names | uniq -}}
imagePullSecrets:
{{- range . }}
- name: {{ . | quote }}
{{- end }}
{{- end -}}
{{- end }}

{{/*
Flags the chart renders itself in the Deployments. extraArgs may not name them, so a free list cannot turn off
leader election, the metrics authentication or the watched namespaces. Keep in step with the args in
templates/workloads. "namespace" is not an arg of the chart but the Pods get it from POD_NAMESPACE.
*/}}
{{- define "shadewright.reservedFlags" -}}
leader-elect health-probe-bind-address metrics-bind-address metrics-secure namespace watch-namespaces cluster-id max-incidents
{{- end }}

{{/*
Refuses values that would weaken the separate identities (invariant 6) or the flags and labels the chart sets.
Takes a dict with .component ("executor" or "analyzer") and .context. Renders nothing.
*/}}
{{- define "shadewright.validate" -}}
{{- $v := .context.Values -}}
{{- $c := index $v .component -}}
{{- $ex := $v.executor.identity.azure.clientId | default "" | lower -}}
{{- $an := $v.analyzer.identity.azure.clientId | default "" | lower -}}
{{- if and $ex $an (eq $ex $an) -}}
{{- fail "executor.identity.azure.clientId and analyzer.identity.azure.clientId are equal: the executor and the analyzer must have separate identities, never one for both" -}}
{{- end -}}
{{- range $k, $_ := $c.serviceAccount.annotations -}}
{{- if hasPrefix "azure.workload.identity/" $k -}}
{{- fail (printf "%s.serviceAccount.annotations may not set %q: use %s.identity.azure.clientId" $.component $k $.component) -}}
{{- end -}}
{{- end -}}
{{- range $k, $_ := $c.podLabels -}}
{{- if or (hasPrefix "azure.workload.identity/" $k) (hasPrefix "app.kubernetes.io/" $k) (hasPrefix "helm.sh/" $k) (eq $k "control-plane") -}}
{{- fail (printf "%s.podLabels may not set %q: the chart sets it (identity labels come from %s.identity.azure.clientId)" $.component $k $.component) -}}
{{- end -}}
{{- end -}}
{{- $reserved := splitList " " (include "shadewright.reservedFlags" .context) -}}
{{- range $c.extraArgs -}}
{{- $name := . | trimPrefix "--" | splitList "=" | first -}}
{{- if has $name $reserved -}}
{{- fail (printf "%s.extraArgs may not set --%s: the chart sets it" $.component $name) -}}
{{- end -}}
{{- end -}}
{{- end }}
