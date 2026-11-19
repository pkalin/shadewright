#!/usr/bin/env python3
"""Checks that docs/reasons.md lists every condition type and reason the code can set, and no other.

Usage (standard library only, run with -I):
  hack/docs-reasons-check.py [--root DIR] [--page FILE]

The reasons are read from the Go constants (Reason* and the ResetReason values) of the packages that write
conditions; the condition types from the Condition* constants. A row of the page starts with
<span id="group-reasonname"></span>`ReasonName`; a condition type is a heading that holds its name.
Exit 1 when a reason or a condition type has no entry, or an entry names one that no longer exists.
"""
import argparse
import glob
import os
import re
import sys

# Packages whose constants end up as the reason or the type of a condition.
SOURCES = [
    "api/v1alpha1/*.go",
    "internal/controller/*.go",
    "internal/analysis/*.go",
    "internal/incidents/*.go",
    "internal/incidents/analysisdue/*.go",
    "internal/verification/rollout/*.go",
    "internal/verification/clearing/*.go",
    "internal/policy/decide.go",
    "internal/policy/freshness/*.go",
    "internal/gitops/sourcemap/fluxrules/*.go",
    "internal/gitops/semdiff/errors.go",
    "internal/analyzer/detection/*.go",
]

# Reason constants in those packages that are not the reason of a condition, with why.
NOT_CONDITION_REASONS = {
    # Reasons of a reviewer hint in status.hints, not of a condition.
    "NoAnswer": "hint reason",
    "HintPermissionUnknown": "hint reason",
    "ReplacementDenied": "hint reason",
    "RequestEnded": "hint reason",
    "PullRequestMerged": "hint reason",
    # Reasons of Warning Events only.
    "ReviewCheckStuck": "Event reason",
    "ReviewCommentsUnread": "Event reason",
    "ReviewCheckConflict": "Event reason",
    # The outcome of a successful source mapping; the condition then carries the policy's reason.
    "SourceResolved": "mapping outcome",
}

REASON_RE = re.compile(
    r'^\s*(?:const\s+)?(Reason[A-Z]\w*|Reset[A-Z]\w*\s+ResetReason)\s+(?:[\w.]+\s+)?=\s*"([^"]+)"', re.M)
COND_RE = re.compile(r'^\s*(?:const\s+)?Condition[A-Z]\w*\s+(?:[\w.]+\s+)?=\s*"([^"]+)"', re.M)
ROW_RE = re.compile(r'^\|\s*<span id="([a-z0-9]+)-([a-z0-9]+)"></span>`([A-Za-z0-9]+)`', re.M)
HEAD_RE = re.compile(r'^#{2,4} .*$', re.M)


def code_names(root):
    reasons, types = set(), set()
    for pat in SOURCES:
        for f in sorted(glob.glob(os.path.join(root, pat))):
            if f.endswith("_test.go"):
                continue
            with open(f, encoding="utf-8") as fh:
                text = fh.read()
            reasons.update(m.group(2) for m in REASON_RE.finditer(text))
            types.update(COND_RE.findall(text))
    return reasons - set(NOT_CONDITION_REASONS), types


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default=os.path.dirname(here))
    ap.add_argument("--page")
    a = ap.parse_args()
    page = a.page or os.path.join(a.root, "docs", "reasons.md")
    with open(page, encoding="utf-8") as fh:
        text = fh.read()
    reasons, types = code_names(a.root)
    problems = []

    seen = set()
    page_reasons = set()
    for m in ROW_RE.finditer(text):
        anchor, name = m.group(1) + "-" + m.group(2), m.group(3)
        if m.group(2) != name.lower():
            problems.append("row %s has the anchor #%s, which must end with the lower case reason" % (name, anchor))
        if anchor in seen:
            problems.append("anchor #%s is used twice" % anchor)
        seen.add(anchor)
        page_reasons.add(name)
    for r in sorted(reasons - page_reasons):
        problems.append("reason %s is in the code but has no row in %s" % (r, os.path.basename(page)))
    for r in sorted(page_reasons - reasons):
        problems.append("row %s names a reason that is not a condition reason in the code" % r)

    heads = "\n".join(HEAD_RE.findall(text))
    for t in sorted(types):
        if not re.search(r"\b" + re.escape(t) + r"\b", heads):
            problems.append("condition type %s has no heading in %s" % (t, os.path.basename(page)))

    for p in problems:
        print("docs-reasons-check: " + p, file=sys.stderr)
    if problems:
        return 1
    print("docs-reasons-check: %d reasons and %d condition types covered" % (len(reasons), len(types)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
