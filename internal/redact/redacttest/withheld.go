/*
Copyright 2026 The Shadewright Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package redacttest

import (
	"fmt"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// SmallInput is the size under which a fuzz input that redaction withholds for work fails its target (P6, #873,
// decision of 2026-11-20): the budget gives every byte a fixed number of steps, so an input this small runs out of
// it only when the work of some reading grows faster than the text. The target then fails and the fuzzer keeps the
// input in testdata/fuzz, so a new superlinear shape is a finding and not a silent withhold. Inputs withheld for
// size (longer than redact.MaxEntry) are far above it.
const SmallInput = 64 << 10

// withheldMetric and workReason name the counter of package redact and its series for the budget
// (shadewright_redaction_withheld_total{reason="work"}, redact.ReasonWork); this package does not import redact,
// whose tests import it.
const (
	withheldMetric = "shadewright_redaction_withheld_total"
	workReason     = "work"
)

// WorkWithheld returns how many entries redaction has withheld for work in this process, read from the counter
// every component registers. A fuzz target reads it before and after an input: the fuzzer runs one input at a
// time in a process, and a go test run reaches the fuzz targets after its tests, parallel ones included, have
// finished, so nothing else redacts meanwhile.
func WorkWithheld() float64 {
	families, err := metrics.Registry.Gather()
	if err != nil {
		panic(fmt.Sprintf("gathering the metrics: %v", err))
	}
	for _, f := range families {
		if f.GetName() != withheldMetric {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == workReason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// SmallWithheldFails returns the P6 failure of an input of size bytes when redaction withheld an entry for work
// since before (a value of [WorkWithheld]) and the input is under [SmallInput], or nothing.
func SmallWithheldFails(what string, size int, before float64) []string {
	if n := WorkWithheld() - before; n > 0 && size < SmallInput {
		return []string{fmt.Sprintf("P6: %s withheld %.0f entries for work on an input of %d bytes, under %d: "+
			"a reading whose work grows faster than the text", what, n, size, SmallInput)}
	}
	return nil
}

// FailSmallWithheld fails t with the P6 failure of [SmallWithheldFails]: for a fuzz target that has no list of
// known failures.
func FailSmallWithheld(t testing.TB, what string, size int, before float64) {
	t.Helper()
	if fails := SmallWithheldFails(what, size, before); len(fails) > 0 {
		t.Fatal(fails[0])
	}
}
