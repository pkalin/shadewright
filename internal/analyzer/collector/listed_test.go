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

	"github.com/prometheus/client_golang/prometheus/testutil"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// reasonFailedMount is the kubelet reason of a volume that fails to mount.
const reasonFailedMount = "FailedMount"

// failedMount returns a kubelet FailedMount event about the web pod, with 14 occurrences, created 20 minutes
// before the collector starts, as the API server stores it (with a resource version).
func failedMount(h *harness) *eventsv1.Event {
	h.t.Helper()
	ev := warning("mount", 14)
	ev.Reason, ev.Action, ev.Note = reasonFailedMount, "", "MountVolume.SetUp failed for volume config"
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
	before := testutil.ToFloat64(staleOnStartFolded)
	h.reconcileEvent(ev.Name)
	got := h.one()
	if !got.FoldOnly {
		t.Fatalf("event of a gone pod listed at start: %+v, want FoldOnly", got)
	}
	if n := testutil.ToFloat64(staleOnStartFolded) - before; n != 1 {
		t.Fatalf("stale_on_start_folded_total rose by %v, want 1", n)
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

// The gap case (#996, decision of 2026-11-21): while no collector ran (a restart or a leader handover), the
// event's count rose and its pod was deleted. The new collector lists it at start, unchanged since, about a
// gone pod, but its last occurrence is within StaleOnStart of the start: no collector handled those
// occurrences, so it counts as before and is not FoldOnly.
func TestRecentEventOfAGonePodListedAtStartIsNotFoldOnly(t *testing.T) {
	for _, last := range []time.Duration{-time.Minute, -StaleOnStart} {
		h := newHarness(t, Config{})
		ev := failedMount(h)
		// Raised while no collector ran: the stored event the start lists.
		ev.Series.Count = 16
		ev.Series.LastObservedTime = metav1.NewMicroTime(t0.Add(last))
		h.update(ev)
		stored := &eventsv1.Event{}
		if err := h.cl.Get(t.Context(), client.ObjectKeyFromObject(ev), stored); err != nil {
			t.Fatal(err)
		}
		listAtStart(h, stored)
		before := testutil.ToFloat64(staleOnStartFolded)
		h.reconcileEvent(ev.Name)
		got := h.one()
		if got.FoldOnly || got.Total != 16 {
			t.Fatalf("last occurrence %v before the start: %+v, want not FoldOnly with Total 16", -last, got)
		}
		if n := testutil.ToFloat64(staleOnStartFolded) - before; n != 0 {
			t.Fatalf("stale_on_start_folded_total rose by %v, want 0", n)
		}
	}
}

// The last occurrence is the series' last observed time, else the deprecated last timestamp, else the event
// time: an old event time with a recent deprecated last timestamp is recent.
func TestDeprecatedLastTimestampOfAListedEventIsItsLastOccurrence(t *testing.T) {
	h := newHarness(t, Config{})
	ev := warning("mount", 0)
	ev.Reason, ev.Action = reasonFailedMount, ""
	ev.EventTime = metav1.NewMicroTime(t0.Add(-time.Hour))
	ev.DeprecatedLastTimestamp = metav1.NewTime(t0.Add(-time.Minute))
	ev.DeprecatedCount = 3
	h.create(ev)
	stored := &eventsv1.Event{}
	if err := h.cl.Get(t.Context(), client.ObjectKeyFromObject(ev), stored); err != nil {
		t.Fatal(err)
	}
	listAtStart(h, stored)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.FoldOnly {
		t.Fatalf("event with a recent deprecated last timestamp: %+v, want not FoldOnly", got)
	}
}

// An event with no usable time (no series, no deprecated last timestamp, no event time) counts as before: its
// creation timestamp is the API server's and does not decide.
func TestListedEventOfAGonePodWithNoTimeIsNotFoldOnly(t *testing.T) {
	h := newHarness(t, Config{})
	ev := warning("mount", 0)
	ev.Reason, ev.Action = reasonFailedMount, ""
	ev.CreationTimestamp = metav1.NewTime(t0.Add(-time.Hour))
	ev.EventTime = metav1.MicroTime{}
	h.create(ev)
	stored := &eventsv1.Event{}
	if err := h.cl.Get(t.Context(), client.ObjectKeyFromObject(ev), stored); err != nil {
		t.Fatal(err)
	}
	listAtStart(h, stored)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.FoldOnly {
		t.Fatalf("event with no usable time: %+v, want not FoldOnly", got)
	}
}
