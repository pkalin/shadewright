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
// analysis budget), so the managed fields tell them from the observation writes.
const triggerFieldOwner = "shadewright-analysis-trigger"

// triggerLoop marks the Incidents due for analysis every TriggerInterval until ctx ends.
func (c *Controller) triggerLoop(ctx context.Context) {
	tick := time.NewTicker(c.cfg.TriggerInterval)
	defer tick.Stop()
	for {
		if err := c.markDue(ctx); err != nil && ctx.Err() == nil {
			logf.FromContext(ctx).Error(err, "Failed to decide which Incidents are due for analysis, will retry")
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

// markDue decides, for every Incident in the namespace, whether it is due for analysis ([Due]) and sets its
// AnalysisDue condition where that changed. The budget used today is read from the RemediationSettings
// status ([UsedToday]); each Incident marked due takes one more unit of it, so no more Incidents are due at
// once than the budget has left. Those already due go first, so the same ones stay due from one pass to the
// next.
//
// It fails closed: without the RemediationSettings object, or with a budget that cannot be trusted, nothing
// is due; when the object cannot be read, the Incidents marked due are set not due and the pass returns the
// error. An Incident whose name does not belong to its fingerprint is not decided: if it is marked due, it is
// set not due.
func (c *Controller) markDue(ctx context.Context) error {
	var list remediationv1alpha1.IncidentList
	if err := c.cache.List(ctx, &list, client.InNamespace(c.cfg.Namespace)); err != nil {
		return fmt.Errorf("listing the Incidents: %w", err)
	}
	now := c.cfg.Now()
	s := TriggerSettings{Debounce: c.cfg.Debounce, Cooldown: c.cfg.Cooldown, AnalysisTimeout: c.cfg.AnalysisTimeout}
	var (
		used     int
		closed   string // the not-due reason of every Incident when the budget is unknown
		settings remediationv1alpha1.RemediationSettings
	)
	settingsErr := c.cache.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, &settings)
	switch {
	case apierrors.IsNotFound(settingsErr):
		closed = ReasonSettingsNotFound
	case settingsErr != nil:
		closed = ReasonSettingsUnreadable
		settingsErr = fmt.Errorf("reading RemediationSettings %s: %w", remediationv1alpha1.SettingsName, settingsErr)
	default:
		var ok bool
		if used, ok = UsedToday(&settings, now); !ok {
			closed = ReasonBudgetUnreadable
		}
		s.MaxAnalysesPerDay = maxAnalysesPerDay(&settings)
	}

	items := list.Items
	slices.SortFunc(items, func(a, b remediationv1alpha1.Incident) int {
		ad, bd := meta.IsStatusConditionTrue(a.Status.Conditions, ConditionAnalysisDue),
			meta.IsStatusConditionTrue(b.Status.Conditions, ConditionAnalysisDue)
		if ad != bd {
			if ad {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	var errs []error
	for i := range items {
		inc := &items[i]
		wasDue := meta.IsStatusConditionTrue(inc.Status.Conditions, ConditionAnalysisDue)
		var decide func(*remediationv1alpha1.Incident) (bool, string)
		switch {
		case inc.Name != Name(inc.Spec.Fingerprint):
			// Not an Incident this controller writes (another fingerprint holds the name): only a mark that says
			// due is taken back.
			if !wasDue {
				continue
			}
			decide = func(*remediationv1alpha1.Incident) (bool, string) { return false, ReasonForeignFingerprint }
		case closed != "":
			if closed == ReasonSettingsUnreadable && !wasDue {
				// Without the settings nothing else changes: the marks that say due are taken back, no more.
				continue
			}
			decide = func(*remediationv1alpha1.Incident) (bool, string) { return false, closed }
		default:
			held := c.heldSince(inc.Spec.Fingerprint)
			decide = func(fresh *remediationv1alpha1.Incident) (bool, string) {
				return Due(fresh, held, now, s, used)
			}
		}
		due, err := c.setDue(ctx, inc, decide)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
		// A failed write may still have landed (a lost answer), and a mark it did not change still says due:
		// either way the Incident takes its unit of the budget.
		if due || (err != nil && wasDue) {
			used++
		}
	}
	if settingsErr != nil && !apierrors.IsNotFound(settingsErr) {
		errs = append([]error{settingsErr}, errs...)
	}
	if len(errs) > 0 {
		return fmt.Errorf("deciding which Incidents are due, %d errors, first: %w", len(errs), errs[0])
	}
	return nil
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
// with an error, since a write whose answer was lost may have landed.
func (c *Controller) setDue(ctx context.Context, cached *remediationv1alpha1.Incident,
	decide func(*remediationv1alpha1.Incident) (bool, string)) (bool, error) {
	due, reason := decide(cached)
	if conditionAgrees(cached, due, reason) {
		return due, nil
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
		return false, nil
	}
	if err != nil {
		return due, fmt.Errorf("setting AnalysisDue on Incident %s: %w", cached.Name, err)
	}
	return due, nil
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
	ReasonFirstAnalysis:      "The incident was never analyzed and has settled",
	ReasonNewOccurrence:      "An occurrence began after the last analysis and the cooldown has passed",
	ReasonCooldownExpired:    "The symptom recurred and the cooldown since the last analysis has passed",
	ReasonAnalysisTimedOut:   "The last analysis did not finish within the analysis timeout",
	ReasonNotObserved:        "No observation is recorded yet",
	ReasonAnalyzing:          "An analysis is running",
	ReasonProposed:           "The last analysis made a proposal",
	ReasonEscalated:          "The last analysis handed the incident to a person",
	ReasonNoNewData:          "Nothing was observed since the last analysis",
	ReasonCoolingDown:        "The cooldown since the last analysis has not passed",
	ReasonDebouncing:         "Waiting for the incident to settle",
	ReasonAnalysisDisabled:   "RemediationSettings maxAnalysesPerDay is 0",
	ReasonDailyBudgetReached: "The analyses of the current UTC day reached maxAnalysesPerDay",
	ReasonBudgetUnreadable:   "The analysis budget in the RemediationSettings status cannot be trusted, so it is taken as spent",
	ReasonSettingsNotFound:   "The RemediationSettings object does not exist, so the daily budget is unknown",
	ReasonSettingsUnreadable: "The RemediationSettings object could not be read, so the daily budget is unknown",
	ReasonForeignFingerprint: "The Incident's name belongs to another fingerprint",
}
