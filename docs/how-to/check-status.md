---
title: Check status with kubectl
---

# Check status with kubectl

`kubectl shadewright status` answers in one command what you would otherwise read from conditions by hand: which Incidents exist, whether each is due for analysis and why, what the last analysis decided, which requests it made with their policy decision and pull request, how much of the day's analysis budget is used, and whether the kill switch is on. `kubectl shadewright explain <name>` answers why one request is in its phase ([below](#explain-one-request)), and `kubectl shadewright doctor` checks an install ([check an install](check-install.md)). It is a kubectl plugin: a separate binary that runs on your machine with your kubeconfig and your permissions. Nothing is installed in the cluster.

## Install

Each release (pre-releases too) carries `kubectl-shadewright` archives for linux and darwin on amd64 and arm64, a `checksums.txt` and its signature `checksums.txt.sigstore.json`, built and signed by the [release-plugin workflow](https://github.com/pkalin/shadewright/blob/main/.github/workflows/release-plugin.yml). Download the files of one release, verify them, and put the binary on your `PATH`; kubectl finds any executable named `kubectl-shadewright` there.

```bash
VERSION=0.2.0                 # the release without the leading v; the tag is v$VERSION
OS=linux ARCH=amd64           # linux or darwin, amd64 or arm64
gh release download "v${VERSION}" --repo pkalin/shadewright \
  --pattern "kubectl-shadewright_${VERSION}_${OS}_${ARCH}.tar.gz" \
  --pattern checksums.txt --pattern checksums.txt.sigstore.json
```

Verify before you run anything. The signature covers `checksums.txt`, which lists the sha256 of every archive; the identity is the workflow file on a release tag of this repository, issued by GitHub's OIDC provider.

```bash
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/pkalin/shadewright/\.github/workflows/release-plugin\.yml@refs/tags/v[0-9].*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing checksums.txt     # macOS: shasum -a 256 --check --ignore-missing checksums.txt
gh attestation verify "kubectl-shadewright_${VERSION}_${OS}_${ARCH}.tar.gz" \
  --repo pkalin/shadewright \
  --signer-workflow pkalin/shadewright/.github/workflows/release-plugin.yml
```

To pin one release, replace `v[0-9].*$` in the regular expression with `vX.Y.Z$`. Each command exits non-zero when the signature, the identity, the checksum or the provenance does not match. Then install:

```bash
tar -xzf "kubectl-shadewright_${VERSION}_${OS}_${ARCH}.tar.gz" kubectl-shadewright
install -m 0755 kubectl-shadewright ~/.local/bin/   # any directory on PATH
kubectl plugin list | grep shadewright
```

### Build it yourself

For development, build from a checkout or install with Go. These binaries are not signed.

```bash
make build-plugin      # bin/kubectl-shadewright
go install github.com/pkalin/shadewright/cmd/kubectl-shadewright@latest   # or @vX.Y.Z; reports version dev
```

`make build-plugin` stamps the version from `git describe` (override with `PLUGIN_VERSION=...`); a plain `go build` or `go install` is `dev`.

```bash
kubectl shadewright version     # kubectl-shadewright v0.2.0
```

`status` compares that version with the label `app.kubernetes.io/version` that the Helm chart puts on the Shadewright CRDs, and prints a one-line warning on standard error when the release differs: the plugin may then miss or misread fields. Only the release is compared: a leading `v` and the suffix `git describe` adds (`-3-gabc1234`, `-dirty`) are ignored, so a local build a few commits past `v0.2.0` matches CRDs labelled `0.2.0`. No warning is printed for a `dev` build or one with no tag (a bare commit SHA), for CRDs installed with kustomize (they carry no such label), or when your role may not read the CRDs.

## Run it

```bash
kubectl shadewright status -n shadewright-system
kubectl shadewright status -A            # every namespace that holds Incidents or requests, and the context's
kubectl shadewright status -n shadewright-system -o json
kubectl shadewright status -n shadewright-system -w      # redraw every 5 seconds until Ctrl-C
```

| Flag | Meaning |
| --- | --- |
| `-n`, `--namespace` | The operator namespace. Default: the namespace of the kubeconfig context, as for kubectl |
| `-A`, `--all-namespaces` | Read Incidents and requests in every namespace, grouped by namespace. The namespace of `-n` or of the context is always shown, with its budget, even without Incidents |
| `-o`, `--output` | `text` (default) or `json`. The JSON holds the same fields, with times in RFC 3339 |
| `--kubeconfig`, `--context` | As for kubectl |
| `--request-timeout` | Limit for all reads together, as kubectl takes it: seconds (`30`), a duration (`30s`, `1m`), or `0` for no limit (default `30s`). With `-w` the limit applies to each refresh. The CRD version check has its own 5 second limit and runs next to the reads |
| `-w`, `--watch` | `status` only: read again every `--interval` and show the result until you press Ctrl-C ([below](#watch-a-request-move)) |
| `--interval` | `status` only: time between refreshes with `-w`, as seconds (`10`) or a duration (`10s`, `1m`), at least `1s` (default `5s`). Refused without `-w` |

The text output, per namespace:

```text
Kill switch:  off: writes are allowed

Namespace shadewright-system
  Analyses today (UTC): 4 of 20

  Incident incident-3f1c...
    Subject:   Deployment demo/web
    Seen:      OOMKilled, count 3, last seen 2m ago
    Symptom:   <the normalized, redacted symptom>
    Phase:     Proposed
    Due:       no, Proposed: The last analysis made a proposal (since 60s ago)
    Analysis:  began 70s ago, outcome Proposed: The analysis proposed a change; a RemediationRequest was created for the policy check
    Request:   rr-...  AwaitingApproval  SetContainerMemoryLimit on Deployment/web, policy Allowed, PR https://github.com/<owner>/<repo>/pull/12, created 60s ago
```

- **Due** is the `AnalysisDue` condition: `yes` or `no`, its reason and its message. The reasons are explained in [troubleshooting](../troubleshooting.md#not-request-reasons).
- **Analysis** is when the last analysis began (`status.lastAnalysisTime`) and the outcome the analysis wrote in the `Analyzed` condition. The model and the tokens of a run are not stored in the cluster: they are in the analyzer log (`Analyzed Incident`) and, with a record store configured, in an analysis record whose ID is in `status.analysisRecords` ([#77](https://github.com/pkalin/shadewright/issues/77)). The last entry is the latest run only when that run stored its record: a run whose record was not written adds no ID, and neither does a run whose claim was lost after its record was stored (the analyzer log names that record in its "Dropped the outcome" line), so compare the time in the ID (the run's `lastAnalysisTime`, as `<uid>-YYYYMMDDtHHMMSSz`) with **Analysis**, and look for a Warning event `AnalysisRecordNotWritten` on the Incident and `shadewright_analysis_records_total`.
- **Request** lines list the RemediationRequests whose `spec.incidentRef` names the Incident (name and UID), oldest first, with the phase, the reason of the `PolicyAllowed` condition and the pull request. A request that names an Incident not in the list (deleted, or another UID) is listed under "Requests without a listed Incident".
- **Analyses today** is the analysis budget ConfigMap `shadewright-analysis-budget`, read with the analyzer's own parser, against the daily limit in `RemediationSettings`. When the analyzer refuses every analysis, the line says `analysis stopped:`, why, and the analyzer's own `AnalysisDue` reason: `SettingsNotFound`, `SettingsIncomplete`, `BudgetNotFound` (recreate the ConfigMap empty), `BudgetUnreadable` (content the analyzer does not trust, taken as spent), `AnalysisDisabled` (limit 0) or `DailyBudgetReached` (resets at 00:00 UTC). These are checked in the analyzer's order.
- **Kill switch** is `spec.paused` of `RemediationSettings` `default` ([pause and resume](pause-and-resume.md)).

A setting or the budget that the plugin cannot read with your permissions, or that fails to read, is shown as unknown with the reason, never as "analysis stopped": it says nothing about what the analyzer can read; a list of Incidents or requests that fails stops the command with an error.

## Watch a request move

`-w` runs `status` again every `--interval` until you press Ctrl-C (or the process gets SIGTERM), so you can follow a request through its phases without rerunning the command. Both flags apply to `status` only: `explain` and `doctor` refuse them.

- **Text**, on a terminal: the screen is cleared and redrawn each time, under a line with the interval and the time of the read (`Every 5s, last read 2026-11-06T12:00:05Z (Ctrl-C to stop)`). When the output is redirected to a file or a pipe nothing is cleared: each refresh follows the previous one after a blank line.
- **JSON** (`-o json`): one JSON document per refresh, each on one line (newline-delimited JSON), with the same fields as a single `-o json`, for `jq` or a script.
- **A refresh that fails** (the API server unreachable, `--request-timeout` reached) prints `Error at <time>: <reason>` on standard error and nothing on standard output, and the next refresh runs as planned: on a terminal the last good status stays on the screen, and standard output keeps only complete refreshes, so the JSON stays parseable.
- **A refresh refused** as Forbidden or Unauthorized (a missing permission, expired credentials) stops the command with that error and exit status 1, since waiting does not grant access.
- **A closed pipe** (`-w -o json | head -1`) ends it as it ends other commands: killed by SIGPIPE, exit status 141 in the shell. A failed write to another kind of output stops it with an error.
- Ctrl-C stops it with exit status 0, between refreshes or in the middle of one; a refresh cut short prints nothing. If a read does not return, a second Ctrl-C ends the process at once. The CRD version warning is printed after the first refresh and, on a terminal, again under each redraw, which clears it.

It polls with the same `get` and `list` reads as a single `status`, not with a Kubernetes `watch`: no new permission, the [roles below](#permissions) are enough. Each refresh reads everything again, so nothing is missed or shown twice when the API server restarts between two refreshes. The interval counts from the end of one refresh to the start of the next. The minimum of one second keeps one watching person from becoming a load on the API server; with `-A` on a large cluster, prefer a longer interval.

## Explain one request

```bash
kubectl shadewright explain <request> -n shadewright-system
kubectl shadewright explain <incident> -n shadewright-system      # each of its requests, newest first
kubectl shadewright explain <request> -n shadewright-system -o json
```

The name is looked up as a RemediationRequest first, then as an Incident. The flags are those of `status` without `-A`. For a request it shows:

```text
Request rr-3f1c... in namespace shadewright-system
  Phase:        NeedsIntervention, since 60s ago
  Stopped by:   Approved False, BranchDiverged: <the condition's message>
  Read more:    https://shadewright.io/troubleshooting/#pull-request-step
  Incident:     incident-3f1c... (Deployment demo/web, OOMKilled, phase Proposed)
  Action:       SetContainerMemoryLimit on Deployment demo/web, container app, 256Mi to 512Mi
  Policy:       WithinApprovedBounds: Every check passed (policy default, revision default/3), approval required
  Plan digest:  sha256:...
  Source:       repository fleet, branch main, file apps/demo/deployment.yaml
  Pull request: https://github.com/<owner>/<repo>/pull/12, head 4b825dc
  Conditions, oldest first:
    30m ago    PolicyAllowed=True WithinApprovedBounds: Every check passed
    29m ago    PullRequestOpened=True PullRequestOpened: <message>
    2m ago     Approved=False BranchDiverged: <message>

Kill switch:  off: writes are allowed
```

- **Stopped by** is shown only in a phase that ends a request without a merge (`Denied`, `NeedsIntervention`, `Superseded`, `Expired`, `Cancelled`, `Failed`), and in `Inconclusive`, where it is the `Verified` condition: the most recent condition that is not `True`, leaving out `WritesPaused` and `Queued`, which only hold a request back. **Read more** links to the section of [troubleshooting](../troubleshooting.md) whose table lists that reason. Table rows have no anchors of their own on this site, so the link goes to the section and you find the row by its reason. A reason listed in two sections links the one of its condition (on `PlanFresh`, [freshness](../troubleshooting.md#freshness)), else the first. A reason the page does not list links the page and says so.
- **Policy** is the reason and message of `PolicyAllowed`, the policy and its generation the decision used (`status.policyRevision`) and whether a person must approve (`status.effectiveApprovalRequired`). **Action** shows the parameters the policy checked against its bounds: the previous and the desired value. The bounds themselves are in the `RemediationPolicy`, which the plugin does not read.
- **Pull request** follows the link rule of `status` (below), with the head Shadewright recorded and, once merged, the merge commit.
- **Replaces** and **Replaced by** are the two sides of a supersede handoff ([ADR-0028](../adr/0028-reviewer-hints-through-the-pull-request.md)), with the phase of each side; **Chain root** is `spec.chainRoot`. **Reverts** is the name in `spec.reverts`: the failed request a revert undoes ([ADR-0030](../adr/0030-merge-receipt-and-verification-outcomes.md) section 8); **Reverted by** is the other side, on a failed request its Incident's `status.revertsProposed` records as `Created`: the name of the revert the analyzer proposed (it may have been deleted since, and none is proposed again); a revert's **Cleared** line says `RevertApplied`, since its clearing is never checked.
- **Incident** is the Incident the request names, shown only when its UID is the one in `spec.incidentRef`. Otherwise the line says why: not found, another Incident of that name, or not readable with your permissions.

For an Incident, the output starts with the Incident (subject, reason, phase and the last analysis), then each request whose `spec.incidentRef` names it by name and UID, newest first. The JSON output holds the same fields: `kind` (`RemediationRequest` or `Incident`), `incident`, `incidentState`, `settings` and `requests`, each request with `stop` (`condition`, `status`, `reason`, `message`, `link`, `listed`) and `conditions`, oldest first.

`explain` reads the request, its Incident and `RemediationSettings` with `get`, and for an Incident lists the requests of its namespace: reads the Role under [permissions](#permissions) already grants. It calls neither the Git host nor a model.

## What it does not do

- **It only reads.** It sends `get` and `list` requests and nothing else (`-w` repeats them): no write, no `watch`, no `exec`, no logs. The code reads through a client type that has no write methods.
- **It prints no terminal control text.** Names, symptoms, condition messages and any other string from the cluster (which includes text a model wrote, invariant 4 in the [threat model](../threat-model.md)) are cleaned before printing: ANSI and OSC escape sequences are removed whole (so a name cannot set the window title, write a hidden link, move the cursor or clear the screen), then every other control, format (bidirectional overrides, zero-width characters) and line separator character; tabs and line breaks become spaces, and each field is cut to a fixed length. The JSON output carries the same cleaned strings.
- **It links only to your Git repository.** A pull request link is printed only when `status.pullRequest.url` is exactly the URL the GitHub adapter writes for the repository the request was resolved to (`status.source.repositoryRef` in `RemediationSettings`) and that number. Otherwise only the number is printed, with "link not shown". A status written by someone other than the executor, or a repository entry that changed since, cannot put a link on your screen.

## Permissions

The plugin needs these reads; give them to the people who use it. Bind the Role in the operator namespace and the ClusterRole cluster-wide (`RemediationSettings` is cluster-scoped).

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: shadewright-status-viewer
  namespace: shadewright-system
rules:
- apiGroups: ["remediation.shadewright.io"]
  resources: ["incidents", "remediationrequests"]
  verbs: ["get", "list"]
- apiGroups: [""]
  resources: ["configmaps"]
  resourceNames: ["shadewright-analysis-budget"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: shadewright-settings-viewer
rules:
- apiGroups: ["remediation.shadewright.io"]
  resources: ["remediationsettings"]
  resourceNames: ["default"]
  verbs: ["get"]
# Optional: the version check reads the metadata of the Shadewright CRDs.
- apiGroups: ["apiextensions.k8s.io"]
  resources: ["customresourcedefinitions"]
  resourceNames:
  - incidents.remediation.shadewright.io
  - remediationrequests.remediation.shadewright.io
  - remediationsettings.remediation.shadewright.io
  verbs: ["get"]
```

With `-A` the Role must be a ClusterRole bound cluster-wide, since the lists span every namespace. The viewer ClusterRoles the chart creates with `rbac.helpers.enabled` (`incident-viewer-role`, `remediationrequest-viewer-role`, `remediationsettings-viewer-role`) grant the same reads on the custom resources, plus `watch`, which the plugin does not use; the budget ConfigMap needs the Role above either way.

## The columns of kubectl get

Without the plugin, `kubectl get` shows the most important fields too:

- `kubectl -n shadewright-system get incidents`: Phase, Subject, Reason, Due (the reason of `AnalysisDue`), Last analysis and Age. `-o wide` adds Kind, Outcome (the reason of `Analyzed`), Count and Last seen.
- `kubectl -n shadewright-system get rr`: Phase, Policy (the reason of `PolicyAllowed`), Action, Target, PR (the number) and Age.

!!! note "Not yet run"
    The plugin is covered by unit tests in `cmd/kubectl-shadewright` (escape sequences included) and was run once against a local API server (envtest) with the CRDs of this release and hand-written objects, which is where the columns above come from. It has not yet run against a Kind cluster with the OOM scenario.
