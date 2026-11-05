# 12. Cost

This section gives the formula for what one analysis costs (12.1), a worked example with made-up prices (12.2) and the controls that keep spend bounded (12.3). Exact limits and edge cases are in the table "Details" at the end (12.4).

## 12.1. Formula

```text
inference cost =
    uncached input tokens / 1,000,000 * input price
  + cached input tokens   / 1,000,000 * cached input price
  + output tokens         / 1,000,000 * output price
```

Count every call of an analysis: tool steps, retries and escalations. Check how reasoning tokens and long context are billed for the chosen model.

## 12.2. Illustration (made-up prices, not a quote)

100 analyses a day for 30 days, each 6,000 input and 1,500 output tokens: 18 M input and 4.5 M output tokens a month.

| Variant | Input per 1 M | Output per 1 M | Result |
| --- | ---: | ---: | ---: |
| A, cheaper | 0.20 USD | 0.80 USD | 18 × 0.20 + 4.5 × 0.80 = 7.20 USD/month |
| B, stronger | 2.00 USD | 8.00 USD | 18 × 2.00 + 4.5 × 8.00 = 72.00 USD/month |

**What the figures are.** A single-call baseline for the model path, not an upper bound. Retries, tool steps and escalations add calls (12.1). Runbook matches make no model call and cost nothing.

**With escalation.** The common case is one call to A. Say a made-up 5% of analyses also escalate to B with the same context size: 7.20 + 0.05 × 72.00 = 10.80 USD/month. Real escalations cost more, because they also run tools.

## 12.3. Controls

### Real prices

Real prices come from the model card, the deployment configurator and the [Azure Pricing Calculator](https://azure.microsoft.com/en-us/pricing/calculator/). Record them with the date checked.

### What the app enforces

The app will enforce maximum context, steps, output tokens, concurrency, analyses per day and a daily spend limit. All are set in `RemediationSettings` (5.4): the count of analyses in `spec.analysis.maxAnalysesPerDay`, the others beside it in `spec.analysis`.

**Planned.** Only the count of analyses is used today (3.2). The other limits are enforced as the analysis loop ([#76](https://github.com/pkalin/shadewright/issues/76)) lands, and the spend limit once its counter exists ([#208](https://github.com/pkalin/shadewright/issues/208)).

### Why an Azure budget is not enough

An Azure budget only alerts. It does not stop usage ([budgets](https://learn.microsoft.com/en-us/azure/cost-management-billing/costs/tutorial-acm-create-budgets)).

### Keeping calls down

- Deduplication and cached diagnoses keep repeated calls down.
- A failed or timed-out call is charged at its upper bound: the input estimate plus `MaxOutputTokens`.

### The daily count of analyses

The count lives in the ConfigMap `shadewright-analysis-budget` in the operator namespace. Only the analyzer and admins may write it.

- A count that cannot be read or written, or that is missing, means refuse, not allow (section 3.2, [#43](https://github.com/pkalin/shadewright/issues/43)).
- <span class="sw-status sw-planned">planned</span> One unit is one claimed attempt, not a call and not money; see [ADR-0025](../adr/0025-analysis-claims-units-and-evaluation-manifest.md).
- It is enforced once the analysis loop ([#76](https://github.com/pkalin/shadewright/issues/76)) reserves a unit before each model call.

### The spend limit

The spend limit is a count of tokens per model deployment and UTC day (`maxTokensPerDay`), not an amount of money.

- **Why tokens.** Tokens are what the provider reports and bills, and they are whole numbers with no rounding. A money limit would need a price list in the settings that goes stale.
- **Why per deployment.** The triage and escalation models differ in price.
- **No default.** The admin sets it for each deployment.

The money it allows is at most that count times the deployment's highest price per token (12.1). That holds only if the counter reserves a call's upper bound (the input bound plus `maxOutputTokens`) before calling, under one lock shared by concurrent analyses, and settles to the real usage afterwards.

**Planned.** That is a requirement on the counter ([#208](https://github.com/pkalin/shadewright/issues/208)). Where it is counted is open (#208).

### The lab

The lab itself (AKS node, Key Vault, storage, Foundry) is priced in `shadewright-azure` and runs only when needed ([ADR-0008](../adr/0008-disposable-azure-lab.md)).

## 12.4. Details

| Topic | Detail |
| --- | --- |
| Illustration volume | 100 analyses a day, 30 days, 6,000 input and 1,500 output tokens each |
| Illustration escalation rate | 5% to variant B |
| Failed or timed-out call | Charged at input estimate plus `MaxOutputTokens` |
| Spend limit unit | Tokens per model deployment and UTC day (`maxTokensPerDay`), no default |
| Budget ConfigMap | `shadewright-analysis-budget`, operator namespace, writable by the analyzer and admins only |
| Count unreadable, unwritable or missing | Refuse |
