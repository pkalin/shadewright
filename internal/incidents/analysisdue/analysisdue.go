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

// Package analysisdue holds the read side of the analysis trigger: the AnalysisDue condition and its reasons,
// the names and keys of the analysis budget ConfigMap and how its count is read. It imports no client and no
// controller-runtime, so kubectl-shadewright can read the budget without the operator's server-side stack.
// The package internal/incidents keeps every name here as an alias and holds the write side
// (ReserveAnalysis).
package analysisdue

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// ConditionAnalysisDue is the Incident condition that says whether the incident is due for analysis, and
// why (design 3.2). It is a mark only: the analysis itself reads it, and reserves a unit of the daily budget
// (incidents.ReserveAnalysis) before it calls the model.
const ConditionAnalysisDue = "AnalysisDue"

// Reasons of the AnalysisDue condition. The first five come with status True, the others with False.
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
	// ReasonReviewerHint: the incident is Proposed, a request naming it holds a reviewer hint no analysis has
	// answered (incidents.PendingHint) and the cooldown since the last analysis has passed (ADR-0028).
	ReasonReviewerHint = "ReviewerHint"

	// ReasonNotObserved: the status records no observation yet.
	ReasonNotObserved = "NotObserved"
	// ReasonNotRecent: the incident was never analyzed, is not marked due, its newest occurrence is no lasting
	// symptom state of a module, and it was last seen (in the status or held) longer ago than the recent
	// window, so a symptom that stopped before analysis was turned on, or before the daily budget was reset,
	// takes no unit of the budget. A new observation makes it due again through the debounce, the settle
	// limit counting from the controller's first sight of it waiting again.
	ReasonNotRecent = "NotRecent"
	// ReasonAnalyzing: an analysis is running and has not reached the analysis timeout.
	ReasonAnalyzing = "Analyzing"
	// ReasonAnalysisStartUnknown: the incident is in phase Analyzing without a lastAnalysisTime, so neither
	// the analysis timeout nor the cooldown can be placed. It stays not due until the status is repaired, so
	// that a status without the time never starts analyses faster than the cooldown.
	ReasonAnalysisStartUnknown = "AnalysisStartUnknown"
	// ReasonProposed: the last analysis made a proposal; the closure rules move the incident on once its request
	// ends or it goes quiet (incidents.Closure).
	ReasonProposed = "Proposed"
	// ReasonEscalated: the last analysis, or a request that ended without success (incidents.Closure), handed
	// the incident to a person; the closure rules resolve it once it goes quiet.
	ReasonEscalated = "Escalated"
	// ReasonResolved: the incident is Resolved and is not analyzed while it stays so (design 3.2, #251). A
	// symptom seen again moves it to Escalated within incidents.RecurrenceWindow of resolvedTime (or back to
	// Open when it was resolved by quiet time before any analysis), or opens its next occurrence after.
	ReasonResolved = "Resolved"
	// ReasonRecurred: the incident is Escalated because its symptom came back after it was Resolved (the
	// condition Recurred), not because of an analysis; a person decides.
	ReasonRecurred = "Recurred"
	// ReasonNoNewData: nothing was seen since the last analysis.
	ReasonNoNewData = "NoNewData"
	// ReasonCoolingDown: the incident has data the last analysis did not see, but the cooldown since that
	// analysis has not passed.
	ReasonCoolingDown = "CoolingDown"
	// ReasonDebouncing: the incident would be due, but its last observation, written or still held by the
	// controller, is more recent than the debounce, and the settle limit since its first unsettled
	// observation has not passed.
	ReasonDebouncing = "Debouncing"
	// ReasonAnalysisDisabled: the daily limit of the RemediationSettings is 0: spec.analysis.maxAnalysesPerDay,
	// or the deprecated spec.maxAnalysesPerDay when that is set and lower.
	ReasonAnalysisDisabled = "AnalysisDisabled"
	// ReasonDailyBudgetReached: the analyses of the current UTC day reached the daily limit of the
	// RemediationSettings.
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
	// ReasonSettingsIncomplete: the RemediationSettings object sets no spec.analysis.maxAnalysesPerDay (an
	// object the API server's defaults did not fill in), so the budget is unknown.
	ReasonSettingsIncomplete = "SettingsIncomplete"
	// ReasonForeignFingerprint: the Incident's name is not the one derived from its own fingerprint and
	// occurrence number (remediationv1alpha1.IncidentName), so this controller does not analyze it.
	ReasonForeignFingerprint = "ForeignFingerprint"
)

// BudgetName is the name of the ConfigMap in the operator namespace that counts the analyses of the current
// UTC day. It is shipped empty with the analyzer (config/manager/analysis_budget.yaml, before the kustomize
// name prefix), and the analyzer may get and update this one ConfigMap and no other: its Role names it in
// resourceNames, which the name prefix does not rewrite, so it carries the full name.
const BudgetName = "shadewright-analysis-budget"

// The keys of the budget ConfigMap. Both are set or both are absent; no other key is allowed.
const (
	// BudgetDayKey holds the UTC day the count belongs to, as YYYY-MM-DD.
	BudgetDayKey = "day"
	// BudgetUsedKey holds the analyses started on that day, a decimal number from 0 to 10,000.
	BudgetUsedKey = "used"
)

// budgetDayLayout is the layout of the day in the budget ConfigMap.
const budgetDayLayout = "2006-01-02"

// maxBudgetUsed is the largest count the budget holds, the largest spec.analysis.maxAnalysesPerDay.
const maxBudgetUsed = 10000

// budgetUsedPattern is the form of the count: plain decimal digits, no sign, space or leading plus.
var budgetUsedPattern = regexp.MustCompile(`^[0-9]{1,5}$`)

// analysisBudget is the parsed content of the budget ConfigMap. The zero value is an empty ConfigMap: none
// used yet.
type analysisBudget struct {
	day  time.Time // midnight UTC of the day counted; zero when the ConfigMap is empty
	used int
}

// parseBudget reads the budget ConfigMap. It accepts an empty ConfigMap, or exactly the keys day (a valid
// YYYY-MM-DD date) and used (0 to 10,000 in decimal digits), and nothing else: no binary data, no other key,
// not one key without the other.
func parseBudget(cm *corev1.ConfigMap) (analysisBudget, error) {
	if len(cm.BinaryData) > 0 {
		return analysisBudget{}, errors.New("binaryData is not allowed")
	}
	for k := range cm.Data {
		if k != BudgetDayKey && k != BudgetUsedKey {
			return analysisBudget{}, fmt.Errorf("unexpected key %q", k)
		}
	}
	day, hasDay := cm.Data[BudgetDayKey]
	used, hasUsed := cm.Data[BudgetUsedKey]
	switch {
	case !hasDay && !hasUsed:
		return analysisBudget{}, nil
	case hasDay != hasUsed:
		return analysisBudget{}, errors.New("day and used must be set together")
	}
	d, err := time.Parse(budgetDayLayout, day)
	if err != nil || d.Format(budgetDayLayout) != day {
		return analysisBudget{}, errors.New("day is not a YYYY-MM-DD date")
	}
	if !budgetUsedPattern.MatchString(used) {
		return analysisBudget{}, errors.New("used is not a count")
	}
	n, err := strconv.Atoi(used)
	if err != nil || n > maxBudgetUsed {
		return analysisBudget{}, fmt.Errorf("used is above %d", maxBudgetUsed)
	}
	return analysisBudget{day: d, used: n}, nil
}

// budgetDay returns the UTC day of t, at midnight UTC.
func budgetDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// BudgetDayValue returns the value of [BudgetDayKey] for the UTC day of t, as YYYY-MM-DD.
func BudgetDayValue(t time.Time) string {
	return budgetDay(t).Format(budgetDayLayout)
}

// UsedToday returns the analyses counted against the UTC day of now in the budget ConfigMap (nil when it does
// not exist). ok is false when that count cannot be trusted: no ConfigMap, content [parseBudget] refuses, or a
// day after today (a clock ahead, or a forged value). The caller then takes the budget as spent (fail closed).
// An empty ConfigMap, or a count of an earlier day, means none used today.
func UsedToday(cm *corev1.ConfigMap, now time.Time) (used int, ok bool) {
	if cm == nil {
		return 0, false
	}
	b, err := parseBudget(cm)
	if err != nil {
		return 0, false
	}
	today := budgetDay(now)
	switch {
	case b.day.IsZero(), b.day.Before(today):
		return 0, true
	case b.day.After(today):
		return 0, false
	}
	return b.used, true
}

// MaxAnalysesPerDay returns the analyses the settings allow per UTC day: spec.analysis.maxAnalysesPerDay, or
// the deprecated spec.maxAnalysesPerDay when that is set and lower, so a 0 stored before the field moved keeps
// analysis off. ok is false when spec.analysis or its maxAnalysesPerDay is unset, which the API server's
// defaults never leave; the caller then takes the budget as unknown and allows no analysis, rather than
// guessing a limit.
func MaxAnalysesPerDay(settings *remediationv1alpha1.RemediationSettings) (limit int, ok bool) {
	a := settings.Spec.Analysis
	if a == nil || a.MaxAnalysesPerDay == nil {
		return 0, false
	}
	limit = int(*a.MaxAnalysesPerDay)
	if old := settings.Spec.MaxAnalysesPerDay; old != nil {
		limit = min(limit, int(*old))
	}
	return limit, true
}
