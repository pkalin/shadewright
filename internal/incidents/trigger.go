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
// why (design 3.2). It is a mark only: the analysis itself reads it, and reserves a unit of the daily budget
// ([ReserveAnalysis]) before it calls the model.
const ConditionAnalysisDue = "AnalysisDue"

// Reasons of the AnalysisDue condition. The first four come with status True, the others with False.
const (
	// ReasonFirstAnalysis: the incident was never analyzed and has settled.
	ReasonFirstAnalysis = "FirstAnalysis"
	// ReasonNewOccurrence: an occurrence began after the last analysis (a new symptom) and the cooldown has
	// passed.
	ReasonNewOccurrence = "NewOccurrence"
	// ReasonCooldownExpired: the symptom was seen again after the last analysis and the cooldown has passed.
	ReasonCooldownExpired = "CooldownExpired"
	// ReasonAnalysisTimedOut: the incident has been in phase Analyzing for longer than the analysis timeout
	// since its last analysis began, so that analysis is taken as lost.
	ReasonAnalysisTimedOut = "AnalysisTimedOut"

	// ReasonNotObserved: the status records no observation yet.
	ReasonNotObserved = "NotObserved"
	// ReasonAnalyzing: an analysis is running and has not reached the analysis timeout.
	ReasonAnalyzing = "Analyzing"
	// ReasonProposed: the last analysis made a proposal; leaving phase Proposed is later work.
	ReasonProposed = "Proposed"
	// ReasonEscalated: the last analysis handed the incident to a person; leaving phase Escalated is later
	// work.
	ReasonEscalated = "Escalated"
	// ReasonNoNewData: nothing was seen since the last analysis.
	ReasonNoNewData = "NoNewData"
	// ReasonCoolingDown: the incident has data the last analysis did not see, but the cooldown since that
	// analysis has not passed.
	ReasonCoolingDown = "CoolingDown"
	// ReasonDebouncing: the incident would be due, but its last observation, written or still held by the
	// controller, is more recent than the debounce.
	ReasonDebouncing = "Debouncing"
	// ReasonAnalysisDisabled: RemediationSettings maxAnalysesPerDay is 0.
	ReasonAnalysisDisabled = "AnalysisDisabled"
	// ReasonDailyBudgetReached: the analyses of the current UTC day reached maxAnalysesPerDay.
	ReasonDailyBudgetReached = "DailyBudgetReached"
	// ReasonBudgetUnreadable: the analysis budget in the RemediationSettings status is from a future day or
	// out of bounds, so the budget is taken as spent.
	ReasonBudgetUnreadable = "BudgetUnreadable"
	// ReasonSettingsNotFound: the RemediationSettings object does not exist, so the budget is unknown.
	ReasonSettingsNotFound = "SettingsNotFound"
	// ReasonSettingsUnreadable: the RemediationSettings object could not be read, so the budget is unknown.
	ReasonSettingsUnreadable = "SettingsUnreadable"
	// ReasonForeignFingerprint: the Incident's name belongs to another fingerprint, so this controller does
	// not analyze it.
	ReasonForeignFingerprint = "ForeignFingerprint"
)

// Defaults of the analysis trigger (design 3.2 and 5.4).
const (
	DefaultDebounce          = 45 * time.Second
	DefaultCooldown          = 15 * time.Minute
	DefaultAnalysisTimeout   = 10 * time.Minute
	DefaultMaxAnalysesPerDay = 100
)

// TriggerSettings are the limits [Due] applies.
type TriggerSettings struct {
	// Debounce is how long an incident must go without a new observation before it is due.
	Debounce time.Duration
	// Cooldown is the least time between the start of an analysis of an incident and the next one, whatever
	// new data arrives.
	Cooldown time.Duration
	// AnalysisTimeout is how long an incident may stay in phase Analyzing after its last analysis began
	// before it is due again.
	AnalysisTimeout time.Duration
	// MaxAnalysesPerDay is RemediationSettings maxAnalysesPerDay: the analyses allowed per UTC day across all
	// Incidents. 0 disables analysis.
	MaxAnalysesPerDay int
}

// Due decides whether an incident is due for analysis at now, and gives the reason (one of the Reason
// constants). held is the newest observation of the incident the controller holds but has not written yet
// (zero when none). usedToday is the number of analyses already counted against the current UTC day, read
// from the RemediationSettings status ([UsedToday]), not from memory. Due reads only its arguments.
//
// An incident is due when it has data the last analysis did not see, it has settled, the cooldown has
// passed and the budget allows:
//
//   - in phase Proposed or Escalated: not due (Proposed, Escalated);
//   - in phase Analyzing: not due (Analyzing) until AnalysisTimeout after lastAnalysisTime, then due again
//     (AnalysisTimedOut), without a cooldown; without a lastAnalysisTime it has timed out;
//   - never analyzed: due (FirstAnalysis);
//   - nothing seen since the last analysis: not due (NoNewData);
//   - seen again, within the cooldown since the last analysis: not due (CoolingDown), even with a new
//     occurrence, so a crash loop is analyzed at most once per cooldown;
//   - past the cooldown: due, NewOccurrence when an occurrence began after the last analysis (distinct
//     occurrences count, not status.count, which any update of an event series raises), else
//     CooldownExpired;
//   - due by the above, but last seen (in the status or held) less than the debounce ago: not due yet
//     (Debouncing);
//   - due by the above, but the budget is 0 (AnalysisDisabled) or used up (DailyBudgetReached): not due.
//
// An occurrence without a known begin time is never taken as new, since it cannot be placed after the last
// analysis; its observation still counts as seen again.
func Due(inc *remediationv1alpha1.Incident, held, now time.Time, s TriggerSettings, usedToday int) (bool, string) {
	st := &inc.Status
	if st.LastSeen == nil {
		return false, ReasonNotObserved
	}
	reason := ReasonFirstAnalysis
	switch st.Phase {
	case remediationv1alpha1.IncidentProposed:
		return false, ReasonProposed
	case remediationv1alpha1.IncidentEscalated:
		return false, ReasonEscalated
	case remediationv1alpha1.IncidentAnalyzing:
		if st.LastAnalysisTime != nil && now.Before(st.LastAnalysisTime.Add(s.AnalysisTimeout)) {
			return false, ReasonAnalyzing
		}
		reason = ReasonAnalysisTimedOut
	default:
		if st.LastAnalysisTime != nil {
			last := st.LastAnalysisTime.Time
			fresh := newOccurrences(st, last) > 0
			switch {
			case !fresh && !st.LastSeen.After(last):
				return false, ReasonNoNewData
			case now.Before(last.Add(s.Cooldown)):
				return false, ReasonCoolingDown
			case fresh:
				reason = ReasonNewOccurrence
			default:
				reason = ReasonCooldownExpired
			}
		}
	}
	if seen := latest(st.LastSeen.Time, held); now.Before(seen.Add(s.Debounce)) {
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
