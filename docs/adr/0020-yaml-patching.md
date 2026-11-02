# ADR-0020: Patching YAML by replacing one scalar in place

- Status: Proposed
- Date: 2026-11-02

## Context

The executor changes one field of one manifest per request ([design 8.2](../design.md#82-making-a-change), step 3): a container memory limit or a KEDA `maxReplicaCount`. The file is in the user's GitOps repository, and a person reviews the pull request. A good patch changes the one value and nothing else: comments, key order, blank lines, quoting and the other documents in the file stay as they were, so the diff shows exactly the change the policy approved ([ADR-0003](0003-gitops-only-typed-remediation.md)). [ADR-0012](0012-object-to-source-mapping.md) already guarantees that the file holds exactly one plain declaration of the object and that the value in it equals the live value.

The design left the library open, leaning towards `kyaml`. Four approaches were compared with a spike (`hack/yamlpatch-spike`, its own Go module, not shipped). It patches `resources.limits.memory` of container `web` from `512Mi` to `768Mi` in a two-document file with comments, a blank line, a flow map and sequence, quoted and literal block scalars, a non-indented sequence and an anchor, then decodes both files to check that only the target value changed. A second sample puts the target inside an anchored map that another container reuses through an alias, where any edit also changes the alias.

| | Typed round-trip (`sigs.k8s.io/yaml` v1.6.0) | `kyaml` v0.21.2 | `goccy/go-yaml` v1.19.2 | Splice (`go.yaml.in/yaml/v3` v3.0.6) |
| --- | --- | --- | --- | --- |
| Lines changed in the diff | 40 | 4 | 4 | 2 |
| Comments | All lost | Kept, spacing before them collapsed | The one on the changed line lost (with the path replace the spike uses) | Kept |
| Key order, blank lines, quoting | Reordered, flow styles and quotes rewritten | Blank line dropped | Spacing before a comment collapsed | Unchanged |
| Other data changed | No | No | No | No |
| Target inside an anchor | Patched; anchor and alias replaced by two separate copies | Patched, and the alias changed silently | Refused, because its path query cannot pass an anchor | Refused on purpose |
| Multi-document files | Manual split | Yes | Yes | Yes |
| Licence | MIT and BSD-3-Clause | Apache-2.0 | MIT | MIT and Apache-2.0 |
| New modules for the operator | None | 4 (`kyaml`, `go-errors`, `go-gitignore`, `treeprint`) | None (already in the module graph) | None (already indirect) |
| Last release | 2025-07 | 2026-10 | 2026-01 | 2026-10 |

## Decision

Patch by replacing the bytes of one scalar:

1. Parse the file with `go.yaml.in/yaml/v3` (the maintained successor of `gopkg.in/yaml.v3`) into nodes, only to find the document and the scalar node of the field, using the same identity as the source mapping (apiVersion, kind, namespace and name; exactly one container of that name). The node tree is used only for positions; the source mapping stays the authority on which document declares the target.
2. Refuse with `NeedsIntervention` when the scalar or any node on its path has an anchor or is an alias, when there is a merge key (`<<`) or a flow-style collection on the path, when a path mapping repeats a key, or when the scalar is not plain, single-quoted or double-quoted (no block or flow scalar, no tag). The same applies to the identity fields of every document in the file (`apiVersion`, `kind`, `metadata.name`, `metadata.namespace`), so another declaration of the target cannot hide behind an alias, a merge key or a tag such as `!!binary`.
3. Turn the node's line and column into a byte offset, check that the bytes there are exactly the old value in its quoting, and replace only those bytes, keeping the quote style. Columns count characters, not bytes; if the check fails, refuse instead of guessing. This is a check of the position only: whether the file still holds the request's previous value is decided by value, as in [ADR-0012](0012-object-to-source-mapping.md) (`256Mi` equals `268435456`). The new value is written in its canonical form: `Quantity.String()` for memory, a decimal integer for replicas. A plain scalar that YAML reads as a number other than its text (a leading zero, `0x`, `0o`, an underscore or an exponent) is refused.
4. Decode the original and the patched file and require that they differ only in the target value. This also catches any case the rules above miss, such as an alias elsewhere that points into the target. A parse error in either file, or a target whose decoded value is not the desired value, refuses with `NeedsIntervention`.

The code lives in `internal/gitops/patch` (#55); the spike stays under `hack/` as the record of the comparison.

## Consequences

- The pull request diff is one line, so a reviewer sees exactly the approved change, and comments such as `# raised from 384Mi on 2026-10-14` stay attached to the line.
- No new dependency: `go.yaml.in/yaml/v3` is already in the module graph.
- The patcher handles only scalar replacement. Adding a field that does not exist yet, or editing a list, would need a different technique. That fits the closed action catalog: every action changes an existing scalar, and ADR-0012 already requires the old value to be present. A future action that needs more must revisit this ADR.
- Some valid files are refused (anchors, block scalars, tags on the target). They stop with `NeedsIntervention`, never with a wrong edit.
- The semantic check decodes the file twice; the file is at most 1 MiB, so this is cheap.

## Alternatives considered

- **`kyaml`**, the earlier leaning. It keeps comments and is well maintained, but it rewrites the file through its own encoder: it dropped a blank line and changed comment spacing, and it changed an aliased value without warning. It also adds four modules.

```diff
@@ -23,7 +23,6 @@
   selector:
     matchLabels:
       app: orders-worker
-
   template:
     metadata:
       labels:
@@ -31,13 +30,13 @@
     spec:
       containers:
       - name: web
-        image: ghcr.io/example/orders-worker:1.4.2   # pinned by the release job
+        image: ghcr.io/example/orders-worker:1.4.2 # pinned by the release job
         args: ["--queue", "orders", "--concurrency=8"]
         resources:
           requests: &req
             memory: 256Mi
           limits:
-            memory: "512Mi" # raised from 384Mi on 2026-10-14
+            memory: "768Mi" # raised from 384Mi on 2026-10-14
       - name: sidecar
         image: ghcr.io/example/log-shipper:0.9.0
         resources:
```

- **Typed round-trip** with `sigs.k8s.io/yaml`. The data survives, but it loses every comment, reorders keys, rewrites flow and quoting styles and turns anchors into copies: a 40-line diff for a one-value change. Unusable for files people maintain.

```diff
@@ -1,44 +1,47 @@
-# Orders worker: memory raised after the October load test.
 apiVersion: v1
-kind: ConfigMap
-metadata:
-  name: orders-worker
-  namespace: orders
 data:
-  # Literal block: must stay byte for byte.
   worker.conf: |
     batch = 50
       indent = kept
+kind: ConfigMap
+metadata:
+  name: orders-worker
+  namespace: orders
 ---
 apiVersion: apps/v1
 kind: Deployment
 metadata:
+  annotations:
+    note: single quoted
+  labels:
+    app: orders-worker
+    tier: backend
   name: orders-worker
...
```

- **`goccy/go-yaml`.** Its AST keeps comments and positions, but replacing a node through a path dropped the comment on the changed line and collapsed the spacing before another comment, and its path language has no filter by name, so the container index must be looked up first. Its token API could do a splice too, but `go.yaml.in/yaml/v3` is already a dependency and the splice needs only positions.

```diff
@@ -31,13 +31,13 @@
     spec:
       containers:
       - name: web
-        image: ghcr.io/example/orders-worker:1.4.2   # pinned by the release job
+        image: ghcr.io/example/orders-worker:1.4.2 # pinned by the release job
         args: ["--queue", "orders", "--concurrency=8"]
         resources:
           requests: &req
             memory: 256Mi
           limits:
-            memory: "512Mi" # raised from 384Mi on 2026-10-14
+            memory: "768Mi"
       - name: sidecar
         image: ghcr.io/example/log-shipper:0.9.0
         resources:
```

- **Splice** (chosen), for comparison:

```diff
@@ -37,7 +37,7 @@
           requests: &req
             memory: 256Mi
           limits:
-            memory: "512Mi" # raised from 384Mi on 2026-10-14
+            memory: "768Mi" # raised from 384Mi on 2026-10-14
       - name: sidecar
         image: ghcr.io/example/log-shipper:0.9.0
         resources:
```
