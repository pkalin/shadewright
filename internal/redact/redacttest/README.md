# Shared corpus helpers

`EntryCorpus`, `CanonCorpus`, `CanonIndex`, `RaceKeeps` and `ParallelUnderRace`
work in a consumer's test binary. They do not import `redact` or use its
`export_*_test.go` files. The generators, seeds, sampling rates and fixture hashes
are unchanged. `corpus_test.go` compares all helper fields with digests captured
from `origin/main` at `1f672163`, including full, short and race selections.
Existing redaction tests and their fixtures stay in `internal/redact`; its
forwarding names keep those tests running until the entry-point tests move.
The raw cases, builders and generator data also support those forwarding names.

For ADR-0048 point 9, the added imports are `hash/fnv`, `math/rand/v2` and `slices`.
The external helper tests also add `encoding/json`. All other imports were
already on the `redacttest` list. No Kubernetes dependency is added.

The block generator classifies its fixed corpus keys with its own bare-key list.
`TestBlockCorpusKeyClasses` checks agreement with the redactor for every secret
and innocent corpus key. No parser, matcher or redaction rule moves or changes.
