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

const (
	mainBranch    = "main"
	releaseBranch = "release"
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
		{"applied from the target branch", func(s *providers.DeliveryStatus) { s.AppliedRef = mainBranch }, providers.DeliveryApplied},
		{"applied from another branch", func(s *providers.DeliveryStatus) { s.AppliedRef = releaseBranch }, providers.DeliveryPending},
		{"failure on the commit from another branch", func(s *providers.DeliveryStatus) {
			s.Failed, s.AttemptedRef = true, releaseBranch
		}, providers.DeliveryPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := applied
			tc.mutate(&s)
			got, err := s.Outcome(merge, mainBranch)
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
		if got, err := s.Outcome(commit, mainBranch); !errors.Is(err, providers.ErrInvalidCommit) || got != "" {
			t.Errorf("Outcome(%q) = %q, %v; want ErrInvalidCommit", commit, got, err)
		}
	}
}

// TestDeliveryVerifyLaterCommits builds history in the fake Git host: base, then merge, then later on main,
// and other on a branch cut at base. A unit Ready with a commit applied counts as Applied for merge only when
// that commit is merge or a descendant of it.
func TestDeliveryVerifyLaterCommits(t *testing.T) {
	ctx := t.Context()
	git := fake.NewGit(mainBranch, map[string][]byte{"a": []byte("0")})
	base, _ := git.BranchHead(ctx, mainBranch)
	merge, err := git.Push(mainBranch, "a", []byte("1"))
	if err != nil {
		t.Fatal(err)
	}
	later, err := git.Push(mainBranch, "a", []byte("2"))
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
			got, _ := tc.status.Verify(ctx, merge, mainBranch, git)
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
	if got, err := applied(later).Verify(ctx, merge, mainBranch, git); got != providers.DeliveryPending || !errors.Is(err, providers.ErrRetryable) {
		t.Errorf("Verify with a failing ancestry check = %s, %v; want Pending and the error", got, err)
	}
	// The merge commit itself, and a unit that is not Ready, Current and free of failure, ask no ancestry.
	for _, s := range []providers.DeliveryStatus{
		applied(merge),
		{AppliedCommit: later, Generation: 1, ObservedGeneration: 1},
		{AppliedCommit: later, Ready: true, Generation: 2, ObservedGeneration: 1},
		{AppliedCommit: later, AttemptedCommit: other, AttemptedRef: releaseBranch, Failed: true, Generation: 1, ObservedGeneration: 1},
		{AppliedCommit: "", Ready: true, Generation: 1, ObservedGeneration: 1},
	} {
		if _, err := s.Verify(ctx, merge, mainBranch, git); err != nil {
			t.Errorf("Verify(%+v) asked for ancestry: %v", s, err)
		}
	}
	if got, err := applied(later).Verify(ctx, merge, mainBranch, nil); got != providers.DeliveryPending || err != nil {
		t.Errorf("Verify without an ancestry checker = %s, %v; want Pending", got, err)
	}
	if _, err := applied(later).Verify(ctx, mainBranch, mainBranch, git); !errors.Is(err, providers.ErrInvalidCommit) {
		t.Errorf("Verify of a branch name: %v, want ErrInvalidCommit", err)
	}
}

func TestDeliveryOutcomeNeedsABranch(t *testing.T) {
	s := providers.DeliveryStatus{AppliedCommit: strings.Repeat("a", 40), Ready: true, Generation: 1, ObservedGeneration: 1}
	if got, err := s.Outcome(strings.Repeat("a", 40), ""); !errors.Is(err, providers.ErrInvalidBranch) || got != "" {
		t.Errorf("Outcome without a branch = %q, %v; want ErrInvalidBranch", got, err)
	}
}

// TestDeliveryVerifyLaterFailure checks that a failing attempt of a later commit of the branch, which carries
// the merged change, is Failed, and only then.
func TestDeliveryVerifyLaterFailure(t *testing.T) {
	ctx := t.Context()
	git := fake.NewGit(mainBranch, map[string][]byte{"a": []byte("0")})
	base, _ := git.BranchHead(ctx, mainBranch)
	merge, _ := git.Push(mainBranch, "a", []byte("1"))
	later, _ := git.Push(mainBranch, "a", []byte("2"))
	failing := func(c, ref string) providers.DeliveryStatus {
		return providers.DeliveryStatus{AppliedCommit: base, AppliedRef: mainBranch, AttemptedCommit: c, AttemptedRef: ref,
			Failed: true, Generation: 1, ObservedGeneration: 1}
	}
	for _, tc := range []struct {
		name   string
		status providers.DeliveryStatus
		want   providers.DeliveryOutcome
	}{
		{"the merge commit fails", failing(merge, mainBranch), providers.DeliveryFailed},
		{"a later commit fails", failing(later, mainBranch), providers.DeliveryFailed},
		{"a later commit named by commit only fails", failing(later, ""), providers.DeliveryFailed},
		{"the commit before the merge fails", failing(base, mainBranch), providers.DeliveryPending},
		{"a later commit of another branch fails", failing(later, releaseBranch), providers.DeliveryPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := tc.status.Verify(ctx, merge, mainBranch, git); got != tc.want || err != nil {
				t.Errorf("Verify = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
	git.SetFail(func(op string) error {
		if op == "IsAncestor" {
			return &providers.RetryableError{Err: errors.New("compare failed")}
		}
		return nil
	})
	if got, err := failing(later, mainBranch).Verify(ctx, merge, mainBranch, git); got != providers.DeliveryPending || err == nil {
		t.Errorf("Verify with a failing ancestry check = %s, %v; want Pending and the error", got, err)
	}
}

// TestDeliveryVerifyIncomparableCommit checks that an applied commit that cannot be compared with the merge
// commit (SHA-256 against SHA-1) gives the permanent ErrInvalidCommit without asking the host, not an error
// a caller would retry.
func TestDeliveryVerifyIncomparableCommit(t *testing.T) {
	git := fake.NewGit(mainBranch, map[string][]byte{"a": []byte("0")})
	merge, _ := git.Push(mainBranch, "a", []byte("1"))
	s := providers.DeliveryStatus{AppliedCommit: strings.Repeat("ab", 32), AppliedRef: mainBranch, Ready: true,
		Generation: 1, ObservedGeneration: 1}
	git.SetFail(func(op string) error { return errors.New("asked " + op) })
	got, err := s.Verify(t.Context(), merge, mainBranch, git)
	if got != providers.DeliveryPending || !errors.Is(err, providers.ErrInvalidCommit) || errors.Is(err, providers.ErrRetryable) {
		t.Errorf("Verify = %s, %v; want Pending and a permanent ErrInvalidCommit", got, err)
	}
}
