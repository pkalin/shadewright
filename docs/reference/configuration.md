---
title: Configuration reference
---

# Configuration reference

Everything an operator can set today, checked against `cmd/main.go` (the executor), `cmd/analyzer/main.go`, `internal/cmdutil` and `config/` at v0.1.0. The Helm chart in `dist/chart` ([#83](https://github.com/pkalin/shadewright/issues/83)) maps its values onto the settings below; see [Helm chart](#helm-chart). Objects and fields of the custom resources are in the [API reference](api.md). See also [compatibility](compatibility.md), [upgrade and uninstall](../upgrade-uninstall.md), [identity rotation](../identity-rotation.md) and [data handling](../data-handling.md). Metric names are in the [metrics reference](metrics.md).

## Components and images

| Component | Binary | Image | Role |
| --- | --- | --- | --- |
| Executor | `/manager` | `ghcr.io/pkalin/shadewright-executor` | Reconciles `RemediationRequest` objects in the operator namespace, decides with the policy, acts through Git |
| Analyzer | `/analyzer` | `ghcr.io/pkalin/shadewright-analyzer` | Collector, Incident controller, platform and module detection. Creates Incidents and, once the analysis is built, requests. Never acts on them |

Both images are distroless, run as user 65532, and are published for linux/amd64 and linux/arm64. The manifests in `config/manager` use the placeholder `controller:latest` for both, so every install replaces it (the chart refuses to render without a digest). Pin by digest; the [getting started](../getting-started.md#2-install-in-observe-only-mode) page has the v0.1.0 digests and an overlay that does it. Both Deployments run one replica with `--leader-elect`, restricted Pod Security settings (non-root, `RuntimeDefault` seccomp, read-only root filesystem, all capabilities dropped), requests of 10m CPU and 64Mi, and limits of 500m CPU and 128Mi.

## Helm chart

The chart is `dist/chart`. It was generated with Kubebuilder's `helm/v2-alpha` plugin (Kubebuilder v4.16.0) and then adjusted by hand: the plugin renders one manager, so the two Deployments, their ServiceAccounts and the RBAC are written out. **Do not regenerate it with the plugin**; `make test-chart` renders it and fails if its RBAC, NetworkPolicies, Services or CRDs differ from `kustomize build config/default` plus `config/watch` (any extra or missing verb, resource or subject), so a change to `config/rbac` needs the same change in `dist/chart/templates/rbac`. It is published as `oci://ghcr.io/pkalin/charts/shadewright` by the release tag workflow, so the first published version is the one after v0.1.0; until then install from a clone with `./dist/chart`.

```bash
helm install shadewright oci://ghcr.io/pkalin/charts/shadewright --version <version> \
  --namespace shadewright-system --create-namespace -f my-values.yaml
```

Use the release name `shadewright` and the namespace `shadewright-system`: the names of the ServiceAccounts, and so the subject of the federated credential, are the release name plus `-executor` and `-analyzer`. The Leases and the analysis budget ConfigMap keep the fixed names the binaries expect whatever the release is called. `values.schema.json` validates the values on install, upgrade, lint and template; `helm lint` fails without the required ones.

A minimal `my-values.yaml`:

```yaml
clusterId: my-cluster
executor:
  image:
    digest: sha256:<executor image digest>
  identity:
    azure:
      clientId: <client ID of the executor managed identity>
analyzer:
  image:
    digest: sha256:<analyzer image digest>
  watchNamespaces: [<namespace to watch>]
```

| Value | Default | Meaning |
| --- | --- | --- |
| `clusterId` | none, **required** | `--cluster-id` of the analyzer, see [cluster ID](#cluster-id) |
| `executor.image.repository`, `analyzer.image.repository` | `ghcr.io/pkalin/shadewright-executor`, `ghcr.io/pkalin/shadewright-analyzer` | Image repositories (a mirror, for example) |
| `executor.image.digest`, `analyzer.image.digest` | none, **required** | `sha256:<64 hex digits>`; images are pinned by digest, a tag cannot be set |
| `executor.image.pullPolicy`, `analyzer.image.pullPolicy` | `IfNotPresent` | |
| `analyzer.watchNamespaces` | none, **required** | `--watch-namespaces`, and one RoleBinding to the watch ClusterRole per namespace (see [namespaces and RoleBindings](#namespaces-and-rolebindings)) |
| `analyzer.maxIncidents` | unset (binary default 500) | `--max-incidents` |
| `executor.identity.azure.clientId` | empty | Client ID of the executor's managed identity (not a secret): sets the ServiceAccount annotation `azure.workload.identity/client-id` and the Pod label `azure.workload.identity/use`. Empty sets neither; see [workload identity](#workload-identity-azure) |
| `analyzer.identity.azure.clientId` | empty | The same for the analyzer, which needs none today |
| `executor.serviceAccount.annotations`, `analyzer.serviceAccount.annotations` | `{}` | More ServiceAccount annotations, for another cloud's workload identity |
| `executor.resources`, `analyzer.resources` | 10m / 64Mi requests, 500m / 128Mi limits | Container resources |
| `executor.extraArgs`, `analyzer.extraArgs` | `[]` | Further flags, each `--name` or `--name=value` |
| `executor.podLabels`, `podAnnotations`, `nodeSelector`, `tolerations`, `affinity` (and the same under `analyzer`) | empty | Scheduling and metadata |
| `imagePullSecrets` | `[]` | For a private mirror |
| `metrics.enabled`, `metrics.port`, `metrics.secure` | `true`, `8443`, `true` | `--metrics-bind-address`, the two Services, the metrics RBAC and NetworkPolicy; `secure: false` serves plain HTTP with no authentication (a test only) |
| `networkPolicy.enabled` | `true` | The four NetworkPolicies of `config/network-policy` |
| `crd.enabled`, `crd.keep` | `true`, `true` | Install the CRDs; keep them when the release is uninstalled (so Incidents and requests survive). Helm never upgrades CRDs from a `crds/` directory, so they are templates |
| `rbac.helpers.enabled` | `false` | The admin, editor and viewer ClusterRoles for each custom resource (for your people; Shadewright does not use them) |
| `settings.create`, `settings.spec` | `false`, `{}` | Create the `RemediationSettings` object `default` from `spec` (the fields of [RemediationSettings](#remediationsettings)). Helm owns it afterwards: an upgrade overwrites hand edits, so leave it off if you manage the object elsewhere |

Not in the chart: the Prometheus `ServiceMonitor` (as in `config/default`), cert-manager and webhooks (none are installed), and the `Namespace` (use `--create-namespace`). The security contexts, probes, ports and the replica count (1, with leader election) are fixed to what `config/` sets.

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
| `--watch-namespaces` | none, **required** | Comma-separated namespaces whose events, pods and workloads are read. Each must be a valid namespace name; spaces and duplicates are dropped; an empty list is an error, never "all namespaces" | `demo` (the lab) |
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

`--cluster-id` is hashed into every Incident fingerprint, so Incidents from two clusters never merge. It is 1 to 63 letters, digits, `.`, `_` or `-`, starting and ending with a letter or digit. Give each cluster its own. **Changing it starts every Incident afresh**, so choose it once. The manifests ship `lab`; the Helm chart takes it from the required value `clusterId`.

## Namespaces and RoleBindings

Three namespaces matter:

- **Operator namespace** (`shadewright-system`: the `namespace` of `config/default`, with the name prefix `shadewright-`). It holds the Deployments, ServiceAccounts, Incidents, RemediationRequests and the analysis budget. The executor reconciles requests only here; a request anywhere else is never seen. A policy must list this namespace in `requestNamespaces`, or the request is `Denied` (`RequestNamespaceNotAllowed`). Renaming it means changing the federated credential subject, every RoleBinding subject, and `--namespace`.
- **Watched namespaces**, set by `--watch-namespaces`. The analyzer's cache is limited to them, so it asks for no more than its RBAC grants.
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

One cluster-scoped object named `default` (field details in the [API reference](api.md)):

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
