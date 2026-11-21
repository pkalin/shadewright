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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corerecord "k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// waitSkipEvent waits, with a deadline, for the next event of rec and checks that it reports a skip.
func waitSkipEvent(t *testing.T, rec *corerecord.FakeRecorder) {
	t.Helper()
	select {
	case e := <-rec.Events:
		if !strings.HasPrefix(e, "Normal AnalysisSkipped ") {
			t.Fatalf("event %q", e)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no AnalysisSkipped event")
	}
}

// noEvent checks that rec holds no event.
func noEvent(t *testing.T, rec *corerecord.FakeRecorder, what string) {
	t.Helper()
	select {
	case e := <-rec.Events:
		t.Fatalf("%s: %q", what, e)
	default:
	}
}

// skipsSince returns the skips counted since the count was before.
func skipsSince(before float64) float64 { return testutil.ToFloat64(notRecentSkips) - before }

// markCounter is a cache that counts the passes that decide the due marks: each [Controller.markDue] that
// listed the Incidents gets the RemediationSettings from the cache once, after it reported the skips it found
// ([Controller.reportSkips]). passCounter counts every listing, several per pass.
type markCounter struct {
	client.Reader
	gets atomic.Int32
}

func (m *markCounter) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.RemediationSettings); ok {
		m.gets.Add(1)
	}
	return m.Reader.Get(ctx, key, obj, opts...)
}

// afterPasses waits, with a deadline, until at least n passes have decided the due marks from start to end
// since it was called.
func (m *markCounter) afterPasses(t *testing.T, n int32) {
	t.Helper()
	// The pass running now may have reported its skips already: n + 1 more gets mean n whole passes.
	want := m.gets.Load() + n + 1
	deadline := time.Now().Add(20 * time.Second)
	for m.gets.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d passes ran", n)
		}
		time.Sleep(testTriggerInterval / 4)
	}
}

// startSkips starts the trigger on ns with a recorder and a pass counter; lose, when set, is the mark hook of
// the counting client.
func startSkips(t *testing.T, ns string, lose func(client.Object) error) (*running, *corerecord.FakeRecorder, *markCounter) {
	t.Helper()
	rec := corerecord.NewFakeRecorder(20)
	passes := &markCounter{Reader: env.admin}
	cfg := triggerConfig
	cfg.Recorder = rec
	r := startWith(t, ns, cfg, func(c *Controller, w *countingClient) {
		c.cache = passes
		w.mark = lose
	})
	return r, rec, passes
}

// A NotRecent write that lands but whose answer is lost (the pass deadline ran out) is still reported, once
// (#983): the next pass reads the condition this trigger loop wrote and finds no report of it. Later passes
// report nothing more.
func TestLostNotRecentAnswerIsReportedOnce(t *testing.T) {
	ns := namespace(t)
	createIncident(t, ns, 1, incidentStatus(35*time.Minute, 35*time.Minute, 0))
	createSettings(t, 2)
	createBudget(t, ns, nil)
	before := testutil.ToFloat64(notRecentSkips)
	var lost atomic.Int32
	_, rec, passes := startSkips(t, ns, func(obj client.Object) error {
		inc := obj.(*remediationv1alpha1.Incident)
		c := meta.FindStatusCondition(inc.Status.Conditions, ConditionAnalysisDue)
		if c != nil && c.Reason == ReasonNotRecent && lost.Add(1) == 1 {
			// The update lands, its answer does not.
			if err := env.admin.Status().Update(context.Background(), obj); err != nil {
				return err
			}
			return context.DeadlineExceeded
		}
		return nil
	})
	eventually(t, ns, 1, dueIs(false, ReasonNotRecent))
	waitSkipEvent(t, rec)
	passes.afterPasses(t, 3)
	noEvent(t, rec, "a second event for one skip")
	if lost.Load() != 1 {
		t.Fatalf("%d NotRecent writes, want the one whose answer was lost", lost.Load())
	}
	if got := skipsSince(before); got != 1 {
		t.Fatalf("skips counted %v, want 1", got)
	}
}

// A NotRecent condition written before the trigger loop began (a restart or a new leader) is not reported
// again: the process that wrote it did.
func TestNotRecentFromBeforeTheStartIsNotReported(t *testing.T) {
	ns := namespace(t)
	st := incidentStatus(35*time.Minute, 35*time.Minute, 0)
	st.Conditions = []metav1.Condition{{Type: ConditionAnalysisDue, Status: metav1.ConditionFalse,
		Reason: ReasonNotRecent, Message: dueMessages[ReasonNotRecent], ObservedGeneration: 1,
		LastTransitionTime: metav1.NewTime(triggerNow.Add(-time.Hour))}}
	createIncident(t, ns, 1, st)
	createSettings(t, 2)
	createBudget(t, ns, nil)
	before := testutil.ToFloat64(notRecentSkips)
	r, rec, passes := startSkips(t, ns, nil)
	passes.afterPasses(t, 3)
	noEvent(t, rec, "an event for a skip written before the start")
	if got := skipsSince(before); got != 0 {
		t.Fatalf("skips counted %v, want 0", got)
	}
	if m := r.writes.marks.Load(); m != 0 {
		t.Fatalf("%d writes of the trigger, want none: the condition already says NotRecent", m)
	}
}

// A NotRecent whose write was answered is reported by that write, and the passes that read it back from the
// cache afterwards report nothing more.
func TestReportedNotRecentIsNotReportedAgain(t *testing.T) {
	ns := namespace(t)
	createIncident(t, ns, 1, incidentStatus(35*time.Minute, 35*time.Minute, 0))
	createSettings(t, 2)
	createBudget(t, ns, nil)
	before := testutil.ToFloat64(notRecentSkips)
	_, rec, passes := startSkips(t, ns, nil)
	eventually(t, ns, 1, dueIs(false, ReasonNotRecent))
	waitSkipEvent(t, rec)
	passes.afterPasses(t, 3)
	noEvent(t, rec, "a second event for one skip")
	if got := skipsSince(before); got != 1 {
		t.Fatalf("skips counted %v, want 1", got)
	}
}

// What the trigger remembers of a reported skip goes with the Incident: once it is deleted, nothing of it is
// kept, so the memory does not grow with the Incidents deleted over time.
func TestDeletedIncidentForgetsItsReportedSkip(t *testing.T) {
	ns := namespace(t)
	createIncident(t, ns, 1, incidentStatus(35*time.Minute, 35*time.Minute, 0))
	createSettings(t, 2)
	createBudget(t, ns, nil)
	r, rec, passes := startSkips(t, ns, nil)
	inc := eventually(t, ns, 1, dueIs(false, ReasonNotRecent))
	waitSkipEvent(t, rec)
	if err := env.admin.Delete(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	passes.afterPasses(t, 2)
	r.stop() // the memory belongs to the trigger goroutine
	if tr, ok := r.memory[inc.UID]; ok {
		t.Fatalf("the deleted Incident is still remembered: %+v", tr)
	}
}

// A NotRecent written over an older not-due reason keeps that reason's lastTransitionTime, which can be before
// the trigger loop began. When the answer of that write is lost, the write is remembered as pending and the
// next pass that reads NotRecent reports it, once (#983).
func TestLostNotRecentOverAnOlderReasonIsReportedOnce(t *testing.T) {
	ns := namespace(t)
	createIncident(t, ns, 1, withDueCondition(incidentStatus(35*time.Minute, 35*time.Minute, 0),
		metav1.ConditionFalse, ReasonDebouncing))
	createSettings(t, 2)
	createBudget(t, ns, nil)
	before := testutil.ToFloat64(notRecentSkips)
	var lost atomic.Int32
	_, rec, passes := startSkips(t, ns, func(obj client.Object) error {
		inc := obj.(*remediationv1alpha1.Incident)
		c := meta.FindStatusCondition(inc.Status.Conditions, ConditionAnalysisDue)
		if c != nil && c.Reason == ReasonNotRecent && lost.Add(1) == 1 {
			if !c.LastTransitionTime.Before(&metav1.Time{Time: triggerNow}) {
				t.Errorf("transition time %v, want the older reason's, before the start", c.LastTransitionTime)
			}
			// The update lands, its answer does not.
			if err := env.admin.Status().Update(context.Background(), obj); err != nil {
				return err
			}
			return context.DeadlineExceeded
		}
		return nil
	})
	eventually(t, ns, 1, dueIs(false, ReasonNotRecent))
	waitSkipEvent(t, rec)
	passes.afterPasses(t, 3)
	noEvent(t, rec, "a second event for one skip")
	if lost.Load() != 1 {
		t.Fatalf("%d NotRecent writes, want the one whose answer was lost", lost.Load())
	}
	if got := skipsSince(before); got != 1 {
		t.Fatalf("skips counted %v, want 1", got)
	}
}

// laggingCache serves the Incident list it holds until the pass counter reaches until: a cache that lags
// behind a write that landed.
type laggingCache struct {
	*markCounter
	mu       sync.Mutex
	snapshot *remediationv1alpha1.IncidentList
	until    int32
}

func (l *laggingCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if il, ok := list.(*remediationv1alpha1.IncidentList); ok {
		l.mu.Lock()
		snap, until := l.snapshot, l.until
		l.mu.Unlock()
		if snap != nil && l.gets.Load() < until {
			snap.DeepCopyInto(il)
			return nil
		}
	}
	return l.markCounter.List(ctx, list, opts...)
}

// A NotRecent written over an older reason lands with its answer lost, and the cache still shows the older
// reason on the next pass. That pass keeps the pending mark, so the first pass that reads NotRecent reports
// the skip, once (#983).
func TestLostNotRecentReadLateByALaggingCacheIsReportedOnce(t *testing.T) {
	ns := namespace(t)
	createIncident(t, ns, 1, withDueCondition(incidentStatus(35*time.Minute, 35*time.Minute, 0),
		metav1.ConditionFalse, ReasonDebouncing))
	createSettings(t, 2)
	createBudget(t, ns, nil)
	before := testutil.ToFloat64(notRecentSkips)
	var lost atomic.Int32
	rec := corerecord.NewFakeRecorder(20)
	passes := &markCounter{Reader: env.admin}
	lag := &laggingCache{markCounter: passes}
	cfg := triggerConfig
	cfg.Recorder = rec
	startWith(t, ns, cfg, func(c *Controller, w *countingClient) {
		c.cache = lag
		w.mark = func(obj client.Object) error {
			inc := obj.(*remediationv1alpha1.Incident)
			c := meta.FindStatusCondition(inc.Status.Conditions, ConditionAnalysisDue)
			if c == nil || c.Reason != ReasonNotRecent || lost.Add(1) != 1 {
				return nil
			}
			// The next pass lists the Incidents as they are before this write.
			var snap remediationv1alpha1.IncidentList
			if err := env.admin.List(context.Background(), &snap, client.InNamespace(ns)); err != nil {
				return err
			}
			lag.mu.Lock()
			lag.snapshot, lag.until = &snap, passes.gets.Load()+1
			lag.mu.Unlock()
			// The update lands, its answer does not.
			if err := env.admin.Status().Update(context.Background(), obj); err != nil {
				return err
			}
			return context.DeadlineExceeded
		}
	})
	eventually(t, ns, 1, dueIs(false, ReasonNotRecent))
	waitSkipEvent(t, rec)
	passes.afterPasses(t, 3)
	noEvent(t, rec, "a second event for one skip")
	if lost.Load() != 1 {
		t.Fatalf("%d NotRecent writes, want the one whose answer was lost", lost.Load())
	}
	if got := skipsSince(before); got != 1 {
		t.Fatalf("skips counted %v, want 1", got)
	}
}

// failingIncidentReads fails every live read of an Incident once fail is set.
type failingIncidentReads struct {
	client.Reader
	fail atomic.Bool
}

func (f *failingIncidentReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.Incident); ok && f.fail.Load() {
		return errors.New("the API server cannot be reached")
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// A NotRecent write refused with a Conflict sent nothing that landed; when the read of the next attempt fails,
// the write is not remembered as pending, so no later NotRecent read is reported for it.
func TestConflictThenFailedReadLeavesNoPendingSkip(t *testing.T) {
	ns := namespace(t)
	createIncident(t, ns, 1, withDueCondition(incidentStatus(35*time.Minute, 35*time.Minute, 0),
		metav1.ConditionFalse, ReasonDebouncing))
	createSettings(t, 2)
	createBudget(t, ns, nil)
	reads := &failingIncidentReads{Reader: env.admin}
	var conflicts atomic.Int32
	passes := &markCounter{Reader: env.admin}
	r := startWith(t, ns, triggerConfig, func(c *Controller, w *countingClient) {
		c.cache, c.reader = passes, reads
		w.mark = func(obj client.Object) error {
			inc := obj.(*remediationv1alpha1.Incident)
			c := meta.FindStatusCondition(inc.Status.Conditions, ConditionAnalysisDue)
			if c == nil || c.Reason != ReasonNotRecent || conflicts.Add(1) != 1 {
				return nil
			}
			reads.fail.Store(true)
			return apierrors.NewConflict(schema.GroupResource{Group: remediationv1alpha1.GroupVersion.Group,
				Resource: "incidents"}, inc.Name, errors.New("the object has been modified"))
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for conflicts.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no NotRecent write")
		}
		time.Sleep(testTriggerInterval / 4)
	}
	passes.afterPasses(t, 1)
	r.stop() // the memory belongs to the trigger goroutine
	inc := &remediationv1alpha1.Incident{}
	if err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: Name(fp(1))}, inc); err != nil {
		t.Fatal(err)
	}
	tr := r.memory[inc.UID]
	if tr == nil {
		t.Fatal("the Incident is not remembered")
	}
	if tr.skipPending {
		t.Fatal("a NotRecent write that sent nothing is remembered as pending")
	}
	if conflicts.Load() != 1 {
		t.Fatalf("%d NotRecent writes sent, want the one refused", conflicts.Load())
	}
}
