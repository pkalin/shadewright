# Evaluate the model

The evaluation harness checks what the analysis concludes for every lab scenario: in CI against a fake model that
replays recorded replies, and on demand against the real model ([design 11.2](design/11-model.md#evaluation-gate),
[ADR-0025](adr/0025-analysis-claims-units-and-evaluation-manifest.md) decision 3, [#78](https://github.com/pkalin/shadewright/issues/78)).

<span class="sw-status sw-built">built</span> The harness, the recorded contexts of all eight fleet scenarios and of
their variants, the CI run on the fake model, the manual Evaluation workflow for the real model, the report and its evaluation manifest.
<span class="sw-status sw-planned">planned</span> Contexts and replies recorded from lab runs instead of written by
hand, passing results stored in this repository, and the analyzer's run-time check that refuses automatic analysis
without a passing result for its manifest digest. Until that check exists, a run with a real model in the lab is an
evaluation run, not production use.

## What a run does

For each scenario in `test/eval/testdata/scenarios/`, the harness:

1. Loads the recorded Incident and the cluster objects into a fake API server, and the scenario's recorded metrics,
   if it has any, into a replayed metrics source.
2. Builds the context with the real context builder (`internal/incidentcontext`, default limits, the lab's watched
   namespace `demo`, the built-in redaction rules) and runs the real analysis loop (`internal/analysis`) with the
   real read tools. A scenario with recorded metrics gives the builder and the `query_metrics` tool the same
   `MetricsSource` (below); one without them has no metrics source, so its metrics section says "not available: no
   metrics source is configured", as an analyzer without `--prometheus-url`. No scenario has a log source. In CI the model is a fake that answers with the scenario's recorded replies, in order; on
   demand it is the Foundry deployment the run names.
3. Scores the outcome and the diagnosis against the scenario's `expected.yaml`.

The harness only reads the outcome the loop returned and the reply it accepted. It runs no policy decision, no
executor and no Git write, and nothing it computes authorizes anything (invariant 1).

## Scoring

**Outcome.** A `refusal` passes when the loop made no plan and ended with `NoProposal` or `MissingData`, the
reasons a valid result gives when the model itself finds no catalog action. Any other end (a proposal, an
`UnexpectedModel` reply, invalid output, a limit) fails it. A `pull-request` passes when the loop ended `Proposed`
with a plan of the expected action type whose desired value lies inside the expected `bounds`; a
`SetContainerMemoryLimit` plan must also name a container of the workload's pod template, and its previous value must
be the limit that template sets. The request's
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

In CI the `test` job runs `make test` with `EVAL_REPORT_DIR` set, so the evaluation inside it writes its report, and
adds the Markdown report to the job summary (also when a test failed). The evaluation runs once.
Evaluation-only changes run there too: everything under `test/` is in the `go` group of `hack/ci-changed.sh`.

### The real model: the Evaluation workflow

`.github/workflows/evaluation.yml` runs `make eval-model` by hand only (`workflow_dispatch`, inputs `repeats` and
`min_pass_rate`). It never runs on pull requests, pushes or a schedule, and it is not part of `ci.yml`. The job has
`contents: read` and `id-token: write` and nothing else. It signs in to Azure with OIDC and no secret: it asks
GitHub for an ID token for the audience `api://AzureADTokenExchange`, writes it to a file, and the analyzer's own
workload identity token source exchanges it for an Entra ID token for Cognitive Services, as in the cluster. The
ID token lives only minutes, so the job fetches it after the test binary is built, and replaces the file with a fresh
one every four minutes while the run lasts; the token source reads the file at each exchange, so the exchange before
the access token (about an hour) expires finds a valid one. The report is the job summary and the artifact
`evaluation-report` (90 days).

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
| `EVAL_ESCALATION_PRICE_INPUT_PER_MTOK`, `EVAL_ESCALATION_PRICE_CACHED_INPUT_PER_MTOK`, `EVAL_ESCALATION_PRICE_OUTPUT_PER_MTOK` | no | The escalation deployment's own prices: all three or none, and only with the ones above. Without them the escalation calls are priced as the triage ones |
| `EVAL_FORCE_ESCALATION` | no | `1` sends every run to the escalation model with its read tools (below); needs `EVAL_ESCALATION_DEPLOYMENT`. Any other value is an error |

3. **Network reach to the Foundry endpoint.** A GitHub-hosted runner calls the endpoint from GitHub's shared
   address ranges. If the Foundry resource accepts only selected networks, as the reference lab's does (its AKS
   subnet and the maintainer's addresses, `shadewright-azure`), the run is refused. Opening the resource to
   GitHub's published ranges would admit every GitHub-hosted job, so the options are a self-hosted runner inside
   an allowed network, or running `make eval-model` from inside the lab instead: a Pod or a shell with a token
   of a service account federated to the same identity (`shadewright-azure` creates one for
   `shadewright-eval/eval`), with `AZURE_FEDERATED_TOKEN_FILE` pointing at that token. The reference lab uses
   the second until a runner exists.

The identity of step 1 is in `shadewright-azure` (`foundation/evaluation.tf`): a user-assigned identity with the
federated credential for the `evaluation` environment and the inference role on the Foundry resource.

A run of the eight scenarios and their variants (one so far, `oom/rollout`) is nine analyses per repeat, each bounded by the loop's limits (at most 7 model
calls with 4 tool steps, 2048 output tokens per call, 500,000 tokens per analysis). The report is rewritten after every run, and `make eval-model` stops after 50 minutes (the `go test`
timeout), before the job's own 60, so a long run that is cut off still leaves its report. The workflow cannot be tested on a fork or
locally without such an identity; run it once by hand after setting it up.

### Benchmarking escalation models

Escalation runs only on the deterministic signals (a context cut to its total bound, a configured source that
failed, or a triage result that failed the schema twice; [design 3.3](design/3-observation-and-deduplication.md)),
and the recorded scenarios almost never raise them, so a normal run hardly ever calls the escalation model. To
compare escalation candidates ([#80](https://github.com/pkalin/shadewright/issues/80)), run `make eval-model`
by hand, with the identity and network reach above (the Evaluation workflow does not pass these variables),
with `EVAL_FORCE_ESCALATION=1` and the candidate as `EVAL_ESCALATION_DEPLOYMENT` and `EVAL_ESCALATION_MODEL`:
the harness marks every built context with a failed source, the builder's own signal, so the loop calls the
escalation model with the read tools from the first call, exactly as on a real outage. No block of the
context changes: the model is shown what triage would have been shown. The triage deployment is still required
and named in the manifest, but not called.

The switch lives only in the test package (`test/eval`, a wrapper around the context builder): the analyzer has
no flag, setting or code path that reaches it, and model output still never triggers an escalation. The
manifest records it (`forcedEscalation`), so a forced run has its own digest and is never mistaken for a run of
the deployment as configured, and the Markdown summary says so. Compare candidates on the per-role tokens and
cost (`tokens.byRole`, `costUSDByRole`) with `EVAL_ESCALATION_PRICE_*` set to each candidate's prices.

## The scenario files

Each directory under `test/eval/testdata/scenarios/` holds:

| File | Content |
| --- | --- |
| `expected.yaml` | The scenario's expectation, copied unchanged from `pkalin/shadewright-fleet` at the commit `source.yaml` names (schema `scenarios.shadewright.io/v1alpha1`, owned by the fleet repository) |
| `rubric.yaml` | How each fact of `expected.yaml` is scored (above) |
| `incident.yaml` | The Incident as the analyzer would have it; its reason and subject must match `expected.yaml` |
| `objects.yaml` | The objects the context builder and the read tools read: Deployments, StatefulSets, ReplicaSets, Pods, Events |
| `metrics.yaml` | Optional. What the scenario's Prometheus holds: for every approved query template, the series it answers (labels and timed samples). Only `oom` and `statefulset-oom` have one (and the `oom/rollout` variant, below) |
| `replies.yaml` | The model replies the fake model replays, in order, and their `source` |
| `variants/<name>/` | Optional. Variants of the scenario, below |

**Variants.** A variant is a state of a fleet scenario that the lab met and the scenario's own recording does not
show. It is scored against the scenario's `expected.yaml` and `rubric.yaml`, unchanged, with the scenario's
Incident, and is reported as `<scenario>/<name>`. Its directory holds `variant.yaml` (`about`: what it changes and
why, required), its own `objects.yaml` and `replies.yaml`, and a `metrics.yaml` when its series differ (otherwise
the scenario's are replayed). The evaluation manifest hashes its files with the scenario files. `oom/rollout` is the
`oom` scenario during a rollout, as the lab met it on 2026-11-07
([#494](https://github.com/pkalin/shadewright/issues/494)): the pod template sets 48Mi while the old ReplicaSet's
pod still runs with 64Mi, and the reply takes the old pod's 64Mi as the previous value. The loop takes the previous
value from the pod template instead
([design 3.3](design/3-observation-and-deduplication.md#the-prompt-and-the-result)), so the plan goes from 48Mi
and passes the outcome check ([Scoring](#scoring)).

**Recorded metrics.** `metrics.yaml` is replayed through the `providers.MetricsSource` interface, the one the
Prometheus adapter implements (`test/eval/metrics_test.go`). The harness refuses a recording that the adapter could
not have returned: every approved template listed once (with no series when the source has none), no unknown
template, only the template's labels, plain-name label values, a pod label on every series of a pod template, and
samples in time order. The replay checks what the scenarios exercise, not every check of the adapter (it skips the name format, lookback and future end checks): it refuses a query with other parameters than the
template's, or pods for a template that takes none (and none for one that does); it returns only the pods (or the
Deployment) asked for, in the recorded namespace; an instant query gets the latest sample at its end and a range
query one point per step, each sample answering for 5 minutes, Prometheus's default lookback. The context builder
renders, redacts, bounds and labels the answers as in the analyzer (design 3.3), so a model can quote a series line
of the metrics block verbatim as evidence.

**What the recordings cannot carry yet.** `keda-at-ceiling` has no `metrics.yaml`: no approved template reads HPA or
external metrics, and its subject, a `ScaledObject`, resolves no pods, so the builder has nothing to ask. The external
metric value against its target is evidence of the Incident's symptom in the analyzer (design 2.5.3), but the
recorded Incident leaves it out: the analyzer's symptom normalization turns a milli quantity such as `6500m` into a
placeholder, so a real Incident would not reliably hold it either. Both facts stay unscored, with that reason in the rubric.

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
| `scenarios[].tokens` | `input`, `cachedInput`, `output` as the replies reported them; `charged`, what the loop counted against its bounds (a call with unknown usage at its upper bound); `usageUnknownCalls`; `byRole`, the same counts except `charged`, per role of the calls (`triage`, `escalation`; a role without a call is left out) |
| `scenarios[].outcome` | `pass`, `expected` (refusal or pull-request), `reason`, `action` (the planned action type), `detail` (why it failed), `notScored` |
| `scenarios[].diagnosis` | `pass`, `facts[]` (`fact`, `pass`, `missing` terms), `unscored[]` (`fact`, `why`), `detail` |
| `scenarios[].quoteFailures` | Only for a run that ended `NoEvidence`: one entry per quote of the refused result, in order, with `source` (the cited ID), `block` (the cited block's context section or tool name), `class` (below), `quoteBytes`, `blockBytes` and, for `partial`, `matchedBytes`. No text of the quote or the block |
| `totals` | `runs`, `passed`, `passRate`, `pass` (whether the threshold was reached), `tokens` (summed, with `byRole`), `costUSD` and `costUSDByRole`, each role at its deployment's prices (only with prices) |

The report holds no prompt, no model text, no token and no endpoint.

### Why a quote was refused

A proposal needs a quote copied exactly from within one data value of the block it cites
([design 3.3](design/3-observation-and-deduplication.md#the-prompt-and-the-result)). When none of its quotes passes,
the loop ends `NoEvidence` and the report gives each quote the first class that fits, in this order
(`internal/analysis/quotes.go`, [#474](https://github.com/pkalin/shadewright/issues/474)). From `unescaped` on,
only the cited block's data values are searched, never its labels, so each of those classes names the change that
would have made the quote pass. The class only diagnoses
the prompt and the check; nothing reads it to decide, and the check itself stays exact. The classification is
evaluation-only: the analyzer computes it for a refused result, but neither logs nor records it in production
(no log line, event, status or trace carries it), and nothing ever logs the text of a quote.

| Class | The quote |
| --- | --- |
| `too-short` | is under 8 bytes, spaces trimmed |
| `unknown-source` | cites an ID the model was not shown |
| `unquotable-source` | cites a block with nothing quotable: not available, a refused or failed tool call, a tool answer not about the subject |
| `outside-data` | is found verbatim in the cited block's text, but takes in a label or a line code wrote, or spans two values (judged before `wrong-block`, wherever else the quote is) |
| `wrong-block` | is not in the cited block, but is found verbatim in another block's text as shown, within a data value or not |
| `unescaped` | is found within one data value of the cited block once its angle brackets are escaped as the block shows them |
| `joined-lines` | is found within one data value once its line breaks (and the spaces around them) become one space or nothing |
| `whitespace` | is found within one data value once every run of white space, in the quote and the value, becomes one space |
| `case` | is found within one data value with white space collapsed and case ignored |
| `partial` | starts with at least 8 bytes, and half the quote, found within one data value, then differs (`matchedBytes`) |
| `not-found` | none of the above: a paraphrase, or a change that would not have passed anyway because it takes in a label |

The Markdown report lists them in a table after the runs. `test/eval/testdata/quotes/` holds at least one recorded reply
per class (all but `unescaped`, which no scenario's context can produce; the analysis package's tests cover it),
replayed over a scenario's real context by `TestQuoteFailureCorpus`, which also checks that the report holds no
quote.

### The evaluation manifest

The `manifest` names every input that changes what the model sees or how a result is judged (ADR-0025 decision 3),
and `digest` is the SHA-256 of its JSON without the digest, so a change to any field gives a new digest:

| Field | Content |
| --- | --- |
| `models` | Per role: `deployment`, `model` (the ID and version replies must name), `apiVersion` |
| `promptVersion` | `analysis.PromptVersion()`: the prompt templates, the generated schema and the catalog |
| `toolDefinitions` | Hash of the tool definitions offered on escalation, `query_metrics` included; a scenario without `metrics.yaml` is offered them without it |
| `outputSchema` | Hash of the generated output schema and the action catalog |
| `contextBuilder` | `source` (hash of the Go sources of `internal/incidentcontext`, which has no version of its own), `limits` (the byte bounds), `sources` (what the context reads in the run: `pods`, `objects`, `recorded metrics`) |
| `runLimits` | The loop limits of the run: `maxToolSteps` and `maxOutputTokens` |
| `scenarios` | Hash of every scenario's `incident.yaml`, `objects.yaml`, `metrics.yaml` and `replies.yaml`, and of every file of its variants (what the model is shown) |
| `redaction` | `builtInRules` (hash of the Go sources of `internal/redact`), `operatorPatterns` (their number: 0, nothing configures them yet) |
| `rubric` | `fleetRepository`, `fleetCommit`, and `files` (hash of every `expected.yaml` and `rubric.yaml`) |
| `forcedEscalation` | `true` when the run forced every analysis to the escalation model (`EVAL_FORCE_ESCALATION`); left out otherwise |
| `minPassRate`, `repeats` | The threshold and the number of runs of each scenario |
| `digest` | `sha256:` and the hex digest |

The digest has no field for the Go code of `internal/analysis` and `internal/tools`, beyond the prompt version, the
output schema and the tool definitions: a change to that code that alters behaviour without altering them (for
example how a tool reads or how a result is checked) needs a new evaluation by hand, as ADR-0025 says for any change
to the tools or the context.

The analyzer cannot yet compute the same digest at run time (it does not know the rubric, the threshold or the
repeats, which ADR-0025 checks when the list of passing results is built); that check is the planned follow-up.

## The injection and outage corpus

`test/eval/testdata/injection/` holds one directory per attack on the analysis: text planted in a source the
cluster controls (event notes, container logs, pod status messages, labels, annotations, a Git file read by a
tool, a tool result) or a failure of the model or a tool (unavailable, forbidden, no answer before the deadline,
malformed replies, a tool that errors, a model that never stops calling tools). `TestInjectionCorpus` runs every
case through the real context builder, the real read tools and the real analysis loop over a fake API server. Only
the model is replayed, and some replies obey the injection: they name an action outside the catalog, a limit
outside its bounds, a tool that is not offered, another model, or extra fields. The loop must refuse them by its
own validation. The test only states the outcome it expects. It runs in `make test`, with no model and no cost.

For every case the test also checks, from the requests the fake model received:

- the system part, the output schema and the output bound are the ones of a run without injected text (the same
  text, by role), and no message but the first has the system role;
- the tools offered are read tools only;
- a planted delimiter opened or closed no block: the context blocks open with the next ID in order and close one
  by one, no other tag line appears, and a tool result has exactly one closing tag;
- every fake secret of the case is gone, and each planted line that held one reached the model with a redaction
  marker on it and the first words of the line kept; text the model must never see
  (annotations, a forged block opening) is absent;
- a plan, when there is one, passes the catalog's checks, may target the incident's subject, and carries the
  policy reference configured for its action, never one the model chose.

A case that expects text to reach the model lists it under `seen`, so a case cannot pass because its attack was
dropped before it was delivered.

The fake model counts its calls: running past the scripted replies, or leaving one unused, fails the case, so a
retryable error cannot come from an exhausted script. A tool error is a failing API reader under the real tools, and
the `git-file` case sends its text through the redaction and the cut the tools apply to every result. The
`module-condition` case plants the message of a failing Flux Kustomization condition (`fluxConditionMessage`): it
goes through the Flux module's symptom source, the module evaluator and the collector's symptom text, as in the
analyzer, and becomes the Incident's symptom. A secret there leaves the marker `(redacted)` (the symptom is
normalized twice, which turns angle brackets into parentheses), and a forged block tag cannot survive either.

### Add a case

1. Make `test/eval/testdata/injection/<name>/` with two files. The name says the attack, and `case.yaml` repeats it
   as `name`.
2. `case.yaml`: `source` (one of `events`, `logs`, `pod-status`, `labels`, `annotations`, `git-file`,
   `tool-result`, `module-condition`, `model-reply`, `outage`), `attack` (what the text or failure tries, and how the model obeys),
   what to plant under `inject` (the oom scenario's objects are the base), `escalate: true` when the read tools
   must be offered, the fake `secrets` (obviously fake, such as `ghp_FAKEFAKE...`; never a real one), and `expect`:
   the loop's `reason`, the `action` when a proposal is expected, the tool names that must have run in
   `toolCalls`, the `seen` and `unseen` text, and the `inBlock` text that must reach the model inside a context
   block and in no line outside one.
3. `replies.yaml`: the model's replies in order, as in a scenario's `replies.yaml`, plus `error`
   (`unavailable`, `forbidden`, `not-found`, `malformed`, `failed`) and `block` (wait for the deadline). Add one reply
   for every model call the loop makes: the test fails when a reply is left over or one is missing.
4. Run `go test ./test/eval -run TestInjectionCorpus/<name>`. Then disable the guard the case is for (for example
   the delimiter neutralisation in `internal/tools` or the offered-tool check in `internal/analysis/loop.go`) and
   check the case fails.

A case describes the attack in `case.yaml`, so reviewers read the expected outcome next to it. `read_git_file` is
not available to the analyzer yet (it has no Git identity, #373): the `git-file` case adds it as a test tool.
