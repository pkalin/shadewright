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
)

// The tests of the late-cause fold (ADR-0039 points 12 to 15 and 23).

const (
	foldConsequenceUID = types.UID("61616161-6161-4161-8161-616161616161")
	foldCauseUID       = types.UID("62626262-6262-4262-8262-626262626262")
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
		"Unhealthy":       {change: func(c, _ *remediationv1alpha1.Incident) { c.Spec.Reason = reasonUnhealthy }, want: true},
		"phase Analyzing": {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Phase = remediationv1alpha1.IncidentAnalyzing }, want: true},
		"phase Proposed":  {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Phase = remediationv1alpha1.IncidentProposed }, want: true},
		"phase Escalated": {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Phase = remediationv1alpha1.IncidentEscalated }, want: true},
		"phase Resolved":  {change: func(c, _ *remediationv1alpha1.Incident) { c.Status.Phase = remediationv1alpha1.IncidentResolved }},
		"already folded": {change: func(c, _ *remediationv1alpha1.Incident) {
			c.Status.FoldedInto = &remediationv1alpha1.IncidentRef{Name: "x", UID: "y"}
		}},
		"non-zero exit termination":    {change: func(c, _ *remediationv1alpha1.Incident) { c.Spec.Reason = "Error" }},
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
		"request without a pull request": {requests: []remediationv1alpha1.RemediationRequest{
			foldRequest("a", remediationv1alpha1.PhasePolicyChecked, false),
			foldRequest("b", remediationv1alpha1.PhaseDenied, false)}, want: true},
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
			if inc.Status.Phase != before.Status.Phase || inc.Status.FoldedInto != nil {
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
		cond.Message != lifecycleMessages[ReasonFoldedIntoCause] || inc.Status.FoldedInto == nil ||
		inc.Status.FoldedInto.Name != Name(fp(1)) || types.UID(inc.Status.FoldedInto.UID) != foldCauseUID {
		t.Fatalf("phase %s resolvedTime %v lifecycle %+v foldedInto %+v, want folded into %s", inc.Status.Phase,
			inc.Status.ResolvedTime, cond, inc.Status.FoldedInto, Name(fp(1)))
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
		o := symptomOf(3, "Error", terminationID(3), 1, fingerprint.SourcePodStatus, "web", collector.ClassNone, triggerNow)
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
	if inc.Status.Phase != remediationv1alpha1.IncidentEscalated || inc.Status.FoldedInto != nil {
		t.Fatalf("phase %s foldedInto %+v, want Escalated and cleared", inc.Status.Phase, inc.Status.FoldedInto)
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
