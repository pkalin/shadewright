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

package verifier

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// TestTrimKeepsOnlyTheFieldsOfSection7: a pod read by the verifier keeps only UID, namespace, owner references,
// labels, the Ready and PodScheduled conditions, creation and deletion times, container names, the memory limit,
// restart counts and the reason and finish time of terminations. Everything else, the termination message,
// environment values and annotations included, is gone before anything can store, log or emit it.
//
//nolint:goconst // literals keep the fixture readable
func TestTrimKeepsOnlyTheFieldsOfSection7(t *testing.T) {
	const secret = "s3cr3t-token-value"
	finished := metav1.NewTime(time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC))
	term := &corev1.ContainerStateTerminated{Reason: "OOMKilled", Message: secret, ExitCode: 137, FinishedAt: finished}
	now := metav1.NewTime(finished.Add(time.Minute))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-" + secret, Namespace: "orders", UID: "pod-uid",
			Labels: map[string]string{"app": "web"}, Annotations: map[string]string{"note": secret},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-rs", UID: "rs-uid", Controller: new(true)}},
			CreationTimestamp: finished, DeletionTimestamp: &now},
		Spec: corev1.PodSpec{
			NodeName: "node-" + secret,
			InitContainers: []corev1.Container{{Name: "sidecar", Image: "img/" + secret,
				RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways)}},
			Containers: []corev1.Container{{Name: "worker", Image: "img/" + secret, Args: []string{secret},
				Env: []corev1.EnvVar{{Name: "TOKEN", Value: secret}},
				Resources: corev1.ResourceRequirements{
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("384Mi"), corev1.ResourceCPU: resource.MustParse("1")},
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}}}},
		},
		Status: corev1.PodStatus{
			Message: secret, Reason: secret, PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, Message: secret, Reason: secret},
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, Message: secret},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "worker", RestartCount: 2, Image: "img/" + secret,
				ImageID: secret, ContainerID: secret,
				State:                corev1.ContainerState{Terminated: term.DeepCopy()},
				LastTerminationState: corev1.ContainerState{Terminated: term.DeepCopy()}}},
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "sidecar", RestartCount: 1,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: secret}}}},
		},
	}

	got := Trim(pod)

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Errorf("the trimmed pod still holds text from the cluster: %s", b)
	}
	for _, field := range []string{"10.0.0.1", "ContainersReady", "CrashLoopBackOff", "137", `"cpu"`, "requests", "annotations", "nodeName"} {
		if strings.Contains(string(b), field) {
			t.Errorf("the trimmed pod keeps %s: %s", field, b)
		}
	}
	if got.UID != pod.UID || got.Namespace != "orders" || got.Labels["app"] != "web" || len(got.OwnerReferences) != 1 ||
		!got.CreationTimestamp.Equal(&finished) || got.DeletionTimestamp == nil {
		t.Errorf("metadata the sample needs was dropped: %+v", got.ObjectMeta)
	}
	if len(got.Status.Conditions) != 2 {
		t.Errorf("conditions = %+v, want Ready and PodScheduled", got.Status.Conditions)
	}
	c := got.Spec.Containers[0]
	if c.Name != "worker" || c.Resources.Limits.Memory().String() != "384Mi" {
		t.Errorf("container = %+v, want its name and memory limit", c)
	}
	if ic := got.Spec.InitContainers[0]; ic.Name != "sidecar" || ic.RestartPolicy == nil {
		t.Errorf("init container = %+v, want its name and restart policy", ic)
	}
	s := got.Status.ContainerStatuses[0]
	if s.RestartCount != 2 || s.State.Terminated == nil || s.State.Terminated.Reason != "OOMKilled" ||
		!s.LastTerminationState.Terminated.FinishedAt.Equal(&finished) {
		t.Errorf("container status = %+v, want restarts and terminations", s)
	}
	if got.Status.InitContainerStatuses[0].RestartCount != 1 {
		t.Errorf("init container status = %+v", got.Status.InitContainerStatuses[0])
	}
}
