# YAML patching spike

The comparison behind [ADR-0020](../../docs/adr/0020-yaml-patching.md). Not shipped: it is its own Go module, so the
candidate libraries stay out of the operator's `go.mod`, and `make lint test` does not build it.

```bash
cd hack/yamlpatch-spike
go run .                     # writes out/<sample>-<approach>.yaml and prints the semantic check
diff -u testdata/sample.yaml out/sample-splice.yaml
```

`testdata/sample.yaml` is the main case. `testdata/anchor.yaml` puts the target inside an anchored map that another
container reuses through an alias; a correct patcher refuses it.
