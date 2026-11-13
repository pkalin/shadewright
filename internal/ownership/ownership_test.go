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

package ownership

import (
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestSelectorBounds(t *testing.T) {
	big := map[string]string{}
	for i := range MaxSelectorTerms + 1 {
		big[fmt.Sprintf("k%d", i)] = "v"
	}
	for name, ls := range map[string]*metav1.LabelSelector{
		"nil":       nil,
		"empty":     {},
		"too large": {MatchLabels: big},
		"too many values": {MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: "k", Operator: metav1.LabelSelectorOpIn, Values: make([]string, MaxSelectorTerms),
		}}},
		"invalid": {MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "Near"}}},
	} {
		if _, ok := Selector(ls); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	sel, ok := Selector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}})
	if !ok {
		t.Fatal("refused a plain selector")
	}
	obj := &metav1.ObjectMeta{Labels: map[string]string{"app": "web"}}
	if !Selects(sel, obj) {
		t.Error("does not select a matching object")
	}
	for i := range MaxLabels {
		obj.Labels[fmt.Sprintf("extra%d", i)] = "x"
	}
	if Selects(sel, obj) {
		t.Errorf("selects an object with %d labels", len(obj.Labels))
	}
}

const (
	labelApp  = "app"
	web       = "web"
	rsUID     = "11111111-0000-4000-8000-000000000001"
	deployUID = "11111111-0000-4000-8000-000000000002"
	otherUID  = "99999999-0000-4000-8000-000000000000"
)

func controller(apiVersion, kind, name string, uid types.UID) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: new(true)}}
}

// hop is one pod, its ReplicaSet and the ReplicaSet's Deployment, every hop valid.
func hop() (*corev1.Pod, *appsv1.ReplicaSet, *appsv1.Deployment) {
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: web}}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: web, UID: deployUID, Labels: map[string]string{labelApp: web}},
		Spec:       appsv1.DeploymentSpec{Selector: sel},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-1", UID: rsUID, Labels: map[string]string{labelApp: web},
			OwnerReferences: controller(APIVersionApps, KindDeployment, web, deployUID),
		},
		Spec: appsv1.ReplicaSetSpec{Selector: sel},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "web-1-a", Labels: map[string]string{labelApp: web},
		OwnerReferences: controller(APIVersionApps, KindReplicaSet, "web-1", rsUID),
	}}
	return pod, rs, d
}

// The hop rules of an owner chain, shared by the collector and the verification code: owner references are
// untrusted, so a reference to an object with another UID, a missing object, a kind that is not followed or
// a selector that does not match the child never holds. The collector's end-to-end cases are in
// internal/analyzer/collector/owners_test.go (TestUnverifiedOwnersAreNotTrusted).
func TestOwnerHopIsVerified(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*corev1.Pod, *appsv1.ReplicaSet, *appsv1.Deployment)
		// rsHolds and dHolds are whether the pod to ReplicaSet and the ReplicaSet to Deployment hops hold.
		rsHolds, dHolds bool
	}{
		{"every hop valid", func(*corev1.Pod, *appsv1.ReplicaSet, *appsv1.Deployment) {}, true, true},
		{"pod claims a ReplicaSet UID that is not the ReplicaSet's", func(p *corev1.Pod, _ *appsv1.ReplicaSet, _ *appsv1.Deployment) {
			p.OwnerReferences[0].UID = otherUID
		}, false, true},
		{"ReplicaSet claims a Deployment UID that is not the Deployment's", func(_ *corev1.Pod, _ *appsv1.ReplicaSet, d *appsv1.Deployment) {
			d.UID = otherUID
		}, true, false},
		{"owner kind that is not followed", func(p *corev1.Pod, _ *appsv1.ReplicaSet, _ *appsv1.Deployment) {
			p.OwnerReferences[0].Kind = "DaemonSet"
		}, false, true},
		{"owner in another API group", func(p *corev1.Pod, _ *appsv1.ReplicaSet, _ *appsv1.Deployment) {
			p.OwnerReferences[0].APIVersion = "example.com/v1"
		}, false, true},
		{"pod whose labels the ReplicaSet does not select", func(p *corev1.Pod, _ *appsv1.ReplicaSet, _ *appsv1.Deployment) {
			p.Labels = map[string]string{labelApp: "intruder"}
		}, false, true},
		{"owner reference not marked controller", func(p *corev1.Pod, _ *appsv1.ReplicaSet, _ *appsv1.Deployment) {
			p.OwnerReferences[0].Controller = nil
		}, false, true},
		{"ReplicaSet whose labels the Deployment does not select", func(_ *corev1.Pod, _ *appsv1.ReplicaSet, d *appsv1.Deployment) {
			d.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: "other"}}
		}, true, false},
		{"ReplicaSet with an empty selector", func(_ *corev1.Pod, rs *appsv1.ReplicaSet, _ *appsv1.Deployment) {
			rs.Spec.Selector = &metav1.LabelSelector{}
		}, false, true},
		{"ReplicaSet with another name than the reference", func(_ *corev1.Pod, rs *appsv1.ReplicaSet, _ *appsv1.Deployment) {
			rs.Name = "web-2"
		}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod, rs, d := hop()
			c.mod(pod, rs, d)
			rsHolds, dHolds := false, false
			if ref := PodController(pod); ref != nil {
				if o, ok := Verify(*ref, rs, rs.Spec.Selector); ok && o.Controls(pod) {
					rsHolds = true
				}
			}
			// The second hop starts at the ReplicaSet's own controller reference, whatever the first gave.
			if parent := metav1.GetControllerOf(rs); parent != nil {
				if o, ok := Verify(*parent, d, d.Spec.Selector); ok && o.Controls(rs) {
					dHolds = true
				}
			}
			if rsHolds != c.rsHolds || dHolds != c.dHolds {
				t.Fatalf("pod to ReplicaSet holds = %v, want %v; ReplicaSet to Deployment holds = %v, want %v",
					rsHolds, c.rsHolds, dHolds, c.dHolds)
			}
		})
	}
}

func rsRef(rs *appsv1.ReplicaSet) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: APIVersionApps, Kind: KindReplicaSet, Name: rs.Name, UID: rs.UID}
}

func TestVerifyRefusesNilAndLargeOwners(t *testing.T) {
	_, rs, _ := hop()
	o, ok := Verify(rsRef(rs), rs, rs.Spec.Selector)
	if !ok || o.Parent == nil || o.Parent.UID != deployUID {
		t.Fatalf("a ReplicaSet's Deployment is not its parent: %+v, %v", o, ok)
	}
	rs.OwnerReferences[0].Kind = KindStatefulSet
	if o, _ := Verify(rsRef(rs), rs, rs.Spec.Selector); o.Parent != nil {
		t.Error("a ReplicaSet's controller that is not a Deployment is its parent")
	}
	if _, ok := Verify(rsRef(rs), nil, rs.Spec.Selector); ok {
		t.Error("a missing owner holds")
	}
	for i := range MaxLabels + 1 {
		rs.Labels[fmt.Sprintf("k%d", i)] = "v"
	}
	if _, ok := Verify(rsRef(rs), rs, rs.Spec.Selector); ok {
		t.Errorf("an owner with %d labels holds", len(rs.Labels))
	}
}

func TestPodControllerNamesOnlyPodControllers(t *testing.T) {
	pod, _, _ := hop()
	if PodController(pod) == nil {
		t.Fatal("a ReplicaSet controller was not returned")
	}
	pod.OwnerReferences[0].Kind = KindStatefulSet
	if PodController(pod) == nil {
		t.Fatal("a StatefulSet controller was not returned")
	}
	pod.OwnerReferences[0].Kind = KindDeployment
	if PodController(pod) != nil {
		t.Error("a pod naming a Deployment directly has a controller")
	}
}
