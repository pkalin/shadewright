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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestEnvtestPodUpdateSamplesTheHold: a BackOff event with no cause is held, and the hold ends on an update of
// its pod alone, with no new event. The update shows a waiting cause (ImagePullBackOff), so the sample it makes
// releases the hold and the consequence is emitted with that cause. Only the pod watch of the event controller
// (heldEventsOf) can reconcile the event before its requeue at the end of ConsequenceGrace.
func TestEnvtestPodUpdateSamplesTheHold(t *testing.T) {
	startEnv(t)
	ctx := t.Context()
	labels := map[string]string{containerApp: "held"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "held", Namespace: watchedNS},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: containerApp, Image: "example.invalid/held:1"}}},
			},
		},
	}
	if err := env.admin.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "held-5c9d7b6f4", Namespace: watchedNS, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: apiApps, Kind: kindDeployment, Name: d.Name, UID: d.UID, Controller: new(true),
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Selector: d.Spec.Selector, Template: d.Spec.Template},
	}
	if err := env.admin.Create(ctx, rs); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "held-5c9d7b6f4-x2k8p", Namespace: watchedNS, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: apiApps, Kind: kindReplicaSet, Name: rs.Name, UID: rs.UID, Controller: new(true),
			}},
		},
		Spec: d.Spec.Template.Spec,
	}
	if err := env.admin.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	// Waiting in CrashLoopBackOff: failing, with no cause.
	pod.Status.ContainerStatuses = holdPod(reasonBackOff, sampleFailing, 3).Status.ContainerStatuses
	if err := env.admin.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	ev := envEvent(watchedNS, "held-backoff", 0)
	ev.Regarding = corev1.ObjectReference{
		APIVersion: apiCore, Kind: kindPod, Namespace: watchedNS, Name: pod.Name, UID: pod.UID, FieldPath: appPath,
	}
	if err := env.admin.Create(ctx, ev); err != nil {
		t.Fatal(err)
	}
	held := func(o Observation) bool { return o.Reason == reasonBackOff && o.Subject.Name == d.Name }
	never(t, 2*time.Second, held)
	if len(env.col.holds.ofPod(pod.UID)) != 1 {
		t.Fatal("the BackOff event with no cause is not held")
	}

	// Only the pod changes: its container now waits on an image it cannot pull, a cause.
	pod.Status.ContainerStatuses[0].State.Waiting.Reason = reasonImagePull
	if err := env.admin.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	got := await(t, held)
	if got.Class != ClassConsequence || got.CauseFingerprint == "" {
		t.Fatalf("emitted %+v, want the consequence released with its cause", got)
	}
	if keys := env.col.holds.ofPod(pod.UID); len(keys) != 0 {
		t.Fatalf("holds %v left on the pod after the release, want none", keys)
	}
}
