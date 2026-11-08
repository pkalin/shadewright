# ADR-0027: Default models for triage and escalation

- Status: Proposed
- Date: 2026-11-07

## Context

Design 11.2 leaves open which concrete models do the triage and the escalation jobs ([#80](https://github.com/pkalin/shadewright/issues/80), open question 3 in [design 15](../design/15-decisions-made-and-open-questions.md)). The evaluation harness ([#78](https://github.com/pkalin/shadewright/issues/78)) now gives real-model numbers, but they are thin: three repeats, one day, and a build with a known regression. This ADR records what runs today and says plainly what the numbers do not support.

Decided on 2026-11-05 ([#80](https://github.com/pkalin/shadewright/issues/80)), restated here:

1. Microsoft Foundry is the only provider in v0.3.0. Other providers come later as adapters behind `ModelProvider`, each through the evaluation gate.
2. Data Zone EU deployments are the documented default. Global Standard is an operator's documented choice.
3. Ranking: zero false fixes is a hard gate; then correct diagnoses; then cost per resolved incident; latency only breaks ties.
4. No runtime fallback. One deployment per role; throttling or an outage means bounded retries, then a hand-off to a person.
5. No model is named in code. An operator sets the deployment (endpoint and deployment name) in `RemediationSettings` (`spec.analysis.models`) and the model ID and version that deployment serves in the chart values `analyzer.analysis.triageModel` and `analyzer.analysis.escalationModel` (the flags `--triage-model` and `--escalation-model`). A reply that names another model is refused ([ADR-0023](0023-how-the-analyzer-calls-the-model.md)), so the value must be the exact ID and version.

## Decision

1. **False fix, defined.** A false fix is a proposal that reaches a pull request and would not resolve the incident, or would cause harm. A proposal that policy denies or that falls outside the action catalog is not a false fix, because policy stopped it, but the evaluation counts and reports it per model. No run so far had a false fix.
2. **Triage default: `gpt-5.4-mini-2026-03-17`, provisional.** It is what the lab runs today and it was safe on every refusal scenario, but on the comparable set it scored below `gpt-5.4` alone (evidence below). Epic [#503](https://github.com/pkalin/shadewright/issues/503) settles it before v0.4.0.
3. **Escalation: no default.** `escalationModel` stays empty, so an escalation signal hands the incident to a person. `gpt-5.4-2026-03-05` is the leading candidate, not a default: escalation with tools has not been measured for any model. Epic #503 decides it.
4. The chart keeps `analyzer.analysis.triageModel` empty, so analysis stays off until an operator opts in. The evaluation gate of [ADR-0025](0025-analysis-claims-units-and-evaluation-manifest.md) is planned, not built: until it exists, analysis is for evaluation runs in the lab only, as the chart values say.

### Evidence

All runs on 2026-11-07, three repeats each. The 27-run set has 9 scenarios, the same prompt and rubric for every row.

| Run | Passed | Estimated cost | Proposals denied by policy |
|---|---|---|---|
| `gpt-5.4-mini` triage with `gpt-5.4` escalation | 10 of 27 | USD 0.167 | 1 (keda-at-ceiling) |
| `gpt-5.4` alone | 14 of 27 | USD 0.188 | 1 (keda-at-ceiling) |
| `gpt-6.1-terra` alone | 13 of 27 | not reported | 0 |
| `gpt-6.1-sol` alone | 12 of 27 | USD 0.149 | 0 |
| Forced escalation, `gpt-5.4` | 14 of 27, zero tool calls | | |
| Forced escalation, both `gpt-6.1` models | 0 of 27 | | |

- Safety: on an earlier run `gpt-5.4-mini` triage was correct and safe on all 15 refusal scenario runs (5 scenarios, 3 repeats). The proposals policy denied were `AdjustKedaMaxReplicas` to 4 where policy allows 2 to 3, made before [#493](https://github.com/pkalin/shadewright/issues/493) showed the model the policy bounds. None reached Git.
- Accuracy: on the comparable set the proposed pair is the lowest row, and `gpt-5.4` alone costs only about 11% more. The pair is the configuration whose score dropped with regression [#499](https://github.com/pkalin/shadewright/issues/499) (14 of 24 to 10 of 24 on the older 8-scenario set). The effect of #499 on the single-model runs is not measured.
- An older run on the 8-scenario set with a different prompt (`78b6640`) passed 17 of 24 at about USD 0.14. It is not comparable with the table.
- The `gpt-6.1` models scored 0 in forced escalation because chat completions refuse function tools for them ([#501](https://github.com/pkalin/shadewright/issues/501)): an adapter limit, not a verdict on the models. `gpt-5.4` made no tool calls when forced, and escalation never starts in normal runs ([#473](https://github.com/pkalin/shadewright/issues/473)).

### What these numbers are not

- **Deployment type.** The benchmarked `gpt-5.4-mini`, `gpt-5.4` and `gpt-6.1-terra` deployments were Global Standard, because Data Zone quota was pending ([#411](https://github.com/pkalin/shadewright/issues/411)). Only `gpt-6.1-sol` ran as Data Zone Standard. Numbers on Data Zone are expected to match but are not measured.
- **Injection corpus.** The false-fix gate on the prompt injection and outage corpus ([#79](https://github.com/pkalin/shadewright/issues/79)) is not yet measured for these models.
- **Cost.** Per-run costs are estimates from the evaluation report, not a cost per resolved incident.

## Consequences

An operator who turns analysis on for a lab evaluation has a documented triage model that the lab runs today, marked provisional, and no escalation. Escalation signals hand off to a person, which is the safe outcome.

Both defaults may change. Because the model ID is a chart value and the deployment a setting, changing either is configuration plus a new evaluation record. A follow-up ADR from #503 supersedes this one.

Before v0.4.0 this needs: a run with #499 fixed, a run on Data Zone deployments, the injection corpus against the chosen models, escalation measured with tools (#473, and #501 or a documented reason to exclude the `gpt-6.1` family), and policy denials reported per model by the evaluation.

The lab Terraform (`shadewright-azure`) defaults a model deployment to Global Standard when no SKU is given; only the example variables use Data Zone Standard. A deployment added for #503 without an explicit SKU would route outside the EU, so the Terraform default should change to Data Zone Standard.

## Alternatives considered

- Wait and decide nothing until #503: operators and the lab would have no documented triage model, although the lab already runs one.
- Triage on `gpt-5.4` alone: best on the comparable set, but the margin is from one regressed build with three repeats; kept as a candidate for #503.
- A provisional escalation default (`gpt-5.4`): rejected, a placeholder never measured with tools gets copied into installs and outlives the follow-up.
- A runtime fallback between models: rejected on 2026-11-05, it sends the same data to an unevaluated model and possibly another region.
- Name a model in the chart defaults or in code: rejected, the operator chooses the model and the chart stays empty.

## Amendments

None.
