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
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// budgetOf reads the data of the budget ConfigMap in ns.
func budgetOf(t *testing.T, ns string) map[string]string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: BudgetName}, cm); err != nil {
		t.Fatal(err)
	}
	return cm.Data
}

// maxAnalysesPerDay goes by spec.analysis.maxAnalysesPerDay, takes the deprecated spec.maxAnalysesPerDay when
// that is set and lower, and reports no limit (fail closed) when spec.analysis or its field is unset, never a
// default.
func TestMaxAnalysesPerDayFailsClosedAndTakesTheLower(t *testing.T) {
	for _, tc := range []struct {
		name     string
		analysis *remediationv1alpha1.AnalysisSettings
		old      *int32
		want     int
		wantOK   bool
	}{
		{"no analysis block", nil, nil, 0, false},
		{"no analysis block, old field set", nil, new(int32(5)), 0, false},
		{"empty analysis block", &remediationv1alpha1.AnalysisSettings{}, nil, 0, false},
		{"set to 0", &remediationv1alpha1.AnalysisSettings{MaxAnalysesPerDay: new(int32(0))}, nil, 0, true},
		{"set to 7", &remediationv1alpha1.AnalysisSettings{MaxAnalysesPerDay: new(int32(7))}, nil, 7, true},
		{"old 0 wins over 100", &remediationv1alpha1.AnalysisSettings{MaxAnalysesPerDay: new(int32(100))}, new(int32(0)), 0, true},
		{"old 50 wins over 100", &remediationv1alpha1.AnalysisSettings{MaxAnalysesPerDay: new(int32(100))}, new(int32(50)), 50, true},
		{"new 3 wins over old 50", &remediationv1alpha1.AnalysisSettings{MaxAnalysesPerDay: new(int32(3))}, new(int32(50)), 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &remediationv1alpha1.RemediationSettings{}
			s.Spec.Analysis, s.Spec.MaxAnalysesPerDay = tc.analysis, tc.old
			if got, ok := maxAnalysesPerDay(s); got != tc.want || ok != tc.wantOK {
				t.Fatalf("maxAnalysesPerDay = %d, %v; want %d, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// editedSettings is a reader that, while edit is set, changes the RemediationSettings it returns, to show the
// analyzer objects the API server would not store today: one without spec.analysis (not defaulted) or one
// with the deprecated spec.maxAnalysesPerDay set (stored by v0.1.0).
type editedSettings struct {
	client.Reader
	edit atomic.Pointer[func(*remediationv1alpha1.RemediationSettings)]
}

func (e *editedSettings) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := e.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if s, ok := obj.(*remediationv1alpha1.RemediationSettings); ok {
		if f := e.edit.Load(); f != nil {
			(*f)(s)
		}
	}
	return nil
}

// withoutAnalysis drops spec.analysis, like an object the schema's defaults did not fill in.
func withoutAnalysis(s *remediationv1alpha1.RemediationSettings) { s.Spec.Analysis = nil }

// withOldZero sets the deprecated spec.maxAnalysesPerDay to 0, like an object v0.1.0's runbook paused.
func withOldZero(s *remediationv1alpha1.RemediationSettings) {
	s.Spec.MaxAnalysesPerDay = new(int32(0))
}

// ReserveAnalysis refuses when the settings hold no daily limit, and when the deprecated field is a lower 0,
// and takes no unit either way.
func TestReserveAnalysisFailsClosedOnTheSettings(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 10)
	createBudget(t, ns, nil)
	r := &editedSettings{Reader: env.admin}
	for _, tc := range []struct {
		edit func(*remediationv1alpha1.RemediationSettings)
		want error
	}{
		{withoutAnalysis, ErrSettingsIncomplete},
		{withOldZero, ErrBudgetSpent},
	} {
		r.edit.Store(&tc.edit)
		if err := ReserveAnalysis(t.Context(), r, env.admin, ns, triggerNow); !errors.Is(err, tc.want) {
			t.Fatalf("reservation = %v, want %v", err, tc.want)
		}
	}
	if b := budgetOf(t, ns); b[BudgetUsedKey] != "" {
		t.Fatalf("a refused reservation changed the budget to %v", b)
	}
}

// ReserveAnalysis counts up to spec.analysis.maxAnalysesPerDay, refuses beyond it, and starts again on a new UTC day.
func TestReserveAnalysisCountsAndRollsOver(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 2)
	createBudget(t, ns, budgetData(yesterday, "2"))
	for i := 1; i <= 2; i++ {
		if err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, triggerNow); err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
		if b := budgetOf(t, ns); b[BudgetDayKey] != today || b[BudgetUsedKey] != strconv.Itoa(i) {
			t.Fatalf("after reservation %d the budget is %v", i, b)
		}
	}
	if err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, triggerNow); !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("a third reservation of 2 = %v, want ErrBudgetSpent", err)
	}
	if b := budgetOf(t, ns); b[BudgetUsedKey] != "2" {
		t.Fatalf("a refused reservation changed the budget to %v", b)
	}
	// Just after midnight UTC the day starts again.
	if err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, time.Date(2026, 11, 4, 0, 0, 1, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if b := budgetOf(t, ns); b[BudgetDayKey] != tomorrow || b[BudgetUsedKey] != "1" {
		t.Fatalf("after midnight the budget is %v", b)
	}
}

// Reservations from many callers at once take each unit once: conflicts are retried on a fresh read.
func TestReserveAnalysisIsConflictSafe(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 5)
	createBudget(t, ns, nil)
	var (
		wg               sync.WaitGroup
		mu               sync.Mutex
		granted, refused int
		others           []error
	)
	for range 12 {
		wg.Go(func() {
			err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, triggerNow)
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
	if b := budgetOf(t, ns); granted != 5 || refused != 7 || b[BudgetUsedKey] != "5" {
		t.Fatalf("granted %d, refused %d, budget %v; want 5, 7, 5 used", granted, refused, b)
	}
}

// A budget that cannot be trusted, a missing budget, a budget of 0 and missing settings all refuse, and
// none of them writes the ConfigMap.
func TestReserveAnalysisFailsClosed(t *testing.T) {
	t.Run("no settings", func(t *testing.T) {
		ns := namespace(t)
		createBudget(t, ns, nil)
		if err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, triggerNow); !apierrors.IsNotFound(err) {
			t.Fatalf("without settings = %v, want not found", err)
		}
	})
	t.Run("no budget ConfigMap", func(t *testing.T) {
		ns := namespace(t)
		createSettings(t, 5)
		if err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, triggerNow); !errors.Is(err, ErrBudgetNotFound) {
			t.Fatalf("without the ConfigMap = %v, want ErrBudgetNotFound", err)
		}
		cm := &corev1.ConfigMap{}
		if err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: BudgetName}, cm); !apierrors.IsNotFound(err) {
			t.Fatalf("a refused reservation created the ConfigMap: %v", err)
		}
	})
	t.Run("future day", func(t *testing.T) {
		ns := namespace(t)
		createSettings(t, 5)
		createBudget(t, ns, budgetData(tomorrow, "0"))
		if err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, triggerNow); !errors.Is(err, ErrBudgetUnreadable) {
			t.Fatalf("a budget from a future day = %v, want ErrBudgetUnreadable", err)
		}
		if b := budgetOf(t, ns); b[BudgetDayKey] != tomorrow || b[BudgetUsedKey] != "0" {
			t.Fatalf("a refused reservation changed the budget to %v", b)
		}
	})
	t.Run("another key", func(t *testing.T) {
		ns := namespace(t)
		createSettings(t, 5)
		createBudget(t, ns, map[string]string{BudgetDayKey: today, BudgetUsedKey: "0", "used2": "0"})
		if err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, triggerNow); !errors.Is(err, ErrBudgetUnreadable) {
			t.Fatalf("a budget with another key = %v, want ErrBudgetUnreadable", err)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		ns := namespace(t)
		createSettings(t, 0)
		createBudget(t, ns, nil)
		if err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, triggerNow); !errors.Is(err, ErrBudgetSpent) {
			t.Fatalf("a budget of 0 = %v, want ErrBudgetSpent", err)
		}
	})
}
