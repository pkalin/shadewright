---
title: Scoring intake reference
---

# Scoring intake reference

`cmd/intake` scores one analysis record written by an analyzer we do not run ([ADR-0047](../adr/0047-scoring-analysis-records-outside-the-cluster.md)). It is not part of an installation: the chart, the Azure module and the install guides do not use it, and nobody who installs Shadewright needs it. The code is `internal/intake`.

## What is built

<span class="sw-status sw-built">built</span> (#989): the command reads one record from a file and writes one score JSON. It runs on a laptop against downloaded copies of records, with the keys of whoever runs it (ADR-0047 section 1; never the intake's keys from the vault, which only the job gets, and never the analyzer's or the lab's) or with `--dry-run`, and keeps its output local.

<span class="sw-status sw-planned">planned</span>: the blob source, the scores and quarantine containers, the queue trigger, the Container Apps job and its Terraform (#991), the image (#990), the comparison with the 2026-11-20 scorecard (#992), which also measures the share of those records the redactor check quarantines although they hold no secret (ADR-0047 known limit (d)). That share is not measured here: it needs the records themselves.

## What one run does

In this order, each check final (ADR-0047 section 3). Steps 1 to 6 run before any model call; the check of step 5 runs again before each later call:

1. The file's base name must be `<record ID>.json`, the ID as the analyzer makes it: the Incident UID as a lower-case UUID, a dash, and a valid UTC time as `20060102t150405z`. Any other name is quarantined without being read; the score holds the name's SHA-256, not the name.
2. A file over 1 MiB (the analyzer store's limit) is quarantined without being read. The read stops at the limit plus one byte, and a file that changed since its size was read is a failed read.
3. The schema: `schemaVersion` is checked first and must be one the intake knows (1). Then no object may hold a key twice, compared after unescaping and without regard to case (`"summary"`, `"\u0073ummary"` and `"Summary"` are one key), and nothing may nest deeper than 16 levels: a decoder that keeps the first of two members and one that keeps the last would read different records. Then the bytes must decode into the record type with no unknown field and nothing after the object, the `id` must be the one of the name, and `incident.uid` must be the UID in that `id`.
4. The redactor runs again over every string, every key, and every scalar member as `key: value`, as the analyzer's store does before its write. It keeps only the record's ID, its fingerprint (`sha256:` and 64 hex digits), the Incident's name when `ParseIncidentName` gives that fingerprint, and the request's name when it is `remediation-` and 24 hex digits; a value that fails its shape check quarantines the record. Any other match quarantines the record with the rule kinds that matched. The same check runs once more over the values the judge is given (step 7), built from the decoded record.
5. Each text built from the record for a model (the judge's values, the cross-check question) has every whole occurrence of the record ID, the Incident's name, the request's name and the fingerprint (with or without `sha256:`) replaced by the fixed words `[record-id]`, `[incident-name]`, `[request-name]` and `[fingerprint]`, which the judge's prompt explains. "Whole" is what the redactor's keep list takes as whole: no letter, digit or one of `+/_-=.` on either side. So `incident-<digits>` followed by `-2`, by a full stop or by other text is not replaced, and step 4 has already quarantined such a record. The check that decides comes last, where each request to a model is built: the redactor's `Clean` on every message exactly as it is sent (after formatting, with its delimiters), on the tool calls and on the tool definitions, with nothing kept. A name right after a secret-named flag (`--token <name>`), or a secret-named flag at the end of one field with a value at the start of the next, quarantines the record. Only a request that passed this check can reach a model. Nothing is redacted and passed on.
6. The record's `models` decide its family by a fixed table of ID prefixes (`claude-` is Anthropic; `gpt-`, `o1`, `o3`, `o4` are OpenAI). No model, an unknown model, models of both families, or an OpenAI model under an `Anthropic` provider make the record `unjudged`, with no model call.
7. The judge is one call to the model of the other family, with a fixed prompt (`internal/intake/judge_prompt.txt`, version in `PromptVersion`) and an allowlist of fields in a delimited block: the Incident's reason and subject kind, the summary, the missing data, `escalated` and the call counts. The answer must match a schema, with property names in exactly this case and none twice: four integer scores from 0 to 5 (`specific`, `consistent`, `causeSupported`, `missingData`), a verdict (`useful`, `partial`, `unhelpful`, `misleading`) and an optional note of at most 300 bytes, kept only if the redactor finds nothing in it. An answer that does not match is `invalid` and not retried.
8. The judge's one tool, `cross_check`, asks a model of the record's family one question of at most 500 bytes. Only the first ask is served: a second ask in the same reply gets a fixed refusal, and an ask in the reply after the tool results makes the score `invalid`. A question the redactor would change, alone or as sent, is not sent and still counts as the ask. The answer is redacted and cut to 2,000 bytes, and the judge's second request is checked as sent (step 5): if it fails, nothing more is sent and the record is `quarantined`. That request also hands the judge's own reply back to it, so a refused question in that reply ends the run the same way.

## Token caps

Before each call (the judge, the cross-check, the judge with the tool result) the intake reserves that call's maximum, its estimated input and its output limit, and makes the call only when no cap would be passed: the call cap, the run cap (the run's reservations together), and the month cap of the call's family, summed from the names of that month's reservations. The month is the intake's own clock, read once per run. At the judge's cap the outcome is `budget` and no call is made; at the cross-check family's cap the judge gets a fixed refusal and answers without it. A reservation is never released: a call whose reply never arrives was counted at its maximum. Each call is one attempt: the adapters are built without automatic retries, so no attempt is billed outside its reservation. A failed call ends the run with exit code 1, and the next run reserves again under its own run ID.

The `reason` of a `budget` outcome names the cap, and every `budget` outcome logs a warning with the family and the month. Only the month cap clears by itself, in the next month. A call over the call cap or the run cap gets `budget` on every run, because the same record makes the same request: such a record is scored only after the cap is raised with `--max-call-tokens` or `--max-run-tokens`.

Reservations are empty files in `--spend-dir`, `spend/<month>/<family>/<run>.<call>.<tokens>.json`, created only if absent. The run ID is random per run and stands for the message ID and dequeue count of the job's names (ADR-0047 section 5), which a file run does not have. A file there that is not a reservation stops the run with an error, because the month's sum cannot be trusted.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--record` | required | Path of the record file, named `<record ID>.json`. |
| `--spend-dir` | required | Directory of the spend reservations, kept across runs. |
| `--out` | `-` | Path of the score JSON, created only if absent, or `-` for standard output. A score file that exists ends the run without a model call, whatever record it scored; with `-` nothing is skipped, and a second run of the same record spends again. Naming scores by record ID and the hash of the bytes comes with the scores container. |
| `--anthropic-model` | | Model ID of the Anthropic family: the judge of OpenAI records and the cross-check of Anthropic ones. |
| `--anthropic-key-file` | | File with the Anthropic API key of whoever runs the command, read at each call. |
| `--openai-model` | | Model ID of the OpenAI family: the judge of Anthropic records and the cross-check of OpenAI ones. |
| `--openai-key-file` | | File with the OpenAI API key of whoever runs the command, read at each call. |
| `--openai-endpoint` | `https://api.openai.com/v1` | HTTPS API base of the OpenAI family. |
| `--max-call-tokens` | 40000 | Cap of one call. |
| `--max-run-tokens` | 100000 | Cap of one run. |
| `--max-month-tokens` | 4000000 | Cap of a month, for each family. |
| `--dry-run` | false | A built-in fake model of each family answers a fixed score: no key, no network. Reservations are still counted. |

Both families need a model and a key file unless `--dry-run` is set. The two key files must differ; each adapter refuses the other's file. The keys are those of whoever runs the command (ADR-0047 section 1), read from the files at each call; never the intake's keys from the vault, which only the job gets through its identity (section 6), and never the analyzer's or the lab's.

## Output and exit codes

The score JSON has `status` (`scored`, `unjudged`, `invalid`, `quarantined` or `budget`), the `check` and the fixed `reason` of a refusal, the redactor's rule `kinds`, the record's ID and SHA-256, the month, the record's and the judge's family, the judge's model, `crossCheck` (`answered`, `notSent` or `budget`; absent when not asked), the prompt version, the `answer`, the tokens `reserved` and the `usage` the providers reported. It holds no record text: every string in it is fixed by code, a checked shape, or the judge's note after the redactor found nothing in it. In the job, `quarantined` and `budget` go to the quarantine container and the `budget/` prefix (ADR-0047 section 5); a file run writes them as a score with that status.

| Exit | Meaning |
| --- | --- |
| 0 | An outcome was written, or the score file existed already. |
| 1 | A step failed (a read, a reservation, a model call, the run's deadline of 8 minutes): try again. No score was written; the reservations made before the failed step stay in `--spend-dir` and count against the caps. |
| 2 | A flag or the configuration is wrong. |

Example, on a downloaded record:

```bash
go run ./cmd/intake --record ./records/<record ID>.json --spend-dir ./intake-spend --dry-run
```
