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
)

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
	status := metav1.ConditionUnknown
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
// revision was applied at appliedAt. A rollout that has not completed by appliedAt plus the clamped deadline is
// Stuck. The object's own status is the only input; it is the API server's, but its names still go into the
// message only as object names the API server validated.
func Evaluate(target client.Object, appliedAt, now time.Time, deadline time.Duration) (Result, error) {
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

// statefulSet follows kubectl rollout status for a StatefulSet and infers the pods on the old revision.
func statefulSet(ss *appsv1.StatefulSet) (bool, string, []string) {
	want := replicas(ss.Spec.Replicas)
	s := ss.Status
	if s.ObservedGeneration < ss.Generation {
		return false, "the StatefulSet controller has not observed the new template yet", nil
	}
	var partition int32
	rolling := ss.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType
	if rolling && ss.Spec.UpdateStrategy.RollingUpdate != nil && ss.Spec.UpdateStrategy.RollingUpdate.Partition != nil {
		partition = min(max(*ss.Spec.UpdateStrategy.RollingUpdate.Partition, 0), want)
	}
	updated := min(max(s.UpdatedReplicas, 0), want)
	if partition > 0 {
		// Pods below the partition stay on the old revision on purpose; only those above it must be updated.
		if updated >= want-partition {
			return true, "", nil
		}
	} else if s.UpdateRevision == s.CurrentRevision && updated >= want && s.ReadyReplicas >= want {
		return true, "", nil
	}

	if !rolling {
		return false, fmt.Sprintf("%d of %d pods run update revision %q; the update strategy is OnDelete, so the "+
			"StatefulSet controller replaces a pod only after it is deleted. Shadewright does not delete pods: "+
			"check the new template, then delete the pods on the old revision yourself",
			updated, want, s.UpdateRevision), nil
	}
	// A rolling update replaces pods from the highest ordinal down to the partition, so the pods not yet
	// updated are the ordinals from want-1-updated down to the partition. The highest of them is the one
	// the controller waits for.
	var old []string
	total := 0
	for i := want - 1 - updated; i >= partition; i-- {
		total++
		if len(old) < maxNamedPods {
			old = append(old, fmt.Sprintf("%s-%d", ss.Name, i))
		}
	}
	if len(old) == 0 {
		return false, fmt.Sprintf("%d of %d pods are ready (current revision %q, update revision %q)",
			max(s.ReadyReplicas, 0), want, s.CurrentRevision, s.UpdateRevision), old
	}
	names := strings.Join(old, ", ")
	if total > len(old) {
		names += fmt.Sprintf(" and %d more", total-len(old))
	}
	return false, fmt.Sprintf("pods on the old revision %q (by ordinal, from the StatefulSet status): %s. "+
		"The StatefulSet controller does not replace a pod that is not Ready on the old revision (a broken "+
		"rollout needs a manual step, see the Kubernetes StatefulSet docs on forced rollback). Shadewright does "+
		"not delete pods: check that the new template is correct, then delete %s yourself so it is recreated "+
		"from update revision %q", s.CurrentRevision, names, old[0], s.UpdateRevision), old
}
