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
	"testing"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeNamespace is the operator namespace of fakeController.
const fakeNamespace = "operator"

// fakeController is a controller on a fake API server holding occurrence 1 of fp(1), Resolved 25 hours
// before triggerNow, loaded.
func fakeController(t *testing.T) (*Controller, client.Client, *remediationv1alpha1.Incident) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	old := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: Name(fp(1)), Namespace: fakeNamespace},
		Spec:       firstSpec(), Status: resolvedStatus(25 * time.Hour),
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&remediationv1alpha1.Incident{}).WithObjects(old).Build()
	c, err := New(Config{Namespace: fakeNamespace, Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	c.client, c.reader, c.cache = cl, cl, cl
	if err := c.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c, cl, old
}

// A trigger pass that read the Incident Resolved before a write escalated it must not let a later sighting
// open the next occurrence: opening reads the predecessor live and sends the observations back to it.
func TestStaleResolvedSnapshotOpensNoOccurrence(t *testing.T) {
	c, cl, old := fakeController(t)
	// A trigger pass has listed old, but has not called noteStates yet.
	stale := *old.DeepCopy()
	// An observation write escalates the resolved incident in the meantime.
	c.consume(t.Context(), immediate(seenAt(100, triggerNow.Add(-24*time.Hour))))
	c.flush(t.Context(), fp(1))
	got := &remediationv1alpha1.Incident{}
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(old), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != remediationv1alpha1.IncidentEscalated {
		t.Fatalf("setup: %s", got.Status.Phase)
	}
	// The delayed pass overwrites the newer state from the successful write.
	c.noteStates([]remediationv1alpha1.Incident{stale})
	c.consume(t.Context(), immediate(seenAt(101, triggerNow)))
	c.flush(t.Context(), fp(1))
	err := cl.Get(t.Context(), client.ObjectKey{Namespace: fakeNamespace, Name: second}, got)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Escalated incident opened occurrence 2: phase=%s, error=%v", got.Status.Phase, err)
	}
	// The observation goes to the Escalated Incident instead.
	c.flush(t.Context(), fp(1))
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(old), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != remediationv1alpha1.IncidentEscalated || got.Status.Count != 3 {
		t.Fatalf("the earlier occurrence has phase %s count %d, want Escalated and 3", got.Status.Phase, got.Status.Count)
	}
}

// Once the create of the next occurrence succeeded, a retry of its status write only updates: the
// occurrence deleted in between is dropped as deleted, never created again.
func TestOccurrenceDeletedBeforeItsRetryIsNotRecreated(t *testing.T) {
	c, cl, _ := fakeController(t)
	w := &countingClient{Client: cl}
	w.instead = func(_ context.Context) error { return apierrors.NewServiceUnavailable("unavailable") }
	c.client = w
	c.consume(t.Context(), immediate(seenAt(100, triggerNow)))
	c.flush(t.Context(), fp(1))
	got := &remediationv1alpha1.Incident{}
	key := client.ObjectKey{Namespace: fakeNamespace, Name: second}
	if err := cl.Get(t.Context(), key, got); err != nil {
		t.Fatal(err)
	}
	if err := cl.Delete(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	w.instead = nil
	c.flush(t.Context(), fp(1))
	err := cl.Get(t.Context(), key, got)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("deleted occurrence re-created by retry: phase=%s, error=%v", got.Status.Phase, err)
	}
	if c.nameOf(fp(1)) != Name(fp(1)) {
		t.Fatalf("latest occurrence %s, want %s", c.nameOf(fp(1)), Name(fp(1)))
	}
}

// The occurrence that deletion handling adopts as the latest takes its place under the cap.
func TestAdoptedLatestOccurrenceCountsUnderTheCap(t *testing.T) {
	_, cl, _ := fakeController(t)
	secondInc := &remediationv1alpha1.Incident{ObjectMeta: metav1.ObjectMeta{Name: second, Namespace: fakeNamespace}, Spec: nextSpec(firstSpec(), 2), Status: resolvedStatus(25 * time.Hour)}
	if err := cl.Create(t.Context(), secondInc); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Namespace: fakeNamespace, MaxIncidents: 2, Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	c.client, c.reader, c.cache = cl, cl, cl
	if err := c.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := cl.Delete(t.Context(), secondInc); err != nil {
		t.Fatal(err)
	}
	thirdInc := &remediationv1alpha1.Incident{ObjectMeta: metav1.ObjectMeta{Name: third, Namespace: fakeNamespace}, Spec: nextSpec(firstSpec(), 3)}
	if err := cl.Create(t.Context(), thirdInc); err != nil {
		t.Fatal(err)
	}
	// A late observation targets deleted occurrence 2 and discovers occurrence 3.
	c.consume(t.Context(), seenAt(100, triggerNow.Add(-24*time.Hour)))
	c.flush(t.Context(), fp(1))
	if c.nameOf(fp(1)) != third {
		t.Fatalf("setup did not adopt occurrence 3")
	}
	c.consume(t.Context(), immediate(observation(2, 101, 1, 1, triggerNow)))
	c.flush(t.Context(), fp(2))
	var list remediationv1alpha1.IncidentList
	if err := cl.List(t.Context(), &list, client.InNamespace(fakeNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) > 2 {
		t.Fatalf("cap 2 exceeded: %d objects, %d reserved names", len(list.Items), len(c.names))
	}
}

// An occurrence write that read the Incident before the revert proposer added an entry to
// status.revertsProposed gets a conflict and is done again from a fresh read: the entry stays and the
// observation is counted.
func TestOccurrenceWriteKeepsRevertsProposed(t *testing.T) {
	c, cl, old := fakeController(t)
	entry := remediationv1alpha1.ProposedRevert{RequestRef: remediationv1alpha1.RequestRef{Name: "remediation-a",
		UID: "27272727-2727-4727-8727-272727272727"}, State: remediationv1alpha1.RevertPending}
	w := &countingClient{Client: cl}
	raced := false
	w.before = func(ctx context.Context, _ *remediationv1alpha1.Incident) {
		if raced {
			return
		}
		raced = true
		live := &remediationv1alpha1.Incident{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(old), live); err != nil {
			t.Fatal(err)
		}
		live.Status.RevertsProposed = append(live.Status.RevertsProposed, entry)
		if err := cl.Status().Update(ctx, live); err != nil {
			t.Fatal(err)
		}
	}
	c.client = w
	c.consume(t.Context(), immediate(seenAt(100, triggerNow.Add(-24*time.Hour))))
	c.flush(t.Context(), fp(1))
	got := &remediationv1alpha1.Incident{}
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(old), got); err != nil {
		t.Fatal(err)
	}
	if !raced || w.updates.Load() < 2 {
		t.Fatalf("setup: raced %v, %d updates, want a refused write and its retry", raced, w.updates.Load())
	}
	if len(got.Status.RevertsProposed) != 1 || got.Status.RevertsProposed[0] != entry {
		t.Errorf("status.revertsProposed = %+v, want the proposer's entry", got.Status.RevertsProposed)
	}
	if got.Status.Phase != remediationv1alpha1.IncidentEscalated {
		t.Errorf("phase %s, want Escalated: the observation was not written", got.Status.Phase)
	}
}
