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
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corerecord "k8s.io/client-go/tools/record"
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
	Debounce: 45 * time.Second, SettleLimit: 5 * time.Minute, Cooldown: 15 * time.Minute,
	AnalysisTimeout: 10 * time.Minute, RecentWindow: 15 * time.Minute, MaxAnalysesPerDay: 100,
}

// quietThenSeen returns the status of an incident never analyzed, first seen an hour ago, whose first
// occurrence began oldAgo ago and whose newest began burstAgo ago, last seen seenAgo ago.
func quietThenSeen(seenAgo, burstAgo, oldAgo time.Duration) remediationv1alpha1.IncidentStatus {
	st := incidentStatus(seenAgo, burstAgo, 0)
	st.FirstSeen = ago(time.Hour)
	st.Occurrences = append(st.Occurrences, remediationv1alpha1.OccurrenceRecord{ID: eventID(2), Total: 1, Began: ago(oldAgo)})
	return st
}

// conditionID is the occurrence ID of a symptom state of a module's symptom source.
var conditionID = occurrenceCondition + strings.Repeat("c", 64)

// withDueCondition returns st with an AnalysisDue condition of status and reason, set 30 min ago.
func withDueCondition(st remediationv1alpha1.IncidentStatus, status metav1.ConditionStatus, reason string) remediationv1alpha1.IncidentStatus {
	st.Conditions = []metav1.Condition{{Type: ConditionAnalysisDue, Status: status, Reason: reason,
		LastTransitionTime: metav1.Time{Time: triggerNow.Add(-30 * time.Minute)}}}
	return st
}

// incidentStatus returns a status first and last seen seenAgo ago, with one occurrence that began beganAgo
// ago, and, when analyzedAgo is positive, analyzed that long ago.
func incidentStatus(seenAgo, beganAgo, analyzedAgo time.Duration) remediationv1alpha1.IncidentStatus {
	st := remediationv1alpha1.IncidentStatus{
		Phase:     remediationv1alpha1.IncidentOpen,
		FirstSeen: ago(seenAgo),
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
		waiting  time.Duration // how long ago the controller first saw the incident waiting; 0 for never
		quiet    bool          // the controller found the incident gone quiet (Memory.WentQuiet)
		watched  time.Duration // how long ago the passes in a row that looked at it began; 0 for none
		gen      int64         // the Incident's generation
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
			name: "an analysis past the timeout but within the cooldown is not due again",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Minute, 10*time.Minute, 10*time.Minute)
				st.Phase = remediationv1alpha1.IncidentAnalyzing
				return st
			},
			reason: ReasonCoolingDown,
		},
		{
			name: "an analysis past the timeout and the cooldown is due again",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(20*time.Minute, 20*time.Minute, 15*time.Minute)
				st.Phase = remediationv1alpha1.IncidentAnalyzing
				return st
			},
			due: true, reason: ReasonAnalysisTimedOut,
		},
		{
			name: "an analysis timeout longer than the cooldown: due at the timeout",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(30*time.Minute, 30*time.Minute, 20*time.Minute)
				st.Phase = remediationv1alpha1.IncidentAnalyzing
				return st
			},
			settings: func(s *TriggerSettings) { s.AnalysisTimeout = 25 * time.Minute },
			reason:   ReasonAnalyzing,
		},
		{
			name: "an analysis without a start time cannot be placed: not due",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Minute, 10*time.Minute, 0)
				st.Phase = remediationv1alpha1.IncidentAnalyzing
				return st
			},
			reason: ReasonAnalysisStartUnknown,
		},
		{
			// A Resolved Incident is never analyzed again (#251): a recurrence escalates it or opens the next
			// occurrence, both in the controller's write, never through the trigger.
			name: "resolved is never due, even seen again past the cooldown",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(2*time.Minute, time.Hour, 20*time.Minute)
				st.Phase = remediationv1alpha1.IncidentResolved
				return st
			},
			reason: ReasonResolved,
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
			name: "escalated by a recurrence after resolution: its own reason, not the analysis's",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(2*time.Minute, 2*time.Minute, time.Hour)
				st.Phase = remediationv1alpha1.IncidentEscalated
				meta.SetStatusCondition(&st.Conditions, metav1.Condition{
					Type: ConditionRecurred, Status: metav1.ConditionTrue, Reason: ReasonRecurredWithinWindow, Message: "x",
				})
				return st
			},
			reason: ReasonRecurred,
		},
		{
			name:   "not recent: never analyzed and last seen longer ago than the window",
			status: func() remediationv1alpha1.IncidentStatus { return incidentStatus(35*time.Minute, time.Hour, 0) },
			reason: ReasonNotRecent,
		},
		{
			name: "not recent: last seen just over the window ago",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(15*time.Minute+time.Nanosecond, time.Hour, 0)
			},
			reason: ReasonNotRecent,
		},
		{
			name:   "not recent: last seen exactly the window ago is still recent",
			status: func() remediationv1alpha1.IncidentStatus { return incidentStatus(15*time.Minute, time.Hour, 0) },
			due:    true, reason: ReasonFirstAnalysis,
		},
		{
			name:   "not recent: wins over the budget, so it takes no unit",
			status: func() remediationv1alpha1.IncidentStatus { return incidentStatus(35*time.Minute, time.Hour, 0) },
			used:   100,
			reason: ReasonNotRecent,
		},
		{
			name:     "not recent: wins over a disabled budget",
			status:   func() remediationv1alpha1.IncidentStatus { return incidentStatus(35*time.Minute, time.Hour, 0) },
			settings: func(s *TriggerSettings) { s.MaxAnalysesPerDay = 0 },
			reason:   ReasonNotRecent,
		},
		{
			name: "not recent: seen 10 min ago and again now (held), it never went quiet: the settle limit counts from firstSeen",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Minute, time.Hour, 0)
				st.FirstSeen = ago(time.Hour)
				return st
			},
			held: 10 * time.Second,
			due:  true, reason: ReasonFirstAnalysis,
		},
		{
			name:   "gone quiet (#430 item 2): seen again while its observation is held, it debounces",
			status: func() remediationv1alpha1.IncidentStatus { return incidentStatus(35*time.Minute, time.Hour, 0) },
			held:   10 * time.Second,
			quiet:  true,
			reason: ReasonDebouncing,
		},
		{
			name:    "gone quiet: the settle limit counts from the first sight of it waiting again",
			status:  func() remediationv1alpha1.IncidentStatus { return incidentStatus(10*time.Second, time.Hour, 0) },
			quiet:   true,
			waiting: 5 * time.Minute,
			due:     true, reason: ReasonFirstAnalysis,
		},
		{
			name:    "gone quiet: first seen waiting again 4 min ago has not reached the settle limit",
			status:  func() remediationv1alpha1.IncidentStatus { return incidentStatus(10*time.Second, time.Hour, 0) },
			quiet:   true,
			waiting: 4 * time.Minute,
			reason:  ReasonDebouncing,
		},
		{
			name: "gone quiet (#430 item 6): an event series counting up again after NotRecent debounces, after a restart too",
			status: func() remediationv1alpha1.IncidentStatus {
				// One occurrence, begun an hour ago: no gap in the records, only lastSeen moved.
				st := withDueCondition(incidentStatus(10*time.Second, time.Hour, 0), metav1.ConditionFalse, ReasonNotRecent)
				st.FirstSeen = ago(time.Hour)
				return st
			},
			reason: ReasonDebouncing,
		},
		{
			name: "gone quiet: a burst older than the NotRecent spell does not count",
			status: func() remediationv1alpha1.IncidentStatus {
				return withDueCondition(quietThenSeen(10*time.Second, 40*time.Minute, time.Hour), metav1.ConditionFalse, ReasonNotRecent)
			},
			reason: ReasonDebouncing,
		},
		{
			name: "watched (#430 item 5): a gap between begins this process watched is no quiet spell",
			status: func() remediationv1alpha1.IncidentStatus {
				return quietThenSeen(10*time.Second, 10*time.Second, time.Hour)
			},
			watched: 2 * time.Hour,
			due:     true, reason: ReasonFirstAnalysis, // the settle limit counts from firstSeen, an hour ago
		},
		{
			name: "watched: a mark that says due is not taken back for a gap this process watched",
			status: func() remediationv1alpha1.IncidentStatus {
				return withDueCondition(quietThenSeen(10*time.Second, 10*time.Second, time.Hour), metav1.ConditionTrue, ReasonFirstAnalysis)
			},
			watched: time.Hour,
			due:     true, reason: ReasonFirstAnalysis,
		},
		{
			name: "watched: a gap that began before this process watched still counts",
			status: func() remediationv1alpha1.IncidentStatus {
				return quietThenSeen(10*time.Second, 10*time.Second, time.Hour)
			},
			watched: 30 * time.Minute,
			reason:  ReasonDebouncing,
		},
		{
			name: "generation (#430 item 8): a mark of another generation is no mark, so NotRecent",
			status: func() remediationv1alpha1.IncidentStatus {
				return withDueCondition(incidentStatus(35*time.Minute, time.Hour, 0), metav1.ConditionTrue, ReasonFirstAnalysis)
			},
			gen:    2,
			reason: ReasonNotRecent,
		},
		{
			name: "not recent: a new occurrence after going quiet debounces, though firstSeen is past the settle limit",
			status: func() remediationv1alpha1.IncidentStatus {
				return quietThenSeen(10*time.Second, 10*time.Second, time.Hour)
			},
			reason: ReasonDebouncing,
		},
		{
			name: "not recent: after going quiet, the settle limit counts from the new burst",
			status: func() remediationv1alpha1.IncidentStatus {
				return quietThenSeen(10*time.Second, 5*time.Minute, time.Hour)
			},
			due: true, reason: ReasonFirstAnalysis,
		},
		{
			name: "not recent: after going quiet, a burst that began 4 min ago has not reached the settle limit",
			status: func() remediationv1alpha1.IncidentStatus {
				return quietThenSeen(10*time.Second, 4*time.Minute, time.Hour)
			},
			reason: ReasonDebouncing,
		},
		{
			name: "not recent: a new occurrence after going quiet is due once settled",
			status: func() remediationv1alpha1.IncidentStatus {
				return quietThenSeen(45*time.Second, 45*time.Second, time.Hour)
			},
			due: true, reason: ReasonFirstAnalysis,
		},
		{
			name: "not recent: a gap of exactly the window is no quiet spell",
			status: func() remediationv1alpha1.IncidentStatus {
				return quietThenSeen(10*time.Second, 10*time.Second, 15*time.Minute+10*time.Second)
			},
			due: true, reason: ReasonFirstAnalysis, // the settle limit counts from firstSeen, an hour ago
		},
		{
			name: "not recent: a gap just over the window is a quiet spell",
			status: func() remediationv1alpha1.IncidentStatus {
				return quietThenSeen(10*time.Second, 10*time.Second, 15*time.Minute+10*time.Second+time.Nanosecond)
			},
			reason: ReasonDebouncing,
		},
		{
			name: "not recent: a quiet spell before the dropped records counts too",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Second, 10*time.Second, 0)
				st.FirstSeen = ago(2 * time.Hour)
				st.OccurrencesDroppedThrough = ago(time.Hour)
				return st
			},
			reason: ReasonDebouncing,
		},
		{
			name: "not recent: without a window the settle limit counts from firstSeen",
			status: func() remediationv1alpha1.IncidentStatus {
				return quietThenSeen(10*time.Second, 10*time.Second, time.Hour)
			},
			settings: func(s *TriggerSettings) { s.RecentWindow = 0 },
			due:      true, reason: ReasonFirstAnalysis,
		},
		{
			name: "not recent: an Incident already marked due stays due",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(35*time.Minute, time.Hour, 0)
				st.Conditions = []metav1.Condition{{Type: ConditionAnalysisDue, Status: metav1.ConditionTrue,
					Reason: ReasonFirstAnalysis, LastTransitionTime: metav1.Time{Time: triggerNow.Add(-30 * time.Minute)}}}
				return st
			},
			due: true, reason: ReasonFirstAnalysis,
		},
		{
			name: "not recent: a mark taken back by a failed settings read still counts as due",
			status: func() remediationv1alpha1.IncidentStatus {
				return withDueCondition(incidentStatus(35*time.Minute, time.Hour, 0), metav1.ConditionFalse, ReasonSettingsUnreadable)
			},
			due: true, reason: ReasonFirstAnalysis,
		},
		{
			name: "not recent: a mark taken back by a failed budget read still counts as due",
			status: func() remediationv1alpha1.IncidentStatus {
				return withDueCondition(incidentStatus(35*time.Minute, time.Hour, 0), metav1.ConditionFalse, ReasonBudgetReadFailed)
			},
			due: true, reason: ReasonFirstAnalysis,
		},
		{
			name: "not recent: a budget missing is no failed read, so its reason tells nothing of a mark",
			status: func() remediationv1alpha1.IncidentStatus {
				return withDueCondition(incidentStatus(35*time.Minute, time.Hour, 0), metav1.ConditionFalse, ReasonBudgetNotFound)
			},
			reason: ReasonNotRecent,
		},
		{
			name: "not recent: a lasting state seen once an hour ago is recent",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(time.Hour, time.Hour, 0)
				st.Occurrences[0].ID = conditionID
				return st
			},
			due: true, reason: ReasonFirstAnalysis,
		},
		{
			name: "not recent: a lasting state counts only when it is the newest occurrence",
			status: func() remediationv1alpha1.IncidentStatus {
				st := quietThenSeen(35*time.Minute, 35*time.Minute, time.Hour)
				st.Occurrences[1].ID = conditionID
				return st
			},
			reason: ReasonNotRecent,
		},
		{
			name: "not recent: the newest occurrence is found by its begin time, not its place",
			status: func() remediationv1alpha1.IncidentStatus {
				st := quietThenSeen(35*time.Minute, time.Hour, 35*time.Minute)
				st.Occurrences[1].ID = conditionID
				return st
			},
			due: true, reason: ReasonFirstAnalysis,
		},
		{
			name: "not recent: a lastSeen ahead of the clock (skew) is recent",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(35*time.Minute, time.Hour, 0)
				st.LastSeen = ago(-time.Hour)
				return st
			},
			due: true, reason: ReasonFirstAnalysis, // firstSeen is past the settle limit

		},
		{
			name:     "not recent: the window is never shorter than the debounce",
			status:   func() remediationv1alpha1.IncidentStatus { return incidentStatus(45*time.Second, time.Hour, 0) },
			settings: func(s *TriggerSettings) { s.RecentWindow = 10 * time.Second },
			due:      true, reason: ReasonFirstAnalysis,
		},
		{
			name:     "not recent: no window, any incident never analyzed may be due",
			status:   func() remediationv1alpha1.IncidentStatus { return incidentStatus(35*time.Minute, time.Hour, 0) },
			settings: func(s *TriggerSettings) { s.RecentWindow = 0 },
			due:      true, reason: ReasonFirstAnalysis,
		},
		{
			name: "not recent: applies only to a first analysis, not to a re-analysis",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(35*time.Minute, 36*time.Minute, time.Hour)
			},
			due: true, reason: ReasonNewOccurrence,
		},
		{
			name: "not recent: not to a timed-out analysis either",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(time.Hour, time.Hour, 30*time.Minute)
				st.Phase = remediationv1alpha1.IncidentAnalyzing
				return st
			},
			due: true, reason: ReasonAnalysisTimedOut,
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
			name: "settle limit: first seen 5 min ago and still flapping is due",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Second, 5*time.Minute, 0)
				st.FirstSeen = ago(5 * time.Minute)
				return st
			},
			held: 5 * time.Second,
			due:  true, reason: ReasonFirstAnalysis,
		},
		{
			name: "settle limit: first seen just under 5 min ago and still flapping is not due yet",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Second, 5*time.Minute, 0)
				st.FirstSeen = ago(5*time.Minute - time.Second)
				return st
			},
			reason: ReasonDebouncing,
		},
		{
			name: "settle limit: a new occurrence that began 5 min ago, still flapping, is due",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(10*time.Second, 5*time.Minute, 20*time.Minute)
			},
			due: true, reason: ReasonNewOccurrence,
		},
		{
			name: "settle limit: a new occurrence that began 4 min ago, still flapping, is not due yet",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(10*time.Second, 4*time.Minute, 20*time.Minute)
			},
			reason: ReasonDebouncing,
		},
		{
			name: "settle limit: a recurrence of a known occurrence waited 5 min in this process",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(10*time.Second, time.Hour, 20*time.Minute)
			},
			waiting: 5 * time.Minute,
			due:     true, reason: ReasonCooldownExpired,
		},
		{
			name: "settle limit: a recurrence of a known occurrence, first seen waiting 1 min ago",
			status: func() remediationv1alpha1.IncidentStatus {
				return incidentStatus(10*time.Second, time.Hour, 20*time.Minute)
			},
			waiting: time.Minute,
			reason:  ReasonDebouncing,
		},
		{
			name: "settle limit: a begin time before the last analysis does not count",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Second, 30*time.Minute, 20*time.Minute)
				st.FirstSeen = ago(time.Hour)
				return st
			},
			reason: ReasonDebouncing,
		},
		{
			name: "settle limit: past it, the budget still applies",
			status: func() remediationv1alpha1.IncidentStatus {
				st := incidentStatus(10*time.Second, 5*time.Minute, 0)
				st.FirstSeen = ago(10 * time.Minute)
				return st
			},
			used:   100,
			reason: ReasonDailyBudgetReached,
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
			inc.Generation = tt.gen
			m := Memory{WentQuiet: tt.quiet}
			if tt.held > 0 {
				m.Held = triggerNow.Add(-tt.held)
			}
			if tt.waiting > 0 {
				m.Waiting = triggerNow.Add(-tt.waiting)
			}
			if tt.watched > 0 {
				m.WatchedSince = triggerNow.Add(-tt.watched)
			}
			due, reason := Due(inc, m, triggerNow, s, tt.used)
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
	budget := func(data map[string]string) *corev1.ConfigMap { return &corev1.ConfigMap{Data: data} }
	tests := []struct {
		name string
		cm   *corev1.ConfigMap
		used int
		ok   bool
	}{
		{name: "empty, as shipped", cm: budget(nil), used: 0, ok: true},
		{name: "today", cm: budget(usedToday(7)), used: 7, ok: true},
		{name: "today at the largest count", cm: budget(usedToday(10000)), used: 10000, ok: true},
		{name: "yesterday: a new day starts at 0", cm: budget(budgetData(yesterday, "100")), used: 0, ok: true},
		{name: "no ConfigMap fails closed", cm: nil},
		{name: "a future day fails closed", cm: budget(budgetData(tomorrow, "0"))},
		{name: "a day that does not parse fails closed", cm: budget(budgetData("2026-13-45", "0"))},
		{name: "a day with a time fails closed", cm: budget(budgetData("2026-11-03T00:00:00Z", "0"))},
		{name: "a day without leading zeros fails closed", cm: budget(budgetData("2026-11-3", "0"))},
		{name: "a count above the bound fails closed", cm: budget(usedToday(10001))},
		{name: "a negative count fails closed", cm: budget(budgetData(today, "-1"))},
		{name: "a signed count fails closed", cm: budget(budgetData(today, "+1"))},
		{name: "a count with spaces fails closed", cm: budget(budgetData(today, " 1"))},
		{name: "a count that is not a number fails closed", cm: budget(budgetData(today, "one"))},
		{name: "an empty count fails closed", cm: budget(budgetData(today, ""))},
		{name: "a day without a count fails closed", cm: budget(map[string]string{BudgetDayKey: today})},
		{name: "a count without a day fails closed", cm: budget(map[string]string{BudgetUsedKey: "0"})},
		{name: "another key fails closed", cm: budget(map[string]string{BudgetDayKey: today, BudgetUsedKey: "0", "note": "x"})},
		{name: "binary data fails closed", cm: &corev1.ConfigMap{BinaryData: map[string][]byte{"used": []byte("0")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			used, ok := UsedToday(tt.cm, triggerNow)
			if used != tt.used || ok != tt.ok {
				t.Fatalf("UsedToday = %d, %v; want %d, %v", used, ok, tt.used, tt.ok)
			}
		})
	}
	// The budget day is the UTC day, whatever the clock's zone: 00:30 in UTC+1 is still 2 November.
	late := time.Date(2026, 11, 3, 0, 30, 0, 0, time.FixedZone("CET", 3600))
	if used, ok := UsedToday(budget(budgetData(yesterday, "3")), late); used != 3 || !ok {
		t.Fatalf("UsedToday across a time zone = %d, %v; want 3, true", used, ok)
	}
}

// createSettings creates the one RemediationSettings object for the duration of the test. The tests of this
// package do not run in parallel, so no other test sees it.
func createSettings(t *testing.T, maxPerDay int32) *remediationv1alpha1.RemediationSettings {
	t.Helper()
	s := testfixtures.RemediationSettings()
	s.Name = remediationv1alpha1.SettingsName
	s.Spec.Analysis = &remediationv1alpha1.AnalysisSettings{MaxAnalysesPerDay: new(maxPerDay)}
	if err := env.admin.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.admin.Delete(context.Background(), s); err != nil {
			t.Errorf("deleting the RemediationSettings: %v", err)
		}
	})
	return s
}

// createBudget creates the budget ConfigMap in ns with data (nil: empty, as shipped).
func createBudget(t *testing.T, ns string, data map[string]string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: BudgetName}, Data: data}
	if err := env.admin.Create(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	return cm
}

// budgetData is the content of a budget ConfigMap.
func budgetData(day, used string) map[string]string {
	return map[string]string{BudgetDayKey: day, BudgetUsedKey: used}
}

// usedToday is a budget of n analyses on the day of triggerNow.
func usedToday(n int) map[string]string { return budgetData(today, strconv.Itoa(n)) }

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
	if !strings.HasPrefix(st.Occurrences[0].ID, occurrenceCondition) {
		st.Occurrences[0].ID = eventID(f)
	}
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
// testTriggerInterval is the trigger interval of these tests. The pass and the budget read take their
// deadlines from it (180 ms and 135 ms), long enough for envtest under -race.
const testTriggerInterval = 200 * time.Millisecond

var triggerConfig = Config{TriggerInterval: testTriggerInterval, Now: func() time.Time { return triggerNow }}

// Restarting the controller with 50 open Incidents whose cooldowns hold marks none of them due again, and
// the budget used today, read back from the budget ConfigMap, still limits the new ones.
func TestRestartKeepsCooldownsAndBudget(t *testing.T) {
	ns := namespace(t)
	// 51 analyses today (fp(1) to fp(51)) of 53: two slots, for fp(51) and fp(52), none for fp(53) and fp(54).
	createSettings(t, 53)
	createBudget(t, ns, usedToday(51))
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

// The budget lives in its ConfigMap, not in the Incidents: deleting the Incidents analyzed
// today frees none of it.
func TestDeletingIncidentsDoesNotFreeBudget(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 2)
	createBudget(t, ns, usedToday(2))
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
	time.Sleep(10 * triggerConfig.TriggerInterval)
	if inc := eventually(t, ns, 3, func(*remediationv1alpha1.Incident) bool { return true }); meta.IsStatusConditionTrue(
		inc.Status.Conditions, ConditionAnalysisDue) || r.writes.marks.Load() != marks {
		t.Fatalf("deleting the analyzed Incidents freed budget: %+v", inc.Status.Conditions)
	}
}

// A burst whose newest observations the controller still holds unwritten has not settled, even when the
// last observation written is older than the debounce.
func TestHeldObservationsDebounce(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	createBudget(t, ns, nil)
	var now atomic.Int64
	now.Store(triggerNow.UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()).UTC() }
	// A long window holds every observation after the first.
	r := start(t, ns, Config{Window: time.Hour, TriggerInterval: testTriggerInterval, Now: clock})
	r.obs <- immediate(observation(1, 1, 1, 1, triggerNow))
	eventually(t, ns, 1, dueIs(false, ReasonDebouncing))
	// The burst goes on: an observation every 10 s, held unwritten.
	for i := 2; i <= 5; i++ {
		at := triggerNow.Add(time.Duration(i-1) * 10 * time.Second)
		now.Store(at.UnixNano())
		r.obs <- immediate(observation(1, i, 1, 1, at))
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
	createSettings(t, 10)
	createBudget(t, ns, nil)
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
}

// Settings without spec.analysis.maxAnalysesPerDay leave the daily limit unknown, so nothing is due (fail
// closed, not a default of 100); a deprecated spec.maxAnalysesPerDay of 0 wins over the new field's 100.
func TestSettingsWithoutALimitOrWithAnOldZeroMarkNothingDue(t *testing.T) {
	for name, tc := range map[string]struct {
		edit   func(*remediationv1alpha1.RemediationSettings)
		reason string
	}{
		"no analysis": {withoutAnalysis, ReasonSettingsIncomplete},
		"old field 0": {withOldZero, ReasonAnalysisDisabled},
	} {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			createSettings(t, 100)
			createBudget(t, ns, nil)
			createIncident(t, ns, 1, incidentStatus(2*time.Minute, time.Hour, 0))
			var r *editedSettings
			startWith(t, ns, triggerConfig, func(c *Controller, _ *countingClient) {
				r = &editedSettings{Reader: c.cache}
				c.cache = r
			})
			eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
			r.edit.Store(&tc.edit)
			eventually(t, ns, 1, dueIs(false, tc.reason))
			r.edit.Store(nil)
			eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
		})
	}
}

// A budget from a future day cannot be trusted, so nothing is due.
func TestBudgetFromTheFutureFailsClosed(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	createBudget(t, ns, budgetData(tomorrow, "0"))
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
	createSettings(t, 10)
	createBudget(t, ns, nil)
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
	createSettings(t, 10)
	createBudget(t, ns, nil)
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
	settings := createSettings(t, 5)
	createBudget(t, ns, usedToday(5))
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
	settings.Spec.Analysis.MaxAnalysesPerDay = new(int32(0))
	if err := env.admin.Update(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * triggerConfig.TriggerInterval)
	if got := r.writes.marks.Load() - marks; got != 0 {
		t.Fatalf("%d conditions rewritten when only the not-due reason changed", got)
	}
}

// An Incident is marked due once the debounce has passed on the controller's clock, and no longer due once
// an analysis is recorded on it.
func TestDueFollowsTheClockAndTheAnalysis(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	createBudget(t, ns, nil)
	var now atomic.Int64
	now.Store(triggerNow.UnixNano())
	createIncident(t, ns, 1, incidentStatus(10*time.Second, time.Minute, 0))
	start(t, ns, Config{TriggerInterval: testTriggerInterval,
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

// claimed returns st as an analysis claimed it analyzedAgo ago: phase Analyzing, lastAnalysisTime and the
// AnalysisDue condition (not due, Analyzing) for generation 1, as the analysis controller writes it.
func claimed(st remediationv1alpha1.IncidentStatus, analyzedAgo time.Duration) remediationv1alpha1.IncidentStatus {
	st.Phase = remediationv1alpha1.IncidentAnalyzing
	st.LastAnalysisTime = ago(analyzedAgo)
	st.Conditions = []metav1.Condition{{Type: ConditionAnalysisDue, Status: metav1.ConditionFalse,
		Reason: ReasonAnalyzing, Message: dueMessages[ReasonAnalyzing], ObservedGeneration: 1,
		LastTransitionTime: *ago(analyzedAgo)}}
	return st
}

// passCounter is a cache that counts the trigger's passes: each pass lists the Incidents once, first.
type passCounter struct {
	client.Reader
	lists atomic.Int32
}

func (p *passCounter) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	p.lists.Add(1)
	return p.Reader.List(ctx, list, opts...)
}

// afterPasses waits until at least n passes have run from start to end since it was called.
func (p *passCounter) afterPasses(t *testing.T, n int32) {
	t.Helper()
	// The pass running now may have listed already: n + 1 more lists mean n whole passes.
	want := p.lists.Load() + n + 1
	deadline := time.Now().Add(10 * time.Second)
	for p.lists.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d passes ran", n)
		}
		time.Sleep(testTriggerInterval / 4)
	}
}

// setPhase sets the phase of the Incident of fp(1), as the analysis's outcome write does: the phase only,
// AnalysisDue is the trigger's.
func setPhase(t *testing.T, ns string, phase remediationv1alpha1.IncidentPhase) {
	t.Helper()
	inc := get(t, ns)
	inc.Status.Phase = phase
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
}

// The AnalysisDue condition says an analysis is running only while one is (#424). It keeps Analyzing while
// the run is within the analysis timeout; once the run has ended (an outcome written, or the timeout reached)
// the reason the trigger decides replaces it, whatever it is, and with the budget unknown an outcome still
// shows as Proposed or Escalated. The passes after that rewrite nothing.
func TestAnalyzingEndsWithTheRun(t *testing.T) {
	recent := incidentStatus(10*time.Second, time.Hour, 0)
	settled := incidentStatus(2*time.Minute, time.Hour, 0)
	withPhase := func(st remediationv1alpha1.IncidentStatus, phase remediationv1alpha1.IncidentPhase) remediationv1alpha1.IncidentStatus {
		st.Phase = phase
		return st
	}
	for _, tc := range []struct {
		name string
		// settings is the daily limit of the RemediationSettings; -1 means none exist.
		settings int32
		budget   map[string]string
		cfg      Config
		status   remediationv1alpha1.IncidentStatus
		// unreadable makes the reads of the RemediationSettings fail.
		unreadable bool
		want       string
		// then runs after want is reached, and then is the reason expected after it.
		then     func(t *testing.T, ns string)
		thenWant string
	}{
		{name: "running", settings: 10, status: claimed(settled, time.Minute), want: ReasonAnalyzing},
		{name: "running, settings missing", settings: -1, status: claimed(settled, time.Minute), want: ReasonAnalyzing},
		{name: "escalated", settings: 10, status: withPhase(claimed(settled, time.Minute), remediationv1alpha1.IncidentEscalated),
			want: ReasonEscalated},
		{name: "proposed", settings: 10, status: withPhase(claimed(settled, time.Minute), remediationv1alpha1.IncidentProposed),
			want: ReasonProposed},
		{name: "ends while running", settings: 10, status: claimed(settled, time.Minute), want: ReasonAnalyzing,
			then: func(t *testing.T, ns string) { setPhase(t, ns, remediationv1alpha1.IncidentEscalated) }, thenWant: ReasonEscalated},
		{name: "timed out, cooling down", settings: 10, status: claimed(settled, 12*time.Minute), want: ReasonCoolingDown},
		{name: "timed out past the cooldown, debouncing", settings: 10, status: claimed(recent, 20*time.Minute),
			want: ReasonDebouncing},
		{name: "timed out past the cooldown, budget spent", settings: 10, budget: usedToday(10),
			status: claimed(settled, 20*time.Minute), want: ReasonDailyBudgetReached},
		{name: "cooldown shorter than the timeout", settings: 10,
			cfg:    Config{Cooldown: 5 * time.Minute, AnalysisTimeout: 10 * time.Minute},
			status: claimed(recent, 12*time.Minute), want: ReasonDebouncing},
		{name: "timed out, settings missing", settings: -1, status: claimed(settled, 12*time.Minute),
			want: ReasonSettingsNotFound},
		{name: "escalated, settings missing", settings: -1,
			status: withPhase(claimed(settled, time.Minute), remediationv1alpha1.IncidentEscalated), want: ReasonEscalated,
			then: func(t *testing.T, _ string) { createSettings(t, 10) }, thenWant: ReasonEscalated},
		{name: "proposed, settings unreadable", settings: 10, unreadable: true,
			status: withPhase(claimed(settled, time.Minute), remediationv1alpha1.IncidentProposed), want: ReasonProposed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := namespace(t)
			if tc.settings >= 0 {
				createSettings(t, tc.settings)
			}
			createBudget(t, ns, tc.budget)
			createIncident(t, ns, 1, tc.status)
			cfg := tc.cfg
			cfg.TriggerInterval, cfg.Now = triggerConfig.TriggerInterval, triggerConfig.Now
			settings := &failingSettings{Reader: env.admin}
			settings.fail.Store(tc.unreadable)
			passes := &passCounter{Reader: settings}
			r := startWith(t, ns, cfg, func(c *Controller, _ *countingClient) { c.cache = passes })
			eventually(t, ns, 1, dueIs(false, tc.want))
			if tc.then != nil {
				tc.then(t, ns)
				eventually(t, ns, 1, dueIs(false, tc.thenWant))
			}
			marks := r.writes.marks.Load()
			passes.afterPasses(t, 3)
			if got := r.writes.marks.Load() - marks; got != 0 {
				t.Fatalf("%d conditions rewritten after the reason settled", got)
			}
			eventually(t, ns, 1, dueIs(false, cmp.Or(tc.thenWant, tc.want)))
		})
	}
}

// An Incident marked due whose condition cannot be rewritten keeps its unit of the budget, so a failed
// write never lets more Incidents be due than the budget allows.
func TestFailedMarkKeepsItsBudget(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 1)
	createBudget(t, ns, nil)
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
	createSettings(t, 1)
	createBudget(t, ns, nil)
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

// An incident that keeps recurring less than the debounce apart is still due once the settle limit has
// passed since the controller first found it waiting, so a steady trickle of observations cannot keep it
// from analysis. Here the recurrence is of an occurrence the last analysis already counted, so the status
// does not record when it began again and the bound comes from the controller's memory.
func TestFlappingIncidentIsDueWithinTheSettleLimit(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	createBudget(t, ns, nil)
	// Analyzed 20 min ago; nothing seen since.
	createIncident(t, ns, 1, incidentStatus(30*time.Minute, time.Hour, 20*time.Minute))
	var now atomic.Int64
	now.Store(triggerNow.UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()).UTC() }
	r := start(t, ns, Config{Window: time.Hour, SettleLimit: 5 * time.Minute, TriggerInterval: testTriggerInterval, Now: clock})
	eventually(t, ns, 1, dueIs(false, ReasonNoNewData))
	// The same occurrence counts up every 10 s, from triggerNow on.
	recur := func(i int) {
		at := triggerNow.Add(time.Duration(i) * 10 * time.Second)
		now.Store(at.UnixNano())
		o := observation(1, 1, int64(i+2), 1, triggerNow.Add(-time.Hour))
		o.ObservedAt = at
		r.obs <- o
	}
	// The condition keeps its first not-due reason (NoNewData) while the incident debounces, so the test
	// waits for the first recurrence to be written and for a few passes to see it.
	recur(0)
	written := &metav1.Time{Time: triggerNow}
	eventually(t, ns, 1, func(inc *remediationv1alpha1.Incident) bool { return inc.Status.LastSeen.Equal(written) })
	time.Sleep(4 * 50 * time.Millisecond)
	for i := 1; i < 30; i++ { // up to 4 min 50 s
		recur(i)
		time.Sleep(2 * 50 * time.Millisecond)
		if inc := get(t, ns); meta.IsStatusConditionTrue(inc.Status.Conditions, ConditionAnalysisDue) {
			t.Fatalf("at %d s the flapping incident is due before the settle limit: %+v", i*10, inc.Status.Conditions)
		}
	}
	recur(30) // 5 min after the first recurrence, still flapping
	eventually(t, ns, 1, dueIs(true, ReasonCooldownExpired))
}

// Settings applied after Incidents exist (#423): the never-analyzed Incidents last seen longer ago than the
// recent window are not due (NotRecent) and take no unit of the budget, so the recent one is due though it is
// the newest. Each skip is reported once, in an event and the metric, not again on later passes. One of the
// quiet ones seen again is due once it has settled, not at once, though its firstSeen, and the controller's
// first sight of it not due, are past the settle limit.
func TestQuietIncidentsTakeNoBudgetWhenSettingsArriveLate(t *testing.T) {
	ns := namespace(t)
	for f := 1; f <= 3; f++ {
		createIncident(t, ns, f, incidentStatus(35*time.Minute, 35*time.Minute, 0))
	}
	const recent = 4
	createIncident(t, ns, recent, incidentStatus(2*time.Minute, 2*time.Minute, 0))
	var now atomic.Int64
	now.Store(triggerNow.UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()).UTC() }
	rec := corerecord.NewFakeRecorder(20)
	r := start(t, ns, Config{TriggerInterval: testTriggerInterval, Now: clock, Recorder: rec})
	for f := 1; f <= recent; f++ {
		eventually(t, ns, f, dueIs(false, ReasonSettingsNotFound))
	}
	skips := testutil.ToFloat64(notRecentSkips)
	createSettings(t, 2)
	createBudget(t, ns, nil)
	eventually(t, ns, recent, dueIs(true, ReasonFirstAnalysis))
	for f := 1; f <= 3; f++ {
		eventually(t, ns, f, dueIs(false, ReasonNotRecent))
	}
	time.Sleep(4 * testTriggerInterval) // later passes report nothing more
	for range 3 {
		select {
		case e := <-rec.Events:
			if !strings.HasPrefix(e, "Normal AnalysisSkipped ") || !strings.Contains(e, "15m0s") {
				t.Fatalf("event %q", e)
			}
		default:
			t.Fatal("fewer than 3 AnalysisSkipped events")
		}
	}
	select {
	case e := <-rec.Events:
		t.Fatalf("an event more than one per skipped Incident: %q", e)
	default:
	}
	if got := testutil.ToFloat64(notRecentSkips) - skips; got != 3 {
		t.Fatalf("skips counted %v, want 3", got)
	}

	// 10 min later, past the settle limit since the quiet ones were first found not recent, fp(1) is seen
	// again: it debounces (its condition keeps NotRecent meanwhile), then takes the unit left.
	again := triggerNow.Add(10 * time.Minute)
	now.Store(again.UnixNano())
	time.Sleep(4 * testTriggerInterval)
	r.obs <- immediate(observation(1, 99, 1, 1, again))
	written := &metav1.Time{Time: again}
	eventually(t, ns, 1, func(inc *remediationv1alpha1.Incident) bool { return inc.Status.LastSeen.Equal(written) })
	time.Sleep(4 * testTriggerInterval)
	if inc := get(t, ns); meta.IsStatusConditionTrue(inc.Status.Conditions, ConditionAnalysisDue) {
		t.Fatalf("an Incident seen again after going quiet is due before the debounce: %+v", inc.Status.Conditions)
	}
	now.Store(again.Add(45 * time.Second).UnixNano())
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
	for f := 2; f <= 3; f++ {
		eventually(t, ns, f, dueIs(false, ReasonNotRecent))
	}
}

// Settings applied after Incidents exist: a lasting symptom state of a module (a "condition:" occurrence),
// emitted once while it lasts, is not taken as gone though it was seen an hour ago, so it is due; an event seen
// once an hour ago is not (NotRecent).
func TestLastingStateIsNotSkippedWhenSettingsArriveLate(t *testing.T) {
	ns := namespace(t)
	lasting := incidentStatus(time.Hour, time.Hour, 0)
	lasting.Occurrences[0].ID = conditionID
	createIncident(t, ns, 1, lasting)
	// Seen less than QuietBeforeResolved ago, so the closure rules leave it Open.
	createIncident(t, ns, 2, incidentStatus(50*time.Minute, time.Hour, 0))
	start(t, ns, triggerConfig)
	eventually(t, ns, 1, dueIs(false, ReasonSettingsNotFound))
	createSettings(t, 10)
	createBudget(t, ns, nil)
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
	eventually(t, ns, 2, dueIs(false, ReasonNotRecent))
}

// A failed budget read takes the mark that says due back (BudgetReadFailed). Once the budget reads again, an
// Incident quiet longer than the recent window meanwhile is due again, not NotRecent: one failed read does not
// end a pending first analysis.
func TestFailedReadDoesNotEndAPendingFirstAnalysis(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	createBudget(t, ns, nil)
	createIncident(t, ns, 1, incidentStatus(14*time.Minute, time.Hour, 0))
	var now atomic.Int64
	now.Store(triggerNow.UnixNano())
	reader := &failingBudget{Reader: env.admin}
	startWith(t, ns, Config{TriggerInterval: testTriggerInterval, Now: func() time.Time { return time.Unix(0, now.Load()).UTC() }},
		func(c *Controller, _ *countingClient) { c.reader = reader })
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
	reader.fail.Store(true)
	eventually(t, ns, 1, dueIs(false, ReasonBudgetReadFailed))
	now.Store(triggerNow.Add(5 * time.Minute).UnixNano())
	time.Sleep(4 * testTriggerInterval)
	reader.fail.Store(false)
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
}

// An Incident that went quiet and was seen again while no settings existed (so no pass decided it NotRecent)
// is not due at once when the settings arrive, though its firstSeen is past the settle limit: the settle limit
// counts from the new burst, which its records show, so it is due once it has settled.
func TestQuietIncidentSeenAgainBeforeSettingsDebounces(t *testing.T) {
	ns := namespace(t)
	createIncident(t, ns, 1, quietThenSeen(10*time.Second, 10*time.Second, time.Hour))
	var now atomic.Int64
	now.Store(triggerNow.UnixNano())
	start(t, ns, Config{TriggerInterval: testTriggerInterval, Now: func() time.Time { return time.Unix(0, now.Load()).UTC() }})
	eventually(t, ns, 1, dueIs(false, ReasonSettingsNotFound))
	createSettings(t, 10)
	createBudget(t, ns, nil)
	time.Sleep(6 * testTriggerInterval)
	if inc := get(t, ns); meta.IsStatusConditionTrue(inc.Status.Conditions, ConditionAnalysisDue) {
		t.Fatalf("an Incident seen again after going quiet is due before the debounce: %+v", inc.Status.Conditions)
	}
	now.Store(triggerNow.Add(35 * time.Second).UnixNano())
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
}

// When the budget does not cover every Incident that is due, the oldest by firstSeen goes first, whatever
// their names, so new fingerprints cannot push an older incident out of the day's budget.
func TestOldestIncidentTakesTheBudgetFirst(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 1)
	createBudget(t, ns, nil)
	// Give the Incident whose name sorts last the older firstSeen.
	older, newer := 1, 2
	if Name(fp(1)) < Name(fp(2)) {
		older, newer = 2, 1
	}
	st := incidentStatus(2*time.Minute, time.Hour, 0)
	st.FirstSeen = ago(time.Hour)
	createIncident(t, ns, older, st)
	createIncident(t, ns, newer, incidentStatus(2*time.Minute, time.Hour, 0))
	start(t, ns, triggerConfig)
	eventually(t, ns, older, dueIs(true, ReasonFirstAnalysis))
	eventually(t, ns, newer, dueIs(false, ReasonDailyBudgetReached))
}

// Without the budget ConfigMap the budget is unknown, so nothing is due, and the analyzer does not create
// it: deleting it stops analysis instead of resetting the count.
func TestNothingIsDueWithoutTheBudget(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	cm := createBudget(t, ns, nil)
	createIncident(t, ns, 1, incidentStatus(2*time.Minute, time.Hour, 0))
	start(t, ns, triggerConfig)
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
	if err := env.admin.Delete(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	eventually(t, ns, 1, dueIs(false, ReasonBudgetNotFound))
	time.Sleep(10 * triggerConfig.TriggerInterval)
	if err := env.admin.Get(t.Context(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the budget ConfigMap came back: %v", err)
	}
}

// failingBudget is a reader whose reads of the budget ConfigMap fail while fail is set.
type failingBudget struct {
	client.Reader
	fail atomic.Bool
}

func (f *failingBudget) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok && f.fail.Load() {
		return errors.New("refused by the test")
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// When the budget ConfigMap cannot be read, the budget is unknown: an Incident marked due is set not due.
func TestUnreadableBudgetTakesTheMarkBack(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	createBudget(t, ns, nil)
	createIncident(t, ns, 1, incidentStatus(2*time.Minute, time.Hour, 0))
	reader := &failingBudget{Reader: env.admin}
	startWith(t, ns, triggerConfig, func(c *Controller, _ *countingClient) { c.reader = reader })
	eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
	reader.fail.Store(true)
	eventually(t, ns, 1, dueIs(false, ReasonBudgetReadFailed))
}

// hangs blocks until ctx ends, like an API server that accepts a request and never answers.
func hangs(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// hangingGets is a reader whose Gets of objects of type T hang, while hang is set, until their context ends.
type hangingGets[T client.Object] struct {
	client.Reader
	hang atomic.Bool
}

func (h *hangingGets[T]) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(T); ok && h.hang.Load() {
		return hangs(ctx)
	}
	return h.Reader.Get(ctx, key, obj, opts...)
}

// A budget read that never answers ends at ReadTimeout: the pass goes on with the budget unknown, so an
// Incident marked due is set not due, and later passes still run (#243). Without the deadline the read waits
// for the trigger's context, which lasts as long as the controller, and the mark stays.
func TestHangingBudgetReadEndsAtTheDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		reason string
		setup  func(c *Controller) *atomic.Bool
	}{
		"settings": {ReasonSettingsUnreadable, func(c *Controller) *atomic.Bool {
			h := &hangingGets[*remediationv1alpha1.RemediationSettings]{Reader: c.cache}
			c.cache = h
			return &h.hang
		}},
		"budget": {ReasonBudgetReadFailed, func(c *Controller) *atomic.Bool {
			h := &hangingGets[*corev1.ConfigMap]{Reader: c.reader}
			c.reader = h
			return &h.hang
		}},
	} {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			createSettings(t, 10)
			createBudget(t, ns, nil)
			createIncident(t, ns, 1, incidentStatus(2*time.Minute, time.Hour, 0))
			var hang *atomic.Bool
			startWith(t, ns, triggerConfig, func(c *Controller, _ *countingClient) { hang = tc.setup(c) })
			eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
			hang.Store(true)
			eventually(t, ns, 1, dueIs(false, tc.reason))
			// Later passes still run: once the reads answer again, the Incident is due again.
			hang.Store(false)
			eventually(t, ns, 1, dueIs(true, ReasonFirstAnalysis))
		})
	}
}

// hangingList is a cache whose Lists hang until their context ends, counting the calls.
type hangingList struct {
	client.Reader
	calls atomic.Int32
}

func (h *hangingList) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	h.calls.Add(1)
	return hangs(ctx)
}

// A pass that stalls ends at PassTimeout and the next pass runs (#243): here the list from the cache never
// answers, and the loop must still try again. Without the pass deadline the first list holds the loop until
// the controller stops.
func TestHangingPassEndsAtTheDeadline(t *testing.T) {
	c, err := New(Config{Namespace: "ns", TriggerInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	cache := &hangingList{}
	c.cache = cache
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { c.triggerLoop(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for cache.calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("%d passes in 10 s with a 50 ms interval: a stalled pass held the loop", cache.calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// New refuses a pass deadline that is not shorter than the trigger interval, and a budget read deadline that
// is not shorter than the pass, and derives both from a short interval.
func TestNewBoundsTheTriggerDeadlines(t *testing.T) {
	for name, cfg := range map[string]Config{
		"pass equals interval": {TriggerInterval: time.Second, PassTimeout: time.Second},
		"pass over interval":   {TriggerInterval: time.Second, PassTimeout: 2 * time.Second},
		"pass over default":    {PassTimeout: DefaultTriggerInterval},
		"read equals pass":     {PassTimeout: time.Second, ReadTimeout: time.Second},
		"read over derived":    {TriggerInterval: time.Second, ReadTimeout: time.Second},
	} {
		cfg.Namespace = "ns"
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted pass %s and read %s for interval %s", name, cfg.PassTimeout, cfg.ReadTimeout,
				cfg.TriggerInterval)
		}
	}
	c, err := New(Config{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.PassTimeout != DefaultPassTimeout || c.cfg.ReadTimeout != DefaultReadTimeout {
		t.Errorf("defaults: pass %s, read %s", c.cfg.PassTimeout, c.cfg.ReadTimeout)
	}
	c, err = New(Config{Namespace: "ns", TriggerInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.PassTimeout != 900*time.Millisecond || c.cfg.ReadTimeout != 675*time.Millisecond {
		t.Errorf("from a 1 s interval: pass %s, read %s", c.cfg.PassTimeout, c.cfg.ReadTimeout)
	}
}

// New resolves the recent window: the cooldown by default. One shorter than the debounce is passed on as set
// (#430 item 9): Due alone raises it to the debounce, and the skip event names the window Due applies.
func TestNewResolvesTheRecentWindow(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg       Config
		want      time.Duration
		effective string
	}{
		"default":            {Config{}, DefaultCooldown, "15m0s"},
		"follows cooldown":   {Config{Cooldown: 20 * time.Minute}, 20 * time.Minute, "20m0s"},
		"set":                {Config{RecentWindow: 30 * time.Minute}, 30 * time.Minute, "30m0s"},
		"below the debounce": {Config{Debounce: 2 * time.Minute, RecentWindow: time.Minute}, time.Minute, "2m0s"},
	} {
		tc.cfg.Namespace = "ns"
		rec := corerecord.NewFakeRecorder(1)
		tc.cfg.Recorder = rec
		c, err := New(tc.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if c.cfg.RecentWindow != tc.want {
			t.Errorf("%s: recent window %s, want %s", name, c.cfg.RecentWindow, tc.want)
		}
		c.reportNotRecent(&remediationv1alpha1.Incident{})
		select {
		case e := <-rec.Events:
			if !strings.Contains(e, "recent window of "+tc.effective+";") {
				t.Errorf("%s: event %q, want the window %s", name, e, tc.effective)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no event", name)
		}
	}
}

// quietIncident returns an Incident of fp(1) with the given UID, never analyzed, first seen an hour ago and
// last seen 35 min ago, so gone quiet at triggerNow.
func quietIncident(uid types.UID) remediationv1alpha1.Incident {
	inc := remediationv1alpha1.Incident{ObjectMeta: metav1.ObjectMeta{Name: Name(fp(1)), UID: uid},
		Spec: specOf(observation(1, 1, 1, 1, triggerNow)), Status: incidentStatus(35*time.Minute, time.Hour, 0)}
	inc.Status.FirstSeen = ago(time.Hour)
	return inc
}

// The trigger finds an Incident gone quiet before it decides, from the stored lastSeen, on every pass
// (#430 items 2 and 3): it forgets when it first saw it waiting, which belongs to the spell before, and a new
// observation still held unwritten does not let it skip the debounce. What it remembers belongs to one UID:
// an Incident created again under the same name starts with nothing.
func TestWatchFindsAnIncidentGoneQuiet(t *testing.T) {
	c, err := New(Config{Namespace: "ns", Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	s := triggerSettings
	inc := quietIncident("uid-1")
	// Waiting since before an outage of the budget, which wrote nothing while the Incident went quiet.
	c.memory[inc.UID] = &trace{waiting: triggerNow.Add(-time.Hour)}
	c.watch([]remediationv1alpha1.Incident{inc}, triggerNow, s)
	if tr := c.memory[inc.UID]; !tr.waiting.IsZero() || !tr.quiet || !tr.watchedSince.Equal(triggerNow) {
		t.Fatalf("after the quiet spell: %+v", *tr)
	}
	// Seen again: the new observation is held unwritten.
	c.pending[inc.Spec.Fingerprint] = &batch{first: triggerNow.Add(-10 * time.Second), last: triggerNow.Add(-10 * time.Second)}
	used := 0
	if due, reason := c.decider(&inc, triggerNow, s, &used)(&inc); due || reason != ReasonDebouncing {
		t.Fatalf("Due = %v, %s; want false, Debouncing", due, reason)
	}
	// Created again under the same name: nothing of the old one is kept.
	again := quietIncident("uid-2")
	again.Status = incidentStatus(10*time.Second, 10*time.Second, 0)
	c.watch([]remediationv1alpha1.Incident{again}, triggerNow, s)
	if _, ok := c.memory[inc.UID]; ok || c.memory[again.UID].quiet || len(c.memory) != 1 {
		t.Fatalf("the Incident created again inherits: %v", c.memory)
	}
	// Once analyzed, it is no longer remembered gone quiet; once gone, nothing of it is kept.
	inc.Status.LastAnalysisTime = ago(time.Minute)
	c.memory[inc.UID] = &trace{quiet: true}
	c.watch([]remediationv1alpha1.Incident{inc}, triggerNow, s)
	if c.memory[inc.UID].quiet {
		t.Fatal("an analyzed Incident is still remembered gone quiet")
	}
	c.watch(nil, triggerNow, s)
	if len(c.memory) != 0 {
		t.Fatalf("a gone Incident is remembered: %v", c.memory)
	}
}

// An Incident gone quiet and seen again whose new observation stays held unwritten (its write failing) keeps
// the wait of the new burst across passes: the settle limit keeps counting, and it is due once that has
// passed, though every pass still finds its stored lastSeen stale.
func TestHeldObservationKeepsTheSettleLimitCounting(t *testing.T) {
	c, err := New(Config{Namespace: "ns", Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	s := triggerSettings
	inc := quietIncident("uid-1")
	c.watch([]remediationv1alpha1.Incident{inc}, triggerNow, s)
	start := triggerNow
	used := 0
	for i := 0; i <= 31; i++ { // every 10 s for a little over the settle limit, observed every pass
		now := start.Add(time.Duration(i) * 10 * time.Second)
		c.failed[inc.Spec.Fingerprint] = &batch{first: start, last: now}
		c.watch([]remediationv1alpha1.Incident{inc}, now, s)
		due, reason := c.decider(&inc, now, s, &used)(&inc)
		c.rememberWaiting(inc.UID, reason, now)
		if limit := !now.Before(start.Add(s.SettleLimit)); due != limit {
			t.Fatalf("at %s: Due = %v, %s; want due %v", now.Sub(start), due, reason, limit)
		}
	}
}

// overriding is a cache whose List returns the Incidents with edit applied, like a cache behind the API
// server.
type overriding struct {
	client.Reader
	edit func(*remediationv1alpha1.Incident)
}

func (o *overriding) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := o.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if l, ok := list.(*remediationv1alpha1.IncidentList); ok {
		for i := range l.Items {
			o.edit(&l.Items[i])
		}
	}
	return nil
}

// One pass with many quiet Incidents (#430 item 4) writes the mark of the one due first, though it is the
// newest, then at most notRecentWritesPerPass NotRecent conditions; the next pass writes the rest. A skip
// whose fresh copy was seen again since the cache was filled is not written by the deferred writes, so it
// is decided in its place in dueOrder on the next pass. The passes are run one by one, without the loop.
func TestNotRecentWritesComeLastAndAreBounded(t *testing.T) {
	ns := namespace(t)
	const quiet = notRecentWritesPerPass + 5
	for f := 1; f <= quiet; f++ {
		createIncident(t, ns, f, incidentStatus(35*time.Minute, 35*time.Minute, 0))
	}
	const recent, lagging = quiet + 1, quiet + 2
	createIncident(t, ns, recent, incidentStatus(2*time.Minute, 2*time.Minute, 0))
	// Seen 2 min ago, but the cache still shows it quiet.
	createIncident(t, ns, lagging, incidentStatus(2*time.Minute, 2*time.Minute, 0))
	createSettings(t, 10)
	createBudget(t, ns, nil)
	c, err := New(Config{Namespace: ns, Window: time.Second, Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	w := &countingClient{Client: env.admin, mark: func(obj client.Object) error {
		inc := obj.(*remediationv1alpha1.Incident)
		reasons = append(reasons, meta.FindStatusCondition(inc.Status.Conditions, ConditionAnalysisDue).Reason)
		return nil
	}}
	stale := ago(time.Hour)
	c.client, c.reader = w, env.admin
	c.cache = &overriding{Reader: env.admin, edit: func(inc *remediationv1alpha1.Incident) {
		if inc.Name == Name(fp(lagging)) {
			inc.Status.LastSeen, inc.Status.FirstSeen = stale, stale
		}
	}}
	if err := c.markDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 1+notRecentWritesPerPass || reasons[0] != ReasonFirstAnalysis ||
		slices.ContainsFunc(reasons[1:], func(r string) bool { return r != ReasonNotRecent }) {
		t.Fatalf("first pass wrote %v, want FirstAnalysis, then %d NotRecent", reasons, notRecentWritesPerPass)
	}
	reasons = nil
	c.cache = env.admin // the cache caught up
	if err := c.markDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	slices.Sort(reasons)
	want := []string{ReasonFirstAnalysis, ReasonNotRecent, ReasonNotRecent, ReasonNotRecent, ReasonNotRecent, ReasonNotRecent}
	if !slices.Equal(reasons, want) {
		t.Fatalf("second pass wrote %v, want %v", reasons, want)
	}
	eventually(t, ns, lagging, dueIs(true, ReasonFirstAnalysis))
	for f := 1; f <= quiet; f++ {
		eventually(t, ns, f, dueIs(false, ReasonNotRecent))
	}
}

// A quiet spell that ends between two passes, a little after lastSeen crossed the recent window, is never
// stale at a pass instant, but the jump of the stored lastSeen across the window shows it: the new burst
// debounces instead of being due at once (#430 item 2).
func TestQuietSpellBetweenTwoPassesDebounces(t *testing.T) {
	c, err := New(Config{Namespace: "ns", Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	s := triggerSettings
	window := recentWindow(s)
	last := triggerNow.Add(-time.Hour)
	inc := quietIncident("uid-1")
	inc.Status.LastSeen = &metav1.Time{Time: last}
	// Passes every 10 s since before lastSeen, so the gap in the records is one this process watched; the
	// last of them 5 s before lastSeen crosses the window.
	first := last.Add(window - 5*time.Second)
	for p := first.Add(-90 * 10 * time.Second); !p.After(first); p = p.Add(10 * time.Second) {
		c.watch([]remediationv1alpha1.Incident{inc}, p, s)
	}
	// The spell lasts the window and 2 s; the new occurrence is written 7 s after that pass.
	seen := last.Add(window + 2*time.Second)
	inc.Status.LastSeen = &metav1.Time{Time: seen}
	inc.Status.Occurrences = append(inc.Status.Occurrences,
		remediationv1alpha1.OccurrenceRecord{ID: eventID(2), Total: 1, Began: &metav1.Time{Time: seen}})
	next := first.Add(10 * time.Second)
	c.watch([]remediationv1alpha1.Incident{inc}, next, s)
	used := 0
	if due, reason := c.decider(&inc, next, s, &used)(&inc); due || reason != ReasonDebouncing {
		t.Fatalf("Due = %v, %s; want false, Debouncing", due, reason)
	}
}

// A wait from before the quiet spell is forgotten even while a new observation is held unwritten (a failed
// write): it is older than the spell's begin plus the recent window, so it is not the new burst's.
func TestWaitFromBeforeTheSpellIsForgottenWhileHeld(t *testing.T) {
	c, err := New(Config{Namespace: "ns", Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	s := triggerSettings
	inc := quietIncident("uid-1") // last seen 35 min ago
	c.memory[inc.UID] = &trace{waiting: triggerNow.Add(-40 * time.Minute)}
	c.failed[inc.Spec.Fingerprint] = &batch{first: triggerNow.Add(-5 * time.Second), last: triggerNow.Add(-5 * time.Second)}
	c.watch([]remediationv1alpha1.Incident{inc}, triggerNow, s)
	if w := c.memory[inc.UID].waiting; !w.IsZero() {
		t.Errorf("the wait from before the spell (%s) is still remembered", w)
	}
	used := 0
	if due, reason := c.decider(&inc, triggerNow, s, &used)(&inc); due || reason != ReasonDebouncing {
		t.Fatalf("Due = %v, %s; want false, Debouncing", due, reason)
	}
}

// A gap between two lastSeen values shorter than the recent window is no quiet spell, even when the pass that
// reads the new one runs a little after the old one plus the window (#448 review).
func TestShortGapReadByALatePassIsNotQuiet(t *testing.T) {
	c, err := New(Config{Namespace: "ns", Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	s := triggerSettings
	window := recentWindow(s)
	last := triggerNow.Add(-time.Hour)
	inc := quietIncident("uid-1")
	inc.Status.LastSeen = &metav1.Time{Time: last}
	first := last.Add(window - 5*time.Second)
	for p := first.Add(-90 * 10 * time.Second); !p.After(first); p = p.Add(10 * time.Second) {
		c.watch([]remediationv1alpha1.Incident{inc}, p, s)
	}
	seen := last.Add(window - 2*time.Second) // gap 2 s shorter than the window: no spell
	inc.Status.LastSeen = &metav1.Time{Time: seen}
	next := first.Add(10 * time.Second) // 5 s after last + window
	c.watch([]remediationv1alpha1.Incident{inc}, next, s)
	if c.memory[inc.UID].quiet {
		t.Fatalf("gap of window-2s taken as a quiet spell")
	}
}

// failingList is a cache whose Lists fail.
type failingList struct{ client.Reader }

func (failingList) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("list failed")
}

// A pass whose list fails breaks the run of passes, so the lastSeen read before it is not compared with the
// one read after it: activity that went on unwatched is no quiet spell (#448 review).
func TestFailedListForgetsTheSampledLastSeen(t *testing.T) {
	c, err := New(Config{Namespace: "ns", Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	s := triggerSettings
	window := recentWindow(s)
	last := triggerNow.Add(-time.Hour)
	inc := quietIncident("uid-1")
	inc.Status.LastSeen = &metav1.Time{Time: last}
	// Sampled 10 min after lastSeen, so a gap longer than the window fits in the next 7 min, within the window
	// of that sample.
	sampled := last.Add(10 * time.Minute)
	c.watch([]remediationv1alpha1.Incident{inc}, sampled, s)
	c.cache = failingList{}
	if err := c.markDue(t.Context()); err == nil {
		t.Fatal("markDue with a failing list returned no error")
	}
	// Observations went on unwatched while the lists failed, from a minute after the sample: the real gap was
	// 11 min, but the two samples are further apart than the window.
	seen := sampled.Add(7 * time.Minute)
	if !seen.After(last.Add(window)) {
		t.Fatal("setup: the samples are not further apart than the window")
	}
	inc.Status.LastSeen = &metav1.Time{Time: seen}
	c.watch([]remediationv1alpha1.Incident{inc}, seen.Add(time.Second), s)
	if c.memory[inc.UID].quiet {
		t.Fatal("a lastSeen sampled before failed passes was compared with the one after them")
	}
}

// When the stored lastSeen is stale itself, the spell it ends is the later one: a wait seen after the previous
// sample's spell but before the stored lastSeen plus the window belongs to that later spell and is forgotten,
// not kept as the new burst's (#448 review).
func TestStaleStoredLastSeenBeginsTheSpell(t *testing.T) {
	c, err := New(Config{Namespace: "ns", Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	s := triggerSettings
	inc := quietIncident("uid-1") // last seen 35 min ago, stale at triggerNow
	// The previous pass, 10 s ago, read a lastSeen two hours old: the stored one jumped across the window too.
	c.memory[inc.UID] = &trace{lastSeen: triggerNow.Add(-2 * time.Hour), sampled: triggerNow.Add(-10 * time.Second),
		waiting: triggerNow.Add(-time.Hour)}
	c.watch([]remediationv1alpha1.Incident{inc}, triggerNow, s)
	tr := c.memory[inc.UID]
	if !tr.quiet || !tr.waiting.IsZero() {
		t.Fatalf("quiet = %v, waiting = %s; want quiet and the wait from before the stored lastSeen forgotten",
			tr.quiet, tr.waiting)
	}
}

// A lastSeen read while the newest occurrence was a lasting state is frozen while that state lasts, so a new
// occurrence more than the recent window after it is no quiet spell (#448 review).
func TestLastingStateBeforeANewOccurrenceIsNotQuiet(t *testing.T) {
	c, err := New(Config{Namespace: "ns", Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	s := triggerSettings
	window := recentWindow(s)
	last := triggerNow.Add(-time.Hour)
	inc := quietIncident("uid-1")
	inc.Status.LastSeen = &metav1.Time{Time: last}
	inc.Status.Occurrences = []remediationv1alpha1.OccurrenceRecord{
		{ID: occurrenceCondition + "ScalingLimited", Total: 1, Began: &metav1.Time{Time: last}}}
	first := last.Add(window + 10*time.Second)
	for p := last; !p.After(first); p = p.Add(10 * time.Second) {
		c.watch([]remediationv1alpha1.Incident{inc}, p, s)
	}
	seen := first.Add(5 * time.Second)
	inc.Status.LastSeen = &metav1.Time{Time: seen}
	inc.Status.Occurrences = append(inc.Status.Occurrences,
		remediationv1alpha1.OccurrenceRecord{ID: eventID(2), Total: 1, Began: &metav1.Time{Time: seen}})
	c.watch([]remediationv1alpha1.Incident{inc}, first.Add(10*time.Second), s)
	if c.memory[inc.UID].quiet {
		t.Fatal("a frozen lastSeen of a lasting state taken as the begin of a quiet spell")
	}
}

// A lastSeen sampled longer ago than the recent window (the process stalled or was paused) says nothing about
// the activity since: a jump across the window from it is no quiet spell (#448 review).
func TestLastSeenSampledLongAgoIsNotCompared(t *testing.T) {
	c, err := New(Config{Namespace: "ns", Now: func() time.Time { return triggerNow }})
	if err != nil {
		t.Fatal(err)
	}
	s := triggerSettings
	window := recentWindow(s)
	last := triggerNow.Add(-time.Hour)
	inc := quietIncident("uid-1")
	inc.Status.LastSeen = &metav1.Time{Time: last}
	c.watch([]remediationv1alpha1.Incident{inc}, last.Add(time.Second), s)
	// No pass for longer than the window; observations went on meanwhile, the last one 30 s ago.
	now := last.Add(window + time.Minute)
	inc.Status.LastSeen = &metav1.Time{Time: now.Add(-30 * time.Second)}
	c.watch([]remediationv1alpha1.Incident{inc}, now, s)
	if c.memory[inc.UID].quiet {
		t.Fatal("a lastSeen sampled longer ago than the window was compared with the stored one")
	}
}
