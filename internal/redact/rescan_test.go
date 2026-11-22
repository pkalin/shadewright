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
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// An extra pattern whose expression reads the rest of the text on every match attempt (a.*z|a on a run of a:
// each of the 4096 matches is one "a", found only after the engine read to the end looking for a z) costs the
// square of the text, about twice the budget of the entry here. Its scan is charged per byte the engine
// reads, so the entry is withheld once the reading passes the budget, instead of running for seconds and
// returning a result (#873, decision of 2026-11-20). The test counts the steps the budget saw, not seconds
// (decision of 2026-11-21): the scan stops within one attempt of the budget running out, so the steps spent
// pass the budget by at most the length of the text, where a scan read to the end before it is charged would
// spend about twice the budget.
func TestExpressionRescanIsChargedPerAttempt(t *testing.T) {
	parallelUnderRace(t)
	r, err := New(Pattern{Kind: "custom", Expr: "a.*z|a"})
	if err != nil {
		t.Fatal(err)
	}
	const n = 4096
	s := strings.Repeat("a", n)
	if out, ok := r.StringChecked(s); ok || out != Withheld {
		t.Errorf("a run of %d a: ok %v, %d bytes out; want withheld", len(s), ok, len(out))
	}
	budget := budgetOf(n)
	wm := meter.New(budget)
	over := false
	func() {
		defer meter.Catch(&over)
		render(wm, s, r.spans(wm, s))
	}()
	if !over || wm.Spent() > budget+n {
		t.Errorf("a run of %d a: stopped %v after %d steps, want stopped within %d steps of the budget of %d",
			n, over, wm.Spent(), n, budget)
	}
}
