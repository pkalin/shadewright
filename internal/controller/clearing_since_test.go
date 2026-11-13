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

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	verification "github.com/pkalin/shadewright/internal/verification/clearing"
)

// A window counts no time from before the process began reconciling (ADR-0030 section 4, amendment of
// 2026-11-13). A window opened after that moment counts as stored, and a closed one stays closed.
func TestCountedStartsNoWindowBeforeReconciling(t *testing.T) {
	since := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *metav1.Time { m := metav1.NewTime(since.Add(d)); return &m }
	reset := func(d time.Duration) *remediationv1alpha1.WindowReset {
		return &remediationv1alpha1.WindowReset{Reason: remediationv1alpha1.ResetNotAvailable, Time: *at(d)}
	}
	now := since.Add(5 * time.Minute)
	cases := []struct {
		name string
		w    verification.Window
		span time.Duration
	}{
		{"open before the start counts from the start", verification.Window{Start: at(-time.Hour)}, 5 * time.Minute},
		{"open before the start after a reset", verification.Window{Start: at(-time.Hour), LastReset: reset(-2 * time.Hour)},
			5 * time.Minute},
		{"open after the start is unaffected", verification.Window{Start: at(time.Minute)}, 4 * time.Minute},
		{"open at the start is unaffected", verification.Window{Start: at(0)}, 5 * time.Minute},
		{"closed before the start stays closed", verification.Window{Start: at(-time.Hour), LastReset: reset(-time.Minute)}, 0},
		{"never opened", verification.Window{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &RemediationRequestReconciler{Now: func() time.Time { return since }}
			r.since.mark(since)
			stored := c.w.Start.DeepCopy()
			got := r.counted(c.w)
			if s := got.Span(now); s != c.span {
				t.Errorf("span = %s, want %s", s, c.span)
			}
			if got.Open() != c.w.Open() {
				t.Errorf("open = %t, want %t", got.Open(), c.w.Open())
			}
			if !c.w.Start.Equal(stored) {
				t.Errorf("the window passed in changed its start to %v", c.w.Start)
			}
		})
	}
}

// The moment is taken once: by the runnable that starts when leader election is won, or by the first check
// when that check runs first. A later mark, by either, keeps the first.
func TestReconcilingSinceIsTakenOnce(t *testing.T) {
	elected := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	clock := elected
	r := &RemediationRequestReconciler{Now: func() time.Time { return clock }}
	if !(sinceMarker{r: r}).NeedLeaderElection() {
		t.Fatal("the moment must be taken when leadership is won, not when a standby process starts")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sinceMarker{r: r}.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.since.t.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the runnable did not take the moment")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start: %v", err)
	}
	clock = elected.Add(time.Hour)
	w := verification.Window{Start: &metav1.Time{Time: elected.Add(-time.Hour)}}
	if s := r.counted(w).Span(clock); s != time.Hour {
		t.Errorf("span = %s, want 1h counted from the election", s)
	}

	first := &RemediationRequestReconciler{Now: func() time.Time { return elected }}
	if got := first.since.mark(elected); !got.Equal(elected) {
		t.Fatalf("mark = %s, want %s", got, elected)
	}
	if got := first.since.mark(elected.Add(time.Minute)); !got.Equal(elected) {
		t.Errorf("a later mark moved the moment to %s", got)
	}
}
