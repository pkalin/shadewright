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
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// The tests of the consequence rule (ADR-0039 points 6, 7, 16, 17, 24 and 25). Each builds its observations
// the way the collector reports them: a cause read from pod status, a consequence from a kubelet event whose
// pod status shows that cause (CauseFingerprint), or one the collector did not link.

const (
	podSubjectUID   = "33333333-3333-4333-8333-333333333333"
	otherSubjectUID = "44444444-4444-4444-8444-444444444444"
	reasonOOM       = "OOMKilled"
	reasonConfig    = "CreateContainerConfigError"
	reasonBackOff   = string(remediationv1alpha1.AbsorbedBackOff)
	reasonUnhealthy = string(remediationv1alpha1.AbsorbedUnhealthy)
	condReviewed    = "Reviewed"
)

func terminationID(i int) string { return fmt.Sprintf("termination:%064x", i) }
func waitingID(i int) string     { return fmt.Sprintf("waiting:%064x", i) }

// symptomOf returns an observation of fingerprint fp(f) built on [observation] (subject demo/orders with
// podSubjectUID, the kubelet as reporting controller), with reason, occurrence, total, origin, container and
// class set, observed at at.
func symptomOf(f int, reason, occurrence string, total int64, origin fingerprint.Source, container string,
	class collector.Class, at time.Time) collector.Observation {
	o := observation(f, 0, total, total, at)
	o.Reason, o.Symptom, o.Occurrence = reason, reason+" of container "+container, occurrence
	o.Origin, o.Container, o.Class = origin, container, class
	return o
}

// subjectWith returns the subject of [observation] with another UID.
func subjectWith(uid string) remediationv1alpha1.ObjectRef {
	s := observation(0, 0, 0, 0, time.Time{}).Subject
	s.UID = remediationv1alpha1.UID(uid)
	return s
}

// oomCause is an OOMKilled termination of container web in fingerprint fp(1), read from pod status: a cause
// that opens its Incident at once.
func oomCause(i int, at time.Time) collector.Observation {
	o := symptomOf(1, reasonOOM, terminationID(i), 1, fingerprint.SourcePodStatus, "web", collector.ClassCause, at)
	o.Immediate = true
	return o
}

// waitingCause is a container waiting with CreateContainerConfigError (a missing Secret), read from pod status
// and reported again RecurAfter after the collector first saw it, so it opens its Incident.
func waitingCause(f int, container string, i int, at time.Time) collector.Observation {
	o := symptomOf(f, reasonConfig, waitingID(i), 1, fingerprint.SourcePodStatus, container, collector.ClassCause, at)
	o.Began = at.Add(-RecurAfter)
	return o
}

// forgedOOM is an Event whose reason, note and controller copy those of a pod-status termination. It is no
// cause: the collector gives it no class, and its fingerprint hashes no pod-status source. Its series proves
// recurrence, so it opens an Incident of its own.
func forgedOOM(f, i int, at time.Time) collector.Observation {
	o := symptomOf(f, reasonOOM, eventID(i), 2, fingerprint.SourceEvent, "web", collector.ClassNone, at)
	o.Began = at.Add(-RecurAfter)
	return o
}

// consequenceOf is a kubelet BackOff or Unhealthy event of container in own fingerprint f, linked to the cause
// fingerprint causeF (none when 0). Its series proves recurrence, so when it is not counted as evidence it
// opens its own Incident at once.
func consequenceOf(f, causeF int, container, reason string, i int, count int64, at time.Time) collector.Observation {
	o := symptomOf(f, reason, eventID(i), count, fingerprint.SourceEvent, container, collector.ClassConsequence, at)
	o.Began = at.Add(-RecurAfter)
	if causeF > 0 {
		o.CauseFingerprint = fp(causeF)
	}
	return o
}

// want is what one Incident must show once every observation is written.
type want struct {
	count       int64
	lastSeen    time.Time
	cause       bool
	containers  []string
	absorbed    map[remediationv1alpha1.AbsorbedReason]int64
	absorbedAt  map[remediationv1alpha1.AbsorbedReason]time.Time
	occurrences []string
	absorbedIDs []string
}

func (w *want) addAbsorbed(reason string, n int64, at time.Time, id string) {
	r := remediationv1alpha1.AbsorbedReason(reason)
	if w.absorbed == nil {
		w.absorbed, w.absorbedAt = map[remediationv1alpha1.AbsorbedReason]int64{}, map[remediationv1alpha1.AbsorbedReason]time.Time{}
	}
	w.absorbed[r] += n
	w.absorbedAt[r] = latest(w.absorbedAt[r], at)
	w.absorbedIDs = append(w.absorbedIDs, id)
}

func (w *want) addOwn(n int64, at time.Time, id, container string) {
	w.count += n
	w.lastSeen = latest(w.lastSeen, at)
	w.occurrences = append(w.occurrences, id)
	if container != "" && !slices.Contains(w.containers, container) {
		w.containers = append(w.containers, container)
	}
}

// mismatch returns how inc differs from w, or "".
func (w *want) mismatch(inc *remediationv1alpha1.Incident) string {
	st := &inc.Status
	var out []string
	if st.Count != w.count {
		out = append(out, fmt.Sprintf("count %d, want %d", st.Count, w.count))
	}
	if !timeFrom(st.LastSeen).Equal(w.lastSeen) {
		out = append(out, fmt.Sprintf("lastSeen %v, want %v", timeFrom(st.LastSeen), w.lastSeen))
	}
	if st.Cause != w.cause {
		out = append(out, fmt.Sprintf("cause %v, want %v", st.Cause, w.cause))
	}
	containers := make([]string, 0, len(st.Containers))
	for _, c := range st.Containers {
		containers = append(containers, string(c))
	}
	if !sameSet(containers, w.containers) {
		out = append(out, fmt.Sprintf("containers %v, want %v", containers, w.containers))
	}
	if len(st.Absorbed) != len(w.absorbed) {
		out = append(out, fmt.Sprintf("absorbed %+v, want %v", st.Absorbed, w.absorbed))
	}
	for _, a := range st.Absorbed {
		if a.Count != w.absorbed[a.Reason] || !a.LastSeen.Time.Equal(w.absorbedAt[a.Reason]) {
			out = append(out, fmt.Sprintf("absorbed %s: %d at %v, want %d at %v", a.Reason, a.Count, a.LastSeen.Time,
				w.absorbed[a.Reason], w.absorbedAt[a.Reason]))
		}
	}
	if ids := recordIDs(st.Occurrences); !sameSet(ids, w.occurrences) {
		out = append(out, fmt.Sprintf("occurrences %v, want %v", ids, w.occurrences))
	}
	if ids := recordIDs(st.AbsorbedOccurrences); !sameSet(ids, w.absorbedIDs) {
		out = append(out, fmt.Sprintf("absorbedOccurrences %v, want %v", ids, w.absorbedIDs))
	}
	return strings.Join(out, "; ")
}

func recordIDs(rs []remediationv1alpha1.OccurrenceRecord) []string {
	ids := make([]string, 0, len(rs))
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	return ids
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// settledTo waits until the Incidents of ns are exactly those of wants, each as it says.
func settledTo(t *testing.T, ns string, wants map[string]*want) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var list remediationv1alpha1.IncidentList
		if err := env.admin.List(t.Context(), &list, client.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		var problems []string
		seen := map[string]bool{}
		for i := range list.Items {
			inc := &list.Items[i]
			seen[inc.Name] = true
			w, ok := wants[inc.Name]
			if !ok {
				problems = append(problems, fmt.Sprintf("unexpected Incident %s (reason %s, count %d)", inc.Name,
					inc.Spec.Reason, inc.Status.Count))
				continue
			}
			if m := w.mismatch(inc); m != "" {
				problems = append(problems, inc.Name+": "+m)
			}
		}
		for name := range wants {
			if !seen[name] {
				problems = append(problems, "missing Incident "+name)
			}
		}
		if len(problems) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Incidents do not match:\n%s", strings.Join(problems, "\n"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The variants of a consequence in the generated corpus. Only the two linked ones are counted as evidence;
// every other opens or counts on its own Incident.
const (
	linkWeb      = iota // linked to the OOM cause of container web
	linkDB              // linked to the missing Secret cause of container db
	noCause             // the pod status shows no cause: no CauseFingerprint
	unknownCause        // a CauseFingerprint with no Incident (the cause is not consumed yet)
	otherSubject        // linked to the web cause's fingerprint, but another subject UID
	noContainer         // no named container (point 11)
	forgedCause         // a CauseFingerprint of an Incident a forged Event opened, which is no cause
	variants
)

// TestConsequenceCorpus runs generated sequences of causes, consequences and forged Events whose answer is
// known by construction: a consequence of a linked variant is counted in the cause's absorbed tally and
// absorbedOccurrences and moves neither its count nor its lastSeen; every other observation counts on its own
// fingerprint's Incident. The cause's status.cause and containers are set, a forged Event's never.
func TestConsequenceCorpus(t *testing.T) {
	const (
		web, db, forged = 1, 2, 3
		missing         = 50
	)
	for seed := range uint64(12) {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 791)) // #nosec G404 -- a reproducible test corpus, not a secret
			ns := namespace(t)
			r := start(t, ns, Config{Window: 100 * time.Millisecond})
			at := time.Now().Add(-time.Hour).Truncate(time.Second)
			step := func() time.Time {
				at = at.Add(10 * time.Second)
				return at
			}
			wants := map[string]*want{}
			wantOf := func(f int) *want {
				if wants[Name(fp(f))] == nil {
					wants[Name(fp(f))] = &want{}
				}
				return wants[Name(fp(f))]
			}
			send := func(o collector.Observation) {
				t.Helper()
				r.obs <- o
			}
			seq := 0
			next := func() int {
				seq++
				return seq
			}

			// The causes first (P1: the cause comes first).
			o := oomCause(next(), step())
			send(o)
			wantOf(web).addOwn(1, o.ObservedAt, o.Occurrence, "web")
			wantOf(web).cause = true
			withDB, withForged := rng.IntN(2) == 0, rng.IntN(2) == 0
			if withDB {
				o := waitingCause(db, "db", next(), step())
				send(o)
				wantOf(db).addOwn(1, o.ObservedAt, o.Occurrence, "db")
				wantOf(db).cause = true
			}
			if withForged {
				o := forgedOOM(forged, next(), step())
				send(o)
				wantOf(forged).addOwn(2, o.ObservedAt, o.Occurrence, "web")
			}

			for range 4 + rng.IntN(8) {
				if rng.IntN(4) == 0 {
					// Another kill of the web container: the cause's own count and lastSeen move.
					o := oomCause(next(), step())
					send(o)
					wantOf(web).addOwn(1, o.ObservedAt, o.Occurrence, "web")
				}
				v := rng.IntN(variants)
				if (v == linkDB && !withDB) || (v == forgedCause && !withForged) {
					v = noCause
				}
				reason := []string{reasonBackOff, reasonUnhealthy}[rng.IntN(2)]
				own := 100 + 2*v + slices.Index([]string{reasonBackOff, reasonUnhealthy}, reason)
				count := int64(1 + rng.IntN(3))
				var o collector.Observation
				switch v {
				case linkWeb:
					o = consequenceOf(own, web, "web", reason, next(), count, step())
				case linkDB:
					o = consequenceOf(own, db, "db", reason, next(), count, step())
				case noCause:
					o = consequenceOf(own, 0, "web", reason, next(), count, step())
				case unknownCause:
					o = consequenceOf(own, missing, "web", reason, next(), count, step())
				case otherSubject:
					o = consequenceOf(own, web, "web", reason, next(), count, step())
					o.Subject = subjectWith(otherSubjectUID)
				case noContainer:
					o = consequenceOf(own, web, "", reason, next(), count, step())
				case forgedCause:
					o = consequenceOf(own, forged, "web", reason, next(), count, step())
				}
				send(o)
				switch v {
				case linkWeb:
					wantOf(web).addAbsorbed(reason, count, o.ObservedAt, o.Occurrence)
					wantOf(web).containers = append(wantOf(web).containers, "web")
				case linkDB:
					wantOf(db).addAbsorbed(reason, count, o.ObservedAt, o.Occurrence)
				default:
					wantOf(own).addOwn(count, o.ObservedAt, o.Occurrence, o.Container)
				}
			}
			settledTo(t, ns, wants)
		})
	}
}

// TestOOMCaseGivesOneIncident is the oom case of the lab with the cause first: an OOMKilled cause followed by
// BackOff and Unhealthy for the same container gives one Incident, whose absorbed tally has both reasons with
// their counts while its count and lastSeen are the cause's only. With room for one Incident, the
// consequences take no place under the cap (point 25).
func TestOOMCaseGivesOneIncident(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond, MaxIncidents: 1})
	refused := testutil.ToFloat64(refusedObservations)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r.obs <- oomCause(1, at)
	r.obs <- consequenceOf(2, 1, "web", reasonBackOff, 2, 3, at.Add(30*time.Second))
	r.obs <- consequenceOf(3, 1, "web", reasonUnhealthy, 3, 5, at.Add(40*time.Second))
	r.obs <- consequenceOf(2, 1, "web", reasonBackOff, 4, 1, at.Add(50*time.Second))
	w := &want{cause: true}
	w.addOwn(1, at, terminationID(1), "web")
	w.addAbsorbed(reasonBackOff, 3, at.Add(30*time.Second), eventID(2))
	w.addAbsorbed(reasonUnhealthy, 5, at.Add(40*time.Second), eventID(3))
	w.addAbsorbed(reasonBackOff, 1, at.Add(50*time.Second), eventID(4))
	settledTo(t, ns, map[string]*want{Name(fp(1)): w})
	if got := testutil.ToFloat64(refusedObservations); got != refused {
		t.Fatalf("%v observations refused at the cap, want none", got-refused)
	}
}

// TestCauseTakesEvidenceInAnyPhase: the phase of the cause's Incident is never read. Escalated, Proposed and
// Resolved causes take the evidence, and evidence alone neither escalates nor reopens a Resolved cause, nor
// moves its count or lastSeen.
func TestCauseTakesEvidenceInAnyPhase(t *testing.T) {
	for _, phase := range []remediationv1alpha1.IncidentPhase{
		remediationv1alpha1.IncidentEscalated, remediationv1alpha1.IncidentProposed, remediationv1alpha1.IncidentResolved,
	} {
		t.Run(string(phase), func(t *testing.T) {
			ns := namespace(t)
			r := start(t, ns, Config{Window: 100 * time.Millisecond})
			at := time.Now().Add(-time.Hour).Truncate(time.Second)
			r.obs <- oomCause(1, at)
			inc := eventually(t, ns, 1, countIs(1))
			inc.Status.Phase = phase
			resolved := metav1.NewTime(at.Add(time.Minute))
			if phase == remediationv1alpha1.IncidentResolved {
				inc.Status.ResolvedTime = &resolved
				meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{Type: ConditionLifecycle,
					Status: metav1.ConditionTrue, Reason: ReasonRequestSucceeded, Message: "set by the test"})
			}
			if err := env.admin.Status().Update(t.Context(), inc); err != nil {
				t.Fatal(err)
			}
			r.obs <- consequenceOf(2, 1, "web", reasonBackOff, 2, 2, at.Add(5*time.Minute))
			got := eventually(t, ns, 1, func(inc *remediationv1alpha1.Incident) bool {
				return len(inc.Status.Absorbed) == 1 && inc.Status.Absorbed[0].Count == 2
			})
			if got.Status.Phase != phase || got.Status.Count != 1 || !got.Status.LastSeen.Time.Equal(at) {
				t.Fatalf("phase %s count %d lastSeen %v, want %s 1 %v", got.Status.Phase, got.Status.Count,
					got.Status.LastSeen.Time, phase, at)
			}
			if meta.FindStatusCondition(got.Status.Conditions, ConditionRecurred) != nil {
				t.Fatal("evidence alone escalated the cause as a recurrence")
			}
			var list remediationv1alpha1.IncidentList
			if err := env.admin.List(t.Context(), &list, client.InNamespace(ns)); err != nil || len(list.Items) != 1 {
				t.Fatalf("%d Incidents (%v), want one", len(list.Items), err)
			}
		})
	}
}

// TestAbsorbedNotCountedTwiceAfterRestart: after a restart the controller loads the absorbed records, so an
// occurrence whose record is kept counts only what is new of it, and Handled answers for it.
func TestAbsorbedNotCountedTwiceAfterRestart(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r.obs <- oomCause(1, at)
	r.obs <- consequenceOf(2, 1, "web", reasonBackOff, 2, 2, at.Add(time.Minute))
	absorbedIs := func(n int64) func(*remediationv1alpha1.Incident) bool {
		return func(inc *remediationv1alpha1.Incident) bool {
			return len(inc.Status.Absorbed) == 1 && inc.Status.Absorbed[0].Count == n
		}
	}
	eventually(t, ns, 1, absorbedIs(2))
	r.stop()

	r = start(t, ns, Config{Window: 100 * time.Millisecond})
	if got, found, err := r.Handled(t.Context(), occurrenceAt(eventID(2), at.Add(time.Minute-RecurAfter))); err != nil || !found || got != 2 {
		t.Fatalf("Handled = %d, %v, %v after a restart; want 2, true", got, found, err)
	}
	again := consequenceOf(2, 1, "web", reasonBackOff, 2, 0, at.Add(2*time.Minute))
	again.Total, again.MayRepeat = 2, true
	r.obs <- again
	again.Total = 3
	r.obs <- again
	eventually(t, ns, 1, absorbedIs(3))
	time.Sleep(300 * time.Millisecond)
	if inc := get(t, ns); inc.Status.Absorbed[0].Count != 3 || inc.Status.Count != 1 {
		t.Fatalf("absorbed %+v count %d after a restart, want 3 and 1", inc.Status.Absorbed, inc.Status.Count)
	}
}

// TestContainerNamesFilteredBeforeTheWrite: a container name that is not a DNS label is left out, and names
// past the cap of 8 are left out, so the status write that counts the observation is never refused.
func TestContainerNamesFilteredBeforeTheWrite(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	bad := immediate(observation(1, 1, 1, 1, at))
	bad.Container = "Web_1"
	r.obs <- bad
	inc := eventually(t, ns, 1, countIs(1))
	if len(inc.Status.Containers) != 0 {
		t.Fatalf("containers %v, want none", inc.Status.Containers)
	}
	for i := range 10 {
		o := observation(1, 10+i, 1, 1, at.Add(time.Duration(i)*time.Second))
		o.Container = fmt.Sprintf("c%d", i)
		r.obs <- o
	}
	inc = eventually(t, ns, 1, countIs(11))
	if len(inc.Status.Containers) != remediationv1alpha1.MaxIncidentContainers {
		t.Fatalf("containers %v, want %d", inc.Status.Containers, remediationv1alpha1.MaxIncidentContainers)
	}
}

// TestFoldedIncidentReopens: an Incident closed as FoldedIntoCause and seen again within 24 hours is reopened
// and analysed, not escalated, also when it was analysed before its cause appeared (point 24, decision 3 of
// 2026-11-15). One resolved by its request keeps the escalation.
func TestFoldedIncidentReopens(t *testing.T) {
	for name, folded := range map[string]bool{"folded": true, "resolved by its request": false} {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			r := start(t, ns, Config{Window: 100 * time.Millisecond})
			at := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
			r.obs <- consequenceOf(1, 0, "web", reasonBackOff, 1, 2, at)
			inc := eventually(t, ns, 1, countIs(2))
			analysed := metav1.NewTime(at.Add(time.Minute))
			resolved := metav1.NewTime(at.Add(10 * time.Minute))
			inc.Status.Phase, inc.Status.ResolvedTime, inc.Status.LastAnalysisTime =
				remediationv1alpha1.IncidentResolved, &resolved, &analysed
			reason := ReasonRequestSucceeded
			if folded {
				reason = ReasonFoldedIntoCause
				inc.Status.FoldedInto = &remediationv1alpha1.IncidentRef{Name: Name(fp(9)),
					UID: "55555555-5555-4555-8555-555555555555"}
			}
			meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{Type: ConditionLifecycle,
				Status: metav1.ConditionTrue, Reason: reason, Message: "set by the test"})
			if err := env.admin.Status().Update(t.Context(), inc); err != nil {
				t.Fatal(err)
			}
			r.obs <- consequenceOf(1, 0, "web", reasonBackOff, 2, 1, at.Add(time.Hour))
			got := eventually(t, ns, 1, countIs(3))
			if !folded {
				if got.Status.Phase != remediationv1alpha1.IncidentEscalated {
					t.Fatalf("phase %s, want Escalated", got.Status.Phase)
				}
				return
			}
			cond := meta.FindStatusCondition(got.Status.Conditions, ConditionLifecycle)
			if got.Status.Phase != remediationv1alpha1.IncidentOpen || cond == nil || cond.Reason != ReasonReopened ||
				got.Status.ResolvedTime != nil || got.Status.FoldedInto != nil {
				t.Fatalf("phase %s lifecycle %+v resolvedTime %v foldedInto %v, want Open, Reopened, none, none",
					got.Status.Phase, cond, got.Status.ResolvedTime, got.Status.FoldedInto)
			}
			if meta.FindStatusCondition(got.Status.Conditions, ConditionRecurred) != nil {
				t.Fatal("a folded Incident was escalated as a recurrence")
			}
		})
	}
}

// TestAbsorbedEvidenceIsInvisibleToTheTrigger is generated: for causes with random histories, adding absorbed
// consequences at any time never changes what the trigger decides (Due, for every phase it reads and many
// clocks) nor when the quiet rule counts the Incident quiet from (quietSince), so an absorbed observation never
// makes the cause due again, restarts no cooldown or debounce and does not keep a cause open.
func TestAbsorbedEvidenceIsInvisibleToTheTrigger(t *testing.T) {
	s := TriggerSettings{Debounce: 2 * time.Minute, SettleLimit: 10 * time.Minute, Cooldown: 30 * time.Minute,
		AnalysisTimeout: 5 * time.Minute, RecentWindow: 30 * time.Minute, MaxAnalysesPerDay: 10}
	rng := rand.New(rand.NewPCG(791, 17)) // #nosec G404 -- a reproducible test corpus, not a secret
	for i := range 200 {
		st := &remediationv1alpha1.IncidentStatus{}
		at := t0
		for j := range 1 + rng.IntN(5) {
			at = at.Add(time.Duration(1+rng.IntN(40)) * time.Minute)
			merge(st, &batch{count: 1, first: at, last: at, cause: true, occurrences: map[string]heldOccurrence{
				terminationID(j): {counted: counted{total: 1, began: at}},
			}})
		}
		if rng.IntN(2) == 0 {
			st.LastAnalysisTime = &metav1.Time{Time: at.Add(-time.Duration(rng.IntN(60)) * time.Minute)}
		}
		st.Phase = []remediationv1alpha1.IncidentPhase{remediationv1alpha1.IncidentOpen,
			remediationv1alpha1.IncidentAnalyzing, remediationv1alpha1.IncidentResolved}[rng.IntN(3)]
		before := &remediationv1alpha1.Incident{Status: *st.DeepCopy()}
		after := &remediationv1alpha1.Incident{Status: *st}
		for k := range 1 + rng.IntN(40) {
			when := at.Add(time.Duration(rng.IntN(120)) * time.Minute)
			reason := []remediationv1alpha1.AbsorbedReason{remediationv1alpha1.AbsorbedBackOff,
				remediationv1alpha1.AbsorbedUnhealthy}[rng.IntN(2)]
			merge(&after.Status, &batch{
				absorbed: map[remediationv1alpha1.AbsorbedReason]tally{reason: {count: 1, last: when}},
				occurrences: map[string]heldOccurrence{
					eventID(1000*i + k): {counted: counted{total: 1, began: when}, absorbed: reason},
				},
			})
		}
		if len(after.Status.Absorbed) == 0 {
			t.Fatalf("case %d: nothing absorbed", i)
		}
		if a, b := quietSince(before, time.Time{}), quietSince(after, time.Time{}); !a.Equal(b) {
			t.Fatalf("case %d: quietSince moved from %v to %v", i, a, b)
		}
		for m := range 60 {
			now := at.Add(time.Duration(m*3) * time.Minute)
			d1, r1 := Due(before, Memory{}, now, s, 0)
			d2, r2 := Due(after, Memory{}, now, s, 0)
			if d1 != d2 || r1 != r2 {
				t.Fatalf("case %d at %v: Due %v %s without evidence, %v %s with", i, now, d1, r1, d2, r2)
			}
		}
	}
}

// TestAbsorbedSurvivesAConflict: another writer changes the cause's Incident between the read and the write
// that counts the evidence. The write is tried again on a fresh read, keeps the other writer's change, and
// counts the evidence once.
func TestAbsorbedSurvivesAConflict(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond, RetryAfter: time.Hour})
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r.obs <- oomCause(1, at)
	eventually(t, ns, 1, countIs(1))
	var once sync.Once
	r.writes.before = func(ctx context.Context, _ *remediationv1alpha1.Incident) {
		once.Do(func() {
			inc := get(t, ns)
			meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
				Type: condReviewed, Status: metav1.ConditionTrue, Reason: "OtherWriter", Message: "written between the read and the write",
			})
			if err := env.admin.Status().Update(ctx, inc); err != nil {
				t.Errorf("updating as another writer: %v", err)
			}
		})
	}
	r.obs <- consequenceOf(2, 1, "web", reasonUnhealthy, 2, 4, at.Add(time.Minute))
	eventually(t, ns, 1, func(inc *remediationv1alpha1.Incident) bool { return len(inc.Status.Absorbed) == 1 })
	time.Sleep(300 * time.Millisecond)
	inc := get(t, ns)
	if inc.Status.Absorbed[0].Count != 4 || inc.Status.Count != 1 ||
		meta.FindStatusCondition(inc.Status.Conditions, condReviewed) == nil {
		t.Fatalf("absorbed %+v count %d conditions %+v: want 4, 1 and the other writer's condition",
			inc.Status.Absorbed, inc.Status.Count, inc.Status.Conditions)
	}
}

// TestRecreatedCauseTakesNoEvidence: a cause's Incident deleted and created again under its name, with another
// UID and no status.cause, is no cause. Once a listing shows it, a consequence linked to its fingerprint opens
// its own Incident instead of being counted on the replacement.
func TestRecreatedCauseTakesNoEvidence(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r.obs <- oomCause(1, at)
	old := eventually(t, ns, 1, func(inc *remediationv1alpha1.Incident) bool { return inc.Status.Cause })
	name := Name(fp(1))
	deadline := time.Now().Add(20 * time.Second)
	for {
		r.mu.Lock()
		uid := r.causes[name].uid
		r.mu.Unlock()
		if uid == old.UID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the controller remembers the cause with UID %q, want %q", uid, old.UID)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := env.admin.Delete(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	again := &remediationv1alpha1.Incident{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: old.Spec}
	if err := env.admin.Create(t.Context(), again); err != nil {
		t.Fatal(err)
	}
	var list remediationv1alpha1.IncidentList
	if err := env.admin.List(t.Context(), &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	r.noteStates(list.Items)

	r.obs <- consequenceOf(2, 1, "web", reasonBackOff, 2, 3, at.Add(time.Minute))
	w := &want{}
	w.addOwn(3, at.Add(time.Minute), eventID(2), "web")
	settledTo(t, ns, map[string]*want{name: {}, Name(fp(2)): w})
}

// TestHeldConsequenceIsAbsorbedWhenItsCauseOpens: a consequence held as a first sighting before its cause's
// Incident existed is counted in full on that Incident once a later observation of it links there, not only
// the increase the later one brings.
func TestHeldConsequenceIsAbsorbedWhenItsCauseOpens(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	first := consequenceOf(2, 1, "web", reasonBackOff, 2, 3, at)
	first.Began = at // seen once, not yet a recurrence: held as a first sighting
	r.obs <- first
	r.obs <- oomCause(1, at.Add(10*time.Second))
	eventually(t, ns, 1, countIs(1))
	later := first
	later.Total, later.Count, later.ObservedAt = 5, 2, at.Add(20*time.Second)
	r.obs <- later
	w := &want{cause: true}
	w.addOwn(1, at.Add(10*time.Second), terminationID(1), "web")
	w.addAbsorbed(reasonBackOff, 5, at.Add(20*time.Second), eventID(2))
	settledTo(t, ns, map[string]*want{Name(fp(1)): w})
}

// startWithAbsorbedDropMark stores the cause Incident of fp(1) with the drop mark of its absorbed list at
// at+30m, as a run before a restart left it, and starts the controller, which loads it.
func startWithAbsorbedDropMark(t *testing.T, at time.Time) *running {
	t.Helper()
	ns := namespace(t)
	inc := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: Name(fp(1))},
		Spec:       specOf(oomCause(1, at)),
	}
	if err := env.admin.Create(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	mark := metav1.NewTime(at.Add(30 * time.Minute))
	inc.Status.Phase, inc.Status.Cause, inc.Status.AbsorbedOccurrencesDroppedThrough =
		remediationv1alpha1.IncidentOpen, true, &mark
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	return start(t, ns, Config{Window: 100 * time.Millisecond})
}

// TestAbsorbedDropMarkAnswersOnlyForItsConsequences: after a restart the drop mark of a cause's absorbed list
// answers only for a consequence linked to that cause's fingerprint. A replayed observation of any other
// occurrence that began before the mark is not taken as counted: one of no class, a consequence of another
// cause, a cause of the same subject, or a consequence of another container of the same workload that was
// never linked to a cause (a sidecar's BackOff).
func TestAbsorbedDropMarkAnswersOnlyForItsConsequences(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r := startWithAbsorbedDropMark(t, at)

	linked := consequenceOf(2, 1, "web", reasonBackOff, 9, 0, at.Add(time.Minute))
	linked.Began = at
	unlinked := linked
	unlinked.CauseFingerprint = fp(3)
	sidecar := consequenceOf(4, 0, "sidecar", reasonBackOff, 8, 0, at.Add(11*time.Minute))
	sidecar.Began, sidecar.Total, sidecar.MayRepeat = at.Add(10*time.Minute), 4, true
	cause := oomCause(7, at)
	for _, tc := range []struct {
		name  string
		o     collector.Observation
		found bool
	}{
		{"an event of another Incident", occurrenceAt(eventID(9), at), false},
		{"a consequence of another cause", unlinked, false},
		{"an unlinked consequence of another container of this subject", sidecar, false},
		{"a cause of this subject", cause, false},
		{"a consequence of this cause", linked, true},
	} {
		got, found, err := r.Handled(t.Context(), tc.o)
		if err != nil || found != tc.found || (found && got != collector.AllHandled) {
			t.Errorf("%s: Handled = %d, %v, %v; want found %v", tc.name, got, found, err, tc.found)
		}
	}
}

// TestAbsorbedDropMarkMissesAReplayWhoseCauseChanged pins the accepted limit of the drop mark's key (ADR-0039,
// 2026-11-16 amendment): after a restart in an OOM kill loop the pod status shows only the newest kill, so the
// collector may replay an old BackOff with another cause fingerprint or none. The mark is keyed by the cause
// fingerprint, so it does not recognise such a replay: Handled answers not found, and the replay goes through
// the gate, where it can open one consequence Incident of its own. This fails toward opening.
func TestAbsorbedDropMarkMissesAReplayWhoseCauseChanged(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r := startWithAbsorbedDropMark(t, at)

	replay := consequenceOf(2, 0, "web", reasonBackOff, 9, 0, at.Add(10*time.Minute))
	replay.Began, replay.Total, replay.MayRepeat = at, 9, true
	otherCause := replay
	otherCause.CauseFingerprint = fp(3)
	for _, tc := range []struct {
		name string
		o    collector.Observation
	}{
		{"no cause fingerprint", replay},
		{"another cause fingerprint", otherCause},
	} {
		if got, found, err := r.Handled(t.Context(), tc.o); err != nil || found {
			t.Errorf("%s: Handled = %d, %v, %v; want not found", tc.name, got, found, err)
		}
	}
	// The same replay with its cause's fingerprint is answered as counted.
	linked := replay
	linked.CauseFingerprint = fp(1)
	if got, found, err := r.Handled(t.Context(), linked); err != nil || !found || got != collector.AllHandled {
		t.Errorf("the replay linked to its cause: Handled = %d, %v, %v; want %d, true", got, found, err,
			collector.AllHandled)
	}
}

// TestAbsorbedDropMarkGoesWithItsIncident: a trigger pass keeps the drop mark of an absorbed list only while
// the Incident of its cause fingerprint is in names, so the map holds no more entries than there are
// Incidents.
func TestAbsorbedDropMarkGoesWithItsIncident(t *testing.T) {
	c, err := New(Config{Namespace: fakeNamespace})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	mark := metav1.NewTime(at.Add(30 * time.Minute))
	cause := remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: fakeNamespace, Name: Name(fp(1))},
		Spec:       specOf(oomCause(1, at)),
		Status: remediationv1alpha1.IncidentStatus{Phase: remediationv1alpha1.IncidentOpen, Cause: true,
			AbsorbedOccurrencesDroppedThrough: &mark},
	}
	c.mu.Lock()
	c.names[cause.Name] = struct{}{}
	c.remember(fp(1), cause.Name, &cause.Status)
	c.mu.Unlock()
	replay := consequenceOf(2, 1, "web", reasonBackOff, 9, 0, at)
	replay.Began = at

	c.noteStates([]remediationv1alpha1.Incident{cause})
	if _, found := c.handled(replay); !found {
		t.Fatal("the mark of an Incident in names was dropped")
	}
	c.mu.Lock()
	delete(c.names, cause.Name)
	c.mu.Unlock()
	c.noteStates(nil)
	if _, found := c.handled(replay); found || len(c.absorbedDroppedThrough) != 0 {
		t.Errorf("after a pass without its Incident: found %v, marks %v; want none", found, c.absorbedDroppedThrough)
	}
}

// TestOverflowOfAConsequenceNamesItsCause: a consequence left uncounted at the cap on occurrences held
// unwritten is logged under the Incident it would have been counted on, its cause's.
func TestOverflowOfAConsequenceNamesItsCause(t *testing.T) {
	c, err := New(Config{Namespace: fakeNamespace, MaxPendingOccurrencesPerIncident: 1})
	if err != nil {
		t.Fatal(err)
	}
	logs := &logLines{}
	ctx := logs.context(t.Context())
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	c.consume(ctx, oomCause(1, at))
	c.consume(ctx, consequenceOf(2, 1, "web", reasonBackOff, 2, 3, at.Add(time.Minute)))
	if got := logs.with("Left new occurrences uncounted", `"incident"="`+Name(fp(1))+`"`, `"held"=1`); len(got) != 1 {
		t.Fatalf("overflow lines naming the cause: %q; all lines %q", got, logs.lines)
	}
}

// TestDropLogsCountTheAbsorbed: the log line of a dropped batch names what it held as evidence, so a batch of
// consequences alone does not read as nothing dropped.
func TestDropLogsCountTheAbsorbed(t *testing.T) {
	c, err := New(Config{Namespace: fakeNamespace})
	if err != nil {
		t.Fatal(err)
	}
	logs := &logLines{}
	at := time.Now().Truncate(time.Second)
	b := &batch{spec: specOf(oomCause(1, at)), occurrences: map[string]heldOccurrence{},
		absorbed: map[remediationv1alpha1.AbsorbedReason]tally{remediationv1alpha1.AbsorbedBackOff: {count: 4, last: at}}}
	c.dropDeleted(logs.context(t.Context()), fp(1), Name(fp(1)), []*batch{b}, &deletedError{name: Name(fp(1))}, true)
	if got := logs.with("Dropped observations of a deleted Incident", `"count"=0`, `"absorbed"=4`); len(got) != 1 {
		t.Fatalf("drop lines with the absorbed count: %q; all lines %q", got, logs.lines)
	}
}

// TestRefusedCauseDropsTheEvidenceHeldForIt: a consequence counted as evidence while the create of its cause's
// Incident is in flight goes with that Incident when the API server refuses the create. It is logged with
// the refusal, and the name is not left behind to be dropped later as a deleted Incident.
func TestRefusedCauseDropsTheEvidenceHeldForIt(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond, RetryAfter: 50 * time.Millisecond})
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	var once sync.Once
	r.writes.create = func(obj client.Object) error {
		var refuse error
		once.Do(func() {
			r.obs <- consequenceOf(2, 1, "web", reasonBackOff, 2, 3, at.Add(time.Minute))
			// The loop takes the next observation only once it consumed the consequence; this one counts
			// nothing.
			r.obs <- observation(9, 9, 0, 0, at)
			refuse = apierrors.NewInvalid(remediationv1alpha1.GroupVersion.WithKind("Incident").GroupKind(),
				obj.GetName(), nil)
		})
		return refuse
	}
	r.obs <- oomCause(1, at)
	deadline := time.Now().Add(20 * time.Second)
	for len(r.logs.with("refuses to store", `"absorbed"=3`)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no refusal logged with the absorbed count; lines %q", r.logs.with())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if got := r.logs.with("deleted Incident"); len(got) != 0 {
		t.Fatalf("an Incident never created was dropped as deleted: %q", got)
	}
	r.mu.Lock()
	_, named := r.names[Name(fp(1))]
	_, cause := r.causes[Name(fp(1))]
	pending := r.pending[fp(1)]
	r.mu.Unlock()
	if named || cause || pending != nil {
		t.Fatalf("after the refused create: name kept %v, cause kept %v, pending %+v", named, cause, pending)
	}
}

// newAt is a controller with no API server, at t0 on its own clock.
func newAt(t *testing.T) (*Controller, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: t0}
	c, err := New(Config{Namespace: "x", Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return c, clock
}

// heldOnce is a BackOff of container web in fingerprint fp(heldF), occurrence eventID(2), linked to the cause
// fingerprint fp(causeF), seen once at at: its occurrence began when it was observed, so it is
// held as a first sighting.
func heldOnce(causeF int, count int64, at time.Time) collector.Observation {
	o := consequenceOf(heldF, causeF, "web", reasonBackOff, 2, count, at)
	o.Began = at
	return o
}

// absorbedOn returns what the pending batch of fp holds as evidence under BackOff, and which occurrences.
func absorbedOn(c *Controller, fp string) (int64, []string) {
	b := c.pending[fp]
	if b == nil {
		return 0, nil
	}
	var ids []string
	for id, o := range b.occurrences {
		if o.absorbed != "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return b.absorbed[remediationv1alpha1.AbsorbedBackOff].count, ids
}

// TestHeldConsequenceIsAbsorbedWhenItsCauseOpensAlone (#820): a consequence seen once, just before its cause,
// is held as a first sighting; the cause opens its Incident and the held consequence is counted there in the
// same pass, with no later observation of it. Its record names the cause's Incident, so Handled answers for it.
func TestHeldConsequenceIsAbsorbedWhenItsCauseOpensAlone(t *testing.T) {
	c, _ := newAt(t)
	c.consume(t.Context(), heldOnce(1, 3, t0))
	if !c.sightings.holds(fp(2)) {
		t.Fatal("the consequence is not held as a first sighting, so the test proves nothing")
	}
	if !c.consume(t.Context(), oomCause(1, t0.Add(time.Second))) {
		t.Error("the cause's Incident is not due at once")
	}
	if n, ids := absorbedOn(c, fp(1)); n != 3 || !slices.Equal(ids, []string{eventID(2)}) {
		t.Fatalf("absorbed on the cause = %d %v, want 3 [%s]", n, ids, eventID(2))
	}
	if c.sightings.holds(fp(2)) || c.sightings.len() != 0 || len(c.sightings.byCause) != 0 {
		t.Fatalf("still held: %d first sightings, index %v", c.sightings.len(), c.sightings.byCause)
	}
	if r := c.records[eventID(2)]; r.incident != Name(fp(1)) || r.total != 3 {
		t.Fatalf("record %+v, want total 3 on %s", r, Name(fp(1)))
	}
	if _, ok := c.pending[fp(2)]; ok {
		t.Fatal("the consequence opened an Incident of its own")
	}
}

// TestHeldConsequenceIsAbsorbedWhenAWaitingCauseOpens (#820): a waiting cause held as a first sighting opens
// its Incident when its repeat proves it recurs, with nothing new to count; the consequence held for it before
// is counted there too.
func TestHeldConsequenceIsAbsorbedWhenAWaitingCauseOpens(t *testing.T) {
	c, clock := newAt(t)
	wait := symptomOf(1, reasonConfig, waitingID(1), 1, fingerprint.SourcePodStatus, "web", collector.ClassCause, t0)
	wait.Began = t0
	c.consume(t.Context(), wait)
	c.consume(t.Context(), heldOnce(1, 2, t0.Add(time.Second)))
	if c.sightings.len() != 2 || len(c.pending) != 0 {
		t.Fatalf("%d first sightings, %d pending batches; want the cause and the consequence held, nothing open",
			c.sightings.len(), len(c.pending))
	}
	repeat := wait
	repeat.Count, repeat.MayRepeat, repeat.ObservedAt = 0, true, t0.Add(RecurAfter)
	clock.set(t0.Add(RecurAfter))
	c.consume(t.Context(), repeat)
	if b := c.pending[fp(1)]; b == nil || !b.cause || b.count != 1 {
		t.Fatalf("the waiting cause did not open its Incident: %+v", b)
	}
	if n, ids := absorbedOn(c, fp(1)); n != 2 || !slices.Equal(ids, []string{eventID(2)}) {
		t.Fatalf("absorbed on the cause = %d %v, want 2 [%s]", n, ids, eventID(2))
	}
	if c.sightings.len() != 0 {
		t.Fatalf("%d first sightings still held", c.sightings.len())
	}
}

// TestHeldConsequenceIsAbsorbedWhenAnIncidentBecomesACause (#820): the Incident of the cause's fingerprint
// exists but is not known as a cause (stored without status.cause), so a consequence seen then is held. The
// cause reported again for that Incident makes it a cause and counts the held consequence there.
func TestHeldConsequenceIsAbsorbedWhenAnIncidentBecomesACause(t *testing.T) {
	c, _ := newAt(t)
	c.names[Name(fp(1))] = struct{}{}
	c.consume(t.Context(), heldOnce(1, 4, t0))
	if !c.sightings.holds(fp(2)) {
		t.Fatal("the consequence is not held, so the test proves nothing")
	}
	c.consume(t.Context(), oomCause(2, t0.Add(time.Second)))
	if n, ids := absorbedOn(c, fp(1)); n != 4 || !slices.Equal(ids, []string{eventID(2)}) {
		t.Fatalf("absorbed on the cause = %d %v, want 4 [%s]", n, ids, eventID(2))
	}
	if c.sightings.len() != 0 {
		t.Fatalf("%d first sightings still held", c.sightings.len())
	}
}

// The consequence fingerprint fp(heldF) and its cause fingerprint fp(heldCauseF) of the tests of a held consequence
// that its cause's Incident is looked up for.
const heldF, heldCauseF = 2, 7

// causeOf is an OOMKilled termination of container web in fingerprint fp(cf), read from pod status and
// observed at at: a cause that opens its Incident at once.
func causeOf(cf int, at time.Time) collector.Observation {
	o := symptomOf(cf, reasonOOM, terminationID(1), 1, fingerprint.SourcePodStatus, "web", collector.ClassCause, at)
	o.Immediate = true
	return o
}

// heldFor fails the test unless the first sighting of fp(heldF) is held and indexed under the cause fp(heldCauseF),
// so that the cause observation that follows looks it up ([Controller.absorbHeldFor]).
func heldFor(t *testing.T, c *Controller) {
	t.Helper()
	if _, ok := c.sightings.byCause[fp(heldCauseF)][fp(heldF)]; !ok || !c.sightings.holds(fp(heldF)) {
		t.Fatalf("fp(%d) is not held under the cause fp(%d), so the case proves nothing", heldF, heldCauseF)
	}
}

// TestHeldConsequenceStaysHeldWithoutItsCause pins what #820 leaves as it was: a held consequence is looked up
// when a cause observation of its cause fingerprint is consumed, and stays held, counted nowhere, when the
// controller's list does not link it to that cause's Incident ([Controller.causeFor]): the Incident is of
// another subject UID, it is not under the cap, it remembers another fingerprint, it was Resolved more than
// RecurrenceWindow before the consequence was seen, or the cause opens no Incident (held as a first sighting
// itself, or refused at the cap). Seen again 2 to 15 minutes later it opens its own Incident through the
// gate, with what was held.
func TestHeldConsequenceStaysHeldWithoutItsCause(t *testing.T) {
	cases := []struct {
		name string
		// setup prepares what the controller knows of the cause's Incident before the consequence is seen.
		setup func(c *Controller, clock *fakeClock)
		// consequence is the consequence held first; cause is then consumed.
		consequence, cause func() collector.Observation
		// atCap leaves out the consequence seen again: its own Incident is refused at the cap too.
		atCap bool
		// check, when set, shows the cause was handled the way the case needs.
		check func(t *testing.T, c *Controller)
	}{{
		name: "another subject UID",
		consequence: func() collector.Observation {
			o := heldOnce(heldCauseF, 1, t0)
			o.Subject = subjectWith(otherSubjectUID)
			return o
		},
		cause: func() collector.Observation { return causeOf(heldCauseF, t0.Add(time.Second)) },
	}, {
		// A cause remembered for a name that left the cap: dropDeleted takes the first occurrence's name out of
		// names when no occurrence is left, and only the deleted occurrence's cause.
		name: "Incident not under the cap",
		setup: func(c *Controller, _ *fakeClock) {
			c.causes[Name(fp(heldCauseF))] = causeIncident{spec: specOf(causeOf(heldCauseF, t0))}
		},
		consequence: func() collector.Observation { return heldOnce(heldCauseF, 1, t0) },
		cause: func() collector.Observation {
			wait := symptomOf(heldCauseF, reasonConfig, waitingID(heldCauseF), 1, fingerprint.SourcePodStatus, "web",
				collector.ClassCause, t0.Add(time.Second))
			wait.Began = wait.ObservedAt
			return wait
		},
	}, {
		name: "Incident of another fingerprint",
		setup: func(c *Controller, _ *fakeClock) {
			c.names[Name(fp(heldCauseF))] = struct{}{}
			c.causes[Name(fp(heldCauseF))] = causeIncident{spec: specOf(causeOf(heldCauseF+1, t0))}
		},
		consequence: func() collector.Observation { return heldOnce(heldCauseF, 1, t0) },
		cause:       func() collector.Observation { return causeOf(heldCauseF, t0.Add(time.Second)) },
	}, {
		// The cause observation is older than the consequence: it is counted on the Resolved Incident (it does
		// not roll), but the consequence, seen more than RecurrenceWindow after the Incident was Resolved, is not.
		name: "Resolved more than RecurrenceWindow before the consequence",
		setup: func(c *Controller, clock *fakeClock) {
			c.names[Name(fp(heldCauseF))] = struct{}{}
			c.causes[Name(fp(heldCauseF))] = causeIncident{spec: specOf(causeOf(heldCauseF, t0))}
			c.resolvedAt[Name(fp(heldCauseF))] = t0
			clock.set(t0.Add(RecurrenceWindow + time.Hour))
		},
		consequence: func() collector.Observation {
			return heldOnce(heldCauseF, 1, t0.Add(RecurrenceWindow+time.Hour))
		},
		cause: func() collector.Observation { return causeOf(heldCauseF, t0.Add(time.Hour)) },
	}, {
		name:        "cause held as a first sighting",
		consequence: func() collector.Observation { return heldOnce(heldCauseF, 1, t0) },
		check: func(t *testing.T, c *Controller) {
			if !c.sightings.holds(fp(heldCauseF)) || len(c.pending) != 0 {
				t.Fatalf("the cause is not held as a first sighting: %d pending batches", len(c.pending))
			}
		},
		cause: func() collector.Observation {
			wait := symptomOf(heldCauseF, reasonConfig, waitingID(heldCauseF), 1, fingerprint.SourcePodStatus, "web",
				collector.ClassCause, t0.Add(time.Second))
			wait.Began = wait.ObservedAt
			return wait
		},
	}, {
		name:  "cause refused at the cap",
		atCap: true,
		check: func(t *testing.T, c *Controller) {
			if !c.refused.has(fp(heldCauseF)) || len(c.pending) != 0 {
				t.Fatalf("the cause is not refused at the cap: %d pending batches", len(c.pending))
			}
		},
		setup: func(c *Controller, _ *fakeClock) {
			c.cfg.MaxIncidents = 1
			c.names[Name(fp(9))] = struct{}{}
		},
		consequence: func() collector.Observation { return heldOnce(heldCauseF, 1, t0) },
		cause:       func() collector.Observation { return causeOf(heldCauseF, t0.Add(time.Second)) },
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, clock := newAt(t)
			if tc.setup != nil {
				tc.setup(c, clock)
			}
			c.consume(t.Context(), tc.consequence())
			heldFor(t, c)
			c.consume(t.Context(), tc.cause())
			if tc.check != nil {
				tc.check(t, c)
			}
			if n, ids := absorbedOn(c, fp(heldCauseF)); n != 0 || len(ids) != 0 {
				t.Fatalf("absorbed on the cause = %d %v, want nothing", n, ids)
			}
			if !c.sightings.holds(fp(heldF)) {
				t.Fatal("the consequence is no longer held")
			}
			if r, ok := c.records[eventID(2)]; ok {
				t.Fatalf("the consequence is recorded on %s", r.incident)
			}
			if tc.atCap {
				return
			}
			again := tc.consequence()
			again.ObservedAt = clock.Now().Add(RecurAfter)
			again.Began, again.Total, again.Count = again.ObservedAt, 3, 2
			clock.set(again.ObservedAt)
			c.consume(t.Context(), again)
			if b := c.pending[fp(heldF)]; b == nil || b.count != 3 || len(b.absorbed) != 0 {
				t.Fatalf("the consequence seen again did not open its own Incident with what was held: %+v", b)
			}
		})
	}
}

// TestHeldConsequenceGoesToTheNewOccurrence (#820): a consequence seen more than RecurrenceWindow after its
// cause's Incident was Resolved is held, and the cause seen again rolls to the next occurrence; the held
// consequence is counted on that occurrence, never on the Resolved one.
func TestHeldConsequenceGoesToTheNewOccurrence(t *testing.T) {
	c, clock := newAt(t)
	old := Name(fp(heldCauseF))
	c.names[old] = struct{}{}
	c.causes[old] = causeIncident{spec: specOf(causeOf(heldCauseF, t0))}
	c.resolvedAt[old] = t0
	at := t0.Add(RecurrenceWindow + time.Hour)
	clock.set(at.Add(time.Second))
	c.consume(t.Context(), heldOnce(heldCauseF, 4, at))
	heldFor(t, c)
	c.consume(t.Context(), causeOf(heldCauseF, at.Add(time.Second)))
	next := remediationv1alpha1.IncidentName(fp(heldCauseF), 2)
	if got := c.nameOf(fp(heldCauseF)); got != next {
		t.Fatalf("the cause did not roll: latest is %s, want %s", got, next)
	}
	if n, ids := absorbedOn(c, fp(heldCauseF)); n != 4 || !slices.Equal(ids, []string{eventID(2)}) {
		t.Fatalf("absorbed on the cause = %d %v, want 4 [%s]", n, ids, eventID(2))
	}
	if r := c.records[eventID(2)]; r.incident != next {
		t.Fatalf("the consequence is recorded on %q, want %s", r.incident, next)
	}
	if c.sightings.len() != 0 {
		t.Fatalf("%d first sightings still held", c.sightings.len())
	}
}

// TestConsequenceJustBeforeItsCauseIsCounted (#820, found by the eval case of #795): the collector releases a
// kubelet consequence linked to its cause at the end of its hold (collector.ConsequenceGrace) a moment before
// the pod reconciler's cause observation reaches the controller, and never sends it again. It is held as a
// first sighting; the cause then opens its Incident, and the stored Incident shows the consequence in its
// absorbed tally, with no Incident of the consequence's own.
func TestConsequenceJustBeforeItsCauseIsCounted(t *testing.T) {
	ns := namespace(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	clock := &fakeClock{now: at}
	r := start(t, ns, Config{Window: 100 * time.Millisecond, Now: clock.Now})
	r.obs <- heldOnce(1, 5, at)
	waitHeld(t, r, "the consequence held as a first sighting", func() bool { return r.sightings.len() == 1 })
	clock.set(at.Add(time.Second))
	r.obs <- oomCause(1, at.Add(time.Second))
	w := &want{cause: true}
	w.addOwn(1, at.Add(time.Second), terminationID(1), "web")
	w.addAbsorbed(reasonBackOff, 5, at, eventID(2))
	settledTo(t, ns, map[string]*want{Name(fp(1)): w})
	// Past the first sighting's window: nothing is left held to open an Incident of its own later.
	clock.set(at.Add(RecurWithin + time.Minute))
	waitHeld(t, r, "no first sighting held", func() bool { return r.sightings.len() == 0 })
	time.Sleep(300 * time.Millisecond)
	settledTo(t, ns, map[string]*want{Name(fp(1)): w})
}

// TestHeldConsequenceNamingItsCauseOnARepeatIsAbsorbed (#820): a consequence held as a first sighting with no
// cause fingerprint, then seen again within RecurAfter naming its cause, takes the cause from the repeat; when
// the cause opens its Incident, what is held is counted there.
func TestHeldConsequenceNamingItsCauseOnARepeatIsAbsorbed(t *testing.T) {
	c, _ := newAt(t)
	c.consume(t.Context(), heldOnce(0, 2, t0))
	repeat := heldOnce(1, 3, t0.Add(time.Second))
	repeat.Count = 1
	c.consume(t.Context(), repeat)
	if !c.sightings.holds(fp(2)) || len(c.pending) != 0 {
		t.Fatal("the consequence is not held, so the test proves nothing")
	}
	c.consume(t.Context(), oomCause(1, t0.Add(2*time.Second)))
	if n, ids := absorbedOn(c, fp(1)); n != 3 || !slices.Equal(ids, []string{eventID(2)}) {
		t.Fatalf("absorbed on the cause = %d %v, want 3 [%s]", n, ids, eventID(2))
	}
	if c.sightings.len() != 0 || len(c.sightings.byCause) != 0 {
		t.Fatalf("still held: %d first sightings, index %v", c.sightings.len(), c.sightings.byCause)
	}
}
