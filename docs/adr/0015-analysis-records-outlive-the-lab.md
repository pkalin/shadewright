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
- A redaction bug would now persist data, so redaction gets its own tests and is applied before both the model call and the write.

## Alternatives considered

- Drop transcripts after analysis: no audit trail and no evaluation data.
- Store them in the cluster: lost with the lab, and too large for etcd.
