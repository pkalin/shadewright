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
source checkout has no digest: it fails closed, unless image.allowTag opts in to the appVersion tag. Takes a
dict with .component and .context.
*/}}
{{- define "shadewright.image" -}}
{{- $c := index .context.Values .component -}}
{{- if $c.image.digest -}}
{{- printf "%s@%s" $c.image.repository $c.image.digest -}}
{{- else if .context.Values.image.allowTag -}}
{{- printf "%s:%s" $c.image.repository .context.Chart.AppVersion -}}
{{- else -}}
{{- fail (printf "%s.image.digest is empty: set it (sha256:<64 hex digits>), or set image.allowTag=true to run the %s tag with images built from this same checkout" .component .context.Chart.AppVersion) -}}
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
Image pull secrets of one Pod: the global list and the component's own (executor, analyzer or tests), without
duplicates. Takes a dict with .component and .context. Renders nothing when both are empty.
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
The analyzer's module grants that the chart renders (templates/rbac/analyzer-module-roles.yaml), comma-separated,
as --module-grants takes them: the analyzer watches only the module kinds of these grants, and leaves the optional
kyverno-cluster out without reporting it as missing when it is not listed.
*/}}
{{- define "shadewright.analyzerModuleGrants" -}}
{{- $m := .Values.analyzer.modules -}}
{{- $g := list -}}
{{- if $m.keda.enabled }}{{ $g = append $g "keda" }}{{ end -}}
{{- if $m.flux.enabled }}{{ $g = append $g "flux" }}{{ end -}}
{{- if $m.kyverno.enabled }}{{ $g = append $g "kyverno" }}{{ end -}}
{{- if $m.kyverno.clusterWide.enabled }}{{ $g = append $g "kyverno-cluster" }}{{ end -}}
{{- join "," $g -}}
{{- end }}

{{/*
Flags the chart renders itself in the Deployments. extraArgs may not name them, so a free list cannot turn off
leader election, the metrics authentication or the watched namespaces. Keep in step with the args in
templates/workloads. "namespace" is not an arg of the chart but the Pods get it from POD_NAMESPACE.
*/}}
{{- define "shadewright.reservedFlags" -}}
leader-elect health-probe-bind-address metrics-bind-address metrics-secure metrics-cert-path metrics-cert-name metrics-cert-key namespace watch-namespaces cluster-id max-incidents pod-logs prometheus-url prometheus-cluster triage-model escalation-model model-endpoint-suffixes action-policies analysis-store-account analysis-store-container module-grants
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
{{- if $c.metricsTLS.secretName -}}
{{- if not (and $v.metrics.enabled $v.metrics.secure) -}}
{{- fail (printf "%s.metricsTLS.secretName needs metrics.enabled and metrics.secure: a serving certificate is only used by the HTTPS metrics endpoint" $.component) -}}
{{- end -}}
{{- end -}}
{{- if eq .component "analyzer" -}}
{{- $a := $c.analysis | default dict -}}
{{- if and $a.triageModel (not $c.identity.azure.clientId) -}}
{{- fail "analyzer.analysis.triageModel needs analyzer.identity.azure.clientId: the model call authenticates with the analyzer's Azure workload identity" -}}
{{- end -}}
{{- if and $a.triageModel (not $a.endpointSuffixes) -}}
{{- fail "analyzer.analysis.triageModel needs analyzer.analysis.endpointSuffixes: no model endpoint host is allowed by default, so list your own resource, such as .<resource>.services.ai.azure.com" -}}
{{- end -}}
{{- if and $a.escalationModel (not $a.triageModel) -}}
{{- fail "analyzer.analysis.escalationModel needs analyzer.analysis.triageModel" -}}
{{- end -}}
{{- $blob := (($a.recordStore | default dict).azureBlob | default dict) -}}
{{- if and $blob.account (not $a.triageModel) -}}
{{- fail "analyzer.analysis.recordStore.azureBlob.account needs analyzer.analysis.triageModel: records are written only by the analysis" -}}
{{- end -}}
{{- if and $blob.account (not $blob.container) -}}
{{- fail "analyzer.analysis.recordStore.azureBlob.account needs analyzer.analysis.recordStore.azureBlob.container" -}}
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

{{/*
The DNS egress rule of networkPolicy.dns: the one rule the analyzer and executor NetworkPolicies and the helm test
hook's NetworkPolicy all use, so the test passes only if the workloads can resolve too. Mode "kubeDns" (default)
selects the kube-dns Pods in kube-system; mode "resolverCIDRs" selects the listed resolver addresses (NodeLocal
DNSCache). Port 53 over UDP and TCP in both.
*/}}
{{- define "shadewright.dnsEgressRule" -}}
{{- $dns := .Values.networkPolicy.dns -}}
- ports:
  - port: 53
    protocol: UDP
  - port: 53
    protocol: TCP
  to:
{{- if eq $dns.mode "resolverCIDRs" }}
{{- if not $dns.resolverCIDRs }}
{{- fail "networkPolicy.dns.resolverCIDRs is empty: list the resolver address of NodeLocal DNSCache, or use networkPolicy.dns.mode=kubeDns" }}
{{- end }}
{{- range $dns.resolverCIDRs }}
  - ipBlock:
      cidr: {{ . | quote }}
{{- end }}
{{- else if eq $dns.mode "kubeDns" }}
  - namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: kube-system
    podSelector:
      matchLabels:
        k8s-app: kube-dns
{{- else }}
{{- fail (printf "networkPolicy.dns.mode %q is not kubeDns or resolverCIDRs" $dns.mode) }}
{{- end }}
{{- end }}

{{/*
Affinity of one component's Pod: the configured affinity, plus a preferred anti-affinity by node when the
component runs standby replicas (replicas above 1) and the configured affinity sets no podAntiAffinity itself.
Takes a dict with .component and .context. Renders nothing when the result is empty.
*/}}
{{- define "shadewright.affinity" -}}
{{- $c := index .context.Values .component }}
{{- $aff := deepCopy (default dict $c.affinity) }}
{{- if and (gt (int $c.replicas) 1) (not $aff.podAntiAffinity) }}
{{- $term := dict "weight" 100 "podAffinityTerm" (dict "topologyKey" "kubernetes.io/hostname" "labelSelector" (dict "matchLabels" (include "shadewright.selectorLabels" . | fromYaml))) }}
{{- $_ := set $aff "podAntiAffinity" (dict "preferredDuringSchedulingIgnoredDuringExecution" (list $term)) }}
{{- end }}
{{- if $aff }}
{{- toYaml $aff }}
{{- end }}
{{- end }}

{{/*
GOMEMLIMIT of one component's container, in bytes: the goMemLimit value when set (a Go memory limit such as
"100MiB"), else 90 percent of resources.limits.memory, so the Go runtime collects before the heap reaches the
limit and a bounded parse is not OOM-killed (#601). Takes a dict with .component and .context. Reads the limit
as an integer or a Kubernetes quantity with a binary (Ki, Mi, Gi, Ti) or decimal (k, K, M, G, T) suffix; any
other form (for example "1.5Gi" or "1e9") fails the render and asks for goMemLimit.
*/}}
{{- define "shadewright.goMemLimit" -}}
{{- $c := index .context.Values .component }}
{{- if $c.goMemLimit }}
{{- $c.goMemLimit }}
{{- else }}
{{- $raw := $c.resources.limits.memory }}
{{- $q := ternary (printf "%.0f" $raw) (toString $raw) (kindIs "float64" $raw) }}
{{- $re := "^([0-9]+)(Ki|Mi|Gi|Ti|k|K|M|G|T)?$" }}
{{- if not (regexMatch $re $q) }}
{{- fail (printf "%s.resources.limits.memory %q is not an integer or a whole number with a Ki, Mi, Gi, Ti, k, K, M, G or T suffix: set %s.goMemLimit" .component $q .component) }}
{{- end }}
{{- $mult := dict "" 1 "Ki" 1024 "Mi" 1048576 "Gi" 1073741824 "Ti" 1099511627776 "k" 1000 "K" 1000 "M" 1000000 "G" 1000000000 "T" 1000000000000 }}
{{- $bytes := mul (int64 (regexReplaceAll $re $q "${1}")) (int64 (index $mult (regexReplaceAll $re $q "${2}"))) }}
{{- div (mul $bytes 90) 100 }}
{{- end }}
{{- end }}
