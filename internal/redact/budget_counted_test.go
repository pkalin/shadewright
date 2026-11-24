//go:build redactwork

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

package redact

import (
	"testing"
)

// Every step an entry point counts is counted on its budget (#873, budget.go): the work the linearity tests see
// (tick, with the tag redactwork) is the work the meter sees, so no reading escapes the budget. A rule function
// made when the redactor is made and called later (a closure that kept the meter of New) would count its steps
// on no budget, and fail here.
func TestEveryStepIsMetered(t *testing.T) {
	var inputs []string
	for i, in := range releasedInputs(t) {
		if i%13 == 0 {
			inputs = append(inputs, in)
		}
	}
	measures := corpusShapes(16 << 10)
	for _, name := range []string{`stair "pass\u200b: |"`, "form key and value in two strings", "repeated quoted keys"} {
		in, ok := measures[name]
		if !ok {
			t.Fatalf("no measure %s", name)
		}
		inputs = append(inputs, in)
	}
	for _, in := range budgetShapes(4 << 10) {
		inputs = append(inputs, in)
	}
	for _, in := range shortFlagShapes(4 << 10) {
		inputs = append(inputs, in)
	}
	for _, e := range budgetEntries {
		for i, in := range inputs {
			if n := Unmetered(func() { e.public(in) }); n != 0 {
				t.Fatalf("%s counted %d of its steps on no budget, on input %d (%d bytes)", e.name, n, i, len(in))
			}
		}
	}
}
