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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/ownership"
	"github.com/pkalin/shadewright/internal/verification/rollout"
)

// StatefulSetRevisionLabel is the label a StatefulSet's controller sets on each pod to the revision it was
// created from.
const StatefulSetRevisionLabel = "controller-revision-hash"

// ReasonOOMKilled is the termination reason of a container the kernel killed for its memory limit.
const ReasonOOMKilled = "OOMKilled"

// ReplicaSetGetter reads one ReplicaSet by name in the target's namespace, with a get. It returns nil and no
// error when there is none; any other failure is an error, and the sample is not taken.
type ReplicaSetGetter func(name string) (*appsv1.ReplicaSet, error)

// Input is what the verifier read for one sample.
type Input struct {
	// Now is the verifier's clock at the sample, taken after every read of the sample returned (the target,
	// the pod list and the ReplicaSets of [ReplicaSetNames]), so that sampledAt and windowStart never predate
	// what the sample observed.
	Now time.Time
	// Target is the target as read now: an *appsv1.Deployment or an *appsv1.StatefulSet, nil when it does
	// not exist.
	Target client.Object
	// Pods are the pods the target's selector matched in its namespace, from at most MaxPages pages of
	// PageSize.
	Pods []corev1.Pod
	// Truncated is true when the list had more pods than the pages hold.
	Truncated bool
	// ReplicaSets reads the ReplicaSet a pod's controller reference names; used for a Deployment only, and
	// only for the names [ReplicaSetNames] returns.
	ReplicaSets ReplicaSetGetter
	// MemoryLimit is the desired memory limit of the action's container for SetContainerMemoryLimit, nil for
	// an action without one. A pod whose container has another limit is not a pod of the rollout.
	MemoryLimit *resource.Quantity
}

// Result is the outcome of one sample.
type Result struct {
	// Status is the report status to write: the previous one with this sample applied.
	Status v1alpha1.VerificationReportStatus
	// Reason is why the sample was not good, empty for a good one.
	Reason v1alpha1.ResetReason
	// OOMKilled is true when this sample recorded the OOM kill of the action's container: the request ends
	// Failed at once.
	OOMKilled bool
}

// errNoGetter is returned for a Deployment target sampled without a ReplicaSet reader.
var errNoGetter = errors.New("sampling a Deployment needs a ReplicaSet reader")

// Sample applies one sample to the report status prev (ADR-0030 section 4): it proves which pods belong to the
// rollout, compares their restart counts with the durable baselines, applies the gap rule, records an OOM kill
// of the action's container, prunes the baselines of deleted pods, and moves the window. A read that failed
// is an error: the caller retries, and nothing is written, since a failed read is not a sample.
func Sample(spec v1alpha1.VerificationReportSpec, prev v1alpha1.VerificationReportStatus, in Input) (Result, error) {
	now := seconds(in.Now)
	st := *prev.DeepCopy()
	// A report with samples but no sampledAt cannot show it was watched: that counts as a gap.
	gap := prev.Sample > 0 && (prev.SampledAt == nil || now.Sub(prev.SampledAt.Time) > GapAfter)
	st.Sample = prev.Sample + 1
	st.SampledAt = &now

	o, err := evaluate(spec, &prev, &st, in, now.Time, gap)
	if err != nil {
		return Result{}, err
	}
	w := ReportWindow(&st)
	switch {
	case o.reason == "":
		w.Good(now.Time)
	case o.nodeTimeOnly:
		w.ResetUnrecorded(o.reason, now.Time)
	default:
		w.Reset(o.reason, now.Time)
	}
	w.store(&st)
	return Result{Status: st, Reason: o.reason, OOMKilled: o.oomRecorded}, nil
}

// podSample is what one pod of the rollout showed.
type podSample struct {
	uid      types.UID
	restarts int64
	// complete: a status for every container of the pod's spec.
	complete bool
	ready    bool
	// terminatedAt holds the finish time of every current and last termination of the pod's containers.
	terminatedAt []time.Time
	// oom is the OOM-killed termination of the action's container, if any.
	oom *v1alpha1.OOMKill
}

// outcome is what evaluate found in one sample.
type outcome struct {
	// reason is the reset reason, empty for a good sample.
	reason v1alpha1.ResetReason
	// oomRecorded is true when this sample recorded the OOM kill.
	oomRecorded bool
	// nodeTimeOnly is true for a Restarted reset that only a termination time shows: no restart count above
	// its baseline and no OOM kill. Node times can only delay or deny success, so it is not durable trouble.
	nodeTimeOnly bool
}

// evaluate fills st (desiredReplicas, podsCovered, baselines, oomKill) and returns what the sample showed.
func evaluate(spec v1alpha1.VerificationReportSpec, prev, st *v1alpha1.VerificationReportStatus, in Input,
	now time.Time, gap bool) (outcome, error) {
	replicas, ok := targetReplicas(spec, in.Target)
	if !ok {
		// The target is gone, replaced or names another template revision: the executor ends the request on
		// its own reads; for the report this sample is no evidence.
		st.PodsCovered = 0
		return outcome{reason: v1alpha1.ResetEvidenceMissing}, nil
	}
	st.DesiredReplicas = replicas
	if replicas == 0 {
		st.PodsCovered = 0
		return outcome{reason: v1alpha1.ResetScaledToZero}, nil
	}
	// A list longer than the pages hold, or more desired replicas than the bound, cannot be covered.
	coverable := !in.Truncated && len(in.Pods) <= PageSize*MaxPages && replicas <= MaxPods

	pods, otherRevision, err := rolloutPods(spec, in)
	if err != nil {
		return outcome{}, err
	}
	// An OOM kill of the action's container on a returned pod of the rollout is affirmative evidence on its
	// own: a pod of the rollout is proven to run the applied template with the desired limit. It is recorded
	// before any bound rejects coverage.
	var oom *v1alpha1.OOMKill
	for _, p := range pods {
		if p.oom != nil {
			oom = p.oom
			break
		}
	}
	recorded := false
	if oom != nil && st.OOMKill == nil {
		st.OOMKill = oom
		recorded = true
	}
	// uncovered ends a sample that cannot cover every pod: the baselines stay as they are, none is dropped to
	// fit. It is EvidenceMissing, or Restarted when an OOM kill was seen.
	uncovered := func() (outcome, error) {
		st.PodsCovered = 0
		if oom != nil {
			return outcome{reason: v1alpha1.ResetRestarted, oomRecorded: recorded}, nil
		}
		return outcome{reason: v1alpha1.ResetEvidenceMissing}, nil
	}
	if !coverable {
		return uncovered()
	}
	listed := make(map[types.UID]bool, len(in.Pods))
	for i := range in.Pods {
		listed[in.Pods[i].UID] = true
	}
	baselines := make(map[types.UID]int64, len(prev.Baselines))
	for _, b := range prev.Baselines {
		if listed[types.UID(b.PodUID)] {
			baselines[types.UID(b.PodUID)] = b.Restarts
		}
	}
	// A pod still listed but not of the rollout (terminating) keeps its baseline.
	next := make(map[types.UID]int64, len(baselines)+len(pods))
	maps.Copy(next, baselines)
	for _, p := range pods {
		next[p.uid] = p.restarts
	}
	if len(pods) > MaxPods || len(next) > MaxPods {
		return uncovered()
	}

	c := compare(pods, baselines, prev, now, gap)
	st.PodsCovered = int32(len(pods)) // #nosec G115 -- at most MaxPods, checked above
	st.Baselines = make([]v1alpha1.PodBaseline, 0, len(next))
	for uid, n := range next {
		st.Baselines = append(st.Baselines, v1alpha1.PodBaseline{PodUID: v1alpha1.UID(uid), Restarts: n})
	}
	slices.SortFunc(st.Baselines, func(a, b v1alpha1.PodBaseline) int { return strings.Compare(string(a.PodUID), string(b.PodUID)) })

	switch {
	case oom != nil || c.counted:
		return outcome{reason: v1alpha1.ResetRestarted, oomRecorded: recorded}, nil
	case c.terminated:
		return outcome{reason: v1alpha1.ResetRestarted, nodeTimeOnly: true}, nil
	case c.newAfterGap:
		return outcome{reason: v1alpha1.ResetObservationGap}, nil
	case otherRevision || len(pods) != int(replicas):
		return outcome{reason: v1alpha1.ResetPartialRollout}, nil
	case c.incomplete:
		return outcome{reason: v1alpha1.ResetEvidenceMissing}, nil
	case c.notReady:
		return outcome{reason: v1alpha1.ResetPodNotReady}, nil
	}
	return outcome{}, nil
}

// comparison is what the pods of the rollout showed against the baselines.
type comparison struct {
	// counted: a restart count above its baseline. terminated: a termination at or after the window's start
	// minus TerminationMargin, a node time.
	counted, terminated, newAfterGap, incomplete, notReady bool
}

// compare checks the pods of the rollout against the stored baselines (those of pods still listed): a count
// above its baseline, or a termination at or after the window's start minus TerminationMargin, is a restart.
// A pod without a baseline takes its current count on the report's first sample, is an observation gap after
// a gap, and starts at 0 otherwise.
func compare(pods []podSample, baselines map[types.UID]int64, prev *v1alpha1.VerificationReportStatus,
	now time.Time, gap bool) comparison {
	// The window a termination is compared with: the open window's start, or this sample, which would open
	// one. One poll interval of margin absorbs skew between the node's clock and the verifier's.
	since := now
	if w := ReportWindow(prev); w.Open() {
		since = w.Start.Time
	}
	since = since.Add(-TerminationMargin)

	var c comparison
	for _, p := range pods {
		base, known := baselines[p.uid]
		switch {
		case known:
		case prev.Sample == 0:
			// The first sample of the report takes every pod's current count as its baseline.
			base = p.restarts
		case gap:
			// The pod may have replaced one whose history went with it.
			c.newAfterGap = true
			base = p.restarts
		default:
			// A new pod without a gap (a scale-out or a replacement) starts at 0.
			base = 0
		}
		c.counted = c.counted || p.restarts > base
		c.terminated = c.terminated || slices.ContainsFunc(p.terminatedAt, func(t time.Time) bool { return !t.Before(since) })
		c.incomplete = c.incomplete || !p.complete
		c.notReady = c.notReady || !p.ready
	}
	return c
}

// targetReplicas returns the target's desired replicas when it is the target the report names: the same UID
// and the report's template revision.
func targetReplicas(spec v1alpha1.VerificationReportSpec, target client.Object) (int32, bool) {
	var replicas *int32
	switch t := target.(type) {
	case *appsv1.Deployment:
		if t == nil {
			return 0, false
		}
		replicas = t.Spec.Replicas
	case *appsv1.StatefulSet:
		if t == nil {
			return 0, false
		}
		replicas = t.Spec.Replicas
	default:
		return 0, false
	}
	if target.GetUID() != types.UID(spec.TargetUID) {
		return 0, false
	}
	if rev, ok := rollout.Revision(target, 0); !ok || rev != spec.TemplateRevision {
		return 0, false
	}
	if replicas == nil {
		return 1, true // the API server defaults spec.replicas to 1
	}
	return max(*replicas, 0), true
}

// rolloutPods returns the pods of the rollout and whether a pod of the target runs another revision. A pod
// counts when it is not terminating, its ownership by the target is proven over typed objects (owner
// references are untrusted), it runs the report's template revision, and, for an action that names a
// container, that container exists with the desired limit.
func rolloutPods(spec v1alpha1.VerificationReportSpec, in Input) ([]podSample, bool, error) {
	var (
		pods          []podSample
		otherRevision bool
		rsCache       = map[string]*appsv1.ReplicaSet{}
	)
	for i := range in.Pods {
		pod := &in.Pods[i]
		if pod.DeletionTimestamp != nil || pod.Namespace != in.Target.GetNamespace() {
			continue
		}
		rev, owned, err := ownedRevision(in, pod, rsCache)
		if err != nil {
			return nil, false, err
		}
		if !owned {
			continue
		}
		if rev != spec.TemplateRevision {
			otherRevision = true
			continue
		}
		if !hasContainer(pod, spec.Container, in.MemoryLimit) {
			continue
		}
		pods = append(pods, observe(pod, spec.Container))
	}
	return pods, otherRevision, nil
}

// ReplicaSetNames returns the names of the ReplicaSets that [Sample] reads through in.ReplicaSets for this spec
// and input, each once, in the order of the pods. A caller reads them all before it takes the time of the
// sample, so that Input.Now is never earlier than a read it covers, and passes a getter over what it read. It
// ignores in.Now and in.ReplicaSets.
func ReplicaSetNames(spec v1alpha1.VerificationReportSpec, in Input) []string {
	d, ok := in.Target.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	if replicas, ok := targetReplicas(spec, in.Target); !ok || replicas == 0 {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	for i := range in.Pods {
		pod := &in.Pods[i]
		if pod.DeletionTimestamp != nil || pod.Namespace != d.Namespace {
			continue
		}
		ref := ownership.PodController(pod)
		if ref == nil || ref.Kind != ownership.KindReplicaSet || seen[ref.Name] {
			continue
		}
		seen[ref.Name] = true
		names = append(names, ref.Name)
	}
	return names
}

// ownedRevision proves that pod belongs to the target and returns the template revision it runs.
//
// For a Deployment: the pod's controller reference names a ReplicaSet that exists with that UID and whose
// selector matches the pod; the ReplicaSet's controller reference names the target with its UID, and the
// target's selector matches the ReplicaSet's labels; the revision is the ReplicaSet's revision annotation.
// For a StatefulSet: the pod's controller reference names the target with its UID and the target's selector
// matches the pod; the revision is the pod's controller-revision-hash label.
func ownedRevision(in Input, pod *corev1.Pod, rsCache map[string]*appsv1.ReplicaSet) (string, bool, error) {
	ref := ownership.PodController(pod)
	if ref == nil {
		return "", false, nil
	}
	switch t := in.Target.(type) {
	case *appsv1.StatefulSet:
		if ref.Kind != ownership.KindStatefulSet || ref.Name != t.Name {
			return "", false, nil
		}
		o, ok := ownership.Verify(*ref, t, t.Spec.Selector)
		if !ok || !o.Controls(pod) {
			return "", false, nil
		}
		return pod.Labels[StatefulSetRevisionLabel], true, nil
	case *appsv1.Deployment:
		if ref.Kind != ownership.KindReplicaSet {
			return "", false, nil
		}
		rs, err := readReplicaSet(in.ReplicaSets, ref.Name, rsCache)
		if err != nil {
			return "", false, err
		}
		if rs == nil || rs.Namespace != t.Namespace {
			return "", false, nil
		}
		o, ok := ownership.Verify(*ref, rs, rs.Spec.Selector)
		if !ok || !o.Controls(pod) || o.Parent == nil || o.Parent.Name != t.Name {
			return "", false, nil
		}
		d, ok := ownership.Verify(*o.Parent, t, t.Spec.Selector)
		if !ok || !d.Controls(rs) {
			return "", false, nil
		}
		return rs.Annotations[rollout.DeploymentRevisionAnnotation], true, nil
	}
	return "", false, nil
}

// readReplicaSet reads a ReplicaSet once per sample.
func readReplicaSet(get ReplicaSetGetter, name string, cache map[string]*appsv1.ReplicaSet) (*appsv1.ReplicaSet, error) {
	if rs, ok := cache[name]; ok {
		return rs, nil
	}
	if get == nil {
		return nil, errNoGetter
	}
	rs, err := get(name)
	if err != nil {
		return nil, fmt.Errorf("reading the ReplicaSet of a Pod: %w", err)
	}
	cache[name] = rs
	return rs, nil
}

// hasContainer reports whether the pod has the action's container with the desired memory limit; true for
// an action that names no container.
func hasContainer(pod *corev1.Pod, name string, limit *resource.Quantity) bool {
	if name == "" {
		return true
	}
	i := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == name })
	if i < 0 {
		return false
	}
	if limit == nil {
		return true
	}
	got, ok := pod.Spec.Containers[i].Resources.Limits[corev1.ResourceMemory]
	return ok && got.Cmp(*limit) == 0
}

// watchedContainer is a container whose restarts count: one of the pod's containers, or a native sidecar (an
// init container with restartPolicy Always), which runs beside them for the pod's life. An ordinary init
// container runs to completion before them and does not count.
type watchedContainer struct {
	name string
	// statuses is the status list the container's status is in.
	statuses []corev1.ContainerStatus
	// regular is true for one of the pod's containers, the only kind the action's container can be.
	regular bool
}

func watchedContainers(pod *corev1.Pod) []watchedContainer {
	out := make([]watchedContainer, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			out = append(out, watchedContainer{name: c.Name, statuses: pod.Status.InitContainerStatuses})
		}
	}
	for _, c := range pod.Spec.Containers {
		out = append(out, watchedContainer{name: c.Name, statuses: pod.Status.ContainerStatuses, regular: true})
	}
	return out
}

// observe keeps what a sample needs of one pod of the rollout: typed fields only, never a message. A watched
// container without a status, or whose current termination has no finish time, leaves the pod incomplete.
func observe(pod *corev1.Pod, container string) podSample {
	p := podSample{uid: pod.UID, complete: true}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			p.ready = c.Status == corev1.ConditionTrue
		}
	}
	for _, c := range watchedContainers(pod) {
		i := slices.IndexFunc(c.statuses, func(s corev1.ContainerStatus) bool { return s.Name == c.name })
		if i < 0 {
			p.complete = false
			continue
		}
		s := c.statuses[i]
		p.restarts += int64(max(s.RestartCount, 0))
		if s.State.Terminated != nil && s.State.Terminated.FinishedAt.IsZero() {
			// A termination without a time cannot be placed against the window.
			p.complete = false
		}
		for _, term := range []*corev1.ContainerStateTerminated{s.State.Terminated, s.LastTerminationState.Terminated} {
			if term == nil {
				continue
			}
			if !term.FinishedAt.IsZero() {
				p.terminatedAt = append(p.terminatedAt, term.FinishedAt.Time)
			}
			if c.regular && container != "" && s.Name == container && term.Reason == ReasonOOMKilled && p.oom == nil {
				p.oom = &v1alpha1.OOMKill{PodUID: v1alpha1.UID(pod.UID), Container: s.Name}
				if !term.FinishedAt.IsZero() {
					at := metav1.NewTime(term.FinishedAt.UTC().Truncate(time.Second))
					p.oom.FinishedAt = &at
				}
			}
		}
	}
	return p
}
