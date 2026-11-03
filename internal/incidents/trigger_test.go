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

// The UTC days around triggerNow, as the analysis budget writes them.
const (
	yesterday = "2026-11-02"
	today     = "2026-11-03"
	tomorrow  = "2026-11-04"
)

func ago(d time.Duration) *metav1.Time { return &metav1.Time{Time: triggerNow.Add(-d)} }

var triggerSettings = TriggerSettings{
	Debounce: 45 * time.Second, Cooldown: 15 * time.Minute, AnalysisTimeout: 10 * time.Minute, MaxAnalysesPerDay: 100,
}

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
	}
	return st
}

func TestDue(t *testing.T) {
	tests := []struct {
		name     string
		status   func() remediationv1alpha1.IncidentStatus
		settings func(*TriggerSettings)
		held     time.Duration // how long ago the newest observation held unwritten was made; 0 for none
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
				st := incidentStatus(10*time.Minute, 10*time.Minute, 9*time.Minute)
				st.Phase = remediationv1alpha1.IncidentAnalyzing
				return st
			},
			reason: ReasonAnalyzing,
		},
		{
			name: "an analysis running past the timeout is due again, without a cooldown",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Minute, 10*time.Minute, 10*time.Minute)
				st.Phase = remediationv1alpha1.IncidentAnalyzing
				return st
			},
			due: true, reason: ReasonAnalysisTimedOut,
		},
		{
			name: "an analysis without a start time has timed out",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Minute, 10*time.Minute, 0)
				st.Phase = remediationv1alpha1.IncidentAnalyzing
				return st
			},
			due: true, reason: ReasonAnalysisTimedOut,
		},
		{
			name: "a timed-out analysis still needs the budget",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Minute, 10*time.Minute, time.Hour)
				st.Phase = remediationv1alpha1.IncidentAnalyzing
				return st
			},
			used:   100,
			reason: ReasonDailyBudgetReached,
		},
		{
			name: "proposed: not due while the proposal stands, even with a new occurrence",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(2*time.Minute, 2*time.Minute, time.Hour)
				st.Phase = remediationv1alpha1.IncidentProposed
				return st
			},
			reason: ReasonProposed,
		},
		{
			name: "escalated: not due while a person has it, even with a new occurrence",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(2*time.Minute, 2*time.Minute, time.Hour)
				st.Phase = remediationv1alpha1.IncidentEscalated
				return st
			},
			reason: ReasonEscalated,
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
			name:   "debounce: an observation held unwritten is newer than the debounce",
			status: func() remediationv1alpha1.IncidentStatus { return incidentStatus(2*time.Minute, time.Hour, 0) },
			held:   10 * time.Second,
			reason: ReasonDebouncing,
		},
		{
			name:   "debounce: an observation held unwritten has settled too",
			status: func() remediationv1alpha1.IncidentStatus { return incidentStatus(2*time.Minute, time.Hour, 0) },
			held:   time.Minute,
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
			name: "crash loop: a new occurrence within the cooldown is not due",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(2*time.Minute, 2*time.Minute, 5*time.Minute)
			},
			reason: ReasonCoolingDown,
		},
		{
			name: "new occurrence once the cooldown has passed",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(2*time.Minute, 2*time.Minute, 15*time.Minute)
			},
			due: true, reason: ReasonNewOccurrence,
		},
		{
			name: "new occurrence past the cooldown, still settling",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(10*time.Second, 20*time.Second, 20*time.Minute)
			},
			reason: ReasonDebouncing,
		},
		{
			name: "new occurrence past the cooldown, known only from the drop mark",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(2*time.Minute, time.Hour, 20*time.Minute)
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
				return incidentStatus(2*time.Minute, 2*time.Minute, 20*time.Minute)
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
			var held time.Time
			if tt.held > 0 {
				held = triggerNow.Add(-tt.held)
			}
			due, reason := Due(inc, held, triggerNow, s, tt.used)
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
	settings := func(day string, used int32) *remediationv1alpha1.RemediationSettings {
		s := &remediationv1alpha1.RemediationSettings{}
		if day != "" {
			s.Status.AnalysisBudget = &remediationv1alpha1.AnalysisBudget{Day: day, Used: used}
		}
		return s
	}
	tests := []struct {
		name     string
		settings *remediationv1alpha1.RemediationSettings
		used     int
		ok       bool
	}{
		{name: "no budget yet", settings: settings("", 0), used: 0, ok: true},
		{name: "today", settings: settings(today, 7), used: 7, ok: true},
		{name: "yesterday: a new day starts at 0", settings: settings(yesterday, 100), used: 0, ok: true},
		{name: "a future day fails closed", settings: settings(tomorrow, 0)},
		{name: "a day that does not parse fails closed", settings: settings("2026-13-45", 0)},
		{name: "a count above the schema's bound fails closed", settings: settings(today, 10001)},
		{name: "a negative count fails closed", settings: settings(today, -1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			used, ok := UsedToday(tt.settings, triggerNow)
			if used != tt.used || ok != tt.ok {
				t.Fatalf("UsedToday = %d, %v; want %d, %v", used, ok, tt.used, tt.ok)
			}
		})
	}
	// The budget day is the UTC day, whatever the clock's zone: 00:30 in UTC+1 is still 2 November.
	late := time.Date(2026, 11, 3, 0, 30, 0, 0, time.FixedZone("CET", 3600))
	if used, ok := UsedToday(settings(yesterday, 3), late); used != 3 || !ok {
		t.Fatalf("UsedToday across a time zone = %d, %v; want 3, true", used, ok)
	}
}

// createSettings creates the one RemediationSettings object for the duration of the test, with budget, when
// not nil, as its status. The tests of this package do not run in parallel, so no other test sees it.
func createSettings(t *testing.T, maxPerDay int32, budget *remediationv1alpha1.AnalysisBudget) *remediationv1alpha1.RemediationSettings {
	t.Helper()
	s := testfixtures.RemediationSettings()
	s.Name = remediationv1alpha1.SettingsName
	s.Spec.MaxAnalysesPerDay = new(maxPerDay)
	if err := env.admin.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.admin.Delete(context.Background(), s); err != nil {
			t.Errorf("deleting the RemediationSettings: %v", err)
		}
	})
	if budget != nil {
		s.Status.AnalysisBudget = budget
		if err := env.admin.Status().Update(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// usedToday is a budget of n analyses on the day of triggerNow.
func usedToday(n int32) *remediationv1alpha1.AnalysisBudget {
	return &remediationv1alpha1.AnalysisBudget{Day: today, Used: n}
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

// triggerConfig is a controller config with the fixed clock and a short trigger interval.
var triggerConfig = Config{TriggerInterval: 50 * time.Millisecond, Now: func() time.Time { return triggerNow }}

// Restarting the controller with 50 open Incidents whose cooldowns hold marks none of them due again, and
// the budget used today, read back from the RemediationSettings status, still limits the new ones.
func TestRestartKeepsCooldownsAndBudget(t *testing.T) {
	ns := namespace(t)
	// 51 analyses today (fp(1) to fp(51)) of 53: two slots, for fp(51) and fp(52), none for fp(53) and fp(54).
	createSettings(t, 53, usedToday(51))
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

	first := start(t, ns, triggerConfig)
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
	start(t, ns, triggerConfig)
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

// The budget lives in the RemediationSettings status, not in the Incidents: deleting the Incidents analyzed
// today frees none of it.
func TestDeletingIncidentsDoesNotFreeBudget(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 2, usedToday(2))
	createIncident(t, ns, 1, incidentStatus(30*time.Minute, time.Hour, 20*time.Minute))
	createIncident(t, ns, 2, incidentStatus(30*time.Minute, time.Hour, 20*time.Minute))
	createIncident(t, ns, 3, incidentStatus(2*time.Minute, time.Hour, 0))
	r := start(t, ns, triggerConfig)
	eventually(t, ns, 3, dueIs(false, ReasonDailyBudgetReached))
	for _, f := range []int{1, 2} {
		inc := &remediationv1alpha1.Incident{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: Name(fp(f))}}
		if err := env.admin.Delete(t.Context(), inc); err != nil {
			t.Fatal(err)
		}
	}
	marks := r.writes.marks.Load()
	time.Sleep(20 * triggerConfig.TriggerInterval)
	if inc := eventually(t, ns, 3, func(*remediationv1alpha1.Incident) bool { return true }); meta.IsStatusConditionTrue(
		inc.Status.Conditions, ConditionAnalysisDue) || r.writes.marks.Load() != marks {
		t.Fatalf("deleting the analyzed Incidents freed budget: %+v", inc.Status.Conditions)
	}
}

// A burst whose newest observations the controller still holds unwritten has not settled, even when the
// last observation written is older than the debounce.
func TestHeldObservationsDebounce(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10, nil)
	var now atomic.Int64
	now.Store(triggerNow.UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()).UTC() }
	// A long window holds every observation after the first.
	r := start(t, ns, Config{Window: time.Hour, TriggerInterval: 50 * time.Millisecond, Now: clock})
	r.obs <- observation(1, 1, 1, 1, triggerNow)
	eventually(t, ns, 1, dueIs(false, ReasonDebouncing))
	// The burst goes on: an observation every 10 s, held unwritten.
	for i := 2; i <= 5; i++ {
		at := triggerNow.Add(time.Duration(i-1) * 10 * time.Second)
		now.Store(at.UnixNano())
		r.obs <- observation(1, i, 1, 1, at)
	}
	// 45 s after the first observation, 5 s after the newest: not settled.
	now.Store(triggerNow.Add(45 * time.Second).UnixNano())
	time.Sleep(10 * 50 * time.Millisecond)
	if inc := get(t, ns); !dueIs(false, ReasonDebouncing)(inc) || inc.Status.Count != 1 {
		t.Fatalf("a burst with held observations is due at 45 s: count %d, %+v", inc.Status.Count, inc.Status.Conditions)
	}
	// 45 s after the newest observation it has settled.
	now.Store(triggerNow.Add(85 * time.Second).UnixNano())
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
}

// Without the RemediationSettings object the budget is unknown, so nothing is due (fail closed); once it
// exists, the Incident becomes due.
func TestNothingIsDueWithoutSettings(t *testing.T) {
	ns := namespace(t)
	createIncident(t, ns, 1, incidentStatus(2*time.Minute, time.Hour, 0))
	start(t, ns, triggerConfig)
	eventually(t, ns, 1, dueIs(false, ReasonSettingsNotFound))
	createSettings(t, 10, nil)
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
}

// A budget from a future day cannot be trusted, so nothing is due.
func TestBudgetFromTheFutureFailsClosed(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10, &remediationv1alpha1.AnalysisBudget{Day: tomorrow, Used: 0})
	createIncident(t, ns, 1, incidentStatus(2*time.Minute, time.Hour, 0))
	start(t, ns, triggerConfig)
	eventually(t, ns, 1, dueIs(false, ReasonBudgetUnreadable))
}

// failingSettings is a cache whose reads of the RemediationSettings fail while fail is set.
type failingSettings struct {
	client.Reader
	fail atomic.Bool
}

func (f *failingSettings) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.RemediationSettings); ok && f.fail.Load() {
		return errors.New("refused by the test")
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// When the RemediationSettings cannot be read, the budget is unknown: an Incident marked due is set not due.
func TestUnreadableSettingsTakeTheMarkBack(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10, nil)
	createIncident(t, ns, 1, incidentStatus(2*time.Minute, time.Hour, 0))
	cache := &failingSettings{Reader: env.admin}
	startWith(t, ns, triggerConfig, func(c *Controller, _ *countingClient) { c.cache = cache })
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
	cache.fail.Store(true)
	eventually(t, ns, 1, dueIs(false, ReasonSettingsUnreadable))
}

// An Incident whose name holds another fingerprint is not this controller's to analyze: a mark that says due
// is taken back.
func TestForeignIncidentIsNotDue(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10, nil)
	squat := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: Name(fp(1))},
		Spec:       specOf(observation(2, 1, 1, 1, triggerNow)),
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
	eventually(t, ns, 1, dueIs(false, ReasonForeignFingerprint))
}

// A change that turns the not-due reason of every Incident (here the budget set to 0 while it is used up)
// rewrites no condition: the reason given first stays.
func TestNotDueReasonChangesWriteNothing(t *testing.T) {
	ns := namespace(t)
	settings := createSettings(t, 5, usedToday(5))
	const n = 20
	for f := 1; f <= n; f++ {
		createIncident(t, ns, f, incidentStatus(2*time.Minute, time.Hour, 0))
	}
	r := start(t, ns, triggerConfig)
	for f := 1; f <= n; f++ {
		eventually(t, ns, f, dueIs(false, ReasonDailyBudgetReached))
	}
	marks := r.writes.marks.Load()
	if err := env.admin.Get(t.Context(), client.ObjectKeyFromObject(settings), settings); err != nil {
		t.Fatal(err)
	}
	settings.Spec.MaxAnalysesPerDay = new(int32(0))
	if err := env.admin.Update(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * triggerConfig.TriggerInterval)
	if got := r.writes.marks.Load() - marks; got != 0 {
		t.Fatalf("%d conditions rewritten when only the not-due reason changed", got)
	}
}

// An Incident is marked due once the debounce has passed on the controller's clock, and no longer due once
// an analysis is recorded on it.
func TestDueFollowsTheClockAndTheAnalysis(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10, nil)
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
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	eventually(t, ns, 1, dueIs(false, ReasonNoNewData))
}

// An Incident marked due whose condition cannot be rewritten keeps its unit of the budget, so a failed
// write never lets more Incidents be due than the budget allows.
func TestFailedMarkKeepsItsBudget(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 1, nil)
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
	var refusals atomic.Int32
	startWith(t, ns, triggerConfig, func(_ *Controller, w *countingClient) {
		w.mark = func(obj client.Object) error {
			if obj.GetName() == Name(fp(1)) {
				refusals.Add(1)
				return errors.New("refused by the test")
			}
			return nil
		}
	})
	eventually(t, ns, 2, dueIs(false, ReasonDailyBudgetReached))
	if refusals.Load() == 0 {
		t.Fatal("the condition of fp(1) was never rewritten")
	}
}

// A mark whose answer is lost may have landed: the Incident it meant to mark due takes its unit of the
// budget although its cached condition did not say due, so the next Incident is not marked due as well.
func TestLostMarkAnswerKeepsItsBudget(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 1, nil)
	createIncident(t, ns, 1, incidentStatus(2*time.Minute, time.Hour, 0))
	createIncident(t, ns, 2, incidentStatus(2*time.Minute, time.Hour, 0))
	var lost, overspent atomic.Int32
	startWith(t, ns, triggerConfig, func(_ *Controller, w *countingClient) {
		w.mark = func(obj client.Object) error {
			inc := obj.(*remediationv1alpha1.Incident)
			if obj.GetName() == Name(fp(2)) && meta.IsStatusConditionTrue(inc.Status.Conditions, ConditionAnalysisDue) {
				overspent.Add(1)
			}
			if obj.GetName() == Name(fp(1)) {
				// The update lands, its answer does not.
				lost.Add(1)
				if err := env.admin.Status().Update(context.Background(), obj); err != nil {
					return err
				}
				return errors.New("answer lost by the test")
			}
			return nil
		}
	})
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
	eventually(t, ns, 2, dueIs(false, ReasonDailyBudgetReached))
	time.Sleep(10 * triggerConfig.TriggerInterval)
	if lost.Load() == 0 {
		t.Fatal("no answer was lost")
	}
	if n := overspent.Load(); n > 0 {
		t.Fatalf("fp(2) was marked due %d times beyond the budget of 1", n)
	}
}
