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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// Evictions are out of the scope of #432 (decided 2026-11-21, designed in #1077): an eviction, its
// terminations, the kubelet's Evicted event and the BackOff and Unhealthy events of an evicted pod's containers
// behave as they did before the crash loop link was built. TestEvictionsAsBeforeTheCrashLoopLink feeds eviction
// sequences through the collector and compares every observation with testdata/evictions-before-432.golden.
// That file was written by this same test, with WRITE_EVICTIONS_GOLDEN=1, on a checkout of origin/main at
// f2d69cd7 (before #432), so its values come from the code before the change, not from the code under test.
//
// The one difference allowed is the crash loop link itself (ADR-0039 point 3, amended 2026-11-20): a non-zero
// exit of a pod that is not evicted is of class cause. Only the step that delivers such a termination, before the
// pod is evicted, maps that class back (collectNotEvicted); every observation made once the pod is evicted is
// compared as it is.

// evictionsGolden is the file of observations the sequences gave on main before #432.
const evictionsGolden = "testdata/evictions-before-432.golden"

// renderObservation is one line with every field of o.
func renderObservation(o Observation) string {
	return fmt.Sprintf("reason=%s origin=%s class=%q container=%q fingerprint=%s cause=%q subject=%s/%s/%s "+
		"source=%s type=%s reporter=%s occurrence=%s total=%d count=%d began=%s observed=%s mayRepeat=%t "+
		"foldOnly=%t immediate=%t symptom=%q",
		o.Reason, o.Origin, o.Class, o.Container, o.Fingerprint, o.CauseFingerprint, o.Subject.Kind,
		o.Subject.Name, o.Subject.UID, o.Source, o.Type, o.ReportingController, o.Occurrence, o.Total, o.Count,
		o.Began.UTC().Format(time.RFC3339), o.ObservedAt.UTC().Format(time.RFC3339), o.MayRepeat, o.FoldOnly,
		o.Immediate, o.Symptom)
}

// Messages of the kubelet's evictions: a limit eviction and two node-pressure evictions that differ only in
// their quantities.
const (
	// reasonEvicted is the pod's status.reason and the kubelet's event reason of an eviction.
	reasonEvicted     = "Evicted"
	evictLimitMessage = "Pod ephemeral local storage usage exceeds the total limit of containers 1Gi."
	evictPressureA    = "The node was low on resource: ephemeral-storage. Threshold quantity: 1Gi, available: " +
		"91236Ki. Container app was using 2Gi, request is 0, has larger consumption of ephemeral-storage. "
	evictPressureB = "The node was low on resource: ephemeral-storage. Threshold quantity: 1Gi, available: " +
		"52100Ki. Container app was using 3Gi, request is 0, has larger consumption of ephemeral-storage. "
)

// evictionSeq drives one sequence on a fresh harness with the web workload.
type evictionSeq struct {
	h   *harness
	t   *testing.T
	got []string
}

func (s *evictionSeq) collect(step string) {
	for _, o := range s.h.drain() {
		s.got = append(s.got, step+": "+renderObservation(o))
	}
}

// collectNotEvicted is collect for a step whose pod is not evicted: a non-zero exit read from its status is a
// cause since #432 and had no class before, so that class is mapped back. Nothing else is.
func (s *evictionSeq) collectNotEvicted(step string) {
	for _, o := range s.h.drain() {
		if o.Origin == fingerprint.SourcePodStatus && o.Reason != ReasonOOMKilled &&
			strings.HasPrefix(o.Occurrence, "termination:") && o.Class == ClassCause {
			o.Class = ClassNone
		}
		s.got = append(s.got, step+": "+renderObservation(o))
	}
}

// evConsequence is the kubelet's BackOff or Unhealthy event name about the app container of pod with uid, with
// its own time at.
func evConsequence(name, reason, pod string, uid types.UID, at time.Time) *eventsv1.Event {
	ev := consequence(name, reason, appPath, at)
	ev.CreationTimestamp = metav1.NewTime(at)
	ev.Regarding.Name, ev.Regarding.UID = pod, uid
	return ev
}

// consequencesAround creates and reconciles a BackOff and an Unhealthy event of pod A's app container at each
// offset from at, then collects them under step.
func (s *evictionSeq) consequencesAround(step string, at time.Time, offsets ...time.Duration) {
	for _, off := range offsets {
		for _, reason := range []string{reasonBack, ReasonUnhealthy} {
			name := fmt.Sprintf("ev-%s-%s", strings.ToLower(reason), off)
			s.h.create(evConsequence(name, reason, evPodA, evUIDA, at.Add(off)))
			s.h.reconcileEvent(name)
		}
	}
	s.collect(step)
}

// runningPod is the web pod named name with uid, running its init, app and sidecar containers.
func runningPod(name string, uid types.UID) *corev1.Pod {
	p := specPod()
	p.Name, p.UID = name, uid
	p.Status.Phase = corev1.PodRunning
	return p
}

// evictStatus sets the status of an evicted pod with message msg: phase Failed, reason Evicted, the init
// container completed, the app container killed with exit code 137 (reason appReason, finished at at) and the
// sidecar ContainerStatusUnknown with no finish time when unknownSidecar is set.
func evictStatus(p *corev1.Pod, msg, appReason string, at time.Time, unknownSidecar bool) {
	p.Status.Phase, p.Status.Reason, p.Status.Message = corev1.PodFailed, reasonEvicted, msg
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: containerInit,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ContainerID: "containerd://init-" + string(p.UID), Reason: "Completed", ExitCode: 0,
			StartedAt: metav1.NewTime(at.Add(-10 * time.Minute)), FinishedAt: metav1.NewTime(at.Add(-9 * time.Minute)),
		}},
	}}
	app := corev1.ContainerStatus{Name: containerApp, RestartCount: 0,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ContainerID: "containerd://app-" + string(p.UID), Reason: appReason, ExitCode: exitOOM,
			StartedAt: metav1.NewTime(at.Add(-5 * time.Minute)), FinishedAt: metav1.NewTime(at),
		}}}
	sidecar := corev1.ContainerStatus{Name: containerSidecar,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at.Add(-5 * time.Minute))}}}
	if unknownSidecar {
		sidecar.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "ContainerStatusUnknown", ExitCode: exitOOM,
			Message: "The container could not be located when the pod was terminated",
		}}
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{app, sidecar}
}

// evictedWarning is the kubelet's Evicted warning name about pod with uid, at at, with note.
func evictedWarning(name, pod string, uid types.UID, note string, at time.Time) *eventsv1.Event {
	ev := warning(name, 0)
	ev.Reason, ev.Action, ev.Note = reasonEvicted, "", note
	ev.CreationTimestamp = metav1.NewTime(at)
	ev.EventTime = metav1.NewMicroTime(at)
	ev.Regarding = corev1.ObjectReference{APIVersion: apiCore, Kind: kindPod, Namespace: testNS, Name: pod, UID: uid}
	return ev
}

const (
	evPodA = "web-7d4b9c8f6-aaaaa"
	evPodB = "web-7d4b9c8f6-bbbbb"
	evUIDA = types.UID("aaaaaaaa-0000-4000-8000-000000000001")
	evUIDB = types.UID("bbbbbbbb-0000-4000-8000-000000000002")
)

// evictionSequences are the sequences of the comparison, by name.
var evictionSequences = []struct {
	name string
	run  func(s *evictionSeq)
}{
	{"status then event", func(s *evictionSeq) {
		s.h.create(runningPod(evPodA, evUIDA))
		s.h.reconcilePod(evPodA)
		s.collect("running")
		at := t0.Add(time.Minute)
		s.h.clock.set(at)
		p := readPod(s.t, s.h, evPodA)
		evictStatus(p, evictLimitMessage, reasonError, at, true)
		s.h.update(p)
		s.h.reconcilePod(evPodA)
		s.collect("evicted status")
		s.h.clock.set(at.Add(5 * time.Second))
		s.h.create(evictedWarning("ev-a", evPodA, evUIDA, evictLimitMessage, at))
		s.h.reconcileEvent("ev-a")
		s.collect("event")
		s.h.clock.set(at.Add(2 * time.Minute))
		s.h.reconcilePod(evPodA)
		s.h.reconcileEvent("ev-a")
		s.collect("again")
	}},
	{"event then status", func(s *evictionSeq) {
		s.h.create(runningPod(evPodA, evUIDA))
		at := t0.Add(time.Minute)
		s.h.clock.set(at)
		s.h.create(evictedWarning("ev-a", evPodA, evUIDA, evictLimitMessage, at))
		s.h.reconcileEvent("ev-a")
		s.collect("event")
		s.h.clock.set(at.Add(5 * time.Second))
		p := readPod(s.t, s.h, evPodA)
		evictStatus(p, evictLimitMessage, reasonError, at, false)
		s.h.update(p)
		s.h.reconcilePod(evPodA)
		s.collect("evicted status")
		s.h.clock.set(at.Add(2 * time.Minute))
		s.h.reconcileEvent("ev-a")
		s.h.reconcilePod(evPodA)
		s.collect("again")
	}},
	{"termination then eviction", func(s *evictionSeq) {
		at := t0.Add(time.Minute)
		p := runningPod(evPodA, evUIDA)
		evictStatus(p, "", reasonError, at, false)
		p.Status.Phase, p.Status.Reason = corev1.PodRunning, ""
		s.h.create(p)
		s.h.clock.set(at)
		s.h.reconcilePod(evPodA)
		s.collectNotEvicted("killed container")
		s.h.clock.set(at.Add(3 * time.Second))
		p = readPod(s.t, s.h, evPodA)
		p.Status.Phase, p.Status.Reason, p.Status.Message = corev1.PodFailed, reasonEvicted, evictLimitMessage
		s.h.update(p)
		s.h.reconcilePod(evPodA)
		s.collect("evicted status")
		s.h.create(evictedWarning("ev-a", evPodA, evUIDA, evictLimitMessage, at))
		s.h.reconcileEvent("ev-a")
		s.collect("event")
	}},
	{"node pressure, two pods", func(s *evictionSeq) {
		for i, pod := range []struct {
			name string
			uid  types.UID
			msg  string
		}{{evPodA, evUIDA, evictPressureA}, {evPodB, evUIDB, evictPressureB}} {
			at := t0.Add(time.Duration(i+1) * 3 * time.Minute)
			s.h.clock.set(at)
			p := runningPod(pod.name, pod.uid)
			evictStatus(p, pod.msg, reasonError, at, false)
			s.h.create(p)
			s.h.reconcilePod(pod.name)
			s.collect("evicted " + pod.name)
			name := "ev-" + pod.name
			s.h.create(evictedWarning(name, pod.name, pod.uid, pod.msg, at))
			s.h.reconcileEvent(name)
			s.collect("event " + pod.name)
		}
	}},
	{"pod deleted after its eviction", func(s *evictionSeq) {
		at := t0.Add(time.Minute)
		s.h.clock.set(at)
		p := runningPod(evPodA, evUIDA)
		evictStatus(p, evictLimitMessage, "ContainerStatusUnknown", at, true)
		s.h.create(p)
		s.h.reconcilePod(evPodA)
		s.collect("evicted status")
		if err := s.h.cl.Delete(s.t.Context(), readPod(s.t, s.h, evPodA)); err != nil {
			s.t.Fatal(err)
		}
		s.h.reconcilePod(evPodA)
		s.collect("deleted")
		s.h.create(evictedWarning("ev-a", evPodA, evUIDA, evictLimitMessage, at))
		s.h.reconcileEvent("ev-a")
		s.collect("event")
	}},
	{"deleted pod at start", func(s *evictionSeq) {
		ev := evictedWarning("ev-a", evPodA, evUIDA, evictLimitMessage, t0.Add(-20*time.Minute))
		s.h.create(ev)
		stored := &eventsv1.Event{}
		if err := s.h.cl.Get(s.t.Context(), types.NamespacedName{Namespace: testNS, Name: "ev-a"}, stored); err != nil {
			s.t.Fatal(err)
		}
		listAtStart(s.h, stored)
		s.h.reconcileEvent("ev-a")
		s.collect("listed at start")
	}},
	{"recent deleted pod at start", func(s *evictionSeq) {
		ev := evictedWarning("ev-a", evPodA, evUIDA, evictPressureA, t0.Add(-time.Minute))
		s.h.create(ev)
		stored := &eventsv1.Event{}
		if err := s.h.cl.Get(s.t.Context(), types.NamespacedName{Namespace: testNS, Name: "ev-a"}, stored); err != nil {
			s.t.Fatal(err)
		}
		listAtStart(s.h, stored)
		s.h.reconcileEvent("ev-a")
		s.collect("listed at start")
	}},
	// The kubelet's BackOff and liveness failures of the evicted container, with their own times up to 10
	// minutes (TerminationCauseWindow) before and after the eviction's exit 137, reconciled once the pod shows the
	// eviction: they link to nothing, as before.
	{"consequences around the eviction", func(s *evictionSeq) {
		s.h.create(runningPod(evPodA, evUIDA))
		at := t0.Add(15 * time.Minute)
		s.h.clock.set(at)
		p := readPod(s.t, s.h, evPodA)
		evictStatus(p, evictLimitMessage, reasonError, at, false)
		s.h.update(p)
		s.h.reconcilePod(evPodA)
		s.collect("evicted status")
		s.h.clock.set(at.Add(10 * time.Minute))
		s.consequencesAround("consequences", at, -10*time.Minute, -3*time.Minute, 0, 3*time.Minute,
			10*time.Minute)
	}},
	// The same events reconciled while the pod still runs, then the eviction: nothing links either.
	{"consequences then eviction", func(s *evictionSeq) {
		s.h.create(runningPod(evPodA, evUIDA))
		at := t0.Add(15 * time.Minute)
		s.h.clock.set(at.Add(-time.Minute))
		s.consequencesAround("consequences, running", at, -8*time.Minute, -time.Minute)
		s.h.clock.set(at)
		p := readPod(s.t, s.h, evPodA)
		evictStatus(p, evictLimitMessage, reasonError, at, false)
		s.h.update(p)
		s.h.reconcilePod(evPodA)
		s.collect("evicted status")
		s.h.clock.set(at.Add(5 * time.Minute))
		s.consequencesAround("consequences, evicted", at, 2*time.Minute, 4*time.Minute)
	}},
	// A container that exited 137 while the pod ran, then the pod is evicted: a BackOff of that container
	// reconciled after the eviction links to nothing, as before.
	{"crash then eviction then consequences", func(s *evictionSeq) {
		at := t0.Add(15 * time.Minute)
		p := runningPod(evPodA, evUIDA)
		evictStatus(p, "", reasonError, at, false)
		p.Status.Phase, p.Status.Reason = corev1.PodRunning, ""
		s.h.create(p)
		s.h.clock.set(at)
		s.h.reconcilePod(evPodA)
		s.collectNotEvicted("killed container")
		s.h.clock.set(at.Add(time.Minute))
		p = readPod(s.t, s.h, evPodA)
		p.Status.Phase, p.Status.Reason, p.Status.Message = corev1.PodFailed, reasonEvicted, evictLimitMessage
		s.h.update(p)
		s.h.reconcilePod(evPodA)
		s.collect("evicted status")
		s.h.clock.set(at.Add(3 * time.Minute))
		s.consequencesAround("consequences", at, -2*time.Minute, 2*time.Minute)
	}},
}

// TestEvictionsAsBeforeTheCrashLoopLink is the comparison above: the same sequences give the same observations,
// field by field, as on main before #432.
func TestEvictionsAsBeforeTheCrashLoopLink(t *testing.T) {
	var got []string
	for _, seq := range evictionSequences {
		d, rs, _ := workload()
		s := &evictionSeq{h: newHarness(t, Config{}, d, rs), t: t}
		s.h.c.holdOff = true
		seq.run(s)
		got = append(got, "# "+seq.name)
		got = append(got, s.got...)
	}
	text := strings.Join(got, "\n") + "\n"
	if os.Getenv("WRITE_EVICTIONS_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(evictionsGolden), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(evictionsGolden, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(evictionsGolden)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := strings.Split(strings.TrimSuffix(string(want), "\n"), "\n")
	if len(wantLines) != len(got) {
		t.Errorf("got %d lines, want %d as before #432:\n%s", len(got), len(wantLines), text)
	}
	for i := range min(len(got), len(wantLines)) {
		if got[i] != wantLines[i] {
			t.Errorf("line %d differs from before #432:\n got: %s\nwant: %s", i+1, got[i], wantLines[i])
		}
	}
}
