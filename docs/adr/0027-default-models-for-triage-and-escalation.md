# ADR-0027: Default models for triage and escalation

- Status: Proposed
- Date: 2026-11-07

## Context

Design 11.2 leaves open which concrete models do the triage and the escalation jobs ([#80](https://github.com/pkalin/shadewright/issues/80), open question 3 in [design 15](../design/15-decisions-made-and-open-questions.md)). The evaluation harness ([#78](https://github.com/pkalin/shadewright/issues/78)) now gives real-model numbers, but they are uneven: strong for triage, weak for escalation. This ADR decides what the evidence supports today and says plainly what it does not.

Decided on 2026-11-05 ([#80](https://github.com/pkalin/shadewright/issues/80)), restated here:

1. Microsoft Foundry is the only provider in v0.3.0. Other providers come later as adapters behind `ModelProvider`, each through the evaluation gate.
2. Data Zone EU deployments are the default in the chart, the docs and the lab. Global Standard is an operator's documented choice.
3. Ranking: zero false fixes is a hard gate; then correct diagnoses; then cost per resolved incident; latency only breaks ties.
4. No runtime fallback. One deployment per role; throttling or an outage means bounded retries, then a hand-off to a person.
5. Models are set only in `RemediationSettings` (`spec.analysis.models`), never in code.

## Decision

1. **Triage default: `gpt-5.4-mini`.** Decided on the evidence below.
2. **Escalation default: `gpt-5.4`, provisional.** It is one of a near tie and the only candidate that ran at all in forced escalation. A follow-up ADR from epic [#503](https://github.com/pkalin/shadewright/issues/503) settles it before v0.4.0. Until then this is not a measured ranking.
3. The restated decisions above stand. Nothing about the safety gates or the settings-only rule changes.
4. These are documented defaults, not code: the chart keeps `analyzer.analysis.triageModel` empty (analysis stays off until an operator opts in), and the model is named in `RemediationSettings`.

### Evidence for triage

- First run: `gpt-5.4-mini-2026-03-17` was correct and safe on all 15 of 15 refusal scenario runs. Nothing outside the catalog was ever proposed.
- Third run at `78b6640`: 17 of 24 passed, about USD 0.14 estimated, no `NoEvidence`.
- The failures are in pull-request scenarios and trace to context and analyzer problems ([#472](https://github.com/pkalin/shadewright/issues/472), [#481](https://github.com/pkalin/shadewright/issues/481), [#487](https://github.com/pkalin/shadewright/issues/487), [#493](https://github.com/pkalin/shadewright/issues/493), [#494](https://github.com/pkalin/shadewright/issues/494)), not to the model.

### Evidence for escalation

| Run | Result (27 scenario runs) |
|---|---|
| Model alone, `gpt-5.4` | 14 of 27 |
| Model alone, `gpt-6.1-terra` | 13 of 27 |
| Model alone, `gpt-6.1-sol` | 12 of 27 |
| Forced escalation, `gpt-5.4` | 14 of 27, with zero tool calls |
| Forced escalation, both `gpt-6.1` models | 0 of 27 |

- The three models are a near tie, so the numbers do not separate them.
- The `gpt-6.1` models scored 0 in forced escalation because chat completions refuse function tools for them ([#501](https://github.com/pkalin/shadewright/issues/501)). That is an adapter limit, not a verdict on the models.
- `gpt-5.4` made no tool calls when forced to escalate, so escalation with tools is not yet measured for any model.
- Escalation never starts in normal runs ([#473](https://github.com/pkalin/shadewright/issues/473)).
- These runs were made on the same day as regression [#499](https://github.com/pkalin/shadewright/issues/499) (oom facts lost after [#498](https://github.com/pkalin/shadewright/issues/498)), which affected every model, so the ranking may be skewed.

### What these numbers are not

- **Deployment type.** The benchmarked `gpt-5.4-mini`, `gpt-5.4` and `gpt-6.1-terra` deployments were Global Standard, because Data Zone quota was pending ([#411](https://github.com/pkalin/shadewright/issues/411)). Only `gpt-6.1-sol` ran as Data Zone Standard. The documented default stays Data Zone EU. Numbers on Data Zone are expected to match but are not yet measured.
- **Injection corpus.** The false-fix gate on the prompt injection and outage corpus ([#79](https://github.com/pkalin/shadewright/issues/79)) is not yet measured for these models in this ADR. The refusal scenarios show no unsafe proposal, which is not the same gate.
- **Cost.** Per-run costs are estimates from the evaluation report, not a cost per resolved incident.

## Consequences

Triage can be configured and documented now. A user who turns analysis on gets a default with evidence behind it, and the evaluation gate ([ADR-0025](0025-analysis-claims-units-and-evaluation-manifest.md)) still decides whether a model and prompt pair may run automatically.

The escalation default may change. Because models live only in `RemediationSettings`, changing it is a settings change plus a new evaluation record. The cost is that for a short time the documented escalation model is a placeholder, and the follow-up ADR must supersede or amend this one when #503 reports.

Before v0.4.0 this needs: a run with the regression fixed, a run on Data Zone deployments, the injection corpus run against the chosen models, and function tool support for the `gpt-6.1` family (or a documented reason to exclude it).

## Alternatives considered

- Wait and decide both roles after #503: nothing is documented for triage although the evidence is good, and operators get no default.
- Pick the escalation model from the near tie (for example `gpt-6.1-terra` or `gpt-6.1-sol`): the ranking may be skewed by #499, and the `gpt-6.1` models cannot use tools yet ([#501](https://github.com/pkalin/shadewright/issues/501)).
- A runtime fallback between models: rejected on 2026-11-05, it sends the same data to an unevaluated model and possibly another region.
- Name the models in the chart or in code: rejected, models are settings.

## Amendments

None.
