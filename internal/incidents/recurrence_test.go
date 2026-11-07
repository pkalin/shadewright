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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The 24 hour rule (design 3.2, #251): within the window of resolvedTime, at its edge, before it (a clock
// that disagrees) or with either time unknown, a recurrence is escalated; only past the window does it open
// the next occurrence. Any phase but Resolved is left alone.
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
		{"seen before resolvedTime", status(remediationv1alpha1.IncidentResolved, &resolved), resolved.Add(-time.Hour), escalate},
		{"resolvedTime missing", status(remediationv1alpha1.IncidentResolved, nil), triggerNow, escalate},
		{"resolvedTime zero", status(remediationv1alpha1.IncidentResolved, &zero), triggerNow, escalate},
		{"seen time unknown", status(remediationv1alpha1.IncidentResolved, &resolved), time.Time{}, escalate},
		{"past the window", status(remediationv1alpha1.IncidentResolved, &resolved),
			resolved.Add(RecurrenceWindow + time.Second), nextOccurrence},
	} {
		if got := onResolved(tc.st, tc.seen); got != tc.want {
			t.Errorf("%s: onResolved = %d, want %d", tc.name, got, tc.want)
		}
	}
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

// recurrenceOf is the spec of fp(1) as observation i gives it.
func recurrenceOf(i int) remediationv1alpha1.IncidentSpec {
	return specOf(observation(1, i, 1, 1, triggerNow))
}

// A symptom of a Resolved Incident seen again within 24 hours of resolvedTime, or with resolvedTime missing,
// moves the same Incident to Escalated with no analysis: no new Incident, the trigger never marks it due.
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
			r.obs <- observation(1, 100, 1, 1, triggerNow)
			inc := eventually(t, ns, 1, func(inc *remediationv1alpha1.Incident) bool {
				return inc.Status.Phase == remediationv1alpha1.IncidentEscalated && inc.Status.Count == 2
			})
			cond := meta.FindStatusCondition(inc.Status.Conditions, ConditionRecurred)
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tc.reason || cond.Message == "" {
				t.Fatalf("Recurred condition %+v, want True %s", cond, tc.reason)
			}
			time.Sleep(5 * testTriggerInterval)
			if meta.IsStatusConditionTrue(get(t, ns).Status.Conditions, ConditionAnalysisDue) {
				t.Fatal("the escalated recurrence was marked due for analysis")
			}
			absent(t, ns, remediationv1alpha1.IncidentName(fp(1), 2))
			if got := r.writes.creates.Load(); got != 0 {
				t.Fatalf("%d creates, want none", got)
			}
		})
	}
}

// Seen again more than 24 hours after resolvedTime, the symptom opens occurrence 2 as a new Incident linked
// to the first, which stays Resolved and keeps its counts. The new one starts with this observation only,
// nothing of the earlier outcome, and the trigger takes it as its own and marks it due.
func TestRecurrenceAfterTheWindowOpensTheNextOccurrence(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	createBudget(t, ns, nil)
	first := resolvedStatus(25 * time.Hour)
	first.LastAnalysisTime = ago(26 * time.Hour)
	first.RemediationRequests = []remediationv1alpha1.Name{"remediation-0123456789abcdef01234567"}
	createNamed(t, ns, 1, first)
	r := start(t, ns, triggerConfig)
	o := observation(1, 100, 1, 1, triggerNow.Add(-2*time.Minute))
	r.obs <- o
	second := remediationv1alpha1.IncidentName(fp(1), 2)
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
	old := get(t, ns)
	if old.Status.Phase != remediationv1alpha1.IncidentResolved || old.Status.Count != 1 {
		t.Fatalf("the first occurrence changed: %+v", old.Status)
	}
	named(t, ns, second, dueIs(true, ReasonFirstAnalysis))
	if got := r.writes.creates.Load(); got != 1 {
		t.Fatalf("%d creates, want 1", got)
	}
}

// Idempotence (invariant 5): after a restart the controller finds the latest occurrence from the names it
// lists and writes there, also when the previous process created it and stopped before its status write. No
// third Incident appears and nothing is created again.
func TestRestartFindsTheLatestOccurrence(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, resolvedStatus(25*time.Hour))
	second := remediationv1alpha1.IncidentName(fp(1), 2)
	// Occurrence 2 as a crash between its create and its first status write leaves it: no status at all.
	inc := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: second},
		Spec:       nextSpec(recurrenceOf(1), 2),
	}
	if err := env.admin.Create(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	r := start(t, ns, triggerConfig)
	r.obs <- observation(1, 100, 1, 1, triggerNow)
	named(t, ns, second, countIs(1))
	r.stop()
	r = start(t, ns, triggerConfig)
	r.obs <- observation(1, 101, 1, 1, triggerNow)
	named(t, ns, second, countIs(2))
	absent(t, ns, remediationv1alpha1.IncidentName(fp(1), 3))
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
	r.obs <- observation(1, 100, 1, 1, triggerNow)
	named(t, ns, remediationv1alpha1.IncidentName(fp(1), 2), countIs(2))
	absent(t, ns, Name(fp(1)))
	if got := r.writes.creates.Load(); got != 0 {
		t.Fatalf("%d creates, want none", got)
	}
}

// A next occurrence another replica created after this controller loaded is adopted, not duplicated: the
// write steps from the Resolved occurrence it knows to the one that exists.
func TestNextOccurrenceCreatedElsewhereIsAdopted(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, resolvedStatus(25*time.Hour))
	r := start(t, ns, triggerConfig)
	<-r.ready
	second := remediationv1alpha1.IncidentName(fp(1), 2)
	createNamed(t, ns, 2, incidentStatus(time.Minute, time.Minute, 0))
	r.obs <- observation(1, 100, 1, 1, triggerNow)
	named(t, ns, second, countIs(2))
	absent(t, ns, remediationv1alpha1.IncidentName(fp(1), 3))
	if got := r.writes.creates.Load(); got != 0 {
		t.Fatalf("%d creates, want none", got)
	}
}

// A next occurrence is a new object and counts under the cap like a new fingerprint, so at the cap it is
// refused: nothing is created and the Resolved Incident stays as it was.
func TestNextOccurrenceIsRefusedAtTheCap(t *testing.T) {
	ns := namespace(t)
	createNamed(t, ns, 1, resolvedStatus(25*time.Hour))
	before := testutil.ToFloat64(refusedObservations)
	r := start(t, ns, Config{MaxIncidents: 1, TriggerInterval: testTriggerInterval, Now: triggerConfig.Now})
	r.obs <- observation(1, 100, 1, 1, triggerNow)
	deadline := time.Now().Add(20 * time.Second)
	for testutil.ToFloat64(refusedObservations)-before < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the next occurrence was not refused")
		}
		time.Sleep(20 * time.Millisecond)
	}
	absent(t, ns, remediationv1alpha1.IncidentName(fp(1), 2))
	if old := get(t, ns); old.Status.Count != 1 || old.Status.Phase != remediationv1alpha1.IncidentResolved {
		t.Fatalf("the Resolved Incident changed: %+v", old.Status)
	}
	if got := r.writes.creates.Load(); got != 0 {
		t.Fatalf("%d creates, want none", got)
	}
	// The memory went back to what the Resolved Incident holds: the refused occurrence is not counted.
	r.mu.Lock()
	_, found := r.counted(eventID(100))
	r.mu.Unlock()
	if found {
		t.Fatal("the refused occurrence is remembered as counted")
	}
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
			r.obs <- observation(1, 100, 1, 1, triggerNow)
			inc := eventually(t, ns, 1, countIs(2))
			if inc.Status.Phase != phase || meta.FindStatusCondition(inc.Status.Conditions, ConditionRecurred) != nil {
				t.Fatalf("phase %s, conditions %+v", inc.Status.Phase, inc.Status.Conditions)
			}
			absent(t, ns, remediationv1alpha1.IncidentName(fp(1), 2))
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
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: remediationv1alpha1.IncidentName(fp(1), 2)},
		Spec:       recurrenceOf(1),
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
