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

package released

import (
	"math/rand/v2"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// The reading of the release before sorts its spans with sortFunc (#873, decision of 2026-11-20): a sort with no
// budget left stops, as 65,536 shuffled spans take far more comparisons than four steps each.
func TestSortWithNoBudgetLeftStops(t *testing.T) {
	const n = 65536
	rng := rand.New(rand.NewPCG(873, 3)) // #nosec G404 -- reproducible order
	spans := make([]span, n)
	for i, j := range rng.Perm(n) {
		spans[i] = span{start: 2 * j, end: 2*j + 1}
	}
	wm := meter.New(4 * n)
	over := false
	func() {
		defer meter.Catch(&over)
		mergeRanges(wm, spans)
	}()
	if !over {
		t.Errorf("mergeRanges of %d shuffled spans spent %d steps and did not stop on a budget of %d", n, wm.Spent(), 4*n)
	}
}
