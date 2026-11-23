---
id: docs-with-code
name: Docs change with the code
mode: warning
paths:
  - "internal/**"
  - "cmd/**"
  - "api/**"
exclude:
  - "**/*_test.go"
---
Pass if a change of behaviour under these paths comes with a change under docs/ in the same pull request.
Pass if only tests, comments or names change. Fail if behaviour that a docs page describes changes and no
docs file does.
