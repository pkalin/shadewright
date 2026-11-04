# shadewright

![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square)

<!-- No version badges: release-please bumps Chart.yaml in the release pull request, and a version printed here would make this file stale on every release. The version is on the releases page and in Chart.yaml. -->

<!-- This file is generated from values.yaml and README.md.gotmpl by helm-docs (make chart-docs). Do not edit it by hand. -->

[Shadewright](https://shadewright.io) is a Kubernetes operations agent. It watches a cluster, groups repeated symptoms into incidents, asks an LLM for a diagnosis and proposes a fix only as a pull request to your GitOps repository. The GitOps controller deploys the fix and Shadewright verifies the effect. Model output is never authorization: deterministic policy decides what may run.

This chart installs the two components (the analyzer and the executor), their RBAC, NetworkPolicies, metrics Services and the CRDs.

## Prerequisites

- Kubernetes version `>=1.28.0-0` (the `kubeVersion` of Chart.yaml).
- Flux, the GitOps controller that deploys the pull requests the executor opens (Argo CD is not supported yet).
- Workload identity for the executor: a managed identity (Azure) with a federated credential for the executor ServiceAccount, allowed to sign with the GitHub App key in Key Vault. Its client ID is `executor.identity.azure.clientId`. The ServiceAccount names derive from the release name: install as release `shadewright` in the namespace `shadewright-system` to get `shadewright-executor` and `shadewright-analyzer`, and check them with `helm template` before you create the credential.
- The images: the published chart runs the release's signed images by digest (written into its `values.yaml` when the chart is published). A chart from a source checkout has no digests and refuses to render until you set them, or opt in with `image.allowTag: true` to run the `appVersion` tag with images built from that same checkout (the v0.1.0 images stop at start with an empty watch list).

## Install

With no values the published chart installs an idle Shadewright. The analyzer starts and is Ready but watches no namespace (no RoleBinding outside the operator namespace, no Incident): it reads no namespaced object outside the operator namespace, has no Git identity, and writes only in the operator namespace (its Leases, the budget ConfigMap, Events) plus the settings status once a settings object exists; it still lists Nodes and reads discovery and the settings cluster-wide. The executor has no Azure workload identity, so it cannot reach Key Vault to sign for the GitHub App. The cluster ID defaults to the UID of the `kube-system` namespace. To catch a forgotten watch list, alert on `shadewright_analyzer_watched_namespaces == 0`.

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

## Verify the signatures

The chart and the images are signed with cosign keyless (GitHub OIDC) by digest. Verify before you install. The identity of the chart is the chart workflow on a release tag of this repository:

```bash
cosign verify \
  --certificate-identity-regexp '^https://github\.com/pkalin/shadewright/\.github/workflows/chart\.yml@refs/tags/v[0-9].*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/pkalin/charts/shadewright@sha256:<chart digest>
```

The digest is in the run summary of the Chart workflow, or `helm pull oci://ghcr.io/pkalin/charts/shadewright --version <version>` prints it (`Digest:`). Image signatures are verified the same way: use the image name (`ghcr.io/pkalin/shadewright-executor@sha256:<digest>` or `shadewright-analyzer`) and the `images\.yml` workflow in the identity.

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

### Metrics

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| metrics.enabled | bool | `true` | Serve the metrics endpoint of both components, with its two Services. When false there are no Services, no metrics RBAC and no metrics NetworkPolicy. |
| metrics.port | int | `8443` | Metrics port, 1 to 65535. Passed as `--metrics-bind-address` and opened by the metrics NetworkPolicy. |
| metrics.secure | bool | `true` | Serve metrics over HTTPS with a TokenReview and a SubjectAccessReview per request. `false` serves plain HTTP with no authentication: for a test only, `helm install` prints a warning. |
| metrics.serviceMonitor.enabled | bool | `false` | Install a Prometheus Operator `ServiceMonitor` for each component (needs the `monitoring.coreos.com/v1` CRDs and `metrics.enabled`). Prometheus scrapes with its own ServiceAccount token, so bind that ServiceAccount to the `metrics-reader` ClusterRole, and label its namespace `metrics: enabled` for the NetworkPolicy. The endpoint's certificate is self-signed, so the ServiceMonitor skips its verification. It scrapes with `bearerTokenFile` (Prometheus's own token), which Prometheus Operator deprecates and rejects when `arbitraryFSAccessThroughSMs` is denied: then keep this off and write your own ServiceMonitor with `authorization` from a Secret that holds a token allowed to read metrics. |
| metrics.serviceMonitor.interval | string | `""` | Scrape interval, such as `30s` or `1m`. Empty uses the Prometheus default. |
| metrics.serviceMonitor.labels | object | `{}` | Further labels on the ServiceMonitors, for the `serviceMonitorSelector` of your Prometheus. The chart's own labels cannot be overridden. |

### Network policy

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| networkPolicy.enabled | bool | `true` | Install the NetworkPolicies: default deny for the namespace, DNS and TCP 443 and 6443 egress per component, and ingress to the metrics port from namespaces labeled `metrics: enabled`. Setting `false` installs no NetworkPolicy at all, so the namespace loses its default deny too and every Pod in it can talk to anything unless you restrict it yourself (`helm install` prints a warning). |

### CRDs

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| crd.enabled | bool | `true` | Install the CRDs as templates (Helm does not upgrade or delete CRDs from a `crds/` directory). Set `false` only if you install them another way. |
| crd.keep | bool | `true` | Keep the CRDs, and so every Incident and RemediationRequest, when the release is uninstalled (adds the `helm.sh/resource-policy: keep` annotation). Set `false` to delete all of them with the release: `helm uninstall` then removes the executor and the CRDs together, so delete the requests first (steps 1 to 3 of upgrade and uninstall). |

### RBAC

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| rbac.helpers.enabled | bool | `false` | Install admin, editor and viewer ClusterRoles for each custom resource. Shadewright does not use them, they are for your people. Off by default (least privilege). |

### Identity

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| executor.identity.azure.clientId | string | empty (no identity) | Client ID (a UUID, not a secret) of the executor's Azure managed identity for workload identity. Sets the ServiceAccount annotation `azure.workload.identity/client-id` and the Pod label `azure.workload.identity/use`. Empty sets neither: the executor then cannot sign for the GitHub App, so a request that reaches the Git step is retried quietly and stays Pending. Must differ from the analyzer's, or the chart refuses to render (separate identities, never one for both). |
| analyzer.identity.azure.clientId | string | empty (no identity) | Client ID (a UUID, not a secret) of the analyzer's Azure managed identity. The analyzer has no cloud identity today (the model call is not built, ADR-0023), so leave it empty. Same effect as the executor's value, and it must differ from the executor's or the chart refuses to render. |

### Executor

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| executor.serviceAccount.annotations | object | `{}` | Further ServiceAccount annotations, for another cloud's workload identity. Keys under `azure.workload.identity/` are refused: use `executor.identity.azure.clientId`. |
| executor.resources.limits.cpu | string | `"500m"` | CPU limit. `limits` is required and cannot be empty, so the container is never unbounded. |
| executor.resources.limits.memory | string | `"128Mi"` | Memory limit. |
| executor.resources.requests.cpu | string | `"10m"` | CPU request. |
| executor.resources.requests.memory | string | `"64Mi"` | Memory request. |
| executor.extraArgs | list | `[]` | Further command line flags, each `--name` or `--name=value`. The chart refuses the flags it sets itself, so a free list cannot turn off leader election, metrics authentication or the watched namespaces: `--leader-elect`, `--health-probe-bind-address`, `--metrics-bind-address`, `--metrics-secure`, `--namespace`, `--watch-namespaces`, `--cluster-id` and `--max-incidents`. |
| executor.podLabels | object | `{}` | Further Pod labels. The chart refuses `control-plane`, `app.kubernetes.io/*`, `helm.sh/*` and `azure.workload.identity/*`: it sets them itself (the identity labels come from `identity.azure.clientId`). |
| executor.podAnnotations | object | `{}` | Further Pod annotations. |
| executor.nodeSelector | object | `{}` | Node selector of the Pod, at most 32 labels. |
| executor.tolerations | list | `[]` | Tolerations of the Pod, at most 32 Kubernetes toleration objects. |
| executor.affinity | object | `{}` | Affinity of the Pod, a Kubernetes affinity object (`nodeAffinity`, `podAffinity`, `podAntiAffinity`). |
| executor.topologySpreadConstraints | list | `[]` | Topology spread constraints of the Pod, at most 8 Kubernetes `topologySpreadConstraints` objects. |
| executor.priorityClassName | string | `""` | Priority class of the Pod. Empty uses the cluster default. |
| executor.imagePullSecrets | list | `[]` | Image pull secrets of this component only, added to the global `imagePullSecrets`, a list of `{name: <secret>}` (at most 16). |

### Analyzer

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| analyzer.watchNamespaces | list | empty (no namespace) | Unique namespace names, at most 256. The analyzer reads events, Pods and workloads only in these namespaces: the chart passes them as `--watch-namespaces` and binds the read-only watch ClusterRole with one RoleBinding in each (`kube-system` included only if you list it). Empty (the default) watches no namespace, never all of them: the analyzer starts, is Ready and records no Incident, and no RoleBinding is created. `*` is refused. |
| analyzer.maxIncidents | string | null (the binary's default, 500) | Most Incidents kept in the operator namespace (`--max-incidents`), 1 or more. Null (unset) keeps the binary's default of 500. |
| analyzer.serviceAccount.annotations | object | `{}` | Further ServiceAccount annotations, for another cloud's workload identity. Keys under `azure.workload.identity/` are refused: use `analyzer.identity.azure.clientId`. |
| analyzer.resources.limits.cpu | string | `"500m"` | CPU limit. `limits` is required and cannot be empty, so the container is never unbounded. |
| analyzer.resources.limits.memory | string | `"128Mi"` | Memory limit. |
| analyzer.resources.requests.cpu | string | `"10m"` | CPU request. |
| analyzer.resources.requests.memory | string | `"64Mi"` | Memory request. |
| analyzer.extraArgs | list | `[]` | Further command line flags, each `--name` or `--name=value`. The chart refuses the flags it sets itself, so a free list cannot turn off leader election, metrics authentication or the watched namespaces: `--leader-elect`, `--health-probe-bind-address`, `--metrics-bind-address`, `--metrics-secure`, `--namespace`, `--watch-namespaces`, `--cluster-id` and `--max-incidents`. |
| analyzer.podLabels | object | `{}` | Further Pod labels. The chart refuses `control-plane`, `app.kubernetes.io/*`, `helm.sh/*` and `azure.workload.identity/*`: it sets them itself (the identity labels come from `identity.azure.clientId`). |
| analyzer.podAnnotations | object | `{}` | Further Pod annotations. |
| analyzer.nodeSelector | object | `{}` | Node selector of the Pod, at most 32 labels. |
| analyzer.tolerations | list | `[]` | Tolerations of the Pod, at most 32 Kubernetes toleration objects. |
| analyzer.affinity | object | `{}` | Affinity of the Pod, a Kubernetes affinity object (`nodeAffinity`, `podAffinity`, `podAntiAffinity`). |
| analyzer.topologySpreadConstraints | list | `[]` | Topology spread constraints of the Pod, at most 8 Kubernetes `topologySpreadConstraints` objects. |
| analyzer.priorityClassName | string | `""` | Priority class of the Pod. Empty uses the cluster default. |
| analyzer.imagePullSecrets | list | `[]` | Image pull secrets of this component only, added to the global `imagePullSecrets`, a list of `{name: <secret>}` (at most 16). |

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

