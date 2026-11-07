# Evaluate the model

The evaluation harness checks what the analysis concludes for every lab scenario: in CI against a fake model that
replays recorded replies, and on demand against the real model ([design 11.2](design/11-model.md#evaluation-gate),
[ADR-0025](adr/0025-analysis-claims-units-and-evaluation-manifest.md) decision 3, [#78](https://github.com/pkalin/shadewright/issues/78)).

<span class="sw-status sw-built">built</span> The harness, the recorded contexts of all eight fleet scenarios, the
CI run on the fake model, the manual Evaluation workflow for the real model, the report and its evaluation manifest.
<span class="sw-status sw-planned">planned</span> Contexts and replies recorded from lab runs instead of written by
hand, passing results stored in this repository, and the analyzer's run-time check that refuses automatic analysis
without a passing result for its manifest digest. Until that check exists, a run with a real model in the lab is an
evaluation run, not production use.

## What a run does

For each scenario in `test/eval/testdata/scenarios/`, the harness:

1. Loads the recorded Incident and the cluster objects into a fake API server.
2. Builds the context with the real context builder (`internal/incidentcontext`, default limits, the lab's watched
   namespace `demo`, the built-in redaction rules) and runs the real analysis loop (`internal/analysis`) with the
   real read tools. In CI the model is a fake that answers with the scenario's recorded replies, in order; on
   demand it is the Foundry deployment the run names.
3. Scores the outcome and the diagnosis against the scenario's `expected.yaml`.

The harness only reads the outcome the loop returned and the reply it accepted. It runs no policy decision, no
executor and no Git write, and nothing it computes authorizes anything (invariant 1).

## Scoring

**Outcome.** A `refusal` passes when the loop made no plan and ended with `NoProposal` or `MissingData`, the
reasons a valid result gives when the model itself finds no catalog action. Any other end (a proposal, an
`UnexpectedModel` reply, invalid output, a limit) fails it. A `pull-request` passes when the loop ended `Proposed`
with a plan of the expected action type whose desired value lies inside the expected `bounds`. The request's
phase (`NeedsIntervention`) and the file `paths` are decided by the executor and its source mapping, so they are
listed in the report as not scored.

**Diagnosis.** Each scenario's `rubric.yaml` names every fact of `expected.yaml` verbatim, and either the terms
the diagnosis must contain (`all`: every term; `any`: at least one; case ignored) or why the fact is not scored
(`unscored`, for example a planned tool or source the context cannot carry yet). The diagnosis text is the
accepted result's summary, hypotheses and evidence quotes; when the loop accepted no result there is nothing to
score and the diagnosis fails. The harness refuses a rubric that leaves out, rewords or repeats a fact, so a
change of the facts in the fleet repository cannot pass unscored.

A scenario run passes when both parts pass. The run passes when the share of passing scenario runs reaches the
threshold (`EVAL_MIN_PASS_RATE`, 1 by default; CI always uses 1).

## Run it

```bash
make test         # runs the evaluation on the fake model with every other test
make eval         # only the evaluation, report in bin/eval/ (EVAL_REPORT_DIR to change it)
make eval-model   # the real model; needs the variables below and an Azure workload identity; costs money
```

In CI the `test` job runs `make test` and then `make eval`, and writes the Markdown report into the job summary.
Evaluation-only changes run there too: everything under `test/` is in the `go` group of `hack/ci-changed.sh`.

### The real model: the Evaluation workflow

`.github/workflows/evaluation.yml` runs `make eval-model` by hand only (`workflow_dispatch`, inputs `repeats` and
`min_pass_rate`). It never runs on pull requests, pushes or a schedule, and it is not part of `ci.yml`. The job has
`contents: read` and `id-token: write` and nothing else. It signs in to Azure with OIDC and no secret: it asks
GitHub for an ID token for the audience `api://AzureADTokenExchange`, writes it to a file, and the analyzer's own
workload identity token source exchanges it for an Entra ID token for Cognitive Services, as in the cluster. The
report is the job summary and the artifact `evaluation-report` (90 days).

Before the first run, the maintainer sets up:

1. **An identity.** An app registration or user-assigned managed identity with a federated credential whose
   subject is `repo:pkalin/shadewright:environment:evaluation` and audience `api://AzureADTokenExchange`, and the
   role **Cognitive Services OpenAI User** on the Foundry resource only. Not the analyzer's lab identity: a
   separate one, so the lab cluster's credential never leaves it.
2. **The environment `evaluation`** in the repository settings, with a required reviewer if a run should wait for
   approval before it spends money, and these variables (not secrets: none of them is a credential):

| Variable | Required | Meaning |
| --- | --- | --- |
| `EVAL_AZURE_CLIENT_ID` | yes | Client ID of the identity above |
| `EVAL_AZURE_TENANT_ID` | yes | Its tenant ID |
| `EVAL_ENDPOINT` | yes | HTTPS base URL of the Foundry endpoint, as in `spec.analysis.models` |
| `EVAL_ENDPOINT_SUFFIXES` | yes | Host suffix allow-list, as the analyzer's `--model-endpoint-suffixes` (for example `.services.ai.azure.com`) |
| `EVAL_TRIAGE_DEPLOYMENT` | yes | Name of the triage deployment |
| `EVAL_TRIAGE_MODEL` | yes | Model ID and version the triage replies must name, as `--triage-model` |
| `EVAL_ESCALATION_DEPLOYMENT` | no | Name of the escalation deployment; without it an escalation is handed off |
| `EVAL_ESCALATION_MODEL` | with the above | Model ID and version of the escalation replies, as `--escalation-model` |
| `EVAL_API_VERSION` | no | Inference API version, `v1` by default |
| `EVAL_PRICE_INPUT_PER_MTOK`, `EVAL_PRICE_CACHED_INPUT_PER_MTOK`, `EVAL_PRICE_OUTPUT_PER_MTOK` | no | USD per million tokens of the deployment, for the cost estimate: all three or none. Nothing is built in; take them from the Azure price list for the region |

A run of the eight scenarios is eight analyses per repeat, each bounded by the loop's limits (at most 7 model
calls with 4 tool steps, 2048 output tokens per call, 500,000 tokens per analysis). The job's timeout is 60 minutes, which also
keeps it inside the lifetime of the access token it exchanged once. The workflow cannot be tested on a fork or
locally without such an identity; run it once by hand after setting it up.

## The scenario files

Each directory under `test/eval/testdata/scenarios/` holds:

| File | Content |
| --- | --- |
| `expected.yaml` | The scenario's expectation, copied unchanged from `pkalin/shadewright-fleet` at the commit `source.yaml` names (schema `scenarios.shadewright.io/v1alpha1`, owned by the fleet repository) |
| `rubric.yaml` | How each fact of `expected.yaml` is scored (above) |
| `incident.yaml` | The Incident as the analyzer would have it; its reason and subject must match `expected.yaml` |
| `objects.yaml` | The objects the context builder and the read tools read: Deployments, StatefulSets, ReplicaSets, Pods, Events |
| `replies.yaml` | The model replies the fake model replays, in order, and their `source` |

Today every context and reply is `source: synthetic`: written by hand from the scenario manifests and the
observations in `expected.yaml`, so CI checks the harness, the loop and the scoring end to end. They hold no
secrets, no real IDs (UIDs and fingerprints are placeholders), no emails and no local paths. To update the
expectations, copy the fleet's `scenarios/*/expected.yaml` from one commit, set that commit in `source.yaml`, and
fix any rubric the harness then refuses.

## The report

`report.json` (schema version 1) and `report.md`, the same content for a person. The JSON fields:

| Field | Meaning |
| --- | --- |
| `schemaVersion` | 1; raised on any change of a field's meaning |
| `mode` | `fake` (recorded replies) or `foundry` (the real model) |
| `manifest` | The evaluation manifest, below |
| `scenarios[]` | One entry per scenario run: `name`, `repeat`, `pass`, `recording` (fake mode), the loop's `reason`, `escalated`, `calls`, `toolCalls`, `models` (as the replies named them), `tokens`, `outcome`, `diagnosis` and `error` (set when the run could not start; it fails) |
| `scenarios[].tokens` | `input`, `cachedInput`, `output` as the replies reported them; `charged`, what the loop counted against its bounds (a call with unknown usage at its upper bound); `usageUnknownCalls` |
| `scenarios[].outcome` | `pass`, `expected` (refusal or pull-request), `reason`, `action` (the planned action type), `detail` (why it failed), `notScored` |
| `scenarios[].diagnosis` | `pass`, `facts[]` (`fact`, `pass`, `missing` terms), `unscored[]` (`fact`, `why`), `detail` |
| `totals` | `runs`, `passed`, `passRate`, `pass` (whether the threshold was reached), `tokens` (summed), `costUSD` (only with prices) |

The report holds no prompt, no model text, no token and no endpoint.

### The evaluation manifest

The `manifest` names every input that changes what the model sees or how a result is judged (ADR-0025 decision 3),
and `digest` is the SHA-256 of its JSON without the digest, so a change to any field gives a new digest:

| Field | Content |
| --- | --- |
| `models` | Per role: `deployment`, `model` (the ID and version replies must name), `apiVersion` |
| `promptVersion` | `analysis.PromptVersion()`: the prompt templates, the generated schema and the catalog |
| `toolDefinitions` | Hash of the tool definitions offered on escalation |
| `outputSchema` | Hash of the generated output schema and the action catalog |
| `contextBuilder` | `source` (hash of the Go sources of `internal/incidentcontext`, which has no version of its own), `limits` (the byte bounds), `sources` (what the context reads in the run) |
| `redaction` | `builtInRules` (hash of the Go sources of `internal/redact`), `operatorPatterns` (their number: 0, nothing configures them yet) |
| `rubric` | `fleetRepository`, `fleetCommit`, and `files` (hash of every `expected.yaml` and `rubric.yaml`) |
| `minPassRate`, `repeats` | The threshold and the number of runs of each scenario |
| `digest` | `sha256:` and the hex digest |

The analyzer cannot yet compute the same digest at run time (it does not know the rubric, the threshold or the
repeats, which ADR-0025 checks when the list of passing results is built); that check is the planned follow-up.
