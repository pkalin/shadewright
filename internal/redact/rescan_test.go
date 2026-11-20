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
	"time"
)

// An extra pattern whose expression reads the rest of the text on every match attempt (a.*z|a on a run of a:
// each of the 32768 matches is one "a", found only after the engine read to the end looking for a z) costs the
// square of the text. Its scan is charged per byte the engine reads, so the entry is withheld once the reading
// passes the budget, instead of running for seconds and returning a result (#873, decision of 2026-11-20).
func TestExpressionRescanIsChargedPerAttempt(t *testing.T) {
	r, err := New(Pattern{Kind: "custom", Expr: "a.*z|a"})
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Repeat("a", 32768)
	start := time.Now()
	out, ok := r.StringChecked(s)
	took := time.Since(start)
	if ok || out != Withheld {
		t.Errorf("a run of %d a: ok %v, %d bytes out; want withheld", len(s), ok, len(out))
	}
	if !raceDetector && took > 3*time.Second {
		t.Errorf("withheld after %v, want the budget to stop the scan early", took)
	}
}
