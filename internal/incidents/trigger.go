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
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// ConditionAnalysisDue is the Incident condition that says whether the incident is due for analysis, and
// why (design 3.2). It is a mark only: the analysis itself reads it, and checks the budget again before it
// calls the model.
const ConditionAnalysisDue = "AnalysisDue"

// Reasons of the AnalysisDue condition. The first three come with status True, the others with False.
const (
	// ReasonFirstAnalysis: the incident was never analyzed and has settled.
	ReasonFirstAnalysis = "FirstAnalysis"
	// ReasonNewOccurrence: an occurrence began after the last analysis (a new symptom), even within the
	// cooldown.
	ReasonNewOccurrence = "NewOccurrence"
	// ReasonCooldownExpired: the symptom was seen again after the last analysis and the cooldown has passed.
	ReasonCooldownExpired = "CooldownExpired"

	// ReasonNotObserved: the status records no observation yet.
	ReasonNotObserved = "NotObserved"
	// ReasonAnalyzing: an analysis is running.
	ReasonAnalyzing = "Analyzing"
	// ReasonNoNewData: nothing was seen since the last analysis.
	ReasonNoNewData = "NoNewData"
	// ReasonCoolingDown: the symptom was seen again, but no new occurrence began, and the cooldown since the
	// last analysis has not passed.
	ReasonCoolingDown = "CoolingDown"
	// ReasonDebouncing: the incident would be due, but its last observation is more recent than the debounce.
	ReasonDebouncing = "Debouncing"
	// ReasonAnalysisDisabled: RemediationSettings maxAnalysesPerDay is 0.
	ReasonAnalysisDisabled = "AnalysisDisabled"
	// ReasonDailyBudgetReached: the analyses of the current UTC day reached maxAnalysesPerDay.
	ReasonDailyBudgetReached = "DailyBudgetReached"
	// ReasonSettingsNotFound: the RemediationSettings object does not exist, so the budget is unknown.
	ReasonSettingsNotFound = "SettingsNotFound"
)

// Defaults of the analysis trigger (design 3.2 and 5.4).
const (
	DefaultDebounce          = 45 * time.Second
	DefaultCooldown          = 15 * time.Minute
	DefaultMaxAnalysesPerDay = 100
)

// TriggerSettings are the limits [Due] applies.
type TriggerSettings struct {
	// Debounce is how long an incident must go without a new observation before it is due.
	Debounce time.Duration
	// Cooldown is the least time between two analyses of an incident whose symptom recurs without a new
	// occurrence.
	Cooldown time.Duration
	// MaxAnalysesPerDay is RemediationSettings maxAnalysesPerDay: the analyses allowed per UTC day across all
	// Incidents. 0 disables analysis.
	MaxAnalysesPerDay int
}

// Due decides whether an incident is due for analysis at now, and gives the reason (one of the Reason
// constants). usedToday is the number of analyses already counted against the current UTC day, derived
// from the Incidents' status ([UsedToday]), not from memory. Due reads only its arguments.
//
// An incident is due when it has data the last analysis did not see, it has settled, and the budget allows:
//
//   - never analyzed: due (FirstAnalysis);
//   - an occurrence began after the last analysis: due even within the cooldown (NewOccurrence). Distinct
//     occurrences count, not status.count, which any update of an event series raises (decision on #42);
//   - only seen again (lastSeen after the last analysis), with no new occurrence: due once the cooldown has
//     passed (CooldownExpired), otherwise not (CoolingDown);
//   - nothing seen since the last analysis: not due (NoNewData);
//   - due by the above, but last seen less than the debounce ago: not due yet (Debouncing);
//   - due by the above, but the budget is 0 (AnalysisDisabled) or used up (DailyBudgetReached): not due.
//
// An incident in phase Analyzing is never due. An occurrence without a known begin time is never taken as
// new, since it cannot be placed after the last analysis; its observation still counts as seen again.
func Due(inc *remediationv1alpha1.Incident, now time.Time, s TriggerSettings, usedToday int) (bool, string) {
	st := &inc.Status
	if st.LastSeen == nil {
		return false, ReasonNotObserved
	}
	if st.Phase == remediationv1alpha1.IncidentAnalyzing {
		return false, ReasonAnalyzing
	}
	reason := ReasonFirstAnalysis
	if st.LastAnalysisTime != nil {
		last := st.LastAnalysisTime.Time
		switch {
		case newOccurrences(st, last) > 0:
			reason = ReasonNewOccurrence
		case !st.LastSeen.After(last):
			return false, ReasonNoNewData
		case now.Before(last.Add(s.Cooldown)):
			return false, ReasonCoolingDown
		default:
			reason = ReasonCooldownExpired
		}
	}
	if now.Before(st.LastSeen.Add(s.Debounce)) {
		return false, ReasonDebouncing
	}
	if s.MaxAnalysesPerDay <= 0 {
		return false, ReasonAnalysisDisabled
	}
	if usedToday >= s.MaxAnalysesPerDay {
		return false, ReasonDailyBudgetReached
	}
	return true, reason
}

// newOccurrences returns the number of distinct occurrences that began after since: the records that did,
// plus one when the newest dropped record did (then every kept record did too, as they are newer).
func newOccurrences(st *remediationv1alpha1.IncidentStatus, since time.Time) int {
	n := 0
	for _, r := range st.Occurrences {
		if r.Began != nil && r.Began.After(since) {
			n++
		}
	}
	if st.OccurrencesDroppedThrough != nil && st.OccurrencesDroppedThrough.After(since) {
		n++
	}
	return n
}

// UsedToday returns the analyses counted against the UTC day of now: for each Incident whose last analysis
// falls on that day or later, its lastAnalysisDayCount, at least 1. It reads only the status, so a restart
// does not reset the budget. A last analysis in the future (a clock ahead) counts as today, so a skew can
// only use the budget up sooner.
func UsedToday(items []remediationv1alpha1.Incident, now time.Time) int {
	day := now.UTC().Truncate(24 * time.Hour)
	used := 0
	for i := range items {
		st := &items[i].Status
		if st.LastAnalysisTime == nil || st.LastAnalysisTime.Time.Before(day) {
			continue
		}
		used += max(int(st.LastAnalysisDayCount), 1)
	}
	return used
}
