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
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents/analysisdue"
)

// Read side of the analysis budget, kept here as aliases of [analysisdue], which kubectl-shadewright imports
// without this package's controller-runtime dependencies.
const (
	// BudgetName is [analysisdue.BudgetName].
	BudgetName = analysisdue.BudgetName
	// BudgetDayKey is [analysisdue.BudgetDayKey].
	BudgetDayKey = analysisdue.BudgetDayKey
	// BudgetUsedKey is [analysisdue.BudgetUsedKey].
	BudgetUsedKey = analysisdue.BudgetUsedKey
)

// UsedToday is [analysisdue.UsedToday].
func UsedToday(cm *corev1.ConfigMap, now time.Time) (used int, ok bool) {
	return analysisdue.UsedToday(cm, now)
}

// MaxAnalysesPerDay is [analysisdue.MaxAnalysesPerDay].
func MaxAnalysesPerDay(settings *remediationv1alpha1.RemediationSettings) (limit int, ok bool) {
	return analysisdue.MaxAnalysesPerDay(settings)
}

// reserveBackoff spaces the retries of [ReserveAnalysis] on conflict: jittered, so callers that collided
// do not collide again, and enough of them for each to see the count once the budget is spent.
var reserveBackoff = wait.Backoff{Steps: 8, Duration: 10 * time.Millisecond, Factor: 2, Jitter: 1}

// Errors of [ReserveAnalysis]. Each means: do not call the model.
var (
	// ErrBudgetSpent: the analyses of the current UTC day reached the limit of spec.analysis.maxAnalysesPerDay
	// (or the lower deprecated spec.maxAnalysesPerDay), which may be 0.
	ErrBudgetSpent = errors.New("the daily analysis budget is spent")
	// ErrBudgetUnreadable: the budget ConfigMap holds a day in the future, a count out of bounds, or keys it
	// should not.
	ErrBudgetUnreadable = errors.New("the analysis budget ConfigMap cannot be trusted")
	// ErrBudgetNotFound: the budget ConfigMap does not exist. It is not created again here, so deleting it
	// stops analysis instead of resetting the count.
	ErrBudgetNotFound = errors.New("the analysis budget ConfigMap does not exist")
	// ErrSettingsIncomplete: the RemediationSettings has no spec.analysis.maxAnalysesPerDay, so the daily
	// limit is unknown.
	ErrSettingsIncomplete = errors.New("the RemediationSettings sets no spec.analysis.maxAnalysesPerDay")
)

// ReserveAnalysis takes one unit of the daily analysis budget, before a model-backed analysis (#16). It reads
// the RemediationSettings (spec.analysis.maxAnalysesPerDay) and the budget ConfigMap in namespace fresh with reader, adds one
// to the count (starting at 1 on a new UTC day) and updates the ConfigMap with the resourceVersion it read
// (an update, not a patch), retrying on conflict, so concurrent callers never take the same unit twice. It
// refuses with [ErrSettingsIncomplete] when the settings hold no limit ([MaxAnalysesPerDay]), with
// [ErrBudgetSpent] when the day's count reached the limit, with [ErrBudgetNotFound]
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
		limit, ok := MaxAnalysesPerDay(settings)
		if !ok {
			return ErrSettingsIncomplete
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
		if used >= limit {
			return ErrBudgetSpent
		}
		cm.Data = map[string]string{
			BudgetDayKey:  analysisdue.BudgetDayValue(now),
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
