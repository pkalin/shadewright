---
id: reconcile
name: Reconcile code
paths:
  - "internal/controller/**"
  - "internal/incidents/**"
  - "internal/analyzer/**"
exclude:
  - "**/*_test.go"
---
Invariant 5. Look for: a status write that is lost when a later step fails or the context is cancelled; a
requeue with no delay; a retry that leaves no condition, event or log line; a step that a final phase
skips; a second pull request or branch after a restart; a decision taken from a read that failed, was
refused or timed out. A new status field or phase needs its CRD validation and its transition tests.
