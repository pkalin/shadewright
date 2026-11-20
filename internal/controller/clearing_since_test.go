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
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	verification "github.com/pkalin/shadewright/internal/verification/clearing"
)

// The first check of a new process resets every open window that opened before the process began reconciling,
// with ExecutorRestarted, in the window it stores (ADR-0030 section 4, amendment of 2026-11-20, #895). The reset
// is the second before that moment, so a good check of the new process opens the window again at once. A window
// opened since, or in the same second, and a closed one stay as they are.
func TestRestartedResetsWindowsFromBeforeReconciling(t *testing.T) {
	since := time.Date(2026, 11, 13, 12, 0, 0, 500_000_000, time.UTC)
	second := since.Truncate(time.Second)
	at := func(d time.Duration) *metav1.Time { m := metav1.NewTime(second.Add(d)); return &m }
	reset := func(d time.Duration) *remediationv1alpha1.WindowReset {
		return &remediationv1alpha1.WindowReset{Reason: remediationv1alpha1.ResetNotAvailable, Time: *at(d)}
	}
	restartReset := &remediationv1alpha1.WindowReset{Reason: remediationv1alpha1.ResetExecutorRestarted,
		Time: *at(-time.Second)}
	cases := []struct {
		name string
		w    verification.Window
		want verification.Window
	}{
		{"open before the start is reset", verification.Window{Start: at(-time.Hour), Resets: 2},
			verification.Window{Start: at(-time.Hour), LastReset: restartReset, Resets: 3}},
		{"open before the start after a reset is reset",
			verification.Window{Start: at(-time.Hour), LastReset: reset(-2 * time.Hour), Resets: 1},
			verification.Window{Start: at(-time.Hour), LastReset: restartReset, Resets: 2}},
		{"open one second before the start is reset at its start",
			verification.Window{Start: at(-time.Second)},
			verification.Window{Start: at(-time.Second), LastReset: restartReset, Resets: 1}},
		{"open in the second of the start is kept", verification.Window{Start: at(0)},
			verification.Window{Start: at(0)}},
		{"open after the start is kept", verification.Window{Start: at(time.Minute)},
			verification.Window{Start: at(time.Minute)}},
		{"closed before the start is kept",
			verification.Window{Start: at(-time.Hour), LastReset: reset(-time.Minute), Resets: 1},
			verification.Window{Start: at(-time.Hour), LastReset: reset(-time.Minute), Resets: 1}},
		{"never opened is kept", verification.Window{}, verification.Window{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &RemediationRequestReconciler{Now: func() time.Time { return since.Add(time.Hour) }}
			r.since.mark(since)
			got := c.w
			r.restarted(&got)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("window = %+v, want %+v", got, c.want)
			}
			// A second check of the same process changes nothing more, and a good check opens the window at
			// once: the window is never open from before the start of reconciling.
			again := got
			r.restarted(&again)
			if !reflect.DeepEqual(again, got) {
				t.Errorf("a second check changed the window to %+v", again)
			}
			again.Good(since)
			if !again.Open() || again.Start.Before(at(0)) {
				t.Errorf("after a good check at the start the window is %+v, want open from the start", again)
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
	r.restarted(&w)
	if w.LastReset == nil || !w.LastReset.Time.Equal(&metav1.Time{Time: elected.Add(-time.Second)}) {
		t.Errorf("last reset = %+v, want the second before the election", w.LastReset)
	}

	first := &RemediationRequestReconciler{Now: func() time.Time { return elected }}
	if got := first.since.mark(elected); !got.Equal(elected) {
		t.Fatalf("mark = %s, want %s", got, elected)
	}
	if got := first.since.mark(elected.Add(time.Minute)); !got.Equal(elected) {
		t.Errorf("a later mark moved the moment to %s", got)
	}
}
