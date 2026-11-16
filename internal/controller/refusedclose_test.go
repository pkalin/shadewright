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

package controller

import (
	"errors"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestBoundLeftNeverShortensABound: a transition at 12:00:00.900 is persisted as 12:00:00 (metav1.Time keeps whole
// seconds), and neither deletion bound may end before it has lasted its full length from the transition itself.
func TestBoundLeftNeverShortensABound(t *testing.T) {
	at := time.Date(2026, 11, 14, 12, 0, 0, 900*int(time.Millisecond), time.UTC)
	for _, bound := range []time.Duration{closeRefusedGrace, cleanupFailingBound} {
		for name, c := range map[string]*metav1.Condition{
			"persisted": {LastTransitionTime: metav1.NewTime(at.Truncate(time.Second))},
			"in memory": {LastTransitionTime: metav1.NewTime(at)},
		} {
			if left := boundLeft(c, bound, at.Add(bound-time.Millisecond)); left <= 0 {
				t.Errorf("%s, bound %v: %v left a millisecond before the bound ends, want more than zero", name, bound, left)
			}
			if left := boundLeft(c, bound, at.Add(bound+time.Second)); left > 0 {
				t.Errorf("%s, bound %v: %v left a second after the persisted second plus one, want none", name, bound, left)
			}
		}
	}
}

// TestReinstalledDeadlineOfAnotherStepIsSaid (#797): a cleanup through a new installation whose answer from the Git
// host names no step (here a pull request read as gone) and that ran past the deadline is still a failure of the new
// installation: the deadline keeps the *reinstalledError, and PullRequestNotClosed says it with GitFailed.
func TestReinstalledDeadlineOfAnotherStepIsSaid(t *testing.T) {
	timeout := fmt.Errorf("cleanup of the change request: %w", ErrTimeout)
	stepErr := &reinstalledError{err: fmt.Errorf("%w: pull request #7 lacks this request's marker", errChangeRequestNotOurs)}
	err := reinstalledTimeout(stepErr, timeout)
	if _, ok := errors.AsType[*reinstalledError](err); !ok {
		t.Fatalf("reinstalledTimeout = %v, want it inside a *reinstalledError", err)
	}
	if !errors.Is(err, ErrTimeout) || errors.Is(err, errChangeRequestNotOurs) {
		t.Errorf("reinstalledTimeout = %v, want the deadline and not the answer it replaced", err)
	}
	s, ok := reinstalledCloseFailure(err)
	if !ok || s.step != stepOther || s.reason != ReasonGitFailed {
		t.Errorf("reinstalledCloseFailure = %+v, %v, want stepOther with %s", s, ok, ReasonGitFailed)
	}
}
