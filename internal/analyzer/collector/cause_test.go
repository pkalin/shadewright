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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

const (
	containerInit    = "init-db"
	containerSidecar = "proxy"
	reasonImagePull  = "ImagePullBackOff"
	exitOOM          = 137
)

// specPod returns the web pod with the app and proxy containers and the init-db init container in its spec.
func specPod() *corev1.Pod {
	_, _, p := workload()
	p.Spec.InitContainers = []corev1.Container{{Name: containerInit}}
	p.Spec.Containers = []corev1.Container{{Name: containerApp}, {Name: containerSidecar}}
	return p
}

// causeHarness starts a harness with the web workload and pod, after setPod changed the pod.
func causeHarness(t *testing.T, setPod func(*corev1.Pod)) (*harness, *corev1.Pod) {
	t.Helper()
	d, rs, _ := workload()
	pod := specPod()
	if setPod != nil {
		setPod(pod)
	}
	h := newHarness(t, Config{}, d, rs, pod)
	// These tests read the fields of the observation; the hold of a consequence with no cause is tested in
	// hold_test.go.
	h.c.holdOff = true
	return h, pod
}

// consequence returns a kubelet event about the web pod's container named by fieldPath, with the reason
// BackOff or Unhealthy, and its own time at.
func consequence(name, reason, fieldPath string, at time.Time) *eventsv1.Event {
	ev := warning(name, 0)
	ev.Reason, ev.EventTime = reason, metav1.NewMicroTime(at)
	if reason == ReasonUnhealthy {
		// A liveness failure: the readiness and startup rules of #726 do not apply to it.
		ev.Action, ev.Note = "Killing", "Liveness probe failed: HTTP probe failed with statuscode: 500"
	}
	ev.Regarding.FieldPath = fieldPath
	return ev
}

// TestNamedContainerCorpus runs generated field paths, each with its answer known by construction, through
// the event collector: only spec.containers{<name>} or spec.initContainers{<name>} with a DNS label name in that
// list of the verified pod's spec names a container, and only then can the event be a consequence.
func TestNamedContainerCorpus(t *testing.T) {
	type tc struct {
		path string
		want string
	}
	var cases []tc
	valid := map[string]string{
		"spec.containers{" + containerApp + "}":          containerApp,
		"spec.containers{" + containerSidecar + "}":      containerSidecar,
		"spec.initContainers{" + containerInit + "}":     containerInit,
		"spec.containers{" + containerInit + "}":         "", // an init container's name used for a regular one
		"spec.initContainers{" + containerApp + "}":      "", // and the other way round
		"spec.containers{ghost}":                         "", // not in the pod
		"spec.ephemeralContainers{" + containerApp + "}": "",
		"": "",
	}
	for p, w := range valid {
		cases = append(cases, tc{p, w})
	}
	// Hostile forms of every valid path: each mutation leaves the container empty by construction.
	mutations := []func(string) string{
		func(p string) string { return "x" + p },                                        // wrong prefix
		func(p string) string { return strings.Replace(p, "spec.", "status.", 1) },      // wrong prefix
		func(p string) string { return strings.Replace(p, "spec.", "Spec.", 1) },        // wrong prefix
		func(p string) string { return strings.Replace(p, "ontainers", "ontainer", 1) }, // wrong list
		func(p string) string { return " " + p },
		func(p string) string { return p + " " },
		func(p string) string { return p + "\n" },
		func(p string) string { return p + "x" },
		func(p string) string { return strings.Replace(p, "{", "{{", 1) + "}" }, // nested braces
		func(p string) string { return strings.Replace(p, "{", "{a{", 1) },      // nested braces
		func(p string) string { return strings.Replace(p, "}", "}}", 1) },       // a brace too many
		func(p string) string { return strings.Replace(p, "{", "", 1) },         // brace missing
		func(p string) string { return strings.TrimSuffix(p, "}") },             // brace missing
		func(p string) string { return strings.NewReplacer("{", "[", "}", "]").Replace(p) },
		strings.ToUpper, // not a DNS label
		func(p string) string { return strings.Replace(p, "{", "{-", 1) },                        // not a DNS label
		func(p string) string { return strings.Replace(p, "}", "-}", 1) },                        // not a DNS label
		func(p string) string { return strings.Replace(p, "{", "{a_", 1) },                       // not a DNS label
		func(p string) string { return strings.Replace(p, "{", "{a.", 1) },                       // not a DNS label
		func(p string) string { return strings.Replace(p, "{", "{а", 1) },                        // a Cyrillic a
		func(p string) string { return strings.Replace(p, "{", "{"+strings.Repeat("a", 60), 1) }, // over 63
		func(p string) string { return p + strings.Repeat("}", 10000) },                          // very long
		func(p string) string { return strings.Repeat(p, 1000) },                                 // very long
		func(p string) string { return strings.Replace(p, "{", "{\x00", 1) },
		func(p string) string { return strings.Replace(p, "}", "%7D", 1) },
	}
	for _, p := range []string{
		"spec.containers{" + containerApp + "}", "spec.initContainers{" + containerInit + "}",
	} {
		for i, m := range mutations {
			mp := m(p)
			if mp == p {
				t.Fatalf("mutation %d leaves %q unchanged", i, p)
			}
			cases = append(cases, tc{mp, ""})
		}
	}
	for i, c := range cases {
		h, _ := causeHarness(t, nil)
		ev := consequence(fmt.Sprintf("path-%d", i), reasonBack, c.path, t0)
		h.create(ev)
		h.reconcileEvent(ev.Name)
		got := h.one()
		wantClass := ClassNone
		if c.want != "" {
			wantClass = ClassConsequence
		}
		if got.Container != c.want || got.Class != wantClass || got.Origin != fingerprint.SourceEvent {
			t.Errorf("field path %.80q: container %q, class %q, origin %q; want %q, %q, event", c.path,
				got.Container, got.Class, got.Origin, c.want, wantClass)
		}
	}
}

// TestNamedContainerNeedsTheVerifiedPod checks that a valid field path names nothing when the regarding
// object is not the Pod whose UID the collector verified.
func TestNamedContainerNeedsTheVerifiedPod(t *testing.T) {
	pod := specPod()
	path := "spec.containers{" + containerApp + "}"
	ok := corev1.ObjectReference{APIVersion: apiCore, Kind: kindPod, Namespace: testNS, Name: webPod, UID: podUID,
		FieldPath: path}
	if got, _ := namedContainer(ok, pod); got != containerApp {
		t.Fatalf("verified pod: %q, want %q", got, containerApp)
	}
	for name, mutate := range map[string]func(*corev1.ObjectReference, **corev1.Pod){
		"another UID":     func(r *corev1.ObjectReference, _ **corev1.Pod) { r.UID = "forged-uid" },
		"no UID":          func(r *corev1.ObjectReference, _ **corev1.Pod) { r.UID = "" },
		"a ReplicaSet":    func(r *corev1.ObjectReference, _ **corev1.Pod) { r.Kind = kindReplicaSet },
		"another version": func(r *corev1.ObjectReference, _ **corev1.Pod) { r.APIVersion = "v2" },
		"no pod":          func(_ *corev1.ObjectReference, p **corev1.Pod) { *p = nil },
	} {
		r, p := ok, pod
		mutate(&r, &p)
		if got, _ := namedContainer(r, p); got != "" {
			t.Errorf("%s: container %q, want none", name, got)
		}
	}

	// A name the spec holds that is not a DNS label (the API server refuses it, the cache is not trusted).
	odd := specPod()
	odd.Spec.Containers = append(odd.Spec.Containers, corev1.Container{Name: "App_1"})
	r := ok
	r.FieldPath = "spec.containers{App_1}"
	if got, _ := namedContainer(r, odd); got != "" {
		t.Errorf("a name that is not a DNS label: container %q, want none", got)
	}

	// Through the collector: an event about a pod of the same name and another UID (a forged or stale reference)
	// names no container.
	h, _ := causeHarness(t, nil)
	ev := consequence("other-uid", reasonBack, path, t0)
	ev.Regarding.UID = types.UID("1a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d")
	h.create(ev)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.Container != "" || got.Class != ClassNone {
		t.Errorf("event about another UID: container %q, class %q", got.Container, got.Class)
	}
}

// TestEventImitatingATerminationIsNoCause checks that an Event with the reason, controller and note of a
// pod-status observation has the origin event, another fingerprint and never the class cause.
func TestEventImitatingATerminationIsNoCause(t *testing.T) {
	h, pod := causeHarness(t, func(p *corev1.Pod) {
		withTermination(p, "containerd://oom", ReasonOOMKilled, exitOOM, t0)
		p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: reasonImagePull}
	})
	h.reconcilePod(pod.Name)
	real := map[string]Observation{}
	for _, o := range h.drain() {
		if o.Origin != fingerprint.SourcePodStatus || o.Class != ClassCause || o.Container != containerApp {
			t.Errorf("pod-status observation %s: origin %q, class %q, container %q", o.Reason, o.Origin, o.Class,
				o.Container)
		}
		real[o.Reason] = o
	}
	if len(real) != 2 {
		t.Fatalf("pod status gave %d observations, want the kill and the wait: %+v", len(real), real)
	}
	notes := map[string]string{
		ReasonOOMKilled: terminationNote(containerApp, exitOOM),
		reasonImagePull: waitingNote(containerApp, reasonImagePull),
	}
	i := 0
	for reason, note := range notes {
		for _, path := range []string{"", "spec.containers{" + containerApp + "}"} {
			for _, action := range []string{"", "Killing"} {
				i++
				ev := warning(fmt.Sprintf("imitation-%d", i), 0)
				ev.Reason, ev.Note, ev.Action = reason, note, action
				ev.Regarding.FieldPath = path
				h.create(ev)
				h.reconcileEvent(ev.Name)
				got := h.one()
				if got.Origin != fingerprint.SourceEvent || got.Class == ClassCause || got.CauseFingerprint != "" {
					t.Errorf("imitation of %s (path %q): origin %q, class %q, cause %q", reason, path, got.Origin,
						got.Class, got.CauseFingerprint)
				}
				if got.Fingerprint == real[reason].Fingerprint {
					t.Errorf("imitation of %s (path %q, action %q) has the pod-status fingerprint", reason, path, action)
				}
			}
		}
	}
}

// TestCauseFingerprintKillWindow checks the bound of an OOM kill: a BackOff or Unhealthy event from the kill's
// finishedAt to 10 minutes after links to the termination's fingerprint; a time before it, 10 minutes and
// 1 second after it, or no kubelet time links nothing.
func TestCauseFingerprintKillWindow(t *testing.T) {
	finished := t0.Add(-30 * time.Minute)
	type tc struct {
		name   string
		at     *time.Time
		linked bool
	}
	at := func(d time.Duration) *time.Time { v := finished.Add(d); return &v }
	cases := []tc{
		{"at finishedAt", at(0), true},
		{"one minute after", at(time.Minute), true},
		{"ten minutes after", at(KillCauseWindow), true},
		{"one second before", at(-time.Second), false},
		{"ten minutes and one second after", at(KillCauseWindow + time.Second), false},
		{"no kubelet time", nil, false},
	}
	for _, reason := range []string{reasonBack, ReasonUnhealthy} {
		for _, c := range cases {
			t.Run(reason+" "+c.name, func(t *testing.T) {
				h, pod := causeHarness(t, func(p *corev1.Pod) {
					withTermination(p, "containerd://oom", ReasonOOMKilled, exitOOM, finished)
				})
				h.reconcilePod(pod.Name)
				kill := h.one()
				ev := consequence("c", reason, "spec.containers{"+containerApp+"}", t0)
				if c.at == nil {
					// Only the creation time, which the API server stamps, is set.
					ev.EventTime = metav1.MicroTime{}
					ev.CreationTimestamp = metav1.NewTime(finished.Add(time.Minute))
				} else {
					ev.EventTime = metav1.NewMicroTime(*c.at)
				}
				h.create(ev)
				h.reconcileEvent(ev.Name)
				got := h.one()
				if got.Class != ClassConsequence {
					t.Fatalf("class %q, want consequence", got.Class)
				}
				want := ""
				if c.linked {
					want = kill.Fingerprint
				}
				if got.CauseFingerprint != want {
					t.Errorf("cause %q, want %q", got.CauseFingerprint, want)
				}
			})
		}
	}
}

// TestCauseFingerprintSeriesTime checks that the time of a series is its last observed time.
func TestCauseFingerprintSeriesTime(t *testing.T) {
	finished := t0.Add(-30 * time.Minute)
	h, pod := causeHarness(t, func(p *corev1.Pod) {
		withTermination(p, "containerd://oom", ReasonOOMKilled, exitOOM, finished)
	})
	h.reconcilePod(pod.Name)
	kill := h.one()
	ev := consequence("series", reasonBack, "spec.containers{"+containerApp+"}", finished)
	ev.Series = &eventsv1.EventSeries{Count: 3, LastObservedTime: metav1.NewMicroTime(finished.Add(KillCauseWindow + time.Minute))}
	h.create(ev)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.CauseFingerprint != "" {
		t.Errorf("series last seen 11 minutes after the kill: cause %q, want none (kill %s)", got.CauseFingerprint,
			kill.Fingerprint)
	}
}

// TestWaitingReasonWinsOverAnOlderKill checks that a current waiting reason of the list is the cause even when
// an OOM kill of the same container is within the window.
func TestWaitingReasonWinsOverAnOlderKill(t *testing.T) {
	for _, reason := range WaitingCauseReasons {
		t.Run(reason, func(t *testing.T) {
			h, pod := causeHarness(t, func(p *corev1.Pod) {
				withTermination(p, "containerd://oom", ReasonOOMKilled, exitOOM, t0.Add(-time.Minute))
				p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: reason}
			})
			h.reconcilePod(pod.Name)
			fps := map[string]string{}
			for _, o := range h.drain() {
				fps[o.Reason] = o.Fingerprint
			}
			if fps[reason] == "" || fps[ReasonOOMKilled] == "" {
				t.Fatalf("pod status gave %v, want the wait and the kill", fps)
			}
			ev := consequence("c", reasonBack, "spec.containers{"+containerApp+"}", t0)
			h.create(ev)
			h.reconcileEvent(ev.Name)
			if got := h.one(); got.CauseFingerprint != fps[reason] {
				t.Errorf("cause %q, want the wait's %q (kill %q)", got.CauseFingerprint, fps[reason], fps[ReasonOOMKilled])
			}
		})
	}
}

// TestWaitingCauseOfAnInitContainer checks that the cause is read from the list the field path names.
func TestWaitingCauseOfAnInitContainer(t *testing.T) {
	h, pod := causeHarness(t, func(p *corev1.Pod) {
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: containerInit,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError"}}}}
	})
	h.reconcilePod(pod.Name)
	wait := h.one()
	if wait.Container != containerInit || wait.Class != ClassCause {
		t.Fatalf("init wait: container %q, class %q", wait.Container, wait.Class)
	}
	for path, want := range map[string]string{
		"spec.initContainers{" + containerInit + "}": wait.Fingerprint,
		"spec.containers{" + containerApp + "}":      "",
	} {
		ev := consequence("c-"+strings.NewReplacer("{", "-", "}", "").Replace(path), reasonBack, path, t0)
		h.create(ev)
		h.reconcileEvent(ev.Name)
		if got := h.one(); got.CauseFingerprint != want {
			t.Errorf("%s: cause %q, want %q", path, got.CauseFingerprint, want)
		}
	}
}

// TestNonZeroExitIsACause checks that a crash (a non-zero exit) is a cause read from pod status, and that a
// BackOff of its container within KillCauseWindow links to it (ADR-0039 point 3, amended 2026-11-20, #432).
func TestNonZeroExitIsACause(t *testing.T) {
	h, pod := causeHarness(t, func(p *corev1.Pod) {
		withTermination(p, "containerd://crash", reasonError, 1, t0)
	})
	h.reconcilePod(pod.Name)
	crash := h.one()
	if crash.Class != ClassCause || crash.Container != containerApp || !strings.HasPrefix(crash.Occurrence, "termination:") {
		t.Errorf("crash: class %q, container %q, occurrence %q", crash.Class, crash.Container, crash.Occurrence)
	}
	ev := consequence("c", reasonBack, "spec.containers{"+containerApp+"}", t0)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	if got := h.one(); got.Class != ClassConsequence || got.CauseFingerprint != crash.Fingerprint {
		t.Errorf("BackOff after a crash: class %q, cause %q, want %q", got.Class, got.CauseFingerprint, crash.Fingerprint)
	}
}

// TestOnlyKubeletBackOffAndUnhealthyAreConsequences checks the class of events on a named container.
func TestOnlyKubeletBackOffAndUnhealthyAreConsequences(t *testing.T) {
	h, _ := causeHarness(t, nil)
	cases := []struct {
		reason, controller string
		want               Class
	}{
		{reasonBack, reportingKubelet, ClassConsequence},
		{ReasonUnhealthy, reportingKubelet, ClassConsequence},
		{reasonBack, "intruder-controller", ClassNone},
		{ReasonUnhealthy, "kubelet2", ClassNone},
		{"Failed", reportingKubelet, ClassNone},
		{ReasonOOMKilled, reportingKubelet, ClassNone},
		{reasonImagePull, reportingKubelet, ClassNone},
	}
	for i, c := range cases {
		ev := consequence(fmt.Sprintf("e-%d", i), c.reason, "spec.containers{"+containerApp+"}", t0)
		ev.ReportingController = c.controller
		h.create(ev)
		h.reconcileEvent(ev.Name)
		if got := h.one(); got.Class != c.want {
			t.Errorf("%s from %s: class %q, want %q", c.reason, c.controller, got.Class, c.want)
		}
	}
}

// TestWaitingReasons checks what the pod collector emits for waiting states: a cause observation for a reason
// of the list, not immediate, with a symptom that holds no part of the waiting message, and nothing for any
// other reason.
func TestWaitingReasons(t *testing.T) {
	secret := "password=" + "hunter2" + "-synthetic"
	for _, reason := range []string{"CrashLoopBackOff", "ContainerCreating", "PodInitializing", "imagepullbackoff",
		"ImagePullBackOff2", ""} {
		h, pod := causeHarness(t, func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerApp,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: secret}}}}
		})
		if hasPodSymptom(pod) {
			t.Errorf("waiting %q: the pod watch takes the pod", reason)
		}
		h.reconcilePod(pod.Name)
		h.none()
	}
	fps := map[string]string{}
	for _, reason := range WaitingCauseReasons {
		for _, msg := range []string{secret, "Back-off pulling image \"registry.example/app:1\"", ""} {
			h, pod := causeHarness(t, func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerApp, RestartCount: 2,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}}}
			})
			if !hasPodSymptom(pod) {
				t.Errorf("waiting %q: the pod watch drops the pod", reason)
			}
			h.reconcilePod(pod.Name)
			got := h.one()
			if got.Reason != reason || got.Class != ClassCause || got.Origin != fingerprint.SourcePodStatus ||
				got.Immediate || got.Container != containerApp || got.ReportingController != reportingKubelet ||
				!strings.HasPrefix(got.Occurrence, "waiting:") || got.Total != 1 {
				t.Errorf("waiting %q: %+v", reason, got)
			}
			if strings.Contains(fmt.Sprintf("%+v", got), "hunter2") || strings.Contains(got.Symptom, "registry") {
				t.Errorf("waiting %q: the message reached the observation: %+v", reason, got)
			}
			if fp, ok := fps[reason]; ok && fp != got.Fingerprint {
				t.Errorf("waiting %q: the message changed the fingerprint", reason)
			}
			fps[reason] = got.Fingerprint
			// The same wait seen again is handled.
			h.reconcilePod(pod.Name)
			h.none()
		}
	}
}

// TestWaitingDoesNotPushOutHandledTerminations checks that waiting states, which alternate between two
// reasons while an image cannot be pulled, never make a handled termination of the container look new.
func TestWaitingDoesNotPushOutHandledTerminations(t *testing.T) {
	h, pod := causeHarness(t, func(p *corev1.Pod) {
		withTermination(p, "containerd://oom", ReasonOOMKilled, exitOOM, t0)
	})
	h.reconcilePod(pod.Name)
	h.one()
	for i, reason := range []string{"ErrImagePull", reasonImagePull, "ErrImagePull", reasonImagePull} {
		pod.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: reason}
		h.update(pod)
		h.reconcilePod(pod.Name)
		got := h.drain()
		if i < 2 && (len(got) != 1 || got[0].Reason != reason) {
			t.Errorf("wait %d (%s): %+v, want one wait", i, reason, got)
		}
		if i >= 2 && len(got) != 0 {
			t.Errorf("wait %d (%s) seen again: %+v, want none", i, reason, got)
		}
	}
}

// TestPodStatusFingerprintHashesTheSource checks that the fingerprint of a termination is the pod-status one,
// different from the same input without the source, and that an event's is unchanged.
func TestPodStatusFingerprintHashesTheSource(t *testing.T) {
	h, pod := causeHarness(t, func(p *corev1.Pod) {
		withTermination(p, "containerd://oom", ReasonOOMKilled, exitOOM, t0)
	})
	h.reconcilePod(pod.Name)
	got := h.one()
	fo := fingerprint.Observation{
		ClusterID: testCluster, Namespace: testNS, OwnerUID: string(deployUID), ResourceUID: string(podUID),
		Reason: ReasonOOMKilled, Type: corev1.EventTypeWarning, ReportingController: reportingKubelet,
		Note: terminationNote(containerApp, exitOOM), RegardingKind: kindPod, RegardingName: webPod,
	}
	before := fingerprint.Fingerprint(fo)
	fo.Source = fingerprint.SourcePodStatus
	if got.Fingerprint != fingerprint.Fingerprint(fo) || got.Fingerprint == before {
		t.Errorf("termination fingerprint %s, want %s (and not %s)", got.Fingerprint, fingerprint.Fingerprint(fo), before)
	}
}
