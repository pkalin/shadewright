# ADR-0019: Packaging extensions: compiled in now, signed OCI packs later

- Status: Accepted
- Date: 2026-11-02

## Context

[ADR-0018](0018-three-axes-of-pluggability.md) makes platforms and integrations pluggable. They have to reach a running operator somehow, without weakening the closed action catalog (every action is a typed, schema-validated, tested API value) or the rule that cluster data is untrusted.

Options in the ecosystem:

- Go's `plugin` package: fragile (exact toolchain and dependency match), effectively unused in operators.
- Out-of-process plugins over gRPC, as Terraform providers do: robust, but a new trust boundary and a lot of machinery.
- OCI artifacts. Kubernetes can mount an OCI image as a read-only, non-executable volume (`image` volume source, KEP-4639: alpha in 1.31, beta in 1.33, stable in 1.36). Containerd needs 2.1 or later, and support for non-image artifacts varies. Independently of that feature, an operator can pull an OCI artifact by digest itself and verify its signature.

## Decision

1. **Now:** platforms and integrations are Go packages compiled into the operator and registered at start-up, like `database/sql` drivers. The action catalog is closed per release; extending it is a pull request.
2. **Later, when outside packs are needed:** declarative extension packs shipped as OCI artifacts, pinned by digest and verified with a cosign signature that the cluster admin trusts. A pack contains data, never code:
   - detection rules and diagnosis rules in CEL over object fields and conditions;
   - read-only queries for the analyzer (Kubernetes reads, PromQL templates);
   - redaction patterns and verification checks;
   - action definitions limited to a small set of built-in patch primitives (for example "set an integer field at this path within bounds"), each with a JSON schema for its parameters and a `ValidatingAdmissionPolicy` so the API server still enforces the bounds.
   Packs are loaded either from an image volume (on clusters that support it) or pulled by the operator. Interfaces in step 1 are shaped so a compiled module can later be expressed as a pack.

## Consequences

- Today nothing changes for safety: every action is still compiled, typed and tested.
- Outside teams will eventually be able to add knowledge about their add-ons without forking the operator and without running their code inside it.
- The declarative format is a real design task (what CEL can express, how to test a pack); it is deferred until there is demand.

## Alternatives considered

- **Go plugins**: rejected, too fragile.
- **gRPC plugin processes**: kept in reserve if packs ever need logic that CEL cannot express.
- **Packs with executable content (WebAssembly)**: possible later for diagnosis only; not for actions.

## Amendments

- 2026-11-03: "registered at start-up" means the binary builds one immutable registry from an explicit list of compiled-in modules. There is no global registry and no `init()` registration, so a duplicate name is an error from the constructor instead of a panic at load, and tests can build registries freely ([#214](https://github.com/pkalin/shadewright/pull/214), design 2.4). The decision is unchanged.
