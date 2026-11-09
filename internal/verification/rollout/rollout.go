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

// Package rollout decides, from the status of a Deployment or StatefulSet alone, whether the pods have rolled
// out to the current template, and reports a rollout that did not complete within a deadline after the merged
// revision was applied (#532, design 6 and 13 stage 3).
//
// It only reads the object it is given: it never lists pods and never acts on them (invariant 2, no delete, no
// restart), so the executor needs no permission beyond its existing get on the target kinds (invariant 6). For a
// StatefulSet the pods on the old revision are inferred by ordinal from the status, because a rolling update
// replaces pods from the highest ordinal down; for a Deployment, whose pod names are random, only the counts are
// reported.
package rollout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The condition the executor sets on a request once the merged revision is applied, and its reasons.
const (
	// ConditionRolledOut is True when the target's pods run the applied template, False when they did not
	// within the deadline, Unknown while the rollout is still within it.
	ConditionRolledOut = "RolledOut"
	// ReasonRolloutComplete: every pod runs the current template and is ready.
	ReasonRolloutComplete = "RolloutComplete"
	// ReasonRolloutProgressing: the rollout has not completed, and the deadline has not passed.
	ReasonRolloutProgressing = "RolloutProgressing"
	// ReasonRolloutStuck: the rollout has not completed within the deadline. Shadewright reports it only.
	ReasonRolloutStuck = "RolloutStuck"
	// ReasonDeliveryNotObserved: the GitOps controller was not seen applying the merge commit within the window
	// after the merge, so the rollout was not checked (False). Shadewright reports it once and stops.
	ReasonDeliveryNotObserved = "DeliveryNotObserved"
	// ReasonKindNotChecked: the target is not a Deployment or StatefulSet, whose rollout the check covers
	// (Unknown).
	ReasonKindNotChecked = "KindNotChecked"
	// ReasonRolloutSuperseded: the target moved on to a newer template revision than the one applied with the
	// merge commit, so this rollout is not judged any more (Unknown).
	ReasonRolloutSuperseded = "RolloutSuperseded"
	// ReasonRolloutNotDetermined: the check ended without a verdict, for example the target could not be read
	// until the end of the check or is gone (Unknown).
	ReasonRolloutNotDetermined = "RolloutNotDetermined"
)

// Deadline bounds.
const (
	// DefaultDeadline is the time a rollout gets after the merged revision is applied.
	DefaultDeadline = 10 * time.Minute
	// MinDeadline and MaxDeadline bound a configured deadline.
	MinDeadline = time.Minute
	MaxDeadline = time.Hour
)

// maxNamedPods bounds the pod names a message lists; the rest are counted.
const maxNamedPods = 10

// State is the outcome of one evaluation.
type State int

const (
	// Progressing: not complete, deadline not passed. Nothing is reported.
	Progressing State = iota
	// Complete: every pod runs the current template and is ready.
	Complete
	// Stuck: not complete when the deadline passed.
	Stuck
	// Superseded: the target runs a newer template revision than the one recorded; this rollout is not judged.
	Superseded
)

// DeploymentRevisionAnnotation is the annotation the Deployment controller sets to the revision of the current
// template; it changes only when the template does.
const DeploymentRevisionAnnotation = "deployment.kubernetes.io/revision"

// Revision returns the template revision target's controller rolls out, once it observed at least generation:
// a StatefulSet's status.updateRevision, or a Deployment's revision annotation. ok is false before that, or when
// the object names none.
func Revision(target client.Object, generation int64) (rev string, ok bool) {
	switch t := target.(type) {
	case *appsv1.Deployment:
		if t != nil && t.Status.ObservedGeneration >= generation {
			rev = t.Annotations[DeploymentRevisionAnnotation]
		}
	case *appsv1.StatefulSet:
		if t != nil && t.Status.ObservedGeneration >= generation {
			rev = t.Status.UpdateRevision
		}
	}
	return rev, rev != ""
}

// ObservedGeneration is the generation target's controller last reported in status.observedGeneration, or 0
// for another kind.
func ObservedGeneration(target client.Object) int64 {
	switch t := target.(type) {
	case *appsv1.Deployment:
		if t != nil {
			return t.Status.ObservedGeneration
		}
	case *appsv1.StatefulSet:
		if t != nil {
			return t.Status.ObservedGeneration
		}
	}
	return 0
}

// TemplateHash returns the SHA-256, in lower-case hex, of target's pod template as JSON, or "" for another
// kind. Two reads of an unchanged template give the same hash, so a later generation that only scaled the
// target keeps it.
func TemplateHash(target client.Object) string {
	var tmpl any
	switch t := target.(type) {
	case *appsv1.Deployment:
		if t != nil {
			tmpl = t.Spec.Template
		}
	case *appsv1.StatefulSet:
		if t != nil {
			tmpl = t.Spec.Template
		}
	}
	if tmpl == nil {
		return ""
	}
	b, err := json.Marshal(tmpl)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// currentRevision is the revision target names now, whatever generation its controller observed.
func currentRevision(target client.Object) string {
	rev, _ := Revision(target, 0)
	return rev
}

// Result is what Evaluate found.
type Result struct {
	State State
	// Reason is one of the Reason constants of this package.
	Reason string
	// Message explains the result; for Stuck it names what is still on the old revision and the manual step.
	Message string
	// OldPods are the pods inferred to be on the old revision (StatefulSet only), highest ordinal first, at
	// most maxNamedPods.
	OldPods []string
}

// Condition returns the RolledOut condition for the result, for a request at generation.
func (r Result) Condition(generation int64, now time.Time) metav1.Condition {
	status := metav1.ConditionUnknown // Progressing, Superseded
	switch r.State {
	case Complete:
		status = metav1.ConditionTrue
	case Stuck:
		status = metav1.ConditionFalse
	}
	return metav1.Condition{
		Type:               ConditionRolledOut,
		Status:             status,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(now),
		Reason:             r.Reason,
		Message:            r.Message,
	}
}

// ErrNotEvaluable is returned for input that cannot be judged: an unsupported kind, a nil object or no time the
// revision was applied. The caller reports nothing and treats the rollout as not verified (fail closed).
var ErrNotEvaluable = errors.New("rollout cannot be evaluated")

// ClampDeadline returns d bounded to [MinDeadline, MaxDeadline], or DefaultDeadline when d is zero or negative.
func ClampDeadline(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DefaultDeadline
	case d < MinDeadline:
		return MinDeadline
	case d > MaxDeadline:
		return MaxDeadline
	}
	return d
}

// Evaluate judges the rollout of target, an *appsv1.Deployment or *appsv1.StatefulSet read after the merged
// revision was applied at appliedAt. revision is the template revision recorded for that rollout ([Revision]),
// or empty while none is recorded; a target that names another revision is Superseded. A rollout that has not
// completed by appliedAt plus the clamped deadline is Stuck. The object's own status is the only input; it is
// the API server's, but its names still go into the message only as object names the API server validated.
func Evaluate(target client.Object, revision string, appliedAt, now time.Time, deadline time.Duration) (Result, error) {
	if appliedAt.IsZero() {
		return Result{}, fmt.Errorf("%w: no applied time", ErrNotEvaluable)
	}
	var (
		done   bool
		detail string
		old    []string
	)
	switch t := target.(type) {
	case *appsv1.Deployment:
		if t == nil {
			return Result{}, fmt.Errorf("%w: nil Deployment", ErrNotEvaluable)
		}
		done, detail = deployment(t)
	case *appsv1.StatefulSet:
		if t == nil {
			return Result{}, fmt.Errorf("%w: nil StatefulSet", ErrNotEvaluable)
		}
		done, detail, old = statefulSet(t)
	default:
		return Result{}, fmt.Errorf("%w: unsupported kind %T", ErrNotEvaluable, target)
	}
	if cur := currentRevision(target); revision != "" && cur != "" && cur != revision {
		return Result{State: Superseded, Reason: ReasonRolloutSuperseded, Message: fmt.Sprintf("The target moved "+
			"on to template revision %q after revision %q, applied with the merge commit; this rollout is not "+
			"judged any more", cur, revision)}, nil
	}
	if done {
		return Result{State: Complete, Reason: ReasonRolloutComplete,
			Message: "Every pod runs the current template and is ready"}, nil
	}
	deadline = ClampDeadline(deadline)
	if now.Before(appliedAt.Add(deadline)) {
		return Result{State: Progressing, Reason: ReasonRolloutProgressing,
			Message: "The rollout is in progress: " + detail}, nil
	}
	return Result{State: Stuck, Reason: ReasonRolloutStuck, OldPods: old,
		Message: fmt.Sprintf("The rollout did not complete within %s of the applied revision: %s", deadline, detail)}, nil
}

// replicas returns the desired replica count: spec.replicas, 1 when unset, never negative.
func replicas(p *int32) int32 {
	if p == nil {
		return 1
	}
	return max(*p, 0)
}

// deployment follows kubectl rollout status: the controller observed the generation, every desired pod is
// updated, no old pod is left and every updated pod is available.
func deployment(d *appsv1.Deployment) (bool, string) {
	want := replicas(d.Spec.Replicas)
	s := d.Status
	if s.ObservedGeneration < d.Generation {
		return false, "the Deployment controller has not observed the new template yet"
	}
	if s.UpdatedReplicas < want {
		return false, fmt.Sprintf("%d of %d pods run the new template", max(s.UpdatedReplicas, 0), want)
	}
	if s.Replicas > s.UpdatedReplicas {
		return false, fmt.Sprintf("%d pods still run the old template", s.Replicas-s.UpdatedReplicas)
	}
	if s.AvailableReplicas < s.UpdatedReplicas {
		return false, fmt.Sprintf("%d of %d updated pods are available", max(s.AvailableReplicas, 0), s.UpdatedReplicas)
	}
	return true, ""
}

// statefulSet follows kubectl rollout status for a StatefulSet: every pod is ready, and the pods the update
// strategy replaces run the update revision. It infers the pods on the old revision from the counts.
func statefulSet(ss *appsv1.StatefulSet) (bool, string, []string) {
	want := replicas(ss.Spec.Replicas)
	s := ss.Status
	if s.ObservedGeneration < ss.Generation {
		return false, "the StatefulSet controller has not observed the new template yet", nil
	}
	var start int32
	if ss.Spec.Ordinals != nil {
		start = max(ss.Spec.Ordinals.Start, 0)
	}
	var partition int32
	onDelete := ss.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType
	if !onDelete && ss.Spec.UpdateStrategy.RollingUpdate != nil && ss.Spec.UpdateStrategy.RollingUpdate.Partition != nil {
		partition = min(max(*ss.Spec.UpdateStrategy.RollingUpdate.Partition, 0), want)
	}
	updated := min(max(s.UpdatedReplicas, 0), want)
	ready := min(max(s.ReadyReplicas, 0), want)
	allReady := ready >= want
	switch {
	case onDelete:
		// The current revision is promoted only by a rolling update, so it stays behind after pods were
		// replaced by hand; the counts decide.
		if updated >= want && allReady {
			return true, "", nil
		}
	case partition > 0:
		// Pods below the partition stay on the old revision on purpose; only those above it must be updated.
		if updated >= want-partition && allReady {
			return true, "", nil
		}
	default:
		if s.UpdateRevision == s.CurrentRevision && updated >= want && allReady {
			return true, "", nil
		}
	}

	notReady := fmt.Sprintf("%d of %d pods are not Ready, %d of %d run update revision %q", want-ready, want,
		updated, want, s.UpdateRevision)
	if onDelete {
		return false, notReady + "; the update strategy is OnDelete, so the StatefulSet controller replaces a pod " +
			"only after it is deleted. Shadewright does not delete pods: check the new template, then delete the " +
			"pods on the old revision yourself, never a Ready one first", nil
	}
	// A rolling update replaces pods from the highest ordinal down to the partition, so the pods not yet
	// updated are the ordinal indexes from want-1-updated down to the partition.
	var old []string
	total := 0
	for i := want - 1 - updated; i >= partition; i-- {
		total++
		if len(old) < maxNamedPods {
			old = append(old, fmt.Sprintf("%s-%d", ss.Name, start+i))
		}
	}
	msg := notReady
	if len(old) > 0 {
		names := strings.Join(old, ", ")
		if total > len(old) {
			names += fmt.Sprintf(" and %d more", total-len(old))
		}
		msg += fmt.Sprintf("; pods on the old revision %q (inferred by ordinal from the StatefulSet status): %s",
			s.CurrentRevision, names)
	}
	if ready >= want {
		return false, msg, old
	}
	msg += ". The StatefulSet controller does not replace a pod that is not Ready on the old revision (a broken " +
		"rollout needs a manual step, see the Kubernetes StatefulSet docs on forced rollback). Shadewright does " +
		"not delete pods"
	if updated > 0 {
		msg += fmt.Sprintf(". If a pod on update revision %q is not Ready, the new template fails and deleting pods "+
			"does not help: fix the template", s.UpdateRevision)
	}
	if ready == 0 && partition == 0 && len(old) > 0 {
		// Every pod is not Ready, so the lowest ordinal is certainly a pod that is not Ready on the old revision.
		blocker := fmt.Sprintf("%s-%d", ss.Name, start)
		return false, msg + fmt.Sprintf(". Check that the new template is correct, then delete %s yourself so it "+
			"is recreated from update revision %q", blocker, s.UpdateRevision), old
	}
	return false, msg + fmt.Sprintf(". Find the pod that is not Ready (kubectl get pods -L "+
		"controller-revision-hash); if it runs the old revision %q, check that the new template is correct and "+
		"delete that pod yourself, never a Ready one", s.CurrentRevision), old
}
