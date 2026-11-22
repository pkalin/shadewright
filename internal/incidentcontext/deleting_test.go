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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errorReason is the reason the kubelet records for a non-zero exit.
const errorReason = "Error"

const shutdownNote = "this exit is the shutdown of a pod being deleted, not counted as a failure"

// deleting marks p as being deleted, with its container stopped by the deletion: reason and exit code as the
// kubelet records them (137 after the grace period, 143 on SIGTERM, 137 with OOMKilled for an OOM kill).
func deleting(p *corev1.Pod, reason string, exit int32) *corev1.Pod {
	p.DeletionTimestamp = ptrTime(testNow.Add(30 * time.Second))
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

// A pod being deleted in a rollout is shown as being deleted, its shutdown exit is marked as such, and it is
// ranked below a running pod, not beside the crashing one as a failure (#969). Its logs are not picked as
// trouble either.
func TestDeletedPodShutdownIsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		reason string
		exit   int32
	}{{errorReason, 137}, {errorReason, 143}, {"Completed", 143}} {
		t.Run(fmt.Sprintf("%s-%d", tc.reason, tc.exit), func(t *testing.T) {
			ref := controllerRef(kindReplicaSet, "web-abc", "uid-rs-abc")
			gone := deleting(pod("web-abc-3", webLabels(), longAgo, ref),
				tc.reason, tc.exit)
			c := build(t, newBuilder(t, reader(t, rolloutObjects(gone)...), webSources(), nil), incident(deploymentRef()))
			text := one(t, c, SectionStatus).Text
			if got, want := statusOrder(t, c), []string{crashPod, secondPod, "web-abc-3"}; !slices.Equal(got, want) {
				t.Errorf("pod order %v, want %v\n%s", got, want, text)
			}
			if !strings.Contains(text, "pod web-abc-3, controlled by ReplicaSet web-abc, phase Running, created "+
				stamp(longAgo)+", being deleted (grace period ends "+stamp(*gone.DeletionTimestamp)+")") {
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

	oom := deleting(pod("web-abc-3", labels, longAgo, ref), reasonOOMKilled, 137)
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
