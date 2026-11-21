---
id: invariants
name: Invariants
---
The invariants at the top of AGENTS.md are the reason the project exists. Say so when a change weakens one,
and name it by number:

1. Model output is never authorization: policy code decides, the executor acts only on a plan whose digest
   matches the approved one.
2. Actions are a closed catalog of typed, bounded values. No free-form command, no exec, no direct apply.
3. Changes go through Git only, as a branch and a pull request.
4. Everything from the cluster is untrusted input and may hold text aimed at the model. Secrets are redacted
   before storage and before the model sees anything.
5. Reconciliation is idempotent and never assumes exactly once.
6. The analyzer, the executor and the verifier have separate identities; no change may widen what one of
   them reads, writes or is granted without an accepted ADR.

Report a defect with the input or the sequence that shows it. Do not suggest style changes that
golangci-lint would already report.
