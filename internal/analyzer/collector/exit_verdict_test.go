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

package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// TestFailedExitAskRetriedWithinGrace: CauseSeen runs out of time when asked about a held BackOff's exit. The hold
// asks again after retryAfter, as it does for an OOM kill or a wait, not only at the end of ConsequenceGrace, and
// an answer yes at that retry releases it with the exit.
func TestFailedExitAskRetriedWithinGrace(t *testing.T) {
	e := newExitHold(t, true, Config{})
	e.log.book.fail(context.DeadlineExceeded)
	sink := newKVSink()
	if res := e.eventLogged(sink, 0); res.RequeueAfter != retryAfter {
		t.Fatalf("requeue after %s, want the retry after %s", res.RequeueAfter, retryAfter)
	}
	if got := e.end(); got != endDropped {
		t.Fatalf("after the failed ask the consequence %s, want it still held", got)
	}
	e.log.book.fail(nil)
	e.log.book.record(e.fp)
	e.eventLogged(sink, retryAfter)
	if got := e.end(); got != endCause {
		t.Errorf("the consequence ended %s, want %s at the retry", got, endCause)
	}
}

// TestHeldExitsKeepTheNewest: a hold that already has maxHeldExits exit candidates drops the oldest for a newer
// one, so the newest exit, the one most likely to have an Incident, is the one it keeps.
func TestHeldExitsKeepTheNewest(t *testing.T) {
	var s consequenceHold
	var want []string
	for i := range maxHeldExits + 2 {
		fp := fmt.Sprintf("exit-%d", i)
		s.addExit(Observation{exitCause: fp})
		want = append(want, fp)
	}
	want = want[len(want)-maxHeldExits:]
	if got := s.causes(); !slices.Equal(got, want) {
		t.Errorf("the hold keeps exits %v, want the newest %v", got, want)
	}
}

// oldExit and newExit are the fingerprints of the two exit candidates of TestExitVerdictNamesTheFailedExit.
const (
	oldExit = "exit-old"
	newExit = "exit-new"
)

// TestExitVerdictNamesTheFailedExit: with two exit candidates, the ask about the older one fails and the newer one
// is not confirmed. The cause_unanswered log names the exit whose ask failed, not the last one.
func TestExitVerdictNamesTheFailedExit(t *testing.T) {
	e := newExitHold(t, true, Config{})
	e.c.cfg.CauseSeen = func(_ context.Context, o Observation) (bool, error) {
		if o.CauseFingerprint == oldExit {
			return false, errors.New("no answer")
		}
		return false, nil
	}
	var s consequenceHold
	s.addExit(Observation{exitCause: oldExit})
	s.addExit(Observation{exitCause: newExit})
	sink := newKVSink()
	if err := e.c.judgeExits(logf.IntoContext(t.Context(), logr.New(sink)), &s); err != nil {
		t.Fatal(err)
	}
	var named []any
	for _, l := range sink.lines() {
		if l.error {
			named = append(named, l.kv["causeFingerprint"])
		}
	}
	if len(named) != 1 || named[0] != oldExit {
		t.Errorf("the error log names %v, want once the exit whose ask failed, exit-old", named)
	}
}

// TestTableOfOneConfirmedExitFreesRoom pins the decided behaviour for a hold table of one (decided 2026-11-21 on
// #432): a BackOff whose exit the controller has seen leaves at once, emitted with the exit, and takes no room. The
// Unhealthy event that follows is then held, as any event with room in the table; its container recovers within
// the grace, so it opens nothing. On main the BackOff took the one place and the Unhealthy event overflowed and was
// emitted: the difference is the room in the table, not the link.
func TestTableOfOneConfirmedExitFreesRoom(t *testing.T) {
	e := newExitHold(t, true, Config{MaxHeldConsequences: 1})
	e.log.book.record(e.fp)
	e.event(0)
	if got := e.end(); got != endCause {
		t.Fatalf("the BackOff ended %s, want %s at once", got, endCause)
	}
	if n := e.c.holds.len(); n != 0 {
		t.Fatalf("%d holds after the BackOff left, want none", n)
	}
	unhealthy := consequence("ev-unhealthy", ReasonUnhealthy, appPath, e.at.Add(10*time.Second))
	e.create(unhealthy)
	e.clock.set(e.at.Add(10 * time.Second))
	e.reconcileEvent(unhealthy.Name)
	if n := e.c.holds.len(); n != 1 {
		t.Fatalf("%d holds after the Unhealthy event, want it held", n)
	}
	p := readPod(t, e.harness, webPod)
	p.Status.ContainerStatuses[0].State = running(e.at.Add(20 * time.Second))
	p.Status.ContainerStatuses[0].Ready = true
	e.update(p)
	got := make([]Observation, 0, 4)
	for _, after := range []time.Duration{time.Minute, ConsequenceGrace + 11*time.Second} {
		e.clock.set(e.at.Add(after))
		e.reconcileEvent(unhealthy.Name)
		got = append(got, e.drain()...)
	}
	for _, o := range got {
		if o.Class == ClassConsequence && o.Reason == ReasonUnhealthy {
			t.Errorf("the recovered Unhealthy event was emitted (cause %q), want it dropped by its hold",
				o.CauseFingerprint)
		}
	}
	if n := e.c.holds.len(); n != 0 {
		t.Errorf("%d holds at the end, want the Unhealthy hold ended", n)
	}
}
