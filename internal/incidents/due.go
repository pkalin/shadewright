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

// SettingsName is the name of the one RemediationSettings object, which holds maxAnalysesPerDay.
const SettingsName = "default"

// DefaultTriggerInterval is how often the controller decides again which Incidents are due.
const DefaultTriggerInterval = 10 * time.Second

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
// AnalysisDue condition where that changed. The budget used today is summed from the Incidents' status
// ([UsedToday]); each Incident marked due takes one more unit of it, so no more Incidents are due at once
// than the budget has left. Those already due go first, so the same ones stay due from one pass to the
// next. An Incident whose name does not belong to its fingerprint is left alone, but its analyses still
// count against the budget. Without the RemediationSettings object nothing is due (fail closed); when it
// cannot be read, nothing changes and the pass returns the error.
func (c *Controller) markDue(ctx context.Context) error {
	var settings remediationv1alpha1.RemediationSettings
	found := true
	if err := c.cache.Get(ctx, types.NamespacedName{Name: SettingsName}, &settings); apierrors.IsNotFound(err) {
		found = false
	} else if err != nil {
		return fmt.Errorf("reading RemediationSettings %s: %w", SettingsName, err)
	}
	var list remediationv1alpha1.IncidentList
	if err := c.cache.List(ctx, &list, client.InNamespace(c.cfg.Namespace)); err != nil {
		return fmt.Errorf("listing the Incidents: %w", err)
	}
	s := TriggerSettings{Debounce: c.cfg.Debounce, Cooldown: c.cfg.Cooldown, MaxAnalysesPerDay: DefaultMaxAnalysesPerDay}
	if settings.Spec.MaxAnalysesPerDay != nil {
		s.MaxAnalysesPerDay = int(*settings.Spec.MaxAnalysesPerDay)
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
	now := c.cfg.Now()
	used := UsedToday(items, now)
	var errs []error
	for i := range items {
		if items[i].Name != Name(items[i].Spec.Fingerprint) {
			// Not an Incident this controller writes (another fingerprint holds the name): left alone.
			continue
		}
		decide := func(inc *remediationv1alpha1.Incident) (bool, string) {
			if !found {
				return false, ReasonSettingsNotFound
			}
			return Due(inc, now, s, used)
		}
		due, err := c.setDue(ctx, items[i].Name, &items[i], decide)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// The condition stays as it was: if it says due, it still takes its unit of the budget.
			if meta.IsStatusConditionTrue(items[i].Status.Conditions, ConditionAnalysisDue) {
				used++
			}
			errs = append(errs, err)
			continue
		}
		if due {
			used++
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("setting AnalysisDue on %d Incidents, first: %w", len(errs), errs[0])
	}
	return nil
}

// setDue sets the AnalysisDue condition of one Incident to what decide says. cached is the Incident as the
// cache holds it: when its condition already says the same, nothing is written. Otherwise the Incident is
// read fresh from the API server, decided again on that copy, and its status updated, retrying on conflict
// (the observation writer updates the same status). It reports the decision that holds.
func (c *Controller) setDue(ctx context.Context, name string, cached *remediationv1alpha1.Incident,
	decide func(*remediationv1alpha1.Incident) (bool, string)) (bool, error) {
	if due, reason := decide(cached); conditionSays(cached, due, reason) {
		return due, nil
	}
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	var due bool
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		inc := &remediationv1alpha1.Incident{}
		if err := c.reader.Get(wctx, types.NamespacedName{Namespace: c.cfg.Namespace, Name: name}, inc); err != nil {
			return err
		}
		var reason string
		due, reason = decide(inc)
		if conditionSays(inc, due, reason) {
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
		return c.marker.Status().Update(wctx, inc)
	})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("setting AnalysisDue on Incident %s: %w", name, err)
	}
	return due, nil
}

// conditionSays reports whether the Incident's AnalysisDue condition already holds this decision.
func conditionSays(inc *remediationv1alpha1.Incident, due bool, reason string) bool {
	cond := meta.FindStatusCondition(inc.Status.Conditions, ConditionAnalysisDue)
	if cond == nil {
		return false
	}
	want := metav1.ConditionFalse
	if due {
		want = metav1.ConditionTrue
	}
	return cond.Status == want && cond.Reason == reason && cond.ObservedGeneration == inc.Generation
}

// dueMessages are the fixed messages of the AnalysisDue condition. They hold nothing from the cluster.
var dueMessages = map[string]string{
	ReasonFirstAnalysis:      "The incident was never analyzed and has settled",
	ReasonNewOccurrence:      "An occurrence began after the last analysis",
	ReasonCooldownExpired:    "The symptom recurred and the cooldown since the last analysis has passed",
	ReasonNotObserved:        "No observation is recorded yet",
	ReasonAnalyzing:          "An analysis is running",
	ReasonNoNewData:          "Nothing was observed since the last analysis",
	ReasonCoolingDown:        "The symptom recurred without a new occurrence within the cooldown",
	ReasonDebouncing:         "Waiting for the incident to settle",
	ReasonAnalysisDisabled:   "RemediationSettings maxAnalysesPerDay is 0",
	ReasonDailyBudgetReached: "The analyses of the current UTC day reached maxAnalysesPerDay",
	ReasonSettingsNotFound:   "The RemediationSettings object does not exist, so the daily budget is unknown",
}
