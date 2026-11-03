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

package modules

import (
	"strings"
	"testing"
)

// What refusals of a bad reason or evidence field say.
const (
	wantReason = "reason"
	wantField  = "evidence field"
)

func TestValidSymptom(t *testing.T) {
	valid := func() Symptom {
		return Symptom{Reason: "BuildFailed", Summary: "The build failed.",
			Evidence: []Evidence{{Field: "status.conditions.Ready.message", Value: strings.Repeat("x", 2048)}}}
	}
	if err := ValidSymptom(valid()); err != nil {
		t.Fatalf("a valid symptom is refused: %v", err)
	}
	full := valid()
	full.Summary = strings.Repeat("s", MaxSummaryLength)
	full.Reason = "A" + strings.Repeat("b", 127)
	for range MaxEvidence - 1 {
		full.Evidence = append(full.Evidence, Evidence{Field: "a_b-c/d.e"})
	}
	if err := ValidSymptom(full); err != nil {
		t.Errorf("a symptom at every limit is refused: %v", err)
	}
	cases := []struct {
		name   string
		change func(s *Symptom)
		want   string
	}{
		{"empty reason", func(s *Symptom) { s.Reason = "" }, wantReason},
		{"lower-case reason", func(s *Symptom) { s.Reason = "buildFailed" }, wantReason},
		{"reason with a space", func(s *Symptom) { s.Reason = "Build Failed" }, wantReason},
		{"reason too long", func(s *Symptom) { s.Reason = "A" + strings.Repeat("b", 128) }, wantReason},
		{"summary too long", func(s *Symptom) { s.Summary = strings.Repeat("s", MaxSummaryLength+1) }, "summary"},
		{"too much evidence", func(s *Symptom) { s.Evidence = make([]Evidence, MaxEvidence+1) }, "evidence entries"},
		{"empty field", func(s *Symptom) { s.Evidence[0].Field = "" }, wantField},
		{"field with a bracket", func(s *Symptom) { s.Evidence[0].Field = "status.conditions[0]" }, wantField},
		{"field too long", func(s *Symptom) { s.Evidence[0].Field = strings.Repeat("f", 129) }, wantField},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := valid()
			c.change(&s)
			if err := ValidSymptom(s); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("ValidSymptom = %v, want an error saying %q", err, c.want)
			}
		})
	}
}
