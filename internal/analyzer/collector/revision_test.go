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
	"maps"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

const (
	newRS     = "web-5f6d7c8b9"
	newRSUID  = types.UID("7c1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d13")
	newPod    = "web-5f6d7c8b9-abcde"
	newPodUID = types.UID("8d1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d14")
	revOld    = "db-6b8f9c7d5"
	revNew    = "db-7c9a8d6e4"
	dbPod     = "db-0"
)

// rolledOut returns the lab sequence of #724: Deployment web moved from revision 1 (the ReplicaSet of the
// workload fixture) to revision 2 (a new ReplicaSet), while the pod of revision 1 is still there.
func rolledOut() (*appsv1.Deployment, *appsv1.ReplicaSet, *appsv1.ReplicaSet, *corev1.Pod, *corev1.Pod) {
	d, rs, pod := workload()
	d.Annotations = map[string]string{revisionAnnotation: "2"}
	rs.Annotations = map[string]string{revisionAnnotation: "1"}
	nrs := rs.DeepCopy()
	nrs.Name, nrs.UID = newRS, newRSUID
	nrs.Labels = map[string]string{labelApp: web, "pod-template-hash": "5f6d7c8b9"}
	nrs.Spec.Selector = &metav1.LabelSelector{MatchLabels: maps.Clone(nrs.Labels)}
	nrs.Annotations = map[string]string{revisionAnnotation: "2"}
	np := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: newPod, Namespace: testNS, UID: newPodUID, Labels: maps.Clone(nrs.Labels),
		OwnerReferences: controllerRef(apiApps, kindReplicaSet, newRS, newRSUID),
	}}
	return d, rs, nrs, pod, np
}

// statefulSet returns StatefulSet db, rolled out to revNew, and its pod db-0 at revision podRev.
func statefulSet(podRev string) (*appsv1.StatefulSet, *corev1.Pod) {
	dbLabels := map[string]string{labelApp: "db"}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: testNS, UID: stsUID, Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: dbLabels}},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2, CurrentRevision: revOld, UpdateRevision: revNew,
		},
	}
	podLabels := maps.Clone(dbLabels)
	if podRev != "" {
		podLabels[appsv1.ControllerRevisionHashLabelKey] = podRev
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: dbPod, Namespace: testNS, UID: podUID, Labels: podLabels,
		OwnerReferences: controllerRef(apiApps, kindStatefulSet, "db", stsUID),
	}}
	return sts, pod
}

func oom(pod *corev1.Pod) *corev1.Pod {
	return withTermination(pod, "containerd://"+string(pod.UID), ReasonOOMKilled, 137, t0)
}

// TestReplacedTemplateEvidence checks that a termination of a pod is an observation only while the pod's
// template is its workload's current one, and that every case where that is not known keeps it (#724).
func TestReplacedTemplateEvidence(t *testing.T) {
	partition := int32(1)
	cases := []struct {
		name string
		objs func() ([]client.Object, string) // the objects and the pod reconciled
		want bool                             // an observation
	}{
		{"old ReplicaSet's pod after the Deployment moved on", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			return []client.Object{d, rs, nrs, oom(pod), np}, webPod
		}, false},
		{"new ReplicaSet's pod during the rollout", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			return []client.Object{d, rs, nrs, pod, oom(np)}, newPod
		}, true},
		{"rolled back: the revived ReplicaSet is current", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Annotations[revisionAnnotation], rs.Annotations[revisionAnnotation] = "3", "3"
			return []client.Object{d, rs, nrs, oom(pod), np}, webPod
		}, true},
		{"rolled back: the pod of the abandoned ReplicaSet", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Annotations[revisionAnnotation], rs.Annotations[revisionAnnotation] = "3", "3"
			return []client.Object{d, rs, nrs, pod, oom(np)}, newPod
		}, false},
		{"Deployment controller behind: ReplicaSet newer than the Deployment", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Annotations[revisionAnnotation] = "1"
			return []client.Object{d, rs, nrs, pod, oom(np)}, newPod
		}, true},
		{"ReplicaSet without a revision", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			rs.Annotations = nil
			return []client.Object{d, rs, nrs, oom(pod), np}, webPod
		}, true},
		{"Deployment without a revision", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Annotations = nil
			return []client.Object{d, rs, nrs, oom(pod), np}, webPod
		}, true},
		{"revision not a number", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Annotations[revisionAnnotation] = "two"
			return []client.Object{d, rs, nrs, oom(pod), np}, webPod
		}, true},
		{"bare ReplicaSet", func() ([]client.Object, string) {
			_, rs, _, pod, _ := rolledOut()
			rs.OwnerReferences = nil
			return []client.Object{rs, oom(pod)}, webPod
		}, true},
		{"pod without a followed controller", func() ([]client.Object, string) {
			_, _, _, pod, _ := rolledOut()
			pod.OwnerReferences = controllerRef("batch/v1", "Job", "migrate", rsUID)
			return []client.Object{oom(pod)}, webPod
		}, true},
		{"StatefulSet pod of the old revision", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			return []client.Object{sts, oom(pod)}, dbPod
		}, false},
		{"StatefulSet pod of the update revision", func() ([]client.Object, string) {
			sts, pod := statefulSet(revNew)
			return []client.Object{sts, oom(pod)}, dbPod
		}, true},
		{"StatefulSet pod without a revision label", func() ([]client.Object, string) {
			sts, pod := statefulSet("")
			return []client.Object{sts, oom(pod)}, dbPod
		}, true},
		{"StatefulSet status behind its spec", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			sts.Generation = 3
			return []client.Object{sts, oom(pod)}, dbPod
		}, true},
		{"StatefulSet without an update revision", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			sts.Status.UpdateRevision = ""
			return []client.Object{sts, oom(pod)}, dbPod
		}, true},
		{"StatefulSet with a partition", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: &partition},
			}
			return []client.Object{sts, oom(pod)}, dbPod
		}, true},
		{"StatefulSet updated on delete", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
			return []client.Object{sts, oom(pod)}, dbPod
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs, name := tc.objs()
			h := newHarness(t, Config{}, objs...)
			before := droppedTotal()
			h.reconcilePod(name)
			got := h.drain()
			if tc.want && len(got) != 1 {
				t.Fatalf("got %d observations, want 1", len(got))
			}
			if !tc.want && len(got) != 0 {
				t.Fatalf("got %d observations of a replaced template, want none: %+v", len(got), got)
			}
			if dropped := droppedTotal() - before; (dropped == 1) == tc.want || dropped > 1 {
				t.Errorf("dropped counter rose by %v", dropped)
			}
		})
	}
}

func droppedTotal() float64 {
	return testutil.ToFloat64(replacedTemplateDropped.WithLabelValues(kindDeployment)) +
		testutil.ToFloat64(replacedTemplateDropped.WithLabelValues(kindStatefulSet))
}

// TestRolloutAfterTheOwnerWasCached is the lab sequence of #724 in time: the pod's ReplicaSet was current
// when its owners were first read and cached, then the Deployment moved on while the pod was still there.
// The cached owner facts must not stand for the current revision.
func TestRolloutAfterTheOwnerWasCached(t *testing.T) {
	d, rs, nrs, pod, _ := rolledOut()
	d.Annotations[revisionAnnotation] = "1"
	h := newHarness(t, Config{}, d, rs, oom(pod))
	h.reconcilePod(webPod)
	h.one()

	h.create(nrs)
	d.Annotations[revisionAnnotation] = "2"
	h.update(d)
	var p corev1.Pod
	if err := h.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: webPod}, &p); err != nil {
		t.Fatal(err)
	}
	h.update(withTermination(&p, "containerd://second", ReasonOOMKilled, 137, t0.Add(1)))
	h.reconcilePod(webPod)
	h.none()

	// An event about the same pod after it is gone reaches the workload through what was remembered, and is
	// dropped the same way.
	if err := h.cl.Delete(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	ev := warning("gone", 0)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	h.none()
}

// failRevisionReads fails every read once fail is set, so the owner chain resolves from the cache while the
// read of the current revision fails.
type failRevisionReads struct {
	client.Reader
	fail bool
}

func (f *failRevisionReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if f.fail {
		return errors.New("synthetic read failure")
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// TestRevisionReadErrorKeepsTheEvidence checks the fail-open path: when the current revision cannot be read,
// the evidence is kept as before, not dropped and not retried forever.
func TestRevisionReadErrorKeepsTheEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		objs []client.Object
		pod  string
	}{
		{"Deployment", func() []client.Object {
			d, rs, nrs, pod, np := rolledOut()
			return []client.Object{d, rs, nrs, oom(pod), np}
		}(), webPod},
		{"StatefulSet", func() []client.Object {
			sts, pod := statefulSet(revOld)
			return []client.Object{sts, oom(pod)}
		}(), dbPod},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{}, tc.objs...)
			reader := &failRevisionReads{Reader: h.c.owners.api}
			h.c.owners.api = reader
			// Cache the owner chain first, through a check that is never reached: no termination yet.
			if _, _, _, err := h.c.owners.ownedSubject(t.Context(), subjectOfPod(t, h, tc.pod),
				*controllerOf(t, h, tc.pod), labelsOf(t, h, tc.pod)); err != nil {
				t.Fatal(err)
			}
			reader.fail = true
			h.reconcilePod(tc.pod)
			h.one()
		})
	}
}

func readPod(t *testing.T, h *harness, name string) *corev1.Pod {
	t.Helper()
	var p corev1.Pod
	if err := h.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func subjectOfPod(t *testing.T, h *harness, name string) remediationv1alpha1.ObjectRef {
	p := readPod(t, h, name)
	return remediationv1alpha1.ObjectRef{
		APIVersion: apiCore, Kind: kindPod, Namespace: testNS, Name: p.Name, UID: remediationv1alpha1.UID(p.UID),
	}
}

func controllerOf(t *testing.T, h *harness, name string) *metav1.OwnerReference {
	return metav1.GetControllerOf(readPod(t, h, name))
}

func labelsOf(t *testing.T, h *harness, name string) map[string]string {
	return readPod(t, h, name).Labels
}
