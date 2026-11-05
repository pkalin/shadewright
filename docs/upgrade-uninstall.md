---
title: Upgrade and uninstall
---

# Upgrade and uninstall

What to do, in which order, when you move Shadewright to a new release or remove it. The steps are written for the manifests of v0.1.0 (`config/default`, applied with `kubectl apply -k`, see [getting started](getting-started.md#2-install-in-observe-only-mode)); the commands for the Helm chart are in [Upgrade with Helm](#upgrade-with-helm) and [Uninstall with Helm](#uninstall-with-helm). The Helm chart installs the same objects, except the helper roles (off by default) and the Namespace, and the order below applies to `helm upgrade` and `helm uninstall` too. Note that `helm uninstall` keeps the CRDs by default (`crd.keep`), and that with `crd.keep=false` it removes the executor and the CRDs together, so do steps 1 to 3 of the [uninstall](#uninstall) first. Helm creates the `RemediationSettings` object `default` only when it does not exist and never changes or deletes it afterwards.

The one thing to know first: **a `RemediationRequest` that has opened a pull request carries a finalizer, and only the executor can remove it.** Delete the executor, the namespace or the CRDs before the requests and they stay `Terminating` with nobody to clean up.

## What the finalizer does

The executor adds the finalizer `remediation.shadewright.io/close-pull-request` to a request just before its first Git write, so no branch or pull request exists without it. It is the only finalizer Shadewright uses, and deleting a request never touches the cluster or rolls back a deployment.

When a request is deleted, the executor:

- closes its pull request if the request is before merge (`PolicyChecked`, `PRCreated`, `AwaitingApproval`, `AutoMergeQueued`), stopped in `NeedsIntervention` or `Expired`, or holds the lock on its target (`status.holdsTarget`) in any phase, then removes the finalizer, which also releases that lock. The pull request is closed only if it carries the request's marker; a pull request without it is left alone. The branch `shadewright/<request UID>` is left in place;
- leaves a merged pull request alone (the close is refused with a conflict and the deletion goes on);
- does not wait when the repository left the settings, the target branch changed, or the pull request is already gone;
- **waits** while `spec.paused` is true, because closing is a write (see the [emergency stop](emergency.md));
- **retries without end** on any other error, for example a Git host that refuses the App (`GitForbidden`) or a Key Vault key it cannot sign with. The request stays `Terminating` until the cause is gone.

A request that never reached the Git step has no finalizer and is simply deleted.

## Deleting a request and `Cancelled`

Deleting a request before merge is today the only way to cancel it, and what it does is the list above: close the pull request, remove the object.

`Cancelled` is a phase in the API ("cancelled before the change was merged") and in the [design](design.md), but **nothing sets it in v0.1.0**: there is no field or action that cancels a request and keeps it as a record, and no issue for it yet. If you need the record, copy the object before deleting it. `Cancelled` and `Superseded` differ: `Superseded` is set by the executor when the plan went stale (policy, settings or target changed) and the pull request was closed; see [troubleshooting](troubleshooting.md#freshness).

## Verify a release

Images built from `main` or a `v*` tag after [#82](https://github.com/pkalin/shadewright/issues/82) are signed keyless with cosign (the certificate comes from GitHub's OIDC token, there is no key to trust), carry an SPDX SBOM per platform image (linux/amd64 and linux/arm64) as a signed attestation, and have a GitHub build provenance attestation. v0.1.0 images are not signed. Verify by digest, never by tag, and use the digest you will deploy. You need [Docker](https://docs.docker.com/engine/install/) (for `buildx imagetools`), [jq](https://jqlang.org/), [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 3.x or later (the signatures are stored as Sigstore bundles, which cosign 2.x does not find) and the [GitHub CLI](https://cli.github.com/).

```bash
IMAGE=ghcr.io/pkalin/shadewright-executor   # and shadewright-analyzer
DIGEST=sha256:<digest from the release page>
IDENTITY='^https://github\.com/pkalin/shadewright/\.github/workflows/images\.yml@refs/(heads/main|tags/v[0-9].*)$'
ISSUER=https://token.actions.githubusercontent.com

# Signature: made by the images workflow of this repository.
cosign verify \
  --certificate-identity-regexp "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER" \
  "$IMAGE@$DIGEST"

# SBOM (SPDX JSON) attestations: one per platform image, on the digest of each child of the index,
# with the same identity; the predicate is the SBOM.
for ARCH in amd64 arm64; do
  CHILD=$(docker buildx imagetools inspect --raw "$IMAGE@$DIGEST" | jq -r --arg a "$ARCH" \
    '.manifests[] | select(.platform.os == "linux" and .platform.architecture == $a) | .digest')
  cosign verify-attestation --type spdxjson \
    --certificate-identity-regexp "$IDENTITY" \
    --certificate-oidc-issuer "$ISSUER" \
    "$IMAGE@$CHILD" | jq -r '.payload | @base64d | fromjson | .predicate' > "sbom-$ARCH.spdx.json"
done

# Build provenance: built by this repository's images workflow.
gh attestation verify "oci://$IMAGE@$DIGEST" \
  --repo pkalin/shadewright \
  --signer-workflow pkalin/shadewright/.github/workflows/images.yml
```

Releases after v0.1.0 list the digests of its two images and its chart, with these commands filled in for that tag (the identity pinned to it), once the images and the chart are published. A pre-release tag such as `v0.2.0-rc.1` is built, signed and verified the same way: the identity regexp above accepts it, it is tagged `0.2.0-rc.1` only (no `latest`, no `X.Y`), and its chart has that version and `appVersion`.

Each command exits non-zero when the signature, the identity or the issuer does not match. To pin a single release instead of any `main` or tag build, replace the end of `IDENTITY` with `refs/tags/vX.Y.Z$`. Each SBOM lists the packages of one platform image; the signature and the provenance are on the index digest, the SBOMs on the child digests (images from before [#341](https://github.com/pkalin/shadewright/issues/341) have a single SBOM on the index digest, made from the linux/amd64 image). The same checks run in the [images workflow](https://github.com/pkalin/shadewright/blob/main/.github/workflows/images.yml) right after every push, so a build that cannot be verified fails there.

## Upgrade

Both Deployments run one replica with leader election and have no state of their own: Incidents, requests, the settings and the budget live in the cluster API. Every step of the executor rebuilds what it knows from Git and the API, and branch names and pull request markers derive from the request UID and the plan digest, so a restart in the middle of a step finds the work already done instead of repeating it.

1. **Read the release notes** ([CHANGELOG](https://github.com/pkalin/shadewright/blob/main/CHANGELOG.md) and the GitHub release). While the version is 0.x a minor release may carry breaking changes (a commit with `!` or a `BREAKING CHANGE:` footer): API fields, CRD schema, flags or what the manifests install. The API is `v1alpha1` and there is no conversion between versions.
2. **From v0.1.0: move `maxAnalysesPerDay`.** v0.1.0 had the daily analysis limit at `spec.maxAnalysesPerDay`; it is now `spec.analysis.maxAnalysesPerDay`. Before upgrading, read the old value (`kubectl get remediationsettings default -o jsonpath='{.spec.maxAnalysesPerDay}{"\n"}'`; empty means it was never set and nothing needs to move). Right after step 5 applies the new CRDs (the old schema would drop the new field), set the new field to the value you read and remove the old one in one patch, for example for `0`: `kubectl patch remediationsettings default --type merge -p '{"spec":{"maxAnalysesPerDay":null,"analysis":{"maxAnalysesPerDay":0}}}'`. Change the same in the manifests you keep in Git: a manifest that still sets the old field is refused by the API server, by kubectl and by Flux alike. If you skip this step, a value stored in the old field stays and the analysis trigger goes by the lower of the two fields (the new one defaults to 100), so a stored `0` still keeps analysis off; only a write that sets or changes the old field is refused.
3. **Pause writes (optional)**: set `spec.paused: true` on the `RemediationSettings` object if you want no pull request opened or closed while the pods restart. Analysis goes on. Unpause afterwards.
4. **Use the same release for both images.** Analyzer and executor share the CRDs and the API types, and only a pair built from one tag has been tested together. Replace both digests in your overlay (the release page lists them; [verify them](#verify-a-release) first) and keep the `?ref=` of the base on the same tag.
5. **Apply**: `kubectl apply -k .` applies the CRDs and the Deployments together. Wait for the CRDs (`kubectl wait --for=condition=Established crd/...`, as in getting started) and then for `kubectl -n shadewright-system rollout status deploy/shadewright-analyzer deploy/shadewright-executor`. Only the pod holding the Lease (`shadewright-executor-leader-election`, `shadewright-analyzer-leader-election`) works, so a new pod does nothing until it has the Lease, which can take a short while after the old pod is gone.
6. **Check** the settings status (`DetectionFailed` is `False`), the Deployments' logs for errors, and that the waiting requests still show their phase.
7. **Unpause** if you paused.

What happens to requests that are waiting for a person when you upgrade:

- A request in `PRCreated` or `AwaitingApproval` keeps its pull request. The executor rechecks it every five minutes and on any change, as before.
- A release that changes how the plan digest is computed or what the freshness check compares would end such a request as `Superseded` (`PlanChanged`) and close its pull request. This follows from the freshness check, not from a promise of a release, so read the notes of every upgrade.
- A change to the settings or the policy in the same step has the same effect, whatever the release, and one that removes or re-identifies the repository (URL or installation) closes nothing: see [freshness](troubleshooting.md#freshness) and [identity rotation](identity-rotation.md#2-the-github-app-installation).
- The release that closes #294 records the lock on a target in `status.holdsTarget`. A request written before has no such field: the executor sets it once from the request's status, keeping the lock for a request that the executor still moves on, that is being deleted with its finalizer, or that stopped with a recorded pull request its status does not show closed or merged (see [limits](troubleshooting.md#limits)). Such a stopped request then looks its pull request up every 10 minutes and frees the target once it is closed or merged.

What is not touched by an upgrade: the budget ConfigMap `shadewright-analysis-budget` (it ships without `data`, so applying the manifests does not reset the day's count) and existing Incidents. A downgrade is not tested; the CRDs of the older release may not accept objects the newer one wrote.

!!! note "Flux delivers the install"
    In the reference lab the agent is installed by Flux from a vendored copy of `config/default` (`infrastructure/shadewright/` in the fleet repository, regenerated by `scripts/vendor-shadewright.sh` for a commit and two image digests). An upgrade there is a pull request that regenerates the copy, and Flux applies it. Its Kustomization has `prune: true`, so removing a file from the copy deletes the object from the cluster.

### Upgrade with Helm

Steps 1 to 3 above (release notes, from v0.1.0 the move of `maxAnalysesPerDay`, whose patch follows `helm upgrade`, and the optional pause) come first. The chart carries both image digests of its release, so one chart version is one tested pair of images (step 4), and `helm upgrade` is step 5. Keep your values in a file and pass it on every upgrade:

```bash
helm upgrade shadewright oci://ghcr.io/pkalin/charts/shadewright --version <new version> \
  --namespace shadewright-system -f my-values.yaml --wait --timeout 5m
```

- Pin `--version`, or better install the digest you verified (`oci://ghcr.io/pkalin/charts/shadewright@sha256:<chart digest>` in place of the reference and `--version`; Helm accepts it). Verify the chart and the images first (the chart's cosign identity is in the [configuration reference](reference/configuration.md#helm-chart), the images are in [verify a release](#verify-a-release)).
- Prefer `-f my-values.yaml` to `--reuse-values`, which reuses the values of the last release instead of the new chart's: the digests of the new release may not reach the Deployments. After any upgrade check the images that run (`kubectl -n shadewright-system get deploy -o wide`). `--reset-then-reuse-values` starts from the new chart's values and applies yours.
- From a source checkout, use the path of the clone instead (`./dist/chart`). A clone has no digests: set `executor.image.digest` and `analyzer.image.digest` to the digests of the release you check out, or `image.allowTag=true` to run the `appVersion` tag, which needs images built from that same checkout. Without either, the chart refuses to render. Mind that the clone must be of the tag whose images you run.
- The CRDs are templates of the chart, so `helm upgrade` applies them with the Deployments, unlike a CRD in a `crds/` directory. With `--wait` Helm returns when the Deployments are available; the Lease handover from step 5 still applies.
- Keep the same `clusterId` on every upgrade. It is hashed into every Incident fingerprint, so changing it, also from the empty default (the UID of `kube-system`) to a value of your own, starts every Incident afresh. A client-side render (`helm template`, `helm diff`, Argo CD, Flux with a dry-run) asks no cluster and cannot read the `kube-system` UID, so it fails until `clusterId` is set: put it in the values of any such tool, and set it to the value the live release uses (`helm get values shadewright -n shadewright-system`, or `kubectl get ns kube-system -o jsonpath='{.metadata.uid}'` if you left it empty).
- The settings object is not changed by an upgrade, see [The settings object](#the-settings-object-and-helm).

Then, from v0.1.0, the patch of step 2, and steps 6 and 7 (check, unpause). `helm rollback` is a downgrade and carries the same warning.

## Uninstall

The order matters only if requests exist. Do it in this order:

1. **Get ready to clean up**: make sure `spec.paused` is **false** and that the GitHub App and the Key Vault key still work, because closing pull requests needs both. Nothing else needs stopping first: in v0.1.0 nothing creates requests by itself.
2. **Delete the requests** and wait until they are gone:

   ```bash
   kubectl -n shadewright-system delete remediationrequests --all --wait=true --timeout=5m
   ```

   Each one closes its open pull request. Check the fleet repository for pull requests whose body carries a Shadewright marker and that are still open. Merged ones are history and stay.
3. **If a request stays `Terminating`**, read why (`kubectl -n shadewright-system describe remediationrequest <name>`, and the executor log). Fix the cause, which is usually a pause that was left on or an App that was uninstalled or lost its permission, and the deletion finishes by itself. Only if the executor can never work again (the App or the vault is gone for good): close the pull request by hand, delete the branch `shadewright/<request UID>` if you want, and then remove the finalizer yourself:

   ```bash
   kubectl -n shadewright-system patch remediationrequest <name> --type=merge -p '{"metadata":{"finalizers":null}}'
   ```

   This is a human action. The agent itself never removes finalizers.
4. **Delete the workloads**: the executor and the analyzer (`kubectl delete -k .`, or delete the Deployments first if you want the CRDs to survive a while).
5. **Delete what you created by hand**: the `RemediationPolicy` and `RemediationSettings` objects, the RoleBinding `shadewright-analyzer-watch` in every watched namespace (it binds the ClusterRole `shadewright-analyzer-watch-role`, which `kubectl delete -k .` removes), and the metrics reader binding you made for Prometheus.
6. **Delete the CRDs last.** Removing them deletes every Incident, request, policy and settings object. Incidents hold the deduplication state and are the record of what the collector saw; export them first if you want it (`kubectl -n shadewright-system get incidents -o yaml`, which holds the redacted symptom and no raw logs, see [data handling](data-handling.md)).
7. **Outside the cluster**: uninstall the GitHub App from the repository (or delete its private key on GitHub) and delete the Key Vault key and the federated credential. Branches `shadewright/*` that were left are yours to delete. Analysis records in blob storage are kept for their retention, see [data handling](data-handling.md#retention).

With Flux, removing the `Kustomization` that installs Shadewright prunes its objects, and Flux does not wait for Shadewright's requests to finish their cleanup, so do steps 1 to 3 first. The budget ConfigMap is not kept on this path: `kubectl delete -k .` and the Flux prune delete it with the rest, and the next apply creates it empty, so the day's count starts again (unless you exclude the namespace or the object from the prune). The reference lab is the exception that makes this harmless in practice: it is destroyed as a whole, and any pull request still open stays open on GitHub, to be closed by hand.

### Uninstall with Helm

Do steps 1 to 3 of the [uninstall](#uninstall) first (settings unpaused, requests deleted and gone), then:

```bash
helm uninstall shadewright --namespace shadewright-system
```

What this removes: the two Deployments, the ServiceAccounts, the RBAC, the NetworkPolicies, the metrics Services and the `RoleBinding` the chart made in each watched namespace (which replaces the manual part of step 5 for those). What it leaves, as `helm uninstall` also says:

- **The CRDs, by default** (`crd.keep: true` adds `helm.sh/resource-policy: keep`), and with them every Incident, request and policy object. A later `helm install` of the same release name finds them and goes on with them.
- **The `RemediationSettings` object `default`**, if the chart created it, see below.
- **The budget ConfigMap `shadewright-analysis-budget`** (it carries `helm.sh/resource-policy: keep`), so a `helm install` of the same release name in the same namespace within the same UTC day adopts it and continues the day's count instead of starting from zero. The two leader election Leases are deleted with the release and recreated by the next install; they hold no state worth keeping. To reset the count on purpose, delete the ConfigMap by hand (the next install creates it empty): `kubectl delete configmap shadewright-analysis-budget --namespace shadewright-system`.
- The namespace, when `--create-namespace` made it (Helm never deletes it), and with it anything you created there. Delete it yourself when it holds nothing you need; deleting the namespace deletes the kept budget ConfigMap too.
- What you created by hand: your `RemediationPolicy` objects and a metrics reader binding for Prometheus.

**Remove the CRDs deliberately** (step 6), after the uninstall:

```bash
kubectl delete crd incidents.remediation.shadewright.io remediationpolicies.remediation.shadewright.io \
  remediationrequests.remediation.shadewright.io remediationsettings.remediation.shadewright.io
```

Deleting a CRD deletes every object of its kind, so this deletes **every Incident, `RemediationRequest`, policy and settings object** and cannot be undone. Export what you want to keep first (see step 6). Do it only after the requests are gone and the executor has been uninstalled: with the executor gone, a request that still holds its finalizer cannot be cleaned up.

The alternative is `crd.keep=false`, but Helm reads the annotation from the manifest stored with the release, so it has to be in the release **before** you uninstall: `helm upgrade ... --set crd.keep=false` (or the value in your file), then `helm uninstall`. `helm uninstall` then deletes the executor and the CRDs in one go, which is why steps 1 to 3 must be done and checked beforehand. `kubectl delete crd` after a default uninstall gives you the same result with a pause in between to check that nothing is left.

### The settings object and Helm

With `settings.create=true` and a `settings.spec`, the chart creates the `RemediationSettings` object `default` once, as a post-install and post-upgrade hook, and only when `lookup` finds none. After that Helm renders nothing for it: an upgrade does not change it, so a `spec.paused: true` you set as the [emergency stop](emergency.md) survives every `helm upgrade`, and a changed `settings.spec` in your values does not reach an existing object. Change it with `kubectl` or GitOps. The object has no hook-delete-policy and Helm does not track hook objects, so **`helm uninstall` does not delete it either**: remove it by hand, after the uninstall and before the CRDs if you keep them for a while:

```bash
kubectl delete remediationsettings.remediation.shadewright.io default
```

A new `helm install` with `settings.create=true` over a leftover object keeps the leftover, paused state included (it is not recreated from your values), so check it before you reinstall. `helm template` and `--dry-run` ask no cluster and always show the object. Leave `settings.create` off if you manage the object another way, in your GitOps repository for example.

## Not covered

- Rolling back after a merged pull request: merging is outside Shadewright's control, and undoing it is a revert pull request (the proposal of one is **planned**, [#96](https://github.com/pkalin/shadewright/issues/96)).
