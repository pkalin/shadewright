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
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// lines collects the lines of a section; every value from the cluster goes through Builder.value first, and
// every value from a source that may be quoted as evidence through lines.data.
type lines struct {
	strings.Builder
	values []string
}

// data records v, a value from a source as rendered, as quotable ([Block.Data]) and returns it.
func (l *lines) data(v string) string {
	l.values = append(l.values, v)
	return v
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
	l.add("subject: %s %s/%s", l.data(b.value(s.Subject.Kind)), l.data(b.value(s.Subject.Namespace)), l.data(b.value(s.Subject.Name)))
	l.add("reason: %s (%s)", l.data(b.value(s.Reason)), l.data(b.value(s.Type)))
	if s.ReportingController != "" {
		l.add("reported by: %s", l.data(b.value(s.ReportingController)))
	}
	if s.Symptom != "" {
		l.add("symptom: %s", l.data(b.value(s.Symptom)))
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
		// The phase is written by the analyzer itself, never evidence.
		l.add("phase: %s", b.value(string(st.Phase)))
	}
	blk := Block{Section: SectionSummary, Provenance: ProvenanceIncident, Availability: Available}
	b.finish(&blk, &l, b.limits.Summary)
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
		l.add("not available: the analyzer does not read objects of kind %s", l.data(b.value(t.ref.Kind)))
	case targetUnwatched:
		blk.Availability = NotAvailable
		l.add("not available: the target is in a namespace the analyzer does not watch")
	case targetGone:
		l.add("%s %s/%s no longer exists, or was replaced by another object of the same name",
			l.data(b.value(t.ref.Kind)), l.data(b.value(t.ref.Namespace)), l.data(b.value(t.ref.Name)))
	default:
		b.workloadLines(&l, t)
		b.podLines(&l, t)
	}
	b.podLabels(&blk, t)
	b.finish(&blk, &l, b.limits.Status)
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
	l.add("  condition %s=%s since %s, reason %s: %s", l.data(b.value(typ)), l.data(b.value(status)), stamp(since),
		l.data(b.value(reason)), l.data(b.value(message)))
}

func (b *Builder) workloadLines(l *lines, t *target) {
	switch o := t.workload.(type) {
	case *appsv1.Deployment:
		l.add("Deployment %s/%s, generation %d, observed generation %d", l.data(b.value(o.Namespace)), l.data(b.value(o.Name)),
			o.Generation, o.Status.ObservedGeneration)
		l.add("  replicas: desired %d, updated %d, ready %d, available %d, unavailable %d", desired(o.Spec.Replicas),
			o.Status.UpdatedReplicas, o.Status.ReadyReplicas, o.Status.AvailableReplicas, o.Status.UnavailableReplicas)
		b.templateLines(l, o.Spec.Template.Spec.Containers)
		for _, c := range o.Status.Conditions {
			b.conditionLine(l, string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime)
		}
		for _, rs := range t.replicaSets {
			l.add("  owns ReplicaSet %s, created %s, revision %s, replicas desired %d, ready %d", l.data(b.value(rs.Name)),
				stamp(rs.CreationTimestamp), l.data(b.value(rs.Annotations["deployment.kubernetes.io/revision"])),
				desired(rs.Spec.Replicas), rs.Status.ReadyReplicas)
		}
	case *appsv1.StatefulSet:
		l.add("StatefulSet %s/%s, generation %d, observed generation %d", l.data(b.value(o.Namespace)), l.data(b.value(o.Name)),
			o.Generation, o.Status.ObservedGeneration)
		l.add("  replicas: desired %d, current %d, updated %d, ready %d, available %d", desired(o.Spec.Replicas),
			o.Status.CurrentReplicas, o.Status.UpdatedReplicas, o.Status.ReadyReplicas, o.Status.AvailableReplicas)
		l.add("  current revision %s, update revision %s", l.data(b.value(o.Status.CurrentRevision)), l.data(b.value(o.Status.UpdateRevision)))
		b.templateLines(l, o.Spec.Template.Spec.Containers)
		for _, c := range o.Status.Conditions {
			b.conditionLine(l, string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime)
		}
	case *appsv1.ReplicaSet:
		l.add("ReplicaSet %s/%s, generation %d, observed generation %d", l.data(b.value(o.Namespace)), l.data(b.value(o.Name)),
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

// templateLines shows the memory limit of each container of a workload's pod template, labelled as the
// template's: the workload's current spec, which new pods get and a change in Git edits. During a rollout a
// running pod may still hold an older value, which its own container line shows (#494).
func (b *Builder) templateLines(l *lines, containers []corev1.Container) {
	for _, c := range containers {
		l.add("  pod template (the workload's current spec): container %s, memory limit %s", l.data(b.value(c.Name)),
			b.memoryLimit(l, c.Resources))
	}
}

// memoryLimit renders the memory limit of resources as a quotable value, or "not set".
func (b *Builder) memoryLimit(l *lines, r corev1.ResourceRequirements) string {
	q, ok := r.Limits[corev1.ResourceMemory]
	if !ok {
		return "not set"
	}
	return l.data(b.value(q.String()))
}

// podResources returns the resources a pod's spec gives the container called name.
func podResources(p *corev1.Pod, name string, init bool) (corev1.ResourceRequirements, bool) {
	containers := p.Spec.Containers
	if init {
		containers = p.Spec.InitContainers
	}
	for _, c := range containers {
		if c.Name == name {
			return c.Resources, true
		}
	}
	return corev1.ResourceRequirements{}, false
}

// templateOf returns the containers of the pod template of a Deployment or StatefulSet target that was
// read with its UID, with their memory limits, or nil for any other target ([Context.Template]).
func templateOf(t *target) []TemplateContainer {
	if t.state != targetResolved {
		return nil
	}
	var containers []corev1.Container
	switch o := t.workload.(type) {
	case *appsv1.Deployment:
		containers = o.Spec.Template.Spec.Containers
	case *appsv1.StatefulSet:
		containers = o.Spec.Template.Spec.Containers
	default:
		return nil
	}
	out := make([]TemplateContainer, 0, len(containers))
	for _, c := range containers {
		tc := TemplateContainer{Name: c.Name}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			q = q.DeepCopy()
			tc.MemoryLimit = &q
		}
		out = append(out, tc)
	}
	return out
}

// sameAsTemplate reports whether the pod template tmpl has a regular container called name with the memory
// limit resources r give it, both unset included. Only then does the template line already show the pod's
// limit; a container the template lacks (an injected sidecar), or a subject without a template (a ReplicaSet
// or a pod), is never the same.
func sameAsTemplate(tmpl []TemplateContainer, name string, r corev1.ResourceRequirements) bool {
	i := slices.IndexFunc(tmpl, func(c TemplateContainer) bool { return c.Name == name })
	if i < 0 {
		return false
	}
	q, ok := r.Limits[corev1.ResourceMemory]
	want := tmpl[i].MemoryLimit
	if !ok || want == nil {
		return !ok && want == nil
	}
	// Cmp may store a decimal form in its receiver and its argument, never in the decimal they point to: q is
	// this function's copy and *want is passed by value, so neither the pod nor the template is changed.
	return q.Cmp(*want) == 0
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
			owner = ", controlled by " + l.data(b.value(ref.Kind)) + " " + l.data(b.value(ref.Name))
		}
		l.add("pod %s%s, phase %s, created %s", l.data(b.value(p.Name)), owner, l.data(b.value(string(p.Status.Phase))),
			stamp(p.CreationTimestamp))
		if p.Status.Reason != "" || p.Status.Message != "" {
			l.add("  status reason %s: %s", l.data(b.value(p.Status.Reason)), l.data(b.value(p.Status.Message)))
		}
		for _, c := range p.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				b.conditionLine(l, string(c.Type), string(c.Status), c.Reason, c.Message, c.LastTransitionTime)
			}
		}
		for _, cs := range allStatuses(p) {
			b.containerLine(l, p, cs, t.template)
		}
	}
}

// containerLine describes one container of a running pod, with the memory limit of that pod's own spec,
// labelled as the pod's: during a rollout it may differ from the pod template's (#494). It leaves the limit
// out only when the subject's pod template (tmpl) has the same container with the same limit, which the
// template line shows: with it repeated on the line that holds the restart count, oom diagnoses dropped that
// count and the metric lines (#499).
func (b *Builder) containerLine(l *lines, p *corev1.Pod, cs containerStatus, tmpl []TemplateContainer) {
	kind := "container"
	if cs.init {
		kind = "init container"
	}
	parts := []string{fmt.Sprintf("ready %t, restarts %d", cs.Ready, cs.RestartCount)}
	parts = append(parts, "now "+b.state(l, cs.State))
	if cs.LastTerminationState.Terminated != nil {
		parts = append(parts, "last "+b.state(l, cs.LastTerminationState))
	}
	if r, ok := podResources(p, cs.Name, cs.init); ok && (cs.init || !sameAsTemplate(tmpl, cs.Name, r)) {
		parts = append(parts, "this pod's memory limit "+b.memoryLimit(l, r))
	}
	l.add("  %s %s: %s", kind, l.data(b.value(cs.Name)), strings.Join(parts, "; "))
}

func (b *Builder) state(l *lines, s corev1.ContainerState) string {
	switch {
	case s.Waiting != nil:
		return fmt.Sprintf("waiting, reason %s: %s", l.data(b.value(s.Waiting.Reason)), l.data(b.value(s.Waiting.Message)))
	case s.Terminated != nil:
		x := s.Terminated
		return fmt.Sprintf("terminated, reason %s, exit code %d, signal %d, started %s, finished %s: %s",
			l.data(b.value(x.Reason)), x.ExitCode, x.Signal, stamp(x.StartedAt), stamp(x.FinishedAt), l.data(b.value(x.Message)))
	case s.Running != nil:
		return "running since " + stamp(s.Running.StartedAt)
	}
	return "unknown"
}
