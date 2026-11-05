---
title: Install in observe-only mode
---

# Install in observe-only mode

The first step for every user: install Shadewright so that it watches and records, and changes nothing. Two ways are below: a kustomize overlay on the release manifests, and the Helm chart. The next step is [your first proposal](first-proposal.md). Settings are in the [configuration reference](reference/configuration.md); if something does not start, see [troubleshooting](troubleshooting.md).

What is built and what is not is on the [Get started](getting-started.md) page: read its box first.

## Install with kubectl and kustomize

Observe-only means the analyzer watches the namespaces you name and records `Incident` objects, and nothing else happens: no model is called (nothing calls one yet), no settings entry names a repository, and `spec.paused` is true, so the executor could not write to Git even if a request existed. It writes only its own control state: Incidents, the budget ConfigMap, the settings status and Events.

What you need:

- A cluster with `kubectl` access that may create CRDs, ClusterRoles and a namespace. Kubernetes 1.32 or later: the executor lists requests with CRD field selectors. The CRDs use CEL validation rules, so the API server must support them. The tested Kubernetes versions are in the [compatibility](reference/compatibility.md) page; the envtest suites run against Kubernetes 1.37.
- The namespaces to watch already exist.
- `kubectl` with built-in kustomize (`kubectl apply -k`).

The base is `config/default` of the release tag `v0.1.0`, so the manifests match the images. Both Deployments use the placeholder image `controller:latest`, and the two images are published as separate repositories, so the overlay replaces each by digest. Check the digests against the [release](https://github.com/pkalin/shadewright/releases/tag/v0.1.0) before you use them. Create a directory outside any Git repository of the project with two files.

`kustomization.yaml`:

```yaml
resources:
  - github.com/pkalin/shadewright/config/default?ref=v0.1.0
  - watch-rolebinding.yaml
patches:
  - target:
      kind: Deployment
      name: shadewright-executor
    patch: |-
      - op: replace
        path: /spec/template/spec/containers/0/image
        value: ghcr.io/pkalin/shadewright-executor@sha256:66df21ea967a22e8e50b73284254b796665ef5e9470b320707900983aca37f9c
  - target:
      kind: Deployment
      name: shadewright-analyzer
    patch: |-
      - op: replace
        path: /spec/template/spec/containers/0/image
        value: ghcr.io/pkalin/shadewright-analyzer@sha256:697defe7774b29621f64c967ff93e83b309455c2a72d50b190e179f9ff3fc8e4
      # The base passes --watch-namespaces=demo and --cluster-id=lab. A repeated flag takes its last value,
      # so appending is enough.
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --watch-namespaces=default
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --cluster-id=my-cluster
```

`watch-rolebinding.yaml` (one RoleBinding per watched namespace, with that namespace in `metadata.namespace`; add one more document for each namespace in the flag):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: shadewright-analyzer-watch
  namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: shadewright-analyzer-watch-role
subjects:
  - kind: ServiceAccount
    name: shadewright-analyzer
    namespace: shadewright-system
```

The base's `config/watch` is not used: it holds the lab's `demo` namespace only. Choose the cluster ID once, because changing it starts every Incident afresh ([cluster ID](reference/configuration.md#cluster-id)).

Review what you are about to apply, then apply it and the settings:

```bash
kubectl kustomize . | less
kubectl apply -k .
kubectl wait --for=condition=Established \
  crd/incidents.remediation.shadewright.io crd/remediationpolicies.remediation.shadewright.io \
  crd/remediationrequests.remediation.shadewright.io crd/remediationsettings.remediation.shadewright.io
kubectl apply -f - <<'EOF'
apiVersion: remediation.shadewright.io/v1alpha1
kind: RemediationSettings
metadata:
  name: default
spec:
  paused: true
  analysis:
    maxAnalysesPerDay: 0
EOF
```

Check it:

```bash
kubectl -n shadewright-system rollout status deploy/shadewright-analyzer deploy/shadewright-executor
kubectl -n shadewright-system logs deploy/shadewright-analyzer | grep "Watching namespaces"
kubectl get remediationsettings default -o jsonpath='{.status}{"\n"}'
kubectl -n shadewright-system get incidents
```

Expect both Deployments ready, a log line with your namespaces and cluster ID, and in the settings status the platform detection, the `DetectionFailed` condition `False` and `NoWatchedNamespaces` `False`. An analyzer started with no namespace (the Helm chart's default) logs `Watching no namespaces` instead, reports `NoWatchedNamespaces` `True` once a settings object exists, and records no Incident. Incidents appear once a watched namespace has a symptom the collector knows (for example an `OOMKilled` container or a failing pod); each carries the condition `AnalysisDue` with reason `AnalysisDisabled`, which is what `analysis.maxAnalysesPerDay: 0` does. The logs are the controller-runtime development format until you pass `--zap-devel=false`.

The executor Deployment carries the label `azure.workload.identity/use: "true"` and its ServiceAccount a placeholder client ID. Both are inert off AKS and in observe-only mode, where the executor never opens a repository. They matter in [the next step](first-proposal.md).

To remove observe-only, `kubectl delete -k .` and delete the settings object first. Removing the CRDs deletes every Incident. The order for an install that has requests is in [upgrade and uninstall](upgrade-uninstall.md).

## Install with Helm instead

The chart installs the same objects as `config/default`, except the helper roles (off by default) and the Namespace, plus one `RoleBinding` per watched namespace, with the images, cluster ID, watched namespaces and workload identity as values. With no values the published chart installs an idle Shadewright: no watched namespace, no Azure workload identity, the cluster ID from the `kube-system` UID, and the release's images by digest.

Pick a version on the [releases page](https://github.com/pkalin/shadewright/releases), verify the chart (the commands are in [verify a release](upgrade-uninstall.md#verify-a-release) and in the release's own notes), then install it from the registry:

```bash
helm install shadewright oci://ghcr.io/pkalin/charts/shadewright --version <version> \
  --namespace shadewright-system --create-namespace \
  --set clusterId=my-cluster --set 'analyzer.watchNamespaces={default}'
```

`clusterId` is optional (it defaults to the UID of `kube-system`); `analyzer.watchNamespaces` is what makes the analyzer watch anything. There is no `helm repo add` step because the chart is an OCI artifact, and `--version` is required for a pre-release such as `0.2.0-rc.1`, because Helm does not pick pre-releases by itself. For the strict path, install the digest you verified: `oci://ghcr.io/pkalin/charts/shadewright@sha256:<chart digest>` in place of the reference and `--version` (see the [configuration reference](reference/configuration.md#helm-chart)).

For development, install from a clone with `./dist/chart` in place of the OCI reference. A clone has no image digests: pass `--set executor.image.digest=sha256:<digest> --set analyzer.image.digest=sha256:<digest>` for the release whose tag you checked out, or `--set image.allowTag=true` to run the `appVersion` tag with images built from that same clone.

The same settings object and checks as above apply (`--set settings.create=true` and `settings.spec` create it from values, only when it does not exist; Helm never changes it afterwards). Uninstalling keeps the CRDs by default (`crd.keep`), so Incidents survive. The commands to upgrade the release and to uninstall it, what stays behind (the CRDs and the settings object) and how to remove it are in [upgrade with Helm](upgrade-uninstall.md#upgrade-with-helm), [uninstall with Helm](upgrade-uninstall.md#uninstall-with-helm) and [the settings object](upgrade-uninstall.md#the-settings-object-and-helm).
