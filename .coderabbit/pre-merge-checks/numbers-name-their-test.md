---
id: numbers-name-their-test
name: Numbers in docs name their test
mode: warning
paths:
  - "docs/**"
---
Fail if the diff adds or changes a measured size, threshold, duration or cost in a docs page and the same
paragraph or table row does not name the test or the constant that holds it. Pass for dates, versions,
issue numbers and counts of items in a list.
