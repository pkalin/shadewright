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

package incidentcontext

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errorReason is the reason the kubelet records for a non-zero exit.
const errorReason = "Error"

// completedReason is the reason the kubelet records for a container that ran to completion.
const completedReason = "Completed"

const shutdownNote = "exited after the deletion began"

// deletedPod is the pod of the fixture's ReplicaSet that these tests delete.
const deletedPod = "web-abc-3"

// deletionGrace is the grace period of the deletions of these tests.
const deletionGrace = 30

// deleting marks p as being deleted, with its container stopped by the deletion: reason and exit code as the
// kubelet records them (137 after the grace period, 143 on SIGTERM, 137 with OOMKilled for an OOM kill). The
// deletion began 20 seconds before the container finished, at recent, and its grace period ends 10 seconds
// after.
func deleting(p *corev1.Pod, reason string, exit int32) *corev1.Pod {
	p.DeletionTimestamp = ptrTime(recent.Add(10 * time.Second))
	p.DeletionGracePeriodSeconds = new(int64(deletionGrace))
	// The fake client refuses an object with a deletionTimestamp and no finalizer.
	p.Finalizers = []string{"example.com/hold"}
	p.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name: appName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: reason, ExitCode: exit, StartedAt: longAgo, FinishedAt: recent,
		}},
	}
	return p
}

// rolloutObjects is the web fixture plus a third pod of the same ReplicaSet that is being deleted.
func rolloutObjects(last *corev1.Pod) []client.Object {
	return append(webObjects(), last)
}

// webLabels are the labels of the pods of the fixture's ReplicaSet web-abc.
func webLabels() map[string]string {
	return replicaSet("web-abc", "uid-rs-abc", deployment(), longAgo).Labels
}

func statusOrder(t *testing.T, c *Context) []string {
	t.Helper()
	var out []string
	for line := range strings.SplitSeq(one(t, c, SectionStatus).Text, "\n") {
		if name, ok := strings.CutPrefix(line, "pod "); ok {
			out = append(out, strings.SplitN(name, ",", 2)[0])
		}
	}
	return out
}

// A pod being deleted in a rollout is shown as being deleted with the end of its grace period and no computed
// request time, its exit after the deletion began carries that fact, and it is ranked below a running pod,
// not beside the crashing one (#969). Its logs are not picked as trouble either.
func TestDeletedPodShutdownIsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		reason string
		exit   int32
	}{{errorReason, 137}, {errorReason, 143}, {completedReason, 143}} {
		t.Run(fmt.Sprintf("%s-%d", tc.reason, tc.exit), func(t *testing.T) {
			ref := controllerRef(kindReplicaSet, "web-abc", "uid-rs-abc")
			gone := deleting(pod(deletedPod, webLabels(), longAgo, ref),
				tc.reason, tc.exit)
			c := build(t, newBuilder(t, reader(t, rolloutObjects(gone)...), webSources(), nil), incident(deploymentRef()))
			text := one(t, c, SectionStatus).Text
			if got, want := statusOrder(t, c), []string{crashPod, secondPod, deletedPod}; !slices.Equal(got, want) {
				t.Errorf("pod order %v, want %v\n%s", got, want, text)
			}
			if !strings.Contains(text, "pod "+deletedPod+", controlled by ReplicaSet web-abc, phase Running, created "+
				stamp(longAgo)+", being deleted, grace period ends "+stamp(*gone.DeletionTimestamp)+"\n") ||
				strings.Contains(text, "requested") {
				t.Errorf("the pod is not shown as being deleted:\n%s", text)
			}
			if strings.Count(text, shutdownNote) != 1 {
				t.Errorf("the shutdown exit is not marked once:\n%s", text)
			}
			if tr := troubleOf(gone); tr != (trouble{}) {
				t.Errorf("trouble of the deleted pod %+v, want none", tr)
			}
			tg := onePodTarget(gone)
			if picks := logPicks(tg, windowOf(incident(deploymentRef()), testNow)); len(picks) != 1 || picks[0].score != 0 {
				t.Errorf("log picks %+v, want only the fallback pick, of score 0", picks)
			}
		})
	}
}

// An OOM kill of a pod being deleted, a container of it that terminated with an error before the deletion
// (its last termination) and a running pod's exits stay trouble, and none is marked as a shutdown.
func TestDeletedPodKeepsOOMAndEarlierCrashes(t *testing.T) {
	ref := controllerRef(kindReplicaSet, "web-abc", "uid-rs-abc")
	labels := webLabels()

	oom := deleting(pod(deletedPod, labels, longAgo, ref), reasonOOMKilled, 137)
	if tr := troubleOf(oom); tr.broken != 1 || tr.unready != 1 {
		t.Errorf("trouble of an OOM kill of a pod being deleted %+v, want broken and unready", tr)
	}

	earlier := deleting(pod("web-abc-4", labels, longAgo, ref), errorReason, 143)
	earlier.Status.ContainerStatuses[0].RestartCount = 2
	earlier.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: errorReason, ExitCode: 1, StartedAt: longAgo, FinishedAt: recent},
	}
	if tr := troubleOf(earlier); tr.broken != 1 || tr.restarts != 2 || tr.unready != 0 {
		t.Errorf("trouble of a pod being deleted after a crash %+v, want broken 1, restarts 2, unready 0", tr)
	}
	// The instance that crashed is read first, not the shutdown that followed it.
	if picks := logPicks(onePodTarget(earlier), windowOf(incident(deploymentRef()), testNow)); len(picks) != 1 ||
		!picks[0].previous || !picks[0].other {
		t.Errorf("log picks %+v, want the previous instance first", picks)
	}

	alive := deleting(pod("web-abc-5", labels, longAgo, ref), errorReason, 137)
	alive.DeletionTimestamp, alive.Finalizers = nil, nil
	if tr := troubleOf(alive); tr.broken != 1 {
		t.Errorf("trouble of a pod not being deleted %+v, want broken", tr)
	}

	c := build(t, newBuilder(t, reader(t, rolloutObjects(oom)...), webSources(), nil), incident(deploymentRef()))
	if text := one(t, c, SectionStatus).Text; strings.Contains(text, shutdownNote) {
		t.Errorf("an OOM kill is marked as a shutdown:\n%s", text)
	}
}

// An exit that finished before the deletion began (deletionTimestamp minus the grace period) is a failure like
// any other: it ranks the pod, its logs are read, and it is not marked. A missing finish time is not excused,
// and a missing grace period counts as zero: the deletion began at the deletionTimestamp.
func TestDeletedPodEarlierExitStaysTrouble(t *testing.T) {
	ref := controllerRef(kindReplicaSet, "web-abc", "uid-rs-abc")
	w := windowOf(incident(deploymentRef()), testNow)
	for name, change := range map[string]func(p *corev1.Pod){
		"an hour before": func(p *corev1.Pod) { p.DeletionTimestamp = ptrTime(recent.Add(time.Hour)) },
		"a second before": func(p *corev1.Pod) {
			p.DeletionTimestamp = ptrTime(recent.Add((deletionGrace + 1) * time.Second))
		},
		"no finish time":  func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{} },
		"no grace period": func(p *corev1.Pod) { p.DeletionGracePeriodSeconds = nil },
	} {
		t.Run(name, func(t *testing.T) {
			p := deleting(pod(deletedPod, webLabels(), longAgo, ref), errorReason, 1)
			change(p)
			if tr := troubleOf(p); tr.broken != 1 || tr.unready != 1 {
				t.Errorf("trouble %+v, want broken and unready", tr)
			}
			if picks := logPicks(onePodTarget(p), w); len(picks) != 1 || picks[0].score != 2 {
				t.Errorf("log picks %+v, want the exit picked as trouble", picks)
			}
			c := build(t, newBuilder(t, reader(t, rolloutObjects(p)...), webSources(), nil), incident(deploymentRef()))
			text := one(t, c, SectionStatus).Text
			if strings.Contains(text, shutdownNote) {
				t.Errorf("an exit before the deletion is marked as its shutdown:\n%s", text)
			}
			if got := statusOrder(t, c); !slices.Equal(got, []string{crashPod, deletedPod, secondPod}) {
				t.Errorf("pod order %v, want the deleted pod ranked as troubled\n%s", got, text)
			}
		})
	}
	// With no grace period recorded, an exit at the deletionTimestamp itself is the shutdown.
	p := deleting(pod(deletedPod, webLabels(), longAgo, ref), errorReason, 143)
	p.DeletionGracePeriodSeconds = nil
	p.DeletionTimestamp = ptrTime(recent.Time)
	if tr := troubleOf(p); tr != (trouble{}) {
		t.Errorf("trouble of an exit at the deletionTimestamp with no grace period %+v, want none", tr)
	}
}

// A container that ran to completion (an init container that exited 0) is not marked as the shutdown, even
// when it finished after the deletion began.
func TestDeletedPodCompletedContainerIsNotMarked(t *testing.T) {
	ref := controllerRef(kindReplicaSet, "web-abc", "uid-rs-abc")
	p := deleting(pod(deletedPod, webLabels(), longAgo, ref), errorReason, 143)
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "setup",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: completedReason, ExitCode: 0, StartedAt: longAgo, FinishedAt: recent,
		}},
	}}
	c := build(t, newBuilder(t, reader(t, rolloutObjects(p)...), webSources(), nil), incident(deploymentRef()))
	text := one(t, c, SectionStatus).Text
	if n := strings.Count(text, shutdownNote); n != 1 {
		t.Errorf("%d shutdown notes, want only the app container's:\n%s", n, text)
	}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "setup") && strings.Contains(line, shutdownNote) {
			t.Errorf("the completed init container is marked: %s", line)
		}
	}
}

// A pod past its grace period, held by a finalizer, whose containers still run: it is shown as being deleted,
// nothing is marked as a shutdown, and it is no trouble.
func TestDeletedPodPastGraceStillRunning(t *testing.T) {
	ref := controllerRef(kindReplicaSet, "web-abc", "uid-rs-abc")
	p := pod(deletedPod, webLabels(), longAgo, ref)
	p.DeletionTimestamp, p.DeletionGracePeriodSeconds = ptrTime(recent.Time), new(int64(deletionGrace))
	p.Finalizers = []string{"example.com/hold"}
	if tr := troubleOf(p); tr != (trouble{}) {
		t.Errorf("trouble %+v, want none", tr)
	}
	c := build(t, newBuilder(t, reader(t, rolloutObjects(p)...), webSources(), nil), incident(deploymentRef()))
	text := one(t, c, SectionStatus).Text
	if strings.Contains(text, shutdownNote) || !strings.Contains(text, ", being deleted, grace period ends ") {
		t.Errorf("a running pod being deleted:\n%s", text)
	}
}

// A named limit (ADR-0023, amendment of 2026-11-22): a pod kept by a finalizer after a delete shortened its
// grace period to 0 (the kubelet's final delete) has its deletionTimestamp moved past the exit it already had.
// That exit finished before the start of the last grace period, so it is not marked and ranks the pod as trouble,
// as before #969.
func TestDeletedPodZeroGraceAfterExitRanksAsTrouble(t *testing.T) {
	ref := controllerRef(kindReplicaSet, "web-abc", "uid-rs-abc")
	p := deleting(pod(deletedPod, webLabels(), longAgo, ref), errorReason, 137)
	p.DeletionGracePeriodSeconds = new(int64(0))
	p.DeletionTimestamp = ptrTime(recent.Add(5 * time.Second))
	if tr := troubleOf(p); tr.broken != 1 || tr.unready != 1 {
		t.Errorf("trouble %+v, want broken and unready", tr)
	}
	c := build(t, newBuilder(t, reader(t, rolloutObjects(p)...), webSources(), nil), incident(deploymentRef()))
	text := one(t, c, SectionStatus).Text
	if strings.Contains(text, shutdownNote) {
		t.Errorf("an exit before the moved deletionTimestamp is marked:\n%s", text)
	}
	if got := statusOrder(t, c); !slices.Equal(got, []string{crashPod, deletedPod, secondPod}) {
		t.Errorf("pod order %v, want the deleted pod ranked as troubled\n%s", got, text)
	}
}

// The other named limit: a real crash during the grace period (a segfault while flushing, exit 139 three seconds
// after the start of the grace period) reads like a shutdown. It carries the fact and does not rank the pod.
func TestDeletedPodCrashInsideGraceIsMarkedNotRanked(t *testing.T) {
	ref := controllerRef(kindReplicaSet, "web-abc", "uid-rs-abc")
	p := deleting(pod(deletedPod, webLabels(), longAgo, ref), errorReason, 139)
	p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(deletionStart(p).Add(3 * time.Second))
	if tr := troubleOf(p); tr != (trouble{}) {
		t.Errorf("trouble %+v, want none", tr)
	}
	c := build(t, newBuilder(t, reader(t, rolloutObjects(p)...), webSources(), nil), incident(deploymentRef()))
	text := one(t, c, SectionStatus).Text
	if strings.Count(text, shutdownNote) != 1 {
		t.Errorf("the exit inside the grace period is not marked once:\n%s", text)
	}
	if got := statusOrder(t, c); !slices.Equal(got, []string{crashPod, secondPod, deletedPod}) {
		t.Errorf("pod order %v, want the deleted pod ranked last\n%s", got, text)
	}
}
