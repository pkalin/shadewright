# 8. GitOps on GitHub

This section covers how a fix becomes a pull request: what `approvalRequired` means (8.1), the steps from a request to a merged change (8.2), auto-merge (8.3) and the branch protection the repository must have (8.4). Numbers and edge cases that only matter when you build or debug are in the table "Details" at the end (8.5).

## 8.1. What approvalRequired means

`approvalRequired` is a field of our CRD. It is not a switch that turns off review on GitHub: if the branch requires a review, a PR with `false` still waits for it.

| Request says | Shadewright does | Repository rules |
| --- | --- | --- |
| `true` | Opens the PR and waits for an independent human approval of the current head commit | All branch rules still apply |
| `false` | May enable auto-merge after policy allows automation | All branch rules still apply |
| Anything, but policy requires review | Effective value is `true` | Shadewright cannot lower it |
| Anything, but the change is not allowed | Stops with `Denied` | No PR |

### Where merges happen without a human

Fully automatic merges happen only where the repository owner deliberately allows them. On GitHub that is a ruleset plus `CODEOWNERS`:

- Require code owner review.
- Make every path owned by a human.
- Leave only the narrow paths that policy may auto-merge without an owner (a later `CODEOWNERS` pattern with no owner).

Those PRs then merge on required checks alone; everything else needs a human.

## 8.2. Making a change

The executor runs steps 1 to 7; step 8 is planned (section 6). Every Git write names the state it expects, so a restart or a race finds the existing work instead of repeating it.

1. Map the object to its source in Git (8.2.1).
2. Read the current SHA of the target branch and create branch `shadewright/<request-uid>`.
3. Apply the typed change to the allowed file, checking the previous value. Patch YAML in place, preserving comments and formatting.
4. Render the final manifests and compute the semantic diff, including indirect changes through values (8.2.2).
5. Validate YAML, schemas, safety rules and cost or capacity bounds.
6. Commit and open a PR with the evidence, scope, the size of the step beside the policy (ADR-0026), expected effect, risk and rollback plan.
7. Bind the PR to the request UID and plan digest. Any new push to the branch invalidates the plan.
8. After merge, follow delivery and the success criteria instead of stopping at "merged".

**Lab measurement (2026-11-06).** On one `Standard_D4as_v5` node running `v0.3.0-rc.2` (commit `bbe555ca09db887d1389270e53e6ef64249cd7eb`), symptom to pull request took 3 min 34 s. Measured between object timestamps, not per step: 2 min 16 s from the first `OOMKilled` to the analysis claim (Incident creation, debounce and a trigger pass), 10 s from the claim to the RemediationRequest (context and one model call), 26 s from the request to `PolicyAllowed` (source mapping and the policy check) and 42 s from there to the pull request opened. An idle sample on AKS with 30 Pods and about 400 Events showed the analyzer at 17 Mi and the executor at 13 Mi. The peak memory under a stated load, measured on Kind, is recorded in [3.1](3-observation-and-deduplication.md#31-collecting) (analyzer 87 Mi, executor 20 Mi against 128Mi).

### 8.2.1. Step 1: source mapping

**What it does.** It finds the one file in Git that declares the live object ([ADR-0012](../adr/0012-object-to-source-mapping.md)). The Flux labels on the object name its Kustomization, which gives the source and path. The file is the one plain manifest in that path that declares the object. The result is recorded in `status.source`.

**Why.** The agent must change what Flux delivers, and nothing else. The labels on the object are untrusted, so the mapping is checked from several sides before it is accepted.

**It is accepted only if all of these hold:**

- The Kustomization is Ready (`True` for the current generation) and its `status.inventory` lists the object.
- The value in the file equals the live value (otherwise an inline patch changes it).
- The `GitRepository` tracks a branch (`spec.ref.branch`). Tags, semver, commits and other refs stop with `NeedsIntervention`.
- Its URL equals exactly one repository in RemediationSettings, and that repository is the policy's `gitOps.repositoryRef` (otherwise `Denied`).
- The branch equals the repository's `targetBranch`.
- The file matches the policy's `allowedPaths`.

**Limits.**

- The files searched are the ones the path's kustomization file lists in `resources`.
- A path without a kustomization file is searched as Flux builds it: Flux generates a kustomization from every `.yaml` and `.yml` file below the path, so the mapping lists the path with `GitProvider.ListDir` (one directory per call; GitHub: the Git Trees API, not recursive) to a depth of 5, sorted by name, and follows a nested directory with its own kustomization file as such. Symbolic links and submodules are skipped. Names Flux's source-controller leaves out of every artifact by default are skipped as Flux skips them (`.git/`, `.gitignore`, `.gitmodules`, `.gitattributes`, `.github/`, `.circleci/`, CI files such as `cloudbuild.yaml` and `appveyor.yml`, `*.png`, `*.zip` and other images and archives, `.sops.yaml`, `.flux.yaml`, `.goreleaser.yml`; the patterns are vendored from `ExcludeVCS`, `ExcludeExt`, `ExcludeCI` and `ExcludeExtra` of fluxcd/pkg `sourceignore` v0.9.0 and kept equal to upstream), and so are files that are not manifests and the entries of a directory with its own kustomization file. Hidden directories are listed, as Flux descends into them. A `.sourceignore` file in a listed directory, or a manifest or base that would be in the build and whose name is not a plain path segment (a hidden manifest such as `.env.yaml`), stops the mapping. Depth: a base found `d` directories below the path starts at nesting depth `d`, so `MaxDepth` (8) bounds the directories passed and the chain of kustomizations together; a hand-written kustomization that lists a directory several levels down counts them the same way. Only `ErrPathNotInTree` from `ListDir` means the path is not a directory; a plain not found is retried. The GitHub adapter keeps the tree object ID of each directory it read at a commit, so a walk costs about one request per directory ([#122](https://github.com/pkalin/shadewright/issues/122)).
- Objects from patches, generators or Helm charts are not supported yet: they stop with `NeedsIntervention` (details in ADR-0012).

**What can go wrong: a Kustomization that waits.** Flux includes workload health in Ready only when the Kustomization sets `wait: true` or `healthChecks`. A Kustomization that waits on a broken workload stays not Ready, so it is refused. A Kustomization delivering workloads Shadewright should fix must therefore not wait on them (the fleet's `apps` Kustomizations do not). Accepting applied-but-unhealthy objects needs its own ADR ([#259](https://github.com/pkalin/shadewright/issues/259)).

**What can go wrong: hostile or huge Git contents.** Git contents are untrusted, so the mapping bounds the file contents it downloads, caches and parses:

- Besides the limits of ADR-0012 on depth, files, reads and file size (1 MiB), one mapping reads at most 16 MiB in all and decodes at most 4096 YAML documents (see 8.5). Past either, it stops with `NeedsIntervention` and reason `InputTooLarge`.
- Each read asks the Git provider for at most what is left of the byte budget (and at most 1 MiB). The provider checks the size the host reports before downloading (the tree entry, for GitHub), so a file past the budget is neither downloaded nor kept in the read cache, and a cached file is checked against the budget the same way. A build of exactly 16 MiB resolves; past it, the mapping stops with `InputTooLarge`, also when the file would be over 1 MiB too, so a cold and a warm cache give the same reason. A missing file costs nothing and is never refused for the budget ([#298](https://github.com/pkalin/shadewright/issues/298)).
- The budget counts file contents and directory listings. A `ListDir` call counts as one read and costs 128 bytes per entry (`ListEntryBytes`, about one entry of a GitHub tree answer, since the interface does not report what was downloaded); a directory of more than 1000 entries is refused by the provider (`TooManyFiles`), and a listing past what is left of the budget stops with `InputTooLarge`, from the cache or not. The tree listings the GitHub adapter reads inside a `ReadFile` call to find a file are bounded per call (the adapter's JSON body limit) and in number by the read limit, not by the 16 MiB. The semantic diff's loader asks for what is left of its own 4 MiB build budget the same way (8.2.2).
- It decodes one document at a time and keeps only the declaration.
- It stops with `AmbiguousDeclaration` at the first document that makes the match ambiguous (a second declaration, or one in another version or without a namespace), instead of reading the rest of the build.

Because of this early stop, the reason reported can depend on file order. A near match or second declaration in an earlier file is reported before a problem in a later file (invalid YAML, a missing file, a budget run out). Every one of them is `NeedsIntervention`. Near the end of the byte budget, a `.sourceignore` file or a file over 1 MiB is reported as `InputTooLarge`, since its size is not asked for.

### 8.2.2. Steps 4 and 5: the semantic diff check

**What it does.** It renders the Kustomization path twice, in process with the kustomize API: once from the files at the head of the target branch, and once with the patched file in their place. It passes only when both renders hold the same objects, every object other than the target is unchanged, and the target differs in exactly the action's field, from the previous to the desired value.

**Why.** A one-line edit can change more than it shows (a shared base, an overlay). The check proves the change does only what the plan says.

**It refuses, with a reason, when:**

- another object changed (for example through a shared base);
- an object was added or removed;
- another field of the target changed;
- an overlay overrides the value.

The changed object then gets basic schema checks:

- A Deployment or StatefulSet must convert strictly to its `apps/v1` type and keep its memory limit at or above its memory request.
- A ScaledObject must name its scale target and keep `maxReplicaCount` at or above `minReplicaCount`.

**Where it runs.** Steps 4 and 5 are built in `internal/gitops/semdiff`, with its own tests. The pull request step calls them before every commit and again whenever the target branch moves while the pull request waits (section 6). `Load` reads the files the Kustomization path (`status.source.buildPath`, recorded by the policy check) builds from, at the head of the target branch, the tree the change would merge into (bounded like the source mapping). When the path has no kustomization file, `Load` lists it as the source mapping does and adds the kustomization Flux would generate (`resources` only) to the files, so `Check` renders the same build Flux does. `Check` does the rendering and comparing.

**Rendering is locked down.**

- Every kustomization file may use only resources, components, patches and the built-in name, namespace, label, annotation, image and replica transformers.
- Every reference must be a plain relative path inside the repository, so no URL or remote base reaches kustomize.
- Generators, Helm charts, plugins and KRM functions are refused. Kustomize runs with built-in plugins only and Helm off, and its file system is in memory and holds only the files of the build.
- The walk counts directory visits, input bytes and YAML documents with repetition, so a diamond of overlays over a large base is refused before it is rendered once per path.
- Every file of the build (kustomization files, resource and patch files, inline patches) must be UTF-8 YAML without anchors, aliases or merge keys (a `<<` key or a key with the `!!merge` tag; a plain `<<` value is accepted); anything else is refused with `InvalidYAML`, the error naming the file and line (#488, #492). Kustomize replaces aliases by copies and resolves merges before it looks for Lists, so a few bytes could expand into thousands of objects (an alias bomb, or alias keys and merge tags that hide a List from a count), or, for an anchor that contains itself, recurse until the Go stack overflowed, which ends the process; and it also reads UTF-16 and UTF-32 with a byte order mark. Refusing them fails closed instead of mimicking Kustomize's YAML handling.
- A JSON patch (RFC 6902) `copy` operation in any patch, inline or from a file, YAML or JSON, is refused with `UnsupportedKustomization` before the render (#488): each copy can double a value, so 17 copies in about 1 KiB rendered 10 MB and 25 would reach a gigabyte, past every bound checked on the input. The check looks for an `op` key whose value is `copy` in every mapping of a patch, so a strategic merge patch that sets a field `op` to `copy` is refused too.
- A `.sourceignore` file anywhere on the build stops it, as in the source mapping.

**Limits.** Renders run one at a time. A caller waiting for its turn gives up when its context ends; a render that has started runs to its end.

**Render memory (measured 2026-11-07, issues #254 and #482).** A render runs in the executor's process, so the 128 Mi limit bounds the repository it can check. `hack/load/render` runs `Check` on a repository of ConfigMaps of 4 KiB each in a container limited to 128 MiB (Go 1.26.10, arm64). Before #482 (commit `b33301732e277695333e589befc9556607f59009`): a build of about 4 MiB and 1,021 objects peaked at 68 MiB resident and took 1.5 s; one of 7.8 MiB and 2,041 objects at 109 MiB and 4.7 s; a diamond (one 1 MiB base through 8 overlays) at 97 MiB and 8.6 s. The peak grows by about 13 MiB per MiB of input, and a build at the budget the code then allowed (16 MiB counted with repetition, 4,096 objects), flat or as a diamond, was killed by the memory limit (exit 137). A kill ends the executor's reconcile like a crash, so the request is tried again and fails the same way. **The supported repository is therefore a build of at most 4 MiB and 1,000 objects, and since #482 the limits are set to it: `MaxBuildBytes` 4 MiB (counted with repetition, its own budget, below the source mapping's 16 MiB read budget, which bounds reads, not a render) and `MaxObjects` 1,024.** The re-run at those limits (commit `b302bc300b65a148a844adc9c484d2c87b87e761` plus the change): a flat build of 3.9 MiB and 1,021 objects peaks at 69 MiB resident and takes 0.8 s, a diamond of 4 overlays over a 1 MiB base (1,024 objects) at 65 MiB and 0.9 s, so about 60 MiB stays for the rest of the executor. A build of 5 MiB, or the old 16 MiB one, flat or as a diamond, is now refused with `TooLarge` before kustomize runs (peak at most 67 MiB, the files held in memory). The budget is checked on the tree after the change too, every reference to the changed file included, so a value written one digit longer cannot take a build at the budget over it. The policy check loads the build with the same limits (`semdiff.Load`, at the commit the mapping read), so a build over its input limits ends `NeedsIntervention` with reason `TooLarge` on `PolicyAllowed` before any plan, approval or branch (the object limit is known only after a render). **Many small objects (#488).** The byte budget alone does not bound the number of objects: 1 MiB of ConfigMaps of about 78 bytes each (about 13,400 objects) peaked at 108 MiB and took 97 s before the render refused it, and 4 MiB of them (about 54,000) was killed (exit 137). The walk therefore also counts, with repetition per visit, the YAML documents of the resource and patch files and of inline patches, and refuses more than 4,096 (`MaxBuildDocuments`) with `TooLarge` before the render, in `Load` (so at the policy check too) and on the tree before and after the change. The count is an upper bound on the objects and List nodes Kustomize processes: each file is split at Kustomize's own separator (a line starting with `---`) and each part decoded as Kustomize decodes it, after the refusals above; every document counts 1, empty ones included, and a List (a mapping whose `kind` ends in `List`) counts 1 for itself plus its items, each counted the same way, so an empty List counts too and nested Lists add up. The two limits relate this way: since generators are refused, a render's objects come from the documents counted, each base once per visit, so the document count bounds the objects from above, and it also counts patches and empty documents. It is set higher than `MaxObjects` so that a build of 1,000 objects with some patches is not refused for its patches; the render's own check stays, and refuses 1,025 to 4,096 objects after kustomize has built them. Measured with `hack/load/render` in the same 128 MiB container (commit `2e426dad` plus the change): 4,096 documents of tiny ConfigMaps (shape `docs 4095`) peaks at 53 MiB before the render's object check refuses it; 4,096 ConfigMaps padded to just under 4 MiB, both input bounds at once (`heavy 4095`), at 75 MiB; 1,024 objects with 3,072 strategic merge patches of them (`patches 3072`, accepted) at 77 MiB, so about 50 MiB stays for the rest of the executor. Each took 3 to 15 s, depending on the load of the host, and a check renders twice. One document more, and 1 or 4 MiB of tiny ConfigMaps, are refused before the render (peak at most 35 MiB, the files held in memory). Helm values, safety rules and cost or capacity bounds are not checked yet (planned).

### 8.2.3. Steps 2, 3, 6 and 7: opening the change on GitHub

**What it does.** The executor signs, exchanges the token, and reads the head for the source mapping and the freshness check. In the pull request step (section 6) it creates the branch, commits and opens the pull request.

**Why it is safe to repeat.** The GitHub App key never leaves Key Vault ([ADR-0006](../adr/0006-github-app-key-in-key-vault.md)). Every write names the state it expects, so a restart or a race finds the existing work instead of repeating it.

**Planned.** Steps marked *(planned)* in the diagram are designed but not built, meaning not called by the running operator. Auto-merge is not built in any adapter: `EnableAutoMerge` returns an error and `Capabilities()` reports no auto-merge (#20).

```mermaid
sequenceDiagram
    participant E as Executor
    participant V as Key Vault
    participant G as GitHub
    E->>V: Sign the App JWT, RS256
    V-->>E: Signature
    E->>G: Exchange the JWT for a one-hour installation token, scoped to the one repository
    E->>G: Read the head SHA of targetBranch
    E->>G: Create branch shadewright/#lt;request-uid#gt; at that SHA
    Note over E,G: An existing branch at that SHA is success, at another SHA a conflict
    E->>G: Commit the file, expecting that head
    Note over E,G: A moved head is a conflict, never a force push
    E->>G: Open the pull request with the request UID and plan digest marker
    Note over E,G: An open pull request for the branch is returned, not duplicated
    alt Approval required
        G->>G: Wait for a human approval of the head commit
    else Policy allows automation
        E->>G: Enable auto-merge (squash) if the head is still our commit (planned)
    end
    opt Conflict, extra commits or a digest mismatch
        E->>E: Stop automation: Superseded or NeedsIntervention
    end
    opt Throttling or a transient error
        E->>E: Back off with jitter and Retry-After, a bounded number of times
    end
```

### 8.2.4. The GitHub adapter

**What it does.** The `GitProvider` adapter in `internal/providers/github` does the GitHub steps. The executor already uses it to read Git (head of the branch and files) for the source mapping and the freshness check, to write the branch, commit and pull request, and to close the pull request of a stale plan. It signs through Key Vault with workload identity. Auto-merge is not wired yet (planned).

**How providers are kept.** One provider per repository entry is kept across reconciles, so the token cache survives. It is built on first use from the entry (URL, installation ID, App client ID, key ID, target branch), rebuilt when the entry changes and dropped when it leaves the settings (`internal/providers/registry`).

**Limits.** The adapter is bound to one repository and its `targetBranch`, and refuses anything else before sending a request:

- **Branches.** It writes only to branches named `shadewright/<name>`. A new branch must start at the head of `targetBranch` or an ancestor of it, so a commit of a fork or another branch cannot become the base.
- **Commits.** The commit is built with the Git data API (blob, tree, commit, keeping the file mode) on top of the expected head. The branch moves with GraphQL `updateRefs`: an atomic compare-and-swap on the expected head, without force. A head that moved in any direction, a rewind to an ancestor included, is a conflict; the update never forces.
- **Pull requests.** They are opened, found and closed only when their head is a `shadewright/` branch of this repository and their base is `targetBranch`. A fork's pull request from a branch of the same name is ignored.
- **Reads.** Files are read through the commit's trees: only regular files, the size checked before the download, at most 1 MiB read, and the content checked against its Git object ID.
- **Errors.** They carry fixed text, the status code and their class (not found, conflict, too large, retryable). Never a URL, a header, response text, a repository or branch name, or a token.

**What can go wrong: transient failures.** Network errors, timeouts, 408, 429, 5xx and rate limits are retried with exponential backoff and jitter, at most four attempts.

- A delay GitHub asks for (`Retry-After`, `X-RateLimit-Reset` for an exhausted quota, or one minute for a secondary rate limit without headers) is waited for when it is at most 15 seconds.
- A longer delay goes back to the reconciler with the delay, so it requeues instead of blocking.
- Every attempt of a write (any request but a GET), the first and each retry, asks the kill switch's write checkpoint (5.4) right before it is handed to the HTTP client, after the token and the body. When it refuses, nothing more is sent in that call.

## 8.3. Auto-merge

**What it does.** The GitHub App enables auto-merge (GraphQL `enablePullRequestAutoMerge`, squash). GitHub then merges once required reviews and checks pass.

**When.** Only when policy allows automation, the head SHA is the commit Shadewright created, and the diff still matches the plan digest.

**Limits.** The repository must have "Allow auto-merge" on. Not built yet (planned, #20; see 8.2.3).

## 8.4. Branch protection

**Why.** Shadewright's own checks are not the last line of defence. The repository must refuse unsafe merges even if the agent misbehaves.

**What the repository needs:**

- Required status checks, including a trusted path guard that fails when a PR touches anything outside the allowed paths.
- `.github/`, `CODEOWNERS`, Flux config, RBAC and policy files are owned by a human, so a PR that touches them always needs review.
- Stale approvals dismissed on new pushes, and approval of the most recent push required.
- No direct pushes or force pushes to `main`; only branches and PRs.

**The GitHub App's limits:**

- It is never on a ruleset bypass list.
- It has only `contents: write`, `pull_requests: write`, `issues: write` (for notifications, ADR-0014) and `metadata: read`, on one repository.
- It cannot approve its own PRs.
- <span class="sw-status sw-planned">planned</span> Reviewer hints ([ADR-0028](../adr/0028-reviewer-hints-through-the-pull-request.md)) need no new permission: reading and posting comments and dismissing reviews are `pull_requests: write` and `issues: write`, reading a commenter's permission is `metadata: read`, and the update of the branch is one fast-forward commit under `contents: write`, with the old head as the expected parent. The executor dismisses approvals of earlier heads itself, so it does not depend on the stale-approval rule above (which stays as a second line).

**Scenario clusters.** On the clusters that run scenarios, the protected branch Flux tracks, and that the agent's PRs target, is `cluster/<cluster>`, not `main` ([ADR-0022](../adr/0022-scenario-switch-and-clusters.md)).

- Its rulesets mirror the PR rules of `main` for the agent's PRs: approval, code owner review and the required checks `Conventional PR title`, `Render manifests` and `Path guard`.
- Nobody can force-push or delete it.
- Known limit for v0.1.0, with a planned follow-up: the repository admin role can still push to it directly.

**What can go wrong.** Extra commits, a changed target or a digest mismatch stop automation.

## 8.5. Details

| Topic | Detail |
| --- | --- |
| Source mapping budget | 16 MiB read in all (kustomization and `.sourceignore` lookups, cached reads and `ListDir` listings at 128 bytes per entry included), bounding the file contents downloaded, cached and parsed (the tree listings inside a `ReadFile` call are bounded per call and by the read limit); 4096 YAML documents decoded |
| Render check budget | 4 MiB of build input and 4,096 YAML documents read before the render, both counted with repetition per directory visit, and 1,024 rendered objects (`semdiff.MaxBuildBytes`, `semdiff.MaxBuildDocuments`, `semdiff.MaxObjects`); past any, the render check refuses with `TooLarge` (8.2.2); the input limits are checked at the policy check too, before a plan is made |
| Directory listings | `ListDir` of one directory, at most 1000 entries; a path without a kustomization file is listed 5 directories deep at most |
| YAML documents counted | Ones holding only comments or blank lines are counted; a separator with nothing before it is skipped by the reader and never decoded |
| File size limit | 1 MiB per file, in the source mapping and in the adapter's reads |
| Reason when a budget runs out | `InputTooLarge`, a `NeedsIntervention` |
| Adapter retries | Four attempts, exponential backoff with jitter |
| Longest wait kept in the call | 15 seconds; longer goes back to the reconciler |
| Secondary rate limit without headers | Delay of one minute (over 15 s, so the call returns it to the reconciler) |
| Installation token | One hour, scoped to the one repository |
| Auto-merge method | Squash |
