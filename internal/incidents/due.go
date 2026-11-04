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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// DefaultTriggerInterval is how often the controller decides again which Incidents are due.
const DefaultTriggerInterval = 10 * time.Second

// triggerFieldOwner is the field manager of the analysis trigger's writes (the AnalysisDue condition and the
// budget ConfigMap), so the managed fields tell them from the observation writes.
const triggerFieldOwner = "shadewright-analysis-trigger"

// triggerLoop marks the Incidents due for analysis every TriggerInterval until ctx ends. Each pass runs under
// PassTimeout, shorter than the interval: ctx lasts as long as the controller, so a call that stalls without
// a deadline would stop every later pass.
func (c *Controller) triggerLoop(ctx context.Context) {
	tick := time.NewTicker(c.cfg.TriggerInterval)
	defer tick.Stop()
	for {
		if err := c.pass(ctx); err != nil && ctx.Err() == nil {
			triggerPassErrors.Inc()
			logf.FromContext(ctx).Error(err, "Failed to decide which Incidents are due for analysis, will retry")
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

// pass runs one [Controller.markDue] under PassTimeout.
func (c *Controller) pass(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, c.cfg.PassTimeout)
	defer cancel()
	return c.markDue(pctx)
}

// markDue decides, for every Incident in the namespace, whether it is due for analysis ([Due]) and sets its
// AnalysisDue condition where that changed. The budget used today is read from the budget ConfigMap
// ([UsedToday]); each Incident marked due takes one more unit of it, so no more Incidents are due at once than
// the budget has left. Those already due go first, so the same ones stay due from one pass to the next; then
// the oldest by firstSeen, so a flood of new fingerprints does not push older incidents out of the budget.
//
// It fails closed: without the RemediationSettings object or the budget ConfigMap, or with a budget that
// cannot be trusted, nothing is due; when either cannot be read, the Incidents marked due are set not due and
// the pass returns the error. An Incident whose name does not belong to its fingerprint is not decided: if it
// is marked due, it is set not due.
func (c *Controller) markDue(ctx context.Context) error {
	var list remediationv1alpha1.IncidentList
	if err := c.cache.List(ctx, &list, client.InNamespace(c.cfg.Namespace)); err != nil {
		return fmt.Errorf("listing the Incidents: %w", err)
	}
	now := c.cfg.Now()
	s := TriggerSettings{Debounce: c.cfg.Debounce, SettleLimit: c.cfg.SettleLimit, Cooldown: c.cfg.Cooldown,
		AnalysisTimeout: c.cfg.AnalysisTimeout}
	used, closed, readErr := c.readBudget(ctx, now, &s)

	items := list.Items
	slices.SortFunc(items, dueOrder)
	var errs []error
	listed := make(map[string]struct{}, len(items))
	for i := range items {
		inc := &items[i]
		listed[inc.Name] = struct{}{}
		wasDue := meta.IsStatusConditionTrue(inc.Status.Conditions, ConditionAnalysisDue)
		var decide func(*remediationv1alpha1.Incident) (bool, string)
		switch {
		case inc.Name != Name(inc.Spec.Fingerprint):
			// Not an Incident this controller writes (another fingerprint holds the name): only a mark that says
			// due is taken back.
			delete(c.waiting, inc.Name)
			if !wasDue {
				continue
			}
			decide = func(*remediationv1alpha1.Incident) (bool, string) { return false, ReasonForeignFingerprint }
		case closed != "":
			if readErr != nil && !wasDue {
				// Without the settings or the budget nothing else changes: the marks that say due are taken
				// back, no more.
				continue
			}
			decide = func(*remediationv1alpha1.Incident) (bool, string) { return false, closed }
		default:
			held, waiting := c.heldSince(inc.Spec.Fingerprint), c.waiting[inc.Name]
			decide = func(fresh *remediationv1alpha1.Incident) (bool, string) {
				return Due(fresh, held, waiting, now, s, used)
			}
		}
		due, reason, err := c.setDue(ctx, inc, decide)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
		if closed == "" {
			c.rememberWaiting(inc.Name, reason, now)
		}
		// A failed write may still have landed (a lost answer), and a mark it did not change still says due:
		// either way the Incident takes its unit of the budget.
		if due || (err != nil && wasDue) {
			used++
		}
	}
	for name := range c.waiting {
		if _, ok := listed[name]; !ok {
			delete(c.waiting, name)
		}
	}
	if readErr != nil {
		errs = append([]error{readErr}, errs...)
	}
	if len(errs) > 0 {
		return fmt.Errorf("deciding which Incidents are due, %d errors, first: %w", len(errs), errs[0])
	}
	return nil
}

// readBudget reads maxAnalysesPerDay from the RemediationSettings (into s) and the analyses used today from
// the budget ConfigMap. closed is the not-due reason of every Incident when the budget is unknown, and empty
// otherwise; readErr is set when a read failed for another reason than a missing object. Both reads together
// run under ReadTimeout, shorter than the pass, so a read that stalls leaves the rest of the pass to take the
// marks that say due back.
func (c *Controller) readBudget(ctx context.Context, now time.Time, s *TriggerSettings) (used int, closed string, readErr error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout)
	defer cancel()
	var settings remediationv1alpha1.RemediationSettings
	switch err := c.cache.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, &settings); {
	case apierrors.IsNotFound(err):
		return 0, ReasonSettingsNotFound, nil
	case err != nil:
		return 0, ReasonSettingsUnreadable, fmt.Errorf("reading RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}
	s.MaxAnalysesPerDay = maxAnalysesPerDay(&settings)
	// The ConfigMap is read from the API server on every pass: the analyzer may get it by name only, so there
	// is no informer on it.
	var cm corev1.ConfigMap
	switch err := c.reader.Get(ctx, types.NamespacedName{Namespace: c.cfg.Namespace, Name: BudgetName}, &cm); {
	case apierrors.IsNotFound(err):
		return 0, ReasonBudgetNotFound, nil
	case err != nil:
		return 0, ReasonBudgetReadFailed, fmt.Errorf("reading ConfigMap %s/%s: %w", c.cfg.Namespace, BudgetName, err)
	}
	used, ok := UsedToday(&cm, now)
	if !ok {
		return 0, ReasonBudgetUnreadable, nil
	}
	analysesUsedToday.Set(float64(used))
	analysesPerDayLimit.Set(float64(s.MaxAnalysesPerDay))
	return used, "", nil
}

// dueOrder is the order in which a pass decides the Incidents, so the budget left goes to them in that order:
// those marked due first, then the oldest by firstSeen (an Incident without one last), then by name.
func dueOrder(a, b remediationv1alpha1.Incident) int {
	ad, bd := meta.IsStatusConditionTrue(a.Status.Conditions, ConditionAnalysisDue),
		meta.IsStatusConditionTrue(b.Status.Conditions, ConditionAnalysisDue)
	if ad != bd {
		if ad {
			return -1
		}
		return 1
	}
	af, bf := a.Status.FirstSeen, b.Status.FirstSeen
	switch {
	case af != nil && bf == nil:
		return -1
	case af == nil && bf != nil:
		return 1
	case af != nil && !af.Equal(bf):
		return af.Compare(bf.Time)
	}
	return strings.Compare(a.Name, b.Name)
}

// rememberWaiting keeps when the controller first saw an Incident waiting only to settle or for budget (see
// [Due]), and forgets it once the Incident waits for data again (analyzed, cooling down, or in a phase that is
// not due).
func (c *Controller) rememberWaiting(name, reason string, now time.Time) {
	if waitsForData(reason) {
		delete(c.waiting, name)
		return
	}
	if _, ok := c.waiting[name]; !ok {
		c.waiting[name] = now
	}
}

// heldSince returns the newest observation of the Incident of fp that the controller holds unwritten, in a
// pending or a failed batch, or zero.
func (c *Controller) heldSince(fp string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var t time.Time
	for _, b := range []*batch{c.pending[fp], c.failed[fp]} {
		if b != nil {
			t = latest(t, b.last)
		}
	}
	return t
}

// setDue sets the AnalysisDue condition of one Incident to what decide says. cached is the Incident as the
// cache holds it: when its condition already agrees ([conditionAgrees]), nothing is written. Otherwise the
// Incident is read fresh from the API server, decided again on that copy, and its status updated, retrying
// on conflict (the observation writer updates the same status). It reports the decision it last made, also
// with an error, since a write whose answer was lost may have landed; an Incident gone meanwhile is reported
// not due (NotObserved).
func (c *Controller) setDue(ctx context.Context, cached *remediationv1alpha1.Incident,
	decide func(*remediationv1alpha1.Incident) (bool, string)) (bool, string, error) {
	due, reason := decide(cached)
	if conditionAgrees(cached, due, reason) {
		return due, reason, nil
	}
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		inc := &remediationv1alpha1.Incident{}
		if err := c.reader.Get(wctx, types.NamespacedName{Namespace: c.cfg.Namespace, Name: cached.Name}, inc); err != nil {
			return err
		}
		due, reason = decide(inc)
		if conditionAgrees(inc, due, reason) {
			return nil
		}
		status := metav1.ConditionFalse
		if due {
			status = metav1.ConditionTrue
		}
		// The transition time comes from the controller's clock, like every decision; SetStatusCondition keeps
		// the old one when only the reason changes.
		meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
			Type:               ConditionAnalysisDue,
			Status:             status,
			Reason:             reason,
			Message:            dueMessages[reason],
			ObservedGeneration: inc.Generation,
			LastTransitionTime: metav1.NewTime(c.cfg.Now()),
		})
		return c.client.Status().Update(wctx, inc, client.FieldOwner(triggerFieldOwner))
	})
	if apierrors.IsNotFound(err) {
		return false, ReasonNotObserved, nil
	}
	if err != nil {
		return due, reason, fmt.Errorf("setting AnalysisDue on Incident %s: %w", cached.Name, err)
	}
	return due, reason, nil
}

// conditionAgrees reports whether the Incident's AnalysisDue condition already holds the decision (due,
// reason). A condition that says due must hold the same reason. One that says not due agrees with any
// not-due reason: the reason it gave first stays, so a change that turns every Incident's reason (the budget
// used up, or set to 0) writes nothing.
func conditionAgrees(inc *remediationv1alpha1.Incident, due bool, reason string) bool {
	cond := meta.FindStatusCondition(inc.Status.Conditions, ConditionAnalysisDue)
	if cond == nil || cond.ObservedGeneration != inc.Generation {
		return false
	}
	if due {
		return cond.Status == metav1.ConditionTrue && cond.Reason == reason
	}
	return cond.Status == metav1.ConditionFalse
}

// dueMessages are the fixed messages of the AnalysisDue condition. They hold nothing from the cluster.
var dueMessages = map[string]string{
	ReasonFirstAnalysis:        "The incident was never analyzed and has settled",
	ReasonNewOccurrence:        "An occurrence began after the last analysis and the cooldown has passed",
	ReasonCooldownExpired:      "The symptom recurred and the cooldown since the last analysis has passed",
	ReasonAnalysisTimedOut:     "The last analysis did not finish within the analysis timeout and the cooldown has passed",
	ReasonNotObserved:          "No observation is recorded yet",
	ReasonAnalyzing:            "An analysis is running",
	ReasonAnalysisStartUnknown: "The incident is in phase Analyzing without a lastAnalysisTime, so the cooldown cannot be placed",
	ReasonProposed:             "The last analysis made a proposal",
	ReasonEscalated:            "The last analysis handed the incident to a person",
	ReasonNoNewData:            "Nothing was observed since the last analysis",
	ReasonCoolingDown:          "The cooldown since the last analysis has not passed",
	ReasonDebouncing:           "Waiting for the incident to settle, at most the settle limit after its first unsettled observation",
	ReasonAnalysisDisabled:     "RemediationSettings maxAnalysesPerDay is 0",
	ReasonDailyBudgetReached:   "The analyses of the current UTC day reached maxAnalysesPerDay",
	ReasonBudgetUnreadable:     "The analysis budget ConfigMap cannot be trusted, so the budget is taken as spent",
	ReasonBudgetNotFound:       "The analysis budget ConfigMap does not exist, so the daily budget is unknown",
	ReasonBudgetReadFailed:     "The analysis budget ConfigMap could not be read, so the daily budget is unknown",
	ReasonSettingsNotFound:     "The RemediationSettings object does not exist, so the daily budget is unknown",
	ReasonSettingsUnreadable:   "The RemediationSettings object could not be read, so the daily budget is unknown",
	ReasonForeignFingerprint:   "The Incident's name belongs to another fingerprint",
}
