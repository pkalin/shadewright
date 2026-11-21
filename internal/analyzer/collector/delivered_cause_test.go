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
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The property of the decision of 2026-11-21 on #432: every cause fingerprint on a consequence is the
// fingerprint of an observation the collector delivers for that pod. The tests below use only what main
// has, so they also run there: the deleting pod behaves the same on both.

// holdFinalizer keeps a deleted pod in the fake API server with its deletionTimestamp set.
const holdFinalizer = "test.shadewright.io/hold"

// stoppedPod is the web pod whose app container terminated with reason and exit code exit at t0, after one
// restart, with restartPolicy policy, phase phase and status reason podReason.
func stoppedPod(reason string, exit int32, policy corev1.RestartPolicy, phase corev1.PodPhase,
	podReason string) *corev1.Pod {
	p := specPod()
	p.Finalizers = []string{holdFinalizer}
	p.Spec.RestartPolicy = policy
	p.Status.Phase, p.Status.Reason = phase, podReason
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: containerApp, RestartCount: 1,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ContainerID: "containerd://stopped", Reason: reason, ExitCode: exit,
			StartedAt: metav1.NewTime(t0.Add(-time.Minute)), FinishedAt: metav1.NewTime(t0),
		}},
	}}
	return p
}

// deleteHeld deletes the stored web pod, which its finalizer keeps with a deletionTimestamp.
func deleteHeld(t *testing.T, h *harness) {
	t.Helper()
	if err := h.cl.Delete(t.Context(), readPod(t, h, webPod)); err != nil {
		t.Fatal(err)
	}
	if readPod(t, h, webPod).DeletionTimestamp == nil {
		t.Fatal("the deleted pod has no deletionTimestamp")
	}
}

// TestConsequenceLinksOnlyADeliveredTermination is the property over the kinds of pod: running in a crash loop,
// being deleted (a rollout or scale down stops its container with 143, or the kernel kills it for memory),
// evicted by its node, and with restartPolicy Never. The BackOff and the liveness failure of the container
// link only to a fingerprint the pod reconciler delivered for that pod, and link when it delivered one in the
// window and, for a non-zero exit, the Incident controller confirmed it (decided 2026-11-21): each case runs once
// with a CauseSeen that has seen every delivered cause, and once with the hold off, where nothing is asked and
// so only an OOM kill links.
func TestConsequenceLinksOnlyADeliveredTermination(t *testing.T) {
	cases := []struct {
		name     string
		pod      *corev1.Pod
		deleting bool
		linked   bool
	}{
		{"running crash loop", stoppedPod(reasonError, 2, corev1.RestartPolicyAlways, corev1.PodRunning, ""), false, true},
		{"deleting, stopped with 143", stoppedPod(reasonError, 143, corev1.RestartPolicyAlways, corev1.PodRunning, ""), true, false},
		{"deleting, OOM killed", stoppedPod(ReasonOOMKilled, exitOOM, corev1.RestartPolicyAlways, corev1.PodRunning, ""), true, true},
		{"evicted", stoppedPod(reasonError, exitOOM, corev1.RestartPolicyAlways, corev1.PodFailed, reasonEvicted), false, false},
		{"restartPolicy Never", stoppedPod(reasonError, 1, corev1.RestartPolicyNever, corev1.PodFailed, ""), false, true},
	}
	for _, c := range cases {
		for _, confirmed := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/confirmed=%t", c.name, confirmed), func(t *testing.T) {
				linkedCase(t, c.pod.DeepCopy(), c.deleting, confirmed,
					c.linked && (confirmed || c.pod.Status.ContainerStatuses[0].State.Terminated.Reason == ReasonOOMKilled))
			})
		}
	}
}

// linkedCase runs one case of TestConsequenceLinksOnlyADeliveredTermination: with confirmed, a CauseSeen that
// has seen every cause the pod reconciler delivered; without, the hold off. want is whether the consequences link.
func linkedCase(t *testing.T, pod *corev1.Pod, deleting, confirmed, want bool) {
	t.Helper()
	book := &causeBook{}
	d, rs, _ := workload()
	cfg := Config{}
	if confirmed {
		cfg.CauseSeen = book.fn
	}
	h := newHarness(t, cfg, d, rs, pod)
	h.c.holdOff = !confirmed
	if deleting {
		deleteHeld(t, h)
	}
	at := t0.Add(30 * time.Second)
	h.clock.set(at)
	h.reconcilePod(webPod)
	var delivered []string
	for _, o := range h.drain() {
		delivered = append(delivered, o.Fingerprint)
		book.record(o.Fingerprint)
	}
	for _, reason := range []string{reasonBack, ReasonUnhealthy} {
		ev := consequence("ev-"+reason, reason, appPath, at)
		h.create(ev)
		h.reconcileEvent(ev.Name)
		got := h.drain()
		if want && len(got) == 0 {
			t.Errorf("%s was not emitted at once, want it linked", reason)
		}
		for _, o := range got {
			if o.CauseFingerprint != "" && !slices.Contains(delivered, o.CauseFingerprint) {
				t.Errorf("%s links to %s, which the collector did not deliver (delivered %v)", reason,
					o.CauseFingerprint, delivered)
			}
			if linked := o.CauseFingerprint != ""; linked != want {
				t.Errorf("%s linked %t, want %t", reason, linked, want)
			}
		}
	}
}

// TestDeletingPodConsequenceAsOnMain is finding 2 of the third pass on #432: a pod being deleted whose
// container exits 143 delivers no termination (#966), so its Unhealthy and BackOff events get no cause, are
// handled as consequences with no cause, field by field as on main.
func TestDeletingPodConsequenceAsOnMain(t *testing.T) {
	d, rs, _ := workload()
	h := newHarness(t, Config{}, d, rs, stoppedPod(reasonError, 143, corev1.RestartPolicyAlways, corev1.PodRunning, ""))
	deleteHeld(t, h)
	at := t0.Add(5 * time.Second)
	h.clock.set(at)
	h.reconcilePod(webPod)
	h.none()
	names := []string{"ev-unhealthy", "ev-backoff"}
	for i, reason := range []string{ReasonUnhealthy, reasonBack} {
		ev := consequence(names[i], reason, appPath, at)
		h.create(ev)
		h.reconcileEvent(ev.Name)
	}
	got := h.drain()
	// The pod goes: its finalizer is removed.
	p := readPod(t, h, webPod)
	p.Finalizers = nil
	h.update(p)
	h.clock.set(at.Add(ConsequenceGrace + time.Second))
	for _, name := range names {
		h.reconcileEvent(name)
	}
	got = append(got, h.drain()...)
	for _, o := range got {
		if o.CauseFingerprint != "" {
			t.Errorf("%s of a deleting pod links to %s, a termination the collector never delivers", o.Reason,
				o.CauseFingerprint)
		}
	}
	rendered := make([]string, 0, len(got))
	for _, o := range got {
		rendered = append(rendered, renderObservation(o))
	}
	if !slices.Equal(rendered, deletingPodBefore432) {
		t.Errorf("observations differ from main:\n got: %q\nwant: %q", rendered, deletingPodBefore432)
	}
}

// deletingPodBefore432 is what TestDeletingPodConsequenceAsOnMain gave on main at 64500a78, before #432 (the
// test run there, its output pasted here): the liveness failure, with no cause, and nothing for the BackOff.
var deletingPodBefore432 = []string{
	`reason=Unhealthy origin=event class="consequence" container="app" ` +
		`fingerprint=sha256:ebd37f2abe82d99267585563edbc7ca5bff2035af92e0a70f4fcc89f4b327224 cause="" ` +
		`subject=Deployment/web/4f1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d10 source=shop type=Warning reporter=kubelet ` +
		`occurrence=event:ev-ev-unhealthy total=1 count=0 began=2026-11-03T12:00:00Z observed=2026-11-03T12:00:05Z ` +
		`mayRepeat=true foldOnly=false immediate=false symptom="Liveness probe failed: HTTP probe failed with statuscode: 500"`,
}
