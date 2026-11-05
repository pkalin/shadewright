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
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// lines collects the lines of a section; every value from the cluster goes through Builder.value first.
type lines struct {
	strings.Builder
}

func (l *lines) add(format string, args ...any) {
	fmt.Fprintf(&l.Builder, format, args...)
	_ = l.WriteByte('\n') // a strings.Builder never fails
}

func stamp(t metav1.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// summaryBlock describes the Incident, most relevant first.
func (b *Builder) summaryBlock(inc *remediationv1alpha1.Incident) Block {
	s := inc.Spec
	var l lines
	l.add("subject: %s %s/%s", b.value(s.Subject.Kind), b.value(s.Subject.Namespace), b.value(s.Subject.Name))
	l.add("reason: %s (%s)", b.value(s.Reason), b.value(s.Type))
	if s.ReportingController != "" {
		l.add("reported by: %s", b.value(s.ReportingController))
	}
	if s.Symptom != "" {
		l.add("symptom: %s", b.value(s.Symptom))
	}
	st := inc.Status
	l.add("occurrences: %d", st.Count)
	if st.FirstSeen != nil {
		l.add("first seen: %s", stamp(*st.FirstSeen))
	}
	if st.LastSeen != nil {
		l.add("last seen: %s", stamp(*st.LastSeen))
	}
	if st.Phase != "" {
		l.add("phase: %s", b.value(string(st.Phase)))
	}
	blk := Block{Section: SectionSummary, Provenance: ProvenanceIncident, Availability: Available}
	b.finish(&blk, l.String(), b.limits.Summary)
	return blk
}

// statusBlock describes the target, its owners and its current pods, most relevant first.
func (b *Builder) statusBlock(t *target) Block {
	blk := Block{Section: SectionStatus, Provenance: ProvenanceObjectStatus, Availability: Available}
	var l lines
	switch t.state {
	case targetFailed:
		blk.Availability = CouldNotFetch
		l.add("could not fetch: reading the target or its pods failed")
	case targetUnsupported:
		blk.Availability = NotAvailable
		l.add("not available: the analyzer does not read objects of kind %s", b.value(t.ref.Kind))
	case targetUnwatched:
		blk.Availability = NotAvailable
		l.add("not available: the target is in a namespace the analyzer does not watch")
	case targetGone:
		l.add("%s %s/%s no longer exists, or was replaced by another object of the same name",
			b.value(t.ref.Kind), b.value(t.ref.Namespace), b.value(t.ref.Name))
	default:
		b.workloadLines(&l, t)
		b.podLines(&l, t)
	}
	b.podLabels(&blk, t)
	b.finish(&blk, l.String(), b.limits.Status)
	return blk
}

// podLabels marks a block whose evidence depends on the pod set.
func (b *Builder) podLabels(blk *Block, t *target) {
	if t.state != targetResolved {
		return
	}
	if t.uncertain {
		blk.addLabel(LabelPodsUncertain)
	}
	if t.capped {
		blk.addLabel(LabelPodsCapped)
	}
}

func (b *Builder) conditionLine(l *lines, typ, status, reason, message string, since metav1.Time) {
	l.add("  condition %s=%s since %s, reason %s: %s", b.value(typ), b.value(status), stamp(since),
		b.value(reason), b.value(message))
}

func (b *Builder) workloadLines(l *lines, t *target) {
	switch o := t.workload.(type) {
	case *appsv1.Deployment:
		l.add("Deployment %s/%s, generation %d, observed generation %d", b.value(o.Namespace), b.value(o.Name),
			o.Generation, o.Status.ObservedGeneration)
		l.add("  replicas: desired %d, updated %d, ready %d, available %d, unavailable %d", desired(o.Spec.Replicas),
			o.Status.UpdatedReplicas, o.Status.ReadyReplicas, o.Status.AvailableReplicas, o.Status.UnavailableReplicas)
		for _, c := range o.Status.Conditions {
			b.conditionLine(l, string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime)
		}
		for _, rs := range t.replicaSets {
			l.add("  owns ReplicaSet %s, created %s, revision %s, replicas desired %d, ready %d", b.value(rs.Name),
				stamp(rs.CreationTimestamp), b.value(rs.Annotations["deployment.kubernetes.io/revision"]),
				desired(rs.Spec.Replicas), rs.Status.ReadyReplicas)
		}
	case *appsv1.StatefulSet:
		l.add("StatefulSet %s/%s, generation %d, observed generation %d", b.value(o.Namespace), b.value(o.Name),
			o.Generation, o.Status.ObservedGeneration)
		l.add("  replicas: desired %d, current %d, updated %d, ready %d, available %d", desired(o.Spec.Replicas),
			o.Status.CurrentReplicas, o.Status.UpdatedReplicas, o.Status.ReadyReplicas, o.Status.AvailableReplicas)
		l.add("  current revision %s, update revision %s", b.value(o.Status.CurrentRevision), b.value(o.Status.UpdateRevision))
		for _, c := range o.Status.Conditions {
			b.conditionLine(l, string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime)
		}
	case *appsv1.ReplicaSet:
		l.add("ReplicaSet %s/%s, generation %d, observed generation %d", b.value(o.Namespace), b.value(o.Name),
			o.Generation, o.Status.ObservedGeneration)
		l.add("  replicas: desired %d, ready %d, available %d", desired(o.Spec.Replicas), o.Status.ReadyReplicas,
			o.Status.AvailableReplicas)
		for _, c := range o.Status.Conditions {
			b.conditionLine(l, string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime)
		}
	}
	if t.outOfBounds {
		l.add("  its labels or its selector are larger than the builder follows, or the selector is empty: pods not resolved")
	}
}

func desired(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

func (b *Builder) podLines(l *lines, t *target) {
	if t.workload != nil && !t.outOfBounds {
		if t.total > len(t.pods) {
			l.add("current pods: %d, the %d most troubled shown", t.total, len(t.pods))
		} else {
			l.add("current pods: %d", t.total)
		}
	}
	for i := range t.pods {
		p := &t.pods[i]
		owner := ""
		if ref := metav1.GetControllerOfNoCopy(p); ref != nil {
			owner = ", controlled by " + b.value(ref.Kind) + " " + b.value(ref.Name)
		}
		l.add("pod %s%s, phase %s, created %s", b.value(p.Name), owner, b.value(string(p.Status.Phase)),
			stamp(p.CreationTimestamp))
		if p.Status.Reason != "" || p.Status.Message != "" {
			l.add("  status reason %s: %s", b.value(p.Status.Reason), b.value(p.Status.Message))
		}
		for _, c := range p.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				b.conditionLine(l, string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime)
			}
		}
		for _, cs := range allStatuses(p) {
			b.containerLine(l, cs)
		}
	}
}

func (b *Builder) containerLine(l *lines, cs containerStatus) {
	kind := "container"
	if cs.init {
		kind = "init container"
	}
	parts := []string{fmt.Sprintf("ready %t, restarts %d", cs.Ready, cs.RestartCount)}
	parts = append(parts, "now "+b.state(cs.State))
	if cs.LastTerminationState.Terminated != nil {
		parts = append(parts, "last "+b.state(cs.LastTerminationState))
	}
	l.add("  %s %s: %s", kind, b.value(cs.Name), strings.Join(parts, "; "))
}

func (b *Builder) state(s corev1.ContainerState) string {
	switch {
	case s.Waiting != nil:
		return fmt.Sprintf("waiting, reason %s: %s", b.value(s.Waiting.Reason), b.value(s.Waiting.Message))
	case s.Terminated != nil:
		x := s.Terminated
		return fmt.Sprintf("terminated, reason %s, exit code %d, signal %d, started %s, finished %s: %s",
			b.value(x.Reason), x.ExitCode, x.Signal, stamp(x.StartedAt), stamp(x.FinishedAt), b.value(x.Message))
	case s.Running != nil:
		return "running since " + stamp(s.Running.StartedAt)
	}
	return "unknown"
}
