# 12. Cost

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

Runbook matches make no model call and cost nothing, so the figures above are a single-call baseline for the model path, not an upper bound: retries, tool steps and escalations add calls (12.1). The common case there is one call to A. If a made-up 5% of analyses also escalate to B with the same context size (escalations also run tools, so real ones cost more): 7.20 + 0.05 × 72.00 = 10.80 USD/month.

## 12.3. Controls

Real prices come from the model card, the deployment configurator and the [Azure Pricing Calculator](https://azure.microsoft.com/en-us/pricing/calculator/), recorded with the date checked. The app will enforce maximum context, steps, output tokens, concurrency, analyses per day and a daily spend limit. All of them are set in `RemediationSettings` (5.4): the count of analyses in `spec.analysis.maxAnalysesPerDay`, the others beside it in `spec.analysis`. Planned: only the count of analyses is used today (3.2); the other limits are enforced as the analysis loop ([#76](https://github.com/pkalin/shadewright/issues/76)) lands, and the spend limit once its counter exists ([#208](https://github.com/pkalin/shadewright/issues/208)). An Azure budget only alerts; it does not stop usage ([budgets](https://learn.microsoft.com/en-us/azure/cost-management-billing/costs/tutorial-acm-create-budgets)). Deduplication and cached diagnoses keep repeated calls down. A failed or timed-out call is charged at its upper bound (the input estimate plus `MaxOutputTokens`). The daily count of analyses lives in the ConfigMap `shadewright-analysis-budget` in the operator namespace, which only the analyzer and admins may write; a count that cannot be read or written, or that is missing, means refuse, not allow (section 3.2, [#43](https://github.com/pkalin/shadewright/issues/43)). It is enforced once the analysis loop ([#76](https://github.com/pkalin/shadewright/issues/76)) reserves a unit before each model call. The spend limit is a count of tokens per model deployment and UTC day (`maxTokensPerDay`), not an amount of money: tokens are what the provider reports and bills, they are whole numbers with no rounding, and a money limit would need a price list in the settings that goes stale. It is per deployment because the triage and escalation models differ in price, and it has no default: the admin sets it for each deployment. The spend it allows is at most that count times the deployment's highest price per token (12.1) only if the counter reserves a call's upper bound (the input bound plus `maxOutputTokens`) before calling, under one lock shared by concurrent analyses, and settles to the real usage afterwards; that is a requirement on the counter (planned, [#208](https://github.com/pkalin/shadewright/issues/208)). Where it is counted is open (#208).

The lab itself (AKS node, Key Vault, storage, Foundry) is priced in `shadewright-azure` and runs only when needed ([ADR-0008](../adr/0008-disposable-azure-lab.md)).
