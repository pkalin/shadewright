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

package providers_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

func TestDeliveryOutcome(t *testing.T) {
	merge := strings.Repeat("ab", 20)
	other := strings.Repeat("cd", 20)
	applied := providers.DeliveryStatus{
		AppliedCommit: merge, AttemptedCommit: merge, Ready: true, Generation: 2, ObservedGeneration: 2,
	}
	for _, tc := range []struct {
		name   string
		mutate func(*providers.DeliveryStatus)
		want   providers.DeliveryOutcome
	}{
		{"applied, Ready, current", func(*providers.DeliveryStatus) {}, providers.DeliveryApplied},
		{"another commit applied", func(s *providers.DeliveryStatus) { s.AppliedCommit = other }, providers.DeliveryPending},
		{"no commit applied", func(s *providers.DeliveryStatus) { s.AppliedCommit = "" }, providers.DeliveryPending},
		{"not Ready", func(s *providers.DeliveryStatus) { s.Ready = false }, providers.DeliveryPending},
		{"older generation observed", func(s *providers.DeliveryStatus) { s.ObservedGeneration = 1 }, providers.DeliveryPending},
		{"no generation", func(s *providers.DeliveryStatus) { s.Generation, s.ObservedGeneration = 0, 0 }, providers.DeliveryPending},
		{"failure on the commit", func(s *providers.DeliveryStatus) { s.Failed = true }, providers.DeliveryFailed},
		{"failure on another commit", func(s *providers.DeliveryStatus) {
			s.Failed, s.AttemptedCommit = true, other
		}, providers.DeliveryPending},
		{"failure without an attempted commit", func(s *providers.DeliveryStatus) {
			s.Failed, s.AttemptedCommit = true, ""
		}, providers.DeliveryPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := applied
			tc.mutate(&s)
			got, err := s.Outcome(merge)
			if err != nil || got != tc.want {
				t.Errorf("Outcome = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestDeliveryOutcomeRefusesInvalidCommit checks that a commit that is not a full object ID gives no outcome,
// also against a status that holds the same text.
func TestDeliveryOutcomeRefusesInvalidCommit(t *testing.T) {
	for _, commit := range []string{"", "abc", strings.Repeat("A", 40), strings.Repeat("a", 39), strings.Repeat("a", 41)} {
		s := providers.DeliveryStatus{AppliedCommit: commit, Ready: true, Generation: 1, ObservedGeneration: 1}
		if got, err := s.Outcome(commit); !errors.Is(err, providers.ErrInvalidCommit) || got != "" {
			t.Errorf("Outcome(%q) = %q, %v; want ErrInvalidCommit", commit, got, err)
		}
	}
}
