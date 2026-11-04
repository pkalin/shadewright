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
	"regexp"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
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

// maxBudgetUsed is the largest count the budget holds, the largest maxAnalysesPerDay.
const maxBudgetUsed = 10000

// budgetUsedPattern is the form of the count: plain decimal digits, no sign, space or leading plus.
var budgetUsedPattern = regexp.MustCompile(`^[0-9]{1,5}$`)

// reserveBackoff spaces the retries of [ReserveAnalysis] on conflict: jittered, so callers that collided
// do not collide again, and enough of them for each to see the count once the budget is spent.
var reserveBackoff = wait.Backoff{Steps: 8, Duration: 10 * time.Millisecond, Factor: 2, Jitter: 1}

// Errors of [ReserveAnalysis]. Each means: do not call the model.
var (
	// ErrBudgetSpent: the analyses of the current UTC day reached maxAnalysesPerDay, which may be 0.
	ErrBudgetSpent = errors.New("the daily analysis budget is spent")
	// ErrBudgetUnreadable: the budget ConfigMap holds a day in the future, a count out of bounds, or keys it
	// should not.
	ErrBudgetUnreadable = errors.New("the analysis budget ConfigMap cannot be trusted")
	// ErrBudgetNotFound: the budget ConfigMap does not exist. It is not created again here, so deleting it
	// stops analysis instead of resetting the count.
	ErrBudgetNotFound = errors.New("the analysis budget ConfigMap does not exist")
)

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

// maxAnalysesPerDay returns the settings' maxAnalysesPerDay, or its default when unset.
func maxAnalysesPerDay(settings *remediationv1alpha1.RemediationSettings) int {
	if settings.Spec.MaxAnalysesPerDay != nil {
		return int(*settings.Spec.MaxAnalysesPerDay)
	}
	return DefaultMaxAnalysesPerDay
}

// ReserveAnalysis takes one unit of the daily analysis budget, before a model-backed analysis (#16). It reads
// the RemediationSettings (maxAnalysesPerDay) and the budget ConfigMap in namespace fresh with reader, adds one
// to the count (starting at 1 on a new UTC day) and updates the ConfigMap with the resourceVersion it read
// (an update, not a patch), retrying on conflict, so concurrent callers never take the same unit twice. It
// refuses with [ErrBudgetSpent] when the day's count reached maxAnalysesPerDay, with [ErrBudgetNotFound]
// when the ConfigMap does not exist, and with [ErrBudgetUnreadable] when its content cannot be trusted
// ([UsedToday]).
//
// Only a nil error allows the model call. Any other error, including a lost answer, means do not call the
// model; the unit may still have been taken (an overcount, never an undercount). The count is never given
// back, so a failed analysis still uses its unit.
func ReserveAnalysis(ctx context.Context, reader client.Reader, writer client.Writer, namespace string, now time.Time) error {
	err := retry.RetryOnConflict(reserveBackoff, func() error {
		settings := &remediationv1alpha1.RemediationSettings{}
		if err := reader.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); err != nil {
			return fmt.Errorf("reading RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
		}
		cm := &corev1.ConfigMap{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: BudgetName}, cm); err != nil {
			if apierrors.IsNotFound(err) {
				return ErrBudgetNotFound
			}
			return err
		}
		used, ok := UsedToday(cm, now)
		if !ok {
			return ErrBudgetUnreadable
		}
		if used >= maxAnalysesPerDay(settings) {
			return ErrBudgetSpent
		}
		cm.Data = map[string]string{
			BudgetDayKey:  budgetDay(now).Format(budgetDayLayout),
			BudgetUsedKey: strconv.Itoa(used + 1),
		}
		return writer.Update(ctx, cm, client.FieldOwner(triggerFieldOwner))
	})
	recordReservation(err)
	if err != nil {
		return fmt.Errorf("reserving an analysis in ConfigMap %s/%s: %w", namespace, BudgetName, err)
	}
	return nil
}
