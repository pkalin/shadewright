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
	"maps"
	"regexp"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/ownership"
	"github.com/pkalin/shadewright/internal/groupversion"
)

const (
	kindPod         = "Pod"
	kindReplicaSet  = "ReplicaSet"
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	apiCore         = "v1"
	apiApps         = "apps/v1"
)

// namespacedKinds are the kinds whose regarding reference may omit the namespace, meaning the event's.
var namespacedKinds = map[string]bool{kindPod: true, kindReplicaSet: true, kindDeployment: true, kindStatefulSet: true}

// The ObjectRef patterns of the Incident schema (api/v1alpha1/common_types.go), checked here so that an
// observation always fits the Incident it becomes.
var (
	// The apiVersion pattern is in package groupversion, shared with the module registry.
	kindRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	// reasonRe is the CamelCase form every Kubernetes component uses for reasons.
	reasonRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	// controllerRe is a component name such as kubelet, default-scheduler or example.com/operator. It is
	// stored in the Incident unredacted, so free text is refused.
	controllerRe = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_./]*[A-Za-z0-9])?)?$`)
)

// Lengths of the Incident spec fields and of events.k8s.io/v1 fields.
const (
	maxReason     = 128
	maxAction     = 128
	maxController = 253
	maxKind       = 63
)

func validReason(s string) bool { return len(s) <= maxReason && reasonRe.MatchString(s) }

// validAction checks only the length of an event's action, which the API server bounds too: the action is
// hashed into the fingerprint and nothing else, never stored in an observation or an Incident and never
// shown to a model, so its characters do not matter.
func validAction(s string) bool { return len(s) <= maxAction }

func validController(s string) bool { return len(s) <= maxController && controllerRe.MatchString(s) }

// validRef reports whether r fits the Incident's ObjectRef schema and carries a UID, which the fingerprint
// needs.
func validRef(r remediationv1alpha1.ObjectRef) bool {
	if !groupversion.Valid(r.APIVersion) {
		return false
	}
	if len(r.Kind) > maxKind || !kindRe.MatchString(r.Kind) {
		return false
	}
	if len(validation.IsDNS1123Subdomain(r.Name)) > 0 {
		return false
	}
	if r.Namespace != "" && len(validation.IsDNS1123Label(r.Namespace)) > 0 {
		return false
	}
	return r.UID != "" && len(r.UID) <= 128
}

// Bounds on what is kept from an owner or a pod are those of package ownership, shared with the context
// builder: anything larger is not followed, so the caches cannot be filled with large objects.
const (
	maxLabels = ownership.MaxLabels
	// ownerMissTTL is how long a reference that could not be verified (no such object, another UID, a
	// selector out of bounds) is remembered: briefly, so an owner created a moment after its pod is found
	// soon.
	ownerMissTTL = 5 * time.Second
)

// ownerKey identifies one owner reference: a hit needs the same kind, namespace, name and UID, so a forged
// reference that copies a real UID under another name never shares an entry with the real one. The API
// version is always apps/v1: facts refuses any other before it looks at the cache.
type ownerKey struct {
	kind, namespace, name string
	uid                   types.UID
}

// ownerFacts is what one read of an owner established.
type ownerFacts struct {
	// found is true when the object exists with the referenced UID and its selector and labels are within
	// the bounds.
	found bool
	// selector selects the objects the owner controls: pods, or for a Deployment its ReplicaSets.
	selector labels.Selector
	// labels are the owner's own labels, which its parent's selector must match.
	labels labels.Set
	// parent is the controller reference of a ReplicaSet to a Deployment, when it has one.
	parent *metav1.OwnerReference
}

// podOwner is what is remembered about a pod once its owner was looked up, so that an event about the pod
// that arrives after the pod was deleted still reaches the owning workload.
type podOwner struct {
	namespace, name string
	ref             metav1.OwnerReference
	labels          labels.Set
}

// ownerResolver correlates an object to the workload that owns it, following controller owner references
// from Pod to ReplicaSet to Deployment and from Pod to StatefulSet. Owner references are untrusted: every
// hop is read from the API server, must exist with the referenced UID and must select the object below it
// by its labels, otherwise the chain stops at the last verified object. Facts about owners are cached, a
// bounded number, for OwnerTTL when verified and for ownerMissTTL when not.
type ownerResolver struct {
	pods    client.Reader // the manager's cache, which already holds the watched pods
	api     client.Reader // the API reader: one get per owner, no list, no informer
	timeout time.Duration
	ttl     time.Duration
	cache   *cache.LRUExpireCache // ownerKey to ownerFacts; safe for concurrent use

	mu    sync.Mutex
	known *lru[types.UID, podOwner] // pod UID to its controller reference and labels
}

// clockFunc adapts the collector's clock to the expiring cache.
type clockFunc func() time.Time

func (f clockFunc) Now() time.Time { return f() }

func newOwnerResolver(pods, api client.Reader, cfg Config) *ownerResolver {
	return &ownerResolver{
		pods: pods, api: api, timeout: cfg.LookupTimeout, ttl: cfg.OwnerTTL,
		cache: cache.NewLRUExpireCacheWithClock(max(cfg.MaxOwners, 1), clockFunc(cfg.Now)),
		known: newLRU[types.UID, podOwner](cfg.MaxPods, nil),
	}
}

// subjectOf returns the subject of an event's regarding object and the owner UID for the fingerprint
// (empty when the subject is the object itself). It returns false when the reference does not fit the
// Incident schema.
func (r *ownerResolver) subjectOf(ctx context.Context, eventNS string, reg corev1.ObjectReference) (
	remediationv1alpha1.ObjectRef, string, bool, error) {
	self := remediationv1alpha1.ObjectRef{
		APIVersion: reg.APIVersion, Kind: reg.Kind, Namespace: reg.Namespace, Name: reg.Name,
		UID: remediationv1alpha1.UID(reg.UID),
	}
	if self.Namespace == "" && namespacedKinds[self.Kind] {
		self.Namespace = eventNS
	}
	if !validRef(self) {
		return remediationv1alpha1.ObjectRef{}, "", false, nil
	}
	switch {
	case self.Kind == kindPod && self.APIVersion == apiCore:
		var pod corev1.Pod
		err := r.pods.Get(ctx, client.ObjectKey{Namespace: self.Namespace, Name: self.Name}, &pod)
		if apierrors.IsNotFound(err) || (err == nil && pod.UID != reg.UID) {
			// The pod is gone, or replaced by another of the same name: follow the owner it had when it was
			// last looked up, if it was.
			return r.formerPodSubject(ctx, self)
		}
		if err != nil {
			return remediationv1alpha1.ObjectRef{}, "", false, fmt.Errorf("reading the Pod of an event: %w", err)
		}
		return r.podSubject(ctx, &pod)
	case self.Kind == kindReplicaSet && self.APIVersion == apiApps:
		facts, err := r.facts(ctx, self.Namespace, metav1.OwnerReference{
			APIVersion: apiApps, Kind: kindReplicaSet, Name: self.Name, UID: reg.UID,
		})
		if err != nil {
			return remediationv1alpha1.ObjectRef{}, "", false, err
		}
		if !facts.found || facts.parent == nil {
			// A ReplicaSet without a verified Deployment is its own subject, with no owner.
			return self, "", true, nil
		}
		return r.ownedSubject(ctx, self, *facts.parent, facts.labels)
	case (self.Kind == kindDeployment || self.Kind == kindStatefulSet) && self.APIVersion == apiApps:
		// A workload the analyzer can read is the subject only as it exists: the UID in the event is
		// whatever its writer put there.
		facts, err := r.facts(ctx, self.Namespace, metav1.OwnerReference{
			APIVersion: apiApps, Kind: self.Kind, Name: self.Name, UID: reg.UID,
		})
		if err != nil || !facts.found {
			return remediationv1alpha1.ObjectRef{}, "", false, err
		}
		return self, "", true, nil
	}
	// Other kinds the analyzer cannot read, so it cannot check their UID; such a subject is only ever the
	// object the event itself names.
	return self, "", true, nil
}

// podSubject returns the verified owning workload of pod, or the pod itself, and remembers the pod's
// controller reference for events that arrive after the pod is gone.
func (r *ownerResolver) podSubject(ctx context.Context, pod *corev1.Pod) (
	remediationv1alpha1.ObjectRef, string, bool, error) {
	self := remediationv1alpha1.ObjectRef{
		APIVersion: apiCore, Kind: kindPod, Namespace: pod.Namespace, Name: pod.Name,
		UID: remediationv1alpha1.UID(pod.UID),
	}
	if !validRef(self) {
		return remediationv1alpha1.ObjectRef{}, "", false, nil
	}
	// Only a reference marked controller is followed (GetControllerOf ignores the others), and only to a
	// ReplicaSet or a StatefulSet: those are the controllers of pods. A Deployment is reached only as the
	// controller of a ReplicaSet, never named by a pod directly.
	ref := metav1.GetControllerOf(pod)
	if ref == nil || (ref.Kind != kindReplicaSet && ref.Kind != kindStatefulSet) || len(pod.Labels) > maxLabels {
		return self, "", true, nil
	}
	r.mu.Lock()
	r.known.put(pod.UID, podOwner{
		namespace: pod.Namespace, name: pod.Name, ref: *ref, labels: labels.Set(maps.Clone(pod.Labels)),
	})
	r.mu.Unlock()
	return r.ownedSubject(ctx, self, *ref, pod.Labels)
}

// formerPodSubject is podSubject for a pod that is no longer in the cache, from what was remembered about
// it. A pod deleted before the collector looked up its owner stays its own subject.
func (r *ownerResolver) formerPodSubject(ctx context.Context, self remediationv1alpha1.ObjectRef) (
	remediationv1alpha1.ObjectRef, string, bool, error) {
	r.mu.Lock()
	known, ok := r.known.get(types.UID(self.UID))
	r.mu.Unlock()
	if !ok || known.namespace != self.Namespace || known.name != self.Name {
		return self, "", true, nil
	}
	return r.ownedSubject(ctx, self, known.ref, known.labels)
}

// ownedSubject returns the top of the verified chain that starts at ref with its UID as the owner, or self
// with no owner when ref cannot be verified.
func (r *ownerResolver) ownedSubject(ctx context.Context, self remediationv1alpha1.ObjectRef,
	ref metav1.OwnerReference, childLabels labels.Set) (remediationv1alpha1.ObjectRef, string, bool, error) {
	owner, ok, err := r.chain(ctx, self.Namespace, ref, childLabels)
	if err != nil {
		return remediationv1alpha1.ObjectRef{}, "", false, err
	}
	if !ok {
		return self, "", true, nil
	}
	return owner, string(owner.UID), true, nil
}

// chain returns the workload at the top of the verified chain that starts at ref, and false when ref itself
// cannot be verified: an unknown kind, an object that does not exist or has another UID, or a selector that
// does not match childLabels, the labels of the object that holds ref.
func (r *ownerResolver) chain(ctx context.Context, ns string, ref metav1.OwnerReference, childLabels labels.Set) (
	remediationv1alpha1.ObjectRef, bool, error) {
	facts, err := r.facts(ctx, ns, ref)
	if err != nil {
		return remediationv1alpha1.ObjectRef{}, false, err
	}
	if !facts.found || !facts.selector.Matches(childLabels) {
		return remediationv1alpha1.ObjectRef{}, false, nil
	}
	self := remediationv1alpha1.ObjectRef{
		APIVersion: apiApps, Kind: ref.Kind, Namespace: ns, Name: ref.Name, UID: remediationv1alpha1.UID(ref.UID),
	}
	if !validRef(self) {
		return remediationv1alpha1.ObjectRef{}, false, nil
	}
	if facts.parent == nil {
		return self, true, nil
	}
	// Only a ReplicaSet has a parent, and a Deployment has none, so this recurses at most once.
	top, ok, err := r.chain(ctx, ns, *facts.parent, facts.labels)
	if err != nil {
		return remediationv1alpha1.ObjectRef{}, false, err
	}
	if !ok {
		return self, true, nil
	}
	return top, true, nil
}

// facts returns what is known about the referenced owner, from the cache or from one read. References to
// kinds the analyzer cannot read are refused without a read and are not cached.
func (r *ownerResolver) facts(ctx context.Context, ns string, ref metav1.OwnerReference) (ownerFacts, error) {
	if ref.APIVersion != apiApps {
		return ownerFacts{}, nil
	}
	switch ref.Kind {
	case kindReplicaSet, kindDeployment, kindStatefulSet:
	default:
		// DaemonSets, Jobs and anything else: the analyzer has no grant to read them, so the reference
		// cannot be verified and the pod stays the subject.
		return ownerFacts{}, nil
	}
	key := ownerKey{kind: ref.Kind, namespace: ns, name: ref.Name, uid: ref.UID}
	if v, ok := r.cache.Get(key); ok {
		return v.(ownerFacts), nil
	}
	facts, err := r.read(ctx, ns, ref)
	if err != nil {
		return ownerFacts{}, err
	}
	ttl := r.ttl
	if !facts.found {
		ttl = ownerMissTTL
	}
	r.cache.Add(key, facts, ttl)
	return facts, nil
}

// read gets the referenced object with a deadline (a get, never a list or a watch) and keeps only the
// result of its UID check, its selector, its labels and, for a ReplicaSet, its controller reference to a
// Deployment. The whole object, pod template included, is decoded for the moment of the read, because
// metadata alone has no selector; nothing else of it is kept. A NotFound is final for ownerMissTTL: the
// read goes to the API server, not to a cache, and a controller exists before the pods it creates, so a
// missing owner is not a lag to wait out.
func (r *ownerResolver) read(ctx context.Context, ns string, ref metav1.OwnerReference) (ownerFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var (
		obj client.Object
		sel func() *metav1.LabelSelector
	)
	switch ref.Kind {
	case kindReplicaSet:
		o := &appsv1.ReplicaSet{}
		obj, sel = o, func() *metav1.LabelSelector { return o.Spec.Selector }
	case kindDeployment:
		o := &appsv1.Deployment{}
		obj, sel = o, func() *metav1.LabelSelector { return o.Spec.Selector }
	default:
		o := &appsv1.StatefulSet{}
		obj, sel = o, func() *metav1.LabelSelector { return o.Spec.Selector }
	}
	if err := r.api.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return ownerFacts{}, nil
		}
		return ownerFacts{}, fmt.Errorf("reading the owner of a Pod: %w", err)
	}
	if obj.GetUID() != ref.UID || len(obj.GetLabels()) > maxLabels {
		return ownerFacts{}, nil
	}
	selector, ok := ownership.Selector(sel())
	if !ok {
		return ownerFacts{}, nil
	}
	facts := ownerFacts{found: true, selector: selector, labels: labels.Set(maps.Clone(obj.GetLabels()))}
	if ref.Kind == kindReplicaSet {
		if p := metav1.GetControllerOfNoCopy(obj); p != nil && p.APIVersion == apiApps && p.Kind == kindDeployment {
			parent := *p
			facts.parent = &parent
		}
	}
	return facts, nil
}
