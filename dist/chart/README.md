# shadewright

![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square)

<!-- No version badges: release-please bumps Chart.yaml in the release pull request, and a version printed here would make this file stale on every release. The version is on the releases page and in Chart.yaml. -->

<!-- This file is generated from values.yaml and README.md.gotmpl by helm-docs (make chart-docs). Do not edit it by hand. -->

[Shadewright](https://shadewright.io) is a Kubernetes operations agent. It watches a cluster, groups repeated symptoms into incidents, asks an LLM for a diagnosis and proposes a fix only as a pull request to your GitOps repository. The GitOps controller deploys the fix and Shadewright verifies the effect. Model output is never authorization: deterministic policy decides what may run.

This chart installs the three components (the analyzer, the executor and the verifier), their RBAC, NetworkPolicies, metrics Services and the CRDs.

## Prerequisites

- Kubernetes version `>=1.32.0-0` (the `kubeVersion` of Chart.yaml).
- Flux, the GitOps controller that deploys the pull requests the executor opens (Argo CD is not supported yet).
- Workload identity for the executor: a managed identity (Azure) with a federated credential for the executor ServiceAccount, allowed to sign with the GitHub App key in Key Vault. Its client ID is `executor.identity.azure.clientId`. The ServiceAccount names derive from the release name: install as release `shadewright` in the namespace `shadewright-system` to get `shadewright-executor`, `shadewright-analyzer` and `shadewright-verifier`, and check them with `helm template` before you create the credential.
- The images: the published chart runs the release's signed images by digest (written into its `values.yaml` when the chart is published). A chart from a source checkout has no digests and refuses to render until you set them, or opt in with `image.allowTag: true` to run the `appVersion` tag with images built from that same checkout (the v0.1.0 images stop at start with an empty watch list).

## Install

With no values the published chart installs an idle Shadewright. The analyzer starts and is Ready but watches no namespace (no RoleBinding outside the operator namespace, no Incident): it reads no namespaced object outside the operator namespace, has no Git identity, and writes only in the operator namespace (its Leases, the budget ConfigMap, Events) plus the settings status once a settings object exists; it still lists Nodes and reads discovery and the settings cluster-wide. The executor has no Azure workload identity, so it cannot reach Key Vault to sign for the GitHub App. The verifier follows `analyzer.watchNamespaces` (`verifier.watchNamespaces` is `null`), so it reads no pod either. It has no cloud identity of any kind: no model access and no Git credentials, so there is nothing to configure for it. The cluster ID defaults to the UID of the `kube-system` namespace. To catch a forgotten watch list, alert on `shadewright_analyzer_watched_namespaces == 0`.

```bash
helm install shadewright oci://ghcr.io/pkalin/charts/shadewright --version <version> \
  --namespace shadewright-system --create-namespace
helm test shadewright --namespace shadewright-system
```

`helm test` runs a Pod that checks that the metrics Service of each component answers (so each Deployment has a Ready Pod) and refuses a scrape without a token. To put Shadewright to work, write a `my-values.yaml`:

```yaml
clusterId: my-cluster            # optional: the kube-system UID otherwise; required for helm template
executor:
  identity:
    azure:
      clientId: <client ID of the executor managed identity>
analyzer:
  watchNamespaces: [<namespace to watch>]   # each must exist; empty watches none, never all
```

```bash
helm upgrade --install shadewright oci://ghcr.io/pkalin/charts/shadewright --version <version> \
  --namespace shadewright-system --create-namespace -f my-values.yaml
```

Choose the cluster ID once: it is hashed into every Incident fingerprint, so changing it, also from the default to a value of your own, starts every Incident afresh. GitOps tools and client-side renders (`helm template`, helm diff, Argo CD, Flux with a dry-run) ask no cluster, and an installer without `get` on the `kube-system` namespace cannot read it, so each of those must set `clusterId` explicitly.

`values.schema.json` validates the values on install, upgrade, lint and template. To manage the `RemediationSettings` object from the chart, set `settings.create: true` and give `settings.spec` (the fields are in the [API reference](https://shadewright.io/reference/api/)):

```yaml
settings:
  create: true
  spec:
    maxConcurrentRemediations: 1
    repositories:
      - name: fleet
        url: https://github.com/<owner>/<repo>
        targetBranch: main
        provider: GitHub
        github:
          appClientID: <GitHub App client ID>
          installationID: <installation ID>
          signer:
            azureKeyVault:
              keyID: https://<vault>.vault.azure.net/keys/<key>
    modules:
      platforms: [aks]
      integrations: [flux]
```

## The verifier

`shadewright-verifier` reads pod status to tell whether a merged fix held (ADR-0030). It is deterministic code with no model, no Git credentials and no cloud identity. It writes one `VerificationReport` per request in the operator namespace and never writes request status. Its reads outside the operator namespace are one ClusterRole, `verifier-watch-role` (`list` on `pods`, `get` on `deployments`, `replicasets` and `statefulsets`), bound by one RoleBinding in each namespace of `verifier.watchNamespaces` and never by a ClusterRoleBinding. The same list goes to `--watch-namespaces`. Left at `null` it follows `analyzer.watchNamespaces`; a list of its own, an empty one included, is used as it is, so you can watch fewer namespaces than the analyzer or none.

## Standby replicas

The Deployments run one Pod each by default. `executor.replicas`, `analyzer.replicas` and `verifier.replicas` (each at least 1) add warm standbys:

```yaml
executor:
  replicas: 2
analyzer:
  replicas: 2
verifier:
  replicas: 2
```

- **What a standby gives.** Leader election is always on, so one Pod per component holds the Lease and works; the others wait. When the leader's Pod fails or is evicted, a standby takes over after about one Lease duration (15 seconds by default) instead of after a Pod start. Writes use optimistic concurrency and deterministic names, so a second Pod is safe.
- **What it costs.** An idle Pod per component and identity (its requests and limits, and one more node slot).
- **What it does not give.** No throughput: only the leader reconciles. Horizontal scaling is a separate feature ([#559](https://github.com/pkalin/shadewright/issues/559)).
- **What the chart adds above 1.** A PodDisruptionBudget with `minAvailable: 1` for that component, and a preferred Pod anti-affinity by node (`kubernetes.io/hostname`), so a node drain or a node failure leaves one Pod. With a single node both Pods share it, and a drain of that node stops at the second Pod: its replacement cannot be scheduled while the node is cordoned, so the budget holds until you delete the Pod yourself or uncordon the node. An `affinity` of your own that sets `podAntiAffinity` replaces the chart's term. With 1 replica neither object is rendered.
- **No RBAC change.** Each component already holds the grant for its own Lease.

## Network policy

On by default (`networkPolicy.enabled: true`). The chart installs a default deny for the release namespace (ingress and egress), one egress policy per component and one ingress policy for the metrics port:

- All three components may send DNS (`networkPolicy.dns`) and TCP 443 and 6443 to any address. 443 is the model endpoint, Key Vault, Entra ID, GitHub and the API server of a managed cluster; 6443 is the API server on kubeadm and kind. The policies are separate objects, so you can narrow one without touching the other.
- The analyzer gets one more rule when you set `networkPolicy.analyzer.prometheus.enabled`: TCP to the Prometheus Pods you name (default port 9090). It is off by default because the analyzer sends Prometheus nothing unless `--prometheus-url` is set: with `--prometheus-cluster` too, it probes it at startup and every 10 minutes, and it queries it when the analysis loop ([#76](https://github.com/pkalin/shadewright/issues/76), on with `analyzer.analysis.triageModel`) calls the context builder or a read tool. The executor never gets it. Allow the analyzer in the ingress policy of the Prometheus namespace as well; the chart changes nothing outside its own namespace.
- Ingress to the metrics port comes only from namespaces labeled `metrics: enabled`. Nothing else is allowed in.

**What this does not do.** A NetworkPolicy cannot match host names, and the API server has no label to select, so the chart limits ports and directions, not destinations. The analyzer can reach any HTTPS address at the network level, `api.github.com` included. What keeps it away from GitHub is that its binary has no GitHub adapter and holds no GitHub credential, not the policy. Restricting egress by host name (for example a Cilium `CiliumNetworkPolicy` with `toFQDNs`, or Azure CNI powered by Cilium on AKS with an FQDN policy) is your choice and is not shipped or tested here. The API server address is unknown when the chart renders, so the rule allows 443 and 6443 to any address; to narrow it, replace that rule with `ipBlock` ranges for the API server (on AKS, the private cluster endpoint or the address in `kubectl get endpoints kubernetes`) through your own policy.

A CNI that does not enforce NetworkPolicy accepts the objects and ignores them. Setting `networkPolicy.enabled: false` installs none of them, so the namespace loses its default deny too. Whether the lab CNI (Cilium) lets the API server through the `ipBlock` rule is checked in the lab (#216); until then a missing API server connection after install is the first thing to look at.

## Verify the signatures

The chart and the images are signed with cosign keyless (GitHub OIDC) by digest. Verify before you install. The identity of the chart is the chart workflow on a release tag of this repository:

```bash
cosign verify \
  --certificate-identity-regexp '^https://github\.com/pkalin/shadewright/\.github/workflows/chart\.yml@refs/tags/v[0-9].*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/pkalin/charts/shadewright@sha256:<chart digest>
```

The digest is in the run summary of the Chart workflow, or `helm pull oci://ghcr.io/pkalin/charts/shadewright --version <version>` prints it (`Digest:`). Image signatures are verified the same way: use the image name (`ghcr.io/pkalin/shadewright-executor@sha256:<digest>`, `shadewright-analyzer` or `shadewright-verifier`) and the `images\.yml` workflow in the identity.

## Upgrade

```bash
helm upgrade shadewright oci://ghcr.io/pkalin/charts/shadewright --version <version> \
  --namespace shadewright-system -f my-values.yaml
```

The CRDs are templates of this chart, so `helm upgrade` upgrades them (Helm itself never upgrades CRDs from a `crds/` directory). Read the release notes first: a change to the CRD schema, the flags or the values is listed as breaking. With `settings.create: true` the `RemediationSettings` object is created only when it does not exist: an upgrade never changes it, so a pause set by hand (`spec.paused: true`) survives. Change the object with kubectl or GitOps.

## Uninstall

```bash
helm uninstall shadewright --namespace shadewright-system
```

- With `crd.keep: true` (the default) the CRDs stay, and so does every Incident and RemediationRequest. Set it to `false` before you uninstall only if you want them all deleted with the release.
- The `RemediationSettings` object created by `settings.create` is a Helm hook object: Helm does not track it, so it is **not removed**. Delete it yourself (`kubectl delete remediationsettings default`).
- Read [upgrade and uninstall](https://shadewright.io/upgrade-uninstall/) for the order that avoids stuck requests, and for the GitHub App, Key Vault key and federated credential that live outside the cluster.

## Values

## Values

### General

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| nameOverride | string | `""` | Partially overrides the chart name, which is kept in the release name. At most 63 characters. Empty uses `shadewright`. |
| fullnameOverride | string | `""` | Fully overrides the resource name prefix, at most 63 characters. Empty uses the release name (or `<release>-shadewright` if it does not contain the chart name). The ServiceAccount names, and so the subject of the federated credential, derive from it. |
| clusterId | string | empty (the UID of `kube-system`) | Cluster ID, 1 to 63 letters, digits, `.`, `_` or `-`, starting and ending with a letter or digit. Passed as `--cluster-id` and hashed into every Incident fingerprint so Incidents of two clusters never merge. Empty uses the UID of the `kube-system` namespace, read with `lookup` at install and upgrade (stable and unique for the life of the cluster); `helm template`, `helm lint`, a client-side `--dry-run`, GitOps tools that render client-side (helm diff, Argo CD, Flux with a dry-run) and an installer without `get` on the `kube-system` namespace must set it, or the chart fails. Changing it, also from empty to a value, starts every Incident afresh, so choose it once. It is an ID, never a display name. |
| imagePullSecrets | list | `[]` | Image pull secrets of both Pods, a list of `{name: <secret>}` (at most 16), for a mirror that needs them. The default images are public. Each component's `imagePullSecrets` is added to it. |

### Images

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| image.allowTag | bool | `false` | Allow an image without a digest to run by the `appVersion` tag. Off by default, so the chart fails to render when a digest is missing: the published chart carries the release's signed digests and needs nothing here. Set `true` only from a source checkout, with images built from that same checkout (older images, such as v0.1.0, stop at start with an empty `analyzer.watchNamespaces`). |
| executor.image.repository | string | `"ghcr.io/pkalin/shadewright-executor"` | Repository of the executor image (a mirror, for example). The Deployment runs repository@digest, or repository:appVersion without a digest when `image.allowTag` is true. |
| executor.image.digest | string | the release's digest in the published chart, empty in the source | Digest of the executor image: `sha256:` and 64 lower case hex digits. The chart published on a release tag carries the digest of that release's signed image here. Empty (a source checkout) fails to render unless `image.allowTag` is true, which runs the `appVersion` tag instead. The schema refuses a tag in this field. |
| executor.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. One of `Always`, `IfNotPresent` or `Never`. |
| analyzer.image.repository | string | `"ghcr.io/pkalin/shadewright-analyzer"` | Repository of the analyzer image (a mirror, for example). The Deployment runs repository@digest, or repository:appVersion without a digest when `image.allowTag` is true. |
| analyzer.image.digest | string | the release's digest in the published chart, empty in the source | Digest of the analyzer image: `sha256:` and 64 lower case hex digits. The chart published on a release tag carries the digest of that release's signed image here. Empty (a source checkout) fails to render unless `image.allowTag` is true, which runs the `appVersion` tag instead. The schema refuses a tag in this field. |
| analyzer.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. One of `Always`, `IfNotPresent` or `Never`. |
| verifier.image.repository | string | `"ghcr.io/pkalin/shadewright-verifier"` | Repository of the verifier image (a mirror, for example). The Deployment runs repository@digest, or repository:appVersion without a digest when `image.allowTag` is true. |
| verifier.image.digest | string | the release's digest in the published chart, empty in the source | Digest of the verifier image: `sha256:` and 64 lower case hex digits. The chart published on a release tag carries the digest of that release's signed image here. Empty (a source checkout) fails to render unless `image.allowTag` is true, which runs the `appVersion` tag instead. The schema refuses a tag in this field. |
| verifier.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. One of `Always`, `IfNotPresent` or `Never`. |

### Metrics

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| metrics.enabled | bool | `true` | Serve the metrics endpoint of the three components, with its three Services. When false there are no Services, no metrics RBAC and no metrics NetworkPolicy. |
| metrics.port | int | `8443` | Metrics port, 1 to 65535. Passed as `--metrics-bind-address` and opened by the metrics NetworkPolicy. |
| metrics.secure | bool | `true` | Serve metrics over HTTPS with a TokenReview and a SubjectAccessReview per request. `false` serves plain HTTP with no authentication: for a test only, `helm install` prints a warning. |
| metrics.serviceMonitor.enabled | bool | `false` | Install a Prometheus Operator `ServiceMonitor` for each component (needs the `monitoring.coreos.com/v1` CRDs and `metrics.enabled`). Prometheus scrapes with its own ServiceAccount token, so bind that ServiceAccount to the `metrics-reader` ClusterRole, and label its namespace `metrics: enabled` for the NetworkPolicy. The endpoint's certificate is self-signed, so the ServiceMonitor skips its verification. It scrapes with `bearerTokenFile` (Prometheus's own token), which Prometheus Operator deprecates and rejects when `arbitraryFSAccessThroughSMs` is denied: then keep this off and write your own ServiceMonitor with `authorization` from a Secret that holds a token allowed to read metrics. |
| metrics.serviceMonitor.interval | string | `""` | Scrape interval, such as `30s` or `1m`. Empty uses the Prometheus default. |
| metrics.serviceMonitor.labels | object | `{}` | Further labels on the ServiceMonitors, for the `serviceMonitorSelector` of your Prometheus. The chart's own labels cannot be overridden. |
| metrics.serviceMonitor.tls.enabled | bool | `false` | Verify the serving certificate of the metrics endpoints instead of skipping the check. On, the ServiceMonitors send a `tlsConfig` with `ca` and `serverName` and no `insecureSkipVerify`, and the chart refuses to render without a CA. Needs `executor.metricsTLS.secretName` and `analyzer.metricsTLS.secretName` (certificates signed by that CA). Issuance stays with you: the chart creates no cert-manager object. |
| metrics.serviceMonitor.tls.serverName | string | `""` | Name the certificates are checked against (the TLS server name), a DNS name that every serving certificate lists as a subject alternative name. Required when `tls.enabled`. |
| metrics.serviceMonitor.tls.ca.secret.name | string | `""` | Name of a Secret in the release namespace that holds the CA certificate (PEM) Prometheus trusts. Set this or `ca.configMap.name`, not both. |
| metrics.serviceMonitor.tls.ca.secret.key | string | `"ca.crt"` | Key of the CA certificate in that Secret. |
| metrics.serviceMonitor.tls.ca.configMap.name | string | `""` | Name of a ConfigMap in the release namespace that holds the CA certificate (PEM) Prometheus trusts. Set this or `ca.secret.name`, not both. |
| metrics.serviceMonitor.tls.ca.configMap.key | string | `"ca.crt"` | Key of the CA certificate in that ConfigMap. |

### Network policy

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| networkPolicy.enabled | bool | `true` | Install the NetworkPolicies: default deny for the namespace, DNS and TCP 443 and 6443 egress per component (the same two ports for both: a NetworkPolicy cannot tell GitHub from the model endpoint, see the README), an optional Prometheus rule for the analyzer only, and ingress to the metrics port from namespaces labeled `metrics: enabled`. Setting `false` installs no NetworkPolicy at all, so the namespace loses its default deny too and every Pod in it can talk to anything unless you restrict it yourself (`helm install` prints a warning). |
| networkPolicy.dns.mode | string | `"kubeDns"` | How the analyzer, the executor, the verifier and the `helm test` Pod may reach cluster DNS (port 53, UDP and TCP). `kubeDns` allows the `kube-dns` Pods of `kube-system` (label `k8s-app: kube-dns`, CoreDNS and kube-dns). `resolverCIDRs` allows only the addresses in `resolverCIDRs`: choose it when Pods query a resolver that is not a `kube-dns` Pod, such as NodeLocal DNSCache on a link-local address. Only `kubeDns` was tried on the lab CNI (Cilium). |
| networkPolicy.dns.resolverCIDRs | list | `["169.254.20.10/32"]` | Resolver addresses for `mode: resolverCIDRs`, at least one IPv4 or IPv6 CIDR such as `169.254.20.10/32`, the default address of NodeLocal DNSCache (check the listen address of your node-local-dns DaemonSet; with kube-proxy in IPVS mode that is the kube-dns Service IP as well). Ignored in `kubeDns` mode. |
| networkPolicy.analyzer.prometheus.enabled | bool | `false` | Let the analyzer, and only the analyzer, send TCP to the in-cluster Prometheus it queries (the metrics source, ADR-0017). Off by default: the analyzer sends Prometheus nothing unless `--prometheus-url` is set; with `--prometheus-cluster` too it probes it at startup and every 10 minutes, and it queries it when the analysis loop (#76, on with `analyzer.analysis.triageModel`) calls the context builder or a read tool. Set it together with the analyzer's metrics source, and allow the analyzer in the ingress policy of that Prometheus too (a NetworkPolicy of the Prometheus namespace; the chart does not touch other namespaces). |
| networkPolicy.analyzer.prometheus.namespace | string | `""` | Namespace of the Prometheus (matched by `kubernetes.io/metadata.name`). Required when `enabled`. |
| networkPolicy.analyzer.prometheus.podLabels | object | `{"app.kubernetes.io/component":"server","app.kubernetes.io/name":"prometheus"}` | Labels of the Prometheus server Pods in that namespace. The default matches the prometheus-community chart. |
| networkPolicy.analyzer.prometheus.port | int | `9090` | TCP port of the Prometheus HTTP API on the Pod (the container port, not the Service port), 1 to 65535. |
| networkPolicy.analyzer.traceCollector.enabled | bool | `false` | Let the analyzer, and only the analyzer, send TCP to the OpenTelemetry collector of its traces (`analyzer.tracing.endpoint`). Off by default: with no endpoint the analyzer sends no trace. Set it together with the endpoint, and allow the analyzer in the ingress policy of the collector too, if it has one. |
| networkPolicy.analyzer.traceCollector.namespace | string | `""` | Namespace of the collector (matched by `kubernetes.io/metadata.name`). Required when `enabled`. |
| networkPolicy.analyzer.traceCollector.podLabels | object | `{"app.kubernetes.io/name":"opentelemetry-collector"}` | Labels of the collector Pods in that namespace. The default matches the opentelemetry-collector chart. |
| networkPolicy.analyzer.traceCollector.port | int | `4317` | TCP port of the collector's OTLP gRPC receiver on the Pod, 1 to 65535. |

### CRDs

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| crd.enabled | bool | `true` | Install the CRDs as templates (Helm does not upgrade or delete CRDs from a `crds/` directory). Set `false` only if you install them another way. |
| crd.keep | bool | `true` | Keep the CRDs, and so every Incident and RemediationRequest, when the release is uninstalled (adds the `helm.sh/resource-policy: keep` annotation). Set `false` to delete all of them with the release: `helm uninstall` then removes the executor and the CRDs together, so delete the requests first (steps 1 to 3 of upgrade and uninstall). |

### RBAC

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| rbac.helpers.enabled | bool | `false` | Install admin, editor and viewer ClusterRoles for each custom resource. Shadewright does not use them, they are for your people. Off by default (least privilege). |

### Executor

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| executor.replicas | int | `1` | Number of Pods. 1 (the default) runs the one Pod. Above 1 the extra Pods are warm standbys: leader election keeps one of them active and the others idle, and when the leader's Pod fails a standby takes over after about one Lease duration (15 seconds by default). A standby costs an idle Pod and adds no throughput. Above 1 the chart also renders a PodDisruptionBudget (`minAvailable: 1`) and a preferred Pod anti-affinity by node, unless `affinity` sets `podAntiAffinity` itself. |
| executor.serviceAccount.annotations | object | `{}` | Further ServiceAccount annotations, for another cloud's workload identity. Keys under `azure.workload.identity/` are refused: use `executor.identity.azure.clientId`. |
| executor.goMemLimit | string | 90 percent of `resources.limits.memory` | Go memory limit (`GOMEMLIMIT`) of the container, such as `100MiB`. Empty (the default) sets 90 percent of `resources.limits.memory`, so it follows that limit: the Go runtime then collects before the heap reaches the container's limit, and a large but bounded parse is not OOM-killed. Set it for a limit the chart cannot read (not an integer or a whole number with a `Ki`, `Mi`, `Gi`, `Ti`, `k`, `K`, `M`, `G` or `T` suffix, such as `1.5Gi`: the chart then refuses to render) or for another margin. |
| executor.resources.limits.cpu | string | `"500m"` | CPU limit (optional; removing it with `null` lets the container use idle CPU). |
| executor.resources.limits.memory | string | `"256Mi"` | Memory limit. Required by the schema (a `null` override is refused), so the container's memory is never unbounded. 256Mi gives headroom for large real GitOps trees (#615); the Git input budgets (#495, #595, #482) stay the bound and were measured against 128Mi, and growth from crafted input multiplies, so the limit is not the defence (#614). |
| executor.resources.requests.cpu | string | `"10m"` | CPU request. |
| executor.resources.requests.memory | string | `"64Mi"` | Memory request. |
| executor.reviewerHints.enabled | bool | `false` | Turns reviewer hints on (ADR-0028): passes `--reviewer-hints`, so the executor reads `/shadewright reanalyze:` comments on the pull requests it opened and creates the check run `shadewright/review` on their heads. Off by default. Required repository rule first: the target branch of the GitOps repository must require the status check `shadewright/review` with the Shadewright GitHub App as its expected source (ADR-0028 section 5). Shadewright cannot verify that rule; without it the check only informs and a hint author can approve their own steered change. The App needs `checks: write`, which every token request names since #539; no Kubernetes RBAC changes. |
| executor.verification.enabled | bool | `false` | Turns outcome verification on (ADR-0030): passes `--verify-outcomes`, so a merged request moves through `WaitingForDelivery` and `Verifying` and ends `Succeeded`, `Failed` or `Inconclusive`, with `Cleared` judged from the verifier's `VerificationReport`. Off by default until a lab run with a release candidate decides the default. On with the verifier deployed (`verifier.watchNamespaces`, or `analyzer.watchNamespaces` when unset, not empty), a request can end `Succeeded`; on without a verifier, no report is written and every merge ends `Inconclusive` (`StaleReport`) or `Failed`, so the circuit breaker does not close again after it trips. Off, a merged request keeps `Merged` with its rollout report (`RolledOut`), and turning it off ends a request in `WaitingForDelivery` or `Verifying` `Inconclusive` (`VerificationDisabled`). |
| executor.verification.nodeGrace | string | `"1m"` | How long past the stable window a verification verdict waits, so that the pods of a node that stopped reporting are marked not Ready first (ADR-0030 section 4): passes `--node-grace`. Set it to at least the cluster's node monitor grace period (`--node-monitor-grace-period` of kube-controller-manager, under a minute by default). It is part of the verification timeout, which is 21 minutes for a memory fix with the defaults. A Go duration such as `1m` or `90s`. |
| executor.extraArgs | list | `[]` | Further command line flags, each `--name` or `--name=value`. The chart refuses the flags it sets itself, so a free list cannot turn off leader election, metrics authentication or the watched namespaces: `--leader-elect`, `--health-probe-bind-address`, `--metrics-bind-address`, `--metrics-secure`, `--metrics-cert-path`, `--metrics-cert-name`, `--metrics-cert-key`, `--namespace`, `--watch-namespaces`, `--cluster-id`, `--max-incidents`, `--reviewer-hints` (use `reviewerHints.enabled`), `--node-grace` (use `verification.nodeGrace`) and `--verify-outcomes` (use `verification.enabled`). |
| executor.metricsTLS.secretName | string | `""` | Name of an existing Secret of type `kubernetes.io/tls` (keys `tls.crt` and `tls.key`) in the release namespace that holds the serving certificate of the metrics endpoint. Empty (the default) keeps the certificate the binary generates for itself. Set, the chart mounts the Secret read-only and passes `--metrics-cert-path`; the key must be in the Secret, never in `values`. Needs `metrics.enabled` and `metrics.secure`. Issuance and renewal stay with you. |
| executor.podLabels | object | `{}` | Further Pod labels. The chart refuses `control-plane`, `app.kubernetes.io/*`, `helm.sh/*` and `azure.workload.identity/*`: it sets them itself (the identity labels come from `identity.azure.clientId`). |
| executor.podAnnotations | object | `{}` | Further Pod annotations. |
| executor.nodeSelector | object | `{}` | Node selector of the Pod, at most 32 labels. |
| executor.tolerations | list | `[]` | Tolerations of the Pod, at most 32 Kubernetes toleration objects. |
| executor.affinity | object | `{}` | Affinity of the Pod, a Kubernetes affinity object (`nodeAffinity`, `podAffinity`, `podAntiAffinity`). |
| executor.topologySpreadConstraints | list | `[]` | Topology spread constraints of the Pod, at most 8 Kubernetes `topologySpreadConstraints` objects. |
| executor.priorityClassName | string | `""` | Priority class of the Pod. Empty uses the cluster default. |
| executor.imagePullSecrets | list | `[]` | Image pull secrets of this component only, added to the global `imagePullSecrets`, a list of `{name: <secret>}` (at most 16). |

### Identity

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| executor.identity.azure.clientId | string | empty (no identity) | Client ID (a UUID, not a secret) of the executor's Azure managed identity for workload identity. Sets the ServiceAccount annotation `azure.workload.identity/client-id` and the Pod label `azure.workload.identity/use`. Empty sets neither: the executor then cannot sign for the GitHub App, so a request that reaches the Git step is retried quietly and stays Pending. Must differ from the analyzer's, or the chart refuses to render (separate identities, never one for both). |
| analyzer.identity.azure.clientId | string | empty (no identity) | Client ID (a UUID, not a secret) of the analyzer's Azure managed identity, for the analyzer's Entra ID tokens: the model call (Cognitive Services audience) and, with `analyzer.analysis.recordStore.azureBlob.account`, the analysis records (Storage audience). Sets the ServiceAccount annotation `azure.workload.identity/client-id` and the Pod label `azure.workload.identity/use`. Needed when `analyzer.analysis.triageModel` is set; leave it empty otherwise. It must differ from the executor's or the chart refuses to render (separate identities, never one for both). |

### Analyzer

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| analyzer.replicas | int | `1` | Number of Pods. 1 (the default) runs the one Pod. Above 1 the extra Pods are warm standbys: leader election keeps one of them active and the others idle, and when the leader's Pod fails a standby takes over after about one Lease duration (15 seconds by default). A standby costs an idle Pod and adds no throughput. Above 1 the chart also renders a PodDisruptionBudget (`minAvailable: 1`) and a preferred Pod anti-affinity by node, unless `affinity` sets `podAntiAffinity` itself. |
| analyzer.watchNamespaces | list | empty (no namespace) | Unique namespace names, at most 256. The analyzer reads events, Pods and workloads only in these namespaces: the chart passes them as `--watch-namespaces` and binds the read-only watch ClusterRole with one RoleBinding in each (`kube-system` included only if you list it). Empty (the default) watches no namespace, never all of them: the analyzer starts, is Ready and records no Incident, and no RoleBinding is created. `*` is refused. |
| analyzer.maxIncidents | string | null (the binary's default, 500) | Most Incidents kept in the operator namespace (`--max-incidents`), 1 or more. Null (unset) keeps the binary's default of 500. |
| analyzer.podLogs.enabled | bool | `true` | Give the analyzer the pod logs source (`--pod-logs`) and its grant: `get` on `pods/log` in the watch ClusterRole, bound only in the watched namespaces. `false` passes `--pod-logs=false` and renders no `pods/log` rule. With more than 256 watched namespaces the analyzer refuses to start unless this is `false`. |
| analyzer.modules.flux.enabled | bool | `false` | Grant the analyzer `list` and `watch` on Flux `kustomizations`, `helmreleases` and `gitrepositories` in the watched namespaces (ClusterRole `analyzer-watch-flux`), for the Flux module's symptom sources. Off by default (least privilege): no module used these reads before, so leaving it off stops nothing that worked. Turn it on with the `flux` integration in `spec.modules`. |
| analyzer.modules.keda.enabled | bool | `true` | Grant the analyzer `get`, `list` and `watch` on KEDA `scaledobjects`, and `list` and `watch` on `scaledjobs`, in the watched namespaces (ClusterRole `analyzer-watch-keda`), for the KEDA module's symptom sources. On by default: the KEDA HPA source already used `get` on `scaledobjects` to verify the owner of an HPA, so defaulting it off would quietly change Incidents of an enabled module (about the HPA instead of the ScaledObject). `false` renders no KEDA rule: use it when KEDA is not installed. |
| analyzer.modules.kyverno.enabled | bool | `false` | Grant the analyzer `list` and `watch` on Kyverno `policies` and `wgpolicyk8s.io` `policyreports` in the watched namespaces (ClusterRole `analyzer-watch-kyverno`), for the Kyverno module's namespaced symptom sources. Off by default (least privilege), as for `flux`. |
| analyzer.modules.kyverno.clusterWide.enabled | bool | `false` | Grant the analyzer `list` and `watch` on Kyverno `clusterpolicies` and `wgpolicyk8s.io` `clusterpolicyreports` cluster-wide (ClusterRole and ClusterRoleBinding `analyzer-kyverno-cluster`), for the Kyverno module's cluster-scoped symptom sources. Off by default: it is the one read of an add-on's kinds outside the watched namespaces, and a ClusterPolicyReport names resources of every namespace. Independent of `kyverno.enabled`: each grants its own kinds. Off, the analyzer leaves those kinds out on purpose: no report in `SymptomSourcesNotWatched` and no `shadewright_analyzer_symptom_source_watched` series, so an alert on `0` stays quiet. |
| analyzer.prometheus.url | string | `""` | Base URL of the Prometheus query API the analyzer reads metrics from (`--prometheus-url`), `http` or `https`, with no user info, query or fragment, such as `http://prometheus-server.prometheus.svc`. Empty (the default) passes no flag: no metrics. Open the NetworkPolicy egress to Prometheus too (`networkPolicy.analyzer.prometheus`). |
| analyzer.prometheus.cluster | string | `""` | The `cluster` label value of this cluster's series (`--prometheus-cluster`), for a Prometheus shared by several clusters. Empty passes no flag. Used only with `analyzer.prometheus.url`. |
| analyzer.tracing.endpoint | string | `""` | OTLP gRPC endpoint of an OpenTelemetry collector for the analysis traces (one trace per analysis: phases, model calls with tokens, tool calls; IDs, counts and fixed labels only, never prompt, log or model text), such as `http://otel-collector.observability.svc:4317` (`http` sends plaintext gRPC, `https` TLS), with an explicit port. Sets `OTEL_TRACES_EXPORTER=otlp` and `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`. Empty (the default): no traces and no exporter. With the NetworkPolicies on, the chart refuses to render it unless `networkPolicy.analyzer.traceCollector.enabled` opens the egress to the collector. |
| analyzer.analysis.triageModel | string | `""` | Model ID and version the triage deployment of `RemediationSettings` serves, as its replies name it, such as `gpt-5.4-mini-2026-03-17` (`--triage-model`). A reply naming another model is refused (ADR-0023). Empty (the default) passes no analysis flag: the analysis is off, no model is called and no Azure token is requested. Set, it needs `analyzer.identity.azure.clientId` and `analyzer.analysis.endpointSuffixes` (the chart refuses to render without them). Until the evaluation gate (#78) exists, turn it on only for an evaluation run in the lab. |
| analyzer.analysis.escalationModel | string | `""` | Model ID and version the escalation deployment serves (`--escalation-model`). Empty: escalation is off whatever `RemediationSettings` names, and an escalation signal hands the incident off. Used only with `triageModel`. |
| analyzer.analysis.endpointSuffixes | list | `[]` | Host suffixes a model endpoint in `RemediationSettings` must end with or, without the leading dot, equal (`--model-endpoint-suffixes`), each a dot and two or more lower-case DNS labels, at most 16. Required with `triageModel`: there is no default, so no Azure AI host is allowed unless listed. Any resource under an allowed suffix receives the context and a token, so list your own resource, such as `.<resource>.services.ai.azure.com`, which allows that host only. |
| analyzer.analysis.actionPolicies | object | `{}` | The RemediationPolicy a proposed RemediationRequest of each action type refers to (`--action-policies`), a map from `AdjustKedaMaxReplicas` or `SetContainerMemoryLimit` to a policy name, such as `{SetContainerMemoryLimit: memory-limits}`. An action type without one is never proposed. Used only with `triageModel`. With at least one name, the chart grants the analyzer `get` on exactly these RemediationPolicies (ClusterRole and ClusterRoleBinding `analyzer-policy-reader`, `resourceNames` set to the names, no `list` or `watch`), so the context shows the model the bounds of the policy that will decide its proposal; the policy still decides. Without a name there is no such rule. |
| analyzer.analysis.recordStore.azureBlob.account | string | `""` | Storage account name (3 to 24 lower-case letters and digits) of the Blob Storage container that keeps one redacted record of each analysis (`--analysis-store-account`, ADR-0015). Records are written create-only (`If-None-Match: *`) to `https://<account>.blob.core.windows.net` with the analyzer's workload identity (a token for the Storage audience, no key or SAS); the identity needs a role that may write blobs in that container, such as the analysis writer role of `shadewright-azure/foundation`. Empty (the default) writes no record. Used only with `triageModel` (the chart refuses to render it without one). |
| analyzer.analysis.recordStore.azureBlob.container | string | `"analyses"` | Container of the analysis records in `account` (`--analysis-store-container`), `analyses` in the reference lab. Rendered only with `account`. |
| analyzer.serviceAccount.annotations | object | `{}` | Further ServiceAccount annotations, for another cloud's workload identity. Keys under `azure.workload.identity/` are refused: use `analyzer.identity.azure.clientId`. |
| analyzer.goMemLimit | string | 90 percent of `resources.limits.memory` | Go memory limit (`GOMEMLIMIT`) of the container, such as `100MiB`. Empty (the default) sets 90 percent of `resources.limits.memory`, so it follows that limit: the Go runtime then collects before the heap reaches the container's limit, and a large but bounded parse is not OOM-killed. Set it for a limit the chart cannot read (not an integer or a whole number with a `Ki`, `Mi`, `Gi`, `Ti`, `k`, `K`, `M`, `G` or `T` suffix, such as `1.5Gi`: the chart then refuses to render) or for another margin. |
| analyzer.resources.limits.cpu | string | `"500m"` | CPU limit (optional; removing it with `null` lets the container use idle CPU). |
| analyzer.resources.limits.memory | string | `"128Mi"` | Memory limit. Required by the schema (a `null` override is refused), so the container's memory is never unbounded. |
| analyzer.resources.requests.cpu | string | `"10m"` | CPU request. |
| analyzer.resources.requests.memory | string | `"64Mi"` | Memory request. |
| analyzer.extraArgs | list | `[]` | Further command line flags, each `--name` or `--name=value`. The chart refuses the flags it sets itself, so a free list cannot turn off leader election, metrics authentication or the watched namespaces: `--leader-elect`, `--health-probe-bind-address`, `--metrics-bind-address`, `--metrics-secure`, `--metrics-cert-path`, `--metrics-cert-name`, `--metrics-cert-key`, `--namespace`, `--watch-namespaces`, `--cluster-id`, `--max-incidents`, the context source flags and the analysis flags. |
| analyzer.metricsTLS.secretName | string | `""` | Name of an existing Secret of type `kubernetes.io/tls` (keys `tls.crt` and `tls.key`) in the release namespace that holds the serving certificate of the metrics endpoint. Empty (the default) keeps the certificate the binary generates for itself. Set, the chart mounts the Secret read-only and passes `--metrics-cert-path`; the key must be in the Secret, never in `values`. Needs `metrics.enabled` and `metrics.secure`. Issuance and renewal stay with you. |
| analyzer.podLabels | object | `{}` | Further Pod labels. The chart refuses `control-plane`, `app.kubernetes.io/*`, `helm.sh/*` and `azure.workload.identity/*`: it sets them itself (the identity labels come from `identity.azure.clientId`). |
| analyzer.podAnnotations | object | `{}` | Further Pod annotations. |
| analyzer.nodeSelector | object | `{}` | Node selector of the Pod, at most 32 labels. |
| analyzer.tolerations | list | `[]` | Tolerations of the Pod, at most 32 Kubernetes toleration objects. |
| analyzer.affinity | object | `{}` | Affinity of the Pod, a Kubernetes affinity object (`nodeAffinity`, `podAffinity`, `podAntiAffinity`). |
| analyzer.topologySpreadConstraints | list | `[]` | Topology spread constraints of the Pod, at most 8 Kubernetes `topologySpreadConstraints` objects. |
| analyzer.priorityClassName | string | `""` | Priority class of the Pod. Empty uses the cluster default. |
| analyzer.imagePullSecrets | list | `[]` | Image pull secrets of this component only, added to the global `imagePullSecrets`, a list of `{name: <secret>}` (at most 16). |

### Verifier

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| verifier.replicas | int | `1` | Number of Pods. 1 (the default) runs the one Pod. Above 1 the extra Pods are warm standbys: leader election keeps one of them active and the others idle, and when the leader's Pod fails a standby takes over after about one Lease duration (15 seconds by default). A standby costs an idle Pod and adds no throughput. Above 1 the chart also renders a PodDisruptionBudget (`minAvailable: 1`) and a preferred Pod anti-affinity by node, unless `affinity` sets `podAntiAffinity` itself. |
| verifier.watchNamespaces | string | `null`, so `analyzer.watchNamespaces` | Namespaces whose pods and workloads the verifier reads (`--watch-namespaces`): `list` on `pods`, `get` on `deployments`, `replicasets` and `statefulsets`, through one read-only RoleBinding in each namespace (each must exist). `null` (the default) uses `analyzer.watchNamespaces`; a list, an empty one included, is used as it is, and an empty list watches none (never all). Names must be unique, at most 256, DNS labels (`*` is refused). The verifier is deployed only when this list (after the `null` fallback) is not empty: with none, the chart renders no verifier Deployment, ServiceAccount, Role, binding, PDB, metrics Service or metrics authentication subject, and a merged request can end only `Inconclusive`, never `Succeeded`. There is no separate `enabled` key. |
| verifier.goMemLimit | string | 90 percent of `resources.limits.memory` | Go memory limit (`GOMEMLIMIT`) of the container, such as `100MiB`. Empty (the default) sets 90 percent of `resources.limits.memory`, so it follows that limit. Set it for a limit the chart cannot read (not an integer or a whole number with a `Ki`, `Mi`, `Gi`, `Ti`, `k`, `K`, `M`, `G` or `T` suffix: the chart then refuses to render) or for another margin. |
| verifier.resources.limits.cpu | string | `"500m"` | CPU limit (optional; removing it with `null` lets the container use idle CPU). |
| verifier.resources.limits.memory | string | `"128Mi"` | Memory limit. Required by the schema (a `null` override is refused), so the container's memory is never unbounded. |
| verifier.resources.requests.cpu | string | `"10m"` | CPU request. |
| verifier.resources.requests.memory | string | `"64Mi"` | Memory request. |
| verifier.extraArgs | list | `[]` | Further command line flags, each `--name` or `--name=value`. The chart refuses the flags it sets itself, so a free list cannot turn off leader election, metrics authentication or the watched namespaces: `--leader-elect`, `--health-probe-bind-address`, `--metrics-bind-address`, `--metrics-secure`, `--metrics-cert-path`, `--metrics-cert-name`, `--metrics-cert-key`, `--namespace` and `--watch-namespaces`. |
| verifier.metricsTLS.secretName | string | `""` | Name of an existing Secret of type `kubernetes.io/tls` (keys `tls.crt` and `tls.key`) in the release namespace that holds the serving certificate of the metrics endpoint. Empty (the default) keeps the certificate the binary generates for itself. Set, the chart mounts the Secret read-only and passes `--metrics-cert-path`; the key must be in the Secret, never in `values`. Needs `metrics.enabled` and `metrics.secure`. Issuance and renewal stay with you. |
| verifier.podLabels | object | `{}` | Further Pod labels. The chart refuses `control-plane`, `app.kubernetes.io/*`, `helm.sh/*` and `azure.workload.identity/*`: it sets them itself, and the verifier has no cloud identity. |
| verifier.podAnnotations | object | `{}` | Further Pod annotations. |
| verifier.nodeSelector | object | `{}` | Node selector of the Pod, at most 32 labels. |
| verifier.tolerations | list | `[]` | Tolerations of the Pod, at most 32 Kubernetes toleration objects. |
| verifier.affinity | object | `{}` | Affinity of the Pod, a Kubernetes affinity object (`nodeAffinity`, `podAffinity`, `podAntiAffinity`). |
| verifier.topologySpreadConstraints | list | `[]` | Topology spread constraints of the Pod, at most 8 Kubernetes `topologySpreadConstraints` objects. |
| verifier.priorityClassName | string | `""` | Priority class of the Pod. Empty uses the cluster default. |
| verifier.imagePullSecrets | list | `[]` | Image pull secrets of this component only, added to the global `imagePullSecrets`, a list of `{name: <secret>}` (at most 16). |

### Tests

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| tests.image.repository | string | `"docker.io/curlimages/curl"` | Repository of the image of the `helm test` Pod. It needs `curl`. Rendered only with `metrics.enabled`. |
| tests.image.digest | string | `"sha256:3fb7e13e5d179a845c97d78204f3f1064d1907037f26a3ebf91710571151fd93"` | Digest of the test image (curl 8.23.0), required: `sha256:` and 64 lower case hex digits. |
| tests.imagePullSecrets | list | `[]` | Image pull secrets of the test Pod only, added to the global `imagePullSecrets`, a list of `{name: <secret>}` (at most 16). |

### Settings

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| settings.create | bool | `false` | Create the RemediationSettings object `default` from `spec`, only when it does not exist (a post-install and post-upgrade hook that checks with `lookup`). Helm never changes or deletes it afterwards, so a pause set by hand (`spec.paused: true`) survives every upgrade; change the object with kubectl or GitOps. `lookup` finds nothing under `helm template` and `--dry-run`, so those always render it. Helm does not track hook objects: the object is not deleted when the release is uninstalled. Requires a non-empty `spec`. |
| settings.spec | object | `{}` | Spec of the RemediationSettings object. Required and non-empty when `create` is true (the schema enforces it). The fields are in the API reference, see the README for an example. |

