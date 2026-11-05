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

package incidentcontext

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/ownership"
)

const (
	apiCore         = "v1"
	apiApps         = "apps/v1"
	kindPod         = "Pod"
	kindReplicaSet  = "ReplicaSet"
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"

	// revisionLabel is the label a StatefulSet puts on its pods with the revision that made them.
	revisionLabel = "controller-revision-hash"
)

// Bounds of the pod resolution.
const (
	// MaxPods bounds the pods of the target the context describes, most relevant first. It equals the most
	// pods the metrics source takes in one query.
	MaxPods = 50
	// MaxReplicaSets bounds the ReplicaSets of a Deployment the builder reads (one get each). A rollout has a
	// few with pods; the bound leaves room for ReplicaSets that pods name falsely.
	MaxReplicaSets = 16
)

// targetState says how far the target was resolved.
type targetState int

const (
	// targetResolved: the target exists with its UID, and pods holds its current pods.
	targetResolved targetState = iota
	// targetGone: the target no longer exists, or exists under another UID.
	targetGone
	// targetUnsupported: the analyzer cannot read objects of the target's kind, or the reference is incomplete.
	targetUnsupported
	// targetFailed: a read failed; the status source could not be fetched.
	targetFailed
)

// target is what the builder learned about the Incident's subject.
type target struct {
	ref   remediationv1alpha1.ObjectRef
	state targetState
	// workload is the target object when it is a workload (Deployment, StatefulSet or ReplicaSet).
	workload client.Object
	// replicaSets are the verified ReplicaSets of a Deployment target that control a pod in pods.
	replicaSets []*appsv1.ReplicaSet
	// pods are the target's current pods, most relevant first, at most MaxPods.
	pods []corev1.Pod
	// total is the number of the target's current pods found, before the bound.
	total int
	// uncertain means a pod was created inside the window or the workload rolled within it.
	uncertain bool
	// capped means pods or ReplicaSets were left out.
	capped bool
	// outOfBounds means the workload exists but has more labels, or a larger (or empty) selector, than the
	// builder follows (package ownership), so its pods are not resolved. The workload is not gone.
	outOfBounds bool
}

// resolve reads the target and its current pods. Owner references are untrusted: each hop is read, must
// carry the referenced UID and must select the object below it.
func (b *Builder) resolve(ctx context.Context, ref remediationv1alpha1.ObjectRef, w window) target {
	t := target{ref: ref}
	if ref.UID == "" || ref.Namespace == "" || ref.Name == "" {
		t.state = targetUnsupported
		return t
	}
	var err error
	switch {
	case ref.APIVersion == apiApps && ref.Kind == kindDeployment:
		err = b.resolveDeployment(ctx, &t, w)
	case ref.APIVersion == apiApps && ref.Kind == kindStatefulSet:
		err = b.resolveStatefulSet(ctx, &t)
	case ref.APIVersion == apiApps && ref.Kind == kindReplicaSet:
		err = b.resolveReplicaSet(ctx, &t)
	case ref.APIVersion == apiCore && ref.Kind == kindPod:
		err = b.resolvePod(ctx, &t)
	default:
		t.state = targetUnsupported
		return t
	}
	if err != nil {
		t = target{ref: ref, state: targetFailed}
		return t
	}
	if t.state != targetResolved {
		return t
	}
	// A Pod subject is matched by its UID, so its pod set is exact; a workload's current pods may differ from
	// those it had during the window.
	if ref.Kind != kindPod {
		for i := range t.pods {
			if t.pods[i].CreationTimestamp.After(w.start) {
				t.uncertain = true
			}
		}
	}
	t.pods = sortPods(t.pods)
	t.total = len(t.pods)
	if len(t.pods) > MaxPods {
		t.pods, t.capped = t.pods[:MaxPods], true
	}
	return t
}

// workloadInBounds reports whether the workload's labels are within the bound; otherwise it marks the target
// out of bounds.
func workloadInBounds(t *target, obj client.Object) bool {
	if len(obj.GetLabels()) > ownership.MaxLabels {
		t.outOfBounds = true
		return false
	}
	return true
}

// get reads one object with the builder's timeout and reports whether it exists with the given UID.
func (b *Builder) get(ctx context.Context, r client.Reader, ns, name string, uid types.UID, obj client.Object,
	kind string) (bool, error) {
	err := b.call(ctx, func(ctx context.Context) error {
		return r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj)
	})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading a %s: %w", kind, err)
	}
	return obj.GetUID() == uid, nil
}

// listPods lists the pods of a namespace that the workload's selector matches, from the cache. Each is
// checked again with ownership.Selects, which also bounds its labels.
func (b *Builder) listPods(ctx context.Context, ns string, sel labels.Selector) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := b.call(ctx, func(ctx context.Context) error {
		return b.pods.List(ctx, &list, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel})
	}); err != nil {
		return nil, fmt.Errorf("listing the pods of an Incident's namespace: %w", err)
	}
	return list.Items, nil
}

// controlledBy returns the pod's controller reference when it names an apps/v1 object of the kind.
func controlledBy(obj metav1.Object, kind string) *metav1.OwnerReference {
	ref := metav1.GetControllerOfNoCopy(obj)
	if ref == nil || ref.APIVersion != apiApps || ref.Kind != kind {
		return nil
	}
	return ref
}

// selects reports whether sel matches the object's labels, within the bound on labels.
func selects(sel labels.Selector, obj metav1.Object) bool {
	return ownership.Selects(sel, obj)
}

// selectorOf converts a workload's selector within the bounds, or marks the target out of bounds.
func selectorOf(t *target, ls *metav1.LabelSelector) (labels.Selector, bool) {
	sel, ok := ownership.Selector(ls)
	if !ok {
		t.outOfBounds = true
	}
	return sel, ok
}

func (b *Builder) resolveDeployment(ctx context.Context, t *target, w window) error {
	var d appsv1.Deployment
	found, err := b.get(ctx, b.objects, t.ref.Namespace, t.ref.Name, types.UID(t.ref.UID), &d, kindDeployment)
	if err != nil {
		return err
	}
	if !found {
		t.state = targetGone
		return nil
	}
	t.workload = &d
	if !workloadInBounds(t, &d) {
		return nil
	}
	dsel, ok := selectorOf(t, d.Spec.Selector)
	if !ok {
		return nil
	}
	pods, err := b.listPods(ctx, d.Namespace, dsel)
	if err != nil {
		return err
	}
	// The ReplicaSets the selected pods name as their controller, each to be read once. Those named like the
	// Deployment's own come first, then those named by the most pods, so that a few pods naming made-up
	// ReplicaSets cannot crowd the real ones out of the bound; the name and the count only order the reads,
	// the UID and the owner reference decide. Whoever can create pods in the namespace can still fill the
	// bound with enough of them; the pod set is then labelled capped.
	type rsKey struct {
		name string
		uid  types.UID
	}
	counts := map[rsKey]int{}
	var keys []rsKey
	for i := range pods {
		if ref := controlledBy(&pods[i], kindReplicaSet); ref != nil && selects(dsel, &pods[i]) {
			k := rsKey{ref.Name, ref.UID}
			if counts[k] == 0 {
				keys = append(keys, k)
			}
			counts[k]++
		}
	}
	prefix := d.Name + "-"
	slices.SortFunc(keys, func(a, b rsKey) int {
		return cmp.Or(
			-cmp.Compare(boolInt(strings.HasPrefix(a.name, prefix)), boolInt(strings.HasPrefix(b.name, prefix))),
			-cmp.Compare(counts[a], counts[b]), cmp.Compare(a.name, b.name), cmp.Compare(a.uid, b.uid))
	})
	if len(keys) > MaxReplicaSets {
		keys, t.capped = keys[:MaxReplicaSets], true
	}
	type verified struct {
		rs  *appsv1.ReplicaSet
		sel labels.Selector
	}
	sets := map[rsKey]verified{}
	for _, k := range keys {
		rs := &appsv1.ReplicaSet{}
		found, err := b.get(ctx, b.objects, d.Namespace, k.name, k.uid, rs, kindReplicaSet)
		if err != nil {
			return err
		}
		owner := controlledBy(rs, kindDeployment)
		if !found || owner == nil || owner.UID != d.UID || !selects(dsel, rs) {
			continue
		}
		if sel, ok := ownership.Selector(rs.Spec.Selector); ok {
			sets[k] = verified{rs, sel}
		}
	}
	used := map[rsKey]bool{}
	for i := range pods {
		ref := controlledBy(&pods[i], kindReplicaSet)
		if ref == nil {
			continue
		}
		k := rsKey{ref.Name, ref.UID}
		if v, ok := sets[k]; ok && selects(v.sel, &pods[i]) && selects(dsel, &pods[i]) {
			t.pods = append(t.pods, pods[i])
			used[k] = true
		}
	}
	for _, k := range keys {
		if used[k] {
			rs := sets[k].rs
			t.replicaSets = append(t.replicaSets, rs)
			if rs.CreationTimestamp.After(w.start) {
				t.uncertain = true
			}
		}
	}
	if len(t.replicaSets) > 1 {
		t.uncertain = true
	}
	return nil
}

func (b *Builder) resolveStatefulSet(ctx context.Context, t *target) error {
	var s appsv1.StatefulSet
	found, err := b.get(ctx, b.objects, t.ref.Namespace, t.ref.Name, types.UID(t.ref.UID), &s, kindStatefulSet)
	if err != nil {
		return err
	}
	if !found {
		t.state = targetGone
		return nil
	}
	t.workload = &s
	if !workloadInBounds(t, &s) {
		return nil
	}
	sel, ok := selectorOf(t, s.Spec.Selector)
	if !ok {
		return nil
	}
	pods, err := b.listPods(ctx, s.Namespace, sel)
	if err != nil {
		return err
	}
	revisions := map[string]bool{}
	for i := range pods {
		if ref := controlledBy(&pods[i], kindStatefulSet); ref != nil && ref.UID == s.UID && selects(sel, &pods[i]) {
			t.pods = append(t.pods, pods[i])
			revisions[pods[i].Labels[revisionLabel]] = true
		}
	}
	if len(revisions) > 1 || (s.Status.UpdateRevision != "" && s.Status.CurrentRevision != s.Status.UpdateRevision) {
		t.uncertain = true
	}
	return nil
}

func (b *Builder) resolveReplicaSet(ctx context.Context, t *target) error {
	var rs appsv1.ReplicaSet
	found, err := b.get(ctx, b.objects, t.ref.Namespace, t.ref.Name, types.UID(t.ref.UID), &rs, kindReplicaSet)
	if err != nil {
		return err
	}
	if !found {
		t.state = targetGone
		return nil
	}
	t.workload = &rs
	if !workloadInBounds(t, &rs) {
		return nil
	}
	sel, ok := selectorOf(t, rs.Spec.Selector)
	if !ok {
		return nil
	}
	pods, err := b.listPods(ctx, rs.Namespace, sel)
	if err != nil {
		return err
	}
	for i := range pods {
		if ref := controlledBy(&pods[i], kindReplicaSet); ref != nil && ref.UID == rs.UID && selects(sel, &pods[i]) {
			t.pods = append(t.pods, pods[i])
		}
	}
	return nil
}

func (b *Builder) resolvePod(ctx context.Context, t *target) error {
	var p corev1.Pod
	found, err := b.get(ctx, b.pods, t.ref.Namespace, t.ref.Name, types.UID(t.ref.UID), &p, kindPod)
	if err != nil {
		return err
	}
	if !found {
		t.state = targetGone
		return nil
	}
	t.pods = []corev1.Pod{p}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// trouble scores a pod: the more restarts, waiting or failed containers and unready containers, the more
// relevant it is to the incident.
type trouble struct {
	broken   int   // containers waiting with a reason, terminated with an error, or restarted
	restarts int32 // restarts of all containers
	unready  int   // containers not ready, other than those that ran to completion
}

// completed reports a container that ran to completion (exit code 0), such as a finished init container:
// that is not trouble, although it is not ready.
func completed(cs containerStatus) bool {
	return cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0
}

func troubleOf(p *corev1.Pod) trouble {
	var t trouble
	for _, cs := range allStatuses(p) {
		if (cs.State.Waiting != nil && cs.State.Waiting.Reason != "") ||
			(cs.State.Terminated != nil && !completed(cs)) || cs.LastTerminationState.Terminated != nil {
			t.broken++
		}
		t.restarts += max(cs.RestartCount, 0)
		if !cs.Ready && !completed(cs) {
			t.unready++
		}
	}
	return t
}

// sortPods orders pods most troubled first, then by name, scoring each pod once.
func sortPods(pods []corev1.Pod) []corev1.Pod {
	type scored struct {
		pod *corev1.Pod
		tr  trouble
	}
	order := make([]scored, len(pods))
	for i := range pods {
		order[i] = scored{&pods[i], troubleOf(&pods[i])}
	}
	slices.SortFunc(order, func(a, b scored) int {
		return cmp.Or(
			-cmp.Compare(a.tr.broken, b.tr.broken), -cmp.Compare(a.tr.restarts, b.tr.restarts),
			-cmp.Compare(a.tr.unready, b.tr.unready), cmp.Compare(a.pod.Name, b.pod.Name))
	})
	out := make([]corev1.Pod, len(pods))
	for i, s := range order {
		out[i] = *s.pod
	}
	return out
}

// containerStatus is a container status with whether it belongs to an init container.
type containerStatus struct {
	corev1.ContainerStatus
	init bool
}

// allStatuses returns the statuses of the init containers, then of the containers.
func allStatuses(p *corev1.Pod) []containerStatus {
	out := make([]containerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
	for _, cs := range p.Status.InitContainerStatuses {
		out = append(out, containerStatus{cs, true})
	}
	for _, cs := range p.Status.ContainerStatuses {
		out = append(out, containerStatus{cs, false})
	}
	return out
}
