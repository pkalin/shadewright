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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// deletedWindow is the aggregation window of the tests here. The writer's tick is a tenth of it in real time,
// while what is due follows the test's clock, so a batch counted within the window waits until the test
// moves the clock past it.
const deletedWindow = 2 * time.Second

// openThenHoldOne opens the Incident of fp(1) with occurrence 1 and counts occurrence 2 within the window,
// so it waits unwritten in the pending batch, as the kill of #893 waited in the lab. It returns the running
// controller.
func openThenHoldOne(t *testing.T, ns string, clk *clock, setup func(*Controller, *countingClient)) *running {
	t.Helper()
	r := startWith(t, ns, Config{Window: deletedWindow, Now: clk.now}, setup)
	r.obs <- immediate(seenAt(1, triggerNow))
	named(t, ns, Name(fp(1)), countIs(1))
	idle(t, r)
	clk.advance(time.Second)
	r.obs <- immediate(seenAt(2, triggerNow.Add(time.Second)))
	pollUntil(t, "occurrence 2 held unwritten", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.pending[fp(1)] != nil
	})
	return r
}

// deleteIncident deletes the Incident called name.
func deleteIncident(t *testing.T, ns, name string) {
	t.Helper()
	if err := env.admin.Delete(t.Context(), &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}); err != nil {
		t.Fatal(err)
	}
}

// #893: a first occurrence deleted while an observation counted before the delete waits unwritten is not
// created again from that observation when its batch comes due. The batch is dropped and counted (reason
// "deleted"), and the fingerprint has no Incident from then on.
func TestDeletedFirstOccurrenceIsNotRecreatedFromItsPendingBatch(t *testing.T) {
	ns := namespace(t)
	clk := &clock{}
	r := openThenHoldOne(t, ns, clk, nil)
	deleteIncident(t, ns, Name(fp(1)))
	before := testutil.ToFloat64(droppedBatches.WithLabelValues(dropDeleted))
	clk.advance(2 * deletedWindow)
	pollUntil(t, "the pending batch written or dropped", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.pending[fp(1)] == nil && r.failed[fp(1)] == nil && r.inflight[fp(1)] == nil
	})
	absent(t, ns, Name(fp(1)))
	if got := testutil.ToFloat64(droppedBatches.WithLabelValues(dropDeleted)) - before; got != 1 {
		t.Fatalf("%v batches dropped as deleted, want 1", got)
	}
	if got := r.writes.creates.Load(); got != 1 {
		t.Fatalf("%d creates, want only the first", got)
	}
	r.mu.Lock()
	_, kept := r.names[Name(fp(1))]
	r.mu.Unlock()
	if kept {
		t.Fatal("the deleted Incident still takes a place under the cap")
	}
}

// #893 and #901: once the delete is found at the write, a kill counted after it opens the Incident again at
// once, as for a fingerprint never seen, not a window later; and the new Incident holds only that kill.
func TestKillAfterTheDeleteOpensAtOnce(t *testing.T) {
	ns := namespace(t)
	clk := &clock{}
	r := openThenHoldOne(t, ns, clk, nil)
	deleteIncident(t, ns, Name(fp(1)))
	clk.advance(2 * deletedWindow)
	pollUntil(t, "the pending batch dropped", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, kept := r.names[Name(fp(1))]
		return !kept && r.pending[fp(1)] == nil && r.inflight[fp(1)] == nil
	})
	// The clock does not move from here on: a write held for the window would never come due.
	r.obs <- immediate(seenAt(3, clk.now()))
	inc := named(t, ns, Name(fp(1)), countIs(1))
	if ids := occurrenceIDs(inc); len(ids) != 1 || ids[0] != eventID(3) {
		t.Fatalf("occurrences %v, want only %s", ids, eventID(3))
	}
}

// occurrenceIDs returns the IDs of inc's own occurrence records.
func occurrenceIDs(inc *remediationv1alpha1.Incident) []string {
	ids := make([]string, 0, len(inc.Status.Occurrences))
	for _, r := range inc.Status.Occurrences {
		ids = append(ids, r.ID)
	}
	return ids
}

// incidentCache starts an informer cache of the Incidents in ns on the test API server, as the manager's cache
// is in production, and returns it once synced.
func incidentCache(t *testing.T, ns string) cache.Cache {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ca, err := cache.New(env.testEnv.Config, cache.Options{Scheme: scheme,
		DefaultNamespaces: map[string]cache.Config{ns: {}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = ca.Start(ctx) }()
	if _, err := ca.GetInformer(t.Context(), &remediationv1alpha1.Incident{}); err != nil {
		t.Fatal(err)
	}
	if !ca.WaitForCacheSync(t.Context()) {
		t.Fatal("the Incident cache did not sync")
	}
	return ca
}

// withInformers is a setup for startWith that gives the controller the informers of ca.
func withInformers(ca cache.Cache) func(*Controller, *countingClient) {
	return func(c *Controller, _ *countingClient) { c.informers = ca }
}

// #893 with the delete seen as it happens: the observations counted before it are dropped at once, without a
// write, and a kill counted after it opens the Incident again at once with only that kill, though the clock
// never reaches the end of the window of the dropped batch.
func TestDeleteSeenByTheInformerDropsWhatWasCountedBefore(t *testing.T) {
	ns := namespace(t)
	clk := &clock{}
	r := openThenHoldOne(t, ns, clk, withInformers(incidentCache(t, ns)))
	before := testutil.ToFloat64(droppedBatches.WithLabelValues(dropDeleted))
	deleteIncident(t, ns, Name(fp(1)))
	pollUntil(t, "the delete seen", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, kept := r.names[Name(fp(1))]
		return !kept && r.pending[fp(1)] == nil
	})
	if got := testutil.ToFloat64(droppedBatches.WithLabelValues(dropDeleted)) - before; got != 1 {
		t.Fatalf("%v batches dropped as deleted, want 1", got)
	}
	r.obs <- immediate(seenAt(3, clk.now()))
	inc := named(t, ns, Name(fp(1)), countIs(1))
	if ids := occurrenceIDs(inc); len(ids) != 1 || ids[0] != eventID(3) {
		t.Fatalf("occurrences %v, want only %s", ids, eventID(3))
	}
	if got := r.writes.creates.Load(); got != 2 {
		t.Fatalf("%d creates, want 2", got)
	}
}

// An occurrence the deleted Incident counted, seen again with more to count after the delete (an event series
// that goes on), opens nothing: only an occurrence the deleted Incident never counted does.
func TestOccurrenceOfTheDeletedIncidentOpensNothing(t *testing.T) {
	ns := namespace(t)
	clk := &clock{}
	r := openThenHoldOne(t, ns, clk, withInformers(incidentCache(t, ns)))
	deleteIncident(t, ns, Name(fp(1)))
	pollUntil(t, "the delete seen", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, kept := r.names[Name(fp(1))]
		return !kept
	})
	// Occurrence 1 was written before the delete, occurrence 2 dropped with it: both go on, with a total that
	// proves on its own that the symptom recurs.
	r.obs <- immediate(observation(1, 1, 5, 4, triggerNow))
	r.obs <- immediate(observation(1, 2, 5, 4, triggerNow.Add(time.Second)))
	time.Sleep(500 * time.Millisecond)
	absent(t, ns, Name(fp(1)))
	if got := r.logs.with("Dropped an observation of an occurrence of a deleted Incident"); len(got) != 2 {
		t.Fatalf("log lines %q, want one per observation", got)
	}
	r.obs <- immediate(seenAt(3, clk.now()))
	inc := named(t, ns, Name(fp(1)), countIs(1))
	if ids := occurrenceIDs(inc); len(ids) != 1 || ids[0] != eventID(3) {
		t.Fatalf("occurrences %v, want only %s", ids, eventID(3))
	}
}

// An Incident deleted between the read of a write and its status update is not created again by the retry of
// that write: the next attempt reads it again, finds it gone and drops the batch.
func TestDeleteBetweenTheReadAndTheUpdateIsNotUndone(t *testing.T) {
	ns := namespace(t)
	clk := &clock{}
	var once sync.Once
	r := openThenHoldOne(t, ns, clk, func(_ *Controller, w *countingClient) {
		w.before = func(ctx context.Context, inc *remediationv1alpha1.Incident) {
			if inc.Status.Count < 2 {
				return
			}
			once.Do(func() {
				if err := env.admin.Delete(ctx, &remediationv1alpha1.Incident{
					ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: inc.Name}}); err != nil {
					t.Error(err)
				}
			})
		}
	})
	before := testutil.ToFloat64(droppedBatches.WithLabelValues(dropDeleted))
	clk.advance(2 * deletedWindow)
	pollUntil(t, "the batch dropped", func() bool {
		return testutil.ToFloat64(droppedBatches.WithLabelValues(dropDeleted))-before >= 1
	})
	time.Sleep(500 * time.Millisecond)
	absent(t, ns, Name(fp(1)))
	if got := r.writes.creates.Load(); got != 1 {
		t.Fatalf("%d creates, want only the first", got)
	}
}

// The informers report the delete while a write of what was counted before it is in flight, and a kill counted
// after the delete opens the Incident again: the write in flight finds it gone and drops only what it carried,
// so the new Incident holds only the new kill.
func TestDeleteSeenWhileAWriteIsInFlight(t *testing.T) {
	ns := namespace(t)
	clk := &clock{}
	ca := incidentCache(t, ns)
	release, blocked := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r := openThenHoldOne(t, ns, clk, func(c *Controller, w *countingClient) {
		c.informers = ca
		w.before = func(_ context.Context, inc *remediationv1alpha1.Incident) {
			if inc.Status.Count >= 2 {
				once.Do(func() { close(blocked) })
				<-release
			}
		}
	})
	clk.advance(2 * deletedWindow)
	<-blocked
	deleteIncident(t, ns, Name(fp(1)))
	pollUntil(t, "the delete seen", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, kept := r.names[Name(fp(1))]
		return !kept
	})
	r.obs <- immediate(seenAt(3, clk.now()))
	pollUntil(t, "the new kill counted", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, opening := r.opening[Name(fp(1))]
		return opening
	})
	close(release)
	inc := named(t, ns, Name(fp(1)), countIs(1))
	if ids := occurrenceIDs(inc); len(ids) != 1 || ids[0] != eventID(3) {
		t.Fatalf("occurrences %v, want only %s", ids, eventID(3))
	}
	time.Sleep(500 * time.Millisecond)
	if inc := named(t, ns, Name(fp(1)), func(*remediationv1alpha1.Incident) bool { return true }); inc.Status.Count != 1 {
		t.Fatalf("count %d after the write in flight returned, want 1", inc.Status.Count)
	}
}

// within polls cond until it holds or 20 s pass, and reports whether it held. Unlike pollUntil it never stops
// the test, so a hook of the client, run off the test's goroutine, can wait with it.
func within(cond func() bool) bool {
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// A status write of what was counted before the delete succeeds, and before the controller takes its answer
// the informers report the delete and a kill counted after it starts to open the Incident again. The answer
// of the write is about the deleted object: it neither clears the new opening nor makes the old object the
// one remembered, so the new kill creates the Incident, holding only that kill, and is not dropped as one of
// the deleted Incident (decision of 2026-11-19 on #893).
func TestWriteThatSucceededBeforeTheDeleteKeepsTheNewOpening(t *testing.T) {
	ns := namespace(t)
	clk := &clock{}
	ca := incidentCache(t, ns)
	var r *running
	var carriesTwo, ready atomic.Bool
	var once sync.Once
	hooked := make(chan struct{})
	r = openThenHoldOne(t, ns, clk, func(c *Controller, w *countingClient) {
		c.informers = ca
		w.before = func(_ context.Context, inc *remediationv1alpha1.Incident) {
			carriesTwo.Store(inc.Status.Count >= 2)
		}
		w.after = func(err error) error {
			if err != nil || !ready.Load() || !carriesTwo.Load() {
				return err
			}
			once.Do(func() {
				defer close(hooked)
				if err := env.admin.Delete(context.Background(), &remediationv1alpha1.Incident{
					ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: Name(fp(1))}}); err != nil {
					t.Error(err)
					return
				}
				if !within(func() bool {
					r.mu.Lock()
					defer r.mu.Unlock()
					_, kept := r.names[Name(fp(1))]
					return !kept
				}) {
					t.Error("the informers did not report the delete")
					return
				}
				select {
				case r.obs <- immediate(seenAt(3, clk.now())):
				case <-time.After(20 * time.Second):
					t.Error("the kill after the delete was not taken")
					return
				}
				if !within(func() bool {
					r.mu.Lock()
					defer r.mu.Unlock()
					_, opening := r.opening[Name(fp(1))]
					return opening
				}) {
					t.Error("the kill after the delete did not start to open the Incident")
				}
			})
			return err
		}
	})
	ready.Store(true)
	clk.advance(2 * deletedWindow)
	select {
	case <-hooked:
	case <-time.After(30 * time.Second):
		t.Fatal("the write of occurrence 2 did not succeed")
	}
	inc := named(t, ns, Name(fp(1)), countIs(1))
	if ids := occurrenceIDs(inc); len(ids) != 1 || ids[0] != eventID(3) {
		t.Fatalf("occurrences %v, want only %s", ids, eventID(3))
	}
	r.mu.Lock()
	dropped := r.deletedOccurrences.has(eventID(3))
	r.mu.Unlock()
	if dropped {
		t.Fatalf("%s is remembered as an occurrence of the deleted Incident", eventID(3))
	}
}

// The known limit of decision 2 of 2026-11-19 on #893: an occurrence counted after the delete but before the
// informers report it (here a late tombstone) cannot be told from one counted before the delete, and is
// dropped with them: it opens nothing. The next new occurrence of the same symptom opens the Incident, holding
// only itself, and is not blocked by the memory of the deleted Incident's occurrences.
func TestOccurrenceBeforeTheDeleteIsReportedIsDroppedAndTheNextOpens(t *testing.T) {
	ns := namespace(t)
	clk := &clock{}
	r := openThenHoldOne(t, ns, clk, nil)
	old := named(t, ns, Name(fp(1)), countIs(1))
	deleteIncident(t, ns, Name(fp(1)))
	// Counted while the controller still knows the Incident: it joins the pending batch of occurrence 2, which
	// waits for the window the clock does not reach.
	r.obs <- immediate(seenAt(3, clk.now()))
	pollUntil(t, "occurrence 3 held unwritten", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, held := r.pending[fp(1)].occurrenceOf(eventID(3))
		return held
	})
	r.incidentDeleted(t.Context(), toolscache.DeletedFinalStateUnknown{Key: ns + "/" + old.Name, Obj: old})
	time.Sleep(500 * time.Millisecond)
	absent(t, ns, Name(fp(1)))
	r.mu.Lock()
	dropped := r.deletedOccurrences.has(eventID(3)) && r.pending[fp(1)] == nil
	r.mu.Unlock()
	if !dropped {
		t.Fatalf("%s was not dropped with the deleted Incident", eventID(3))
	}
	r.obs <- immediate(seenAt(4, clk.now()))
	inc := named(t, ns, Name(fp(1)), countIs(1))
	if ids := occurrenceIDs(inc); len(ids) != 1 || ids[0] != eventID(4) {
		t.Fatalf("occurrences %v, want only %s", ids, eventID(4))
	}
}

// deleteDuringUpdate returns an after hook of countingClient that, once a status update of the Incident name
// succeeds, deletes it from cl and reports the delete to c as the informers would, before the caller of the
// update takes its answer.
func deleteDuringUpdate(t *testing.T, c *Controller, cl client.Client, name string) func(error) error {
	var once sync.Once
	return func(err error) error {
		if err != nil {
			return err
		}
		once.Do(func() {
			inc := &remediationv1alpha1.Incident{}
			if err := cl.Get(t.Context(), client.ObjectKey{Namespace: fakeNamespace, Name: name}, inc); err != nil {
				t.Error(err)
				return
			}
			if err := cl.Delete(t.Context(), inc); err != nil {
				t.Error(err)
				return
			}
			c.incidentDeleted(t.Context(), inc)
			c.mu.Lock()
			_, kept := c.names[name]
			c.mu.Unlock()
			if kept {
				t.Error("the delete was not handled")
			}
		})
		return err
	}
}

// A closure move whose write succeeded before the Incident was deleted does not remember the deleted object
// Resolved once the delete was handled: the next observation of the symptom must open occurrence 1 again, not
// roll on from a resolvedTime that no stored object holds.
func TestClosureMoveOfADeletedIncidentIsNotRemembered(t *testing.T) {
	c, cl := closureController(t, closureIncident(remediationv1alpha1.IncidentOpen, 2*time.Hour))
	w := &countingClient{Client: cl}
	w.after = deleteDuringUpdate(t, c, cl, Name(fp(1)))
	c.client = w
	if err := c.closeIncidents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w.updates.Load() != 1 {
		t.Fatalf("%d status writes, want 1", w.updates.Load())
	}
	c.mu.Lock()
	_, remembered := c.resolvedAt[Name(fp(1))]
	c.mu.Unlock()
	if remembered {
		t.Fatal("the deleted Incident is remembered Resolved")
	}
}

// A fold whose write succeeded before the consequence Incident was deleted does not remember the deleted
// object Resolved once the delete was handled.
func TestFoldOfADeletedIncidentIsNotRemembered(t *testing.T) {
	c, cl, w := foldController(t, []*remediationv1alpha1.Incident{foldConsequence(5 * time.Minute)})
	w.after = deleteDuringUpdate(t, c, cl, Name(fp(2)))
	if err := c.foldInto(t.Context(), Name(fp(1))); err != nil {
		t.Fatal(err)
	}
	if w.updates.Load() != 1 {
		t.Fatalf("%d status writes, want 1", w.updates.Load())
	}
	c.mu.Lock()
	_, remembered := c.resolvedAt[Name(fp(2))]
	c.mu.Unlock()
	if remembered {
		t.Fatal("the deleted Incident is remembered Resolved")
	}
}
