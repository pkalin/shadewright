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
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/modules"
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
// workload fixture) to revision 2 (a new ReplicaSet), while the pod of revision 1 is still there. Both
// ReplicaSets still have replicas, as during a rollout: retire scales the old one to 0.
func rolledOut() (*appsv1.Deployment, *appsv1.ReplicaSet, *appsv1.ReplicaSet, *corev1.Pod, *corev1.Pod) {
	d, rs, pod := workload()
	d.Generation, d.Status.ObservedGeneration = 2, 2
	d.Annotations = map[string]string{revisionAnnotation: "2"}
	rs.Annotations = map[string]string{revisionAnnotation: "1"}
	rs.Spec.Replicas = new(int32(1))
	nrs := rs.DeepCopy()
	nrs.Name, nrs.UID = newRS, newRSUID
	nrs.Labels = map[string]string{labelApp: web, "pod-template-hash": "5f6d7c8b9"}
	nrs.Spec.Selector = &metav1.LabelSelector{MatchLabels: maps.Clone(nrs.Labels)}
	nrs.Annotations = map[string]string{revisionAnnotation: "2"}
	nrs.Spec.Replicas = new(int32(1))
	np := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: newPod, Namespace: testNS, UID: newPodUID, Labels: maps.Clone(nrs.Labels),
		OwnerReferences: controllerRef(apiApps, kindReplicaSet, newRS, newRSUID),
	}}
	return d, rs, nrs, pod, np
}

// retire scales rs to 0, as the deployment controller does once the rollout moved past it.
func retire(rs *appsv1.ReplicaSet) *appsv1.ReplicaSet {
	rs.Spec.Replicas = new(int32(0))
	return rs
}

// terminating gives pod a deletion timestamp (and a finalizer, which the fake client needs to keep it).
func terminating(pod *corev1.Pod) *corev1.Pod {
	pod.DeletionTimestamp = &metav1.Time{Time: t0}
	pod.Finalizers = []string{testFinalizer}
	return pod
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

// TestReplacedTemplateEvidence checks that a termination of a pod is dropped only when its workload has
// retired the pod's template, and that a paused or stuck rollout and every case where that is not known keep
// it (#724).
func TestReplacedTemplateEvidence(t *testing.T) {
	partition := int32(1)
	cases := []struct {
		name string
		objs func() ([]client.Object, string) // the objects and the pod reconciled
		want bool                             // an observation
	}{
		{"old ReplicaSet retired, its last pod terminating", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			return []client.Object{d, retire(rs), nrs, terminating(oom(pod)), np}, webPod
		}, false},
		{"old ReplicaSet retired, its pod not yet terminating", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			return []client.Object{d, retire(rs), nrs, oom(pod), np}, webPod
		}, false},
		{"rollout paused, old pods keep serving and failing", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Spec.Paused = true
			return []client.Object{d, rs, nrs, oom(pod), np}, webPod
		}, true},
		{"rollout paused, a new pod of the old ReplicaSet", func() ([]client.Object, string) {
			d, rs, nrs, _, np := rolledOut()
			d.Spec.Paused = true
			rs.Spec.Replicas = new(int32(2))
			_, _, fresh := workload()
			fresh.Name, fresh.UID = webRS+"-fresh", types.UID("9e1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d15")
			return []client.Object{d, rs, nrs, oom(fresh), np}, fresh.Name
		}, true},
		{"rollout stuck, new pods never ready", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing,
				Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
			return []client.Object{d, rs, nrs, oom(pod), np}, webPod
		}, true},
		{"rollback not yet observed: Deployment status behind its spec", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Generation = 3
			return []client.Object{d, retire(rs), nrs, terminating(oom(pod)), np}, webPod
		}, true},
		{"new ReplicaSet's pod during the rollout", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			return []client.Object{d, rs, nrs, pod, oom(np)}, newPod
		}, true},
		{"rolled back: the revived ReplicaSet is current", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Annotations[revisionAnnotation], rs.Annotations[revisionAnnotation] = "3", "3"
			return []client.Object{d, rs, retire(nrs), oom(pod), np}, webPod
		}, true},
		{"rolled back: the pod of the retired ReplicaSet", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Annotations[revisionAnnotation], rs.Annotations[revisionAnnotation] = "3", "3"
			return []client.Object{d, rs, retire(nrs), pod, terminating(oom(np))}, newPod
		}, false},
		{"Deployment controller behind: ReplicaSet newer than the Deployment", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Annotations[revisionAnnotation] = "1"
			return []client.Object{d, rs, retire(nrs), pod, oom(np)}, newPod
		}, true},
		{"ReplicaSet without a revision", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			rs.Annotations = nil
			return []client.Object{d, retire(rs), nrs, oom(pod), np}, webPod
		}, true},
		{"Deployment without a revision", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Annotations = nil
			return []client.Object{d, retire(rs), nrs, oom(pod), np}, webPod
		}, true},
		{"revision not a number", func() ([]client.Object, string) {
			d, rs, nrs, pod, np := rolledOut()
			d.Annotations[revisionAnnotation] = "two"
			return []client.Object{d, retire(rs), nrs, oom(pod), np}, webPod
		}, true},
		{"bare ReplicaSet", func() ([]client.Object, string) {
			_, rs, _, pod, _ := rolledOut()
			rs.OwnerReferences = nil
			return []client.Object{retire(rs), terminating(oom(pod))}, webPod
		}, true},
		{"pod without a followed controller", func() ([]client.Object, string) {
			_, _, _, pod, _ := rolledOut()
			pod.OwnerReferences = controllerRef("batch/v1", "Job", "migrate", rsUID)
			return []client.Object{terminating(oom(pod))}, webPod
		}, true},
		{"StatefulSet pod of the old revision, terminating", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			return []client.Object{sts, terminating(oom(pod))}, dbPod
		}, false},
		{"StatefulSet pod of the old revision, waiting its turn", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			return []client.Object{sts, oom(pod)}, dbPod
		}, true},
		{"StatefulSet pod of the update revision, revision label edited", func() ([]client.Object, string) {
			sts, pod := statefulSet("db-edited")
			return []client.Object{sts, oom(pod)}, dbPod
		}, true},
		{"StatefulSet pod of the update revision, terminating", func() ([]client.Object, string) {
			sts, pod := statefulSet(revNew)
			return []client.Object{sts, terminating(oom(pod))}, dbPod
		}, true},
		{"StatefulSet pod without a revision label", func() ([]client.Object, string) {
			sts, pod := statefulSet("")
			return []client.Object{sts, terminating(oom(pod))}, dbPod
		}, true},
		{"StatefulSet status behind its spec", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			sts.Generation = 3
			return []client.Object{sts, terminating(oom(pod))}, dbPod
		}, true},
		{"StatefulSet without an update revision", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			sts.Status.UpdateRevision = ""
			return []client.Object{sts, terminating(oom(pod))}, dbPod
		}, true},
		{"StatefulSet with a partition", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: &partition},
			}
			return []client.Object{sts, terminating(oom(pod))}, dbPod
		}, true},
		{"StatefulSet updated on delete", func() ([]client.Object, string) {
			sts, pod := statefulSet(revOld)
			sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
			return []client.Object{sts, terminating(oom(pod))}, dbPod
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
				t.Fatalf("got %d observations of a retired template, want none: %+v", len(got), got)
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

func checkErrorsTotal() float64 {
	return testutil.ToFloat64(replacedTemplateCheckErrors.WithLabelValues(kindDeployment)) +
		testutil.ToFloat64(replacedTemplateCheckErrors.WithLabelValues(kindStatefulSet))
}

// TestRetiredTemplateDroppedOnce checks the bookkeeping of a drop: each termination of a pod of a retired
// template is counted once, later updates of the pod neither read nor count it again, and a rollback that
// revives the template does not bring the dropped terminations back.
func TestRetiredTemplateDroppedOnce(t *testing.T) {
	d, rs, nrs, pod, np := rolledOut()
	h := newHarness(t, Config{}, d, retire(rs), nrs, terminating(oom(pod)), np)
	before := droppedTotal()
	h.reconcilePod(webPod)
	h.none()
	gets := h.gets.Load()
	for range 3 {
		p := readPod(t, h, webPod)
		p.Annotations = map[string]string{"touched": h.clock.Now().String()}
		h.update(p)
		h.reconcilePod(webPod)
		h.none()
	}
	if got := droppedTotal() - before; got != 1 {
		t.Errorf("dropped counter rose by %v over repeated updates, want 1", got)
	}
	if n := h.gets.Load() - gets; n != 0 {
		t.Errorf("%d reads for updates of a pod whose termination was handled, want none", n)
	}

	// Rolled back to the pod's template: the dropped termination is not emitted late.
	d = readDeployment(t, h)
	d.Annotations[revisionAnnotation] = "3"
	h.update(d)
	rs = readReplicaSet(t, h, webRS)
	rs.Annotations[revisionAnnotation], rs.Spec.Replicas = "3", new(int32(1))
	h.update(rs)
	h.clock.add(templateCheckTTL + time.Second)
	h.reconcilePod(webPod)
	h.none()
}

// TestRetiredTemplateCheckIsCheap checks the cost of the check: a pod of the current template costs one read
// of its ReplicaSet, reused for the other pods of that ReplicaSet within templateCheckTTL, and a StatefulSet
// pod that is not terminating costs no read beyond its owner chain.
func TestRetiredTemplateCheckIsCheap(t *testing.T) {
	d, rs, pod := workload()
	rs.Spec.Replicas = new(int32(2))
	second := pod.DeepCopy()
	second.Name, second.UID = webRS+"-second", types.UID("9e1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d16")
	h := newHarness(t, Config{}, d, rs, oom(pod), oom(second))
	h.reconcilePod(webPod)
	h.one()
	// The chain (ReplicaSet, Deployment) and one read of the ReplicaSet's state; no read of the Deployment.
	if n := h.gets.Load(); n != 3 {
		t.Errorf("%d reads for the first pod, want 3", n)
	}
	h.reconcilePod(second.Name)
	h.one()
	if n := h.gets.Load(); n != 3 {
		t.Errorf("%d reads after a second pod of the same ReplicaSet, want still 3", n)
	}

	sts, db := statefulSet(revOld)
	hs := newHarness(t, Config{}, sts, oom(db))
	hs.reconcilePod(dbPod)
	hs.one()
	if n := hs.gets.Load(); n != 1 {
		t.Errorf("%d reads for a StatefulSet pod that is not terminating, want 1 (the chain)", n)
	}
}

// TestRolloutAfterTheOwnerWasCached is the lab sequence of #724 in time: the pod's ReplicaSet was current
// when its owners were first read and cached, then the Deployment moved on and retired it while the pod was
// still there. Neither the cached owner facts nor a cached check may stand for longer than templateCheckTTL.
func TestRolloutAfterTheOwnerWasCached(t *testing.T) {
	d, rs, nrs, pod, _ := rolledOut()
	d.Annotations[revisionAnnotation] = "1"
	h := newHarness(t, Config{}, d, rs, oom(pod))
	h.reconcilePod(webPod)
	h.one()

	h.create(nrs)
	d.Annotations[revisionAnnotation] = "2"
	h.update(d)
	h.update(retire(readReplicaSet(t, h, webRS)))
	p := readPod(t, h, webPod)
	h.update(withTermination(p, "containerd://second", ReasonOOMKilled, 137, t0.Add(1)))
	// Within templateCheckTTL the check read before the rollout still stands.
	h.reconcilePod(webPod)
	h.one()

	h.clock.add(templateCheckTTL + time.Second)
	p = readPod(t, h, webPod)
	h.update(withTermination(p, "containerd://third", ReasonOOMKilled, 137, t0.Add(2)))
	h.reconcilePod(webPod)
	h.none()

	// An event about the same pod after it is gone reaches the workload through what was remembered, and is
	// dropped the same way.
	if err := h.cl.Delete(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	ev := warning("gone", 0)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	h.none()
}

// TestReplicaSetDeletedBeforeItsPod is a ReplicaSet deleted while its last pod still terminates
// (revisionHistoryLimit 0): with the owner chain cached, a confirmed NotFound of the ReplicaSet retires the
// template of a terminating pod, and of no other.
func TestReplicaSetDeletedBeforeItsPod(t *testing.T) {
	for _, tc := range []struct {
		name        string
		terminating bool
		want        bool // an observation
	}{
		{"pod terminating", true, false},
		{"pod not terminating", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, rs, nrs, pod, np := rolledOut()
			pod = oom(pod)
			if tc.terminating {
				pod = terminating(pod)
			}
			h := newHarness(t, Config{}, d, retire(rs), nrs, pod, np)
			if _, _, _, err := h.c.owners.ownedSubject(t.Context(), subjectOfPod(t, h, webPod),
				*controllerOf(t, h, webPod), labelsOf(t, h, webPod)); err != nil {
				t.Fatal(err)
			}
			if err := h.cl.Delete(t.Context(), readReplicaSet(t, h, webRS)); err != nil {
				t.Fatal(err)
			}
			before := droppedTotal()
			h.reconcilePod(webPod)
			if got := len(h.drain()); (got == 1) != tc.want {
				t.Fatalf("got %d observations, want an observation: %v", got, tc.want)
			}
			if dropped := droppedTotal() - before; (dropped == 1) == tc.want {
				t.Errorf("dropped counter rose by %v", dropped)
			}
		})
	}
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

// TestRevisionReadErrorKeepsTheEvidence checks the fail-open path: when the workload's state cannot be read,
// the evidence is kept as before, not dropped and not retried forever, and the failed check is counted.
func TestRevisionReadErrorKeepsTheEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		objs []client.Object
		pod  string
	}{
		{"Deployment", func() []client.Object {
			d, rs, nrs, pod, np := rolledOut()
			return []client.Object{d, retire(rs), nrs, terminating(oom(pod)), np}
		}(), webPod},
		{"StatefulSet", func() []client.Object {
			sts, pod := statefulSet(revOld)
			return []client.Object{sts, terminating(oom(pod))}
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
			before := checkErrorsTotal()
			h.reconcilePod(tc.pod)
			h.one()
			if got := checkErrorsTotal() - before; got != 1 {
				t.Errorf("check errors counter rose by %v, want 1", got)
			}
		})
	}
}

// retiredLog captures the debug log of a reconcile.
func retiredLog(t *testing.T) (context.Context, *recordingSink) {
	sink := &recordingSink{}
	return logf.IntoContext(t.Context(), logr.New(sink)), sink
}

const (
	logRetired = "Dropped evidence of a pod whose template its workload has retired"
	logEvent   = "Ignored Event that cannot be correlated"
	logSchema  = "Ignored symptom outside the Incident schema"
)

// TestRetiredTemplateOnTheEventPath checks that an event about an existing pod of a retired template is
// dropped, counted once and logged as such, not as an event that cannot be correlated.
func TestRetiredTemplateOnTheEventPath(t *testing.T) {
	d, rs, nrs, pod, np := rolledOut()
	ev := warning("retired", 0)
	h := newHarness(t, Config{}, d, retire(rs), nrs, terminating(pod), np, ev)
	ctx, sink := retiredLog(t)
	before := droppedTotal()
	if _, err := h.events.Reconcile(ctx, reqFor(ev.Name)); err != nil {
		t.Fatal(err)
	}
	h.none()
	if got := droppedTotal() - before; got != 1 {
		t.Errorf("dropped counter rose by %v, want 1", got)
	}
	infos, _ := sink.messages()
	if !slices.Contains(infos, logRetired) || slices.Contains(infos, logEvent) {
		t.Errorf("logged %q, want %q and not %q", infos, logRetired, logEvent)
	}
}

// TestRetiredTemplateOnThePodSourcePath checks the Pod symptom source: a not-ready cloudflared container of a
// pod whose template its Deployment has retired is dropped, counted once per state and logged as such, not as
// a symptom outside the Incident schema.
func TestRetiredTemplateOnThePodSourcePath(t *testing.T) {
	d, rs, pod := tunnelWorkload()
	d.Generation, d.Status.ObservedGeneration = 2, 2
	d.Annotations = map[string]string{revisionAnnotation: "2"}
	rs.Annotations = map[string]string{revisionAnnotation: "1"}
	h := &podSourceHarness{sel: cloudflaredSelection(t, true)}
	// Not terminating: the source skips pods being deleted, and the ReplicaSet scaled to 0 decides.
	h.harness = newHarness(t, Config{Modules: func() modules.Selection { return h.sel }}, d, retire(rs), pod)
	h.r = &sourceReconciler{c: h.c, reader: h.cl, api: h.cl, gvk: sourcePod}
	ctx, sink := retiredLog(t)
	before := droppedTotal()
	for range 3 {
		if _, err := h.r.Reconcile(ctx, reqFor(webPod)); err != nil {
			t.Fatal(err)
		}
		h.clock.add(time.Minute)
	}
	h.none()
	if got := droppedTotal() - before; got != 1 {
		t.Errorf("dropped counter rose by %v, want 1", got)
	}
	infos, _ := sink.messages()
	if !slices.Contains(infos, logRetired) || slices.Contains(infos, logSchema) {
		t.Errorf("logged %q, want %q and not %q", infos, logRetired, logSchema)
	}
}

func readDeployment(t *testing.T, h *harness) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	if err := h.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: web}, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

func readReplicaSet(t *testing.T, h *harness, name string) *appsv1.ReplicaSet {
	t.Helper()
	var rs appsv1.ReplicaSet
	if err := h.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &rs); err != nil {
		t.Fatal(err)
	}
	return &rs
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

// deadlineReads records the deadline of each read of a ReplicaSet or Deployment, and lets a ReplicaSet read
// take pause before it returns.
type deadlineReads struct {
	client.Reader
	pause     time.Duration
	deadlines map[string]time.Time
}

func (f *deadlineReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if dl, ok := ctx.Deadline(); ok {
		switch obj.(type) {
		case *appsv1.ReplicaSet:
			f.deadlines[kindReplicaSet] = dl
			time.Sleep(f.pause)
		case *appsv1.Deployment:
			f.deadlines[kindDeployment] = dl
		}
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// TestTemplateCheckDeadlinePerRead checks that the reads of the ReplicaSet and the Deployment each get the
// lookup deadline of their own, so a slow first read does not shorten the second.
func TestTemplateCheckDeadlinePerRead(t *testing.T) {
	d, rs, nrs, pod, np := rolledOut()
	h := newHarness(t, Config{}, d, retire(rs), nrs, terminating(oom(pod)), np)
	reads := &deadlineReads{Reader: h.cl, pause: 20 * time.Millisecond, deadlines: map[string]time.Time{}}
	h.c.owners.api = reads
	if _, err := h.c.owners.readReplicaSetState(t.Context(), remediationv1alpha1.ObjectRef{
		APIVersion: apiApps, Kind: kindDeployment, Namespace: testNS, Name: web,
		UID: remediationv1alpha1.UID(deployUID)}, *controllerOf(t, h, webPod)); err != nil {
		t.Fatal(err)
	}
	if gap := reads.deadlines[kindDeployment].Sub(reads.deadlines[kindReplicaSet]); gap < reads.pause {
		t.Errorf("the Deployment read's deadline is %v after the ReplicaSet read's, want at least %v", gap,
			reads.pause)
	}
}
