# ADR-0015: Analysis records are kept and outlive the lab

- Status: Accepted
- Date: 2026-11-01

## Context

Model transcripts (context sent, tool calls, outputs, tokens) are needed for audit, for debugging bad proposals, and for building the evaluation set. The lab cluster is destroyed after every session.

## Decision

- The analyzer writes one record per analysis, after redaction, through the `AnalysisStore` interface.
- In the lab the store is a blob container in the `foundation` Terraform root, not in `lab`, so records survive `terraform destroy` of the cluster. A lifecycle rule deletes records after a fixed retention.
- The analyzer's identity may write blobs in that container, but not read, list or delete them. Blob versioning is on, so a blob that is overwritten keeps its earlier content as a version until retention removes it. Nothing else in the cluster can read them.
- In other clusters the store is on by default and configurable; only redacted data is ever written.

## Consequences

- Real incidents from the lab become evaluation fixtures.
- Storage cost is negligible at lab volume, but retention must be set.
- A redaction bug would now persist data, so redaction gets its own tests and is applied before both the model call and the write.

## Alternatives considered

- Drop transcripts after analysis: no audit trail and no evaluation data.
- Store them in the cluster: lost with the lab, and too large for etcd.

## Amendments

- 2026-11-04: the analysis writer role deployed in `shadewright-azure` grants `blobs/write`, which can overwrite an existing blob; Azure has no data action that creates only. The current guarantee is therefore versioning as recovery evidence (an overwritten record stays readable as a version), not create-only. Making the role create-only is tracked in [#154](https://github.com/pkalin/shadewright/issues/154); until it is fixed the Decision states what is deployed ([#244](https://github.com/pkalin/shadewright/issues/244)).
