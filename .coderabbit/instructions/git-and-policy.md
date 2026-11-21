---
id: git-and-policy
name: Git writes and policy
paths:
  - "internal/gitops/**"
  - "internal/policy/**"
  - "internal/providers/**"
  - "api/**"
  - "config/rbac/**"
---
Invariants 1, 2, 3 and 6. A write to Git is a branch and a pull request, never a push to a protected
branch and never to a path outside the allowed ones. A new action is an enum value with bounded
parameters, a policy rule, a patcher and a test that it is refused outside its bounds. A new RBAC verb,
a read on more objects or a new credential needs an accepted ADR: name the ADR or say none is cited.
Credentials are read from a mounted file at each use and never reach a log line or an error.
