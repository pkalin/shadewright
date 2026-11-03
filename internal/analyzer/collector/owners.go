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
	"regexp"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
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
	apiVersionRe = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?v[0-9]+((alpha|beta)[0-9]+)?$`)
	kindRe       = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
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
	maxAPIVersion = 253
	maxKind       = 63
)

func validReason(s string) bool     { return len(s) <= maxReason && reasonRe.MatchString(s) }
func validAction(s string) bool     { return len(s) <= maxAction }
func validController(s string) bool { return len(s) <= maxController && controllerRe.MatchString(s) }

// validRef reports whether r fits the Incident's ObjectRef schema and carries a UID, which the fingerprint
// needs.
func validRef(r remediationv1alpha1.ObjectRef) bool {
	if len(r.APIVersion) > maxAPIVersion || !apiVersionRe.MatchString(r.APIVersion) {
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

// ownerKey identifies one owner reference: a hit needs the same kind, namespace, name and UID, so a forged
// reference with a real UID but another name is not served from the cache.
type ownerKey struct {
	kind, namespace, name string
	uid                   types.UID
}

type ownerEntry struct {
	ref      remediationv1alpha1.ObjectRef
	verified bool
	expires  time.Time
}

// ownerResolver correlates an object to the workload that owns it, following controller owner references
// from Pod to ReplicaSet to Deployment and from Pod to StatefulSet. Owner references are untrusted: every
// hop is read from the API server and must have the referenced UID, otherwise the chain stops at the last
// verified object. Results are cached, at most a fixed number for a fixed time.
type ownerResolver struct {
	pods    client.Reader // the manager's cache, which already holds the watched pods
	api     client.Reader // the API reader: one metadata read per owner, no informer
	timeout time.Duration
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache *lru[ownerKey, ownerEntry]
}

func newOwnerResolver(pods, api client.Reader, cfg Config) *ownerResolver {
	return &ownerResolver{
		pods: pods, api: api, timeout: cfg.LookupTimeout, ttl: cfg.OwnerTTL, now: cfg.Now,
		cache: newLRU[ownerKey, ownerEntry](cfg.MaxOwners, nil),
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
			return self, "", true, nil
		}
		if err != nil {
			return remediationv1alpha1.ObjectRef{}, "", false, fmt.Errorf("reading the Pod of an event: %w", err)
		}
		return r.podSubject(ctx, &pod)
	case self.Kind == kindReplicaSet && self.APIVersion == apiApps:
		owner, ok, err := r.resolve(ctx, self.Namespace, metav1.OwnerReference{
			APIVersion: apiApps, Kind: kindReplicaSet, Name: self.Name, UID: reg.UID,
		})
		if err != nil {
			return remediationv1alpha1.ObjectRef{}, "", false, err
		}
		if !ok {
			return self, "", true, nil
		}
		return owner, string(owner.UID), true, nil
	}
	return self, "", true, nil
}

// podSubject returns the verified owning workload of pod, or the pod itself.
func (r *ownerResolver) podSubject(ctx context.Context, pod *corev1.Pod) (
	remediationv1alpha1.ObjectRef, string, bool, error) {
	self := remediationv1alpha1.ObjectRef{
		APIVersion: apiCore, Kind: kindPod, Namespace: pod.Namespace, Name: pod.Name,
		UID: remediationv1alpha1.UID(pod.UID),
	}
	if !validRef(self) {
		return remediationv1alpha1.ObjectRef{}, "", false, nil
	}
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return self, "", true, nil
	}
	owner, ok, err := r.resolve(ctx, pod.Namespace, *ref)
	if err != nil {
		return remediationv1alpha1.ObjectRef{}, "", false, err
	}
	if !ok {
		return self, "", true, nil
	}
	return owner, string(owner.UID), true, nil
}

// resolve returns the workload at the top of the verified chain that starts at ref, and false when ref
// itself cannot be verified: an unknown kind, an object that does not exist, or one with another UID.
func (r *ownerResolver) resolve(ctx context.Context, ns string, ref metav1.OwnerReference) (
	remediationv1alpha1.ObjectRef, bool, error) {
	key := ownerKey{kind: ref.Kind, namespace: ns, name: ref.Name, uid: ref.UID}
	r.mu.Lock()
	e, hit := r.cache.get(key)
	r.mu.Unlock()
	if hit && r.now().Before(e.expires) {
		return e.ref, e.verified, nil
	}
	ownerRef, verified, err := r.lookup(ctx, ns, ref)
	if err != nil {
		return remediationv1alpha1.ObjectRef{}, false, err
	}
	r.mu.Lock()
	r.cache.put(key, ownerEntry{ref: ownerRef, verified: verified, expires: r.now().Add(r.ttl)})
	r.mu.Unlock()
	return ownerRef, verified, nil
}

func (r *ownerResolver) lookup(ctx context.Context, ns string, ref metav1.OwnerReference) (
	remediationv1alpha1.ObjectRef, bool, error) {
	if ref.APIVersion != apiApps {
		return remediationv1alpha1.ObjectRef{}, false, nil
	}
	switch ref.Kind {
	case kindReplicaSet, kindDeployment, kindStatefulSet:
	default:
		// DaemonSets, Jobs and anything else: the analyzer has no grant to read them, so the reference
		// cannot be verified and the pod stays the subject.
		return remediationv1alpha1.ObjectRef{}, false, nil
	}
	obj, ok, err := r.read(ctx, ns, ref)
	if err != nil || !ok {
		return remediationv1alpha1.ObjectRef{}, false, err
	}
	self := remediationv1alpha1.ObjectRef{
		APIVersion: apiApps, Kind: ref.Kind, Namespace: ns, Name: ref.Name, UID: remediationv1alpha1.UID(ref.UID),
	}
	if !validRef(self) {
		return remediationv1alpha1.ObjectRef{}, false, nil
	}
	if ref.Kind != kindReplicaSet {
		return self, true, nil
	}
	parent := metav1.GetControllerOfNoCopy(obj)
	if parent == nil || parent.APIVersion != apiApps || parent.Kind != kindDeployment {
		return self, true, nil
	}
	top, ok, err := r.resolve(ctx, ns, *parent)
	if err != nil {
		return remediationv1alpha1.ObjectRef{}, false, err
	}
	if !ok {
		return self, true, nil
	}
	return top, true, nil
}

// read gets the metadata of the referenced object with a deadline and checks its UID.
func (r *ownerResolver) read(ctx context.Context, ns string, ref metav1.OwnerReference) (
	*metav1.PartialObjectMetadata, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	obj := &metav1.PartialObjectMetadata{}
	obj.SetGroupVersionKind(appsGVK(ref.Kind))
	if err := r.api.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading the owner of a Pod: %w", err)
	}
	return obj, obj.UID == ref.UID, nil
}

func appsGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: kind}
}
