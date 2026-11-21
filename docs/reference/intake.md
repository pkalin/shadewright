---
title: Scoring intake reference
---

# Scoring intake reference

`cmd/intake` scores one analysis record written by an analyzer we do not run ([ADR-0047](../adr/0047-scoring-analysis-records-outside-the-cluster.md)). It is not part of an installation: the chart, the Azure module and the install guides do not use it, and nobody who installs Shadewright needs it. The code is `internal/intake`.

## What is built

<span class="sw-status sw-built">built</span> (#989): the command reads one record from a file and writes one score JSON. It runs on a laptop against downloaded copies of records, with the maintainer's own keys or with `--dry-run`, and keeps its output local.

<span class="sw-status sw-planned">planned</span>: the blob source, the scores and quarantine containers, the queue trigger, the Container Apps job and its Terraform (#991), the image (#990), the comparison with the 2026-11-20 scorecard (#992).

## What one run does

In this order, each check final and none after a model call (ADR-0047 section 3):

1. The file's base name must be `<record ID>.json`, the ID as the analyzer makes it (the Incident UID, a dash, the UTC time as `20060102t150405z`; at most 64 bytes of `0-9`, `a-f`, `-`, `t`, `z`). Any other name is quarantined without being read; the score holds the name's SHA-256, not the name.
2. A file over 1 MiB (the analyzer store's limit) is quarantined without being read. The read stops at the limit plus one byte, and a file that changed since its size was read is a failed read.
3. The schema: `schemaVersion` is checked first and must be one the intake knows (1); then the bytes must decode into the record type with no unknown field and nothing after the object, and the `id` must be the one of the name.
4. The redactor runs again over every string, every key, and every scalar member as `key: value`, as the analyzer's store does before its write. It keeps only the record's ID, its fingerprint (`sha256:` and 64 hex digits), the Incident's name when `ParseIncidentName` gives that fingerprint, and the request's name when it is `remediation-` and 24 hex digits; a value that fails its shape check quarantines the record. Any other match quarantines the record with the rule kinds that matched. Nothing is redacted and passed on.
5. The record's `models` decide its family by a fixed table of ID prefixes (`claude-` is Anthropic; `gpt-`, `o1`, `o3`, `o4` are OpenAI). No model, an unknown model, models of both families, or an OpenAI model under an `Anthropic` provider make the record `unjudged`, with no model call.
6. The judge is one call to the model of the other family, with a fixed prompt (`internal/intake/judge_prompt.txt`, version in `PromptVersion`) and an allowlist of fields in a delimited block: the Incident's reason and subject kind, the summary, the missing data, `escalated` and the call counts. The answer must match a schema: four integer scores from 0 to 5 (`specific`, `consistent`, `causeSupported`, `missingData`), a verdict (`useful`, `partial`, `unhelpful`, `misleading`) and a note of at most 300 bytes, kept only if the redactor finds nothing in it. An answer that does not match is `invalid` and not retried.
7. The judge's one tool, `cross_check`, asks a model of the record's family one question of at most 500 bytes. Only the first ask is served: a second ask in the same reply gets a fixed refusal, and an ask in the reply after the tool results makes the score `invalid`. A question the redactor would change is not sent and still counts as the ask. The answer is redacted and cut to 2,000 bytes before the judge sees it.

## Token caps

Before each call (the judge, the cross-check, the judge with the tool result) the intake reserves that call's maximum, its estimated input and its output limit, and makes the call only when no cap would be passed: the call cap, the run cap (the run's reservations together), and the month cap of the call's family, summed from the names of that month's reservations. The month is the intake's own clock, read once per run. At the judge's cap the outcome is `budget` and no call is made; at the cross-check family's cap the judge gets a fixed refusal and answers without it. A reservation is never released: a call whose reply never arrives was counted at its maximum.

Reservations are empty files in `--spend-dir`, `spend/<month>/<family>/<run>.<call>.<tokens>.json`, created only if absent. A file there that is not a reservation stops the run with an error, because the month's sum cannot be trusted.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--record` | required | Path of the record file, named `<record ID>.json`. |
| `--spend-dir` | required | Directory of the spend reservations, kept across runs. |
| `--out` | `-` | Path of the score JSON, created only if absent, or `-` for standard output. A score file that exists ends the run without a model call. |
| `--anthropic-model` | | Model ID of the Anthropic family: the judge of OpenAI records and the cross-check of Anthropic ones. |
| `--anthropic-key-file` | | File with the intake's own Anthropic API key, read at each call. |
| `--openai-model` | | Model ID of the OpenAI family: the judge of Anthropic records and the cross-check of OpenAI ones. |
| `--openai-key-file` | | File with the intake's own OpenAI API key, read at each call. |
| `--openai-endpoint` | `https://api.openai.com/v1` | HTTPS API base of the OpenAI family. |
| `--max-call-tokens` | 40000 | Cap of one call. |
| `--max-run-tokens` | 100000 | Cap of one run. |
| `--max-month-tokens` | 4000000 | Cap of a month, for each family. |
| `--dry-run` | false | A built-in fake model of each family answers a fixed score: no key, no network. Reservations are still counted. |

Both families need a model and a key file unless `--dry-run` is set. The two key files must differ; each adapter refuses the other's file. The keys are the intake's own (ADR-0047 section 6), never the analyzer's or the lab's.

## Output and exit codes

The score JSON has `status` (`scored`, `unjudged`, `invalid`, `quarantined` or `budget`), the `check` and the fixed `reason` of a refusal, the redactor's rule `kinds`, the record's ID and SHA-256, the month, the record's and the judge's family, the judge's model, `crossCheck` (`answered`, `notSent` or `budget`; absent when not asked), the prompt version, the `answer`, the tokens `reserved` and the `usage` the providers reported. It holds no record text: every string in it is fixed by code, a checked shape, or the judge's note after the redactor found nothing in it. In the job, `quarantined` and `budget` go to the quarantine container and the `budget/` prefix (ADR-0047 section 5); a file run writes them as a score with that status.

| Exit | Meaning |
| --- | --- |
| 0 | An outcome was written, or the score file existed already. |
| 1 | A step failed (a read, a reservation, a model call, the run's deadline of 8 minutes): try again. Nothing was written. |
| 2 | A flag or the configuration is wrong. |

Example, on a downloaded record:

```bash
go run ./cmd/intake --record ./records/<record ID>.json --spend-dir ./intake-spend --dry-run
```
