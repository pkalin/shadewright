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
Takes a dict with .component ("executor", "analyzer" or "verifier") and .context.
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
Image pull secrets of one Pod: the global list and the component's own (executor, analyzer, verifier or tests), without
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
The namespaces the verifier watches, as a JSON list (read it with fromJsonArray): verifier.watchNamespaces when it
is set (a list, [] included, is used as it is), else analyzer.watchNamespaces while verifier.watchNamespaces is
null. --watch-namespaces and the RoleBindings of verifier-watch-rolebindings.yaml both take this one list.
*/}}
{{- define "shadewright.verifierWatchNamespaces" -}}
{{- $v := .Values.verifier.watchNamespaces -}}
{{- if kindIs "invalid" $v -}}
{{- $v = .Values.analyzer.watchNamespaces -}}
{{- end -}}
{{- toJson ($v | default list) -}}
{{- end }}

{{/*
"true" when the verifier is deployed, else empty: only with at least one namespace in
shadewright.verifierWatchNamespaces. Without one the chart renders no verifier object at all (Deployment,
ServiceAccount, Roles, bindings, PDB, metrics Service, NetworkPolicy, Lease, ServiceMonitor, the metrics-auth
subject), and a merged request can end only Inconclusive.
*/}}
{{- define "shadewright.verifierEnabled" -}}
{{- if (include "shadewright.verifierWatchNamespaces" . | fromJsonArray) -}}true{{- end -}}
{{- end }}

{{/*
Flags the chart renders itself in the Deployments. extraArgs may not name them, so a free list cannot turn off
leader election, the metrics authentication or the watched namespaces. Keep in step with the args in
templates/workloads. "namespace" is not an arg of the chart but the Pods get it from POD_NAMESPACE.
*/}}
{{- define "shadewright.reservedFlags" -}}
leader-elect health-probe-bind-address metrics-bind-address metrics-secure metrics-cert-path metrics-cert-name metrics-cert-key namespace watch-namespaces cluster-id max-incidents pod-logs prometheus-url prometheus-cluster triage-model escalation-model model-endpoint-suffixes action-policies anthropic-api-key-file disable-foundry openai-compatible-endpoints openai-compatible-in-cluster-cidrs analysis-store-account analysis-store-container module-grants reviewer-hints node-grace verify-outcomes
{{- end }}

{{/*
Refuses values that would weaken the separate identities (invariant 6) or the flags and labels the chart sets.
Takes a dict with .component ("executor", "analyzer" or "verifier") and .context. Renders nothing.
*/}}
{{- define "shadewright.validate" -}}
{{- $v := .context.Values -}}
{{- $c := index $v .component -}}
{{- $ex := $v.executor.identity.azure.clientId | default "" | lower -}}
{{- $an := $v.analyzer.identity.azure.clientId | default "" | lower -}}
{{- if and $ex $an (eq $ex $an) -}}
{{- fail "executor.identity.azure.clientId and analyzer.identity.azure.clientId are equal: the executor and the analyzer must have separate identities, never one for both" -}}
{{- end -}}
{{- if ne .component "verifier" -}}
{{- range $k, $_ := $c.serviceAccount.annotations -}}
{{- if hasPrefix "azure.workload.identity/" $k -}}
{{- fail (printf "%s.serviceAccount.annotations may not set %q: use %s.identity.azure.clientId" $.component $k $.component) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range $k, $_ := $c.podLabels -}}
{{- if and (eq $.component "verifier") (hasPrefix "azure.workload.identity/" $k) -}}
{{- fail (printf "verifier.podLabels may not set %q: the verifier has no cloud identity" $k) -}}
{{- else if or (hasPrefix "azure.workload.identity/" $k) (hasPrefix "app.kubernetes.io/" $k) (hasPrefix "helm.sh/" $k) (eq $k "control-plane") -}}
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
{{- $key := (($a.anthropic | default dict).apiKey | default dict) -}}
{{- /* The chart checks only what it renders itself. For the identity and suffix checks it does not read settings.spec: the RemediationSettings is
created once and never changed by an upgrade, so the values cannot tell which deployments the cluster holds. The
analyzer is the gate for them: without --model-endpoint-suffixes it builds no token source and refuses every
Foundry deployment (#814). */ -}}
{{- if and $a.triageModel (not $a.endpointSuffixes) (not (($a.openAICompatible | default dict).endpoints)) (not (or $key.secretName $key.volume)) -}}
{{- fail "analyzer.analysis.triageModel needs analyzer.analysis.endpointSuffixes (Foundry deployments: no model endpoint host is allowed by default, so list your own resource, such as .<resource>.services.ai.azure.com) or analyzer.analysis.openAICompatible.endpoints (OpenAICompatible deployments) or analyzer.analysis.anthropic.apiKey (Anthropic deployments)" -}}
{{- end -}}
{{- if and $a.triageModel $a.endpointSuffixes (not $a.disableFoundry) (not $c.identity.azure.clientId) -}}
{{- fail "analyzer.analysis.endpointSuffixes needs analyzer.identity.azure.clientId: with a triage model and suffixes the analyzer builds an Azure token source, which needs the analyzer's workload identity" -}}
{{- end -}}
{{- $compatible := $a.openAICompatible | default dict -}}
{{- if and (or $a.disableFoundry $compatible.endpoints) (not $a.triageModel) -}}
{{- fail "analyzer.analysis.openAICompatible and disableFoundry need triageModel" -}}
{{- end -}}
{{- if and $compatible.inClusterCIDRs (not $compatible.endpoints) -}}
{{- fail "analyzer.analysis.openAICompatible.inClusterCIDRs needs analyzer.analysis.openAICompatible.endpoints" -}}
{{- end -}}
{{- if and $compatible.inClusterCIDRs (not (kindIs "slice" $compatible.inClusterCIDRs)) -}}
{{- fail "analyzer.analysis.openAICompatible.inClusterCIDRs must be a list of CIDRs" -}}
{{- end -}}
{{- range $compatible.inClusterCIDRs -}}
{{- $problem := include "shadewright.inClusterCIDRProblem" . -}}
{{- if $problem -}}
{{- fail (printf "analyzer.analysis.openAICompatible.inClusterCIDRs entry %q %s" (toString .) $problem) -}}
{{- end -}}
{{- end -}}
{{- $seen := dict -}}
{{- /* A key belongs to one declaration (ADR-0042): the analyzer compares key files by path and identity, but two
mounts of one Secret key are two files to it, so the chart refuses what it can see, a Secret key or volume
source shared by two endpoints or with the Anthropic key. */ -}}
{{- $anthropicKeyID := include "shadewright.keySourceID" $key -}}
{{- $keyIDs := dict -}}
{{- range $i, $_ := $compatible.endpoints -}}
{{- $keyID := include "shadewright.keySourceID" (.apiKey | default dict) -}}
{{- if $keyID -}}
{{- if eq $keyID $anthropicKeyID -}}
{{- fail (printf "openAICompatible.endpoints[%d] and analyzer.analysis.anthropic.apiKey mount the same key: a key belongs to one declaration" $i) -}}
{{- end -}}
{{- if hasKey $keyIDs $keyID -}}
{{- fail (printf "openAICompatible.endpoints[%s] and endpoints[%d] mount the same key: a key belongs to one declaration" (get $keyIDs $keyID) $i) -}}
{{- end -}}
{{- $_ := set $keyIDs $keyID (toString $i) -}}
{{- end -}}
{{- $base := include "shadewright.compatibleBase" .url -}}
{{- if hasKey $seen $base -}}{{- fail "openAICompatible.endpoints contains duplicate base URLs" -}}{{- end -}}
{{- range $previous, $_ := $seen -}}
{{- if or (hasPrefix (printf "%s/" $previous) $base) (hasPrefix (printf "%s/" $base) $previous) -}}
{{- fail "openAICompatible endpoint base is a path prefix of another declaration" -}}
{{- end -}}
{{- end -}}
{{- $_ := set $seen $base true -}}
{{- if or (not (kindIs "slice" .models)) (not .models) -}}{{- fail "openAICompatible endpoint needs a nonempty models list" -}}{{- end -}}
{{- range .models -}}
{{- if not (kindIs "string" .) -}}{{- fail "openAICompatible endpoint model name must be a string" -}}{{- end -}}
{{- if not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$" .) -}}{{- fail "openAICompatible endpoint model name is invalid" -}}{{- end -}}
{{- end -}}
{{- $host := (urlParse $base).hostname -}}
{{- if or (eq $host "api.anthropic.com") (regexMatch "(^|\\.)(openai\\.azure\\.com|cognitiveservices\\.azure\\.com|services\\.ai\\.azure\\.com)$" $host) -}}
{{- fail "openAICompatible endpoint belongs to Azure or Anthropic" -}}{{- end -}}
{{- range $a.endpointSuffixes -}}
{{- if or (eq $host (trimPrefix "." .)) (hasSuffix . $host) -}}
{{- fail "openAICompatible endpoint host matches Foundry endpointSuffixes" -}}{{- end -}}
{{- end -}}
{{- if not (has .inferenceAPI (list "ChatCompletions" "Responses")) -}}{{- fail "openAICompatible endpoint needs inferenceAPI" -}}{{- end -}}
{{- $key := .apiKey | default dict -}}
{{- if and $key.secretName $key.volume -}}{{- fail "openAICompatible apiKey sets both secretName and volume" -}}{{- end -}}
{{- if $key.volume -}}
{{- if or (ne (len $key.volume) 1) (not (or (hasKey $key.volume "secret") (hasKey $key.volume "csi"))) -}}
{{- fail "openAICompatible apiKey.volume must contain only secret or csi" -}}{{- end -}}
{{- if hasKey $key.volume "secret" -}}
{{- if or (not (kindIs "map" $key.volume.secret)) (not $key.volume.secret.secretName) -}}{{- fail "openAICompatible secret source needs secretName" -}}{{- end -}}
{{- else -}}
{{- if or (not (kindIs "map" $key.volume.csi)) (not $key.volume.csi.driver) -}}{{- fail "openAICompatible csi source needs driver" -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- if or $key.secretName $key.volume -}}
{{- $file := include "shadewright.compatibleKeyFile" . -}}
{{- if not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$" $file) -}}{{- fail "openAICompatible key filename is invalid" -}}{{- end -}}
{{- end -}}
{{- with .service -}}
{{- if or (not .namespace) (not .podSelector) (lt (int .port) 1) (gt (int .port) 65535) (not (regexMatch "^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.svc(\\.cluster\\.local)?$" $host)) (ne (index (splitList "." $host) 1) .namespace) -}}
{{- fail "openAICompatible service needs its DNS namespace, podSelector and target port" -}}{{- end -}}
{{- /* The analyzer's rules (openaicompatible.ValidateEndpoints): 1 to 32 entries, each key a qualified name and each value a label value. */ -}}
{{- if or (not (kindIs "map" .podSelector)) (gt (len .podSelector) 32) -}}
{{- fail "openAICompatible service podSelector must have 1 to 32 labels" -}}{{- end -}}
{{- range $k, $v := .podSelector -}}
{{- $parts := splitList "/" $k -}}
{{- $name := last $parts -}}
{{- $prefixOK := or (eq (len $parts) 1) (and (eq (len $parts) 2) (le (len (first $parts)) 253) (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" (first $parts))) -}}
{{- $nameOK := and (le (len $name) 63) (regexMatch "^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$" $name) -}}
{{- $valueOK := and (kindIs "string" $v) (le (len (toString $v)) 63) (regexMatch "^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$" (toString $v)) -}}
{{- if not (and $prefixOK $nameOK $valueOK) -}}
{{- fail "openAICompatible service podSelector has an invalid label" -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and $a.escalationModel (not $a.triageModel) -}}
{{- fail "analyzer.analysis.escalationModel needs analyzer.analysis.triageModel" -}}
{{- end -}}
{{- $blob := (($a.recordStore | default dict).azureBlob | default dict) -}}
{{- if and $blob.account (not $a.triageModel) -}}
{{- fail "analyzer.analysis.recordStore.azureBlob.account needs analyzer.analysis.triageModel: records are written only by the analysis" -}}
{{- end -}}
{{- if and $blob.account (not $c.identity.azure.clientId) -}}
{{- fail "analyzer.analysis.recordStore.azureBlob.account needs analyzer.identity.azure.clientId: records are written with the analyzer's Azure workload identity" -}}
{{- end -}}
{{- if and $blob.account (not $blob.container) -}}
{{- fail "analyzer.analysis.recordStore.azureBlob.account needs analyzer.analysis.recordStore.azureBlob.container" -}}
{{- end -}}
{{- if and $key.secretName $key.volume -}}
{{- fail "analyzer.analysis.anthropic.apiKey sets both secretName and volume: set one" -}}
{{- end -}}
{{- if and $key.volume (hasKey $key.volume "name") -}}
{{- fail "analyzer.analysis.anthropic.apiKey.volume may not set name: the chart names the volume itself" -}}
{{- end -}}
{{- if and $.context.Values.settings.spec (not $key.secretName) (not $key.volume) -}}
{{- /* A convenience at install only: the analyzer refuses an Anthropic deployment with no key file whatever the chart says (ADR-0037 decision 3). */ -}}
{{- $models := (($.context.Values.settings.spec.analysis | default dict).models | default dict) -}}
{{- range $role := list "triage" "escalation" -}}
{{- if eq ((get $models $role | default dict).provider | default "") "Anthropic" -}}
{{- fail (printf "settings.spec.analysis.models.%s has provider Anthropic: set analyzer.analysis.anthropic.apiKey.secretName (an existing Secret) or apiKey.volume, the analyzer has no model API key otherwise" $role) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $reserved := splitList " " (include "shadewright.reservedFlags" .context) -}}
{{- range $c.extraArgs -}}
{{- $name := . | trimPrefix "--" | splitList "=" | first -}}
{{- if and (eq $name "reviewer-hints") (eq $.component "analyzer") -}}
{{- fail "analyzer.extraArgs may not set --reviewer-hints: it is an executor flag, turned on with executor.reviewerHints.enabled" -}}
{{- else if and (eq $name "reviewer-hints") (eq $.component "executor") -}}
{{- fail "executor.extraArgs may not set --reviewer-hints: use executor.reviewerHints.enabled" -}}
{{- else if and (eq $name "node-grace") (eq $.component "executor") -}}
{{- fail "executor.extraArgs may not set --node-grace: use executor.verification.nodeGrace" -}}
{{- else if and (eq $name "verify-outcomes") (eq $.component "executor") -}}
{{- fail "executor.extraArgs may not set --verify-outcomes: use executor.verification.enabled" -}}
{{- else if has $name $reserved -}}
{{- fail (printf "%s.extraArgs may not set --%s: the chart sets it" $.component $name) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The DNS egress rule of networkPolicy.dns: the one rule the analyzer, executor and verifier NetworkPolicies and the helm test
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
limit and a bounded parse is not OOM-killed (#601). The executor's default is 36 percent instead: its container
also holds the render process of the render check, which the executor stops at 192 MiB resident, and the rest
covers that and a 128 MiB margin (ADR-0035, checked by shadewright.executorMemoryBudget).
Takes a dict with .component and .context. Reads the limit with shadewright.memoryLimitBytes and fails the
render, asking for goMemLimit, on a form it cannot read.
*/}}
{{- define "shadewright.goMemLimit" -}}
{{- $c := index .context.Values .component }}
{{- if $c.goMemLimit }}
{{- $c.goMemLimit }}
{{- else }}
{{- $read := include "shadewright.memoryLimitBytes" . }}
{{- if not $read }}
{{- fail (printf "%s.resources.limits.memory %q is not a plain number of bytes or a number with an optional fraction and a Ki, Mi, Gi, Ti, k, K, M, G or T suffix, so the chart cannot take a share of it: set %s.goMemLimit" .component (include "shadewright.memoryLimitString" .) .component) }}
{{- end }}
{{- $percent := ternary 36 90 (eq .component "executor") }}
{{- /* The hundredths first and the rest apart, so a large limit cannot overflow the integer arithmetic of the
template, and the result is still exactly the percentage rounded down. */}}
{{- $l := $read | int64 }}
{{- add (mul (div $l 100) $percent) (div (mul (mod $l 100) $percent) 100) }}
{{- end }}
{{- end }}

{{/*
resources.limits.memory of one component as written, a number rendered without an exponent.
Takes a dict with .component and .context.
*/}}
{{- define "shadewright.memoryLimitString" -}}
{{- $raw := (index .context.Values .component).resources.limits.memory }}
{{- /* A fractional number of bytes (through --set-json) is rounded up, as Kubernetes rounds a quantity. */}}
{{- ternary (printf "%.0f" (ceil $raw)) (toString $raw) (kindIs "float64" $raw) }}
{{- end }}

{{/*
resources.limits.memory of one component in bytes, rounded up as Kubernetes rounds a quantity, or nothing for a
form it does not read. Reads the forms a memory limit is written in: a number of bytes, or a number with a
binary (Ki, Mi, Gi, Ti) or decimal (k, K, M, G, T) suffix, each with an optional fraction of up to six digits
("1.5Gi", "0.5Gi", "600M", "536870912"). Other valid quantities, such as exponents ("1e9") or milli ("m"), give
nothing. A limit of 1 Ei or more is read as 1 Ei, larger than any budget, so the template's integer arithmetic
cannot overflow. Takes a dict with .component and .context.
*/}}
{{- define "shadewright.memoryLimitBytes" -}}
{{- $q := include "shadewright.memoryLimitString" . }}
{{- $re := "^([0-9]+)(?:\\.([0-9]{1,6}))?(Ki|Mi|Gi|Ti|k|K|M|G|T)?$" }}
{{- if regexMatch $re $q }}
{{- $mult := dict "" 1 "Ki" 1024 "Mi" 1048576 "Gi" 1073741824 "Ti" 1099511627776 "k" 1000 "K" 1000 "M" 1000000 "G" 1000000000 "T" 1000000000000 }}
{{- $m := int64 (index $mult (regexReplaceAll $re $q "${3}")) }}
{{- /* Leading zeros go first: the conversion to an integer would read "010" as octal. */}}
{{- $whole := regexReplaceAll "^0+([0-9])" (regexReplaceAll $re $q "${1}") "${1}" }}
{{- $frac := regexReplaceAll $re $q "${2}" }}
{{- $cap := int64 1152921504606846976 }}
{{- /* More than 18 digits do not fit an int64; fewer that pass the cap once multiplied are capped first. */}}
{{- if or (gt (len $whole) 18) (gt (int64 $whole) (div $cap $m)) }}
{{- $cap }}
{{- else }}
{{- $bytes := mul (int64 $whole) $m }}
{{- if $frac }}
{{- $scale := int64 (printf "1%s" (repeat (len $frac) "0")) }}
{{- $digits := regexReplaceAll "^0+([0-9])" $frac "${1}" }}
{{- $bytes = add $bytes (div (sub (add (mul (int64 $digits) $m) $scale) 1) $scale) }}
{{- end }}
{{- min $bytes $cap }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Fails the render when the executor's memory limit cannot hold the executor and one render process of the render
check (ADR-0035): its Go soft limit (GOMEMLIMIT, shadewright.goMemLimit), plus the render process's resident
limit (192 MiB, semdiff.MaxRenderResident, where the executor kills it), plus a 128 MiB margin
(semdiff.RenderOutsideData). On cgroup v2 a container OOM ends the executor with the render process, so with less
a build that reaches the render process's limit restarts the executor on every retry. The chart tests check these
numbers against the package's constants. A memory limit shadewright.memoryLimitBytes does not read (an exponent
or milli form) skips the check; without goMemLimit, shadewright.goMemLimit then asks for one.
*/}}
{{- define "shadewright.executorMemoryBudget" -}}
{{- $read := include "shadewright.memoryLimitBytes" (dict "component" "executor" "context" .) }}
{{- if $read }}
{{- $limit := $read | int64 }}
{{- $soft := include "shadewright.goMemLimit" (dict "component" "executor" "context" .) }}
{{- $re := "^([0-9]+)(B|KiB|MiB|GiB|TiB)?$" }}
{{- $mult := dict "" 1 "B" 1 "KiB" 1024 "MiB" 1048576 "GiB" 1073741824 "TiB" 1099511627776 }}
{{- /* Leading zeros go first, as for the memory limit: Go reads "0280MiB" as 280 MiB, the conversion as octal. */}}
{{- $softDigits := regexReplaceAll "^0+([0-9])" (regexReplaceAll $re $soft "${1}") "${1}" }}
{{- $softBytes := mul (int64 $softDigits) (int64 (index $mult (regexReplaceAll $re $soft "${2}"))) }}
{{- $need := add $softBytes 201326592 134217728 }}
{{- if lt $limit $need }}
{{- fail (printf "executor.resources.limits.memory %q (%d bytes) cannot hold the executor and its render process: it needs the executor's Go memory limit (%d bytes, executor.goMemLimit or 36 percent of the memory limit) plus 192 MiB for the render process plus a 128 MiB margin, %d bytes (ADR-0035). Raise the memory limit or lower executor.goMemLimit" (include "shadewright.memoryLimitString" (dict "component" "executor" "context" .)) $limit $softBytes $need) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The model API key file of the analyzer (ADR-0037 decision 3): the volume source, as YAML, of the one key source that
is set, or nothing. Used by the analyzer Deployment only. The Secret is never created here.
*/}}
{{- define "shadewright.anthropicKeyVolume" -}}
{{- $key := ((.Values.analyzer.analysis.anthropic | default dict).apiKey | default dict) -}}
{{- if $key.secretName -}}
secret:
  secretName: {{ $key.secretName | quote }}
  defaultMode: 0444
  items:
  - key: {{ $key.secretKey | default "api-key" | quote }}
    path: {{ $key.secretKey | default "api-key" | quote }}
{{- else if $key.volume -}}
{{- toYaml $key.volume -}}
{{- end -}}
{{- end }}

{{/* The file name inside the key volume: the Secret key, or fileName of a volume source. */}}
{{- define "shadewright.anthropicKeyFile" -}}
{{- $key := ((.Values.analyzer.analysis.anthropic | default dict).apiKey | default dict) -}}
{{- if $key.secretName -}}{{ $key.secretKey | default "api-key" }}{{- else -}}{{ $key.fileName | default "api-key" }}{{- end -}}
{{- end }}

{{/*
shadewright.keySourceID prints what a key source (an apiKey value: secretName and secretKey, or volume and
fileName) mounts, so that two declarations of one key can be told apart: the Secret and key, or the volume
source and file name. Empty when no source is set.
*/}}
{{- define "shadewright.keySourceID" -}}
{{- if .secretName -}}
{{- printf "secret %s/%s" .secretName (.secretKey | default "api-key") -}}
{{- else if and .volume (kindIs "map" .volume) (hasKey .volume "secret") (kindIs "map" .volume.secret) -}}
{{- printf "secret %s/%s" (.volume.secret.secretName | default "") (.fileName | default "api-key") -}}
{{- else if .volume -}}
{{- printf "volume %s/%s" (toJson .volume) (.fileName | default "api-key") -}}
{{- end -}}
{{- end }}

{{/* One endpoint's optional key volume. */}}
{{- define "shadewright.compatibleKeyVolume" -}}
{{- $key := .apiKey | default dict -}}
{{- if $key.secretName -}}
secret:
  secretName: {{ $key.secretName | quote }}
  defaultMode: 0444
  items:
  - key: {{ $key.secretKey | default "api-key" | quote }}
    path: {{ $key.secretKey | default "api-key" | quote }}
{{- else if $key.volume -}}
{{- toYaml $key.volume -}}
{{- end -}}
{{- end }}

{{- define "shadewright.compatibleKeyFile" -}}
{{- $key := .apiKey | default dict -}}
{{- if $key.secretName -}}{{ $key.secretKey | default "api-key" }}{{- else -}}{{ $key.fileName | default "api-key" }}{{- end -}}
{{- end }}

{{/* Match the binary's parsed base comparison; never clean a dot segment into an allowed base. */}}
{{- define "shadewright.compatibleBase" -}}
{{- $u := urlParse . -}}
{{- $host := lower ($u.hostname | default "") -}}
{{- $authority := lower ($u.host | default "") -}}
{{- $path := $u.path | default "" -}}
{{- $port := trimPrefix $host $authority -}}
{{- if or (gt (len .) 256) (ne (lower ($u.scheme | default "")) "https") $u.userinfo (contains "@" .) (contains "?" .) (contains "#" .) (contains "%2f" (lower .)) (contains "%5c" (lower .)) (not (regexMatch "^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$" $host)) (not (regexMatch "^(/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)*/?$" $path)) -}}
{{- fail "openAICompatible endpoint must be an unambiguous HTTPS base without userinfo, query, fragment or dot segments" -}}
{{- end -}}
{{- if $port -}}
{{- if or (not (regexMatch "^:[1-9][0-9]{0,4}$" $port)) (gt (int (trimPrefix ":" $port)) 65535) -}}{{- fail "openAICompatible endpoint port is invalid" -}}{{- end -}}
{{- end -}}
{{- if eq $port ":443" -}}{{- $port = "" -}}{{- end -}}
{{- printf "https://%s%s%s" $host $port (trimSuffix "/" $path) -}}
{{- end }}

{{/* Only declared data reaches the process, with one mounted key path per endpoint. */}}
{{- define "shadewright.compatibleEndpoints" -}}
{{- $out := list -}}
{{- range $i, $e := . -}}
{{- $d := dict "url" $e.url "inferenceAPI" $e.inferenceAPI "models" $e.models -}}
{{- if include "shadewright.compatibleKeyVolume" $e -}}
{{- $_ := set $d "keyFile" (printf "/var/run/secrets/shadewright/openai-compatible/%d/%s" $i (include "shadewright.compatibleKeyFile" $e)) -}}
{{- end -}}
{{- with $e.service -}}{{- $_ := set $d "service" . -}}{{- end -}}
{{- $out = append $out $d -}}
{{- end -}}
{{- toJson $out -}}
{{- end }}

{{/*
shadewright.pow2 prints 2 to the power of the argument (0 to 32).
*/}}
{{- define "shadewright.pow2" -}}
{{- $p := 1 -}}
{{- range until (int .) -}}{{- $p = mul $p 2 -}}{{- end -}}
{{- $p -}}
{{- end -}}

{{/*
shadewright.ipv6Text prints eight 16-bit groups in the canonical text form Go's netip prints (RFC 5952):
lower case, no leading zeros, the longest run of two or more zero groups (the first of equal runs) as ::.
*/}}
{{- define "shadewright.ipv6Text" -}}
{{- $groups := . -}}
{{- $best := -1 -}}{{- $bestLen := 1 -}}{{- $start := 0 -}}{{- $run := 0 -}}
{{- range $i, $g := $groups -}}
{{- if eq (int64 $g) 0 -}}
{{- if eq $run 0 -}}{{- $start = $i -}}{{- end -}}
{{- $run = add1 $run -}}
{{- if gt $run $bestLen -}}{{- $bestLen = $run -}}{{- $best = $start -}}{{- end -}}
{{- else -}}
{{- $run = 0 -}}
{{- end -}}
{{- end -}}
{{- $hex := list -}}
{{- range $groups -}}{{- $hex = append $hex (printf "%x" (int64 .)) -}}{{- end -}}
{{- if lt $best 0 -}}
{{- join ":" $hex -}}
{{- else -}}
{{- printf "%s::%s" (join ":" (slice $hex 0 $best)) (join ":" (slice $hex (add $best $bestLen) 8)) -}}
{{- end -}}
{{- end -}}

{{/*
shadewright.prefixesOverlap prints "true" when the IPv6 prefixes a and b (eight groups each) with lengths
aBits and bBits share an address: their first min(aBits, bBits) bits are equal.
*/}}
{{- define "shadewright.prefixesOverlap" -}}
{{- $m := min .aBits .bBits -}}
{{- $equal := true -}}
{{- range $i := until 8 -}}
{{- $rem := sub $m (mul $i 16) -}}
{{- $x := int64 (index $.a $i) -}}{{- $y := int64 (index $.b $i) -}}
{{- if ge $rem 16 -}}
{{- if ne $x $y -}}{{- $equal = false -}}{{- end -}}
{{- else if gt $rem 0 -}}
{{- $unit := int64 (include "shadewright.pow2" (sub 16 $rem)) -}}
{{- if ne (div $x $unit) (div $y $unit) -}}{{- $equal = false -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- if $equal -}}true{{- end -}}
{{- end -}}

{{/*
shadewright.inClusterCIDRProblem prints why one entry of analyzer.analysis.openAICompatible.inClusterCIDRs is
refused, or nothing. The rule is the analyzer's (openaicompatible.CheckInClusterRange), and a chart test runs
the same inputs through both: a CIDR in canonical text form, no host bits, not a /0, and no IPv6 range that
overlaps a form embedding IPv4 addresses (::ffff:0:0/96, ::/96, 64:ff9b::/96, 2002::/16).
*/}}
{{- define "shadewright.inClusterCIDRProblem" -}}
{{- $notCIDR := "is not a CIDR such as 203.0.113.0/24 or 2001:db8::/64" -}}
{{- if not (kindIs "string" .) -}}
{{- $notCIDR -}}
{{- else if not (regexMatch "^[0-9A-Fa-f:.]+/(0|[1-9][0-9]{0,2})$" .) -}}
{{- $notCIDR -}}
{{- else -}}
{{- $addr := regexReplaceAll "/.*$" . "" -}}
{{- $bits := int (regexReplaceAll "^.*/" . "") -}}
{{- if not (contains ":" $addr) -}}
  {{- if or (not (regexMatch "^(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}$" $addr)) (gt $bits 32) -}}
  {{- $notCIDR -}}
  {{- else if eq $bits 0 -}}
  covers every address: no endpoint outside a declared Service could be reached, and the egress rule cannot except it; list the cluster's pod and Service ranges
  {{- else -}}
  {{- $v := int64 0 -}}
  {{- range splitList "." $addr -}}{{- $v = add (mul $v 256) (int64 .) -}}{{- end -}}
  {{- $unit := int64 (include "shadewright.pow2" (sub 32 $bits)) -}}
  {{- $host := mod $v $unit -}}
  {{- if ne $host 0 -}}
  {{- $n := sub $v $host -}}
  {{- printf "has host bits set: write %d.%d.%d.%d/%d" (div $n 16777216) (mod (div $n 65536) 256) (mod (div $n 256) 256) (mod $n 256) $bits -}}
  {{- end -}}
  {{- end -}}
{{- else if or (contains "." $addr) (contains ":::" $addr) (gt (len (regexFindAll "::" $addr -1)) 1) (gt $bits 128) -}}
  {{- $notCIDR -}}
{{- else -}}
  {{- $digits := dict "0" 0 "1" 1 "2" 2 "3" 3 "4" 4 "5" 5 "6" 6 "7" 7 "8" 8 "9" 9 "a" 10 "b" 11 "c" 12 "d" 13 "e" 14 "f" 15 -}}
  {{- $halves := list $addr -}}
  {{- if contains "::" $addr -}}{{- $halves = splitList "::" $addr -}}{{- end -}}
  {{- $parsed := list -}}
  {{- $bad := false -}}
  {{- range $halves -}}
  {{- $groups := list -}}
  {{- if ne . "" -}}
  {{- range splitList ":" . -}}
  {{- if not (regexMatch "^[0-9A-Fa-f]{1,4}$" .) -}}{{- $bad = true -}}{{- else -}}
  {{- $g := 0 -}}
  {{- range splitList "" (lower .) -}}{{- $g = add (mul $g 16) (get $digits .) -}}{{- end -}}
  {{- $groups = append $groups $g -}}
  {{- end -}}
  {{- end -}}
  {{- end -}}
  {{- $parsed = append $parsed $groups -}}
  {{- end -}}
  {{- $groups := list -}}
  {{- if eq (len $parsed) 1 -}}
  {{- $groups = index $parsed 0 -}}
  {{- if ne (len $groups) 8 -}}{{- $bad = true -}}{{- end -}}
  {{- else -}}
  {{- $zeros := sub 8 (add (len (index $parsed 0)) (len (index $parsed 1))) -}}
  {{- if lt $zeros 1 -}}{{- $bad = true -}}{{- else -}}
  {{- $groups = concat (index $parsed 0) (list) -}}
  {{- range until (int $zeros) -}}{{- $groups = append $groups 0 -}}{{- end -}}
  {{- $groups = concat $groups (index $parsed 1) -}}
  {{- end -}}
  {{- end -}}
  {{- if $bad -}}
  {{- $notCIDR -}}
  {{- else if eq $bits 0 -}}
  covers every address: no endpoint outside a declared Service could be reached, and the egress rule cannot except it; list the cluster's pod and Service ranges
  {{- else -}}
  {{- $overlap := "" -}}
  {{- range list (list "::ffff:0:0/96" (list 0 0 0 0 0 65535 0 0) 96) (list "::/96" (list 0 0 0 0 0 0 0 0) 96) (list "64:ff9b::/96" (list 100 65435 0 0 0 0 0 0) 96) (list "2002::/16" (list 8194 0 0 0 0 0 0 0) 16) -}}
  {{- if and (not $overlap) (include "shadewright.prefixesOverlap" (dict "a" $groups "aBits" $bits "b" (index . 1) "bBits" (index . 2))) -}}{{- $overlap = index . 0 -}}{{- end -}}
  {{- end -}}
  {{- if $overlap -}}
  {{- printf "overlaps %s, a form that embeds IPv4 addresses: write the IPv4 range, which the dial guard also applies to that form" $overlap -}}
  {{- else -}}
  {{- $masked := list -}}
  {{- range $i, $g := $groups -}}
  {{- $rem := sub $bits (mul $i 16) -}}
  {{- if le $rem 0 -}}{{- $masked = append $masked 0 -}}
  {{- else if lt $rem 16 -}}{{- $masked = append $masked (sub $g (mod $g (int64 (include "shadewright.pow2" (sub 16 $rem))))) -}}
  {{- else -}}{{- $masked = append $masked $g -}}{{- end -}}
  {{- end -}}
  {{- $text := printf "%s/%d" (include "shadewright.ipv6Text" $masked) $bits -}}
  {{- if ne (toJson $masked) (toJson $groups) -}}
  {{- printf "has host bits set: write %s" $text -}}
  {{- else if ne $text . -}}
  {{- printf "is not in canonical form: write %s" $text -}}
  {{- end -}}
  {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
