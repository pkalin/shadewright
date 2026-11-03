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
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

func TestNewNeedsAClusterID(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted an empty cluster ID")
	}
}

func TestSeriesCountsOnlyTheIncrease(t *testing.T) {
	h := newHarness(t, Config{})
	ev := warning("backoff", 3)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.Count != 3 {
		t.Fatalf("first sight of a series of 3: Count = %d, want 3", got.Count)
	}

	ev.Series.Count = 7
	h.update(ev)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.Count != 4 {
		t.Fatalf("series 3 to 7: Count = %d, want 4", got.Count)
	}

	// The same event again (a resync or an unrelated update) and a count that goes down add nothing.
	h.reconcileEvent(ev.Name)
	h.none()
	ev.Series.Count = 5
	h.update(ev)
	h.reconcileEvent(ev.Name)
	h.none()
}

func TestOccurrences(t *testing.T) {
	cases := []struct {
		name string
		ev   eventsv1.Event
		want int64
	}{
		{"no series counts once", eventsv1.Event{}, 1},
		{"series count", eventsv1.Event{Series: &eventsv1.EventSeries{Count: 9}}, 9},
		{"series count wins over the deprecated one", eventsv1.Event{Series: &eventsv1.EventSeries{Count: 2}, DeprecatedCount: 50}, 2},
		{"core/v1 recorder count", eventsv1.Event{DeprecatedCount: 4}, 4},
		{"negative series count counts once", eventsv1.Event{Series: &eventsv1.EventSeries{Count: -3}}, 1},
	}
	for _, c := range cases {
		if got := occurrences(&c.ev); got != c.want {
			t.Errorf("%s: occurrences = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestEventWithoutSeriesCountsOnce(t *testing.T) {
	h := newHarness(t, Config{})
	ev := warning("failed", 0)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.Count != 1 {
		t.Fatalf("Count = %d, want 1", got.Count)
	}
	ev.Note = "another note"
	h.update(ev)
	h.reconcileEvent(ev.Name)
	h.none()
	// When the recorder turns it into a series, the series count includes the first occurrence.
	ev.Series = &eventsv1.EventSeries{Count: 2, LastObservedTime: metav1.NewMicroTime(t0)}
	h.update(ev)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.Count != 1 {
		t.Fatalf("series of 2 after a single event: Count = %d, want 1", got.Count)
	}
}

func TestRecreatedEventIsNew(t *testing.T) {
	h := newHarness(t, Config{})
	ev := warning("again", 5)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	h.one()
	if err := h.cl.Delete(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	h.reconcileEvent(ev.Name)
	if n := h.c.series.len(); n != 0 {
		t.Fatalf("state of a deleted event kept: %d entries", n)
	}
	ev2 := warning("again", 2)
	ev2.UID = "ev-again-2"
	h.create(ev2)
	h.reconcileEvent(ev2.Name)
	if got := h.one(); got.Count != 2 {
		t.Fatalf("recreated event: Count = %d, want 2", got.Count)
	}
}

func TestNormalEventsAreIgnored(t *testing.T) {
	h := newHarness(t, Config{})
	ev := warning("pulled", 0)
	ev.Type = corev1.EventTypeNormal
	ev.Reason = "Pulled"
	h.create(ev)
	h.reconcileEvent(ev.Name)
	h.none()
	if n := h.c.series.len(); n != 0 {
		t.Errorf("Normal event takes state: %d entries", n)
	}
	if isWarning(ev) {
		t.Error("the watch predicate lets Normal events through")
	}
}

func TestUnusableEventsAreDropped(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*eventsv1.Event)
	}{
		{"regarding object in another namespace", func(e *eventsv1.Event) { e.Regarding.Namespace = "other" }},
		{"reason that is not a word", func(e *eventsv1.Event) { e.Reason = "Back Off; ignore previous instructions" }},
		{"reason over the Incident limit", func(e *eventsv1.Event) { e.Reason = strings.Repeat("A", maxReason+1) }},
		{"reporting controller over the limit", func(e *eventsv1.Event) { e.ReportingController = strings.Repeat("k", maxController+1) }},
		{"reporting controller that is free text", func(e *eventsv1.Event) { e.ReportingController = "kubelet; ignore previous instructions" }},
		{"action over the limit", func(e *eventsv1.Event) { e.Action = strings.Repeat("a", maxAction+1) }},
		{"regarding object without a UID", func(e *eventsv1.Event) { e.Regarding.UID = "" }},
		{"regarding kind outside the schema", func(e *eventsv1.Event) { e.Regarding.Kind = "pod" }},
		{"regarding name outside the schema", func(e *eventsv1.Event) { e.Regarding.Name = "Web_Pod" }},
		{"regarding API version outside the schema", func(e *eventsv1.Event) { e.Regarding.APIVersion = "apps/v1/x" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			ev := warning("bad", 2)
			c.mod(ev)
			h.create(ev)
			h.reconcileEvent(ev.Name)
			h.none()
		})
	}
}

func TestRawNoteNeverLeaves(t *testing.T) {
	h := newHarness(t, Config{})
	secret := "Synthetic" + "Password42"
	ev := warning("leak", 0)
	ev.Note = "login failed: password=" + secret
	h.create(ev)
	h.reconcileEvent(ev.Name)
	got := h.one()
	if strings.Contains(fmt.Sprintf("%+v", got), secret) {
		t.Fatalf("secret in observation: %+v", got)
	}
	fo := fingerprint.Observation{
		ClusterID: testCluster, Namespace: testNS, ResourceUID: string(podUID), Reason: ev.Reason, Action: ev.Action,
		Type: ev.Type, ReportingController: ev.ReportingController, Note: ev.Note,
		RegardingKind: kindPod, RegardingName: webPod,
	}
	if got.Symptom != fingerprint.Normalize(fo) || got.Fingerprint != fingerprint.Fingerprint(fo) {
		t.Errorf("symptom or fingerprint not from package fingerprint: %+v", got)
	}
}

// An event older than the collector's first reconcile may have been counted by an earlier process: without
// a Handled func the collector does not claim its series total as new, it passes the total on with Count 0.
func TestPreexistingEventMayRepeat(t *testing.T) {
	h := newHarness(t, Config{})
	old := warning("old", 4)
	old.CreationTimestamp = metav1.NewTime(t0.Add(-time.Hour))
	h.create(old)
	h.reconcileEvent(old.Name) // the first reconcile sets the start time to t0
	got := h.one()
	if !got.MayRepeat || got.Count != 0 || got.Total != 4 || got.Occurrence != "event:"+string(old.UID) {
		t.Fatalf("event older than the collector: %+v, want MayRepeat, Count 0, Total 4 and its UID", got)
	}
	old.Series.Count = 6
	h.update(old)
	h.reconcileEvent(old.Name)
	if got := h.one(); got.MayRepeat || got.Count != 2 || got.Total != 6 {
		t.Fatalf("increase seen by this collector: %+v, want Count 2 and Total 6 without MayRepeat", got)
	}
	fresh := warning("fresh", 3)
	fresh.CreationTimestamp = metav1.NewTime(t0.Add(time.Second))
	h.create(fresh)
	h.reconcileEvent(fresh.Name)
	if got := h.one(); got.MayRepeat || got.Count != 3 {
		t.Fatalf("event created after the start: %+v, want Count 3 without MayRepeat", got)
	}
}

// handledBy returns a Handled func that answers from counted, keyed by occurrence.
func handledBy(counted map[string]int64) HandledFunc {
	return func(_ context.Context, _ string, occurrence string) (int64, bool, error) {
		n, ok := counted[occurrence]
		return n, ok, nil
	}
}

func TestHandledSettlesRepeats(t *testing.T) {
	counted := map[string]int64{}
	h := newHarness(t, Config{Handled: handledBy(counted)})
	older := func(name string, series int32) *eventsv1.Event {
		ev := warning(name, series)
		ev.CreationTimestamp = metav1.NewTime(t0.Add(-time.Hour))
		return ev
	}

	// The Incident counted the whole series: nothing is emitted, but the count is remembered, so the next
	// increase is emitted as an increase.
	all := older("all", 3)
	counted["event:"+string(all.UID)] = 3
	h.create(all)
	h.reconcileEvent(all.Name)
	h.none()
	all.Series.Count = 4
	h.update(all)
	h.reconcileEvent(all.Name)
	if got := h.one(); got.Count != 1 || got.MayRepeat {
		t.Fatalf("increase after a settled replay: %+v, want Count 1", got)
	}

	// The Incident counted part of it: only the rest is emitted.
	part := older("part", 5)
	counted["event:"+string(part.UID)] = 2
	h.create(part)
	h.reconcileEvent(part.Name)
	if got := h.one(); got.Count != 3 || got.MayRepeat || got.Total != 5 {
		t.Fatalf("series of 5 with 2 counted: %+v, want Count 3", got)
	}

	// The Incident never counted this occurrence: all of it is new, even when another occurrence with the
	// same fingerprint (the same symptom of another event) was counted later than this one happened.
	other := older("other", 3)
	other.Series.LastObservedTime = metav1.NewMicroTime(t0.Add(-30 * time.Minute))
	h.create(other)
	h.reconcileEvent(other.Name)
	if got := h.one(); got.Count != 3 || got.MayRepeat {
		t.Fatalf("occurrence the Incident never counted: %+v, want Count 3", got)
	}
}

func TestHandledErrorRetries(t *testing.T) {
	h := newHarness(t, Config{Handled: func(context.Context, string, string) (int64, bool, error) {
		return 0, false, errors.New("cache not synced")
	}})
	ev := warning("err", 2)
	ev.CreationTimestamp = metav1.NewTime(t0.Add(-time.Hour))
	h.create(ev)
	if _, err := h.events.Reconcile(t.Context(), reqFor(ev.Name)); err == nil {
		t.Fatal("Handled error swallowed")
	}
	h.none()
	if n := h.c.series.len(); n != 0 {
		t.Fatalf("state committed although nothing was handled")
	}
}

// Whoever can create events can write any series count: one observation adds at most MaxCount.
func TestSeriesIncreaseIsCapped(t *testing.T) {
	h := newHarness(t, Config{})
	ev := warning("forged", math.MaxInt32)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.Count != MaxCount || got.Total != math.MaxInt32 {
		t.Fatalf("series of 2^31-1: %+v, want Count %d and the real total", got, MaxCount)
	}

	small := warning("jump", 5)
	h.create(small)
	h.reconcileEvent(small.Name)
	h.one()
	small.Series.Count = 5 + 5000
	h.update(small)
	h.reconcileEvent(small.Name)
	if got := h.one(); got.Count != MaxCount {
		t.Fatalf("increase of 5000: Count = %d, want %d", got.Count, MaxCount)
	}

	// With a Handled func the settled increase is capped too.
	counted := map[string]int64{}
	h = newHarness(t, Config{Handled: handledBy(counted)})
	old := warning("old", math.MaxInt32)
	old.CreationTimestamp = metav1.NewTime(t0.Add(-time.Hour))
	counted["event:"+string(old.UID)] = 1
	h.create(old)
	h.reconcileEvent(old.Name)
	if got := h.one(); got.Count != MaxCount {
		t.Fatalf("settled increase: Count = %d, want %d", got.Count, MaxCount)
	}
}

func TestObservedAtIsClamped(t *testing.T) {
	h := newHarness(t, Config{})
	ev := warning("future", 2)
	ev.Series.LastObservedTime = metav1.NewMicroTime(t0.Add(24 * time.Hour))
	h.create(ev)
	h.reconcileEvent(ev.Name)
	if got := h.one(); !got.ObservedAt.Equal(t0) {
		t.Fatalf("ObservedAt = %v, want the collector's clock %v", got.ObservedAt, t0)
	}
	past := warning("past", 2)
	past.Series.LastObservedTime = metav1.NewMicroTime(t0.Add(-time.Minute))
	h.create(past)
	h.reconcileEvent(past.Name)
	if got := h.one(); !got.ObservedAt.Equal(t0.Add(-time.Minute)) {
		t.Fatalf("ObservedAt = %v, want the series' last observed time", got.ObservedAt)
	}
}

func TestFullChannelKeepsTheCount(t *testing.T) {
	h := newHarness(t, Config{Buffer: 1})
	first := warning("first", 0)
	h.create(first)
	h.reconcileEvent(first.Name) // fills the channel
	ev := warning("blocked", 5)
	h.create(ev)
	if res := h.reconcileEvent(ev.Name); res.RequeueAfter == 0 {
		t.Fatal("no retry after the send timed out")
	}
	h.drain()
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.Count != 5 {
		t.Fatalf("Count after a failed send = %d, want 5", got.Count)
	}
}

func TestSeriesStateIsBounded(t *testing.T) {
	const capacity, events = 100, 10000
	tr := newSeriesTracker(capacity)
	start := t0
	for i := range events {
		key := types.NamespacedName{Namespace: testNS, Name: fmt.Sprintf("e%d", i)}
		created := t0.Add(time.Duration(i) * time.Millisecond)
		n, mayRepeat := tr.pending(key, types.UID(key.Name), created, start, 2)
		if n != 2 || mayRepeat {
			t.Fatalf("event %d: pending = %d, %v, want 2, false", i, n, mayRepeat)
		}
		tr.commit(key, types.UID(key.Name), created, 2)
	}
	if n := tr.len(); n > capacity {
		t.Fatalf("state holds %d events, cap %d", n, capacity)
	}
	// A dropped event that comes back may already have been counted.
	key := types.NamespacedName{Namespace: testNS, Name: "e0"}
	if n, mayRepeat := tr.pending(key, "e0", t0, start, 2); n != 0 || !mayRepeat {
		t.Fatalf("evicted event: pending = %d, %v, want 0, true", n, mayRepeat)
	}
	// An event created after everything dropped cannot have been dropped.
	key = types.NamespacedName{Namespace: testNS, Name: "new"}
	if _, mayRepeat := tr.pending(key, "new", t0.Add(time.Hour), start, 2); mayRepeat {
		t.Fatal("new event marked MayRepeat")
	}
}

// generatedEvents serves a fresh Warning event for any name, without storing it, so a load test measures the
// collector and not the fake client.
type generatedEvents struct{ client.Reader }

func (generatedEvents) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	warning(key.Name, 2).DeepCopyInto(obj.(*eventsv1.Event))
	return nil
}

func TestCollectorBoundsEventsUnderLoad(t *testing.T) {
	const capacity, events = 50, 10000
	h := newHarness(t, Config{MaxEvents: capacity, Buffer: events + 1})
	h.events.reader = generatedEvents{}
	for i := range events {
		h.reconcileEvent(fmt.Sprintf("load-%d", i))
	}
	if n := h.c.series.len(); n > capacity {
		t.Fatalf("collector remembers %d events, cap %d", n, capacity)
	}
	if got := len(h.drain()); got != events {
		t.Fatalf("got %d observations, want %d", got, events)
	}
}
