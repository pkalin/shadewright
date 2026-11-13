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
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corerecord "k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// closureUID is the UID of the Incident the closure tests decide.
const closureUID = types.UID("11111111-1111-4111-8111-111111111111")

// closureIncident is occurrence 1 of fp(1) in phase, last seen seenAgo before triggerNow.
func closureIncident(phase remediationv1alpha1.IncidentPhase, seenAgo time.Duration) *remediationv1alpha1.Incident {
	st := incidentStatus(seenAgo, seenAgo, 0)
	st.Phase = phase
	return &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: Name(fp(1)), Namespace: fakeNamespace, UID: closureUID},
		Spec:       firstSpec(), Status: st,
	}
}

// request is a RemediationRequest named name, created createdAgo before triggerNow, naming the Incident of
// closureIncident, in phase.
func request(name string, createdAgo time.Duration, phase remediationv1alpha1.RemediationPhase) remediationv1alpha1.RemediationRequest {
	return remediationv1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fakeNamespace, CreationTimestamp: *ago(createdAgo)},
		Spec: remediationv1alpha1.RemediationRequestSpec{
			IncidentRef: remediationv1alpha1.IncidentRef{Name: Name(fp(1)), UID: remediationv1alpha1.UID(closureUID)},
		},
		Status: remediationv1alpha1.RemediationRequestStatus{Phase: phase},
	}
}

// mergedElsewhereRequest is request name in phase Superseded with PlanFresh False and reason
// AnotherRequestMerged: a duplicate the executor ended because another request was merged (#556).
func mergedElsewhereRequest(name string, createdAgo time.Duration) remediationv1alpha1.RemediationRequest {
	rr := request(name, createdAgo, remediationv1alpha1.PhaseSuperseded)
	rr.Status.Conditions = []metav1.Condition{{Type: remediationv1alpha1.ConditionPlanFresh,
		Status: metav1.ConditionFalse, Reason: remediationv1alpha1.ReasonAnotherRequestMerged}}
	return rr
}

func TestClosure(t *testing.T) {
	const (
		quiet = QuietBeforeResolved + time.Minute
		fresh = QuietBeforeResolved - time.Minute
	)
	resolved, escalated := remediationv1alpha1.IncidentResolved, remediationv1alpha1.IncidentEscalated
	proposed := func() *remediationv1alpha1.Incident {
		return closureIncident(remediationv1alpha1.IncidentProposed, time.Minute)
	}
	tests := []struct {
		name     string
		inc      func() *remediationv1alpha1.Incident
		requests []remediationv1alpha1.RemediationRequest
		held     time.Duration // how long ago the newest observation held unwritten was made; 0 for none
		phase    remediationv1alpha1.IncidentPhase
		reason   string
	}{
		{name: "open, quiet an hour", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentOpen, quiet)
		}, phase: resolved, reason: ReasonSymptomQuiet},
		{name: "open, seen within the hour", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentOpen, fresh)
		}},
		{name: "open, an observation held unwritten within the hour", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentOpen, quiet)
		}, held: fresh},
		{name: "open, a lasting symptom state is never quiet", inc: func() *remediationv1alpha1.Incident {
			inc := closureIncident(remediationv1alpha1.IncidentOpen, 3*time.Hour)
			inc.Status.Occurrences[0].ID = conditionID
			return inc
		}},
		{name: "proposed, quiet, request still active", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentProposed, quiet)
		}, requests: []remediationv1alpha1.RemediationRequest{request("a", 2*time.Hour, remediationv1alpha1.PhaseAwaitingApproval)}},
		{name: "proposed, quiet, a request with no phase yet is active", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentProposed, quiet)
		}, requests: []remediationv1alpha1.RemediationRequest{request("a", 2*time.Hour, "")}},
		{name: "proposed, quiet, merged counts as final until the verifier", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentProposed, quiet)
		}, requests: []remediationv1alpha1.RemediationRequest{request("a", 2*time.Hour, remediationv1alpha1.PhaseMerged)},
			phase: resolved, reason: ReasonSymptomQuiet},
		{name: "proposed, merged moves nothing by itself", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{request("a", 2*time.Hour, remediationv1alpha1.PhaseMerged)}},
		{name: "proposed, an older active request blocks the newest outcome", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{
				request("a", 2*time.Hour, remediationv1alpha1.PhaseAwaitingApproval),
				request("b", time.Hour, remediationv1alpha1.PhaseDenied),
			}},
		{name: "proposed, an older active request blocks the newest success", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{
				request("a", 2*time.Hour, remediationv1alpha1.PhasePRCreated),
				request("b", time.Hour, remediationv1alpha1.PhaseSucceeded),
			}},
		{name: "escalated by a superseded request, the replacement succeeded", inc: func() *remediationv1alpha1.Incident {
			inc := closureIncident(remediationv1alpha1.IncidentEscalated, time.Minute)
			applyClosure(inc, remediationv1alpha1.IncidentEscalated, ReasonRequestSuperseded, triggerNow.Add(-10*time.Minute))
			return inc
		}, requests: []remediationv1alpha1.RemediationRequest{
			request("a", 2*time.Hour, remediationv1alpha1.PhaseSuperseded),
			request("b", time.Hour, remediationv1alpha1.PhaseSucceeded),
		}, phase: resolved, reason: ReasonRequestSucceeded},
		{name: "escalated by a superseded request, still superseded", inc: func() *remediationv1alpha1.Incident {
			inc := closureIncident(remediationv1alpha1.IncidentEscalated, time.Minute)
			applyClosure(inc, remediationv1alpha1.IncidentEscalated, ReasonRequestSuperseded, triggerNow.Add(-10*time.Minute))
			return inc
		}, requests: []remediationv1alpha1.RemediationRequest{request("a", 2*time.Hour, remediationv1alpha1.PhaseSuperseded)}},
		{name: "proposed, request succeeded", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{request("a", time.Hour, remediationv1alpha1.PhaseSucceeded)},
			phase:    resolved, reason: ReasonRequestSucceeded},
		{name: "proposed, request denied", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{request("a", time.Hour, remediationv1alpha1.PhaseDenied)},
			phase:    escalated, reason: ReasonRequestDenied},
		{name: "proposed, request expired", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{request("a", time.Hour, remediationv1alpha1.PhaseExpired)},
			phase:    escalated, reason: ReasonRequestExpired},
		{name: "proposed, request failed", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{request("a", time.Hour, remediationv1alpha1.PhaseFailed)},
			phase:    escalated, reason: ReasonRequestFailed},
		{name: "proposed, request cancelled", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{request("a", time.Hour, remediationv1alpha1.PhaseCancelled)},
			phase:    escalated, reason: ReasonRequestCancelled},
		{name: "proposed, request needs intervention", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{request("a", time.Hour, remediationv1alpha1.PhaseNeedsIntervention)},
			phase:    escalated, reason: ReasonRequestNeedsIntervention},
		{name: "proposed, request inconclusive", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{request("a", time.Hour, remediationv1alpha1.PhaseInconclusive)},
			phase:    escalated, reason: ReasonRequestInconclusive},
		{name: "proposed, superseded with no newer request", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{request("a", time.Hour, remediationv1alpha1.PhaseSuperseded)},
			phase:    escalated, reason: ReasonRequestSuperseded},
		{name: "proposed, superseded follows the newer active request", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{
				request("a", 2*time.Hour, remediationv1alpha1.PhaseSuperseded),
				request("b", time.Hour, remediationv1alpha1.PhasePRCreated),
			}},
		{name: "proposed, superseded follows the newer request that succeeded", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{
				request("b", time.Hour, remediationv1alpha1.PhaseSucceeded),
				request("a", 2*time.Hour, remediationv1alpha1.PhaseSuperseded),
			},
			phase: resolved, reason: ReasonRequestSucceeded},
		// #556: a newer duplicate that ended Superseded because the older request was merged does not escalate:
		// the merged request decides (Merged moves nothing, Succeeded resolves).
		{name: "proposed, a duplicate superseded by a merge leaves the merged request to decide", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{
				request("a", 2*time.Hour, remediationv1alpha1.PhaseMerged),
				mergedElsewhereRequest("b", time.Hour),
			}},
		{name: "proposed, a duplicate superseded by a merge, the merged request succeeded", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{
				request("a", 2*time.Hour, remediationv1alpha1.PhaseSucceeded),
				mergedElsewhereRequest("b", time.Hour),
			},
			phase: resolved, reason: ReasonRequestSucceeded},
		{name: "proposed, a superseded request with another reason still escalates", inc: proposed,
			requests: func() []remediationv1alpha1.RemediationRequest {
				rr := mergedElsewhereRequest("b", time.Hour)
				rr.Status.Conditions[0].Reason = "TargetChanged"
				return []remediationv1alpha1.RemediationRequest{request("a", 2*time.Hour, remediationv1alpha1.PhaseMerged), rr}
			}(),
			phase: escalated, reason: ReasonRequestSuperseded},
		{name: "proposed, superseded at the same second is the older", inc: proposed,
			requests: []remediationv1alpha1.RemediationRequest{
				request("z", time.Hour, remediationv1alpha1.PhaseSuperseded),
				request("a", time.Hour, remediationv1alpha1.PhaseDenied),
			},
			phase: escalated, reason: ReasonRequestDenied},
		{name: "proposed, a request for another Incident of the same name is ignored", inc: proposed,
			requests: func() []remediationv1alpha1.RemediationRequest {
				rr := request("a", time.Hour, remediationv1alpha1.PhaseSucceeded)
				rr.Spec.IncidentRef.UID = "22222222-2222-4222-8222-222222222222"
				return []remediationv1alpha1.RemediationRequest{rr}
			}()},
		{name: "escalated, quiet an hour", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentEscalated, quiet)
		}, phase: resolved, reason: ReasonSymptomQuiet},
		{name: "escalated by a request within the hour stays, however long ago the symptom", inc: func() *remediationv1alpha1.Incident {
			inc := closureIncident(remediationv1alpha1.IncidentEscalated, 3*time.Hour)
			applyClosure(inc, remediationv1alpha1.IncidentEscalated, ReasonRequestDenied, triggerNow.Add(-fresh))
			return inc
		}, requests: []remediationv1alpha1.RemediationRequest{request("a", 2*time.Hour, remediationv1alpha1.PhaseDenied)}},
		{name: "escalated by an analysis within the hour stays", inc: func() *remediationv1alpha1.Incident {
			inc := closureIncident(remediationv1alpha1.IncidentEscalated, 3*time.Hour)
			inc.Status.LastAnalysisTime = ago(fresh)
			return inc
		}},
		{name: "escalated after a recurrence: an old success does not resolve it at once", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentEscalated, time.Minute)
		}, requests: []remediationv1alpha1.RemediationRequest{request("a", 30*time.Hour, remediationv1alpha1.PhaseSucceeded)}},
		{name: "escalated, quiet, a request still active", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentEscalated, quiet)
		}, requests: []remediationv1alpha1.RemediationRequest{request("a", 2*time.Hour, remediationv1alpha1.PhasePending)}},
		{name: "analyzing is never closed", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentAnalyzing, quiet)
		}},
		{name: "resolved stays", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentResolved, quiet)
		}},
		{name: "resolved, a request denied after resolvedTime escalates", inc: resolvedQuietly,
			requests: []remediationv1alpha1.RemediationRequest{request("a", 5*time.Minute, remediationv1alpha1.PhaseDenied)},
			phase:    escalated, reason: ReasonRequestDenied},
		{name: "resolved, a request created at resolvedTime counts", inc: resolvedQuietly,
			requests: []remediationv1alpha1.RemediationRequest{request("a", resolvedAgo, remediationv1alpha1.PhaseFailed)},
			phase:    escalated, reason: ReasonRequestFailed},
		{name: "resolved, superseded after resolvedTime with no newer request escalates", inc: resolvedQuietly,
			requests: []remediationv1alpha1.RemediationRequest{request("a", 5*time.Minute, remediationv1alpha1.PhaseSuperseded)},
			phase:    escalated, reason: ReasonRequestSuperseded},
		{name: "resolved, superseded follows the newer request that succeeded", inc: resolvedQuietly,
			requests: []remediationv1alpha1.RemediationRequest{
				request("a", 5*time.Minute, remediationv1alpha1.PhaseSuperseded),
				request("b", 4*time.Minute, remediationv1alpha1.PhaseSucceeded),
			}},
		{name: "resolved, a request that succeeded after resolvedTime leaves it resolved", inc: resolvedQuietly,
			requests: []remediationv1alpha1.RemediationRequest{request("a", 5*time.Minute, remediationv1alpha1.PhaseSucceeded)}},
		{name: "resolved, a request merged after resolvedTime moves nothing", inc: resolvedQuietly,
			requests: []remediationv1alpha1.RemediationRequest{request("a", 5*time.Minute, remediationv1alpha1.PhaseMerged)}},
		{name: "resolved, a request stamped seconds before resolvedTime (clock skew) escalates", inc: resolvedQuietly,
			requests: []remediationv1alpha1.RemediationRequest{request("a", resolvedAgo+2*time.Second, remediationv1alpha1.PhaseDenied)},
			phase:    escalated, reason: ReasonRequestDenied},
		{name: "resolved, a request denied before resolvedTime moves nothing", inc: resolvedQuietly,
			requests: []remediationv1alpha1.RemediationRequest{request("a", resolvedAgo+time.Minute, remediationv1alpha1.PhaseDenied)}},
		{name: "resolved, a newer request ended before resolvedTime moves nothing", inc: resolvedQuietly,
			requests: []remediationv1alpha1.RemediationRequest{
				request("a", 2*time.Hour, remediationv1alpha1.PhaseSuperseded),
				request("b", time.Hour, remediationv1alpha1.PhaseCancelled),
			}},
		{name: "resolved with no resolvedTime, a denied request moves nothing", inc: func() *remediationv1alpha1.Incident {
			return closureIncident(remediationv1alpha1.IncidentResolved, quiet)
		}, requests: []remediationv1alpha1.RemediationRequest{request("a", time.Minute, remediationv1alpha1.PhaseDenied)}},
		{name: "a foreign name is never closed", inc: func() *remediationv1alpha1.Incident {
			inc := closureIncident(remediationv1alpha1.IncidentOpen, quiet)
			inc.Name = Name(fp(2))
			return inc
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var held time.Time
			if tt.held > 0 {
				held = triggerNow.Add(-tt.held)
			}
			phase, reason, ok := Closure(tt.inc(), tt.requests, held, triggerNow)
			if ok != (tt.phase != "") || phase != tt.phase || reason != tt.reason {
				t.Fatalf("Closure = %q %q %v, want %q %q", phase, reason, ok, tt.phase, tt.reason)
			}
		})
	}
}

// resolvedAgo is how long before triggerNow the Incident of resolvedQuietly was resolved.
const resolvedAgo = 10 * time.Minute

// resolvedQuietly returns an Incident resolved by the quiet rule resolvedAgo before triggerNow, with its
// resolvedTime.
func resolvedQuietly() *remediationv1alpha1.Incident {
	inc := closureIncident(remediationv1alpha1.IncidentProposed, 2*time.Hour)
	applyClosure(inc, remediationv1alpha1.IncidentResolved, ReasonSymptomQuiet, triggerNow.Add(-resolvedAgo))
	return inc
}

// closureController is a controller on a fake API server holding inc and requests, loaded.
func closureController(t *testing.T, inc *remediationv1alpha1.Incident,
	requests ...remediationv1alpha1.RemediationRequest) (*Controller, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, 1+len(requests))
	objs = append(objs, inc)
	for i := range requests {
		objs = append(objs, &requests[i])
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&remediationv1alpha1.Incident{}, &remediationv1alpha1.RemediationRequest{}).
		WithObjects(objs...).Build()
	return loaded(t, cl), cl
}

// loaded is a new controller on cl, loaded, as after a restart.
func loaded(t *testing.T, cl client.Client) *Controller {
	t.Helper()
	c, err := New(Config{Namespace: fakeNamespace, Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	c.client, c.reader, c.cache = cl, cl, cl
	if err := c.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}

func getIncident(t *testing.T, cl client.Client) *remediationv1alpha1.Incident {
	t.Helper()
	got := &remediationv1alpha1.Incident{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: fakeNamespace, Name: Name(fp(1))}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// A quiet Incident moves to Resolved with resolvedTime and the Lifecycle condition in one write, and the
// controller remembers it Resolved, so a recurrence after the window opens the next occurrence.
func TestQuietIncidentIsResolvedWithItsTime(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentOpen, 2*time.Hour))
	w := &countingClient{Client: cl}
	c.client = w
	if err := c.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := getIncident(t, cl)
	at := triggerNow.Truncate(time.Second)
	if got.Status.Phase != remediationv1alpha1.IncidentResolved || got.Status.ResolvedTime == nil ||
		!got.Status.ResolvedTime.Equal(&metav1.Time{Time: at}) {
		t.Fatalf("phase %s resolvedTime %v, want Resolved at %s", got.Status.Phase, got.Status.ResolvedTime, at)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionLifecycle)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonSymptomQuiet {
		t.Fatalf("Lifecycle condition %+v", cond)
	}
	if n := w.updates.Load(); n != 1 {
		t.Fatalf("%d status writes, want 1", n)
	}
	c.mu.Lock()
	_, remembered := c.resolvedAt[Name(fp(1))]
	c.mu.Unlock()
	if !remembered {
		t.Fatal("the controller does not remember the Incident Resolved")
	}
	// Idempotent: a second pass writes nothing.
	if err := c.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := w.updates.Load(); n != 1 {
		t.Fatalf("%d status writes after a second pass, want 1", n)
	}
}

// A request that Succeeded resolves its Proposed Incident at once; one that was denied escalates it, and the
// escalation then stands for QuietBeforeResolved before the quiet rule may resolve it.
func TestRequestOutcomeMovesTheIncident(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) {
		c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentProposed, time.Minute),
			request("a", 10*time.Minute, remediationv1alpha1.PhaseSucceeded))
		if err := c.closeIncidents(t.Context()); err != nil {
			t.Fatal(err)
		}
		got := getIncident(t, cl)
		if got.Status.Phase != remediationv1alpha1.IncidentResolved || got.Status.ResolvedTime == nil {
			t.Fatalf("phase %s resolvedTime %v, want Resolved with a time", got.Status.Phase, got.Status.ResolvedTime)
		}
	})
	t.Run("denied", func(t *testing.T) {
		c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentProposed, 3*time.Hour),
			request("a", 2*time.Hour, remediationv1alpha1.PhaseDenied))
		if err := c.closeIncidents(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentEscalated || got.Status.ResolvedTime != nil {
			t.Fatalf("phase %s resolvedTime %v, want Escalated without one", got.Status.Phase, got.Status.ResolvedTime)
		}
		// The symptom has been gone for hours, but the escalation is new: the next pass leaves it.
		if err := c.closeIncidents(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentEscalated {
			t.Fatalf("phase %s right after the escalation, want Escalated", got.Status.Phase)
		}
	})
}

// The decision on the cache is only a candidate: the write decides again on the requests listed from the
// API server, so a request the cache has not seen yet keeps the Incident out of Resolved.
func TestLiveRequestsDecideTheWrite(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentProposed, 2*time.Hour))
	live := &extraRequests{Client: cl, extra: []remediationv1alpha1.RemediationRequest{
		request("a", time.Minute, remediationv1alpha1.PhaseAwaitingApproval)}}
	c.reader = live
	if err := c.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentProposed {
		t.Fatalf("phase %s, want Proposed while a live request is active", got.Status.Phase)
	}
}

// A restart in the middle of a move: the write landed but its answer was lost. The pass reports the error;
// the restarted controller finds the Incident Resolved and writes nothing, so resolvedTime stays.
func TestRestartAfterALostAnswerDoesNotMoveTwice(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentOpen, 2*time.Hour))
	lost := &countingClient{Client: cl, after: func(err error) error {
		if err != nil {
			return err
		}
		return context.DeadlineExceeded
	}}
	c.client = lost
	if err := c.closeIncidents(t.Context()); err == nil {
		t.Fatal("a lost answer was not reported")
	}
	first := getIncident(t, cl)
	if first.Status.Phase != remediationv1alpha1.IncidentResolved {
		t.Fatalf("setup: phase %s", first.Status.Phase)
	}
	restarted := loaded(t, cl)
	w := &countingClient{Client: cl}
	restarted.client = w
	restarted.cfg.Now = func() time.Time { return triggerNow.Add(time.Hour) }
	if err := restarted.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := w.updates.Load(); n != 0 {
		t.Fatalf("%d writes after the restart, want 0", n)
	}
	if got := getIncident(t, cl); !got.Status.ResolvedTime.Equal(first.Status.ResolvedTime) {
		t.Fatalf("resolvedTime moved from %v to %v", first.Status.ResolvedTime, got.Status.ResolvedTime)
	}
}

// A restart before the write was sent: nothing landed, and the restarted controller makes the move once.
func TestRestartBeforeTheWriteMovesOnce(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentProposed, time.Minute),
		request("a", resolvedAgo, remediationv1alpha1.PhaseFailed))
	c.client = &countingClient{Client: cl, instead: func(context.Context) error {
		return apierrors.NewServiceUnavailable("unavailable")
	}}
	if err := c.closeIncidents(t.Context()); err == nil {
		t.Fatal("a failed write was not reported")
	}
	if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentProposed {
		t.Fatalf("phase %s after a failed write, want Proposed", got.Status.Phase)
	}
	restarted := loaded(t, cl)
	w := &countingClient{Client: cl}
	restarted.client = w
	for range 2 {
		if err := restarted.closeIncidents(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentEscalated || w.updates.Load() != 1 {
		t.Fatalf("phase %s after %d writes, want Escalated after 1", got.Status.Phase, w.updates.Load())
	}
}

// Fail closed: when the requests cannot be listed, from the cache or from the API server, nothing moves and
// the pass reports the error.
func TestUnreadableRequestsKeepThePhase(t *testing.T) {
	for _, where := range []string{"cache", "live"} {
		t.Run(where, func(t *testing.T) {
			c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentProposed, 2*time.Hour),
				request("a", 3*time.Hour, remediationv1alpha1.PhaseSucceeded))
			failing := &failingRequests{Client: cl}
			if where == "cache" {
				c.cache = failing
			} else {
				c.reader = failing
			}
			w := &countingClient{Client: cl}
			c.client = w
			if err := c.closeIncidents(t.Context()); err == nil {
				t.Fatal("the failed read was not reported")
			}
			if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentProposed || w.updates.Load() != 0 {
				t.Fatalf("phase %s after %d writes, want Proposed and none", got.Status.Phase, w.updates.Load())
			}
		})
	}
}

// Findings of the review of #466: an observation in a write in flight keeps the Incident from going quiet. The
// closure pass runs while the observation writer has taken the batch out of pending and not stored it yet; it
// must not resolve the Incident, or the observation would count as seen before resolvedTime.
func TestInFlightObservationKeepsTheIncidentOpen(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentOpen, 2*time.Hour))
	w := &countingClient{Client: cl}
	c.client = w
	var closeErr error
	ran := false
	w.before = func(ctx context.Context, inc *remediationv1alpha1.Incident) {
		if ran {
			return
		}
		ran = true
		closeErr = c.closeIncidents(ctx)
	}
	c.consume(t.Context(), seenAt(7, triggerNow))
	c.flush(t.Context(), fp(1))
	if !ran || closeErr != nil {
		t.Fatalf("closure pass ran %v, error %v", ran, closeErr)
	}
	got := getIncident(t, cl)
	if got.Status.Phase != remediationv1alpha1.IncidentOpen || !got.Status.LastSeen.Time.Equal(triggerNow) {
		t.Fatalf("phase %s lastSeen %v, want Open and the new observation", got.Status.Phase, got.Status.LastSeen)
	}
	c.mu.Lock()
	left := len(c.inflight)
	c.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d writes still in flight after the flush", left)
	}
}

// Each attempt of a closure write lists the requests again: a replacement created while the first attempt
// conflicted keeps a Superseded chain from escalating.
func TestRetryListsTheRequestsAgain(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentProposed, time.Minute),
		request("a", 2*time.Hour, remediationv1alpha1.PhaseSuperseded))
	w := &countingClient{Client: cl}
	attempts := 0
	w.instead = func(ctx context.Context) error {
		attempts++
		if attempts > 1 {
			return nil
		}
		replacement := request("b", time.Minute, remediationv1alpha1.PhasePRCreated)
		if err := cl.Create(ctx, &replacement); err != nil {
			t.Fatal(err)
		}
		inc := getIncident(t, cl)
		inc.Status.Count++
		if err := cl.Status().Update(ctx, inc); err != nil {
			t.Fatal(err)
		}
		return apierrors.NewConflict(remediationv1alpha1.GroupVersion.WithResource("incidents").GroupResource(), inc.Name,
			errors.New("the object has been modified"))
	}
	c.client = w
	if err := c.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentProposed {
		t.Fatalf("phase %s after a replacement appeared during the retry, want Proposed", got.Status.Phase)
	}
}

// A write that fails does not count against the moves of a pass, so Incidents that keep failing never hold up
// the rest.
func TestFailedWritesDoNotStarveTheRest(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	n := closureWritesPerPass + 1
	objs := make([]client.Object, 0, n)
	for i := 1; i <= n; i++ {
		inc := closureIncident(remediationv1alpha1.IncidentOpen, 2*time.Hour)
		spec := specOf(observation(i, 1, 1, 1, triggerNow))
		inc.Name, inc.Spec, inc.UID = Name(fp(i)), spec, types.UID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
		objs = append(objs, inc)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&remediationv1alpha1.Incident{}).
		WithObjects(objs...).Build()
	c := loaded(t, cl)
	healthy := Name(fp(n))
	w := &countingClient{Client: cl}
	var current string
	w.before = func(_ context.Context, inc *remediationv1alpha1.Incident) { current = inc.Name }
	w.instead = func(context.Context) error {
		if current != healthy {
			return apierrors.NewServiceUnavailable("unavailable")
		}
		return nil
	}
	c.client = w
	if err := c.closeIncidents(t.Context()); err == nil {
		t.Fatal("the failed writes were not reported")
	}
	got := &remediationv1alpha1.Incident{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: fakeNamespace, Name: healthy}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != remediationv1alpha1.IncidentResolved {
		t.Fatalf("the Incident that could be written has phase %s, want Resolved", got.Status.Phase)
	}
	if len(c.closeFailed) != n-1 {
		t.Fatalf("%d Incidents remembered as failed, want %d", len(c.closeFailed), n-1)
	}
}

// A closure step that runs into its deadline leaves the due marks their own time in the pass.
func TestStalledClosureLeavesTheMarksTheirTime(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentOpen, 2*time.Hour))
	c.cfg.PassTimeout, c.cfg.ReadTimeout = 200*time.Millisecond, 50*time.Millisecond
	lists := &countingLists{Client: cl}
	c.cache = lists
	c.reader = &stalling{Client: cl}
	_ = c.pass(t.Context())
	if got := lists.incidents.Load(); got != 2 {
		t.Fatalf("the Incidents were listed %d times in the pass, want 2 (closure and marks)", got)
	}
}

// The cap gauges come from memory, so a pass whose listings fail still sets them.
func TestCapGaugeIsSetWhenTheListingFails(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentOpen, time.Minute))
	c.cfg.MaxIncidents = 7
	c.cache = &failingIncidents{Client: cl}
	incidentCap.Set(0)
	if err := c.pass(t.Context()); err == nil {
		t.Fatal("the failed listing was not reported")
	}
	if v := testutil.ToFloat64(incidentCap); v != 7 {
		t.Fatalf("shadewright_incident_cap = %v, want 7", v)
	}
}

// Decision D2 of #466: an Incident resolved by quiet time before any analysis reopens on a recurrence within
// the window, with no resolvedTime; one that was analyzed, or resolved otherwise, is escalated.
func TestRecurrenceOfAQuietResolution(t *testing.T) {
	resolvedQuiet := func(mut func(*remediationv1alpha1.Incident)) *remediationv1alpha1.Incident {
		inc := closureIncident(remediationv1alpha1.IncidentOpen, 3*time.Hour)
		applyClosure(inc, remediationv1alpha1.IncidentResolved, ReasonSymptomQuiet, triggerNow.Add(-time.Hour))
		if mut != nil {
			mut(inc)
		}
		return inc
	}
	tests := []struct {
		name  string
		inc   *remediationv1alpha1.Incident
		phase remediationv1alpha1.IncidentPhase
	}{
		{"never analyzed reopens", resolvedQuiet(nil), remediationv1alpha1.IncidentOpen},
		{"analyzed escalates", resolvedQuiet(func(inc *remediationv1alpha1.Incident) {
			inc.Status.LastAnalysisTime = ago(2 * time.Hour)
		}), remediationv1alpha1.IncidentEscalated},
		{"with a request escalates", resolvedQuiet(func(inc *remediationv1alpha1.Incident) {
			inc.Status.RemediationRequests = []remediationv1alpha1.Name{"a"}
		}), remediationv1alpha1.IncidentEscalated},
		{"resolved by a success escalates", resolvedQuiet(func(inc *remediationv1alpha1.Incident) {
			applyClosure(inc, remediationv1alpha1.IncidentResolved, ReasonRequestSucceeded, triggerNow.Add(-time.Hour))
		}), remediationv1alpha1.IncidentEscalated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cl := closureController(t, tt.inc)
			c.consume(t.Context(), seenAt(9, triggerNow))
			c.flush(t.Context(), fp(1))
			got := getIncident(t, cl)
			if got.Status.Phase != tt.phase {
				t.Fatalf("phase %s, want %s", got.Status.Phase, tt.phase)
			}
			reopened := tt.phase == remediationv1alpha1.IncidentOpen
			if reopened != (got.Status.ResolvedTime == nil) {
				t.Fatalf("resolvedTime %v after phase %s", got.Status.ResolvedTime, got.Status.Phase)
			}
			if recurred := meta.FindStatusCondition(got.Status.Conditions, ConditionRecurred) != nil; recurred == reopened {
				t.Fatalf("condition Recurred set %v for phase %s", recurred, got.Status.Phase)
			}
			if reopened && lifecycleReason(got) != ReasonReopened {
				t.Fatalf("Lifecycle reason %q, want %s", lifecycleReason(got), ReasonReopened)
			}
		})
	}
}

// A pass whose context ends leaves the Incident as it was.
func TestCancelledPassKeepsThePhase(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentOpen, 2*time.Hour))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.reader = &cancelled{Client: cl}
	if err := c.closeIncidents(ctx); err == nil {
		t.Fatal("a cancelled pass reported no error")
	}
	if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentOpen {
		t.Fatalf("phase %s, want Open", got.Status.Phase)
	}
}

// At 80% of the cap the trigger pass sets the gauge and writes one Warning event IncidentCapNear about the
// Pod; below it, the gauge goes back to 0 and a later crossing warns again.
func TestCapNearWarnsOncePerCrossing(t *testing.T) {
	rec := corerecord.NewFakeRecorder(10)
	c, err := New(Config{Namespace: fakeNamespace, MaxIncidents: 10, Recorder: rec, EventInterval: time.Nanosecond,
		Regarding: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: fakeNamespace, Name: "analyzer-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	set := func(n int) {
		c.mu.Lock()
		clear(c.names)
		for i := range n {
			c.names[Name(fp(i))] = struct{}{}
		}
		c.mu.Unlock()
		c.reportCap(t.Context())
	}
	set(7)
	if v := testutil.ToFloat64(incidentCapNear); v != 0 || testutil.ToFloat64(incidentCap) != 10 || len(rec.Events) != 0 {
		t.Fatalf("below the threshold: near %v cap %v events %d", v, testutil.ToFloat64(incidentCap), len(rec.Events))
	}
	set(8)
	set(9)
	if v := testutil.ToFloat64(incidentCapNear); v != 1 || len(rec.Events) != 1 {
		t.Fatalf("at the threshold: near %v events %d, want 1 and 1", v, len(rec.Events))
	}
	if e := <-rec.Events; !strings.Contains(e, "Warning IncidentCapNear 8 Incidents of the cap of 10") {
		t.Fatalf("event %q", e)
	}
	set(7)
	set(8)
	if v := testutil.ToFloat64(incidentCapNear); v != 1 || len(rec.Events) != 1 {
		t.Fatalf("second crossing: near %v events %d, want 1 and 1", v, len(rec.Events))
	}
}

func TestCapNearThreshold(t *testing.T) {
	for maxIncidents, want := range map[int]int{500: 400, 10: 8, 3: 3, 1: 1} {
		if got := capNearThreshold(maxIncidents); got != want {
			t.Errorf("capNearThreshold(%d) = %d, want %d", maxIncidents, got, want)
		}
	}
}

// extraRequests adds extra to every listing of RemediationRequests.
type extraRequests struct {
	client.Client
	extra []remediationv1alpha1.RemediationRequest
}

func (e *extraRequests) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := e.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if l, ok := list.(*remediationv1alpha1.RemediationRequestList); ok {
		l.Items = append(l.Items, e.extra...)
	}
	return nil
}

// failingRequests fails every listing of RemediationRequests.
type failingRequests struct{ client.Client }

func (f *failingRequests) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*remediationv1alpha1.RemediationRequestList); ok {
		return errors.New("the API server is unavailable")
	}
	return f.Client.List(ctx, list, opts...)
}

// countingLists counts the listings of Incidents.
type countingLists struct {
	client.Client
	incidents atomic.Int32
}

func (c *countingLists) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*remediationv1alpha1.IncidentList); ok {
		c.incidents.Add(1)
	}
	return c.Client.List(ctx, list, opts...)
}

// stalling blocks every Get until its context ends, as an API server that does not answer.
type stalling struct{ client.Client }

func (s *stalling) Get(ctx context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	<-ctx.Done()
	return ctx.Err()
}

// failingIncidents fails every listing of Incidents.
type failingIncidents struct{ client.Client }

func (f *failingIncidents) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*remediationv1alpha1.IncidentList); ok {
		return errors.New("the cache is unavailable")
	}
	return f.Client.List(ctx, list, opts...)
}

// cancelled answers every call with the context's error, as a client does once its context ended.
type cancelled struct{ client.Client }

func (c *cancelled) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *cancelled) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// Review of #466: unwrittenSince reads the held and the in flight batches under one lock, so a batch that
// moves between them (a failed flush putting it back) is never missed by both reads.
func TestUnwrittenSinceSeesABatchMovingBetweenHeldAndInFlight(t *testing.T) {
	c, _ := closureController(t, closureIncident(remediationv1alpha1.IncidentOpen, 2*time.Hour))
	fingerprint := fp(1)
	c.inflight[fingerprint] = &flight{n: 1, last: triggerNow}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.mu.Lock()
			delete(c.inflight, fingerprint)
			c.pending[fingerprint] = &batch{last: triggerNow}
			c.mu.Unlock()
			c.mu.Lock()
			delete(c.pending, fingerprint)
			c.inflight[fingerprint] = &flight{n: 1, last: triggerNow}
			c.mu.Unlock()
		}
	})
	defer func() { close(stop); wg.Wait() }()
	for i := range 100000 {
		if c.unwrittenSince(fingerprint).IsZero() {
			t.Fatalf("unwrittenSince returned zero at iteration %d although an observation was always held or in flight", i)
		}
	}
}

// Review of #466: an observation consumed after the closure rules decided, before their write, is seen within
// the quiet hour before resolvedTime, so it contradicts the quiet resolution: a recurrence (here a reopen,
// never analyzed), not an observation from before the resolution.
func TestObservationAfterTheClosureDecisionIsARecurrence(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentOpen, 2*time.Hour))
	w := &countingClient{Client: cl}
	w.before = func(_ context.Context, _ *remediationv1alpha1.Incident) {
		w.before = nil
		c.consume(t.Context(), seenAt(99, triggerNow))
	}
	c.client = w
	if err := c.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentResolved {
		t.Fatalf("phase %s after the closure pass, want Resolved", got.Status.Phase)
	}
	c.flush(t.Context(), fp(1))
	got := getIncident(t, cl)
	if got.Status.Phase != remediationv1alpha1.IncidentOpen || lifecycleReason(got) != ReasonReopened {
		t.Fatalf("observation seen at the resolution: phase %s, Lifecycle %q, want Open, Reopened",
			got.Status.Phase, lifecycleReason(got))
	}
}

// Review of #466: only an observation seen more than QuietBeforeResolved before a quiet resolvedTime counts as
// from before it; a request's success counts up to resolvedTime.
func TestBeforeAQuietResolutionMeansBeforeTheQuietHour(t *testing.T) {
	resolvedAt := triggerNow.Add(-10 * time.Minute)
	tests := []struct {
		name   string
		reason string
		seen   time.Time
		phase  remediationv1alpha1.IncidentPhase
	}{
		{"quiet, seen in the quiet hour", ReasonSymptomQuiet, resolvedAt.Add(-30 * time.Minute), remediationv1alpha1.IncidentOpen},
		{"quiet, seen before the quiet hour", ReasonSymptomQuiet, resolvedAt.Add(-QuietBeforeResolved - time.Second), remediationv1alpha1.IncidentResolved},
		{"success, seen before resolvedTime", ReasonRequestSucceeded, resolvedAt.Add(-30 * time.Minute), remediationv1alpha1.IncidentResolved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inc := closureIncident(remediationv1alpha1.IncidentOpen, 3*time.Hour)
			applyClosure(inc, remediationv1alpha1.IncidentResolved, tt.reason, resolvedAt)
			c, cl := closureController(t, inc)
			c.consume(t.Context(), seenAt(9, tt.seen))
			c.flush(t.Context(), fp(1))
			if got := getIncident(t, cl); got.Status.Phase != tt.phase {
				t.Fatalf("phase %s, want %s", got.Status.Phase, tt.phase)
			}
		})
	}
}

// Review of #466: a request created after the closure rules listed the requests does not stop the resolve
// (the listing is no lock); the next pass, from what the API server holds, moves the Incident back to
// Proposed with no resolvedTime.
func TestActiveRequestCreatedAfterTheListingUndoesTheResolve(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentProposed, 2*time.Hour))
	w := &countingClient{Client: cl}
	w.before = func(ctx context.Context, _ *remediationv1alpha1.Incident) {
		w.before = nil
		rr := request("replacement", time.Minute, remediationv1alpha1.PhaseAwaitingApproval)
		if err := cl.Create(ctx, &rr); err != nil {
			t.Fatal(err)
		}
	}
	c.client = w
	if err := c.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentResolved {
		t.Fatalf("phase %s after the racing pass, want Resolved", got.Status.Phase)
	}
	if err := c.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := getIncident(t, cl)
	if got.Status.Phase != remediationv1alpha1.IncidentProposed || got.Status.ResolvedTime != nil ||
		lifecycleReason(got) != ReasonRequestActive {
		t.Fatalf("active request names the Incident: phase %s, resolvedTime %v, Lifecycle %q, want Proposed, none, %s",
			got.Status.Phase, got.Status.ResolvedTime, lifecycleReason(got), ReasonRequestActive)
	}
}

// Review of #466: an observation that reaches a quietly Resolved Incident never analyzed more than
// RecurrenceWindow after resolvedTime (held with older ones, so it was not sent to the next occurrence) is
// escalated as any such recurrence, never reopened.
func TestReopenOnlyWithinTheWindow(t *testing.T) {
	inc := closureIncident(remediationv1alpha1.IncidentOpen, 50*time.Hour)
	applyClosure(inc, remediationv1alpha1.IncidentResolved, ReasonSymptomQuiet, triggerNow.Add(-25*time.Hour))
	c, cl := closureController(t, inc)
	c.consume(t.Context(), seenAt(98, triggerNow.Add(-26*time.Hour)))
	c.consume(t.Context(), seenAt(99, triggerNow))
	c.flush(t.Context(), fp(1))
	if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentEscalated {
		t.Fatalf("recurrence 25 h after a quiet resolution: phase %s, want Escalated", got.Status.Phase)
	}
}

// #476: a request created after the closure rules listed the requests, and denied before the next pass,
// still decides the Incident the quiet rule resolved meanwhile: the next pass, also after a restart, moves it
// to Escalated.
func TestRequestEndedBeforeTheNextPassEscalatesTheResolve(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentProposed, 2*time.Hour))
	w := &countingClient{Client: cl}
	rr := request("replacement", 0, remediationv1alpha1.PhaseAwaitingApproval)
	w.before = func(ctx context.Context, _ *remediationv1alpha1.Incident) {
		w.before = nil
		if err := cl.Create(ctx, &rr); err != nil {
			t.Fatal(err)
		}
	}
	c.client = w
	if err := c.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getIncident(t, cl); got.Status.Phase != remediationv1alpha1.IncidentResolved {
		t.Fatalf("racing closure: phase %s, want Resolved", got.Status.Phase)
	}
	rr.Status.Phase = remediationv1alpha1.PhaseDenied
	if err := cl.Status().Update(t.Context(), &rr); err != nil {
		t.Fatal(err)
	}
	c = loaded(t, cl)
	if err := c.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := getIncident(t, cl)
	if got.Status.Phase != remediationv1alpha1.IncidentEscalated || got.Status.ResolvedTime != nil ||
		lifecycleReason(got) != ReasonRequestDenied {
		t.Fatalf("replacement denied before the next pass: phase %s, resolvedTime %v, Lifecycle %q; want Escalated, none, %s",
			got.Status.Phase, got.Status.ResolvedTime, lifecycleReason(got), ReasonRequestDenied)
	}
}

// #476: a request that ended before the Incident was resolved neither reopens nor escalates it, whatever
// its phase.
func TestRequestEndedBeforeResolvedTimeKeepsTheResolve(t *testing.T) {
	for _, phase := range []remediationv1alpha1.RemediationPhase{
		remediationv1alpha1.PhaseDenied, remediationv1alpha1.PhaseSuperseded, remediationv1alpha1.PhaseSucceeded,
		remediationv1alpha1.PhaseNeedsIntervention, remediationv1alpha1.PhaseMerged,
	} {
		t.Run(string(phase), func(t *testing.T) {
			inc := resolvedQuietly()
			c, cl := closureController(t, inc, request("a", resolvedAgo+time.Minute, phase))
			if err := c.closeIncidents(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := getIncident(t, cl)
			if got.Status.Phase != remediationv1alpha1.IncidentResolved || got.Status.ResolvedTime == nil ||
				lifecycleReason(got) != ReasonSymptomQuiet {
				t.Fatalf("request %s before resolvedTime: phase %s, resolvedTime %v, Lifecycle %q; want Resolved as it was",
					phase, got.Status.Phase, got.Status.ResolvedTime, lifecycleReason(got))
			}
		})
	}
}
