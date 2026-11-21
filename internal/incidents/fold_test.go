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
	"slices"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The tests of the late-cause fold (ADR-0039 points 12 to 15 and 23).

const (
	foldConsequenceUID = types.UID("61616161-6161-4161-8161-616161616161")
	foldCauseUID       = types.UID("62626262-6262-4262-8262-626262626262")
	// reasonExitError is the reason of a termination with a non-zero exit code read from pod status.
	reasonExitError = "Error"
	// foldContainer is the container the cause and the consequence of the fold tests name.
	foldContainer = "web"
)

// foldCause is the OOM cause Incident of fp(1), container web, created at triggerNow and first seen an hour
// before it (a cause held by the gate and opened from a recurrence).
func foldCause() *remediationv1alpha1.Incident {
	o := oomCause(1, triggerNow.Add(-time.Hour))
	st := incidentStatus(time.Hour, time.Hour, 0)
	st.Cause, st.Containers = true, []remediationv1alpha1.Name{foldContainer}
	return &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: Name(fp(1)), Namespace: fakeNamespace, UID: foldCauseUID,
			CreationTimestamp: metav1.NewTime(triggerNow)},
		Spec: specOf(o), Status: st,
	}
}

// foldConsequence is a kubelet BackOff Incident of fp(2), container web, of the cause's subject, in phase
// Open, created createdBefore the cause.
func foldConsequence(createdBefore time.Duration) *remediationv1alpha1.Incident {
	o := consequenceOf(2, 0, foldContainer, reasonBackOff, 1, 2, triggerNow.Add(-createdBefore))
	st := incidentStatus(createdBefore, createdBefore, 0)
	st.Containers = []remediationv1alpha1.Name{foldContainer}
	return &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: Name(fp(2)), Namespace: fakeNamespace, UID: foldConsequenceUID,
			CreationTimestamp: metav1.NewTime(triggerNow.Add(-createdBefore))},
		Spec: specOf(o), Status: st,
	}
}

// foldRequest is a request named name of the consequence Incident in phase, with PullRequestOpened True when
// opened.
func foldRequest(name string, phase remediationv1alpha1.RemediationPhase, opened bool) remediationv1alpha1.RemediationRequest {
	rr := request(name, time.Minute, phase)
	rr.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: Name(fp(2)), UID: remediationv1alpha1.UID(foldConsequenceUID)}
	if opened {
		rr.Status.Conditions = []metav1.Condition{{Type: remediationv1alpha1.ConditionPROpened,
			Status: metav1.ConditionTrue, Reason: "Opened", LastTransitionTime: *ago(time.Minute)}}
	}
	return rr
}

// consequenceProposed moves the consequence to phase Proposed, in which its newest request decides it.
func consequenceProposed(c, _ *remediationv1alpha1.Incident) {
	c.Status.Phase = remediationv1alpha1.IncidentProposed
}

// TestFoldable decides the fold for one change at a time from a consequence Incident that folds.
func TestFoldable(t *testing.T) {
	for name, tc := range map[string]struct {
		change   func(cons, cause *remediationv1alpha1.Incident)
		requests []remediationv1alpha1.RemediationRequest
		want     bool
	}{
		"folds": {want: true},
		"created exactly 15 minutes before": {change: func(c, _ *remediationv1alpha1.Incident) {
			c.CreationTimestamp = metav1.NewTime(triggerNow.Add(-RecurWithin))
		}, want: true},
		"created more than 15 minutes before": {change: func(c, _ *remediationv1alpha1.Incident) {
			c.CreationTimestamp = metav1.NewTime(triggerNow.Add(-RecurWithin - time.Second))
		}},
		"created at the same second": {change: func(c, _ *remediationv1alpha1.Incident) { c.CreationTimestamp = metav1.NewTime(triggerNow) }, want: true},
		"created after the cause": {change: func(c, _ *remediationv1alpha1.Incident) {
			c.CreationTimestamp = metav1.NewTime(triggerNow.Add(time.Second))
		}},
		"first seen long before, created within": {change: func(c, cause *remediationv1alpha1.Incident) {
			// firstSeen and lastSeen come from other clocks and are never compared: only creation counts.
			c.Status.FirstSeen, c.Status.LastSeen = ago(3*time.Hour), ago(3*time.Hour)
			cause.Status.FirstSeen = ago(5 * time.Hour)
		}, want: true},
		"Unhealthy": {change: func(c, _ *remediationv1alpha1.Incident) { c.Spec.Reason = reasonUnhealthy }, want: true},
		"BackOff, cause a non-zero exit": {change: func(_, cause *remediationv1alpha1.Incident) {
			cause.Spec.Reason = reasonExitError
		}, want: true},
		"Unhealthy, cause a non-zero exit": {change: func(c, cause *remediationv1alpha1.Incident) {
			c.Spec.Reason, cause.Spec.Reason = reasonUnhealthy, reasonExitError
		}},
		"Unhealthy, cause a wait": {change: func(c, cause *remediationv1alpha1.Incident) {
			c.Spec.Reason, cause.Spec.Reason = reasonUnhealthy, "ImagePullBackOff"
		}, want: true},
		"phase Analyzing": {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Phase = remediationv1alpha1.IncidentAnalyzing }, want: true},
		"phase Proposed":  {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Phase = remediationv1alpha1.IncidentProposed }, want: true},
		"no phase":        {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Phase = "" }, want: true},
		"phase Escalated": {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Phase = remediationv1alpha1.IncidentEscalated }},
		"folded before, reopened and analysed": {change: func(c, _ *remediationv1alpha1.Incident) {
			c.Status.Folded, c.Status.Phase = true, remediationv1alpha1.IncidentProposed
		}},
		"reopened, not folded before": {change: func(c, _ *remediationv1alpha1.Incident) {
			c.Status.Conditions = []metav1.Condition{{Type: ConditionLifecycle, Status: metav1.ConditionTrue,
				Reason: ReasonReopened, LastTransitionTime: *ago(time.Minute)}}
		}, want: true},
		"phase Resolved": {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Phase = remediationv1alpha1.IncidentResolved }},
		"already folded": {change: func(c, _ *remediationv1alpha1.Incident) {
			c.Status.FoldedInto = &remediationv1alpha1.IncidentRef{Name: "x", UID: "y"}
		}},
		"non-zero exit termination":    {change: func(c, _ *remediationv1alpha1.Incident) { c.Spec.Reason = reasonExitError }},
		"Failed Event":                 {change: func(c, _ *remediationv1alpha1.Incident) { c.Spec.Reason = "Failed" }},
		"OOMKilled":                    {change: func(c, _ *remediationv1alpha1.Incident) { c.Spec.Reason = reasonOOM }},
		"BackOff not from the kubelet": {change: func(c, _ *remediationv1alpha1.Incident) { c.Spec.ReportingController = "forged" }},
		"consequence that is a cause":  {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Cause = true }},
		"another container":            {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Containers = []remediationv1alpha1.Name{"db"} }},
		"one container shared": {change: func(c, cause *remediationv1alpha1.Incident) {
			c.Status.Containers = []remediationv1alpha1.Name{"db", "web"}
			cause.Status.Containers = []remediationv1alpha1.Name{"sidecar", "web"}
		}, want: true},
		"no container":        {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Containers = nil }},
		"another subject":     {change: func(c, _ *remediationv1alpha1.Incident) { c.Spec.Subject.UID = otherSubjectUID }},
		"cause without cause": {change: func(_, cause *remediationv1alpha1.Incident) { cause.Status.Cause = false }},
		"itself by name":      {change: func(c, cause *remediationv1alpha1.Incident) { cause.Name = c.Name }},
		"itself by UID":       {change: func(c, cause *remediationv1alpha1.Incident) { cause.UID = c.UID }},
		"name not derived":    {change: func(c, _ *remediationv1alpha1.Incident) { c.Name = "hand-made" }},
		"request with a pull request": {requests: []remediationv1alpha1.RemediationRequest{
			foldRequest("a", remediationv1alpha1.PhasePolicyChecked, true)}},
		"request in a phase after the pull request": {requests: []remediationv1alpha1.RemediationRequest{
			foldRequest("a", remediationv1alpha1.PhaseAwaitingApproval, false)}},
		"request merged and verified": {requests: []remediationv1alpha1.RemediationRequest{
			foldRequest("a", remediationv1alpha1.PhaseSucceeded, false)}},
		"request with a recorded pull request": {requests: func() []remediationv1alpha1.RemediationRequest {
			rr := foldRequest("a", remediationv1alpha1.PhaseFailed, false)
			rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{}
			return []remediationv1alpha1.RemediationRequest{rr}
		}()},
		"request without a pull request, an older one denied": {requests: []remediationv1alpha1.RemediationRequest{
			foldRequest("a", remediationv1alpha1.PhaseDenied, false),
			foldRequest("b", remediationv1alpha1.PhasePolicyChecked, false)}, want: true},
		"newest request denied": {change: consequenceProposed, requests: []remediationv1alpha1.RemediationRequest{
			foldRequest("a", remediationv1alpha1.PhasePolicyChecked, false),
			foldRequest("b", remediationv1alpha1.PhaseDenied, false)}},
		"newest request failed": {change: consequenceProposed, requests: []remediationv1alpha1.RemediationRequest{
			foldRequest("a", remediationv1alpha1.PhaseFailed, false)}},
		"newest request needs intervention": {change: consequenceProposed, requests: []remediationv1alpha1.RemediationRequest{
			foldRequest("a", remediationv1alpha1.PhaseNeedsIntervention, false)}},
		"newest request superseded by another one's merge": {change: consequenceProposed, requests: func() []remediationv1alpha1.RemediationRequest {
			// A request ended by another one's merge never decides: the active one does.
			rr := foldRequest("b", remediationv1alpha1.PhaseSuperseded, false)
			rr.Status.Conditions = []metav1.Condition{{Type: remediationv1alpha1.ConditionPlanFresh,
				Status: metav1.ConditionFalse, Reason: remediationv1alpha1.ReasonAnotherRequestMerged}}
			return []remediationv1alpha1.RemediationRequest{foldRequest("a", remediationv1alpha1.PhasePolicyChecked, false), rr}
		}(), want: true},
		"pull request of another Incident of the same name": {requests: func() []remediationv1alpha1.RemediationRequest {
			rr := foldRequest("a", remediationv1alpha1.PhasePRCreated, true)
			rr.Spec.IncidentRef.UID = "63636363-6363-4363-8363-636363636363"
			return []remediationv1alpha1.RemediationRequest{rr}
		}(), want: true},
	} {
		t.Run(name, func(t *testing.T) {
			cons, cause := foldConsequence(5*time.Minute), foldCause()
			if tc.change != nil {
				tc.change(cons, cause)
			}
			if got := foldable(cons, cause, tc.requests); got != tc.want {
				t.Fatalf("foldable %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFoldRefusesItself: the schema cannot tell that foldedInto names the Incident itself, so fold refuses it
// and leaves the Incident as it was.
func TestFoldRefusesItself(t *testing.T) {
	for name, cause := range map[string]func(*remediationv1alpha1.Incident) *remediationv1alpha1.Incident{
		"same object": func(inc *remediationv1alpha1.Incident) *remediationv1alpha1.Incident { return inc.DeepCopy() },
		"same name": func(inc *remediationv1alpha1.Incident) *remediationv1alpha1.Incident {
			c := foldCause()
			c.Name = inc.Name
			return c
		},
		"same UID": func(inc *remediationv1alpha1.Incident) *remediationv1alpha1.Incident {
			c := foldCause()
			c.UID = inc.UID
			return c
		},
	} {
		t.Run(name, func(t *testing.T) {
			inc := foldConsequence(time.Minute)
			before := inc.DeepCopy()
			if err := fold(inc, cause(inc), triggerNow); !errors.Is(err, errSelfFold) {
				t.Fatalf("fold into itself: %v, want errSelfFold", err)
			}
			if inc.Status.Phase != before.Status.Phase || inc.Status.FoldedInto != nil || inc.Status.Folded {
				t.Fatalf("status changed by a refused fold: %+v", inc.Status)
			}
		})
	}
}

// foldController is a controller on a fake API server holding the cause, the Incidents incs and requests,
// loaded, its writes counted.
func foldController(t *testing.T, incs []*remediationv1alpha1.Incident,
	requests ...remediationv1alpha1.RemediationRequest) (*Controller, client.Client, *countingClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, 1+len(incs)+len(requests))
	objs = append(objs, foldCause())
	for _, inc := range incs {
		objs = append(objs, inc)
	}
	for i := range requests {
		objs = append(objs, &requests[i])
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&remediationv1alpha1.Incident{}, &remediationv1alpha1.RemediationRequest{}).
		WithObjects(objs...).Build()
	c := loaded(t, cl)
	w := &countingClient{Client: cl}
	c.client = w
	return c, cl, w
}

func getNamed(t *testing.T, cl client.Client, name string) *remediationv1alpha1.Incident {
	t.Helper()
	got := &remediationv1alpha1.Incident{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: fakeNamespace, Name: name}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// checkFolded fails unless inc is folded into the cause at triggerNow.
func checkFolded(t *testing.T, inc *remediationv1alpha1.Incident) {
	t.Helper()
	cond := meta.FindStatusCondition(inc.Status.Conditions, ConditionLifecycle)
	at := metav1.NewTime(triggerNow.Truncate(time.Second))
	if inc.Status.Phase != remediationv1alpha1.IncidentResolved || inc.Status.ResolvedTime == nil ||
		!inc.Status.ResolvedTime.Equal(&at) || cond == nil || cond.Reason != ReasonFoldedIntoCause ||
		cond.Message != lifecycleMessages[ReasonFoldedIntoCause] || !inc.Status.Folded || inc.Status.FoldedInto == nil ||
		inc.Status.FoldedInto.Name != Name(fp(1)) || types.UID(inc.Status.FoldedInto.UID) != foldCauseUID {
		t.Fatalf("phase %s resolvedTime %v lifecycle %+v folded %v foldedInto %+v, want folded into %s",
			inc.Status.Phase, inc.Status.ResolvedTime, cond, inc.Status.Folded, inc.Status.FoldedInto, Name(fp(1)))
	}
}

// TestFoldWritesOnce: the fold is one status write of the lifecycle field manager, and a second pass writes
// nothing.
func TestFoldWritesOnce(t *testing.T) {
	c, cl, w := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
	if err := c.foldInto(t.Context(), Name(fp(1))); err != nil {
		t.Fatal(err)
	}
	checkFolded(t, getNamed(t, cl, Name(fp(2))))
	if n := w.updates.Load(); n != 1 {
		t.Fatalf("%d status writes, want 1", n)
	}
	if err := c.foldInto(t.Context(), Name(fp(1))); err != nil {
		t.Fatal(err)
	}
	if n := w.updates.Load(); n != 1 {
		t.Fatalf("%d status writes after a second pass, want 1", n)
	}
	c.mu.Lock()
	_, remembered := c.resolvedAt[Name(fp(2))]
	c.mu.Unlock()
	if !remembered {
		t.Fatal("the controller does not remember the folded Incident Resolved")
	}
}

// TestFoldKeepsAPullRequestsIncident: a consequence Incident one of whose requests has a pull request is not
// folded and keeps its request.
func TestFoldKeepsAPullRequestsIncident(t *testing.T) {
	c, cl, w := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)},
		foldRequest("a", remediationv1alpha1.PhaseAwaitingApproval, true))
	if err := c.foldInto(t.Context(), Name(fp(1))); err != nil {
		t.Fatal(err)
	}
	if got := getNamed(t, cl, Name(fp(2))); got.Status.Phase != remediationv1alpha1.IncidentOpen || got.Status.FoldedInto != nil {
		t.Fatalf("phase %s foldedInto %+v, want Open and not folded", got.Status.Phase, got.Status.FoldedInto)
	}
	rr := &remediationv1alpha1.RemediationRequest{}
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: fakeNamespace, Name: "a"}, rr); err != nil ||
		rr.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval || rr.Spec.Cancel {
		t.Fatalf("request %+v (%v), want it kept as it was", rr.Status, err)
	}
	if n := w.updates.Load(); n != 0 {
		t.Fatalf("%d status writes, want none", n)
	}
}

// TestFoldLeavesWhatIsNotItsConsequence: of the Incidents of the cause's subject, only the consequence of the
// same container created within 15 minutes is folded; a non-zero exit termination and a Failed Event of the
// same container, with a request that has no pull request yet, a consequence of another container and one
// created more than 15 minutes before stay as they were, their requests untouched.
func TestFoldLeavesWhatIsNotItsConsequence(t *testing.T) {
	other := func(f int, uid types.UID, change func(*remediationv1alpha1.Incident)) *remediationv1alpha1.Incident {
		inc := foldConsequence(5 * time.Minute)
		inc.Name, inc.Spec.Fingerprint, inc.UID = Name(fp(f)), fp(f), uid
		change(inc)
		return inc
	}
	termination := other(3, "64646464-6464-4464-8464-646464646464", func(inc *remediationv1alpha1.Incident) {
		o := symptomOf(3, reasonExitError, terminationID(3), 1, fingerprint.SourcePodStatus, "web", collector.ClassNone, triggerNow)
		inc.Spec = specOf(o)
	})
	failed := other(4, "65656565-6565-4565-8565-656565656565", func(inc *remediationv1alpha1.Incident) { inc.Spec.Reason = "Failed" })
	elsewhere := other(5, "66666666-6666-4666-8666-666666666666", func(inc *remediationv1alpha1.Incident) {
		inc.Status.Containers = []remediationv1alpha1.Name{"db"}
	})
	old := other(6, "67676767-6767-4767-8767-676767676767", func(inc *remediationv1alpha1.Incident) {
		inc.CreationTimestamp = metav1.NewTime(triggerNow.Add(-RecurWithin - time.Minute))
	})
	requestOf := func(name string, inc *remediationv1alpha1.Incident) remediationv1alpha1.RemediationRequest {
		rr := request(name, time.Minute, remediationv1alpha1.PhasePolicyChecked)
		rr.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: inc.Name, UID: remediationv1alpha1.UID(inc.UID)}
		return rr
	}
	kept := []*remediationv1alpha1.Incident{termination, failed, elsewhere, old}
	c, cl, _ := foldController(t, append([]*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)}, kept...),
		requestOf("termination", termination), requestOf("failed", failed))
	if err := c.foldInto(t.Context(), Name(fp(1))); err != nil {
		t.Fatal(err)
	}
	checkFolded(t, getNamed(t, cl, Name(fp(2))))
	for _, inc := range kept {
		if got := getNamed(t, cl, inc.Name); got.Status.Phase != remediationv1alpha1.IncidentOpen || got.Status.FoldedInto != nil {
			t.Errorf("%s: phase %s foldedInto %+v, want Open and not folded", inc.Spec.Reason, got.Status.Phase,
				got.Status.FoldedInto)
		}
	}
	for _, name := range []string{"termination", "failed"} {
		rr := &remediationv1alpha1.RemediationRequest{}
		if err := cl.Get(t.Context(), client.ObjectKey{Namespace: fakeNamespace, Name: name}, rr); err != nil ||
			rr.Status.Phase != remediationv1alpha1.PhasePolicyChecked || rr.Spec.Cancel {
			t.Errorf("request %s: %+v (%v), want it untouched", name, rr.Status, err)
		}
	}
}

// TestFoldWriteCarriesTheResourceVersion: a writer that got there between the fold's read and its write (an
// analysis that won, moving the Incident to Proposed and opening a pull request) makes the fold's write fail
// on the resource version it read; the fold is decided again from the new state, and does not fold.
func TestFoldWriteCarriesTheResourceVersion(t *testing.T) {
	c, cl, w := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
	won := false
	c.reader = &afterRead{Client: cl, then: func(ctx context.Context) {
		if won {
			return
		}
		won = true
		rr := foldRequest("won", remediationv1alpha1.PhasePRCreated, true)
		if err := cl.Create(ctx, &rr); err != nil {
			t.Fatal(err)
		}
		live := getNamed(t, cl, Name(fp(2)))
		live.Status.Phase = remediationv1alpha1.IncidentProposed
		live.Status.RemediationRequests = []remediationv1alpha1.Name{"won"}
		if err := cl.Status().Update(ctx, live); err != nil {
			t.Fatal(err)
		}
	}}
	var answers []error
	w.after = func(err error) error {
		answers = append(answers, err)
		return err
	}
	if err := c.foldInto(t.Context(), Name(fp(1))); err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || !apierrors.IsConflict(answers[0]) {
		t.Fatalf("fold writes answered %v, want one conflict: the stale write must fail and the new state not fold",
			answers)
	}
	got := getNamed(t, cl, Name(fp(2)))
	if got.Status.Phase != remediationv1alpha1.IncidentProposed || got.Status.FoldedInto != nil {
		t.Fatalf("phase %s foldedInto %+v, want Proposed and not folded after the analysis won", got.Status.Phase,
			got.Status.FoldedInto)
	}
}

// afterRead runs then after each listing of the RemediationRequests, the last read before the fold's write, as
// another writer acting between the reads and the write that follows them.
type afterRead struct {
	client.Client
	then func(ctx context.Context)
}

func (a *afterRead) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := a.Client.List(ctx, list, opts...)
	if _, ok := list.(*remediationv1alpha1.RemediationRequestList); ok && err == nil {
		a.then(ctx)
	}
	return err
}

// TestFoldRetriesOnTheNewState: a conflict from a write that leaves the Incident foldable (another field
// changed) is decided again and folds on the new version.
func TestFoldRetriesOnTheNewState(t *testing.T) {
	c, cl, w := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
	attempts := 0
	w.before = func(ctx context.Context, sent *remediationv1alpha1.Incident) {
		attempts++
		if attempts > 1 {
			return
		}
		live := getNamed(t, cl, sent.Name)
		live.Status.Count++
		if err := cl.Status().Update(ctx, live); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.foldInto(t.Context(), Name(fp(1))); err != nil {
		t.Fatal(err)
	}
	got := getNamed(t, cl, Name(fp(2)))
	checkFolded(t, got)
	if got.Status.Count != 2 || attempts != 2 {
		t.Fatalf("count %d after %d attempts, want 2 and 2: the fold must keep the other writer's change",
			got.Status.Count, attempts)
	}
}

// TestFailedFoldReadIsRetried: a cause Incident that cannot be read is an error, and the fold stays owed until
// a later pass does it; nothing is folded meanwhile and nothing is dropped.
func TestFailedFoldReadIsRetried(t *testing.T) {
	c, cl, _ := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
	failing := &stalling{Client: cl}
	c.reader = failing
	c.mu.Lock()
	c.oweFold(Name(fp(1)))
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	c.foldDue(ctx)
	cancel()
	c.mu.Lock()
	owed, ok := c.folds[Name(fp(1))]
	c.mu.Unlock()
	if !ok || !owed.at.After(triggerNow) {
		t.Fatalf("owed %+v (%v) after a failed read, want it owed again later", owed, ok)
	}
	if got := getNamed(t, cl, Name(fp(2))); got.Status.FoldedInto != nil {
		t.Fatal("folded although the cause could not be read")
	}
	// Not due yet: nothing is tried.
	c.reader = cl
	c.foldDue(t.Context())
	if got := getNamed(t, cl, Name(fp(2))); got.Status.FoldedInto != nil {
		t.Fatal("a fold owed later was tried before its time")
	}
	c.cfg.Now = func() time.Time { return owed.at }
	c.foldDue(t.Context())
	got := getNamed(t, cl, Name(fp(2)))
	if got.Status.FoldedInto == nil {
		t.Fatal("the owed fold was not done once the read worked")
	}
	c.mu.Lock()
	_, ok = c.folds[Name(fp(1))]
	c.mu.Unlock()
	if ok {
		t.Fatal("a fold done is still owed")
	}
}

// TestClosureSkipsAFoldedIncident (ADR-0039 point 15): a folded Incident does not return to Proposed for an
// active request and does not end Escalated for the cancel (IncidentFolded) that the fold causes.
func TestClosureSkipsAFoldedIncident(t *testing.T) {
	folded := func(t *testing.T) *remediationv1alpha1.Incident {
		inc := closureIncident(remediationv1alpha1.IncidentProposed, 2*time.Hour)
		cause := foldCause()
		cause.Name, cause.UID = Name(fp(9)), foldConsequenceUID
		if err := fold(inc, cause, triggerNow.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		return inc
	}
	for name, rr := range map[string]remediationv1alpha1.RemediationRequest{
		"active request":    request("a", 30*time.Second, remediationv1alpha1.PhasePolicyChecked),
		"cancelled request": request("a", 30*time.Second, remediationv1alpha1.PhaseCancelled),
	} {
		t.Run(name, func(t *testing.T) {
			if phase, reason, ok := Closure(folded(t), []remediationv1alpha1.RemediationRequest{rr}, time.Time{}, triggerNow); ok {
				t.Fatalf("moved to %s (%s), want a folded Incident skipped", phase, reason)
			}
			c, cl := closureController(t, folded(t), rr)
			if err := c.closeIncidents(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := getIncident(t, cl)
			if got.Status.Phase != remediationv1alpha1.IncidentResolved || got.Status.FoldedInto == nil {
				t.Fatalf("phase %s foldedInto %+v, want Resolved and folded", got.Status.Phase, got.Status.FoldedInto)
			}
		})
	}
}

// TestEscalationClearsFoldedInto: a folded Incident escalated as a recurrence leaves Resolved, so it cannot
// keep foldedInto, which the schema allows only in phase Resolved.
func TestEscalationClearsFoldedInto(t *testing.T) {
	inc := foldConsequence(time.Hour)
	if err := fold(inc, foldCause(), triggerNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	escalateRecurrence(inc, triggerNow)
	if inc.Status.Phase != remediationv1alpha1.IncidentEscalated || inc.Status.FoldedInto != nil || !inc.Status.Folded {
		t.Fatalf("phase %s foldedInto %+v folded %v, want Escalated, foldedInto cleared and folded kept",
			inc.Status.Phase, inc.Status.FoldedInto, inc.Status.Folded)
	}
}

// TestLateCauseFoldsItsConsequence (envtest, the schema's rules apply): a BackOff of container web opens its
// own Incident, then the OOM kill of web opens the cause's Incident, and the write loop's pass that created it
// folds the consequence. A BackOff of container db, and a Failed Event of web, stay open.
func TestLateCauseFoldsItsConsequence(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r.obs <- consequenceOf(2, 0, "web", reasonBackOff, 1, 2, at)
	r.obs <- consequenceOf(3, 0, "db", reasonBackOff, 2, 2, at)
	failed := symptomOf(4, "Failed", eventID(3), 2, fingerprint.SourceEvent, "web", collector.ClassNone, at)
	failed.Began = at.Add(-RecurAfter)
	r.obs <- failed
	for _, f := range []int{2, 3, 4} {
		eventually(t, ns, f, countIs(2))
	}
	r.obs <- oomCause(1, at.Add(time.Minute))
	cause := eventually(t, ns, 1, countIs(1))
	got := eventually(t, ns, 2, func(inc *remediationv1alpha1.Incident) bool { return inc.Status.FoldedInto != nil })
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionLifecycle)
	if got.Status.Phase != remediationv1alpha1.IncidentResolved || got.Status.ResolvedTime == nil || cond == nil ||
		cond.Reason != ReasonFoldedIntoCause || got.Status.FoldedInto.Name != cause.Name ||
		types.UID(got.Status.FoldedInto.UID) != cause.UID {
		t.Fatalf("phase %s lifecycle %+v foldedInto %+v, want Resolved, FoldedIntoCause, %s", got.Status.Phase, cond,
			got.Status.FoldedInto, cause.Name)
	}
	managed := false
	for _, m := range got.ManagedFields {
		managed = managed || (m.Manager == lifecycleFieldOwner && m.Subresource == "status")
	}
	if !managed {
		t.Fatalf("no status fields of manager %s: %+v", lifecycleFieldOwner, got.ManagedFields)
	}
	for _, f := range []int{3, 4} {
		inc := &remediationv1alpha1.Incident{}
		if err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: Name(fp(f))}, inc); err != nil {
			t.Fatal(err)
		}
		if inc.Status.FoldedInto != nil || inc.Status.Phase == remediationv1alpha1.IncidentResolved {
			t.Errorf("Incident of fp(%d) (%s): phase %s foldedInto %+v, want it left open", f, inc.Spec.Reason,
				inc.Status.Phase, inc.Status.FoldedInto)
		}
	}
	r.mu.Lock()
	owed := len(r.folds)
	r.mu.Unlock()
	if owed != 0 {
		t.Fatalf("%d folds still owed after the fold", owed)
	}
}

// TestLostCauseWriteStillFolds: the write that made the Incident a cause was stored but its answer was lost.
// The next write finds status.cause already set, so only the lost write can tell that the fold is owed; the
// fold still happens.
func TestLostCauseWriteStillFolds(t *testing.T) {
	ns := namespace(t)
	var lost atomic.Bool
	var sentCause atomic.Bool
	r := startWith(t, ns, Config{Window: 100 * time.Millisecond, RetryAfter: 100 * time.Millisecond},
		func(_ *Controller, w *countingClient) {
			w.before = func(_ context.Context, inc *remediationv1alpha1.Incident) {
				sentCause.Store(inc.Name == Name(fp(1)) && inc.Status.Cause)
			}
			w.after = func(err error) error {
				if err == nil && sentCause.Load() && lost.CompareAndSwap(false, true) {
					return apierrors.NewTimeoutError("the answer was lost", 1)
				}
				return err
			}
		})
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r.obs <- consequenceOf(2, 0, foldContainer, reasonBackOff, 1, 2, at)
	eventually(t, ns, 2, countIs(2))
	r.obs <- oomCause(1, at.Add(time.Minute))
	eventually(t, ns, 1, countIs(1))
	eventually(t, ns, 2, func(inc *remediationv1alpha1.Incident) bool { return inc.Status.FoldedInto != nil })
	if !lost.Load() {
		t.Fatal("no answer of the cause write was lost")
	}
}

// TestStoredCauseIsFoldedByThePass: a cause Incident stored before this controller started (written by the
// final flush at stop, or before a restart) is no fold this controller owes in memory; the trigger pass derives
// the fold from the stored Incidents. A second pass, with nothing left to fold, writes nothing.
func TestStoredCauseIsFoldedByThePass(t *testing.T) {
	c, cl, w := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
	if len(c.folds) != 0 {
		t.Fatalf("%d folds owed after the load, want none: the test needs the stored state alone", len(c.folds))
	}
	// The closure rules may write the cause (quiet for an hour); only the writes of the consequence count.
	var writes int
	w.before = func(_ context.Context, inc *remediationv1alpha1.Incident) {
		if inc.Name == Name(fp(2)) {
			writes++
		}
	}
	_ = c.pass(t.Context())
	checkFolded(t, getNamed(t, cl, Name(fp(2))))
	if writes != 1 {
		t.Fatalf("%d status writes of the consequence, want 1", writes)
	}
	_ = c.pass(t.Context())
	if writes != 1 {
		t.Fatalf("%d status writes of the consequence after a pass with nothing to fold, want still 1", writes)
	}
}

// TestOwedFoldDoesNotAgeOut: the trigger pass decides from the two creationTimestamps alone, never from the
// controller's clock, so a fold owed since long before (an outage, pass timeouts, a late restart) is still
// made; a consequence created more than RecurWithin before its cause is left to the closure rules. Once its
// cause exists, a consequence's lastSeen stops moving, so it is quiet for far longer than QuietBeforeResolved:
// the pass folds it before the closure rules would resolve it SymptomQuiet.
func TestOwedFoldDoesNotAgeOut(t *testing.T) {
	for name, tc := range map[string]struct {
		createdBefore time.Duration
		folds         bool
	}{
		"within RecurWithin of the cause":    {createdBefore: RecurWithin, folds: true},
		"more than RecurWithin before cause": {createdBefore: RecurWithin + time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			// Hours after the cause opened: far past RecurWithin plus a trigger interval. The consequence was
			// last seen when the cause opened, as the observation writer leaves it.
			now := triggerNow.Add(6 * time.Hour)
			cons := foldConsequence(tc.createdBefore)
			cons.Status.LastSeen = &metav1.Time{Time: triggerNow}
			c, cl, _ := foldController(t, []*remediationv1alpha1.Incident{cons})
			c.cfg.Now = func() time.Time { return now }
			var writes int
			c.client.(*countingClient).before = func(_ context.Context, inc *remediationv1alpha1.Incident) {
				if inc.Name == Name(fp(2)) {
					writes++
				}
			}
			_ = c.pass(t.Context())
			got := getNamed(t, cl, Name(fp(2)))
			reason := lifecycleReason(got)
			if tc.folds {
				if got.Status.Phase != remediationv1alpha1.IncidentResolved || !got.Status.Folded ||
					reason != ReasonFoldedIntoCause || got.Status.FoldedInto == nil ||
					got.Status.FoldedInto.Name != Name(fp(1)) || writes != 1 {
					t.Fatalf("phase %s reason %s folded %v foldedInto %+v, %d writes, want folded into %s by one write",
						got.Status.Phase, reason, got.Status.Folded, got.Status.FoldedInto, writes, Name(fp(1)))
				}
				return
			}
			if got.Status.FoldedInto != nil || got.Status.Folded || reason == ReasonFoldedIntoCause {
				t.Fatalf("folded %v foldedInto %+v reason %s, want it not folded",
					got.Status.Folded, got.Status.FoldedInto, reason)
			}
		})
	}
}

// TestFoldLeavesAnEscalationForAPerson: a Proposed consequence whose newest request ended without success and
// without a pull request is escalated by the closure rules; the fold, which would make them skip it, leaves it
// alone, on the write loop's fast path and in the trigger pass.
func TestFoldLeavesAnEscalationForAPerson(t *testing.T) {
	for _, phase := range []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseNeedsIntervention,
		remediationv1alpha1.PhaseDenied, remediationv1alpha1.PhaseFailed} {
		setup := func(t *testing.T) (*Controller, client.Client) {
			cons := foldConsequence(5 * time.Minute)
			cons.Status.Phase = remediationv1alpha1.IncidentProposed
			c, cl, _ := foldController(t, []*remediationv1alpha1.Incident{cons}, foldRequest("a", phase, false))
			return c, cl
		}
		t.Run(string(phase)+" fast path", func(t *testing.T) {
			c, cl := setup(t)
			c.mu.Lock()
			c.oweFold(Name(fp(1)))
			c.mu.Unlock()
			c.foldDue(t.Context())
			if got := getNamed(t, cl, Name(fp(2))); got.Status.Folded || got.Status.FoldedInto != nil ||
				got.Status.Phase != remediationv1alpha1.IncidentProposed {
				t.Fatalf("phase %s folded %v foldedInto %+v, want it Proposed and not folded", got.Status.Phase,
					got.Status.Folded, got.Status.FoldedInto)
			}
		})
		t.Run(string(phase)+" pass", func(t *testing.T) {
			c, cl := setup(t)
			_ = c.pass(t.Context())
			got := getNamed(t, cl, Name(fp(2)))
			if reason := endedWithoutSuccess[phase]; got.Status.Folded || got.Status.FoldedInto != nil ||
				got.Status.Phase != remediationv1alpha1.IncidentEscalated || lifecycleReason(got) != reason {
				t.Fatalf("phase %s reason %s folded %v, want Escalated with %s and not folded", got.Status.Phase,
					lifecycleReason(got), got.Status.Folded, reason)
			}
		})
	}
}

// TestFoldedIncidentIsNeverFoldedAgain: a consequence folded, reopened by a recurrence and analysed has lost
// foldedInto and the Lifecycle reason Reopened (an analysis, a request outcome or an escalation replaces it);
// status.folded still keeps every later pass from folding it a second time. An Escalated consequence, never
// folded, waits for a person and is not folded either.
func TestFoldedIncidentIsNeverFoldedAgain(t *testing.T) {
	t.Run("reopened and analysed", func(t *testing.T) {
		c, cl, _ := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
		_ = c.pass(t.Context())
		checkFolded(t, getNamed(t, cl, Name(fp(2))))
		reopenNamed(t, cl, Name(fp(2)), triggerNow.Add(time.Minute))
		live := getNamed(t, cl, Name(fp(2)))
		// Analysed after the reopen: the Lifecycle reason is no longer Reopened.
		live.Status.Phase = remediationv1alpha1.IncidentAnalyzing
		meta.SetStatusCondition(&live.Status.Conditions, metav1.Condition{Type: ConditionLifecycle,
			Status: metav1.ConditionTrue, Reason: ReasonRequestDenied, Message: "denied"})
		if err := cl.Status().Update(t.Context(), live); err != nil {
			t.Fatal(err)
		}
		_ = c.pass(t.Context())
		if got := getNamed(t, cl, Name(fp(2))); got.Status.FoldedInto != nil ||
			got.Status.Phase != remediationv1alpha1.IncidentAnalyzing {
			t.Fatalf("phase %s foldedInto %+v, want a folded Incident never folded again", got.Status.Phase,
				got.Status.FoldedInto)
		}
	})
	t.Run("escalated", func(t *testing.T) {
		cons := foldConsequence(5 * time.Minute)
		cons.Status.Phase = remediationv1alpha1.IncidentEscalated
		c, cl, _ := foldController(t, []*remediationv1alpha1.Incident{cons})
		if err := c.foldRecent(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := c.foldInto(t.Context(), Name(fp(1))); err != nil {
			t.Fatal(err)
		}
		if got := getNamed(t, cl, Name(fp(2))); got.Status.FoldedInto != nil {
			t.Fatalf("phase %s folded, want an Escalated Incident left for a person", got.Status.Phase)
		}
	})
}

// failingReads fails every read of an Incident with a server error.
type failingReads struct{ client.Client }

func (f *failingReads) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return apierrors.NewServiceUnavailable("unavailable")
}

// TestFoldFailureCounts: a fold of the fast path that fails is a write error, counted once; one cancelled by
// shutdown is none. A fold of the trigger pass that fails is returned by the pass, which counts it as a pass
// error, and not counted as a write error too.
func TestFoldFailureCounts(t *testing.T) {
	for name, tc := range map[string]struct {
		cancelled bool
		want      float64
	}{
		"failed read": {want: 1},
		"shutdown":    {cancelled: true},
	} {
		t.Run(name, func(t *testing.T) {
			c, cl, _ := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
			c.reader = &failingReads{Client: cl}
			ctx := t.Context()
			if tc.cancelled {
				c.reader = &stalling{Client: cl}
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cctx
			}
			c.mu.Lock()
			c.oweFold(Name(fp(1)))
			c.mu.Unlock()
			before := testutil.ToFloat64(incidentWriteErrors)
			c.foldDue(ctx)
			if got := testutil.ToFloat64(incidentWriteErrors) - before; got != tc.want {
				t.Fatalf("write errors counted %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("trigger pass", func(t *testing.T) {
		c, cl, _ := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
		c.reader = &failingReads{Client: cl}
		before := testutil.ToFloat64(incidentWriteErrors)
		if err := c.foldRecent(t.Context()); err == nil {
			t.Fatal("a failed fold of the trigger pass returned no error")
		}
		if got := testutil.ToFloat64(incidentWriteErrors) - before; got != 0 {
			t.Fatalf("write errors counted %v, want 0: the pass counts its failure once", got)
		}
	})
}

// hiding hides the Incident named hidden from the listings of the cache while it is set, as an informer that
// has not seen it yet.
type hiding struct {
	client.Client
	hidden atomic.Pointer[string]
}

func (h *hiding) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := h.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if l, ok := list.(*remediationv1alpha1.IncidentList); ok {
		if name := h.hidden.Load(); name != nil {
			l.Items = slices.DeleteFunc(l.Items, func(inc remediationv1alpha1.Incident) bool { return inc.Name == *name })
		}
	}
	return nil
}

// TestFoldMissedByTheCacheIsFoldedLater: the cache had not seen the consequence when the owed fold ran, so the
// fast path found nothing to fold and dropped it; the next trigger pass, once the cache has it, folds it.
func TestFoldMissedByTheCacheIsFoldedLater(t *testing.T) {
	c, cl, _ := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
	h := &hiding{Client: cl}
	name := Name(fp(2))
	h.hidden.Store(&name)
	c.cache = h
	c.mu.Lock()
	c.oweFold(Name(fp(1)))
	c.mu.Unlock()
	c.foldDue(t.Context())
	if got := getNamed(t, cl, name); got.Status.FoldedInto != nil {
		t.Fatal("folded an Incident the cache did not show")
	}
	_ = c.pass(t.Context())
	if got := getNamed(t, cl, name); got.Status.FoldedInto != nil {
		t.Fatal("folded an Incident the cache did not show")
	}
	h.hidden.Store(nil)
	_ = c.pass(t.Context())
	checkFolded(t, getNamed(t, cl, name))
}

// reopenNamed reopens the folded Incident name as a recurrence does: Open, Lifecycle Reopened, foldedInto
// cleared.
func reopenNamed(t *testing.T, cl client.Client, name string, now time.Time) {
	t.Helper()
	live := getNamed(t, cl, name)
	if live.Status.FoldedInto == nil {
		t.Fatalf("%s is not folded, nothing to reopen", name)
	}
	reopen(live, now)
	if err := cl.Status().Update(t.Context(), live); err != nil {
		t.Fatal(err)
	}
}

// checkReopened fails unless the Incident name is Open, Reopened and not folded.
func checkReopened(t *testing.T, cl client.Client, name string) {
	t.Helper()
	got := getNamed(t, cl, name)
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionLifecycle)
	if got.Status.Phase != remediationv1alpha1.IncidentOpen || got.Status.FoldedInto != nil || cond == nil ||
		cond.Reason != ReasonReopened {
		t.Fatalf("%s: phase %s lifecycle %+v foldedInto %+v, want it reopened and left so", name, got.Status.Phase,
			cond, got.Status.FoldedInto)
	}
}

// TestLostFoldAnswerDoesNotFoldAReopenedIncident: the fold was stored but its answer lost, so it stays owed;
// meanwhile the consequence was seen again and reopened. The retry, and the trigger pass, leave it Open.
func TestLostFoldAnswerDoesNotFoldAReopenedIncident(t *testing.T) {
	c, cl, w := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
	lost := false
	w.after = func(err error) error {
		if err == nil && !lost {
			lost = true
			return apierrors.NewTimeoutError("the answer was lost", 1)
		}
		return err
	}
	c.mu.Lock()
	c.oweFold(Name(fp(1)))
	c.mu.Unlock()
	c.foldDue(t.Context())
	c.mu.Lock()
	owed, ok := c.folds[Name(fp(1))]
	c.mu.Unlock()
	if !lost || !ok {
		t.Fatalf("answer lost %v, owed %v: want the fold owed again after a lost answer", lost, ok)
	}
	reopenNamed(t, cl, Name(fp(2)), triggerNow.Add(time.Minute))
	c.cfg.Now = func() time.Time { return owed.at }
	c.foldDue(t.Context())
	checkReopened(t, cl, Name(fp(2)))
	_ = c.pass(t.Context())
	checkReopened(t, cl, Name(fp(2)))
}

// TestPartialFoldRetryLeavesAReopenedIncident: of two consequences, one is folded and the other's write is
// refused, so the fold stays owed; the folded one is reopened before the retry. The retry folds the other and
// leaves the reopened one Open.
func TestPartialFoldRetryLeavesAReopenedIncident(t *testing.T) {
	second := foldConsequence(5 * time.Minute)
	second.Name, second.Spec.Fingerprint, second.UID = Name(fp(3)), fp(3), "68686868-6868-4868-8868-686868686868"
	c, cl, w := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute), second})
	refuse := true
	var sending string
	w.before = func(_ context.Context, inc *remediationv1alpha1.Incident) { sending = inc.Name }
	w.instead = func(context.Context) error {
		if refuse && sending == second.Name {
			return apierrors.NewServiceUnavailable("refused")
		}
		return nil
	}
	c.mu.Lock()
	c.oweFold(Name(fp(1)))
	c.mu.Unlock()
	c.foldDue(t.Context())
	c.mu.Lock()
	owed, ok := c.folds[Name(fp(1))]
	c.mu.Unlock()
	if !ok {
		t.Fatal("the fold is not owed after a refused write")
	}
	checkFolded(t, getNamed(t, cl, Name(fp(2))))
	if got := getNamed(t, cl, second.Name); got.Status.FoldedInto != nil {
		t.Fatal("the refused write folded")
	}
	reopenNamed(t, cl, Name(fp(2)), triggerNow.Add(time.Minute))
	refuse = false
	c.cfg.Now = func() time.Time { return owed.at }
	c.foldDue(t.Context())
	checkReopened(t, cl, Name(fp(2)))
	if got := getNamed(t, cl, second.Name); got.Status.FoldedInto == nil || got.Status.FoldedInto.Name != Name(fp(1)) {
		t.Fatalf("the other consequence: foldedInto %+v, want folded into %s by the retry", got.Status.FoldedInto,
			Name(fp(1)))
	}
}

// TestCauseStoredAtStopIsFoldedAfterRestart (envtest): the cause's Incident is written by the final flush at
// stop, after the write loop that folds has ended, so that run never folds; the next run's trigger pass does.
func TestCauseStoredAtStopIsFoldedAfterRestart(t *testing.T) {
	ns := namespace(t)
	at := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	first := start(t, ns, Config{Window: 100 * time.Millisecond})
	first.obs <- consequenceOf(2, 0, foldContainer, reasonBackOff, 1, 2, at)
	eventually(t, ns, 2, countIs(2))
	first.stop()

	// A window longer than the test: the cause is written only by the flush at stop.
	second := start(t, ns, Config{Window: time.Hour, SettleLimit: time.Hour})
	second.obs <- oomCause(1, at.Add(time.Minute))
	second.stop()
	cause := eventually(t, ns, 1, countIs(1))
	if !cause.Status.Cause {
		t.Fatal("the stored Incident is not a cause")
	}
	if got := eventually(t, ns, 2, countIs(2)); got.Status.FoldedInto != nil {
		t.Fatal("folded by the run that stopped: the test needs a cause written by the flush at stop")
	}

	third := start(t, ns, Config{Window: 100 * time.Millisecond, TriggerInterval: time.Second})
	defer third.stop()
	got := eventually(t, ns, 2, func(inc *remediationv1alpha1.Incident) bool { return inc.Status.FoldedInto != nil })
	if got.Status.Phase != remediationv1alpha1.IncidentResolved || got.Status.FoldedInto.Name != cause.Name ||
		types.UID(got.Status.FoldedInto.UID) != cause.UID {
		t.Fatalf("phase %s foldedInto %+v, want Resolved and folded into %s", got.Status.Phase, got.Status.FoldedInto,
			cause.Name)
	}
}
