---
title: Configuration reference
---

# Configuration reference

Everything an operator can set today, checked against `cmd/main.go` (the executor), `cmd/analyzer/main.go`, `internal/cmdutil` and `config/` on `main`. The Helm chart in `dist/chart` maps its values onto the settings below; see [Helm chart](#helm-chart). Objects and fields of the custom resources are in the [API reference](api.md). See also [compatibility](compatibility.md), [upgrade and uninstall](../upgrade-uninstall.md), [identity rotation](../identity-rotation.md) and [data handling](../data-handling.md). Metric names are in the [metrics reference](metrics.md).

## Components and images

| Component | Binary | Image | Role |
| --- | --- | --- | --- |
| Executor | `/manager` | `ghcr.io/pkalin/shadewright-executor` | Reconciles `RemediationRequest` objects in the operator namespace, decides with the policy, acts through Git |
| Analyzer | `/analyzer` | `ghcr.io/pkalin/shadewright-analyzer` | Collector, Incident controller, platform and module detection. Creates Incidents and, once the analysis is built, requests. Never acts on them |
| Verifier | `/verifier` | `ghcr.io/pkalin/shadewright-verifier` | Samples the pods of a rollout for each request in `Verifying` and writes its `VerificationReport` (ADR-0030 section 7). No model, no Git App, no Secrets; never writes request status. Requests reach `Verifying` only with the executor flag `--verify-outcomes` (off by default), so on a default install it runs and writes no reports. The executor reads the report as evidence for `Cleared` (#683) |

All three images are distroless, run as user 65532, and are published for linux/amd64 and linux/arm64. The manifests in `config/manager` use the placeholder `controller:latest` for all three, so every install replaces it (the published chart runs the release's signed images by digest; a chart from a source checkout runs the `appVersion` tag). Pin by digest; the [getting started](../install.md#install-with-kubectl-and-kustomize) page has the digests of the latest release and an overlay that does it. All three Deployments run one replica with `--leader-elect`, restricted Pod Security settings (non-root, `RuntimeDefault` seccomp, read-only root filesystem, all capabilities dropped), requests of 10m CPU and 64Mi, and limits of 500m CPU and 128Mi (the executor's memory limit is 640Mi since #614: its container holds the executor and the render process of the render check, ADR-0035; it was 256Mi from #615).

## Helm chart

The chart is `dist/chart`. It was generated with Kubebuilder's `helm/v2-alpha` plugin (Kubebuilder v4.16.0) and then adjusted by hand: the plugin renders one manager, so the three Deployments, their ServiceAccounts and the RBAC are written out. **Do not regenerate it with the plugin**; `make test-chart` renders it and fails if its RBAC, NetworkPolicies, Services or CRDs differ from `kustomize build config/default` plus `config/watch` (any extra or missing verb, resource or subject), so a change to `config/rbac` needs the same change in `dist/chart/templates/rbac`. The Chart workflow runs these checks on every pull request, but they are not required for merge yet (the ruleset requires other checks): a red chart check does not block a merge, so read it. It is published as `oci://ghcr.io/pkalin/charts/shadewright` by the release tag workflow, pre-release tags included; the first published version is `0.2.0-rc.1`, and v0.1.0 has no chart. A pre-release needs an explicit `--version`. From a source checkout, install `./dist/chart` (see [getting started](../install.md#install-with-helm-instead)).

```bash
helm install shadewright oci://ghcr.io/pkalin/charts/shadewright --version <version> \
  --namespace shadewright-system --create-namespace -f my-values.yaml
```

Use the release name `shadewright` and the namespace `shadewright-system`: the names of the ServiceAccounts, and so the subject of the federated credential, come from the chart's full name: the release name when it contains the chart name `shadewright` (so `shadewright-executor` and `shadewright-analyzer` for the release `shadewright`), otherwise `<release>-shadewright`, then `-executor` or `-analyzer` (cut to 63 characters in all; `fullnameOverride` replaces the prefix). Check with `helm template` or `kubectl -n <namespace> get serviceaccounts` before you create the federated credential. The Leases and the analysis budget ConfigMap keep the fixed names the binaries expect whatever the release is called. `values.schema.json` validates the values on install, upgrade, lint and template.

The published chart installs with no values at all as an idle Shadewright: the analyzer watches no namespace (no RoleBinding outside the operator namespace), so it reads no namespaced object outside the operator namespace, has no Git identity, and writes only in the operator namespace (its Leases, the budget ConfigMap, Events) plus the settings status once a settings object exists; it still lists Nodes and reads discovery and the settings cluster-wide. The executor has no Azure workload identity, so it cannot reach Key Vault to sign for the GitHub App. The cluster ID is the UID of `kube-system`. The published chart runs the release's signed images by digest (the publish job writes them into its `values.yaml`). A chart from a source checkout has no digests and refuses to render until you set `executor.image.digest` and `analyzer.image.digest`, or opt in with `image.allowTag: true` to run the `appVersion` tag, which needs images built from that same checkout: the v0.1.0 images stop at start with an empty watch list. `helm test` checks that the metrics Service of each component answers and refuses a scrape without a token. A `my-values.yaml` that puts it to work:

```yaml
clusterId: my-cluster    # optional; required when the chart is rendered with helm template
executor:
  identity:
    azure:
      clientId: <client ID of the executor managed identity>
analyzer:
  watchNamespaces: [<namespace to watch>]
  podLogs:
    enabled: true   # false: --pod-logs=false and no pods/log rule
  modules:         # read grants of the add-ons' kinds; spec.modules still decides what runs
    keda:
      enabled: true   # false: no keda.sh role (KEDA not installed)
    flux:
      enabled: false  # true: list, watch Flux kinds in the watched namespaces
    kyverno:
      enabled: false  # true: Kyverno policies and policy reports in the watched namespaces
      clusterWide:
        enabled: false  # true: ClusterPolicies and ClusterPolicyReports, cluster-wide
  prometheus:
    url: ""         # --prometheus-url, when set
    cluster: ""     # --prometheus-cluster, when set
  analysis:         # off unless triageModel is set; an evaluation run in the lab only until #78
    triageModel: "" # --triage-model; needs a provider allow-list or the Anthropic API key
```

Each component also takes the standard Pod values, bounded by the schema: `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName`, `imagePullSecrets` (added to the global list) and `podAnnotations`. Every object carries the `app.kubernetes.io/*` labels and `helm.sh/chart`. The chart passes `ct lint` and `ct install` (chart-testing, with the default values on Kind) and `helm lint --strict` in CI.

Every value, with its default, its allowed values and its security effect, is in the [chart README](https://github.com/pkalin/shadewright/blob/main/dist/chart/README.md). It is generated from the comments in `dist/chart/values.yaml` with helm-docs (`make chart-docs`), and CI fails if the two differ or a value has no description, so there is one source. `values.schema.json` is the authority on which values are required.

The chart is signed with cosign keyless (GitHub OIDC) by digest in the publish job, like the images, and has a GitHub build provenance attestation. Verify it before installing; the identity is the chart workflow on a release tag of this repository:

```bash
cosign verify \
  --certificate-identity-regexp '^https://github\.com/pkalin/shadewright/\.github/workflows/chart\.yml@refs/tags/v[0-9].*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/pkalin/charts/shadewright@sha256:<chart digest>

# Build provenance: built by this repository's chart workflow.
gh attestation verify oci://ghcr.io/pkalin/charts/shadewright@sha256:<chart digest> \
  --repo pkalin/shadewright \
  --signer-workflow pkalin/shadewright/.github/workflows/chart.yml
```

The digest is in the run summary of the Chart workflow, or from `helm pull oci://ghcr.io/pkalin/charts/shadewright --version <version>` (it prints `Digest:`). Image signatures are verified the same way, with the image name and the `images\.yml` workflow in the identity. Releases after v0.1.0 list the chart and image digests on their GitHub release, with these commands filled in. Install the chart you verified by digest, `helm install shadewright oci://ghcr.io/pkalin/charts/shadewright@sha256:<chart digest>` (Helm accepts a digest in an OCI chart reference: `ValidateReference` in `pkg/registry` and the "oci ref with sha256" cases of `pkg/downloader/chart_downloader_test.go`, present in Helm 3.17 and in 4.3, the version CI pins), so the chart cannot change between the check and the install. A pre-release (`v0.2.0-rc.1`) publishes the chart as version `0.2.0-rc.1` (also its `appVersion`), signed by the same workflow; installed by digest it needs no `--version`, otherwise pass `--version 0.2.0-rc.1`, as Helm does not pick pre-releases by itself.

The optional [TLS trust profile](#tls-trust-profile-helm-chart-optional) (`<component>.metricsTLS.secretName`, `metrics.serviceMonitor.tls`) mounts an administrator-issued serving certificate and makes the ServiceMonitors verify it. `metrics.serviceMonitor.enabled` (off by default, as `config/default` leaves `config/prometheus` out) installs a Prometheus Operator `ServiceMonitor` per component; Prometheus then needs the `metrics-reader` ClusterRole for its ServiceAccount and the label `metrics: enabled` on its namespace. Not in the chart: cert-manager and webhooks (none are installed), and the `Namespace` (use `--create-namespace`). The security contexts, probes and ports are fixed to what `config/` sets. The replica count is 1 unless you raise `executor.replicas` or `analyzer.replicas` for a standby ([below](#standby-replicas-helm-chart-optional)).

### Go memory limit (Helm chart)

The chart sets `GOMEMLIMIT` on the analyzer and the verifier to 90 percent of the container's `resources.limits.memory`, in bytes (their default 128Mi gives 120795955), and on the executor to 36 percent (its default 640Mi gives 241591910, about 230 MiB; the rest is for its render process, which the executor stops at 192 MiB resident, and a 128 MiB margin, ADR-0035), so the Go runtime collects before the heap reaches the limit instead of being OOM-killed during a large but bounded parse ([#601](https://github.com/pkalin/shadewright/issues/601)). It follows the limit when you change it. `executor.goMemLimit`, `analyzer.goMemLimit` and `verifier.goMemLimit` override it with a Go memory limit such as `100MiB`. The chart reads a memory limit written as a number of bytes or as a number with an optional fraction and a `Ki`, `Mi`, `Gi`, `Ti`, `k`, `K`, `M`, `G` or `T` suffix (such as `1.5Gi` or `600M`), rounds a fraction of a byte up as Kubernetes does, and reads a limit of 1 Ei or more as 1 Ei; another valid form (an exponent such as `1e9`, or milli) makes the render fail until you set the override. The chart also refuses to render an executor whose memory limit is below its `GOMEMLIMIT` (the default or `executor.goMemLimit`) plus 192 MiB for the render process plus a 128 MiB margin, so `256Mi` is refused (ADR-0035); for a limit in a form it does not read, it skips that check. The manifests in `config/manager` set the executor's `GOMEMLIMIT` to the same 241591910 bytes, with its 640Mi limit, and none on the analyzer.

### Reviewer hints (Helm chart, optional)

`executor.reviewerHints.enabled` (default `false`) passes `--reviewer-hints` to the executor, which then reads `/shadewright reanalyze:` comments on the pull requests it opened and creates the check run `shadewright/review` on their heads ([ADR-0028](../adr/0028-reviewer-hints-through-the-pull-request.md)). Turn it on only after the target branch of the GitOps repository requires the check `shadewright/review` with the Shadewright App as its expected source (ADR-0028 section 5): Shadewright cannot verify that rule, and without it the check only informs. The chart refuses `--reviewer-hints` in `executor.extraArgs`. Steps: [turn reviewer hints on](../how-to/turn-on-reviewer-hints.md).

### Model API key (Helm chart, optional)

A model deployment with `provider: Anthropic` ([ADR-0037](../adr/0037-the-anthropic-api-as-a-second-model-provider.md)) needs an API key, the one long-lived credential of the analyzer. The analyzer reads it from a file given by `--anthropic-api-key-file`. The chart renders that flag, and a read-only volume mounted whole (no `subPath`) at `/var/run/secrets/shadewright/anthropic`, in the analyzer container only, from one of two sources under `analyzer.analysis.anthropic.apiKey`:

| Value | Default | Meaning |
| --- | --- | --- |
| `secretName` | empty | An existing Secret in the release namespace. The file is its key `secretKey`. |
| `secretKey` | `api-key` | The key in that Secret, also the file name. |
| `volume` | empty | A volume source object instead of `secretName`, for example a `csi` block for the Secrets Store CSI driver. Setting both fails the render. |
| `fileName` | `api-key` | The file inside `volume` that holds the key. |

With neither source set, nothing is mounted and no flag is passed. The chart never creates the Secret and the analyzer gets no RBAC on Secrets: the kubelet or the CSI driver mounts the file. The render also fails when `settings.spec` names an `Anthropic` deployment and no source is set. That check is a convenience at install only: `RemediationSettings` is edited live afterwards, and the analyzer itself refuses such a deployment when it has no key file.

Operator guidance (ADR-0037 decision 3):

- Create the key in a dedicated vendor workspace used only for Shadewright, with a spend limit set in the vendor console, so a leaked key has a bounded cost.
- Rotate by updating the Secret or the CSI source. The adapter reads the file on every call, so no restart is needed (a mounted Secret reaches the Pod after the kubelet sync period, so a rotation can take a minute or two).
- Whoever can `get` that Secret, or read the volume's source (the Key Vault, for a CSI volume), can read the key. Whoever can edit `RemediationSettings` can move triage or escalation to this provider, which sends the redacted context to the vendor ([data handling](../data-handling.md)).

### Standby replicas (Helm chart, optional)

`executor.replicas` and `analyzer.replicas` (default 1, at least 1) add warm standbys. Leader election is always on, so one Pod per component holds its Lease and works while the others wait; when the leader's Pod fails, a standby takes over after about one Lease duration (15 seconds by default), not after a Pod start. The cost is an idle Pod per identity. A standby adds no throughput, because only the leader reconciles (horizontal scaling is [#559](https://github.com/pkalin/shadewright/issues/559)). Above 1 the chart renders a PodDisruptionBudget with `minAvailable: 1` and a preferred Pod anti-affinity by node (`kubernetes.io/hostname`) for that component; with 1 replica it renders neither. An `affinity` value of your own with a `podAntiAffinity` replaces the chart's term. No RBAC changes: each component already holds the grant for its own Lease. A standby is Ready once its informers have synced; readiness does not wait for the Lease ([health probes](../operate.md#health-probes)). The kustomize manifests keep one replica.

## Flags

Flags are passed as container args. A flag given twice takes its last value. The shipped manifests set the ones marked in the last column.

### All binaries

| Flag | Default | Meaning | `config/` sets |
| --- | --- | --- | --- |
| `--namespace` | `$POD_NAMESPACE` | The operator namespace. Required: the binary exits if both are empty | `POD_NAMESPACE` from the downward API |
| `--leader-elect` | `false` | Only one replica is active. Leases: `shadewright-executor-leader-election`, `shadewright-analyzer-leader-election`, `shadewright-verifier-leader-election` | on |
| `--health-probe-bind-address` | `:8081` | Serves `/healthz` and `/readyz` | `:8081` |
| `--metrics-bind-address` | `0` (off) | Address of the metrics endpoint, `:8443` for HTTPS | `:8443` (patch in `config/default`) |
| `--metrics-secure` | `true` | Serve metrics over HTTPS with authentication and authorization. `--metrics-secure=false` serves plain HTTP with no authentication | default |
| `--metrics-cert-path`, `--metrics-cert-name`, `--metrics-cert-key` | empty, `tls.crt`, `tls.key` | Directory and file names of the metrics certificate. Without a certificate a self-signed one is generated | unset |
| `--enable-http2` | `false` | HTTP/2 on the metrics server. Off to avoid the stream-cancellation vulnerabilities | default |
| `--kubeconfig` | none | Only for running outside a cluster (`make run`) | unset |
| `--zap-devel` | `true` | Development logging: console encoder, debug level. Pass `--zap-devel=false` for JSON at info level | default |
| `--zap-encoder`, `--zap-log-level`, `--zap-stacktrace-level`, `--zap-time-encoding` | zap defaults | Standard zap options | unset |

### Executor only

| Flag | Default | Meaning |
| --- | --- | --- |
| `--webhook-port` | `9443` (`-1` disables) | Webhook server port |
| `--webhook-cert-path`, `--webhook-cert-name`, `--webhook-cert-key` | empty, `tls.crt`, `tls.key` | Webhook certificate |
| `--pull-request-poll-interval` | `1m` | How often the executor looks at a pull request that waits for review (`PRCreated`, `AwaitingApproval`), so a merge or a close is seen within this interval plus one reconcile. A look lists the pull requests of the request's branch (one GitHub REST call); the full recheck of the plan stays every 5 min. From `30s` to `5m` (`0` uses the default); a value outside refuses to start. At the default a waiting pull request costs about 150 GitHub REST calls an hour (48 looks of one call and 12 full rechecks of about 8 calls; about 100 without the looks), plus one look after each status write of the executor on the request, which happens only on a change (the move to `AwaitingApproval`, a new render, a refusal, a pause and its end). With `maxConcurrentRemediations` at its maximum of 20 that is about 3,000 an hour. The worst case is the minimum `30s` with 20 waiting pull requests: about 200 calls an hour each (108 looks and 12 full rechecks), about 4,000 in all. A full recheck after a merge to the target branch also maps the source again at the new commit (its read cache is keyed by commit), up to 512 file reads once per commit. The installation's limit of at least 5,000 calls an hour is shared with the policy check's source mapping and with the writes (branch, commit, pull request), so keep the default unless merges must be seen sooner. Set it in the chart with `executor.extraArgs` |
| `--node-grace` | `1m` | How long past the stable window a verification verdict waits, so the pods of a node that stopped reporting are marked not Ready first (ADR-0030 section 4). Set it to at least the cluster's node monitor grace period (`--node-monitor-grace-period` of kube-controller-manager). It is part of the timeout of a request in `Verifying` (the larger of `spec.verification.timeout` and the rollout deadline plus the effective `stableFor` plus this): 21 minutes for a memory fix with the defaults. `0` uses the default; a negative value refuses to start. Set it in the chart with `executor.verification.nodeGrace` |
| `--verify-outcomes` | `false` | Move merged requests through `WaitingForDelivery` and `Verifying` to `Succeeded`, `Failed` or `Inconclusive` (ADR-0030), with `Cleared` judged from the verifier's `VerificationReport`. Off by default until a lab run with a release candidate decides the default. On with the verifier deployed, a request can end `Succeeded`; on without a verifier, no report is written, every merge ends `Inconclusive` (`StaleReport`) or `Failed`, and the circuit breaker does not close again once it tripped. Off, a merged request keeps `Merged` with its rollout report (`RolledOut`), and a request found in `WaitingForDelivery` or `Verifying` ends `Inconclusive` with reason `VerificationDisabled`. On, the circuit breaker reads a `Merged` request with a merge receipt as undecided until it ends; off, as a success. Logged once at start (`Configured outcome verification`). Set it in the chart with `executor.verification.enabled` |
| `--reviewer-hints` | `false` | Read reviewer hints from the comments of the pull requests the executor opened, and create the check run `shadewright/review` on their heads (ADR-0028). Turn it on only when the target branch requires that check from the Shadewright App ([turn reviewer hints on](../how-to/turn-on-reviewer-hints.md)). Off fails closed: hints stored while it was on are declined (`HintsTurnedOff`) and no replacement takes a pull request over. Logged once at start (`Configured reviewer hints`). Set it in the chart with `executor.reviewerHints.enabled` |

`config/default` installs no admission webhook (the webhook and cert-manager sections are commented out), so the webhook flags have no use in the shipped manifests.

### Verifier only

| Flag | Default | Meaning | `config/` sets |
| --- | --- | --- | --- |
| `--watch-namespaces` | required | Comma-separated namespaces whose pods the verifier may list; the verifier does not start with an empty list. A request whose target is in another namespace gets no report. Each needs its own RoleBinding of `verifier-watch-role` (chart: `verifier.watchNamespaces`, `null` by default, which uses `analyzer.watchNamespaces`; a list, `[]` included, is used as it is; unique names, at most 256, `*` refused; with an empty list the chart deploys no verifier) | `demo` (the lab) |

### OpenAI-compatible endpoints

`provider: OpenAICompatible` calls an operator-allowed HTTPS endpoint, including
`https://api.openai.com/v1`, a gateway API base or an explicitly named cluster
Service. `deployment` is the model name. `inferenceAPI` is `ChatCompletions` (the
default) or `Responses`. Leave `apiVersion` out: Azure version fields are refused.
The adapter appends `/chat/completions` or `/responses` to the base path. It shares
the Foundry wire parsers and requests strict structured output. Server replies
still pass typed result and tool argument validation; invalid replies are never
repaired into proposals. Qualify each server and model with the
[evaluation harness](../evaluation.md#openai-compatible-models-and-servers).

| Install setting | Meaning |
| --- | --- |
| `analyzer.analysis.endpointSuffixes` / `--model-endpoint-suffixes` | Foundry-only destination allow-list, unchanged for existing installs. No default |
| `analyzer.analysis.openAICompatible.endpoints` / `--openai-compatible-endpoints` | Exact endpoint declarations. The flag is a JSON array with `url`, `inferenceAPI`, a nonempty `models` list (`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`), optional `keyFile` and optional `service`. Settings must match a parsed base, API and declared model name byte for byte (`EndpointNotAllowed` or `ModelNotAllowed` otherwise); duplicate bases and segment path prefixes on the same authority are refused; request paths come only from the declaration |
| `analyzer.analysis.openAICompatible.inClusterCIDRs` / `--openai-compatible-in-cluster-cidrs` | Default empty. Up to 32 IPv4 or IPv6 CIDRs that count as in-cluster besides the private, shared (100.64.0.0/10) and link-local ranges: the cluster's pod and Service ranges when they are routable addresses. A compatible endpoint that is not a declared `service` gets no connection to an address in them, checked at dial time for every resolved address before any byte is sent. The IPv4 ranges are added to the except list of the egress rule for declared ports other than 443. A malformed CIDR, one with host bits set, or one containing an endpoint's literal address stops the analyzer at start; the chart refuses malformed entries and the value without `endpoints` at render. The flag is comma-separated |
| `analyzer.analysis.disableFoundry` / `--disable-foundry` | Default false. True disables Foundry and its inference token source. Blob records still need Azure identity |
| `endpoints[].apiKey.secretName`, `secretKey` | Existing Secret and its key (default `api-key`), mounted whole and read-only only in the analyzer. The chart creates no Secret or RBAC |
| `endpoints[].apiKey.volume`, `fileName` | Alternative Secret or CSI volume and file name (default `api-key`). Do not combine with `secretName` |
| `endpoints[].keyFile` (flag only) | Absolute clean file path, read per attempt. The chart derives `/var/run/secrets/shadewright/openai-compatible/<index>/<fileName>` from each endpoint's key source. No source means no Authorization header. Unavailable/empty files are retryable; malformed files, including nonempty keys shorter than 16 bytes, are refused without retry. Fixed errors name the minimum and never quote paths or keys |
| `endpoints[].service` | Exact Service URL's namespace, a `podSelector` of 1 to 32 labels with valid label keys and values (the chart refuses others at render, the analyzer at start) and pod target `port`. Grants private unicast dialing and one selector-based egress rule. Verified TLS, trusted certificates and address guards remain required |

Duplicate parsed bases, Azure/Anthropic hosts, Foundry-listed hosts and ambiguous
URLs fail chart rendering and analyzer startup. Scheme/host case, port 443 and one
trailing slash normalize; dot segments, userinfo, queries and fragments are refused.

The key is separate from the Anthropic key. It is never sent to Foundry or
Anthropic hosts, and no Entra token reaches this adapter. The client uses no
proxy and follows no redirects, including to another allowed host. Missing or
incomplete usage is refused with `UsageUnknown` and charged at the input estimate
plus the output limit with a rounded-up 10% margin. Complete reports cost at least
the input estimate. Above-ceiling usage is refused and charged that ceiling.
Each endpoint owns its optional key, including separate account paths on one host. A compatible server's 429 is never assumed unbilled.

The dial guard treats the private, shared and link-local ranges as in-cluster. If
the cluster's pods or Services use other addresses, list them in
`inClusterCIDRs`: otherwise a declared public name that resolves into them would
reach a pod or Service with the key. To find a cluster's ranges: the pod ranges are in
`kubectl get nodes -o jsonpath='{.items[*].spec.podCIDRs}'` (empty with some CNIs,
which keep them in their own configuration, such as the Cilium `IPPool` or the
Calico `IPPool` objects); the Service range is the API server's
`--service-cluster-ip-range`, `kubectl get servicecidrs` on clusters that serve
that API, or the cluster's settings at your cloud (for AKS,
`az aks show --query networkProfile`).
See [ADR-0042](../adr/0042-openai-compatible-model-endpoints.md), and the [installation runbook](../how-to/qualify-openai-compatible.md).

## Analyzer only

| Flag | Default | Meaning | `config/` sets |
| --- | --- | --- | --- |
| `--watch-namespaces` | empty (no namespace) | Comma-separated namespaces whose events, pods and workloads are read. Each must be a valid namespace name (`*` is refused); spaces and duplicates are dropped. An empty list watches no namespace, never "all namespaces": the analyzer starts and is Ready, starts no collector, records no Incident, logs `Watching no namespaces` and reports the condition `NoWatchedNamespaces` True in the settings status | `demo` (the lab) |
| `--cluster-id` | none, **required** | See [cluster ID](#cluster-id) | `lab` (the lab) |
| `--max-incidents` | `500` | Most Incidents kept in the operator namespace; at least 1. At the cap a new fingerprint is not stored, is counted in `shadewright_incident_observations_refused_total` and reported in a Warning event (at most one per 10 minutes) | unset |
| `--triage-model` | empty (analysis off) | The model ID and version the triage deployment of `spec.analysis.models` serves, as its replies name it, such as `gpt-5.4-mini-2026-03-17`: 1 to 128 letters, digits, `.`, `_`, `:`, `/` or `-`. Empty: the analysis loop does not start, no model is called and no Azure token is requested. Set: it needs `--model-endpoint-suffixes`, `--openai-compatible-endpoints` or `--anthropic-api-key-file`, and with `--model-endpoint-suffixes` the analyzer reads its [workload identity](#workload-identity-azure) at start and refuses to start without it. It refuses a reply that names another model (`UnexpectedModel`, [ADR-0023](../adr/0023-how-the-analyzer-calls-the-model.md)). Until the evaluation gate ([#78](https://github.com/pkalin/shadewright/issues/78)) exists, set it only for an evaluation run in the lab ([lab runbook](../lab-oom-acceptance.md#evaluation-run-the-first-analysis-with-a-real-model)) | unset |
| `--escalation-model` | empty | The same for the escalation deployment. Empty: escalation is off whatever the settings name, and an escalation signal hands off (`EscalationNotConfigured`). Needs `--triage-model` | unset |
| `--model-endpoint-suffixes` | empty | Required with `--triage-model` for a `Foundry` deployment. The list never allows a compatible endpoint. With `--disable-foundry`, no inference token source is built. Empty: the analyzer reads no workload identity, builds no Azure token source and refuses every `Foundry` deployment (`ProviderNotConfigured`, the log line names `--model-endpoint-suffixes`), another provider's configuration is then required. Comma-separated host suffixes a model endpoint in the settings must end with or, without the leading dot, equal, each a dot and two or more lower-case DNS labels. An endpoint outside them, or on a port other than 443, is refused before a token is requested (`EndpointNotAllowed`). Keep it as narrow as your deployment allows: any resource under an allowed suffix can receive redacted context (and a Cognitive Services token on the Foundry path), so name your resource, such as `.<resource>.services.ai.azure.com`, which allows that host only. Used with `--triage-model`. A Foundry deployment at `api.anthropic.com` is refused whatever the list says, and the list is not consulted for a deployment with `provider: Anthropic` ([ADR-0037](../adr/0037-the-anthropic-api-as-a-second-model-provider.md)) | unset |
| `--anthropic-api-key-file` | empty (no Anthropic deployment) | Absolute path of a file that holds the Anthropic API key, for deployments with `provider: Anthropic` ([ADR-0037](../adr/0037-the-anthropic-api-as-a-second-model-provider.md)). Mount it read-only from a Secret or a CSI volume (for example the Secrets Store CSI driver with Azure Key Vault), the whole volume and no `subPath`, which never refreshes; the analyzer gets no RBAC on Secrets. It is read at every call, so a rotated key needs no restart, and not at start; a missing, empty or unreadable file is a retryable model error, then a hand-off with `ProviderNotConfigured` when no attempt of the call sent a request; a malformed file (larger than 4 KiB, or a key longer than 512 bytes or with a character a header cannot carry, such as an inner newline or a UTF-8 byte order mark) is not retried and ends the same way. Each failed call is logged with the adapter's fixed error text, never the key or the path, and a call that sent nothing is charged no tokens. Empty: every `Anthropic` deployment is refused before any call (`ProviderNotConfigured`). With it, an `Anthropic` deployment must name the model ID of its role's `--triage-model` or `--escalation-model` in `deployment` (`UnexpectedModel` otherwise). The key authorizes inference at the vendor up to the spend limit of its workspace: use a workspace and a key made for this cluster, with a spend limit. Needs `--triage-model` | `/var/run/secrets/shadewright/anthropic/<fileName>` when `analyzer.analysis.anthropic.apiKey` is set ([Model API key](#model-api-key-helm-chart-optional)); unset otherwise |
| `--action-policies` | empty | Comma-separated `ActionType=policy` pairs: the `RemediationPolicy` a proposed request of each action type names, such as `SetContainerMemoryLimit=memory-limits`. Types from the catalog, each once; names DNS subdomain names. An action type without one is never proposed (`NoPolicyForAction`). The context builder also gets each named policy (one `get` by name) and shows the model its bounds for the subject (design 3.3, #493); the analyzer needs `get` on those names (`analyzer-policy-reader`, below). Used with `--triage-model` | unset |
| `--analysis-store-account` | empty (no records) | Azure storage account name (3 to 24 lower-case letters and digits) of the Blob Storage container that keeps one redacted record of each analysis ([ADR-0015](../adr/0015-analysis-records-outlive-the-lab.md)), at `https://<account>.blob.core.windows.net` only. Writes are create-only (`If-None-Match: *`), with the analyzer's workload identity (Storage audience, no key or SAS); the record's ID goes into `status.analysisRecords`. A write that fails is logged (`Failed to write the analysis record`) and changes nothing else. Needs `--triage-model` and `--analysis-store-container` | unset |
| `--analysis-store-container` | empty | The container of the records in `--analysis-store-account`, such as `analyses`. Set with the account or not at all | unset |
| `--module-grants` | not set (every module kind is checked) | Comma-separated module grants the install rendered: `keda`, `flux`, `kyverno`, `kyverno-cluster` (the roles of `analyzer.modules`, design 2.2.2). A module kind whose grant is not listed is not checked or watched: it is reported as `NotGranted` in `SymptomSourcesNotWatched`, except the optional `kyverno-cluster` kinds, which are left out without a report or gauge series. Empty means none was rendered. An unknown name stops the analyzer at start. `flux` also gives the read tool `get_delivery_status` the Flux adapter, reading the watched namespaces with that grant's `list` (design 4); given without `flux`, the tool is not available. Not set, the tool is on like the collector's Flux sources, and answers `could not fetch` where the analyzer may not list Flux's kinds. The Helm chart always sets it from `analyzer.modules`; `config/` does not, since any of `config/modules` may be added | unset |
| `--pod-logs` | `true` | Give the context builder the pod logs source: bounded, redacted excerpts of one container per call, in the watched namespaces only (`get` on `pods/log` through the watch role, see [design 2.3.2](../design/2-architecture.md#232-pod-logs-adapter)). The source takes at most 256 namespaces: with more in `--watch-namespaces`, the analyzer refuses to start with a configuration error that names the limit, unless this is `false`. `false` reads no logs. The Helm chart removes the grant from the watch role too (`analyzer.podLogs.enabled=false` renders this flag and no `pods/log` rule); `config/` always keeps it | unset |
| `--prometheus-url` | empty (no metrics) | Base URL of the Prometheus query API the context builder reads metrics from, with the approved query templates only (see [metrics source profile](#metrics-source-profile-prometheus)), such as `http://prometheus-server.prometheus.svc`: `http` or `https`, no credentials, query or fragment. At most 256 watched namespaces when set, as for `--pod-logs` | unset |
| `--prometheus-cluster` | empty | The `cluster` label value of this cluster's series in a Prometheus shared by several clusters; needs `--prometheus-url`. See [metrics source profile](#metrics-source-profile-prometheus) for the check at startup | unset |

### Environment

| Variable | Used by | Meaning |
| --- | --- | --- |
| `POD_NAMESPACE` | both | Default of `--namespace` |
| `POD_NAME` | analyzer | The Pod the cap Warning event is about. Without it (running outside a cluster) the cap is only logged and counted |
| `OTEL_TRACES_EXPORTER`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` and the other `OTEL_*` variables | analyzer | The OpenTelemetry trace of each analysis, off unless `OTEL_TRACES_EXPORTER=otlp`; see the [traces reference](traces.md). The chart value `analyzer.tracing.endpoint` sets them |

### Values with no flag

These are compiled in. Changing one needs a code change.

| Value | Setting |
| --- | --- |
| Incident debounce, cooldown, analysis timeout | 45 s, 15 min, 10 min |
| Analysis due check | every 10 s |
| Executor: policy check, cleanup and one Git write step | 2 min each |
| Executor: time from `PolicyChecked` to an open pull request | 30 min, then `Expired` |
| Executor: wait for KEDA to report a `ScaledObject` | 10 min, then `NeedsIntervention` |
| Executor: freshness recheck of a waiting request | every 5 min, and on a change to the request when no full recheck ran in the last 5 min (between those, a waiting pull request is only looked at, see `--pull-request-poll-interval`) |
| Source mapping limits | directories 8 deep, 256 files, 512 reads, 1 MiB per file |

## Cluster ID

`--cluster-id` is hashed into every Incident fingerprint, so Incidents from two clusters never merge. It is 1 to 63 letters, digits, `.`, `_` or `-`, starting and ending with a letter or digit. Give each cluster its own. **Changing it starts every Incident afresh**, so choose it once. The manifests ship `lab`. The Helm chart takes it from the value `clusterId`; left empty, it uses the UID of the `kube-system` namespace, read with `lookup` at install and upgrade (stable and unique for the life of the cluster). `helm template`, `helm lint` and a client-side `--dry-run` fail with a message until `clusterId` is set. GitOps tools and client-side renders (`helm template`, helm diff, Argo CD, Flux with a dry-run) ask no cluster, and an installer without `get` on the `kube-system` namespace cannot read it, so each of those must set `clusterId` explicitly. Moving from the default to a value of your own also starts every Incident afresh. It is an ID, never a display name.

## Namespaces and RoleBindings

Three namespaces matter:

- **Operator namespace** (`shadewright-system`: the `namespace` of `config/default`, with the name prefix `shadewright-`). It holds the Deployments, ServiceAccounts, Incidents, RemediationRequests and the analysis budget. The executor reconciles requests only here; a request anywhere else is never seen. A policy must list this namespace in `requestNamespaces`, or the request is `Denied` (`RequestNamespaceNotAllowed`). Renaming it means changing the federated credential subject, every RoleBinding subject, and `--namespace`.
- **Watched namespaces**, set by `--watch-namespaces` (separately for the analyzer and the verifier). The analyzer's cache is limited to them, so it asks for no more than its RBAC grants. None by default in the chart: an empty list watches nothing.
- **Target namespaces**, set by the policy (`targets.namespaces`). Independent of the watched ones: a policy only allows what a request names.

Each watched namespace needs a RoleBinding that binds the ClusterRole `shadewright-analyzer-watch-role` to the ServiceAccount `shadewright-analyzer` of the operator namespace. `config/watch` is the example and holds only the lab's `demo` binding; it is built on its own because `config/default` sets every namespace to the operator's. Without the binding the API server refuses the analyzer's reads there and nothing is observed in that namespace. The verifier needs its own RoleBinding of `verifier-watch-role` to the ServiceAccount `shadewright-verifier` in each namespace it is given (`config/watch/role_binding_verifier_demo.yaml` for the lab). A repository test checks that the lab's `--watch-namespaces` and `config/watch` agree; it cannot check yours.

The permissions, as installed:

| Identity | Scope | Grants |
| --- | --- | --- |
| Analyzer, each watched namespace | RoleBinding to `analyzer-watch-role` | `get`, `list`, `watch` Pods, `events.k8s.io` Events and `autoscaling` HorizontalPodAutoscalers; `get` on `pods/log` (the pod logs source, `--pod-logs`); `get` Deployments, ReplicaSets, StatefulSets |
| Analyzer, each watched namespace, per chart value | RoleBinding to `analyzer-watch-keda` (`analyzer.modules.keda.enabled`, on by default), `analyzer-watch-flux` (`.flux.enabled`, off) and `analyzer-watch-kyverno` (`.kyverno.enabled`, off) | `list` and `watch` only, for the sources' informers: KEDA `scaledjobs` (and `get` too on `scaledobjects`, the owner a KEDA HPA names); Flux `kustomizations`, `helmreleases` and `gitrepositories`; Kyverno `policies` and `wgpolicyk8s.io` `policyreports`. Read only while the module is active; an active module without its role is reported in the settings condition `SymptomSourcesNotWatched` |
| Analyzer, cluster, per chart value | `analyzer-kyverno-cluster` ClusterRole and ClusterRoleBinding (`analyzer.modules.kyverno.clusterWide.enabled`, off) | `list`, `watch` Kyverno `clusterpolicies` and `wgpolicyk8s.io` `clusterpolicyreports`. Off, the Kyverno module's cluster-scoped kinds are left out without a report (`--module-grants`) |
| Analyzer, cluster, with the analysis | `analyzer-policy-reader` ClusterRole and ClusterRoleBinding (`analyzer.analysis.triageModel` and at least one `analyzer.analysis.actionPolicies` name; `config/analysis-policies` for kustomize) | `get` only on the `remediationpolicies` named in `actionPolicies` (`resourceNames`), for the policy bounds the context shows the model (#493). No `list` or `watch`, and no rule without a name. With kustomize, `resourceNames` in `config/analysis-policies` must list every policy of `--action-policies` by hand: an analyzer whose role leaves one out refuses to start ("may not get RemediationPolicy") |
| Analyzer, cluster | `analyzer-role` ClusterRole | `list` Nodes; `get`, `list`, `watch` `remediationsettings`; `update` `remediationsettings/status` |
| Analyzer, operator namespace | `analyzer-role` Role | `get`, `update` the ConfigMap `shadewright-analysis-budget` only (no create); `create`, `get`, `list`, `watch` Incidents and RemediationRequests; `update` `incidents/status`; its own Lease, and `create`, `patch` on core Events (leader election) |
| Executor, cluster | `executor-role` ClusterRole | `get` only: Deployments, StatefulSets, KEDA `scaledobjects`, Flux `kustomizations` and `gitrepositories`, `remediationpolicies`, `remediationsettings` |
| Executor, operator namespace | `executor-role` Role | `get`, `list`, `patch`, `watch` RemediationRequests; `get`, `patch`, `update` `remediationrequests/status`; its own Lease, and `create`, `patch` on core Events (leader election). After v0.1.0 (#265, on main): `get` on the one Incident a request names, to verify the name shown in the pull request body. #683: `get` on `verificationreports` (no `list` or `watch`, and no grant on pods), to read the verifier's report of a request |
| Verifier, each watched namespace | RoleBinding `verifier-watch` to `verifier-watch-role` (never a ClusterRoleBinding) | `list` Pods (no `get`, no `watch`); `get` `apps` ReplicaSets, Deployments and StatefulSets |
| Verifier, operator namespace | `verifier-role` Role | `get`, `list`, `watch` RemediationRequests; `get`, `create` `verificationreports`; `update` `verificationreports/status`; its own Lease (`get`, `update`, `patch` by name) and `create`, `patch` on core Events |
| Metrics | `metrics-auth-role`, one ClusterRoleBinding to all three ServiceAccounts (the verifier's only while the chart deploys it) | `create` TokenReviews and SubjectAccessReviews: the verifier's only cluster-wide grant |

None of the three identities can read Secrets, and none is `cluster-admin`. `config/rbac` also installs admin, editor and viewer ClusterRoles for each custom resource; Shadewright does not use them, they are for your people.

The ConfigMap `shadewright-analysis-budget` holds the analyses started on the current UTC day. It ships without data on purpose; keep `data` out of anything you apply, so a GitOps sync never resets the count. If it is deleted the analyzer cannot recreate it, and nothing is due for analysis until you do (`BudgetNotFound`).

## RemediationSettings

One cluster-scoped object named `default` (field details in the [API reference](api.md)). With the Helm chart, `settings.create=true` creates it from `settings.spec` only when it does not exist (a hook that checks with `lookup`, which finds nothing under `helm template` and `--dry-run`, so those always show it); Helm never changes or deletes it afterwards, so a `spec.paused: true` set by hand survives every `helm upgrade`. Change it with kubectl or GitOps:

| Field | Default | Meaning |
| --- | --- | --- |
| `spec.paused` | `false` | Stops every write: pull requests, closes, finalizer cleanup. Analysis continues. See [emergency stop](../emergency.md) |
| `spec.maxConcurrentRemediations` | `1` | Requests active at once (1 to 20), in the operator namespace: from `PolicyChecked` until `Merged` or a final phase, and while a deleted one still closes its pull request. A further request the policy allows stays `Pending` with the condition `Queued` (`ConcurrencyLimitReached`); a refusal at the first check is never held back, but a request already `Queued` is decided again only once the limits let it through. See [troubleshooting](../troubleshooting.md#limits) |
| `spec.analysis.maxAnalysesPerDay` | `100` | Model-backed analyses per UTC day across all Incidents, 0 to 10000; the default applies also when `spec.analysis` is left out. Read by the analysis trigger: `0` marks every Incident not due (`AnalysisDisabled`). With `--triage-model` the analysis reserves one unit per claimed attempt before its first model call, so the count is enforced; without it nothing adds to the count |
| `spec.maxAnalysesPerDay` | none | Deprecated: the v0.1.0 place of `spec.analysis.maxAnalysesPerDay`. A CEL rule on the field refuses every write that sets or changes it, whatever the client. kubectl validates fields strictly and would refuse an unknown field anyway, but clients that send no field validation get the API server's default, a warning, and an unknown field is dropped without a word: Flux and other controller-runtime clients among them. The rule is what refuses those writes now. An object stored by v0.1.0 keeps the value through updates that leave it as it is (validation ratcheting), and removing it always passes. While it is set, the analysis trigger goes by the lower of the two fields, so a stored `0` keeps analysis off. Move the value and remove this field, see [upgrade](../upgrade-uninstall.md#upgrade) |
| `spec.analysis.maxContextBytes` | `32768` | Bytes of redacted context in one analysis, 4096 to 262144. Planned: the context builder still uses its own bound (#372) |
| `spec.analysis.maxToolSteps` | `4` | Read tool calls of one escalated analysis, 0 to 16. Enforced by the analysis loop (`--triage-model`) |
| `spec.analysis.maxOutputTokens` | `2048` | Output tokens of one model call, 256 to 16384. An analysis makes at most 3 + `maxToolSteps` calls. Enforced by the analysis loop (`--triage-model`) |
| `spec.analysis.maxConcurrentAnalyses` | `1` | Analyses calling a model at once, 1 to 8. Enforced by the analysis loop (`--triage-model`) |
| `spec.analysis.models.triage`, `spec.analysis.models.escalation` | none | The small model for the one structured call and the optional stronger model for escalation (ADR-0023); escalation needs triage, and the two may not name the same endpoint and deployment. Each has `provider` (`Foundry`, the default, `Anthropic` or `OpenAICompatible`: the API the adapter speaks, [ADR-0037](../adr/0037-the-anthropic-api-as-a-second-model-provider.md)), `endpoint` (HTTPS, at most 256 characters, a DNS host name, port 1 to 65535 if any, no IP, user info, query or fragment; exactly `https://api.anthropic.com` for `Anthropic`, and never that host for `Foundry`), `deployment` (1 to 64 letters, digits, `.`, `_`, `-`; for `Anthropic` the model ID) and `apiVersion` (`v1` or a date such as `2024-10-21`, optionally `-preview`; required for `Foundry`; for `Anthropic` leave it out: a value is accepted, so an object stored while the field was required stays valid, but never sent), and for `Foundry` or `OpenAICompatible` an optional `inferenceAPI` (`ChatCompletions`, used when left out, or `Responses`, which needs `apiVersion: v1` for Foundry; set it for a model that refuses function tools on chat completions, such as the `gpt-6.1` models on escalation, [#501](https://github.com/pkalin/shadewright/issues/501)). For OpenAICompatible, omit `apiVersion` and use its separate optional file and host binding described above. No credential is stored here: the analyzer uses its workload identity for `Foundry` and the file of `--anthropic-api-key-file` for `Anthropic`. With `Anthropic` the redacted context leaves Azure ([data handling](../data-handling.md#where-the-context-goes-per-provider)). The endpoint decides where that token and the redacted context go, so guard write access to this object (the `patch` right of the [emergency runbook](../emergency.md) included). No triage model: no model call, a hand-off. Read by the analysis loop (`--triage-model`), which refuses an endpoint outside `--model-endpoint-suffixes` or on another port than 443, and private addresses when it dials ([#76](https://github.com/pkalin/shadewright/issues/76)) |
| `spec.analysis.models.*.maxTokensPerDay` | required | Daily spend cap of the deployment in tokens (input plus output) per UTC day, 0 to 1,000,000,000; `0`, or a spent cap, stops calls to it and hands off. Enforced within one analysis; counting across analyses is planned (#208) |
| `spec.repositories` | none | Up to 16 repositories Shadewright may open pull requests in. URLs are unique |
| `spec.repositories[].url` | required | GitHub: `https://github.com/<owner>/<repo>`, lower case, no `.git`. Must equal the Flux `GitRepository` URL byte for byte, or the request is `Denied` (`RepositoryNotConfigured`) |
| `spec.repositories[].targetBranch` | `main` | The only branch pull requests may target. Must equal the branch Flux tracks |
| `spec.repositories[].provider` | required | `GitHub` is the only value |
| `spec.repositories[].github` | required for GitHub | `appClientID`, `installationID` and `signer.azureKeyVault.keyID` (a key URL such as `https://<vault>.vault.azure.net/keys/<name>`, with an optional 32-digit version) |
| `spec.modules.platforms`, `spec.modules.integrations` | empty | Modules enabled by name. A module contributes only if it is enabled here and detected. `generic` is always on and is not listed. Compiled in: platforms `aks` and, detection only (no diagnosis rules), `eks` and `gke`; integrations `cloudflared`, `flux`, `keda`, `kyverno`, `secrets-store-csi-driver`. A name no module has is accepted and reported in the `UnknownModules` condition |

An action contributed by an integration module (`AdjustKedaMaxReplicas` needs `keda`) is `Denied` with `ModuleNotEnabled` unless the module is listed. The settings `status` is a report written by the analyzer; no component reads it to decide anything.

## Metrics endpoint

`config/default` patches all three Deployments to serve metrics on `:8443` over HTTPS and creates the Services `shadewright-executor-metrics-service`, `shadewright-analyzer-metrics-service` and `shadewright-verifier-metrics-service` (port `https`, 8443). With `--metrics-secure=true` each request is authenticated by a TokenReview and authorized by a SubjectAccessReview. A scraper therefore needs a ServiceAccount token, and that identity needs the `get` verb on the non-resource URL `/metrics`: the ClusterRole `shadewright-metrics-reader` grants it, and **nothing binds it by default**, so bind it to your Prometheus ServiceAccount.

The shipped NetworkPolicies also gate the network: the namespace is default-deny for ingress and egress, and ingress to port 8443 is allowed only from namespaces labeled `metrics: enabled`. Egress for the executor and the analyzer is limited to cluster DNS and TCP 443 and 6443 (the Kubernetes API, Git host and Key Vault are on those ports); the verifier's is DNS and the Kubernetes API only. With the chart value `networkPolicy.analyzer.prometheus.enabled` (off by default) the analyzer, and only the analyzer, gets one more rule, to the Prometheus Pods you name in one namespace on one port (default 9090); see the chart README. `networkPolicy.analyzer.traceCollector.enabled` (off by default) adds the same kind of rule to the OpenTelemetry collector of the [analysis traces](traces.md) (default port 4317).

**DNS egress profile (chart only).** One value, `networkPolicy.dns`, decides where port 53 (UDP and TCP) is open, and the analyzer policy, the executor policy and the NetworkPolicy of the `helm test` Pod all use that same rule, so the test cannot pass where the workloads could not resolve. Pick the profile that matches the resolver your Pods query (`kubectl exec` into a Pod and read `/etc/resolv.conf`):

| `networkPolicy.dns.mode` | Allows | Pick it when |
| --- | --- | --- |
| `kubeDns` (default) | Pods labeled `k8s-app: kube-dns` in the `kube-system` namespace | `nameserver` in `/etc/resolv.conf` is the cluster DNS Service IP and CoreDNS or kube-dns serves it. This is what `config/default` ships, so a default install renders the same rules as before. |
| `resolverCIDRs` | The addresses in `networkPolicy.dns.resolverCIDRs` (at least one IPv4 or IPv6 CIDR, no duplicates, at most 8) | Pods query an address that is not a `kube-dns` Pod, such as NodeLocal DNSCache on `169.254.20.10`: set `resolverCIDRs={169.254.20.10/32}`. With kube-proxy in IPVS mode, NodeLocal DNSCache listens on the cluster DNS Service IP as well; add that address too. |

The schema refuses an unknown mode and an empty or malformed `resolverCIDRs`. The `kubeDns` profile is the only one tried on the lab (Cilium); `resolverCIDRs` is checked by the chart's render tests only, not on any CNI. NodeLocal DNSCache and other CNIs are untested, so after choosing a profile run `helm test` and resolve a name from a Pod in the namespace. The kustomize manifests (`config/default`) have only the `kubeDns` rule: edit the policies there if you use them with NodeLocal DNSCache.

The metrics each component serves are listed in the [metrics reference](metrics.md).

The certificate is self-signed unless you set `--metrics-cert-path` (the cert-manager overlay is commented out in `config/default`). `config/prometheus/monitor.yaml` is a `ServiceMonitor` for the Prometheus Operator, not included by default; it scrapes with the Pod's ServiceAccount token and `insecureSkipVerify: true`, so use a real certificate and `serverName` if you care about that. Do not use `--metrics-secure=false` outside a test.

### TLS trust profile (Helm chart, optional)

By default the endpoint serves a certificate the binary generates for itself, so a scraper cannot tell that it reached the genuine endpoint, and the chart's `ServiceMonitor` skips verification (the token is still checked with a TokenReview). The chart has an optional profile that turns this around. Issuance stays with you: the chart creates no cert-manager object, and the private key never goes through `values`.

1. Issue one serving certificate per component from a CA you control. Both list the DNS name you will give as `serverName` as a subject alternative name (the chart's two ServiceMonitors take one value).
2. Create a Secret of type `kubernetes.io/tls` for each component in the release namespace, and the CA certificate as a Secret or ConfigMap Prometheus can read through its ServiceMonitor (the same namespace as the release):

```bash
kubectl -n shadewright-system create secret tls shadewright-executor-metrics-tls --cert=executor.crt --key=executor.key
kubectl -n shadewright-system create secret tls shadewright-analyzer-metrics-tls --cert=analyzer.crt --key=analyzer.key
kubectl -n shadewright-system create configmap shadewright-metrics-ca --from-file=ca.crt=ca.crt
```

3. Point the chart at them:

```yaml
executor:
  metricsTLS:
    secretName: shadewright-executor-metrics-tls
analyzer:
  metricsTLS:
    secretName: shadewright-analyzer-metrics-tls
metrics:
  serviceMonitor:
    enabled: true
    tls:
      enabled: true
      serverName: metrics.shadewright.example   # a SAN of both certificates
      ca:
        configMap:
          name: shadewright-metrics-ca          # or ca.secret.name; key defaults to ca.crt
```

`<component>.metricsTLS.secretName` mounts the Secret read-only at `/etc/shadewright/metrics-tls` and passes `--metrics-cert-path` (the files are `tls.crt` and `tls.key`, the defaults of `--metrics-cert-name` and `--metrics-cert-key`). The binary reloads the files when they change, so a renewal that updates the Secret needs no restart. `metrics.serviceMonitor.tls.enabled` makes each ServiceMonitor send `tlsConfig` with the `ca` and the `serverName` and no `insecureSkipVerify`; rendering fails when it is on without a CA (or with a Secret and a ConfigMap both), and the schema refuses names that are not DNS-1123 and keys it does not know. Without a profile nothing changes. TokenReview, the RBAC (the `metrics-reader` ClusterRole for Prometheus) and the NetworkPolicies are the same with or without it. Not covered by the chart tests yet: a scrape on a real cluster that succeeds with the right CA and fails with a wrong one ([#343](https://github.com/pkalin/shadewright/issues/343) follow-up).

## Metrics source profile (Prometheus)

The analyzer's `query_metrics` tool reads cAdvisor and kube-state-metrics series through the Prometheus adapter (`internal/providers/metrics/prometheus`, [design section 4](../design/4-tools.md#4-tools)). The analyzer creates it from `--prometheus-url` and `--prometheus-cluster` and gives it the watched namespaces; it hands it to the context builder and the `query_metrics` tool, which the analysis loop calls when the analyzer runs with `--triage-model` ([#76](https://github.com/pkalin/shadewright/issues/76)). The chart sets the flags from `analyzer.prometheus.url` and `analyzer.prometheus.cluster`; set `networkPolicy.analyzer.prometheus` with them. This section states what source the adapter supports, so that your Prometheus and the flags agree.

For the Incident of a KEDA `ScaledObject`, the context builder also reads the HPA series of kube-state-metrics, `kube_horizontalpodautoscaler_status_target_metric` and `kube_horizontalpodautoscaler_spec_target_metric`, which kube-state-metrics exports by default ([#480](https://github.com/pkalin/shadewright/issues/480)). A kube-state-metrics that drops them (`--metric-denylist`) leaves those lines of the metrics block at "no series". kube-state-metrics marks `kube_horizontalpodautoscaler_spec_target_metric` as EXPERIMENTAL (and `kube_horizontalpodautoscaler_status_target_metric` as STABLE), so a release can rename or drop it; when the target series is missing, the context shows `hpa_metric_target: no series` and keeps the current-metric line.

| Source | Supported | What to set |
| --- | --- | --- |
| A Prometheus that holds the series of this cluster only (the lab's, [ADR-0017](../adr/0017-prometheus-in-the-lab.md)) | yes, the default | nothing |
| The same, with cAdvisor or kube-state-metrics scraped by two jobs (a ServiceMonitor next to a static job, two kube-state-metrics replicas), or the kubelet's `/metrics/resource` next to `/metrics/cadvisor` | yes | nothing: duplicates count once |
| A standalone cAdvisor next to the kubelet's | yes, if relabeled | relabel its series to the kubelet's labels (`namespace`, `pod`, `container`) with the same `id` (the cgroup path), or its CPU counts as another container instance |
| A shared backend that holds several clusters (Thanos, Mimir, Grafana Cloud, Azure Monitor managed Prometheus), each cluster's series under a `cluster` label | untested; needs the cluster setting | `--prometheus-cluster`: this cluster's `cluster` label value |
| A shared backend whose clusters are not told apart by a `cluster` label | **no** | give the analyzer a source with this cluster's series only |

**Mixed clusters are refused.** Every query keeps the `cluster` label apart and drops it from the result. Without the cluster setting, an answer with two series of one container that differ only in `cluster` (two clusters with the same namespace, pod and container, or a source that labels only some of its series) is refused as an invalid response, never mixed. Set `--prometheus-cluster` for a shared backend, or the queries of any name the clusters share fail.

**The cluster label must be stored on the series**, put there by the scrape (a relabeling) or by remote write. An external label that the queried Prometheus adds only to what it sends out (federation, remote write, alerts) is not on the series it answers queries from, so a selector on it matches nothing. A mistyped value, or a label the series do not carry, leaves every result empty, and an empty result can read as "no restarts". So the analyzer checks at startup and every 10 minutes, with one fixed query that returns no label (`group(kube_pod_container_status_restarts_total{cluster="<value>"}) and group(container_memory_working_set_bytes{cluster="<value>"})`), that both kube-state-metrics and cAdvisor hold series with the value. When neither or only one does, the analyzer still starts (no crash loop), logs an error, and marks metrics **unavailable**: every metrics query fails, so the metrics section of a context is "could not fetch", which escalates the analysis instead of reading empty answers as evidence of absence. While the last check found the series, queries go straight to the adapter; a label that disappears later switches metrics off at the next check. A check that cannot reach Prometheus is not "not found": it changes nothing the last answer said, and before any answer, or once the last one is older than 10 minutes, a query checks first, waiting no longer than its own deadline. With no watched namespace the analyzer builds no source and runs no check.

`--prometheus-cluster` is set by the administrator only; no query, model output or custom resource can set or change it. It is added as a quoted equality matcher (`cluster="aks-lab"`) to every selector of every query template. The adapter refuses to start (invalid configuration) on a value that is not 1 to 100 letters, digits, `.`, `_`, `:`, `/` or `-`, starting and ending with a letter or digit (an EKS cluster ARN and `prod/eu-west-1` fit); the error does not repeat the value. A `job` setting is not offered: cAdvisor and kube-state-metrics are scraped by different jobs, so one `job` matcher on every selector would leave half of the templates silently empty, and duplicate jobs need no matcher.

**Labels the scraped pods set.** The namespace scope of `query_metrics` (it names only the analyzer's watched namespaces) holds only if the `namespace`, `pod`, `container` and `cluster` labels of the cAdvisor and kube-state-metrics series are the scrape's, not a tenant's. A job that scrapes tenant pods must not honour their labels (`honor_labels: false`, the default), or must drop the cAdvisor and kube-state-metrics metric names (`container_*`, `kube_*`) from what it keeps; otherwise a pod can expose a series named like a template's metric with another namespace's labels. Metric values stay evidence a tenant can influence (a pod controls its own CPU and memory use), never a fact the analyzer acts on alone.

How duplicates and restarts are counted: each template first reduces the series of one container (namespace, pod, container, cluster) to one value and only then aggregates. Memory, limits, restart counts and states take the largest series of the container: duplicates carry the same value, and after a restart the result is the larger of the old and the new instance while both still have series, never their sum. The CPU templates take the largest series of each container instance (cAdvisor's `id` label, its cgroup) and add the instances, since they ran one after the other within the 5-minute window; series without `id` (the kubelet's `/metrics/resource`, or a scrape that drops the label) are reduced to their largest apart, and the result is the larger of the two estimates, so a container measured both ways is never counted twice. Without `id`, a window with a restart can be understated, never doubled.

## Workload identity (Azure)

The executor needs a cloud identity: it signs the GitHub App's JWT with a non-exportable Key Vault key. The analyzer needs one only with Foundry enabled, `--triage-model` and `--model-endpoint-suffixes`, for a `Foundry` model call, or with `--analysis-store-account`; an analyzer whose deployments are all `Anthropic`, or `OpenAICompatible` with `--disable-foundry`, needs none. The key never enters the cluster. On AKS with workload identity:

- The ServiceAccount `shadewright-executor` carries the annotation `azure.workload.identity/client-id`. In `config/rbac/executor/service_account.yaml` it is the placeholder `<executor-managed-identity-client-id>` and **must be replaced**; the ID is not a secret, but it belongs in your overlay, not in a public repository. The Deployment's Pods already carry the label `azure.workload.identity/use: "true"`.
- The managed identity needs a federated credential for the subject `system:serviceaccount:shadewright-system:shadewright-executor` and audience `api://AzureADTokenExchange`, and only the `sign` operation on the key. If you rename the namespace or either ServiceAccount, the credential must change with it.
- The analyzer has no identity annotation or label in `config/`, because the analysis is off by default. With Foundry enabled, `--triage-model` and `--model-endpoint-suffixes` it exchanges its projected service account token for an Entra ID token for the Cognitive Services audience only (`AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and `AZURE_FEDERATED_TOKEN_FILE`, set by the workload identity webhook), and refuses to start without them. It needs its own managed identity (never the executor's), with a federated credential for `system:serviceaccount:shadewright-system:shadewright-analyzer`, audience `api://AzureADTokenExchange`, and a data-plane role on the Foundry resource that allows inference only (`Cognitive Services OpenAI User`, or `Azure AI User` on a Foundry resource). In the Helm chart set `analyzer.identity.azure.clientId`: it adds the annotation and the Pod label `azure.workload.identity/use`. With `config/`, patch the analyzer ServiceAccount as below (name `shadewright-analyzer`) and add that label to the analyzer Pod template.

A kustomize patch for the annotation, in your overlay:

```yaml
patches:
  - target:
      kind: ServiceAccount
      name: shadewright-executor
    patch: |-
      - op: replace
        path: /metadata/annotations/azure.workload.identity~1client-id
        value: "<client id of the executor identity>"
```

Without a working identity, a request that reaches the Git step is retried quietly and stays `Pending`; see [troubleshooting](../troubleshooting.md#no-reason-at-all). Rotating the key, the installation and the credentials is in [identity rotation](../identity-rotation.md).
