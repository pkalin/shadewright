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
- 2026-11-17 ([#845](https://github.com/pkalin/shadewright/issues/845)): the Decision is unchanged; this records how redaction, which every record and every model call relies on, is applied. Redaction decides once, on one canonical view of the text as it was read: every Unicode space, control and format character and every invalid byte counts as indentation, LF, CRLF and a lone CR end a line, and nothing is replaced, escaped or cut to make the view. Every rule reports spans of the original bytes, and the spans are merged and replaced once, so no rule reads another's output. No caller redacts text it has already encoded, escaped, cut or repaired; a sequence of strings is decided as a whole: a caller that holds the strings (`redact.Strings`, also the name and value of a metric label in `query_metrics`) gets the decision String takes on the strings joined by line breaks, mapped back to each string, so what String hides in the strings joined, Strings hides in them apart by construction, and one function adds what a joined text does not show as a key and value, for that caller and for a sequence written in a text. The rules also read a folded view where a reader sees another text than the bytes (an invisible character inside a key name or before its separator, a colon look-alike from Unicode's confusables list, an explicit YAML key, a separator of other characters after a secret key; the invisible characters and the colon look-alikes are classes generated from the Unicode data, not lists of forms found so far), in a text and in each string of a sequence, and a match on it counts on the bytes it was folded from; this is a second reading of the same text, not a pass over another's output. The two remaining second looks are on text that only lost characters: the context builder and the log tool redact the excerpt of any `LogSource` at their boundary, since a source may not redact, and the fingerprint redacts its stored symptom until it no longer changes (design 10.3). Alternative not taken: more passes in a fixed order, each redacting what an earlier step had escaped, flattened, repaired or cut. #840 went that way, and each new step needed its own pass and its own place in the order; a form one pass rewrote (a `>` sanitized to `)`, a line break escaped by JSON, a key line dropped by a cut) hid a secret from the next pass, and the result depended on the order, which no test can cover for every caller. One view read once has no order to get wrong, and its cost is counted (`redactwork`): about three quarters more work per byte than main on the corpus of #845 (46.9 steps against 27.3 on its 14,310 inputs, 1.72 times, as `TestCanonCorpusWorkIsLinear` prints them), within the same budget of 1,024 steps per byte.
- 2026-11-18 ([#845](https://github.com/pkalin/shadewright/issues/845), third decision): the Decision is unchanged. A tool result is decided as one text: each read tool lays out the values it shows (the fields in document order as `name: value` lines, the strings of a list as lines of their own, nothing escaped or encoded), `redact.Pieces` takes the one decision on that text, and each value gets its part back before the result is encoded. No field, list or map is decided by itself, and the encoded answer is not redacted again, so a key in one field and its value in a sibling or the next field (an HTTP header's name and value, a flag at the end of a command and its value in the arguments) are seen together, as main's second pass over the whole answer saw them. The string after a secret flag is always its value (`["--token", "--verbose"]` hides `--verbose`). Cost, as the tests print it with `redactwork`: `get_resource` on a pod of 64 and 256 containers takes 30.5 and 30.6 steps per byte of the object as JSON (`TestGetResourceWorkIsLinear`), against 15.0 when each field was decided apart and 57.7 on main 7e5cedb1 with its second pass (the same test run at those commits); `{password: marigold` followed by 512, 1,024 and 2,048 closing braces takes 21,325, 41,805 and 82,765 steps (`TestTokenShapesWorkIsLinear`; 152,137, 565,577 and 2,178,889 before, when each brace read the rest of the line again).
