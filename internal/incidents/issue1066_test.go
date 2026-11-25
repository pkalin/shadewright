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

package incidents

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/analyzer/collector"
	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
)

// The tests of #1066 at the Incident controller (ADR-0039 point 12, amended 2026-11-21):
//   - kubelet image-pull Failed events (ClassConsequence, Reason "Failed") count on their container's image-pull
//     cause Incident (ImagePullBackOff, ErrImagePull or InvalidImageName) and open no Incident of their own;
//   - a pull Failed event is never absorbed by a non-pull cause (OOMKilled, non-zero exit, CreateContainerConfigError,
//     CreateContainerError), and a non-pull Failed event (ClassNone) is never absorbed by anything;
//   - a pull Failed Incident opened before its container's pull cause Incident is folded into it within RecurWithin
//     (15 minutes), while a non-pull Failed Incident on the same container is never folded.

const (
	reasonFailed      = "Failed"
	reasonImagePull   = "ImagePullBackOff"
	reasonErrImage    = "ErrImagePull"
	reasonInvalidImg  = "InvalidImageName"
	reasonCreateError = "CreateContainerError"
)

func pullCause(reason string, at time.Time) collector.Observation {
	o := symptomOf(1, reason, waitingID(1), 1, fingerprint.SourcePodStatus, foldContainer, collector.ClassCause, at)
	o.Began = at.Add(-RecurAfter)
	return o
}

// TestIssue1066ImagePullOpensOneIncident reproduces the rc.3 bug at the Incident controller: one image-pull cause
// Incident absorbs the three kubelet Failed pull events and BackOff of its container, opening 1 Incident in all.
func TestIssue1066ImagePullOpensOneIncident(t *testing.T) {
	for _, waitReason := range []string{reasonImagePull, reasonErrImage, reasonInvalidImg} {
		t.Run(waitReason, func(t *testing.T) {
			c, clock := newAt(t)
			at := t0.Add(5 * time.Minute)
			clock.set(at)
			c.consume(t.Context(), pullCause(waitReason, at))
			if got := opened(c); !slices.Equal(got, []string{fp(1)}) {
				t.Fatalf("after %s cause, opened %v, want [%s]", waitReason, got, fp(1))
			}

			// Three distinct Failed pull event fingerprints (different notes) + BackOff.
			c.consume(t.Context(), consequenceOf(2, 1, "web", reasonFailed, 2, 3, at.Add(time.Second)))
			c.consume(t.Context(), consequenceOf(3, 1, "web", reasonFailed, 3, 2, at.Add(2*time.Second)))
			c.consume(t.Context(), consequenceOf(4, 1, "web", reasonFailed, 4, 4, at.Add(3*time.Second)))
			c.consume(t.Context(), consequenceOf(5, 1, "web", reasonBackOff, 5, 5, at.Add(4*time.Second)))

			if got := opened(c); !slices.Equal(got, []string{fp(1)}) {
				t.Errorf("%s with pull Failed and BackOff opened %d Incidents %v, want 1 (%s)",
					waitReason, len(got), got, fp(1))
			}
			c.mu.Lock()
			b := c.pending[fp(1)]
			failedCount := b.absorbed[remediationv1alpha1.AbsorbedReason(reasonFailed)].count
			backOffCount := b.absorbed[remediationv1alpha1.AbsorbedBackOff].count
			c.mu.Unlock()
			if failedCount != 9 {
				t.Errorf("absorbed Failed count %d, want 9", failedCount)
			}
			if backOffCount != 5 {
				t.Errorf("absorbed BackOff count %d, want 5", backOffCount)
			}
		})
	}
}

// TestIssue1066FailedAbsorptionBoundaries checks that:
//   - a pull Failed event is NOT absorbed by OOMKilled, a non-zero exit (Error), CreateContainerConfigError or
//     CreateContainerError, and CauseSeen answers false for each;
//   - a non-pull Failed event (ClassNone) is NOT absorbed by ImagePullBackOff and opens its own Incident.
func TestIssue1066FailedAbsorptionBoundaries(t *testing.T) {
	t.Run("non-pull causes never absorb pull Failed", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			cause func(at time.Time) collector.Observation
		}{
			{"OOMKilled", func(at time.Time) collector.Observation { return oomCause(1, at) }},
			{"Error", func(at time.Time) collector.Observation {
				o := symptomOf(1, "Error", terminationID(1), 1, fingerprint.SourcePodStatus, "web", collector.ClassCause, at)
				o.Began = at.Add(-RecurAfter)
				return o
			}},
			{"CreateContainerConfigError", func(at time.Time) collector.Observation {
				return waitingCause(1, "web", 1, at)
			}},
			{"CreateContainerError", func(at time.Time) collector.Observation {
				return pullCause(reasonCreateError, at)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c, clock := newAt(t)
				at := t0.Add(5 * time.Minute)
				clock.set(at)
				c.consume(t.Context(), tc.cause(at))
				pullFailed := consequenceOf(2, 1, "web", reasonFailed, 2, 2, at.Add(time.Second))
				c.mu.Lock()
				seen := c.causeSeen(pullFailed)
				c.mu.Unlock()
				if seen {
					t.Errorf("CauseSeen(%s, pull Failed) = true, want false", tc.name)
				}
				c.consume(t.Context(), pullFailed)
				if got := opened(c); !slices.Equal(got, []string{fp(1), fp(2)}) {
					t.Errorf("open Incidents %v, want both cause %s and pull Failed %s", got, fp(1), fp(2))
				}
			})
		}
	})

	t.Run("non-pull Failed opens its own Incident alongside ImagePullBackOff", func(t *testing.T) {
		c, clock := newAt(t)
		at := t0.Add(5 * time.Minute)
		clock.set(at)
		c.consume(t.Context(), pullCause(reasonImagePull, at))
		nonPull := symptomOf(2, reasonFailed, eventID(2), 2, fingerprint.SourceEvent, "web", collector.ClassNone, at.Add(time.Second))
		nonPull.Began = at.Add(-RecurAfter)
		nonPull.CauseFingerprint = fp(1) // even if somehow set, ClassNone is never absorbed
		c.consume(t.Context(), nonPull)
		if got := opened(c); !slices.Equal(got, []string{fp(1), fp(2)}) {
			t.Errorf("open Incidents %v, want both %s and non-pull Failed %s", got, fp(1), fp(2))
		}
	})
}

// TestIssue1066PullFailedFoldsIntoPullCause checks Decision 2 row 4 and row 6: when a pull Failed event opens an
// Incident before its container's pull cause Incident (ImagePullBackOff, ErrImagePull or InvalidImageName), the
// pull cause folds it within RecurWithin, while a non-pull Failed Incident on the same container, a pull Failed
// Incident on another container, one older than RecurWithin, one with a pull request, or any pull Failed Incident
// against a non-pull cause stays untouched.
func TestIssue1066PullFailedFoldsIntoPullCause(t *testing.T) {
	// Write a pull Failed Incident and a non-pull Failed Incident through the controller's merge path so the
	// stored status is exactly what the controller writes.
	buildStored := func(t *testing.T, f int, uid types.UID, o collector.Observation, created time.Time) *remediationv1alpha1.Incident {
		t.Helper()
		c, clock := newAt(t)
		clock.set(created)
		c.consume(t.Context(), o)
		c.mu.Lock()
		b := c.pending[fp(f)]
		c.mu.Unlock()
		if b == nil {
			t.Fatalf("fp(%d) did not open", f)
		}
		inc := &remediationv1alpha1.Incident{
			ObjectMeta: metav1.ObjectMeta{
				Name: Name(fp(f)), Namespace: fakeNamespace, UID: uid,
				CreationTimestamp: metav1.NewTime(created),
			},
			Spec: specOf(o),
		}
		merge(&inc.Status, b)
		addContainers(&inc.Status, b)
		return inc
	}

	at := triggerNow
	pullFailedInc := buildStored(t, 2, foldConsequenceUID,
		consequenceOf(2, 0, foldContainer, reasonFailed, 2, 2, at.Add(-2*time.Minute)),
		at.Add(-2*time.Minute))
	nonPullFailed := symptomOf(3, reasonFailed, eventID(3), 2, fingerprint.SourceEvent, foldContainer, collector.ClassNone, at.Add(-2*time.Minute))
	nonPullFailed.Began = at.Add(-5 * time.Minute)
	nonPullFailedInc := buildStored(t, 3, types.UID("63636363-6363-4363-8363-636363636363"),
		nonPullFailed, at.Add(-2*time.Minute))
	otherContainerInc := buildStored(t, 4, types.UID("64646464-6464-4464-8464-646464646464"),
		consequenceOf(4, 0, "proxy", reasonFailed, 4, 2, at.Add(-2*time.Minute)),
		at.Add(-2*time.Minute))
	tooOldInc := buildStored(t, 5, types.UID("65656565-6565-4565-8565-656565656565"),
		consequenceOf(5, 0, foldContainer, reasonFailed, 5, 2, at.Add(-RecurWithin-time.Second)),
		at.Add(-RecurWithin-time.Second))

	for _, waitReason := range []string{reasonImagePull, reasonErrImage, reasonInvalidImg} {
		cause := buildStored(t, 1, foldCauseUID, pullCause(waitReason, at), at)
		if !foldable(pullFailedInc, cause, nil) {
			t.Errorf("pull Failed Incident did not fold into %s cause", waitReason)
		}
		if foldable(nonPullFailedInc, cause, nil) {
			t.Errorf("non-pull Failed Incident folded into %s cause, want untouched", waitReason)
		}
		if foldable(otherContainerInc, cause, nil) {
			t.Errorf("pull Failed Incident of another container folded into %s cause, want untouched", waitReason)
		}
		if foldable(tooOldInc, cause, nil) {
			t.Errorf("pull Failed Incident older than RecurWithin folded into %s cause, want untouched", waitReason)
		}
		withPR := []remediationv1alpha1.RemediationRequest{foldRequest("rr-1", remediationv1alpha1.PhasePRCreated, true)}
		if foldable(pullFailedInc, cause, withPR) {
			t.Errorf("pull Failed Incident with a PR folded into %s cause, want untouched", waitReason)
		}
	}

	// Non-pull causes never fold a pull Failed Incident.
	for _, otherReason := range []string{reasonOOM, reasonExitError, reasonConfig, reasonCreateError} {
		cause := foldCause()
		cause.Spec.Reason = otherReason
		if foldable(pullFailedInc, cause, nil) {
			t.Errorf("pull Failed Incident folded into non-pull cause %s, want untouched", otherReason)
		}
	}
}

// TestIssue1066ConsequenceStatusFlagSafety checks that status.consequence is set only when an Incident starts from
// consequence observations alone, stays true across further consequence batches, clears if a non-consequence
// observation ever lands on the same fingerprint, and is never set on an Incident that already counted a
// non-consequence observation or is a cause.
func TestIssue1066ConsequenceStatusFlagSafety(t *testing.T) {
	// Consequence-only start -> Consequence true, stays true on further consequence batches.
	var st remediationv1alpha1.IncidentStatus
	merge(&st, &batch{count: 1, first: t0, last: t0, consequence: true})
	if !st.Consequence {
		t.Fatalf("consequence-only first write: Consequence = false, want true")
	}
	merge(&st, &batch{count: 2, first: t0, last: t0.Add(time.Minute), consequence: true})
	if !st.Consequence {
		t.Fatalf("second consequence-only write: Consequence = false, want true")
	}
	// A non-consequence observation colliding on the same fingerprint clears Consequence so it can never be folded.
	merge(&st, &batch{count: 1, first: t0, last: t0.Add(2 * time.Minute), nonConsequence: true})
	if st.Consequence {
		t.Errorf("after non-consequence observation: Consequence = true, want false")
	}

	// An Incident started from a non-consequence observation never becomes Consequence true.
	var nonCons remediationv1alpha1.IncidentStatus
	merge(&nonCons, &batch{count: 1, first: t0, last: t0, nonConsequence: true})
	if nonCons.Consequence {
		t.Fatalf("non-consequence first write: Consequence = true, want false")
	}
	merge(&nonCons, &batch{count: 1, first: t0, last: t0.Add(time.Minute), consequence: true})
	if nonCons.Consequence {
		t.Errorf("consequence batch on existing non-consequence Incident: Consequence = true, want false")
	}

	// A mixed first batch (consequence and non-consequence together) leaves Consequence false.
	var mixed remediationv1alpha1.IncidentStatus
	merge(&mixed, &batch{count: 2, first: t0, last: t0, consequence: true, nonConsequence: true})
	if mixed.Consequence {
		t.Errorf("mixed first batch: Consequence = true, want false")
	}
}
