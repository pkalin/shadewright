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
	"github.com/pkalin/shadewright/internal/providers/fake"
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

// TestDeliveryVerifyLaterCommits builds history in the fake Git host: base, then merge, then later on main,
// and other on a branch cut at base. A unit Ready with a commit applied counts as Applied for merge only when
// that commit is merge or a descendant of it.
func TestDeliveryVerifyLaterCommits(t *testing.T) {
	ctx := t.Context()
	git := fake.NewGit("main", map[string][]byte{"a": []byte("0")})
	base, _ := git.BranchHead(ctx, "main")
	merge, err := git.Push("main", "a", []byte("1"))
	if err != nil {
		t.Fatal(err)
	}
	later, err := git.Push("main", "a", []byte("2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := git.CreateBranch(ctx, "shadewright/x", base); err != nil {
		t.Fatal(err)
	}
	other, err := git.Push("shadewright/x", "a", []byte("3"))
	if err != nil {
		t.Fatal(err)
	}
	applied := func(c string) providers.DeliveryStatus {
		return providers.DeliveryStatus{AppliedCommit: c, AttemptedCommit: c, Ready: true, Generation: 1, ObservedGeneration: 1}
	}
	for _, tc := range []struct {
		name   string
		status providers.DeliveryStatus
		want   providers.DeliveryOutcome
	}{
		{"the merge commit", applied(merge), providers.DeliveryApplied},
		{"a later commit of the branch", applied(later), providers.DeliveryApplied},
		{"the commit before the merge", applied(base), providers.DeliveryPending},
		{"an unrelated commit", applied(other), providers.DeliveryPending},
		{"a commit the host does not know", applied(strings.Repeat("e", 40)), providers.DeliveryPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := tc.status.Verify(ctx, merge, git)
			if got != tc.want {
				t.Errorf("Verify = %s, want %s", got, tc.want)
			}
		})
	}

	// An ancestry check that fails is Pending with the error, never Applied.
	git.SetFail(func(op string) error {
		if op == "IsAncestor" {
			return &providers.RetryableError{Err: errors.New("compare failed")}
		}
		return nil
	})
	if got, err := applied(later).Verify(ctx, merge, git); got != providers.DeliveryPending || !errors.Is(err, providers.ErrRetryable) {
		t.Errorf("Verify with a failing ancestry check = %s, %v; want Pending and the error", got, err)
	}
	// The merge commit itself, and a unit that is not Ready, Current and free of failure, ask no ancestry.
	for _, s := range []providers.DeliveryStatus{
		applied(merge),
		{AppliedCommit: later, Generation: 1, ObservedGeneration: 1},
		{AppliedCommit: later, Ready: true, Generation: 2, ObservedGeneration: 1},
		{AppliedCommit: later, AttemptedCommit: other, Failed: true, Ready: true, Generation: 1, ObservedGeneration: 1},
		{AppliedCommit: "", Ready: true, Generation: 1, ObservedGeneration: 1},
	} {
		if _, err := s.Verify(ctx, merge, git); err != nil {
			t.Errorf("Verify(%+v) asked for ancestry: %v", s, err)
		}
	}
	if got, err := applied(later).Verify(ctx, merge, nil); got != providers.DeliveryPending || err != nil {
		t.Errorf("Verify without an ancestry checker = %s, %v; want Pending", got, err)
	}
	if _, err := applied(later).Verify(ctx, "main", git); !errors.Is(err, providers.ErrInvalidCommit) {
		t.Errorf("Verify of a branch name: %v, want ErrInvalidCommit", err)
	}
}
