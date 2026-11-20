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

// The first check of a new process resets every open window that no check of its own left open, with
// ExecutorRestarted, in the window it stores (ADR-0030 section 4, amendments of 2026-11-20, #895). Which window is
// its own is decided by its memory, never by comparing the stored windowStart, on the clock of the process that
// wrote it, with the start of reconciling on this one: the old leader's clock may be behind this one's (its
// windowStart looks earlier) or ahead (it looks later, even after the start of reconciling). Either way the
// window is reset. The reset is the second before that moment, or the window's start when that is later, so a
// good check of the new process opens the window again once this clock passes both. A closed window stays as it
// is, and a window this process left open is kept.
func TestRestartedResetsWindowsNotOfThisProcess(t *testing.T) {
	since := time.Date(2026, 11, 13, 12, 0, 0, 500_000_000, time.UTC)
	second := since.Truncate(time.Second)
	at := func(d time.Duration) *metav1.Time { m := metav1.NewTime(second.Add(d)); return &m }
	reset := func(d time.Duration) *remediationv1alpha1.WindowReset {
		return &remediationv1alpha1.WindowReset{Reason: remediationv1alpha1.ResetNotAvailable, Time: *at(d)}
	}
	restartAt := func(d time.Duration) *remediationv1alpha1.WindowReset {
		return &remediationv1alpha1.WindowReset{Reason: remediationv1alpha1.ResetExecutorRestarted, Time: *at(d)}
	}
	cases := []struct {
		name string
		w    verification.Window
		want verification.Window
		// opens is the earliest time of this clock at which a good check opens the window again.
		opens time.Duration
	}{
		{"open long before the start is reset", verification.Window{Start: at(-time.Hour), Resets: 2},
			verification.Window{Start: at(-time.Hour), LastReset: restartAt(-time.Second), Resets: 3}, 0},
		{"open before the start after a reset is reset",
			verification.Window{Start: at(-time.Hour), LastReset: reset(-2 * time.Hour), Resets: 1},
			verification.Window{Start: at(-time.Hour), LastReset: restartAt(-time.Second), Resets: 2}, 0},
		{"open one second before the start is reset",
			verification.Window{Start: at(-time.Second)},
			verification.Window{Start: at(-time.Second), LastReset: restartAt(-time.Second), Resets: 1}, 0},
		// Skew: the old leader's clock is ahead of this one's, so the start it wrote looks like this process's
		// own time. It is still reset, at that start, and opens again only after it.
		{"open in the second of the start, old clock ahead, is reset", verification.Window{Start: at(0)},
			verification.Window{Start: at(0), LastReset: restartAt(0), Resets: 1}, time.Second},
		{"open two minutes after the start, old clock ahead, is reset",
			verification.Window{Start: at(2 * time.Minute)},
			verification.Window{Start: at(2 * time.Minute), LastReset: restartAt(2 * time.Minute), Resets: 1},
			2*time.Minute + time.Second},
		// Skew the other way: the old leader's clock is behind, its window looks two minutes older.
		{"open two minutes before the start, old clock behind, is reset",
			verification.Window{Start: at(-2 * time.Minute), LastReset: reset(-3 * time.Minute), Resets: 1},
			verification.Window{Start: at(-2 * time.Minute), LastReset: restartAt(-time.Second), Resets: 2}, 0},
		{"closed before the start is kept",
			verification.Window{Start: at(-time.Hour), LastReset: reset(-time.Minute), Resets: 1},
			verification.Window{Start: at(-time.Hour), LastReset: reset(-time.Minute), Resets: 1}, 0},
		{"never opened is kept", verification.Window{}, verification.Window{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &RemediationRequestReconciler{Now: func() time.Time { return since.Add(time.Hour) }}
			r.since.mark(since)
			rr := &remediationv1alpha1.RemediationRequest{}
			rr.Namespace, rr.Name, rr.UID = "ns", "rr", "uid-1"
			got := c.w
			fired := r.restarted(rr, &got)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("window = %+v, want %+v", got, c.want)
			}
			if fired != c.w.Open() {
				t.Errorf("restarted = %t, want %t", fired, c.w.Open())
			}
			if got.Open() {
				t.Fatalf("the window is open from before this process: %+v", got)
			}
			// A good check before the reset's time leaves it closed; at the time given it opens.
			early := got
			early.Good(second.Add(c.opens - time.Second))
			if c.opens > 0 && early.Open() {
				t.Errorf("a good check before the window's start opened it: %+v", early)
			}
			again := got
			again.Good(second.Add(c.opens))
			if c.w.Start != nil && !again.Open() {
				t.Fatalf("a good check at +%s left the window closed: %+v", c.opens, again)
			}
			// The check notes the window it left open: the next check of this process keeps it.
			r.ownWindows.note(rr, again)
			kept := again
			if r.restarted(rr, &kept) || !reflect.DeepEqual(kept, again) {
				t.Errorf("a second check changed this process's window to %+v", kept)
			}
			// A later check of this process closes the window and its write is lost: status still holds the
			// window this process opened, which is not taken for an earlier process's.
			closed := again
			closed.Reset(remediationv1alpha1.ResetNotAvailable, second.Add(c.opens+time.Minute))
			r.ownWindows.note(rr, closed)
			stored := again
			if r.restarted(rr, &stored) {
				t.Errorf("a window of this process whose reset was lost was taken for an earlier process's")
			}
			// The same window under a new request with the same name is not this process's.
			other := rr.DeepCopy()
			other.UID = "uid-2"
			if again.Open() && !r.restarted(other, &kept) {
				t.Errorf("a window noted for another request with the same name was kept")
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
	r.restarted(&remediationv1alpha1.RemediationRequest{}, &w)
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
