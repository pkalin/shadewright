---
title: Configuration reference
---

# Configuration reference

Everything an operator can set today, checked against `cmd/main.go` (the executor), `cmd/analyzer/main.go`, `internal/cmdutil` and `config/` at v0.1.0. The Helm chart in `dist/chart` maps its values onto the settings below; see [Helm chart](#helm-chart). Objects and fields of the custom resources are in the [API reference](api.md). See also [compatibility](compatibility.md), [upgrade and uninstall](../upgrade-uninstall.md), [identity rotation](../identity-rotation.md) and [data handling](../data-handling.md). Metric names are in the [metrics reference](metrics.md).

## Components and images

| Component | Binary | Image | Role |
| --- | --- | --- | --- |
| Executor | `/manager` | `ghcr.io/pkalin/shadewright-executor` | Reconciles `RemediationRequest` objects in the operator namespace, decides with the policy, acts through Git |
| Analyzer | `/analyzer` | `ghcr.io/pkalin/shadewright-analyzer` | Collector, Incident controller, platform and module detection. Creates Incidents and, once the analysis is built, requests. Never acts on them |

Both images are distroless, run as user 65532, and are published for linux/amd64 and linux/arm64. The manifests in `config/manager` use the placeholder `controller:latest` for both, so every install replaces it (the published chart runs the release's signed images by digest; a chart from a source checkout runs the `appVersion` tag). Pin by digest; the [getting started](../getting-started.md#2-install-in-observe-only-mode) page has the v0.1.0 digests and an overlay that does it. Both Deployments run one replica with `--leader-elect`, restricted Pod Security settings (non-root, `RuntimeDefault` seccomp, read-only root filesystem, all capabilities dropped), requests of 10m CPU and 64Mi, and limits of 500m CPU and 128Mi.

## Helm chart

The chart is `dist/chart`. It was generated with Kubebuilder's `helm/v2-alpha` plugin (Kubebuilder v4.16.0) and then adjusted by hand: the plugin renders one manager, so the two Deployments, their ServiceAccounts and the RBAC are written out. **Do not regenerate it with the plugin**; `make test-chart` renders it and fails if its RBAC, NetworkPolicies, Services or CRDs differ from `kustomize build config/default` plus `config/watch` (any extra or missing verb, resource or subject), so a change to `config/rbac` needs the same change in `dist/chart/templates/rbac`. The Chart workflow runs these checks on every pull request, but they are not required for merge yet (the ruleset requires other checks): a red chart check does not block a merge, so read it. It is published as `oci://ghcr.io/pkalin/charts/shadewright` by the release tag workflow, so the first published version is the one after v0.1.0; until then install from a clone with `./dist/chart`.

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
```

Each component also takes the standard Pod values, bounded by the schema: `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName`, `imagePullSecrets` (added to the global list) and `podAnnotations`. Every object carries the `app.kubernetes.io/*` labels and `helm.sh/chart`. The chart passes `ct lint` and `ct install` (chart-testing, with the default values on Kind) and `helm lint --strict` in CI.

Every value, with its default, its allowed values and its security effect, is in the [chart README](https://github.com/pkalin/shadewright/blob/main/dist/chart/README.md). It is generated from the comments in `dist/chart/values.yaml` with helm-docs (`make chart-docs`), and CI fails if the two differ or a value has no description, so there is one source. `values.schema.json` is the authority on which values are required.

The chart is signed with cosign keyless (GitHub OIDC) by digest in the publish job, like the images. Verify it before installing; the identity is the chart workflow on a release tag of this repository:

```bash
cosign verify \
  --certificate-identity-regexp '^https://github\.com/pkalin/shadewright/\.github/workflows/chart\.yml@refs/tags/v[0-9].*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/pkalin/charts/shadewright@sha256:<chart digest>
```

The digest is in the run summary of the Chart workflow, or from `helm pull oci://ghcr.io/pkalin/charts/shadewright --version <version>` (it prints `Digest:`). Image signatures are verified the same way, with the image name and the `images\.yml` workflow in the identity.

`metrics.serviceMonitor.enabled` (off by default, as `config/default` leaves `config/prometheus` out) installs a Prometheus Operator `ServiceMonitor` per component; Prometheus then needs the `metrics-reader` ClusterRole for its ServiceAccount and the label `metrics: enabled` on its namespace. Not in the chart: cert-manager and webhooks (none are installed), and the `Namespace` (use `--create-namespace`). The security contexts, probes, ports and the replica count (1, with leader election) are fixed to what `config/` sets.

## Flags

Flags are passed as container args. A flag given twice takes its last value. The shipped manifests set the ones marked in the last column.

### Both binaries

| Flag | Default | Meaning | `config/` sets |
| --- | --- | --- | --- |
| `--namespace` | `$POD_NAMESPACE` | The operator namespace. Required: the binary exits if both are empty | `POD_NAMESPACE` from the downward API |
| `--leader-elect` | `false` | Only one replica is active. Leases: `shadewright-executor-leader-election`, `shadewright-analyzer-leader-election` | on |
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

`config/default` installs no admission webhook (the webhook and cert-manager sections are commented out), so the webhook flags have no use in the shipped manifests.

### Analyzer only

| Flag | Default | Meaning | `config/` sets |
| --- | --- | --- | --- |
| `--watch-namespaces` | empty (no namespace) | Comma-separated namespaces whose events, pods and workloads are read. Each must be a valid namespace name (`*` is refused); spaces and duplicates are dropped. An empty list watches no namespace, never "all namespaces": the analyzer starts and is Ready, starts no collector, records no Incident, logs `Watching no namespaces` and reports the condition `NoWatchedNamespaces` True in the settings status | `demo` (the lab) |
| `--cluster-id` | none, **required** | See [cluster ID](#cluster-id) | `lab` (the lab) |
| `--max-incidents` | `500` | Most Incidents kept in the operator namespace; at least 1. At the cap a new fingerprint is not stored, is counted in `shadewright_incident_observations_refused_total` and reported in a Warning event (at most one per 10 minutes) | unset |

### Environment

| Variable | Used by | Meaning |
| --- | --- | --- |
| `POD_NAMESPACE` | both | Default of `--namespace` |
| `POD_NAME` | analyzer | The Pod the cap Warning event is about. Without it (running outside a cluster) the cap is only logged and counted |

### Values with no flag

These are compiled in. Changing one needs a code change.

| Value | Setting |
| --- | --- |
| Incident debounce, cooldown, analysis timeout | 45 s, 15 min, 10 min |
| Analysis due check | every 10 s |
| Executor: policy check, cleanup and one Git write step | 2 min each |
| Executor: time from `PolicyChecked` to an open pull request | 30 min, then `Expired` |
| Executor: wait for KEDA to report a `ScaledObject` | 10 min, then `NeedsIntervention` |
| Executor: freshness recheck of a waiting request | every 5 min and on any change to the request |
| Source mapping limits | directories 8 deep, 256 files, 512 reads, 1 MiB per file |

## Cluster ID

`--cluster-id` is hashed into every Incident fingerprint, so Incidents from two clusters never merge. It is 1 to 63 letters, digits, `.`, `_` or `-`, starting and ending with a letter or digit. Give each cluster its own. **Changing it starts every Incident afresh**, so choose it once. The manifests ship `lab`. The Helm chart takes it from the value `clusterId`; left empty, it uses the UID of the `kube-system` namespace, read with `lookup` at install and upgrade (stable and unique for the life of the cluster). `helm template`, `helm lint` and a client-side `--dry-run` fail with a message until `clusterId` is set. GitOps tools and client-side renders (`helm template`, helm diff, Argo CD, Flux with a dry-run) ask no cluster, and an installer without `get` on the `kube-system` namespace cannot read it, so each of those must set `clusterId` explicitly. Moving from the default to a value of your own also starts every Incident afresh. It is an ID, never a display name.

## Namespaces and RoleBindings

Three namespaces matter:

- **Operator namespace** (`shadewright-system`: the `namespace` of `config/default`, with the name prefix `shadewright-`). It holds the Deployments, ServiceAccounts, Incidents, RemediationRequests and the analysis budget. The executor reconciles requests only here; a request anywhere else is never seen. A policy must list this namespace in `requestNamespaces`, or the request is `Denied` (`RequestNamespaceNotAllowed`). Renaming it means changing the federated credential subject, every RoleBinding subject, and `--namespace`.
- **Watched namespaces**, set by `--watch-namespaces`. The analyzer's cache is limited to them, so it asks for no more than its RBAC grants. None by default in the chart: an empty list watches nothing.
- **Target namespaces**, set by the policy (`targets.namespaces`). Independent of the watched ones: a policy only allows what a request names.

Each watched namespace needs a RoleBinding that binds the ClusterRole `shadewright-analyzer-watch-role` to the ServiceAccount `shadewright-analyzer` of the operator namespace. `config/watch` is the example and holds only the lab's `demo` binding; it is built on its own because `config/default` sets every namespace to the operator's. Without the binding the API server refuses the analyzer's reads there and nothing is observed in that namespace. A repository test checks that the lab's `--watch-namespaces` and `config/watch` agree; it cannot check yours.

The permissions, as installed:

| Identity | Scope | Grants |
| --- | --- | --- |
| Analyzer, each watched namespace | RoleBinding to `analyzer-watch-role` | `get`, `list`, `watch` Pods and `events.k8s.io` Events; `get` Deployments, ReplicaSets, StatefulSets |
| Analyzer, cluster | `analyzer-role` ClusterRole | `list` Nodes; `get`, `list`, `watch` `remediationsettings`; `update` `remediationsettings/status` |
| Analyzer, operator namespace | `analyzer-role` Role | `get`, `update` the ConfigMap `shadewright-analysis-budget` only (no create); `create`, `get`, `list`, `watch` Incidents and RemediationRequests; `update` `incidents/status`; its own Lease, and `create`, `patch` on core Events (leader election) |
| Executor, cluster | `executor-role` ClusterRole | `get` only: Deployments, StatefulSets, KEDA `scaledobjects`, Flux `kustomizations` and `gitrepositories`, `remediationpolicies`, `remediationsettings` |
| Executor, operator namespace | `executor-role` Role | `get`, `list`, `patch`, `watch` RemediationRequests; `get`, `patch`, `update` `remediationrequests/status`; its own Lease, and `create`, `patch` on core Events (leader election). After v0.1.0 (#265, on main): `get` on the one Incident a request names, to verify the name shown in the pull request body |
| Metrics | `metrics-auth-role`, bound to both ServiceAccounts | `create` TokenReviews and SubjectAccessReviews |

Neither identity can read Secrets, and neither is `cluster-admin`. `config/rbac` also installs admin, editor and viewer ClusterRoles for each custom resource; Shadewright does not use them, they are for your people.

The ConfigMap `shadewright-analysis-budget` holds the analyses started on the current UTC day. It ships without data on purpose; keep `data` out of anything you apply, so a GitOps sync never resets the count. If it is deleted the analyzer cannot recreate it, and nothing is due for analysis until you do (`BudgetNotFound`).

## RemediationSettings

One cluster-scoped object named `default` (field details in the [API reference](api.md)). With the Helm chart, `settings.create=true` creates it from `settings.spec` only when it does not exist (a hook that checks with `lookup`, which finds nothing under `helm template` and `--dry-run`, so those always show it); Helm never changes or deletes it afterwards, so a `spec.paused: true` set by hand survives every `helm upgrade`. Change it with kubectl or GitOps:

| Field | Default | Meaning |
| --- | --- | --- |
| `spec.paused` | `false` | Stops every write: pull requests, closes, finalizer cleanup. Analysis continues. See [emergency stop](../emergency.md) |
| `spec.maxAnalysesPerDay` | `100` | Model-backed analyses per UTC day, 0 to 10000. `0` marks every Incident not due (`AnalysisDisabled`). The model call itself is not built yet |
| `spec.maxConcurrentRemediations` | `1` | Requests active at once (1 to 20), in the operator namespace: from `PolicyChecked` until `Merged` or a final phase, and while a deleted one still closes its pull request. A further request the policy allows stays `Pending` with the condition `Queued` (`ConcurrencyLimitReached`); a refusal at the first check is never held back, but a request already `Queued` is decided again only once the limits let it through. See [troubleshooting](../troubleshooting.md#limits) |
| `spec.repositories` | none | Up to 16 repositories Shadewright may open pull requests in. URLs are unique |
| `spec.repositories[].url` | required | GitHub: `https://github.com/<owner>/<repo>`, lower case, no `.git`. Must equal the Flux `GitRepository` URL byte for byte, or the request is `Denied` (`RepositoryNotConfigured`) |
| `spec.repositories[].targetBranch` | `main` | The only branch pull requests may target. Must equal the branch Flux tracks |
| `spec.repositories[].provider` | required | `GitHub` is the only value |
| `spec.repositories[].github` | required for GitHub | `appClientID`, `installationID` and `signer.azureKeyVault.keyID` (a key URL such as `https://<vault>.vault.azure.net/keys/<name>`, with an optional 32-digit version) |
| `spec.modules.platforms`, `spec.modules.integrations` | empty | Modules enabled by name. A module contributes only if it is enabled here and detected. `generic` is always on and is not listed. Compiled in at v0.1.0: platform `aks`; integrations `cloudflared`, `flux`, `keda`, `kyverno`, `secrets-store-csi-driver`. Added since: platforms `eks` and `gke`, which only detect (no diagnosis rules). A name no module has is accepted and reported in the `UnknownModules` condition |

An action contributed by an integration module (`AdjustKedaMaxReplicas` needs `keda`) is `Denied` with `ModuleNotEnabled` unless the module is listed. The settings `status` is a report written by the analyzer; no component reads it to decide anything.

## Metrics endpoint

`config/default` patches both Deployments to serve metrics on `:8443` over HTTPS and creates the Services `shadewright-executor-metrics-service` and `shadewright-analyzer-metrics-service` (port `https`, 8443). With `--metrics-secure=true` each request is authenticated by a TokenReview and authorized by a SubjectAccessReview. A scraper therefore needs a ServiceAccount token, and that identity needs the `get` verb on the non-resource URL `/metrics`: the ClusterRole `shadewright-metrics-reader` grants it, and **nothing binds it by default**, so bind it to your Prometheus ServiceAccount.

The shipped NetworkPolicies also gate the network: the namespace is default-deny for ingress and egress, and ingress to port 8443 is allowed only from namespaces labeled `metrics: enabled`. Egress for both components is limited to cluster DNS and TCP 443 and 6443 (the Kubernetes API, Git host and Key Vault are on those ports).

The metrics each component serves are listed in the [metrics reference](metrics.md).

The certificate is self-signed unless you set `--metrics-cert-path` (the cert-manager overlay is commented out in `config/default`). `config/prometheus/monitor.yaml` is a `ServiceMonitor` for the Prometheus Operator, not included by default; it scrapes with the Pod's ServiceAccount token and `insecureSkipVerify: true`, so use a real certificate and `serverName` if you care about that. Do not use `--metrics-secure=false` outside a test.

## Workload identity (Azure)

Only the executor needs a cloud identity today: it signs the GitHub App's JWT with a non-exportable Key Vault key. The key never enters the cluster. On AKS with workload identity:

- The ServiceAccount `shadewright-executor` carries the annotation `azure.workload.identity/client-id`. In `config/rbac/executor/service_account.yaml` it is the placeholder `<executor-managed-identity-client-id>` and **must be replaced**; the ID is not a secret, but it belongs in your overlay, not in a public repository. The Deployment's Pods already carry the label `azure.workload.identity/use: "true"`.
- The managed identity needs a federated credential for the subject `system:serviceaccount:shadewright-system:shadewright-executor` and audience `api://AzureADTokenExchange`, and only the `sign` operation on the key. If you rename the namespace or either ServiceAccount, the credential must change with it.
- The analyzer has no identity annotation or label. The model call that will need one is not built (**planned**, [ADR-0023](../adr/0023-how-the-analyzer-calls-the-model.md)).

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
