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

package tools

import (
	"testing"

	"github.com/pkalin/shadewright/internal/redact"
)

// The choice of the notes list_events keeps is linear in the number of notes (#1106): twice the notes, at most
// twice the work and a step, on notes that all fit, none that fit, and every other one fitting.
func TestNotesWithinWorkIsLinear(t *testing.T) {
	work := func(n int, size func(i int) int) int64 {
		lens := make([]int, n)
		for i := range lens {
			lens[i] = size(i)
		}
		return redact.Work(func() { notesWithin(redact.LargestEntry, 0, lens) })
	}
	for name, size := range map[string]func(int) int{
		"all fit":   func(int) int { return 10 },
		"none fits": func(int) int { return redact.LargestEntry },
		"alternate": func(i int) int { return i % 2 * redact.LargestEntry },
	} {
		one, two := work(1000, size), work(2000, size)
		if one == 0 || two > 2*one+1 {
			t.Errorf("%s: work %d for 1000 notes, %d for 2000", name, one, two)
		}
	}
}
