# 14. Testing

Tests target process behaviour, not only code paths.

They cover:

- deduplication across restarts;
- idempotency after a crash between an API call and the status write;
- prompt injection in events and logs;
- refusal of disallowed actions;
- approval invalidated by a plan change;
- branch rules;
- model and GitHub outages;
- an incident that changes while the PR waits for review.

Pilot metrics:

- Time to diagnosis and to fix.
- Events per incident.
- Operator-rated plan accuracy.
- Share of accepted PRs.
- Failed or reverted changes.
- Cost and tokens per incident.
- Actions blocked by policy.
