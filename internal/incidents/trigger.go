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
	// and the cooldown since its last analysis began, so that analysis is taken as lost.
	ReasonAnalysisTimedOut = "AnalysisTimedOut"

	// ReasonNotObserved: the status records no observation yet.
	ReasonNotObserved = "NotObserved"
	// ReasonAnalyzing: an analysis is running and has not reached the analysis timeout.
	ReasonAnalyzing = "Analyzing"
	// ReasonAnalysisStartUnknown: the incident is in phase Analyzing without a lastAnalysisTime, so neither
	// the analysis timeout nor the cooldown can be placed. It stays not due until the status is repaired, so
	// that a status without the time never starts analyses faster than the cooldown.
	ReasonAnalysisStartUnknown = "AnalysisStartUnknown"
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
	// controller, is more recent than the debounce, and the settle limit since its first unsettled
	// observation has not passed.
	ReasonDebouncing = "Debouncing"
	// ReasonAnalysisDisabled: RemediationSettings spec.analysis.maxAnalysesPerDay is 0.
	ReasonAnalysisDisabled = "AnalysisDisabled"
	// ReasonDailyBudgetReached: the analyses of the current UTC day reached spec.analysis.maxAnalysesPerDay.
	ReasonDailyBudgetReached = "DailyBudgetReached"
	// ReasonBudgetUnreadable: the budget ConfigMap holds a day in the future, a count out of bounds or keys
	// it should not, so the budget is taken as spent.
	ReasonBudgetUnreadable = "BudgetUnreadable"
	// ReasonBudgetNotFound: the budget ConfigMap does not exist, so the budget is unknown.
	ReasonBudgetNotFound = "BudgetNotFound"
	// ReasonBudgetReadFailed: the budget ConfigMap could not be read, so the budget is unknown.
	ReasonBudgetReadFailed = "BudgetReadFailed"
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
	// SettleLimit bounds the debounce: an incident is due at the latest SettleLimit after its first unsettled
	// observation ([unsettledSince]), even if observations keep arriving less than Debounce apart. Zero means
	// no bound.
	SettleLimit time.Duration
	// Cooldown is the least time between the start of an analysis of an incident and the next one, whatever
	// new data arrives, a timed-out analysis included.
	Cooldown time.Duration
	// AnalysisTimeout is how long an incident may stay in phase Analyzing after its last analysis began
	// before that analysis is taken as lost. It is due again at the later of AnalysisTimeout and Cooldown.
	AnalysisTimeout time.Duration
	// MaxAnalysesPerDay is RemediationSettings spec.analysis.maxAnalysesPerDay: the analyses allowed per UTC
	// day across all Incidents. 0 disables analysis.
	MaxAnalysesPerDay int
}

// Due decides whether an incident is due for analysis at now, and gives the reason (one of the Reason
// constants). held is the newest observation of the incident the controller holds but has not written yet
// (zero when none). waiting is when the controller first found the incident waiting only to settle or for
// budget, in this process (zero when not); [unsettledSince] uses it when the status does not tell. usedToday
// is the number of analyses already counted against the current UTC day, read from the budget ConfigMap
// ([UsedToday]), not from memory. Due reads only its arguments.
//
// An incident is due when it has data the last analysis did not see, it has settled (or the settle limit has
// passed), the cooldown has passed and the budget allows:
//
//   - nothing observed yet (no lastSeen): not due (NotObserved);
//   - in phase Proposed or Escalated: not due (Proposed, Escalated);
//   - in phase Analyzing: not due (Analyzing) until AnalysisTimeout after lastAnalysisTime, then not due
//     (CoolingDown) until the cooldown has passed too, then due again (AnalysisTimedOut); without a
//     lastAnalysisTime not due (AnalysisStartUnknown), since neither can be placed;
//   - in phase Open or Resolved (or none), never analyzed: due (FirstAnalysis);
//   - nothing seen since the last analysis: not due (NoNewData);
//   - seen again, within the cooldown since the last analysis: not due (CoolingDown), even with a new
//     occurrence, so a crash loop is analyzed at most once per cooldown;
//   - past the cooldown: due, NewOccurrence when an occurrence began after the last analysis (distinct
//     occurrences count, not status.count, which any update of an event series raises), else
//     CooldownExpired;
//   - due by the above, but last seen (in the status or held) less than the debounce ago: not due yet
//     (Debouncing), unless SettleLimit has passed since the first unsettled observation;
//   - due by the above, but the budget is 0 (AnalysisDisabled) or used up (DailyBudgetReached): not due.
//
// An occurrence without a known begin time is never taken as new, since it cannot be placed after the last
// analysis; its observation still counts as seen again.
func Due(inc *remediationv1alpha1.Incident, held, waiting, now time.Time, s TriggerSettings, usedToday int) (bool, string) {
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
		if st.LastAnalysisTime == nil {
			return false, ReasonAnalysisStartUnknown
		}
		last := st.LastAnalysisTime.Time
		switch {
		case now.Before(last.Add(s.AnalysisTimeout)):
			return false, ReasonAnalyzing
		case now.Before(last.Add(s.Cooldown)):
			return false, ReasonCoolingDown
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
		since := unsettledSince(st, waiting)
		if s.SettleLimit <= 0 || since.IsZero() || now.Before(since.Add(s.SettleLimit)) {
			return false, ReasonDebouncing
		}
	}
	if s.MaxAnalysesPerDay <= 0 {
		return false, ReasonAnalysisDisabled
	}
	if usedToday >= s.MaxAnalysesPerDay {
		return false, ReasonDailyBudgetReached
	}
	return true, reason
}

// unsettledSince returns when the incident's first observation that the last analysis did not see was made,
// as far as it is known, or zero: firstSeen for an incident never analyzed; else the begin of the earliest
// occurrence that began after lastAnalysisTime (the drop mark standing in for the dropped records). waiting,
// the controller's own first sight of the incident waiting, is taken when it is earlier, or when the status
// does not tell (a recurrence of an occurrence the last analysis already counted). The status values survive
// a restart; waiting does not.
func unsettledSince(st *remediationv1alpha1.IncidentStatus, waiting time.Time) time.Time {
	var since time.Time
	earliest := func(t time.Time) {
		if !t.IsZero() && (since.IsZero() || t.Before(since)) {
			since = t
		}
	}
	switch {
	case st.LastAnalysisTime == nil && st.Phase != remediationv1alpha1.IncidentAnalyzing:
		if st.FirstSeen != nil {
			earliest(st.FirstSeen.Time)
		}
	case st.LastAnalysisTime != nil:
		last := st.LastAnalysisTime.Time
		for _, r := range st.Occurrences {
			if r.Began != nil && r.Began.After(last) {
				earliest(r.Began.Time)
			}
		}
		if st.OccurrencesDroppedThrough != nil && st.OccurrencesDroppedThrough.After(last) {
			earliest(st.OccurrencesDroppedThrough.Time)
		}
	}
	earliest(waiting)
	return since
}

// waitsForData reports whether reason comes from the checks of the incident's own data, before the debounce
// (Due's not-due reasons up to CoolingDown). An incident with such a reason is not waiting to settle, so the
// controller forgets when it first saw it waiting.
func waitsForData(reason string) bool {
	switch reason {
	case ReasonNotObserved, ReasonProposed, ReasonEscalated, ReasonAnalyzing, ReasonAnalysisStartUnknown,
		ReasonNoNewData, ReasonCoolingDown, ReasonForeignFingerprint:
		return true
	}
	return false
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
