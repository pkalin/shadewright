---
title: Upgrade and uninstall
---

# Upgrade and uninstall

What to do, in which order, when you move Shadewright to a new release or remove it. It is written for the manifests of v0.1.0 (`config/default`, applied with `kubectl apply -k`, see [getting started](getting-started.md#2-install-in-observe-only-mode)). The Helm chart installs the same objects, except the helper roles (off by default) and the Namespace; this page does not give its commands yet ([#306](https://github.com/pkalin/shadewright/issues/306)), but the order below applies to `helm upgrade` and `helm uninstall` too. Note that `helm uninstall` keeps the CRDs by default (`crd.keep`), and that with `crd.keep=false` it removes the executor and the CRDs together, so do steps 1 to 3 first. Helm creates the `RemediationSettings` object `default` only when it does not exist and never changes or deletes it afterwards.

The one thing to know first: **a `RemediationRequest` that has opened a pull request carries a finalizer, and only the executor can remove it.** Delete the executor, the namespace or the CRDs before the requests and they stay `Terminating` with nobody to clean up.

## What the finalizer does

The executor adds the finalizer `remediation.shadewright.io/close-pull-request` to a request just before its first Git write, so no branch or pull request exists without it. It is the only finalizer Shadewright uses, and deleting a request never touches the cluster or rolls back a deployment.

When a request is deleted, the executor:

- closes its pull request if the request is before merge (`PolicyChecked`, `PRCreated`, `AwaitingApproval`, `AutoMergeQueued`) or stopped in `NeedsIntervention` or `Expired`, then removes the finalizer. The pull request is closed only if it carries the request's marker; a pull request without it is left alone. The branch `shadewright/<request UID>` is left in place;
- leaves a merged pull request alone (the close is refused with a conflict and the deletion goes on);
- does not wait when the repository left the settings, the target branch changed, or the pull request is already gone;
- **waits** while `spec.paused` is true, because closing is a write (see the [emergency stop](emergency.md));
- **retries without end** on any other error, for example a Git host that refuses the App (`GitForbidden`) or a Key Vault key it cannot sign with. The request stays `Terminating` until the cause is gone.

A request that never reached the Git step has no finalizer and is simply deleted.

## Deleting a request and `Cancelled`

Deleting a request before merge is today the only way to cancel it, and what it does is the list above: close the pull request, remove the object.

`Cancelled` is a phase in the API ("cancelled before the change was merged") and in the [design](design.md), but **nothing sets it in v0.1.0**: there is no field or action that cancels a request and keeps it as a record, and no issue for it yet. If you need the record, copy the object before deleting it. `Cancelled` and `Superseded` differ: `Superseded` is set by the executor when the plan went stale (policy, settings or target changed) and the pull request was closed; see [troubleshooting](troubleshooting.md#freshness).

## Verify a release

Images built from `main` or a `v*` tag after [#82](https://github.com/pkalin/shadewright/issues/82) are signed keyless with cosign (the certificate comes from GitHub's OIDC token, there is no key to trust), carry an SPDX SBOM as a signed attestation, and have a GitHub build provenance attestation. v0.1.0 images are not signed. Verify by digest, never by tag, and use the digest you will deploy. You need [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 2.x or later and the [GitHub CLI](https://cli.github.com/).

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

# SBOM (SPDX JSON) attestation: the same identity; the predicate is the SBOM.
cosign verify-attestation --type spdxjson \
  --certificate-identity-regexp "$IDENTITY" \
  --certificate-oidc-issuer "$ISSUER" \
  "$IMAGE@$DIGEST" | jq -r '.payload | @base64d | fromjson | .predicate' > sbom.spdx.json

# Build provenance: built by this repository's images workflow.
gh attestation verify "oci://$IMAGE@$DIGEST" \
  --repo pkalin/shadewright \
  --signer-workflow pkalin/shadewright/.github/workflows/images.yml
```

Each command exits non-zero when the signature, the identity or the issuer does not match. To pin a single release instead of any `main` or tag build, replace the end of `IDENTITY` with `refs/tags/vX.Y.Z$`. The SBOM lists the packages of the linux/amd64 image. The same checks run in the [images workflow](https://github.com/pkalin/shadewright/blob/main/.github/workflows/images.yml) right after every push, so a build that cannot be verified fails there.

## Upgrade

Both Deployments run one replica with leader election and have no state of their own: Incidents, requests, the settings and the budget live in the cluster API. Every step of the executor rebuilds what it knows from Git and the API, and branch names and pull request markers derive from the request UID and the plan digest, so a restart in the middle of a step finds the work already done instead of repeating it.

1. **Read the release notes** ([CHANGELOG](https://github.com/pkalin/shadewright/blob/main/CHANGELOG.md) and the GitHub release). While the version is 0.x a minor release may carry breaking changes (a commit with `!` or a `BREAKING CHANGE:` footer): API fields, CRD schema, flags or what the manifests install. The API is `v1alpha1` and there is no conversion between versions.
2. **Pause writes (optional)**: set `spec.paused: true` on the `RemediationSettings` object if you want no pull request opened or closed while the pods restart. Analysis goes on. Unpause afterwards.
3. **Use the same release for both images.** Analyzer and executor share the CRDs and the API types, and only a pair built from one tag has been tested together. Replace both digests in your overlay (the release page lists them; [verify them](#verify-a-release) first) and keep the `?ref=` of the base on the same tag.
4. **Apply**: `kubectl apply -k .` applies the CRDs and the Deployments together. Wait for the CRDs (`kubectl wait --for=condition=Established crd/...`, as in getting started) and then for `kubectl -n shadewright-system rollout status deploy/shadewright-analyzer deploy/shadewright-executor`. Only the pod holding the Lease (`shadewright-executor-leader-election`, `shadewright-analyzer-leader-election`) works, so a new pod does nothing until it has the Lease, which can take a short while after the old pod is gone.
5. **Check** the settings status (`DetectionFailed` is `False`), the Deployments' logs for errors, and that the waiting requests still show their phase.
6. **Unpause** if you paused.

What happens to requests that are waiting for a person when you upgrade:

- A request in `PRCreated` or `AwaitingApproval` keeps its pull request. The executor rechecks it every five minutes and on any change, as before.
- A release that changes how the plan digest is computed or what the freshness check compares would end such a request as `Superseded` (`PlanChanged`) and close its pull request. This follows from the freshness check, not from a promise of a release, so read the notes of every upgrade.
- A change to the settings or the policy in the same step has the same effect, whatever the release, and one that removes or re-identifies the repository (URL or installation) closes nothing: see [freshness](troubleshooting.md#freshness) and [identity rotation](identity-rotation.md#2-the-github-app-installation).

What is not touched by an upgrade: the budget ConfigMap `shadewright-analysis-budget` (it ships without `data`, so applying the manifests does not reset the day's count) and existing Incidents. A downgrade is not tested; the CRDs of the older release may not accept objects the newer one wrote.

!!! note "Flux delivers the install"
    In the reference lab the agent is installed by Flux from a vendored copy of `config/default` (`infrastructure/shadewright/` in the fleet repository, regenerated by `scripts/vendor-shadewright.sh` for a commit and two image digests). An upgrade there is a pull request that regenerates the copy, and Flux applies it. Its Kustomization has `prune: true`, so removing a file from the copy deletes the object from the cluster.

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

With Flux, removing the `Kustomization` that installs Shadewright prunes its objects, and Flux does not wait for Shadewright's requests to finish their cleanup, so do steps 1 to 3 first. The reference lab is the exception that makes this harmless in practice: it is destroyed as a whole, and any pull request still open stays open on GitHub, to be closed by hand.

## Not covered

- Uninstall and upgrade commands for the Helm chart: **planned**, [#306](https://github.com/pkalin/shadewright/issues/306).
- Rolling back after a merged pull request: merging is outside Shadewright's control, and undoing it is a revert pull request (the proposal of one is **planned**, [#96](https://github.com/pkalin/shadewright/issues/96)).
