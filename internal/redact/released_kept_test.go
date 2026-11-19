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
	"slices"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/released"
	v052 "github.com/pkalin/shadewright/internal/redact/released/testdata/v052"
)

// The reading of the release before finds, on every input of the corpora releasedTable is written from, the
// same spans (start, end and kind) as the copy of its code before #873 made its work linear
// (released/testdata/v052): the change of #873 redacts neither more nor less on any of them. String's output on
// the same inputs is checked against the release's stored results by TestReleasedReproducesTheReleaseBefore.
// Under -race a seeded sample of the inputs is compared (raceKeeps), without it all of them.
func TestReleasedSpansMatchKeptCopy(t *testing.T) {
	seen := map[string]bool{}
	fails := 0
	for i, in := range releasedInputs(t) {
		if seen[in] || !raceKeeps("TestReleasedSpansMatchKeptCopy", i) {
			continue
		}
		seen[in] = true
		got, want := released.Spans(in), v052.Spans(in)
		if !slices.EqualFunc(got, want, func(a released.Span, b v052.Span) bool {
			return a.Start == b.Start && a.End == b.End && a.Kind == b.Kind
		}) {
			t.Errorf("on %q: spans %v, before #873 %v", in, got, want)
			if fails++; fails == 10 {
				t.Fatal("stopping after ten differences")
			}
		}
	}
	t.Logf("%d distinct inputs compared", len(seen))
}
