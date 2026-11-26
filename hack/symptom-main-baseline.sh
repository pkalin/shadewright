#!/usr/bin/env bash
# Writes origin/main's stored outputs on the inputs of the stored-symptom differential
# (internal/modules/testdata/symptom-main-regressions.tsv and symptom-main-generated.sha256). It builds commit
# 24cd00a6 from `git archive` in a temporary directory, copies internal/modules/symptom_inputs_test.go into it and
# runs TestWriteSymptomMainBaseline there, so the outputs come from that commit's own code, redact package
# included. The frozen reading (internal/testutil/mainsymptom) is held to these files: regenerating them is a
# reviewed decision, never a way to make a failing test pass.
#
# Usage: hack/symptom-main-baseline.sh   (from the repository root)
set -euo pipefail

commit=24cd00a6703db5f50e656699d67494125a198faf
root=$(git rev-parse --show-toplevel)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

git -C "$root" archive "$commit" | tar -x -C "$work"
cp "$root/internal/modules/symptom_inputs_test.go" "$work/internal/modules/"
(cd "$work" && SYMPTOM_MAIN_BASELINE="$root/internal/modules/testdata" \
	go test ./internal/modules/ -run '^TestWriteSymptomMainBaseline$' -count=1)
