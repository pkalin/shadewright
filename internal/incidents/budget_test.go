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
	"errors"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// budgetOf reads the analysis budget from the RemediationSettings status.
func budgetOf(t *testing.T) *remediationv1alpha1.AnalysisBudget {
	t.Helper()
	s := &remediationv1alpha1.RemediationSettings{}
	if err := env.admin.Get(t.Context(), client.ObjectKey{Name: remediationv1alpha1.SettingsName}, s); err != nil {
		t.Fatal(err)
	}
	return s.Status.AnalysisBudget
}

// ReserveAnalysis counts up to maxAnalysesPerDay, refuses beyond it, and starts again on a new UTC day.
func TestReserveAnalysisCountsAndRollsOver(t *testing.T) {
	namespace(t) // starts the API server
	createSettings(t, 2, &remediationv1alpha1.AnalysisBudget{Day: yesterday, Used: 2})
	for i := int32(1); i <= 2; i++ {
		if err := ReserveAnalysis(t.Context(), env.admin, env.admin, triggerNow); err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
		if b := budgetOf(t); b.Day != today || b.Used != i {
			t.Fatalf("after reservation %d the budget is %+v", i, b)
		}
	}
	if err := ReserveAnalysis(t.Context(), env.admin, env.admin, triggerNow); !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("a third reservation of 2 = %v, want ErrBudgetSpent", err)
	}
	if b := budgetOf(t); b.Used != 2 {
		t.Fatalf("a refused reservation changed the budget to %+v", b)
	}
	// Just after midnight UTC the day starts again.
	if err := ReserveAnalysis(t.Context(), env.admin, env.admin, time.Date(2026, 11, 4, 0, 0, 1, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if b := budgetOf(t); b.Day != tomorrow || b.Used != 1 {
		t.Fatalf("after midnight the budget is %+v", b)
	}
}

// Reservations from many callers at once take each unit once: conflicts are retried on a fresh read.
func TestReserveAnalysisIsConflictSafe(t *testing.T) {
	namespace(t)
	createSettings(t, 5, nil)
	var (
		wg               sync.WaitGroup
		mu               sync.Mutex
		granted, refused int
		others           []error
	)
	for range 12 {
		wg.Go(func() {
			err := ReserveAnalysis(t.Context(), env.admin, env.admin, triggerNow)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				granted++
			case errors.Is(err, ErrBudgetSpent):
				refused++
			default:
				others = append(others, err)
			}
		})
	}
	wg.Wait()
	if len(others) > 0 {
		t.Fatalf("unexpected errors: %v", others)
	}
	if granted != 5 || refused != 7 || budgetOf(t).Used != 5 {
		t.Fatalf("granted %d, refused %d, budget %+v; want 5, 7, 5 used", granted, refused, budgetOf(t))
	}
}

// A budget that cannot be trusted, a budget of 0 and missing settings all refuse.
func TestReserveAnalysisFailsClosed(t *testing.T) {
	namespace(t)
	err := ReserveAnalysis(t.Context(), env.admin, env.admin, triggerNow)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("without settings = %v, want not found", err)
	}
	t.Run("future day", func(t *testing.T) {
		createSettings(t, 5, &remediationv1alpha1.AnalysisBudget{Day: tomorrow, Used: 0})
		if err := ReserveAnalysis(t.Context(), env.admin, env.admin, triggerNow); !errors.Is(err, ErrBudgetUnreadable) {
			t.Fatalf("a budget from a future day = %v, want ErrBudgetUnreadable", err)
		}
		if b := budgetOf(t); b.Day != tomorrow || b.Used != 0 {
			t.Fatalf("a refused reservation changed the budget to %+v", b)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		createSettings(t, 0, nil)
		if err := ReserveAnalysis(t.Context(), env.admin, env.admin, triggerNow); !errors.Is(err, ErrBudgetSpent) {
			t.Fatalf("a budget of 0 = %v, want ErrBudgetSpent", err)
		}
	})
}
