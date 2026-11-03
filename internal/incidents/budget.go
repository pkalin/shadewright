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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// budgetDayLayout is the layout of RemediationSettings status.analysisBudget.day.
const budgetDayLayout = "2006-01-02"

// maxBudgetUsed is the largest count the schema allows, the largest maxAnalysesPerDay.
const maxBudgetUsed = 10000

// reserveBackoff spaces the retries of [ReserveAnalysis] on conflict: jittered, so callers that collided
// do not collide again, and enough of them for each to see the count once the budget is spent.
var reserveBackoff = wait.Backoff{Steps: 8, Duration: 10 * time.Millisecond, Factor: 2, Jitter: 1}

// Errors of [ReserveAnalysis]. Either means: do not call the model.
var (
	// ErrBudgetSpent: the analyses of the current UTC day reached maxAnalysesPerDay, which may be 0.
	ErrBudgetSpent = errors.New("the daily analysis budget is spent")
	// ErrBudgetUnreadable: the RemediationSettings status holds a budget from a future day or out of bounds.
	ErrBudgetUnreadable = errors.New("the analysis budget in the RemediationSettings status cannot be trusted")
)

// budgetDay returns the UTC day of t as the status writes it.
func budgetDay(t time.Time) string { return t.UTC().Format(budgetDayLayout) }

// UsedToday returns the analyses counted against the UTC day of now, from the RemediationSettings status
// (status.analysisBudget). ok is false when that count cannot be trusted: a day that does not parse or lies
// after today (a clock ahead, or a forged value), or a count out of bounds. The caller then takes the budget
// as spent (fail closed). No budget, or one from an earlier day, means none used today.
func UsedToday(settings *remediationv1alpha1.RemediationSettings, now time.Time) (used int, ok bool) {
	b := settings.Status.AnalysisBudget
	if b == nil {
		return 0, true
	}
	day, err := time.Parse(budgetDayLayout, b.Day)
	if err != nil || b.Used < 0 || b.Used > maxBudgetUsed {
		return 0, false
	}
	today, _ := time.Parse(budgetDayLayout, budgetDay(now))
	switch {
	case day.After(today):
		return 0, false
	case day.Before(today):
		return 0, true
	}
	return int(b.Used), true
}

// maxAnalysesPerDay returns the settings' maxAnalysesPerDay, or its default when unset.
func maxAnalysesPerDay(settings *remediationv1alpha1.RemediationSettings) int {
	if settings.Spec.MaxAnalysesPerDay != nil {
		return int(*settings.Spec.MaxAnalysesPerDay)
	}
	return DefaultMaxAnalysesPerDay
}

// ReserveAnalysis takes one unit of the daily analysis budget, before a model-backed analysis (#16). It
// reads the RemediationSettings fresh with reader, adds one to status.analysisBudget (starting at 1 on a new
// UTC day) and updates the status with the resourceVersion it read, retrying on conflict, so concurrent
// callers never take the same unit twice. It refuses with [ErrBudgetSpent] when the day's count reached
// maxAnalysesPerDay, and with [ErrBudgetUnreadable] when the stored budget cannot be trusted ([UsedToday]).
//
// Only a nil error allows the model call. Any other error, including a lost answer, means do not call the
// model; the unit may still have been taken (an overcount, never an undercount). The count is never given
// back, so a failed analysis still uses its unit.
func ReserveAnalysis(ctx context.Context, reader client.Reader, writer client.Client, now time.Time) error {
	err := retry.RetryOnConflict(reserveBackoff, func() error {
		settings := &remediationv1alpha1.RemediationSettings{}
		if err := reader.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); err != nil {
			return err
		}
		used, ok := UsedToday(settings, now)
		if !ok {
			return ErrBudgetUnreadable
		}
		if used >= maxAnalysesPerDay(settings) {
			return ErrBudgetSpent
		}
		settings.Status.AnalysisBudget = &remediationv1alpha1.AnalysisBudget{Day: budgetDay(now), Used: int32(used + 1)}
		return writer.Status().Update(ctx, settings, client.FieldOwner(triggerFieldOwner))
	})
	if err != nil {
		return fmt.Errorf("reserving an analysis in RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}
	return nil
}
