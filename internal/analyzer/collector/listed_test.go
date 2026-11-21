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

package collector

import (
	"testing"
	"time"

	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// failedMount returns a kubelet FailedMount event about the web pod, with 14 occurrences, created 20 minutes
// before the collector starts, as the API server stores it (with a resource version).
func failedMount(h *harness) *eventsv1.Event {
	h.t.Helper()
	ev := warning("mount", 14)
	ev.Reason, ev.Action, ev.Note = "FailedMount", "", "MountVolume.SetUp failed for volume config"
	ev.CreationTimestamp = metav1.NewTime(t0.Add(-20 * time.Minute))
	ev.EventTime = metav1.NewMicroTime(t0.Add(-20 * time.Minute))
	ev.Series.LastObservedTime = metav1.NewMicroTime(t0.Add(-15 * time.Minute))
	h.create(ev)
	stored := &eventsv1.Event{}
	if err := h.cl.Get(h.t.Context(), client.ObjectKeyFromObject(ev), stored); err != nil {
		h.t.Fatal(err)
	}
	return stored
}

// listAtStart hands ev to the event controller's predicate as the informer's initial list does.
func listAtStart(h *harness, ev *eventsv1.Event) {
	h.t.Helper()
	if !h.c.eventPredicate().Create(event.CreateEvent{Object: ev, IsInInitialList: true}) {
		h.t.Fatal("the predicate dropped a Warning event of the initial list")
	}
}

// An event the start listed, about a pod that is gone, is marked FoldOnly: it opens no Incident (#996).
func TestEventOfAGonePodListedAtStartIsFoldOnly(t *testing.T) {
	h := newHarness(t, Config{})
	ev := failedMount(h)
	listAtStart(h, ev)
	h.reconcileEvent(ev.Name)
	got := h.one()
	if !got.FoldOnly {
		t.Fatalf("event of a gone pod listed at start: %+v, want FoldOnly", got)
	}
	if !got.MayRepeat || got.Total != 14 || got.Occurrence != "event:"+string(ev.UID) {
		t.Fatalf("event of a gone pod listed at start: %+v, want MayRepeat with Total 14 and its UID", got)
	}
}

// The same event about a pod that exists is handled as before: its subject is the workload, and it is not
// FoldOnly.
func TestEventOfAPresentPodListedAtStartIsAsBefore(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, pod)
	ev := failedMount(h)
	listAtStart(h, ev)
	h.reconcileEvent(ev.Name)
	got := h.one()
	if got.FoldOnly || got.Subject.Kind != kindDeployment || string(got.Subject.UID) != string(deployUID) {
		t.Fatalf("event of a present pod: %+v, want the Deployment as its subject and not FoldOnly", got)
	}
}

// An event of a gone pod that changed after the start (a new occurrence), or that the start did not list, is
// not FoldOnly: it is a sighting of this collector.
func TestEventOfAGonePodNotUnchangedSinceStartIsNotFoldOnly(t *testing.T) {
	h := newHarness(t, Config{})
	ev := failedMount(h)
	listAtStart(h, ev)
	ev.Series.Count = 15
	h.update(ev)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.FoldOnly {
		t.Fatalf("event changed after the start: %+v, want not FoldOnly", got)
	}
	if n := h.c.listed.len(); n != 0 {
		t.Fatalf("%d listed events remembered, want the changed one forgotten", n)
	}

	h = newHarness(t, Config{})
	ev = failedMount(h)
	if !h.c.eventPredicate().Create(event.CreateEvent{Object: ev}) {
		t.Fatal("the predicate dropped a Warning event")
	}
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.FoldOnly {
		t.Fatalf("event created after the start's list: %+v, want not FoldOnly", got)
	}
}

// A deleted event is forgotten.
func TestDeletedListedEventIsForgotten(t *testing.T) {
	h := newHarness(t, Config{})
	ev := failedMount(h)
	listAtStart(h, ev)
	if err := h.cl.Delete(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	h.reconcileEvent(ev.Name)
	if n := h.c.listed.len(); n != 0 {
		t.Fatalf("%d listed events remembered after the delete, want 0", n)
	}
}
