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

package clearing

import (
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/verification/rollout"
)

const (
	ns            = "shop"
	app           = "main"
	labelApp      = "app"
	dbRevision    = "db-new"
	dbOldRevision = "db-old"
	sidecar       = "proxy"
	revision      = "2"
	oldRevision   = "1"
	deployUID     = "d0000000-0000-4000-8000-000000000001"
	rsUID         = "d0000000-0000-4000-8000-000000000002"
	oldRSUID      = "d0000000-0000-4000-8000-000000000003"
	stsUID        = "d0000000-0000-4000-8000-000000000004"
	requestUID    = "d0000000-0000-4000-8000-000000000005"
	otherUID      = "d0000000-0000-4000-8000-0000000000ff"
	mergeSHA      = "0123456789abcdef0123456789abcdef01234567"
	rsName        = "web-2"
	oldRSName     = "web-1"
)

// t0 is the verifier's clock at the first sample of a test.
var t0 = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

var limit = resource.MustParse("512Mi")

var webLabels = map[string]string{labelApp: "web"}

func podUID(i int) types.UID { return types.UID(fmt.Sprintf("a0000000-0000-4000-8000-%012d", i)) }

func controllerRef(kind, name string, uid types.UID) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: uid, Controller: new(true)}}
}

func deployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: ns, UID: deployUID, Labels: webLabels,
			Annotations: map[string]string{rollout.DeploymentRevisionAnnotation: revision},
		},
		Spec: appsv1.DeploymentSpec{Replicas: new(replicas), Selector: &metav1.LabelSelector{MatchLabels: webLabels}},
	}
}

func replicaSet(name string, uid types.UID, rev string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: uid, Labels: webLabels,
			Annotations:     map[string]string{rollout.DeploymentRevisionAnnotation: rev},
			OwnerReferences: controllerRef("Deployment", "web", deployUID),
		},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: webLabels}},
	}
}

// getter reads the given ReplicaSets by name.
func getter(rss ...*appsv1.ReplicaSet) ReplicaSetGetter {
	return func(name string) (*appsv1.ReplicaSet, error) {
		for _, rs := range rss {
			if rs.Name == name {
				return rs, nil
			}
		}
		return nil, nil
	}
}

// pod is a ready pod of the rollout with the action's container at the desired limit and a sidecar, both
// with the given restart count.
func pod(i int, restarts int32) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("web-2-%d", i), Namespace: ns, UID: podUID(i), Labels: webLabels,
			OwnerReferences: controllerRef("ReplicaSet", rsName, rsUID),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: app, Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: limit}}},
			{Name: sidecar},
		}},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: app, RestartCount: restarts},
				{Name: sidecar},
			},
		},
	}
}

func pods(n int) []corev1.Pod {
	out := make([]corev1.Pod, n)
	for i := range n {
		out[i] = pod(i, 0)
	}
	return out
}

func spec() v1alpha1.VerificationReportSpec {
	return v1alpha1.VerificationReportSpec{
		RequestUID: requestUID, MergeCommitSHA: mergeSHA, TargetUID: deployUID, TemplateRevision: revision, Container: app,
	}
}

// input is a sample of a Deployment with replicas pods, all of the rollout, at now.
func input(now time.Time, replicas int32, ps []corev1.Pod) Input {
	return Input{
		Now: now, Target: deployment(replicas), Pods: ps,
		ReplicaSets: getter(replicaSet(rsName, rsUID, revision), replicaSet(oldRSName, oldRSUID, oldRevision)),
		MemoryLimit: &limit,
	}
}

// sample runs Sample and fails the test on an error.
func sample(t *testing.T, s v1alpha1.VerificationReportSpec, prev v1alpha1.VerificationReportStatus, in Input) Result {
	t.Helper()
	r, err := Sample(s, prev, in)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	return r
}

// samples runs good samples of three pods every poll interval from t0 for n intervals and returns the status.
func samples(t *testing.T, n int) v1alpha1.VerificationReportStatus {
	t.Helper()
	var st v1alpha1.VerificationReportStatus
	for i := range n {
		r := sample(t, spec(), st, input(t0.Add(time.Duration(i)*PollInterval), 3, pods(3)))
		if r.Reason != "" {
			t.Fatalf("sample %d: reason %q, want a good sample", i, r.Reason)
		}
		st = r.Status
	}
	return st
}

func expected() Expected {
	return Expected{
		RequestUID: requestUID, MergeCommitSHA: mergeSHA, TargetUID: deployUID, TemplateRevision: revision,
		Container: app, Replicas: 3,
	}
}

func report(st v1alpha1.VerificationReportStatus) *v1alpha1.VerificationReport {
	return &v1alpha1.VerificationReport{
		ObjectMeta: metav1.ObjectMeta{Name: "vr", Namespace: "shadewright-system", UID: "e0000000-0000-4000-8000-000000000001"},
		Spec:       spec(), Status: st,
	}
}

func mt(t time.Time) *metav1.Time {
	m := metav1.NewTime(t)
	return &m
}
