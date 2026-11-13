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

// Package ownership holds the rules Shadewright applies when it follows the owner references and label
// selectors of workloads: the bounds on labels and selectors, and the check of one hop of an owner chain (Pod
// to ReplicaSet or StatefulSet, ReplicaSet to Deployment). Owner references are untrusted input, so a hop
// holds only when the owner, read by the caller, exists with the referenced UID and its selector matches the
// object below it. The analyzer's collector correlates an event to its owning workload with these rules, the
// context builder (internal/incidentcontext) resolves a workload's pods with the bounds, and the verification
// code (internal/verification/clearing) proves that a pod belongs to the rollout it judges with the same hop
// check, so all of them follow the same objects and refuse the same ones. Real workloads have a few labels
// and a short selector; anything larger is not followed, so a cache or a context cannot be filled with large
// objects. The package reads nothing itself: callers pass the objects they read.
package ownership

import (
	"maps"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	// MaxLabels bounds the labels of an object that is followed.
	MaxLabels = 64
	// MaxSelectorTerms bounds the terms of a selector that is followed: each matchLabels entry, and each
	// matchExpressions entry plus its values.
	MaxSelectorTerms = 64
)

// The kinds an owner chain follows, all in API version apps/v1.
const (
	// APIVersionApps is the only API version an owner reference is followed in.
	APIVersionApps = "apps/v1"
	// KindReplicaSet is the controller of a Deployment's pods.
	KindReplicaSet = "ReplicaSet"
	// KindDeployment is reached only as the controller of a ReplicaSet.
	KindDeployment = "Deployment"
	// KindStatefulSet is the controller of its pods.
	KindStatefulSet = "StatefulSet"
)

// Followed reports whether ref names a kind an owner chain follows: an apps/v1 ReplicaSet, Deployment or
// StatefulSet. A reference to any other kind or API version cannot be verified and is never read.
func Followed(ref metav1.OwnerReference) bool {
	if ref.APIVersion != APIVersionApps {
		return false
	}
	switch ref.Kind {
	case KindReplicaSet, KindDeployment, KindStatefulSet:
		return true
	}
	return false
}

// PodController returns the controller reference of a pod when it names an apps/v1 ReplicaSet or StatefulSet,
// the controllers of pods, and nil otherwise. Only a reference marked controller counts. A Deployment is
// reached only as the controller of a ReplicaSet, never named by a pod directly.
func PodController(pod metav1.Object) *metav1.OwnerReference {
	ref := metav1.GetControllerOfNoCopy(pod)
	if ref == nil || ref.APIVersion != APIVersionApps || (ref.Kind != KindReplicaSet && ref.Kind != KindStatefulSet) {
		return nil
	}
	c := *ref
	return &c
}

// Owner is what one read of an owner established once [Verify] accepted it.
type Owner struct {
	// Selector selects the objects the owner controls: pods, or for a Deployment its ReplicaSets.
	Selector labels.Selector
	// Labels are the owner's own labels, which its parent's selector must match.
	Labels labels.Set
	// Parent is the controller reference of a ReplicaSet to an apps/v1 Deployment, when it has one; nil for
	// every other kind.
	Parent *metav1.OwnerReference
}

// Controls reports whether the owner's selector matches the labels of child, the object that holds the
// reference, refusing a child with more than [MaxLabels] labels.
func (o Owner) Controls(child metav1.Object) bool {
	return o.Selector != nil && Selects(o.Selector, child)
}

// Verify checks that obj, read by the caller from the namespace and name of ref, is the object ref names: a
// followed kind, the referenced UID, at most [MaxLabels] labels, and a selector (sel, the object's
// spec.selector) that [Selector] accepts. It returns what the hop established, and false when it does not
// hold. A nil obj (not found) never holds. The selector's match against the child is checked separately with
// [Owner.Controls], since a ReplicaSet's labels are matched by its Deployment's selector one hop up.
func Verify(ref metav1.OwnerReference, obj metav1.Object, sel *metav1.LabelSelector) (Owner, bool) {
	if !Followed(ref) || obj == nil || obj.GetUID() != ref.UID || obj.GetName() != ref.Name ||
		len(obj.GetLabels()) > MaxLabels {
		return Owner{}, false
	}
	selector, ok := Selector(sel)
	if !ok {
		return Owner{}, false
	}
	o := Owner{Selector: selector, Labels: labels.Set(maps.Clone(obj.GetLabels()))}
	if ref.Kind == KindReplicaSet {
		if p := metav1.GetControllerOfNoCopy(obj); p != nil && p.APIVersion == APIVersionApps && p.Kind == KindDeployment {
			parent := *p
			o.Parent = &parent
		}
	}
	return o, true
}

// Selector converts a workload's selector. It refuses an empty one, which would select everything (the
// apps/v1 API refuses it too), one with more than [MaxSelectorTerms] terms, and one that does not convert.
func Selector(ls *metav1.LabelSelector) (labels.Selector, bool) {
	if ls == nil || len(ls.MatchLabels)+len(ls.MatchExpressions) == 0 {
		return nil, false
	}
	terms := len(ls.MatchLabels)
	for _, e := range ls.MatchExpressions {
		terms += 1 + len(e.Values)
	}
	if terms > MaxSelectorTerms {
		return nil, false
	}
	s, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		return nil, false
	}
	return s, true
}

// Selects reports whether sel matches the object's labels, refusing an object with more than [MaxLabels].
func Selects(sel labels.Selector, obj metav1.Object) bool {
	l := obj.GetLabels()
	return len(l) <= MaxLabels && sel.Matches(labels.Set(l))
}
