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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/testfixtures"
)

// triggerNow is the fixed clock of these tests: noon UTC, so a few minutes either way stay on one day.
var triggerNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) *metav1.Time { return &metav1.Time{Time: triggerNow.Add(-d)} }

var triggerSettings = TriggerSettings{Debounce: 45 * time.Second, Cooldown: 15 * time.Minute, MaxAnalysesPerDay: 100}

// incidentStatus returns a status last seen seenAgo ago, with one occurrence that began beganAgo ago, and,
// when analyzedAgo is positive, analyzed that long ago.
func incidentStatus(seenAgo, beganAgo, analyzedAgo time.Duration) remediationv1alpha1.IncidentStatus {
	st := remediationv1alpha1.IncidentStatus{
		Phase:     remediationv1alpha1.IncidentOpen,
		FirstSeen: ago(time.Hour),
		LastSeen:  ago(seenAgo),
		Count:     1,
		Occurrences: []remediationv1alpha1.OccurrenceRecord{
			{ID: eventID(1), Total: 1, Began: ago(beganAgo)},
		},
	}
	if analyzedAgo > 0 {
		st.LastAnalysisTime = ago(analyzedAgo)
		st.LastAnalysisDayCount = 1
	}
	return st
}

func TestDue(t *testing.T) {
	tests := []struct {
		name     string
		status   func() remediationv1alpha1.IncidentStatus
		settings func(*TriggerSettings)
		used     int
		due      bool
		reason   string
	}{
		{
			name:   "nothing observed yet",
			status: func() remediationv1alpha1.IncidentStatus { return remediationv1alpha1.IncidentStatus{} },
			reason: ReasonNotObserved,
		},
		{
			name: "an analysis is running",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Minute, 10*time.Minute, 0)
				st.Phase = remediationv1alpha1.IncidentAnalyzing
				return st
			},
			reason: ReasonAnalyzing,
		},
		{
			name:   "debounce: a new incident seen 10 s ago has not settled",
			status: func() remediationv1alpha1.IncidentStatus { return incidentStatus(10*time.Second, time.Minute, 0) },
			reason: ReasonDebouncing,
		},
		{
			name:   "debounce: a new incident seen exactly the debounce ago is due",
			status: func() remediationv1alpha1.IncidentStatus { return incidentStatus(45*time.Second, time.Minute, 0) },
			due:    true, reason: ReasonFirstAnalysis,
		},
		{
			name: "cooldown: seen again without a new occurrence 5 min after the analysis",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(2*time.Minute, time.Hour, 5*time.Minute)
			},
			reason: ReasonCoolingDown,
		},
		{
			name: "cooldown: a raised series count is not a new occurrence",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(2*time.Minute, time.Hour, 5*time.Minute)
				st.Count = remediationv1alpha1.MaxIncidentCount
				st.Occurrences[0].Total = 1000000
				return st
			},
			reason: ReasonCoolingDown,
		},
		{
			name: "cooldown: an occurrence without a begin time is not new",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(2*time.Minute, time.Hour, 5*time.Minute)
				st.Occurrences = append(st.Occurrences, remediationv1alpha1.OccurrenceRecord{ID: eventID(2), Total: 1})
				return st
			},
			reason: ReasonCoolingDown,
		},
		{
			name: "cooldown expired and the symptom recurred",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(2*time.Minute, time.Hour, 15*time.Minute)
			},
			due: true, reason: ReasonCooldownExpired,
		},
		{
			name: "cooldown expired but nothing seen since the analysis",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(30*time.Minute, time.Hour, 20*time.Minute)
			},
			reason: ReasonNoNewData,
		},
		{
			name: "new symptom during cooldown: an occurrence began after the analysis",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(2*time.Minute, 2*time.Minute, 5*time.Minute)
			},
			due: true, reason: ReasonNewOccurrence,
		},
		{
			name: "new symptom during cooldown, still settling",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(10*time.Second, 20*time.Second, 5*time.Minute)
			},
			reason: ReasonDebouncing,
		},
		{
			name: "new symptom during cooldown, known only from the drop mark",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(2*time.Minute, time.Hour, 5*time.Minute)
				st.OccurrencesDroppedThrough = ago(3 * time.Minute)
				return st
			},
			due: true, reason: ReasonNewOccurrence,
		},
		{
			name:   "budget reached",
			status: func() remediationv1alpha1.IncidentStatus { return incidentStatus(2*time.Minute, time.Hour, 0) },
			used:   100,
			reason: ReasonDailyBudgetReached,
		},
		{
			name:   "budget with one analysis left",
			status: func() remediationv1alpha1.IncidentStatus { return incidentStatus(2*time.Minute, time.Hour, 0) },
			used:   99,
			due:    true, reason: ReasonFirstAnalysis,
		},
		{
			name: "budget reached, but not due anyway: the data reason wins",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(2*time.Minute, time.Hour, 5*time.Minute)
			},
			used:   100,
			reason: ReasonCoolingDown,
		},
		{
			name:     "budget 0 disables analysis",
			status:   func() remediationv1alpha1.IncidentStatus { return incidentStatus(2*time.Minute, time.Hour, 0) },
			settings: func(s *TriggerSettings) { s.MaxAnalysesPerDay = 0 },
			reason:   ReasonAnalysisDisabled,
		},
		{
			name: "budget 0 disables analysis of a new symptom too",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(2*time.Minute, 2*time.Minute, 5*time.Minute)
			},
			settings: func(s *TriggerSettings) { s.MaxAnalysesPerDay = 0 },
			reason:   ReasonAnalysisDisabled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := triggerSettings
			if tt.settings != nil {
				tt.settings(&s)
			}
			inc := &remediationv1alpha1.Incident{Status: tt.status()}
			due, reason := Due(inc, triggerNow, s, tt.used)
			if due != tt.due || reason != tt.reason {
				t.Fatalf("Due = %v, %s; want %v, %s", due, reason, tt.due, tt.reason)
			}
			if _, ok := dueMessages[reason]; !ok {
				t.Fatalf("reason %s has no message", reason)
			}
		})
	}
}

func TestUsedToday(t *testing.T) {
	midnight := triggerNow.Truncate(24 * time.Hour)
	analyzed := func(at time.Time, count int32) remediationv1alpha1.Incident {
		return remediationv1alpha1.Incident{Status: remediationv1alpha1.IncidentStatus{
			LastAnalysisTime: &metav1.Time{Time: at}, LastAnalysisDayCount: count,
		}}
	}
	tests := []struct {
		name  string
		items []remediationv1alpha1.Incident
		want  int
	}{
		{name: "none", want: 0},
		{name: "never analyzed", items: []remediationv1alpha1.Incident{{}}, want: 0},
		{name: "analyzed yesterday", items: []remediationv1alpha1.Incident{analyzed(midnight.Add(-time.Second), 7)}, want: 0},
		{name: "analyzed at midnight", items: []remediationv1alpha1.Incident{analyzed(midnight, 2)}, want: 2},
		{name: "a day count of 0 counts as 1", items: []remediationv1alpha1.Incident{analyzed(triggerNow.Add(-time.Hour), 0)}, want: 1},
		{name: "a clock ahead counts as today", items: []remediationv1alpha1.Incident{analyzed(triggerNow.Add(48*time.Hour), 1)}, want: 1},
		{
			name: "summed over Incidents",
			items: []remediationv1alpha1.Incident{
				analyzed(triggerNow.Add(-time.Hour), 3), analyzed(triggerNow.Add(-time.Minute), 1), analyzed(midnight.Add(-time.Hour), 9),
			},
			want: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UsedToday(tt.items, triggerNow); got != tt.want {
				t.Fatalf("UsedToday = %d, want %d", got, tt.want)
			}
		})
	}
	// A day that is not UTC in local time: the budget day is the UTC day.
	warsaw := time.FixedZone("CET", 3600)
	late := time.Date(2026, 11, 3, 0, 30, 0, 0, warsaw) // 23:30 UTC on 2 November
	if got := UsedToday([]remediationv1alpha1.Incident{analyzed(time.Date(2026, 11, 2, 22, 0, 0, 0, time.UTC), 1)}, late); got != 1 {
		t.Fatalf("UsedToday across a time zone = %d, want 1", got)
	}
}

// createSettings creates the one RemediationSettings object (it must be named default) for the duration of
// the test. The tests of this package do not run in parallel, so no other test sees it.
func createSettings(t *testing.T, maxPerDay int32) {
	t.Helper()
	s := testfixtures.RemediationSettings()
	s.Name = SettingsName
	s.Spec.MaxAnalysesPerDay = new(maxPerDay)
	if err := env.admin.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.admin.Delete(context.Background(), s); err != nil {
			t.Errorf("deleting the RemediationSettings: %v", err)
		}
	})
}

// createIncident creates the Incident of fp(f) with status st, as an earlier analyzer would have left it.
func createIncident(t *testing.T, ns string, f int, st remediationv1alpha1.IncidentStatus) {
	t.Helper()
	inc := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: Name(fp(f))},
		Spec:       specOf(observation(f, f, 1, 1, triggerNow)),
	}
	if err := env.admin.Create(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	st.Occurrences[0].ID = eventID(f)
	inc.Status = st
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
}

func dueIs(due bool, reason string) func(*remediationv1alpha1.Incident) bool {
	return func(inc *remediationv1alpha1.Incident) bool {
		c := meta.FindStatusCondition(inc.Status.Conditions, ConditionAnalysisDue)
		want := metav1.ConditionFalse
		if due {
			want = metav1.ConditionTrue
		}
		return c != nil && c.Status == want && c.Reason == reason && c.Message != ""
	}
}

// listIncidents returns the Incidents of ns by name.
func listIncidents(t *testing.T, ns string) map[string]remediationv1alpha1.Incident {
	t.Helper()
	var list remediationv1alpha1.IncidentList
	if err := env.admin.List(t.Context(), &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	out := map[string]remediationv1alpha1.Incident{}
	for _, inc := range list.Items {
		out[inc.Name] = inc
	}
	return out
}

// Restarting the controller with 50 open Incidents whose cooldowns hold marks none of them due again, and
// the budget used today, read back from their status, still limits the new ones.
func TestRestartKeepsCooldownsAndBudget(t *testing.T) {
	ns := namespace(t)
	// 50 analyzed today + 3 slots: two for the controls below, none for fp(53) and fp(54).
	createSettings(t, 53)
	for f := 1; f <= 50; f++ {
		seen := 2 * time.Minute // seen again within the cooldown, no new occurrence
		if f%2 == 1 {
			seen = 6 * time.Minute // nothing seen since the analysis
		}
		createIncident(t, ns, f, incidentStatus(seen, time.Hour, 5*time.Minute))
	}
	// Controls: fp(51) is past its cooldown with the symptom recurring, fp(52) and fp(53) were never analyzed.
	createIncident(t, ns, 51, incidentStatus(2*time.Minute, time.Hour, 20*time.Minute))
	createIncident(t, ns, 52, incidentStatus(2*time.Minute, time.Hour, 0))
	createIncident(t, ns, 53, incidentStatus(2*time.Minute, time.Hour, 0))

	cfg := Config{TriggerInterval: 50 * time.Millisecond, Now: func() time.Time { return triggerNow }}
	first := start(t, ns, cfg)
	eventually(t, ns, 53, dueIs(false, ReasonDailyBudgetReached))
	eventually(t, ns, 51, dueIs(true, ReasonCooldownExpired))
	eventually(t, ns, 52, dueIs(true, ReasonFirstAnalysis))
	for f := 1; f <= 50; f++ {
		if f%2 == 1 {
			eventually(t, ns, f, dueIs(false, ReasonNoNewData))
		} else {
			eventually(t, ns, f, dueIs(false, ReasonCoolingDown))
		}
	}
	first.stop()
	before := listIncidents(t, ns)

	// A new Incident arrives while the analyzer is down. After the restart the budget is still used up.
	createIncident(t, ns, 54, incidentStatus(2*time.Minute, time.Hour, 0))
	start(t, ns, cfg)
	eventually(t, ns, 54, dueIs(false, ReasonDailyBudgetReached))

	after := listIncidents(t, ns)
	for f := 1; f <= 53; f++ {
		name := Name(fp(f))
		if b, a := before[name], after[name]; a.ResourceVersion != b.ResourceVersion {
			t.Errorf("Incident %d was written again after the restart: %+v", f, a.Status.Conditions)
		}
	}
	for f := 1; f <= 50; f++ {
		inc := after[Name(fp(f))]
		if meta.IsStatusConditionTrue(inc.Status.Conditions, ConditionAnalysisDue) {
			t.Errorf("Incident %d is due again after the restart", f)
		}
	}
}

// Without the RemediationSettings object the budget is unknown, so nothing is due (fail closed); once it
// exists, the Incident becomes due.
func TestNothingIsDueWithoutSettings(t *testing.T) {
	ns := namespace(t)
	createIncident(t, ns, 1, incidentStatus(2*time.Minute, time.Hour, 0))
	start(t, ns, Config{TriggerInterval: 50 * time.Millisecond, Now: func() time.Time { return triggerNow }})
	eventually(t, ns, 1, dueIs(false, ReasonSettingsNotFound))
	createSettings(t, 10)
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
}

// An Incident is marked due once the debounce has passed on the controller's clock, and no longer due once
// an analysis is recorded on it.
func TestDueFollowsTheClockAndTheAnalysis(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	var now atomic.Int64
	now.Store(triggerNow.UnixNano())
	createIncident(t, ns, 1, incidentStatus(10*time.Second, time.Minute, 0))
	start(t, ns, Config{TriggerInterval: 50 * time.Millisecond,
		Now: func() time.Time { return time.Unix(0, now.Load()).UTC() }})
	eventually(t, ns, 1, dueIs(false, ReasonDebouncing))
	now.Store(triggerNow.Add(35 * time.Second).UnixNano())
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))

	// The analysis (#16) records itself; the symptom has not recurred since.
	inc := get(t, ns)
	inc.Status.LastAnalysisTime = &metav1.Time{Time: triggerNow.Add(30 * time.Second)}
	inc.Status.LastAnalysisDayCount = 1
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	eventually(t, ns, 1, dueIs(false, ReasonNoNewData))
}

// refusingMarker fails every status update of one Incident.
type refusingMarker struct {
	client.Client
	name     string
	refusals atomic.Int32
}

func (m *refusingMarker) Status() client.SubResourceWriter {
	return &refusingStatus{SubResourceWriter: m.Client.Status(), m: m}
}

type refusingStatus struct {
	client.SubResourceWriter
	m *refusingMarker
}

func (s *refusingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if obj.GetName() == s.m.name {
		s.m.refusals.Add(1)
		return errors.New("refused by the test")
	}
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

// An Incident marked due whose condition cannot be rewritten keeps its unit of the budget, so a failed
// write never lets more Incidents be due than the budget allows.
func TestFailedMarkKeepsItsBudget(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 1)
	// fp(1) is marked due with a reason that is no longer right, so the pass must rewrite it, and fails.
	createIncident(t, ns, 1, incidentStatus(2*time.Minute, time.Hour, 0))
	inc := get(t, ns)
	meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
		Type: ConditionAnalysisDue, Status: metav1.ConditionTrue, Reason: ReasonCooldownExpired,
		Message: dueMessages[ReasonCooldownExpired], ObservedGeneration: inc.Generation,
	})
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	createIncident(t, ns, 2, incidentStatus(2*time.Minute, time.Hour, 0))
	marker := &refusingMarker{Client: env.admin, name: Name(fp(1))}
	startMarking(t, ns, Config{TriggerInterval: 50 * time.Millisecond, Now: func() time.Time { return triggerNow }}, marker)
	eventually(t, ns, 2, dueIs(false, ReasonDailyBudgetReached))
	if marker.refusals.Load() == 0 {
		t.Fatal("the condition of fp(1) was never rewritten")
	}
}
