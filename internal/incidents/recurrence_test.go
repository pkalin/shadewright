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
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
)

// What a write does to a Resolved Incident (design 3.2, #251): observations seen no later than resolvedTime
// are no recurrence; later ones, or with either time unknown, escalate it. Any phase but Resolved is left
// alone.
func TestOnResolved(t *testing.T) {
	resolved := triggerNow.Add(-48 * time.Hour)
	status := func(phase remediationv1alpha1.IncidentPhase, at *time.Time) *remediationv1alpha1.IncidentStatus {
		st := &remediationv1alpha1.IncidentStatus{Phase: phase}
		if at != nil {
			st.ResolvedTime = &metav1.Time{Time: *at}
		}
		return st
	}
	zero := time.Time{}
	for _, tc := range []struct {
		name string
		st   *remediationv1alpha1.IncidentStatus
		seen time.Time
		want recurrence
	}{
		{"open", status(remediationv1alpha1.IncidentOpen, &resolved), triggerNow, notResolved},
		{"escalated", status(remediationv1alpha1.IncidentEscalated, &resolved), triggerNow, notResolved},
		{"proposed", status(remediationv1alpha1.IncidentProposed, &resolved), triggerNow, notResolved},
		{"no phase", status("", &resolved), triggerNow, notResolved},
		{"within the window", status(remediationv1alpha1.IncidentResolved, &resolved), resolved.Add(time.Hour), escalate},
		{"at the window's edge", status(remediationv1alpha1.IncidentResolved, &resolved), resolved.Add(RecurrenceWindow), escalate},
		{"past the window, still unrouted", status(remediationv1alpha1.IncidentResolved, &resolved), triggerNow, escalate},
		{"seen before resolvedTime", status(remediationv1alpha1.IncidentResolved, &resolved), resolved.Add(-time.Hour), beforeResolution},
		{"seen at resolvedTime", status(remediationv1alpha1.IncidentResolved, &resolved), resolved, beforeResolution},
		{"resolvedTime missing", status(remediationv1alpha1.IncidentResolved, nil), triggerNow, escalate},
		{"resolvedTime zero", status(remediationv1alpha1.IncidentResolved, &zero), triggerNow, escalate},
		{"seen time unknown", status(remediationv1alpha1.IncidentResolved, &resolved), time.Time{}, escalate},
	} {
		if got := onResolved(tc.st, tc.seen); got != tc.want {
			t.Errorf("%s: onResolved = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// clock is a controller clock a test moves.
type clock struct{ offset atomic.Int64 }

func (c *clock) now() time.Time          { return triggerNow.Add(time.Duration(c.offset.Load())) }
func (c *clock) advance(d time.Duration) { c.offset.Add(int64(d)) }

// movingConfig is triggerConfig with clk as the clock.
func movingConfig(clk *clock) Config {
	cfg := triggerConfig
	cfg.Now = clk.now
	return cfg
}

// resolvedStatus is the status of an Incident of fp(1) Resolved resolvedAgo before triggerNow (never when
// resolvedAgo is negative: no resolvedTime), seen last a little before that.
func resolvedStatus(resolvedAgo time.Duration) remediationv1alpha1.IncidentStatus {
	seen := max(resolvedAgo, 0) + 2*time.Hour
	st := incidentStatus(seen, seen, 0)
	st.Phase = remediationv1alpha1.IncidentResolved
	if resolvedAgo >= 0 {
		st.ResolvedTime = ago(resolvedAgo)
	}
	return st
}

// createNamed creates an Incident of fp(1) named for occurrence n (1 or more) with status st.
func createNamed(t *testing.T, ns string, n int32, st remediationv1alpha1.IncidentStatus) {
	t.Helper()
	spec := specOf(observation(1, 1, 1, 1, triggerNow))
	if n >= 2 {
		spec = nextSpec(spec, n)
	}
	inc := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: remediationv1alpha1.IncidentName(fp(1), n)},
		Spec:       spec,
	}
	if err := env.admin.Create(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	inc.Status = st
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
}

// named waits until cond holds for the Incident called name.
func named(t *testing.T, ns, name string, cond func(*remediationv1alpha1.Incident) bool) *remediationv1alpha1.Incident {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		inc := &remediationv1alpha1.Incident{}
		err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, inc)
		if err == nil && cond(inc) {
			return inc
		}
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: condition not met; last %+v (%v)", name, inc.Status, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// absent fails when an Incident called name exists.
func absent(t *testing.T, ns, name string) {
	t.Helper()
	err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, &remediationv1alpha1.Incident{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Incident %s exists or cannot be read (%v)", name, err)
	}
}

// firstSpec is the spec of fp(1) as its first observation gives it.
func firstSpec() remediationv1alpha1.IncidentSpec {
	return specOf(observation(1, 1, 1, 1, triggerNow))
}

// seenAt is an observation of fp(1), occurrence i, that began and was observed at t.
func seenAt(i int, t time.Time) collector.Observation { return observation(1, i, 1, 1, t) }

// idle waits until the controller has written everything it holds of fp(1) and remembers it.
func idle(t *testing.T, r *running) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		r.mu.Lock()
		idle := r.pending[fp(1)] == nil && r.failed[fp(1)] == nil && r.heldBy(fp(1)) == 0
		r.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the controller still holds observations of fp(1)")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// second and third are the names of occurrences 2 and 3 of fp(1).
var (
	second = remediationv1alpha1.IncidentName(fp(1), 2)
	third  = remediationv1alpha1.IncidentName(fp(1), 3)
)

// A symptom of a Resolved Incident seen again within 24 hours of resolvedTime, or with resolvedTime missing,
// moves the same Incident to Escalated with no analysis: no new Incident, and the trigger says why with its
// own reason, not the one of an analysis that escalated.
func TestRecurrenceWithinTheWindowEscalates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		resolvedAgo time.Duration
		reason      string
	}{
		{"within 24 h", 23 * time.Hour, ReasonRecurredWithinWindow},
		{"resolvedTime missing", -1, ReasonResolvedTimeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := namespace(t)
			createSettings(t, 10)
			createBudget(t, ns, nil)
			createNamed(t, ns, 1, resolvedStatus(tc.resolvedAgo))
			r := start(t, ns, triggerConfig)
			r.obs <- immediate(seenAt(100, triggerNow))
			inc := eventually(t, ns, 1, func(inc *remediationv1alpha1.Incident) bool {
				return inc.Status.Phase == remediationv1alpha1.IncidentEscalated && inc.Status.Count == 2
			})
			cond := meta.FindStatusCondition(inc.Status.Conditions, ConditionRecurred)
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tc.reason || cond.Message == "" {
				t.Fatalf("Recurred condition %+v, want True %s", cond, tc.reason)
			}
			eventually(t, ns, 1, dueIs(false, ReasonRecurred))
			absent(t, ns, second)
			if got := r.writes.creates.Load(); got != 0 {
				t.Fatalf("%d creates, want none", got)
			}
		})
	}
}

// Observations seen no later than resolvedTime (held unwritten across the resolution) are no recurrence:
// they are counted and the Incident stays Resolved.
func TestObservationBeforeResolutionDoesNotEscalate(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, resolvedStatus(time.Hour))
	r := start(t, ns, triggerConfig)
	r.obs <- seenAt(100, triggerNow.Add(-90*time.Minute))
	inc := eventually(t, ns, 1, countIs(2))
	if inc.Status.Phase != remediationv1alpha1.IncidentResolved ||
		meta.FindStatusCondition(inc.Status.Conditions, ConditionRecurred) != nil {
		t.Fatalf("phase %s, conditions %+v; want still Resolved", inc.Status.Phase, inc.Status.Conditions)
	}
	absent(t, ns, second)
	if got := r.writes.creates.Load(); got != 0 {
		t.Fatalf("%d creates, want none", got)
	}
}

// Seen again more than 24 hours after resolvedTime, the symptom opens occurrence 2 as a new Incident linked
// to the first, which stays Resolved and keeps its counts. The new one starts with this observation only,
// nothing of the earlier outcome; the trigger takes it as its own and marks it due, and the next observation
// is counted there.
func TestRecurrenceAfterTheWindowOpensTheNextOccurrence(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	createBudget(t, ns, nil)
	first := resolvedStatus(25 * time.Hour)
	first.LastAnalysisTime = ago(26 * time.Hour)
	first.RemediationRequests = []remediationv1alpha1.Name{"remediation-0123456789abcdef01234567"}
	createNamed(t, ns, 1, first)
	clk := &clock{}
	r := start(t, ns, movingConfig(clk))
	o := immediate(seenAt(100, triggerNow.Add(-2*time.Minute)))
	r.obs <- o
	inc := named(t, ns, second, countIs(1))
	if inc.Spec.OccurrenceNumber != 2 || inc.Spec.PreviousIncident != Name(fp(1)) {
		t.Fatalf("spec links occurrence %d to %q, want 2 and %q", inc.Spec.OccurrenceNumber, inc.Spec.PreviousIncident, Name(fp(1)))
	}
	if inc.Spec.Fingerprint != fp(1) || inc.Spec.Symptom != o.Symptom {
		t.Fatalf("spec %+v", inc.Spec)
	}
	st := inc.Status
	if st.LastAnalysisTime != nil || len(st.RemediationRequests) != 0 || len(st.AnalysisRecords) != 0 ||
		st.ResolvedTime != nil || len(st.Occurrences) != 1 || st.Occurrences[0].ID != eventID(100) {
		t.Fatalf("the new occurrence carries more than its observation: %+v", st)
	}
	if old := get(t, ns); old.Status.Phase != remediationv1alpha1.IncidentResolved || old.Status.Count != 1 {
		t.Fatalf("the first occurrence changed: %+v", old.Status)
	}
	named(t, ns, second, dueIs(true, ReasonFirstAnalysis))
	if got := r.writes.creates.Load(); got != 1 {
		t.Fatalf("%d creates, want 1", got)
	}

	// The next observation, past the write window, goes to occurrence 2 and is remembered there.
	idle(t, r)
	clk.advance(2 * time.Second)
	r.obs <- seenAt(101, triggerNow.Add(time.Second))
	named(t, ns, second, countIs(2))
	r.mu.Lock()
	for _, id := range []string{eventID(100), eventID(101)} {
		if got := r.records[id].incident; got != second {
			r.mu.Unlock()
			t.Fatalf("the record of %s belongs to %q, want %q", id, got, second)
		}
	}
	r.mu.Unlock()
	absent(t, ns, third)
}

// The next occurrence passes the same gate as a new fingerprint (#267): a symptom seen once opens nothing,
// seen again 2 to 15 minutes later it opens occurrence 2 with both sightings.
func TestNextOccurrencePassesTheRecurrenceGate(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, resolvedStatus(25*time.Hour))
	clk := &clock{}
	r := start(t, ns, movingConfig(clk))
	r.obs <- seenAt(100, triggerNow)
	time.Sleep(500 * time.Millisecond)
	absent(t, ns, second)
	if old := get(t, ns); old.Status.Count != 1 || old.Status.Phase != remediationv1alpha1.IncidentResolved {
		t.Fatalf("a single sighting changed the Resolved Incident: %+v", old.Status)
	}
	clk.advance(3 * time.Minute)
	r.obs <- seenAt(101, triggerNow.Add(3*time.Minute))
	named(t, ns, second, countIs(2))
}

// Idempotence (invariant 5): after a restart the controller finds the latest occurrence from the names it
// lists and writes there, also when the previous process created it and stopped before its status write. No
// third Incident appears and nothing is created again.
func TestRestartFindsTheLatestOccurrence(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, resolvedStatus(25*time.Hour))
	// Occurrence 2 as a crash between its create and its first status write leaves it: no status at all.
	inc := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: second},
		Spec:       nextSpec(firstSpec(), 2),
	}
	if err := env.admin.Create(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	r := start(t, ns, triggerConfig)
	r.obs <- seenAt(100, triggerNow)
	named(t, ns, second, countIs(1))
	r.stop()
	r = start(t, ns, triggerConfig)
	r.obs <- seenAt(101, triggerNow)
	named(t, ns, second, countIs(2))
	absent(t, ns, third)
	if got := r.writes.creates.Load(); got != 0 {
		t.Fatalf("%d creates after the restart, want none", got)
	}
	if old := get(t, ns); old.Status.Count != 1 || old.Status.Phase != remediationv1alpha1.IncidentResolved {
		t.Fatalf("the first occurrence changed: %+v", old.Status)
	}
}

// After the clean-up at the cap deletes the Resolved first occurrence (docs/how-to/clear-resolved-incidents.md),
// a restart still writes to the latest occurrence it lists, and does not open occurrence 1 again.
func TestDeletedEarlierOccurrenceIsNotRecreated(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 2, incidentStatus(time.Minute, time.Minute, 0))
	r := start(t, ns, triggerConfig)
	r.obs <- seenAt(100, triggerNow)
	named(t, ns, second, countIs(2))
	absent(t, ns, Name(fp(1)))
	if got := r.writes.creates.Load(); got != 0 {
		t.Fatalf("%d creates, want none", got)
	}
}

// A later occurrence deleted while the controller runs is not created again from memory: the observation
// meant for it is dropped and counted, the occurrence before it is the latest again, and a single sighting
// opens nothing (only the recurrence gate does).
func TestDeletedLatestOccurrenceIsNotRecreated(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, resolvedStatus(25*time.Hour))
	createNamed(t, ns, 2, incidentStatus(time.Minute, time.Minute, 0))
	clk := &clock{}
	r := start(t, ns, movingConfig(clk))
	<-r.ready
	if err := env.admin.Delete(t.Context(), &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: second}}); err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(droppedBatches.WithLabelValues(dropDeleted))
	r.obs <- seenAt(100, triggerNow)
	deadline := time.Now().Add(20 * time.Second)
	for testutil.ToFloat64(droppedBatches.WithLabelValues(dropDeleted))-before < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the observation for the deleted occurrence was not dropped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.Lock()
	latest := r.nameOf(fp(1))
	_, kept := r.names[second]
	r.mu.Unlock()
	if latest != Name(fp(1)) {
		t.Fatalf("latest occurrence %s, want %s", latest, Name(fp(1)))
	}
	if kept {
		t.Fatal("the deleted occurrence still takes a place under the cap")
	}
	absent(t, ns, second)
	clk.advance(2 * time.Second)
	r.obs <- seenAt(101, triggerNow.Add(time.Second))
	time.Sleep(500 * time.Millisecond)
	absent(t, ns, second)
	if old := get(t, ns); old.Status.Count != 1 || old.Status.Phase != remediationv1alpha1.IncidentResolved {
		t.Fatalf("the Resolved Incident changed: %+v", old.Status)
	}
}

// A next occurrence another replica created after this controller loaded is adopted, not duplicated.
func TestNextOccurrenceCreatedElsewhereIsAdopted(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, resolvedStatus(25*time.Hour))
	r := start(t, ns, triggerConfig)
	<-r.ready
	createNamed(t, ns, 2, incidentStatus(time.Minute, time.Minute, 0))
	r.obs <- immediate(seenAt(100, triggerNow))
	named(t, ns, second, countIs(2))
	absent(t, ns, third)
	if got := r.writes.creates.Load(); got != 0 {
		t.Fatalf("%d creates, want none", got)
	}
}

// A create of the next occurrence that fails for a while keeps its one place under the cap and is tried
// again until it succeeds; the place is never taken twice.
func TestFailedCreateOfTheNextOccurrenceKeepsOnePlace(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, resolvedStatus(25*time.Hour))
	var fails atomic.Int32
	cfg := triggerConfig
	cfg.RetryAfter = 50 * time.Millisecond
	clk := &clock{}
	cfg.Now = clk.now
	r := startWith(t, ns, cfg, func(_ *Controller, w *countingClient) {
		w.create = func(client.Object) error {
			if fails.Add(1) <= 2 {
				return apierrors.NewServiceUnavailable("etcd is down")
			}
			return nil
		}
	})
	r.obs <- immediate(seenAt(100, triggerNow))
	go func() {
		for range 200 {
			clk.advance(100 * time.Millisecond)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	named(t, ns, second, countIs(1))
	r.mu.Lock()
	places := len(r.names)
	_, opening := r.opening[second]
	r.mu.Unlock()
	if places != 2 || opening {
		t.Fatalf("%d places under the cap (want 2), still opening %v", places, opening)
	}
}

// A next occurrence is a new object and counts under the cap like a new fingerprint, so at the cap it is
// refused: nothing is created, the Resolved Incident stays as it was, and the refusal is counted and
// remembered by fingerprint like one of a new fingerprint.
func TestNextOccurrenceIsRefusedAtTheCap(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, resolvedStatus(25*time.Hour))
	before := testutil.ToFloat64(refusedObservations)
	cfg := triggerConfig
	cfg.MaxIncidents = 1
	r := start(t, ns, cfg)
	r.obs <- immediate(seenAt(100, triggerNow))
	deadline := time.Now().Add(20 * time.Second)
	for testutil.ToFloat64(refusedObservations)-before < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the next occurrence was not refused")
		}
		time.Sleep(20 * time.Millisecond)
	}
	absent(t, ns, second)
	if old := get(t, ns); old.Status.Count != 1 || old.Status.Phase != remediationv1alpha1.IncidentResolved {
		t.Fatalf("the Resolved Incident changed: %+v", old.Status)
	}
	if got := r.writes.creates.Load(); got != 0 {
		t.Fatalf("%d creates, want none", got)
	}
	r.mu.Lock()
	_, found := r.counted(eventID(100))
	refused, latest := r.refused.has(fp(1)), r.nameOf(fp(1))
	r.mu.Unlock()
	if found {
		t.Fatal("the refused occurrence is remembered as counted")
	}
	if !refused || latest != Name(fp(1)) {
		t.Fatalf("refused %v, latest %s; want the fingerprint refused and occurrence 1 still the latest", refused, latest)
	}
}

// A phase set by another writer while the controller runs is seen within a trigger pass: an Incident
// resolved more than 24 hours before a new sighting then opens its next occurrence.
func TestResolvedSetWhileRunningIsSeen(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, incidentStatus(2*time.Hour, 2*time.Hour, 0))
	r := start(t, ns, triggerConfig)
	<-r.ready
	inc := get(t, ns)
	inc.Status.Phase, inc.Status.ResolvedTime = remediationv1alpha1.IncidentResolved, ago(25*time.Hour)
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		r.mu.Lock()
		_, seen := r.resolvedAt[Name(fp(1))]
		r.mu.Unlock()
		if seen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no trigger pass saw the Incident Resolved")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.obs <- immediate(seenAt(100, triggerNow))
	named(t, ns, second, countIs(1))
}

// Nothing but a Resolved Incident opens a later occurrence: an Incident in any other phase counts a symptom
// seen again however long ago it was last seen, even with a stray resolvedTime.
func TestOnlyAResolvedIncidentOpensAnOccurrence(t *testing.T) {
	for _, phase := range []remediationv1alpha1.IncidentPhase{
		remediationv1alpha1.IncidentOpen, remediationv1alpha1.IncidentProposed, remediationv1alpha1.IncidentEscalated,
	} {
		t.Run(string(phase), func(t *testing.T) {
			ns := namespace(t)
			st := resolvedStatus(72 * time.Hour)
			st.Phase = phase
			createNamed(t, ns, 1, st)
			r := start(t, ns, triggerConfig)
			r.obs <- immediate(seenAt(100, triggerNow))
			inc := eventually(t, ns, 1, countIs(2))
			if inc.Status.Phase != phase || meta.FindStatusCondition(inc.Status.Conditions, ConditionRecurred) != nil {
				t.Fatalf("phase %s, conditions %+v", inc.Status.Phase, inc.Status.Conditions)
			}
			absent(t, ns, second)
			if got := r.writes.creates.Load(); got != 0 {
				t.Fatalf("%d creates, want none", got)
			}
		})
	}
}

// An Incident named for an occurrence its spec does not record is not this controller's: the trigger does
// not analyze it.
func TestOccurrenceNameWithoutItsNumberIsForeign(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	createBudget(t, ns, nil)
	squat := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: second},
		Spec:       firstSpec(),
	}
	if err := env.admin.Create(t.Context(), squat); err != nil {
		t.Fatal(err)
	}
	squat.Status = incidentStatus(2*time.Minute, time.Hour, 0)
	meta.SetStatusCondition(&squat.Status.Conditions, metav1.Condition{
		Type: ConditionAnalysisDue, Status: metav1.ConditionTrue, Reason: ReasonFirstAnalysis,
		Message: dueMessages[ReasonFirstAnalysis], ObservedGeneration: squat.Generation,
	})
	if err := env.admin.Status().Update(t.Context(), squat); err != nil {
		t.Fatal(err)
	}
	start(t, ns, triggerConfig)
	named(t, ns, squat.Name, dueIs(false, ReasonForeignFingerprint))
}
