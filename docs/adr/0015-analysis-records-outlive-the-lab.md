# ADR-0015: Analysis records are kept and outlive the lab

- Status: Accepted
- Date: 2026-11-01

## Context

Model transcripts (context sent, tool calls, outputs, tokens) are needed for audit, for debugging bad proposals, and for building the evaluation set. The lab cluster is destroyed after every session.

## Decision

- The analyzer writes one record per analysis, after redaction, through the `AnalysisStore` interface.
- In the lab the store is a blob container in the `foundation` Terraform root, not in `lab`, so records survive `terraform destroy` of the cluster. A lifecycle rule deletes records after a fixed retention.
- The analyzer's identity may only create blobs in that container. Nothing else in the cluster can read them.
- In other clusters the store is on by default and configurable; only redacted data is ever written.

## Consequences

- Real incidents from the lab become evaluation fixtures.
- Storage cost is negligible at lab volume, but retention must be set.
- The deployed role does not yet enforce create-only: see the amendment of 2026-11-04.
- A redaction bug would now persist data, so redaction gets its own tests and is applied before both the model call and the write.

## Alternatives considered

- Drop transcripts after analysis: no audit trail and no evaluation data.
- Store them in the cluster: lost with the lab, and too large for etcd.

## Amendments

- 2026-11-04: the Decision (create-only) is unchanged, but the analysis writer role deployed in `shadewright-azure` does not enforce it. It grants `blobs/write`, which can overwrite an existing blob. Blob versioning is on, so an overwritten record keeps its earlier content as a version until retention removes it: today the guarantee is versioning as recovery evidence, not create-only. Making the role create-only is tracked in [#154](https://github.com/pkalin/shadewright/issues/154); immutable blob storage is an option there ([#244](https://github.com/pkalin/shadewright/issues/244)).
- 2026-11-17 ([#845](https://github.com/pkalin/shadewright/issues/845)): the Decision is unchanged; this records how redaction, which every record and every model call relies on, is applied. Redaction decides once, on one canonical view of the text as it was read: every Unicode space, control and format character and every invalid byte counts as indentation, LF, CRLF and a lone CR end a line, and nothing is replaced, escaped or cut to make the view. Every rule reports spans of the original bytes, and the spans are merged and replaced once, so no rule reads another's output. No caller redacts text it has already encoded, escaped, cut or repaired; a caller that holds a sequence of strings redacts it as one (`redact.Strings`). The two remaining second looks are on text that only lost characters: the context builder and the log tool redact the excerpt of any `LogSource` at their boundary, since a source may not redact, and the fingerprint redacts its stored symptom until it no longer changes (design 10.3). Alternative not taken: more passes in a fixed order, each redacting what an earlier step had escaped, flattened, repaired or cut. #840 went that way, and each new step needed its own pass and its own place in the order; a form one pass rewrote (a `>` sanitized to `)`, a line break escaped by JSON, a key line dropped by a cut) hid a secret from the next pass, and the result depended on the order, which no test can cover for every caller. One view read once has no order to get wrong, and its cost is counted (`redactwork`): about a quarter more work per byte than before, within the same budget of 1,024 steps per byte.
