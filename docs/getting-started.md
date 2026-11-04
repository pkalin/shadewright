---
title: Getting started
---

# Getting started

This page has three paths, from the one that needs nothing to the one that opens a real pull request: run the tests without a cluster, install the agent in observe-only mode, and take a hand-written request to a reviewed pull request. Settings are in the [configuration reference](reference/configuration.md); a request that stops is explained in [troubleshooting](troubleshooting.md).

!!! warning "What exists today"
    Shadewright is at v0.1.0. Built: the collector, the Incident controller, platform and module detection (all in the analyzer), and the executor's whole path from a `RemediationRequest` to an open pull request waiting for a person (policy check, source mapping, freshness check, branch, commit, pull request). **Not built:** the analysis that calls a model (so nothing proposes a request by itself), auto-merge, delivery tracking, verification, the Helm chart ([#83](https://github.com/pkalin/shadewright/issues/83)) and the `Cancelled` phase. Every request is written by hand today. The [README](https://github.com/pkalin/shadewright#readme) and the [threat model](threat-model.md) list what is built and planned.

## 1. Run the tests without a cluster

You need `git`, `make` and Go. The Go release is the `toolchain` line of `go.mod` (1.26.10 at v0.1.0); a newer local Go can report different lint and vet results, so pin it with `GOTOOLCHAIN`.

```bash
git clone https://github.com/pkalin/shadewright.git
cd shadewright
export GOTOOLCHAIN=go1.26.10   # the toolchain line of go.mod
make test                      # unit tests and envtest
```

`make test` regenerates the manifests, formats and vets the code, downloads the pinned `setup-envtest` and a real `kube-apiserver` and `etcd` on first use (it needs network access then), and runs every package except `test/e2e`. The envtest suites start those two binaries locally, so no cluster, Docker or cloud account is involved. The CRD validation rules (CEL), the controllers and the RBAC checks all run against that API server.

Other checks that need no cluster:

```bash
make lint                # golangci-lint, and a check that the Go release matches the Dockerfile
make verify-generated    # regenerates CRDs, RBAC and the API reference; fails on any difference
```

`make test-e2e` creates a kind cluster and is not needed for contributions; CI runs it. The contributor guide is [AGENTS.md](https://github.com/pkalin/shadewright/blob/main/AGENTS.md).

## 2. Install in observe-only mode

Observe-only means the analyzer watches the namespaces you name and records `Incident` objects, and nothing else happens: no model is called (nothing calls one yet), no settings entry names a repository, and `spec.paused` is true, so the executor could not write to Git even if a request existed. It writes only its own control state: Incidents, the budget ConfigMap, the settings status and Events.

What you need:

- A cluster with `kubectl` access that may create CRDs, ClusterRoles and a namespace. The CRDs use CEL validation rules, so the API server must support them. Which Kubernetes versions are tested is part of the compatibility matrix, which is **planned** (issue [#252](https://github.com/pkalin/shadewright/issues/252)); the envtest suites run against Kubernetes 1.37.
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

Expect both Deployments ready, a log line with your namespaces and cluster ID, and in the settings status the platform detection and the `DetectionFailed` condition `False`. Incidents appear once a watched namespace has a symptom the collector knows (for example an `OOMKilled` container or a failing pod); each carries the condition `AnalysisDue` with reason `AnalysisDisabled`, which is what `maxAnalysesPerDay: 0` does. The logs are the controller-runtime development format until you pass `--zap-devel=false`.

The executor Deployment carries the label `azure.workload.identity/use: "true"` and its ServiceAccount a placeholder client ID. Both are inert off AKS and in observe-only mode, where the executor never opens a repository. They matter in the next path.

To remove observe-only, `kubectl delete -k .` and delete the settings object first. Removing the CRDs deletes every Incident. The order for an install that has requests is part of the upgrade and uninstall guide, which is **planned**.

## 3. From a hand-written request to a reviewed pull request

This is the path that changes something outside the cluster, so read it as a checklist and then follow a runbook. The complete, tested walk-through is the [lab runbook](lab-oom-acceptance.md): it brings up the reference lab in Azure, applies the samples in `config/samples/lab/`, and checks the pull request, the idempotent re-apply and the `Superseded` end.

What a real run needs, all of it checked by the code:

| Need | Why | Where it is checked |
| --- | --- | --- |
| Flux delivers the target: a `Kustomization` whose `status.inventory` lists it, from a `GitRepository` that tracks one branch, with plain manifests | The executor maps the live object to the one file that declares it ([ADR-0012](adr/0012-object-to-source-mapping.md)) and refuses anything else | Source mapping, reasons in [troubleshooting](troubleshooting.md#delivery-from-flux) |
| A `RemediationSettings` named `default` with one repository entry: its URL equal to the `GitRepository` URL, `targetBranch` equal to the branch Flux tracks, GitHub App client ID, installation ID and the Key Vault `keyID` | The model and the request never choose a URL or credentials | Policy check |
| A `RemediationPolicy` that allows the action in the target's namespace, with bounds, the repository by name and `allowedPaths` | Deterministic policy decides, with bounds, what may run | Policy check |
| A GitHub App installed on that repository only, with the four permissions of [ADR-0006](adr/0006-github-app-key-in-key-vault.md) | Branch, commit and pull request are made as the App | First write (`GitForbidden`) |
| The App's private key in Azure Key Vault as a non-exportable key, and a managed identity that may only `sign` with it, federated to the executor ServiceAccount (workload identity on AKS) | The key never enters the cluster. Key Vault is the only signer built | First Git call |
| The request in the operator namespace, with UIDs of the Incident and of the target, `previous` equal to the live value, `desired` inside the policy | A request is immutable, and a changed plan is a new request | Policy check |

The samples show the shape of each object:

```bash
kubectl apply -f config/samples/lab/remediation_v1alpha1_remediationsettings.yaml   # after filling the REPLACE values
kubectl apply -f config/samples/lab/remediation_v1alpha1_remediationpolicy.yaml
kubectl apply -f config/samples/lab/remediation_v1alpha1_remediationrequest.yaml
kubectl -n shadewright-system get remediationrequest -w
```

Expect the phases `Pending`, `PolicyChecked`, `PRCreated`, then `AwaitingApproval`, with one pull request on the branch `shadewright/<request UID>` against the target branch. Read the pull request like any other, and merge it yourself: with `approval: Required` (and also when a policy would allow otherwise, because auto-merge is not built) a person decides. After a merge the request ends `Merged`; delivery tracking and verification are **planned**. Any other end is `Denied`, `NeedsIntervention`, `Expired` or `Superseded` with the reason in the conditions: look it up in [troubleshooting](troubleshooting.md). To stop everything quickly, see the [emergency stop](emergency.md).
