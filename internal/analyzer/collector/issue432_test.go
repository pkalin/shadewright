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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The reproductions of #432 seen on rc.3 outside the lab (2026-11-20): one cause of one Deployment opened
// several Incidents. Each test drives the collector with the pod states and events of the case, in their order
// and at their times (t0 stands for the first sighting of the case), and requires that every observation lands
// on one Incident, its own fingerprint or, for a linked consequence, its cause's.
//
// Properties (ADR-0039 point 3, amended 2026-11-20), one line each:
//   - P1: one crash loop of one container (non-zero exits, BackOff within KillCauseWindow) lands on one Incident.
//   - P2: two faults of one container more than KillCauseWindow apart land on two Incidents, each consequence
//     on the fault within the window, and a consequence past the window on none.
//   - P3: every cause fingerprint on a consequence is the fingerprint of an observation the collector delivers
//     for that pod (decided 2026-11-21): a pod being deleted offers only its OOM kills (#966).
//
// Evictions are not covered (decided 2026-11-21, #1077): evictionmain_test.go shows they behave as before.

// incidentKeys returns the Incident each observation lands on: the cause's fingerprint for a consequence the
// collector linked, its own fingerprint otherwise.
func incidentKeys(obs []Observation) map[string][]string {
	keys := map[string][]string{}
	for _, o := range obs {
		k := o.Fingerprint
		if o.Class == ClassConsequence && o.CauseFingerprint != "" {
			k = o.CauseFingerprint
		}
		keys[k] = append(keys[k], o.Reason+"/"+string(o.Class))
	}
	return keys
}

// crashLoopPod is the web pod whose app container exits with code 2 (an unknown flag), finished at finished,
// after restarts restarts, and now waits in CrashLoopBackOff.
func crashLoopPod(finished time.Time, restarts int32) *corev1.Pod {
	p := holdPod(reasonBack, sampleFailing, restarts)
	p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{
			ContainerID: fmt.Sprintf("containerd://crash-%d", restarts), Reason: reasonError, ExitCode: 2,
			StartedAt: metav1.NewTime(finished), FinishedAt: metav1.NewTime(finished),
		}}
	return p
}

// TestIssue432CrashLoopBackOffOpensASecondIncident is cases (a) and (c) of #432 (P1): a container exits with
// code 2 (reason Error, read from pod status) and the kubelet reports BackOff for it. On main the termination
// was a symptom of no class and the BackOff found no cause in the pod status, so it was held for
// ConsequenceGrace and emitted unlinked: a second Incident about 2 minutes after the first. A non-zero exit is
// now a cause, and the BackOff links to the newest termination within KillCauseWindow of its own time, on
// either side: here the status already shows a crash 13 seconds after the event.
func TestIssue432CrashLoopBackOffOpensASecondIncident(t *testing.T) {
	// rc.3 (a): Error Incident 20:27:28Z, BackOff Incident 20:29:15Z. t0 stands for 20:25:00Z.
	d, rs, _ := workload()
	ev := consequence("ev-backoff", reasonBack, appPath, t0.Add(2*time.Minute+15*time.Second))
	ev.Series = nil
	h := newHarness(t, Config{}, d, rs, crashLoopPod(t0.Add(20*time.Second), 1), ev)

	got := make([]Observation, 0, 3)
	// 20:25:20 first crash, read from pod status.
	h.clock.set(t0.Add(20 * time.Second))
	h.reconcilePod(webPod)
	got = append(got, h.drain()...)
	// 20:27:28 a later crash of the same container: the gate opens the Error Incident on this sighting.
	h.clock.set(t0.Add(2*time.Minute + 28*time.Second))
	cur := readPod(t, h, webPod)
	cur.Status = crashLoopPod(t0.Add(2*time.Minute+28*time.Second), 5).Status
	h.update(cur)
	h.reconcilePod(webPod)
	got = append(got, h.drain()...)
	// 20:27:15 the kubelet's BackOff, then the samples of a hold, if any, to the end of ConsequenceGrace.
	for _, at := range []time.Duration{2*time.Minute + 15*time.Second, 3*time.Minute + 15*time.Second,
		4*time.Minute + 15*time.Second} {
		h.clock.set(t0.Add(at))
		h.reconcileEvent("ev-backoff")
		got = append(got, h.drain()...)
	}

	if len(got) < 3 {
		t.Fatalf("got %d observations, want the two crashes and the BackOff: %+v", len(got), got)
	}
	if keys := incidentKeys(got); len(keys) != 1 {
		t.Errorf("one crash loop lands on %d Incidents, want 1: %v", len(keys), keys)
	}
}

// TestIssue432TwoFaultsOpenTwoIncidents is P2: a container crashes with exit code 2, then, more than
// KillCauseWindow later, with exit code 1. Each BackOff counts on the crash within the window of its time,
// and a BackOff past the window of the last crash links to nothing. Two faults stay two Incidents.
func TestIssue432TwoFaultsOpenTwoIncidents(t *testing.T) {
	d, rs, _ := workload()
	pod := crashLoopPod(t0, 1)
	h := newHarness(t, Config{}, d, rs, pod)
	var got []Observation
	crash := func(exit int32, restarts int32, at time.Time) Observation {
		h.clock.set(at)
		cur := readPod(t, h, webPod)
		cur.Status = crashLoopPod(at, restarts).Status
		cur.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = exit
		h.update(cur)
		h.reconcilePod(webPod)
		o := h.one()
		got = append(got, o)
		return o
	}
	backOff := func(name string, at time.Time) Observation {
		h.clock.set(at)
		ev := consequence(name, reasonBack, appPath, at)
		ev.Series = nil
		h.create(ev)
		h.reconcileEvent(name)
		o := h.one()
		got = append(got, o)
		return o
	}
	first := crash(2, 2, t0.Add(time.Minute))
	if b := backOff("ev-first", t0.Add(90*time.Second)); b.CauseFingerprint != first.Fingerprint {
		t.Errorf("BackOff of the first fault links to %q, want %q", b.CauseFingerprint, first.Fingerprint)
	}
	second := crash(1, 3, t0.Add(time.Minute+KillCauseWindow+time.Minute))
	if b := backOff("ev-second", t0.Add(time.Minute+KillCauseWindow+90*time.Second)); b.CauseFingerprint != second.Fingerprint {
		t.Errorf("BackOff of the second fault links to %q, want %q", b.CauseFingerprint, second.Fingerprint)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatalf("two faults share the fingerprint %s", first.Fingerprint)
	}
	if keys := incidentKeys(got); len(keys) != 2 {
		t.Errorf("two faults land on %d Incidents, want 2: %v", len(keys), keys)
	}
	late := t0.Add(time.Minute + 2*KillCauseWindow + 2*time.Minute)
	h.clock.set(late)
	ev := consequence("ev-late", reasonBack, appPath, late)
	ev.Series = nil
	h.create(ev)
	h.reconcileEvent("ev-late")
	for _, o := range h.drain() {
		if o.CauseFingerprint != "" {
			t.Errorf("BackOff past KillCauseWindow of the last crash links to %q, want none", o.CauseFingerprint)
		}
	}
}

// TestIssue432LinkWindow is the window of a termination cause (P1, P2): a non-zero exit links within
// KillCauseWindow on either side of the consequence's time, an OOM kill only after its finish (ADR-0039 point 5).
func TestIssue432LinkWindow(t *testing.T) {
	cases := []struct {
		reason string
		at     time.Duration // the consequence's time relative to the finish
		want   bool
	}{
		{reasonError, 0, true},
		{reasonError, KillCauseWindow, true},
		{reasonError, KillCauseWindow + time.Second, false},
		{reasonError, -KillCauseWindow, true},
		{reasonError, -KillCauseWindow - time.Second, false},
		{ReasonOOMKilled, 0, true},
		{ReasonOOMKilled, KillCauseWindow, true},
		{ReasonOOMKilled, KillCauseWindow + time.Second, false},
		{ReasonOOMKilled, -time.Second, false},
	}
	for _, c := range cases {
		if got := linksAt(termination{reason: c.reason, finished: t0}, t0.Add(c.at)); got != c.want {
			t.Errorf("%s at %v: links %t, want %t", c.reason, c.at, got, c.want)
		}
	}
}
