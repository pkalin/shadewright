---
title: Compatibility
---

# Compatibility

What Shadewright has been run against, taken from CI, the end-to-end setup, the fleet repository and the reference lab, and what its code is written to accept. The two are kept apart: **tested** means a pipeline or the lab ran it; **accepted** means the code and the CRD rules allow it but nothing runs it. Where nothing is tested, this page says so instead of promising a range. State at v0.2.0-rc.1, with the lab run of 2026-11-04.

## Tested

| Component | Version | Where it runs |
| --- | --- | --- |
| Kubernetes API server (envtest) | 1.37 | Unit and envtest suites in CI (`make test`). The version is the one of `k8s.io/api` in `go.mod`, so it moves with it. CRD validation (CEL), RBAC and the controllers run against it |
| Kubernetes (Kind) | the node image Kind v0.33.0 uses by default | The `e2e` job in CI (`make test-e2e`): installs the CRDs and both Deployments and checks that they run, serve metrics and hold separate Leases. The node image is not pinned in this repository |
| Kubernetes (Kind, fleet) | 1.36.4 | The fleet repository's `Kind` workflow brings up a cluster with Flux and the infrastructure and apps, without Shadewright |
| Kubernetes (AKS) | 1.36 (1.36.5 on 2026-11-04) | The reference lab. `kubernetes_version` is unset in `shadewright-azure`, so the cluster takes the region's default. The fleet pins its schema checks to 1.36.0 |
| Flux | 2.10.0 (source-controller 1.10.1, kustomize-controller 1.10.1, helm-controller 1.7.1, notification-controller 1.10.0) | The lab and the fleet's Kind cluster, installed from the release manifest with images pinned by digest. In the lab only these four controllers run |
| Helm chart | 0.2.0-rc.1 | `ct install` and `helm test` on Kind in CI (pull requests that change the chart, and every release tag). In the lab on 2026-11-04, Flux helm-controller 1.7.1 installed the chart from `oci://ghcr.io/pkalin/charts/shadewright` by digest, after source-controller verified its keyless signature (`OCIRepository` with `verify.provider: cosign`) |
| KEDA | 2.21.0 (chart and app) | The lab and the fleet's Kind cluster, from the upstream chart, not the AKS add-on |
| Go | the `toolchain` line of `go.mod` (1.26.10) | CI and the image build |
| Kustomize (render check) | `sigs.k8s.io/kustomize/api` 0.21.2, linked into the executor | The executor renders in process; there is no kustomize binary to match. The fleet's own checks use kustomize 5.8.1 |

No pipeline runs the operator, Flux and KEDA together: that combination is the manual [OOM acceptance run](../lab-oom-acceptance.md) in the lab.

## Accepted, not tested

| Area | What the code accepts |
| --- | --- |
| Kubernetes | 1.32 or later. The executor's limits list RemediationRequests with field selectors on the CRD's selectable fields (`status.phase`, `status.holdsTarget`, `status.breakerCounts`, #293, #390), which are generally available from Kubernetes 1.32 (beta, on by default, from 1.31). On an API server without CRD field selectors those lists fail and the executor lets no request through the policy check (fail closed). The Helm chart's `kubeVersion` requires it. 1.32 to 1.35 are accepted, not tested. The CRDs use CEL validation rules, so the API server must support them, and the collector reads `events.k8s.io/v1` Events and Pod `status.observedGeneration` where the kubelet writes it. Older than 1.32 is not supported |
| Flux | Detection and source mapping read `kustomize.toolkit.fluxcd.io/v1`, `source.toolkit.fluxcd.io/v1` and `helm.toolkit.fluxcd.io/v2`; the [design](../design.md) calls this Flux 2.3 and later. Beta versions are not read. The reason strings of the Flux module were read from the controller versions in the table above |
| KEDA | `keda.sh/v1alpha1`, the only version KEDA 2.x serves. The policy check refuses any other version for `AdjustKedaMaxReplicas` |
| Other platforms | The `generic` and `aks` platform modules exist, and `eks` and `gke`, which only detect (no diagnosis rules). A cluster is detected as EKS or GKE only when every node with a provider ID carries that platform's node pool label (`eks.amazonaws.com/nodegroup`, `cloud.google.com/gke-nodepool`); nodes with neither a provider ID nor a platform label are skipped. EKS sets its label only on managed node groups, so a cluster with self-managed, Karpenter, Auto Mode or Fargate nodes is detected as generic, and so is a GKE cluster with a self-managed Compute Engine node. Nothing tests a cluster that is neither Kind nor AKS |
| Network policy | The shipped NetworkPolicies need a CNI that enforces them. The lab runs Cilium (AKS, Azure CNI powered by Cilium): the API server is reachable under them there, checked on 2026-11-07 ([#216](https://github.com/pkalin/shadewright/issues/216)), and Cilium enforces their port rules. On a CNI that does not enforce them the objects are accepted and do nothing |

## Architectures

Both images are built for `linux/amd64` and `linux/arm64` (Images workflow, on pull requests without pushing and on `main` and tags with a push). Everything that **runs** in CI and in the lab is amd64: the GitHub-hosted `ubuntu` runners and the lab's `Standard_D4as_v5` node. The arm64 images are built but no pipeline starts them, so arm64 is untested.

## Git hosting

| Item | Supported |
| --- | --- |
| Host | GitHub.com only. The adapter talks to `api.github.com`; GitHub Enterprise Server and other hosts are not supported. GitLab and Azure DevOps are **planned** behind the `GitProvider` interface ([ADR-0013](../adr/0013-adapters-for-every-external-system.md)) |
| Repository URL in `RemediationSettings` | `https://github.com/<owner>/<repo>`, lower case, no `.git` suffix, no trailing slash. 1 to 39 characters for the owner, 1 to 100 for the repository |
| URL of the Flux `GitRepository` | Must equal the settings URL **byte for byte**, so `ssh://git@github.com/...`, a `.git` suffix, upper case or a trailing slash on the Flux side do not match and the request is `Denied` with `RepositoryNotConfigured` |
| Target branch | Must equal the branch the `GitRepository` tracks (`spec.ref.branch`); a tag, a semver range, a commit or a name-only ref is not mapped |
| Authentication | A GitHub App installed on that repository. No personal access tokens |
| Signing | An Azure Key Vault RSA key through workload identity. The key ID is `https://<vault>.vault.azure.net/keys/<name>[/<version>]`, public Azure cloud only |

## Supported source layouts

The executor changes one value in one file. It finds that file by following what Flux delivers, and it refuses everything it cannot follow with a stated reason (see [troubleshooting](../troubleshooting.md#delivery-from-flux)). Today:

- The object is delivered by a Flux `Kustomization` (labels `kustomize.toolkit.fluxcd.io/name` and `/namespace`) that lists it in `status.inventory`, is `Ready` for its current generation, applies to this cluster (no `spec.kubeConfig`) and does not decrypt SOPS (`spec.decryption`).
- Its source is a `GitRepository` that tracks one branch, with no `suspend`, `ignore`, `include`, sparse checkout or submodules, `Ready` for its generation, whose artifact comes from that branch.
- The path holds a `kustomization.yaml` (or `.yml` or `Kustomization`, exactly one) that uses **only `resources`**, pointing at plain manifest files or at directories with the same kind of kustomization file. Patches, generators, components, transformers, `namespace`, `namePrefix`, `images`, `replicas` and labels in that file are refused, because they change what is built. A path **without** a kustomization file is searched as Flux builds it: every `.yaml` and `.yml` file below it, at most 5 directories deep, and every nested directory with its own kustomization file of the same kind. Names Flux leaves out of its artifact by default (such as `cloudbuild.yaml`, `.github/` or `*.png`) and files that are not manifests are skipped, as Flux skips them; a hidden manifest (such as `.env.yaml`) or a manifest in a hidden directory stops the mapping with `InvalidPath`. A base found below such a path counts the directories above it against the nesting depth of 8.
- Exactly one YAML document in those files declares the object, and the file matches the policy's `allowedPaths`. No `.sourceignore` on the way.
- Limits: 8 directories deep, 256 files, 512 reads and 1 MiB per file.
- Helm releases are not mapped. A target owned by a `HelmRelease` is `NeedsIntervention`.
- The two actions today change `resources.limits.memory` of a Deployment or StatefulSet container and `spec.maxReplicaCount` of a KEDA `ScaledObject`. A value that a patch or a generator overrides is caught by the render check, which renders the whole path before and after and requires one difference.

The fleet repository's `apps/` follows these rules on purpose (one object per file, `resources` only, literal values for the fields that are patched); it is the reference layout.
