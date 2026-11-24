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
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	verification "github.com/shadewright-labs/shadewright/internal/verification/clearing"
)

// ownedCheck is one check of the generated sequences: good or bad, and what became of its status write.
type ownedCheck struct {
	good  bool
	write ownedWrite
}

type ownedWrite int

const (
	writeLost ownedWrite = iota
	writeStored
	writeStoredErr // stored, but the call returned an error (a timeout after the server applied it)
)

func (c ownedCheck) String() string {
	s := "bad"
	if c.good {
		s = "good"
	}
	return [...]string{"lost ", "stored ", "stored-err "}[c.write] + s
}

// ownedChecks is every kind of check the sequences draw from.
var ownedChecks = func() []ownedCheck {
	all := make([]ownedCheck, 0, 6)
	for _, good := range []bool{true, false} {
		for _, wr := range []ownedWrite{writeLost, writeStored, writeStoredErr} {
			all = append(all, ownedCheck{good: good, write: wr})
		}
	}
	return all
}()

// ownedRequest is the request the tests of ownWindows check.
func ownedRequest() *remediationv1alpha1.RemediationRequest {
	rr := &remediationv1alpha1.RemediationRequest{}
	rr.Namespace, rr.Name, rr.UID = "ns", "rr", "uid-1"
	return rr
}

// ownedModel is one process's checks of one Verifying request: the window status holds and the process's memory
// (ownWindows, unwrittenResets), saved and restored around each branch of the search.
type ownedModel struct {
	r      *RemediationRequestReconciler
	rr     *remediationv1alpha1.RemediationRequest
	stored *remediationv1alpha1.VerificationWindow
	base   time.Time
	// left is the start of the last window a check of this process left open, whatever its write became.
	left *metav1.Time
}

type ownedState struct {
	stored          *remediationv1alpha1.VerificationWindow
	left            *metav1.Time
	own, kept       any
	hasOwn, hasKept bool
}

func (m *ownedModel) save() ownedState {
	key := client.ObjectKeyFromObject(m.rr)
	own, hasOwn := m.r.ownWindows.m.Load(key)
	kept, hasKept := m.r.unwrittenResets.m.Load(key)
	return ownedState{stored: m.stored, left: m.left, own: own, hasOwn: hasOwn, kept: kept, hasKept: hasKept}
}

func (m *ownedModel) restore(s ownedState) {
	key := client.ObjectKeyFromObject(m.rr)
	m.stored, m.left = s.stored, s.left
	if s.hasOwn {
		m.r.ownWindows.m.Store(key, s.own)
	} else {
		m.r.ownWindows.m.Delete(key)
	}
	if s.hasKept {
		m.r.unwrittenResets.m.Store(key, s.kept)
	} else {
		m.r.unwrittenResets.m.Delete(key)
	}
}

// read is the request as a check reads it: status holds the stored window.
func (m *ownedModel) read() *remediationv1alpha1.RemediationRequest {
	rr := m.rr.DeepCopy()
	if m.stored != nil {
		rr.Status.Verification = m.stored.DeepCopy()
	}
	return rr
}

// key is the state after d checks with every time taken relative to the time of check d, so two orders that
// reach the same key have the same future: the checks only compare these times with each other and with now.
func (m *ownedModel) key(d int) string {
	ref := m.base.Add(time.Duration(d) * verification.PollInterval)
	rel := func(t *metav1.Time) string {
		if t == nil {
			return "-"
		}
		return t.Sub(ref).String()
	}
	var b strings.Builder
	if v := m.stored; v != nil {
		fmt.Fprintf(&b, "stored %s %d", rel(v.WindowStart), v.Resets)
		if v.LastReset != nil {
			fmt.Fprintf(&b, " %s %s", v.LastReset.Reason, rel(&v.LastReset.Time))
		}
	}
	e := m.r.ownWindows.load(m.rr)
	fmt.Fprintf(&b, "|own %t", e.restart != nil)
	for i := range e.starts {
		fmt.Fprintf(&b, " %s", rel(&e.starts[i]))
	}
	if kept, ok := m.r.unwrittenResets.get(m.rr); ok {
		fmt.Fprintf(&b, "|kept %s %s", kept.Reason, rel(&kept.Time))
	}
	fmt.Fprintf(&b, "|left %s", rel(m.left))
	return b.String()
}

// check runs check number i the way clearStep does (checkWindow, the verdict, storeWindow, noteWindow,
// keepVerifying) and reports whether it recorded ExecutorRestarted.
func (m *ownedModel) check(i int, c ownedCheck) bool {
	rr := m.read()
	w, restart := m.r.checkWindow(rr)
	now := m.base.Add(time.Duration(i+1)*verification.PollInterval + 250*time.Millisecond)
	if c.good {
		w.Good(now)
	} else {
		resetWindow(&w, remediationv1alpha1.ResetNotAvailable, now)
	}
	storeWindow(rr, w)
	m.r.noteWindow(rr, w)
	if w.Open() {
		m.left = w.Start
	}
	switch c.write {
	case writeStored:
		m.stored = rr.Status.Verification.DeepCopy()
		m.r.unwrittenResets.forget(client.ObjectKeyFromObject(rr))
	case writeStoredErr:
		m.stored = rr.Status.Verification.DeepCopy()
	}
	return restart
}

// In one process, with any order of lost, stored and stored-but-errored status writes, no check records
// ExecutorRestarted while the request opened at most ownWindowsBound windows (decision of 2026-11-21 on #970,
// ADR-0030 section 4): every order of up to 8 checks, each good or bad, with a read of the stored window after each. Orders that reach
// the same state are searched once.
// A read of a stored open window other than the last one a check of this process left open is counted, so the search is
// known to reach the case the rule is for.
func TestOwnWindowsEveryOrderOfLostWrites(t *testing.T) {
	const depth = 8
	base := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	r := &RemediationRequestReconciler{Now: func() time.Time { return base }}
	r.since.mark(base.Add(-time.Hour))
	rr := ownedRequest()
	m := &ownedModel{r: r, rr: rr, base: base}

	var path []ownedCheck
	failures, states, olderRead := 0, 0, 0
	// seen holds the states already searched: a second order that reaches one has the same future (key).
	seen := map[string]bool{}
	fail := func(at string) {
		failures++
		if failures <= 5 {
			steps := make([]string, len(path))
			for i, c := range path {
				steps[i] = c.String()
			}
			t.Errorf("ExecutorRestarted recorded in one process %s: %s", at, strings.Join(steps, ", "))
		}
	}
	var walk func()
	walk = func() {
		// A read of the window stored now, as the next check of this process would make it.
		s := m.save()
		read := m.read()
		w := executorWindow(read)
		if w.Open() && m.left != nil && !w.Start.Equal(m.left) {
			olderRead++
		}
		if _, restart := m.r.checkWindow(read); restart {
			fail("at the read after the last check")
		}
		m.restore(s)
		k := fmt.Sprintf("%d %s", len(path), m.key(len(path)))
		if len(path) == depth || seen[k] {
			return
		}
		seen[k] = true
		states++
		for _, c := range ownedChecks {
			s := m.save()
			path = append(path, c)
			if m.check(len(path)-1, c) {
				fail(fmt.Sprintf("at check %d", len(path)))
			}
			if failures <= 5 {
				walk()
			}
			path = path[:len(path)-1]
			m.restore(s)
		}
	}
	walk()
	if failures > 0 {
		t.Fatalf("%d checks or reads recorded a restart that did not happen", failures)
	}
	if olderRead == 0 {
		t.Fatalf("no sequence read a stored window other than the last one this process left open: the search misses the case")
	}
	t.Logf("%d distinct states searched over %d checks, %d reads of an earlier window of this process", states, depth,
		olderRead)
}

// The bound (decision of 2026-11-21 on #970): a request keeps at most ownWindowsBound windows this process
// opened. Past it the oldest is forgotten, and a check that then reads that window records a restart, which
// closes the window again (a longer wait, never a shorter one); the windows still remembered are kept.
func TestOwnWindowsBound(t *testing.T) {
	base := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	r := &RemediationRequestReconciler{Now: func() time.Time { return base }}
	r.since.mark(base.Add(-time.Hour))
	rr := ownedRequest()
	opened := func(i int) verification.Window {
		s := metav1.NewTime(base.Add(time.Duration(i) * time.Minute))
		return verification.Window{Start: &s, Resets: int64(i)}
	}
	for i := range ownWindowsBound + 1 {
		w := opened(i)
		r.ownWindows.note(rr, w)
		r.ownWindows.note(rr, w) // a good check that keeps an open window adds nothing
	}
	oldest := opened(0)
	if !r.restarted(rr, &oldest) {
		t.Errorf("the window past the bound was still taken for this process's")
	}
	if oldest.Open() {
		t.Errorf("the restart left the oldest window open: %+v", oldest)
	}
	for i := 1; i <= ownWindowsBound; i++ {
		w := opened(i)
		if r.restarted(rr, &w) {
			t.Errorf("window %d of the last %d was taken for an earlier process's", i, ownWindowsBound)
		}
	}
	other := rr.DeepCopy()
	other.UID = "uid-2"
	last := opened(ownWindowsBound)
	if !r.restarted(other, &last) {
		t.Errorf("a window noted for another request with the same name was kept")
	}
}
