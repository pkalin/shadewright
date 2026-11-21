---
id: numbers-name-their-test
name: Measured figures name their test
mode: warning
paths:
  - "docs/adr/**"
  - "docs/design/**"
  - "docs/threat-model.md"
exclude:
  - "docs/how-it-works/**"
---
Fail if the diff adds or changes a measured figure and the same paragraph or table row does not name the
test or the benchmark that measures it. A measured figure is the result of a measurement of this code: a
size at which something is withheld, steps or bytes per byte, a peak of memory, a run time, a share of a
sample.

Pass for a configured value or default (a window, a hold, a timeout, a limit an operator can set, a retry
count of a library): the constant or setting is its source and the page need not name it. Pass for dates,
versions, issue numbers, counts of items in a list, and any page for operators outside the paths above.
