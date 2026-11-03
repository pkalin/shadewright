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
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

func subjectIs(t *testing.T, got remediationv1alpha1.ObjectRef, kind, name string, uid types.UID) {
	t.Helper()
	if got.Kind != kind || got.Name != name || got.UID != remediationv1alpha1.UID(uid) || got.Namespace != testNS {
		t.Fatalf("subject = %+v, want %s %s/%s (%s)", got, kind, testNS, name, uid)
	}
}

func TestPodEventCorrelatesToDeployment(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, pod)
	ev := warning("backoff", 2)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	got := h.one()
	subjectIs(t, got.Subject, kindDeployment, web, deployUID)
	if got.Subject.APIVersion != apiApps {
		t.Errorf("APIVersion = %q, want apps/v1", got.Subject.APIVersion)
	}
}

func TestSuccessivePodsShareAFingerprint(t *testing.T) {
	d, rs, pod := workload()
	pod2 := pod.DeepCopy()
	pod2.Name, pod2.UID = "web-7d4b9c8f6-xk2lp", "1a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	h := newHarness(t, Config{}, d, rs, pod, pod2)
	a := warning("a", 0)
	b := warning("b", 0)
	b.Regarding.Name, b.Regarding.UID = pod2.Name, pod2.UID
	b.Note = "Back-off 10s restarting failed container app in pod " + pod2.Name
	h.create(a)
	h.create(b)
	h.reconcileEvent(a.Name)
	h.reconcileEvent(b.Name)
	got := h.drain()
	if len(got) != 2 || got[0].Fingerprint != got[1].Fingerprint {
		t.Fatalf("two pods of one Deployment with the same error: %+v", got)
	}
}

func TestStatefulSetPod(t *testing.T) {
	dbLabels := map[string]string{labelApp: "db"}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: testNS, UID: stsUID},
		Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: dbLabels}},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "db-0", Namespace: testNS, UID: podUID, Labels: dbLabels,
		OwnerReferences: controllerRef(apiApps, kindStatefulSet, "db", stsUID),
	}}
	h := newHarness(t, Config{}, sts, pod)
	ev := warning("db", 0)
	ev.Regarding.Name = "db-0"
	h.create(ev)
	h.reconcileEvent(ev.Name)
	subjectIs(t, h.one().Subject, kindStatefulSet, "db", stsUID)
}

func TestReplicaSetEventCorrelatesToDeployment(t *testing.T) {
	d, rs, _ := workload()
	h := newHarness(t, Config{}, d, rs)
	ev := warning("rs", 0)
	ev.Reason = "FailedCreate"
	ev.Regarding = corev1.ObjectReference{APIVersion: apiApps, Kind: kindReplicaSet, Name: webRS, UID: rsUID}
	h.create(ev)
	h.reconcileEvent(ev.Name)
	subjectIs(t, h.one().Subject, kindDeployment, web, deployUID)
}

// Owner references are untrusted: a reference to an object with another UID, a missing object or a kind the
// analyzer cannot read never becomes the subject.
func TestUnverifiedOwnersAreNotTrusted(t *testing.T) {
	cases := []struct {
		name     string
		objs     func() []client.Object
		wantKind string
		wantName string
		wantUID  types.UID
	}{
		{
			name: "pod claims a ReplicaSet UID that is not the ReplicaSet's",
			objs: func() []client.Object {
				d, rs, pod := workload()
				pod.OwnerReferences = controllerRef(apiApps, kindReplicaSet, webRS, "99999999-0000-4000-8000-000000000000")
				return []client.Object{d, rs, pod}
			},
			wantKind: kindPod, wantName: webPod, wantUID: podUID,
		},
		{
			name: "ReplicaSet claims a Deployment UID that is not the Deployment's",
			objs: func() []client.Object {
				d, rs, pod := workload()
				d.UID = "88888888-0000-4000-8000-000000000000"
				return []client.Object{d, rs, pod}
			},
			wantKind: kindReplicaSet, wantName: webRS, wantUID: rsUID,
		},
		{
			name: "owner does not exist",
			objs: func() []client.Object {
				_, _, pod := workload()
				return []client.Object{pod}
			},
			wantKind: kindPod, wantName: webPod, wantUID: podUID,
		},
		{
			name: "owner kind the analyzer cannot read",
			objs: func() []client.Object {
				_, _, pod := workload()
				pod.OwnerReferences = controllerRef(apiApps, "DaemonSet", "agent", rsUID)
				return []client.Object{pod}
			},
			wantKind: kindPod, wantName: webPod, wantUID: podUID,
		},
		{
			name: "owner in another API group",
			objs: func() []client.Object {
				d, rs, pod := workload()
				pod.OwnerReferences = controllerRef("example.com/v1", kindReplicaSet, webRS, rsUID)
				return []client.Object{d, rs, pod}
			},
			wantKind: kindPod, wantName: webPod, wantUID: podUID,
		},
		{
			name: "pod whose labels the ReplicaSet does not select",
			objs: func() []client.Object {
				d, rs, pod := workload()
				pod.Labels = map[string]string{labelApp: intruder}
				return []client.Object{d, rs, pod}
			},
			wantKind: kindPod, wantName: webPod, wantUID: podUID,
		},
		{
			name: "owner reference not marked controller",
			objs: func() []client.Object {
				d, rs, pod := workload()
				pod.OwnerReferences[0].Controller = nil
				return []client.Object{d, rs, pod}
			},
			wantKind: kindPod, wantName: webPod, wantUID: podUID,
		},
		{
			name: "ReplicaSet whose labels the Deployment does not select",
			objs: func() []client.Object {
				d, rs, pod := workload()
				d.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: "other"}}
				return []client.Object{d, rs, pod}
			},
			wantKind: kindReplicaSet, wantName: webRS, wantUID: rsUID,
		},
		{
			name: "ReplicaSet with an empty selector",
			objs: func() []client.Object {
				d, rs, pod := workload()
				rs.Spec.Selector = &metav1.LabelSelector{}
				return []client.Object{d, rs, pod}
			},
			wantKind: kindPod, wantName: webPod, wantUID: podUID,
		},
		{
			name: "pod with another UID than the event's regarding object",
			objs: func() []client.Object {
				d, rs, pod := workload()
				pod.UID = "77777777-0000-4000-8000-000000000000"
				return []client.Object{d, rs, pod}
			},
			wantKind: kindPod, wantName: webPod, wantUID: podUID,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, Config{}, c.objs()...)
			ev := warning("ev", 0)
			h.create(ev)
			h.reconcileEvent(ev.Name)
			subjectIs(t, h.one().Subject, c.wantKind, c.wantName, c.wantUID)
		})
	}
}

func TestOwnersAreCachedAndBounded(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{MaxOwners: 2, OwnerTTL: time.Minute}, d, rs, pod)
	for i := range 3 {
		ev := warning(fmt.Sprintf("e%d", i), 0)
		h.create(ev)
		h.reconcileEvent(ev.Name)
	}
	if n := h.gets.Load(); n != 2 {
		t.Fatalf("owner reads for three events = %d, want 2 (ReplicaSet and Deployment once)", n)
	}
	h.clock.add(2 * time.Minute)
	ev := warning("late", 0)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	if n := h.gets.Load(); n != 4 {
		t.Fatalf("owner reads after the TTL = %d, want 4", n)
	}
	for i := range 20 {
		_, _, err := h.c.owners.chain(t.Context(), testNS, metav1.OwnerReference{
			APIVersion: apiApps, Kind: kindStatefulSet, Name: fmt.Sprintf("s%d", i), UID: types.UID(fmt.Sprint(i)),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := len(h.c.owners.cache.Keys()); n > 2 {
		t.Fatalf("owner cache holds %d entries, cap 2", n)
	}
}

// slowReader blocks every read until its context ends.
type slowReader struct{ client.Reader }

func (slowReader) Get(ctx context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestOwnerLookupHasADeadline(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{LookupTimeout: 20 * time.Millisecond}, d, rs, pod)
	h.init(Config{LookupTimeout: 20 * time.Millisecond}, slowReader{})
	ev := warning("slow", 0)
	h.create(ev)
	done := make(chan error, 1)
	go func() {
		_, err := h.events.Reconcile(context.Background(), reqFor(ev.Name))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a lookup past its deadline did not fail the reconcile")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner lookup has no deadline")
	}
	h.none()
	if n := h.c.series.len(); n != 0 {
		t.Fatal("state committed although nothing was emitted")
	}
}

func TestValidRef(t *testing.T) {
	ok := remediationv1alpha1.ObjectRef{APIVersion: apiApps, Kind: kindDeployment, Namespace: testNS, Name: web, UID: "u"}
	if !validRef(ok) {
		t.Fatalf("valid reference refused: %+v", ok)
	}
	bad := []func(*remediationv1alpha1.ObjectRef){
		func(r *remediationv1alpha1.ObjectRef) { r.APIVersion = "" },
		func(r *remediationv1alpha1.ObjectRef) { r.Kind = "deployment" },
		func(r *remediationv1alpha1.ObjectRef) { r.Name = "" },
		func(r *remediationv1alpha1.ObjectRef) { r.Namespace = "Shop" },
		func(r *remediationv1alpha1.ObjectRef) { r.UID = "" },
	}
	for i, mod := range bad {
		r := ok
		mod(&r)
		if validRef(r) {
			t.Errorf("case %d: invalid reference accepted: %+v", i, r)
		}
	}
}

// A cached owner is reused only for the same reference: a pod that copies a real ReplicaSet's UID under
// another name is not correlated through the cache entry of the real one.
func TestOwnerCacheNeedsTheWholeReference(t *testing.T) {
	d, rs, pod := workload()
	forged := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: intruder, Namespace: testNS, UID: "2a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d",
		OwnerReferences: controllerRef(apiApps, kindReplicaSet, "not-"+webRS, rsUID),
	}}
	h := newHarness(t, Config{}, d, rs, pod, forged)
	genuine := warning("real", 0)
	forgedEv := warning("forged", 0)
	forgedEv.Regarding.Name, forgedEv.Regarding.UID = forged.Name, forged.UID
	h.create(genuine)
	h.create(forgedEv)
	h.reconcileEvent(genuine.Name)
	subjectIs(t, h.one().Subject, kindDeployment, web, deployUID)
	h.reconcileEvent(forgedEv.Name)
	subjectIs(t, h.one().Subject, kindPod, forged.Name, forged.UID)
}

// A forged reference that copies a real ReplicaSet's name and UID under another API version is not
// followed, and does not keep real pods of that ReplicaSet from being correlated.
func TestForgedAPIVersionDoesNotPoisonTheCache(t *testing.T) {
	d, rs, pod := workload()
	forged := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: intruder, Namespace: testNS, UID: "2a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", Labels: pod.Labels,
		OwnerReferences: controllerRef("example.com/v1", kindReplicaSet, webRS, rsUID),
	}}
	h := newHarness(t, Config{}, d, rs, pod, forged)
	forgedEv := warning("forged", 0)
	forgedEv.Regarding.Name, forgedEv.Regarding.UID = forged.Name, forged.UID
	h.create(forgedEv)
	h.reconcileEvent(forgedEv.Name)
	subjectIs(t, h.one().Subject, kindPod, forged.Name, forged.UID)
	genuine := warning("real", 0)
	h.create(genuine)
	h.reconcileEvent(genuine.Name)
	subjectIs(t, h.one().Subject, kindDeployment, web, deployUID)
}

// An owner that could not be verified is read again soon: a pod seen a moment before its ReplicaSet is
// readable is correlated a few seconds later, not after the whole TTL.
func TestUnverifiedOwnerIsCachedBriefly(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{OwnerTTL: time.Hour}, pod)
	first := warning("first", 0)
	h.create(first)
	h.reconcileEvent(first.Name)
	subjectIs(t, h.one().Subject, kindPod, webPod, podUID)
	h.create(d)
	h.create(rs)

	h.clock.add(ownerMissTTL + time.Second)
	later := warning("later", 0)
	h.create(later)
	h.reconcileEvent(later.Name)
	subjectIs(t, h.one().Subject, kindDeployment, web, deployUID)
}

// A ReplicaSet without a Deployment is its own subject, and then the owner is empty, as for any object that
// is its own subject.
func TestStandaloneReplicaSetHasNoOwner(t *testing.T) {
	_, rs, _ := workload()
	rs.OwnerReferences = nil
	h := newHarness(t, Config{}, rs)
	reg := corev1.ObjectReference{APIVersion: apiApps, Kind: kindReplicaSet, Name: webRS, UID: rsUID}
	subject, owner, ok, err := h.c.owners.subjectOf(t.Context(), testNS, reg)
	if err != nil || !ok {
		t.Fatalf("subjectOf: %v, %v", ok, err)
	}
	subjectIs(t, subject, kindReplicaSet, webRS, rsUID)
	if owner != "" {
		t.Fatalf("owner = %q, want empty for an object that is its own subject", owner)
	}
}

// An event about a pod that is gone reaches the pod's workload when the collector looked up the pod's owner
// while the pod existed.
func TestEventAfterThePodIsGone(t *testing.T) {
	d, rs, pod := workload()
	withTermination(pod, "containerd://gone", ReasonOOMKilled, 137, t0)
	h := newHarness(t, Config{}, d, rs, pod)
	h.reconcilePod(pod.Name)
	subjectIs(t, h.one().Subject, kindDeployment, web, deployUID)
	if err := h.cl.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	ev := warning("late", 0)
	h.create(ev)
	h.reconcileEvent(ev.Name)
	subjectIs(t, h.one().Subject, kindDeployment, web, deployUID)

	// A pod the collector never looked up stays its own subject.
	const otherPod, otherUID = "web-7d4b9c8f6-zzzzz", "3a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	other := warning("unknown", 0)
	other.Regarding.Name, other.Regarding.UID = otherPod, otherUID
	h.create(other)
	h.reconcileEvent(other.Name)
	subjectIs(t, h.one().Subject, kindPod, otherPod, otherUID)
}
