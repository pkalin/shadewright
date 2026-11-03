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
	"fmt"
	"regexp"
)

// Bounds of a [Symptom], checked by [ValidSymptom].
const (
	// MaxSummaryLength is the longest summary, in bytes.
	MaxSummaryLength = 256
	// MaxEvidence is the most evidence entries one symptom carries.
	MaxEvidence = 8
	// MaxEvidenceValueLength is the longest evidence value, in bytes, once the evaluator has bounded it.
	MaxEvidenceValueLength = 256
)

var (
	symptomReasonPattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,127}$`)
	evidenceFieldPattern = regexp.MustCompile(`^[A-Za-z0-9./_-]{1,128}$`)
)

// Symptom is what a module's rule found: a reason and a summary the rule wrote itself, and evidence read from
// the cluster.
type Symptom struct {
	// Reason is a CamelCase word written by the rule (^[A-Z][A-Za-z0-9]{0,127}$), never cluster text.
	Reason string
	// Summary is fixed text written by the rule, never cluster text, at most 256 bytes.
	Summary string
	// Evidence is what the rule read from the cluster, at most 8 entries.
	Evidence []Evidence
}

// Evidence is one value a rule read from the cluster.
type Evidence struct {
	// Field says where the value was read, written by the rule (^[A-Za-z0-9./_-]{1,128}$).
	Field string
	// Value is the value read, as the rule found it. It is untrusted. The evaluator (planned, #61) redacts it
	// with fingerprint.Normalize, which by itself caps a value at 1024 bytes, and then cuts it to
	// [MaxEvidenceValueLength] bytes. The one exception is a value that is a whole Flux revision
	// (redact.IsFluxRevision), which holds only a reference name and a commit and is kept as it is.
	Value string
}

// ValidSymptom reports why a symptom breaks the contract a rule must keep: a reason that is not a CamelCase
// word of at most 128 characters, a summary over [MaxSummaryLength] bytes, more than [MaxEvidence] evidence
// entries, or an evidence field outside its pattern. Evidence values are not checked: they are untrusted
// and bounded by the evaluator. The evaluator (planned, #61) will call it on every symptom a rule returns, and module
// tests call it on theirs.
func ValidSymptom(s Symptom) error {
	if !symptomReasonPattern.MatchString(s.Reason) {
		return fmt.Errorf("symptom reason %q is not a CamelCase word of at most 128 characters", s.Reason)
	}
	if len(s.Summary) > MaxSummaryLength {
		return fmt.Errorf("symptom %s: summary of %d bytes, at most %d are allowed", s.Reason, len(s.Summary), MaxSummaryLength)
	}
	if len(s.Evidence) > MaxEvidence {
		return fmt.Errorf("symptom %s: %d evidence entries, at most %d are allowed", s.Reason, len(s.Evidence), MaxEvidence)
	}
	for _, e := range s.Evidence {
		if !evidenceFieldPattern.MatchString(e.Field) {
			return fmt.Errorf("symptom %s: evidence field %q is invalid", s.Reason, e.Field)
		}
	}
	return nil
}
