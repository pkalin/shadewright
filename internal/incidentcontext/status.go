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
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/redact"
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

// The values of the summary that can be shortened, in the order they are shortened (#796, decided
// 2026-11-16): the free text first, the subject's name last.
const (
	cutSymptom = iota
	cutController
	cutReason
	cutName
	cutValues
)

// summaryBlock describes the Incident, most relevant first. A cause Incident of a reason of causereason.Tallied with a
// non-empty absorbed tally gets the tally last and always whole (#796, decided 2026-11-16): the summary keeps room for the longest
// tally ([longestTally]) and fits its other lines in what is left. They fit by shortening values, never by
// removing a line: the symptom, then the reporting controller, then the reason, then the subject's name, each
// cut at a character boundary and followed by a note of the bytes cut ([fitSummary]). This holds at every
// summary bound the settings allow: with every value cut to nothing, the lines of an Incident at the schema's
// limits and the longest tally take 575 bytes, within the smallest bound, [MinSectionBytes]. Without a tally the same fit applies to
// the whole bound, and a summary that fits is what it always was.
func (b *Builder) summaryBlock(inc *remediationv1alpha1.Incident) Block {
	s, st := inc.Spec, inc.Status
	var vals [cutValues]string
	if s.Symptom != "" {
		vals[cutSymptom] = b.value(s.Symptom)
	}
	if s.ReportingController != "" {
		vals[cutController] = b.value(s.ReportingController)
	}
	vals[cutReason], vals[cutName] = b.value(s.Reason), b.value(s.Subject.Name)
	kind, namespace, typ := b.value(s.Subject.Kind), b.value(s.Subject.Namespace), b.value(s.Type)
	render := func(keep [cutValues]int) (*lines, [cutValues]summaryTail) {
		var l lines
		var tails [cutValues]summaryTail
		n := 0
		// add writes the line of value i: label, the value's head as quotable, its note, then rest(), which
		// records its own quotable values after the head, in the order of the line.
		add := func(i int, label string, rest func() string) {
			head, note := cutOf(vals[i], keep[i])
			head = l.data(head)
			tail := head + note + rest()
			if note != "" {
				tails[i] = summaryTail{line: n, text: tail}
			}
			l.add("%s%s", label, tail)
			n++
		}
		none := func() string { return "" }
		add(cutName, "subject: "+l.data(kind)+" "+l.data(namespace)+"/", none)
		add(cutReason, "reason: ", func() string { return " (" + l.data(typ) + ")" })
		if s.ReportingController != "" {
			add(cutController, "reported by: ", none)
		}
		if s.Symptom != "" {
			add(cutSymptom, "symptom: ", none)
		}
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
		return &l, tails
	}
	blk := Block{Section: SectionSummary, Provenance: ProvenanceIncident, Availability: Available}
	var t lines
	if b.tallyShown(s.Reason) {
		absorbedLines(&t, st.Absorbed)
	}
	if t.Len() == 0 {
		b.fitSummary(&blk, vals, render, b.limits.Summary)
		return blk
	}
	tally := b.redact(t.String())
	b.fitSummary(&blk, vals, render, b.limits.Summary-max(len(tally), longestTally))
	blk.unitAt = len(blk.Text)
	blk.Text += tally
	return blk
}

// summaryTail is where a shortened value stands in the rendered summary: its line, and the text from the start
// of the value to the end of that line (the head, the note of the cut and what follows on the line).
type summaryTail struct {
	line int
	text string
}

// fitSummary fits the summary lines render writes into n bytes of redacted text and sets them as blk's text.
// The fit is decided on the text after the redaction of the whole text, which stays as it is: it is the text
// the model sees. While that text is longer than n, values are shortened in the order of vals (cutSymptom
// first). While the redaction changes a shortened value or its note, that value's cut backs off to the start
// of the word it ends in: a note right after "token=" or "Authorization:" is read as the secret's value, and the
// note must stand intact in the final text, its number counting the bytes cut from the value as rendered. A
// cut never lands inside a redaction marker.
//
// The loop is bounded: each pass either lowers the kept length of one value, which starts at most at
// maxValueBytes and never goes below zero, or settles a value whose note the redaction changes even with
// nothing of it kept (text before the line, not the cut, is then taking it: a value of the line above), which
// happens at most once per value, or ends. So it ends after at most cutValues*(maxValueBytes+1) passes;
// in practice a handful. Only values beyond the schema's limits (Kind, namespace and type are not shortened)
// can leave the lines longer than n once nothing more can be shortened: the section's own cut then removes
// lines from the end, as for any other section.
func (b *Builder) fitSummary(blk *Block, vals [cutValues]string, render func([cutValues]int) (*lines, [cutValues]summaryTail), n int) {
	var keep [cutValues]int
	for i, v := range vals {
		keep[i] = len(v)
	}
	var settled [cutValues]bool
	for {
		l, tails := render(keep)
		text := b.redact(l.String())
		if d := damagedCut(l.String(), text, keep, vals, tails, settled); d >= 0 {
			if keep[d] == 0 {
				settled[d] = true
			} else {
				keep[d] = backOff(vals[d], keep[d])
			}
			continue
		}
		over := len(text) - n
		if over <= 0 {
			b.setSummary(blk, l, text, n, keep != lengths(vals))
			return
		}
		shortened := false
		for i := 0; i < cutValues && over > 0; i++ {
			k := shortenBy(vals[i], keep[i], over)
			if k < keep[i] {
				over -= segment(vals[i], keep[i]) - segment(vals[i], k)
				keep[i], shortened = k, true
			}
		}
		if !shortened {
			b.setSummary(blk, l, text, n, true)
			return
		}
	}
}

// setSummary sets text, the redacted lines of l, as blk's text, cut to n bytes in whole lines when it is
// still longer, and marks blk cut when it is, or when cut says a value was shortened.
func (b *Builder) setSummary(blk *Block, l *lines, text string, n int, cut bool) {
	text, whole := keepHead(text, n)
	if cut || whole {
		blk.addLabel(LabelSectionCut)
	}
	blk.Text, blk.Values = text, l.values
}

// lengths returns the length of each value: the kept lengths of values none of which is cut.
func lengths(vals [cutValues]string) [cutValues]int {
	var out [cutValues]int
	for i, v := range vals {
		out[i] = len(v)
	}
	return out
}

// damagedCut returns the first value that is cut and whose head, note or the rest of its line the redaction of
// the whole text changed, or -1. A redaction that changes the number of lines is taken as damage of the first
// cut value not settled. Values in settled are not checked again.
func damagedCut(plain, text string, keep [cutValues]int, vals [cutValues]string, tails [cutValues]summaryTail, settled [cutValues]bool) int {
	if plain == text {
		return -1
	}
	got := strings.Split(text, "\n")
	same := len(got) == strings.Count(plain, "\n")+1
	for i := range cutValues {
		if keep[i] == len(vals[i]) || settled[i] {
			continue
		}
		if !same || !strings.HasSuffix(got[tails[i].line], tails[i].text) {
			return i
		}
	}
	return -1
}

// cutNote is the note that follows a value of which n bytes were cut.
func cutNote(n int) string { return fmt.Sprintf(" (%d bytes cut)", n) }

// cutOf returns the head of v that keep bytes leave, and the note of the cut, empty when nothing is cut.
func cutOf(v string, keep int) (string, string) {
	if keep >= len(v) {
		return v, ""
	}
	return v[:keep], cutNote(len(v) - keep)
}

// segment is the length of v as the summary shows it with keep bytes kept: the head and its note.
func segment(v string, keep int) int {
	head, note := cutOf(v, keep)
	return len(head) + len(note)
}

// shortenBy returns how many bytes of v to keep so that v, kept to keep bytes so far, is shown by bytes
// shorter, or as short as it can be. The cut is at a rune boundary and outside any redaction marker. A cut
// that would not make v shorter, because of its note, returns keep: v stays as it is.
func shortenBy(v string, keep, by int) int {
	k := max(segment(v, keep)-by-len(cutNote(len(v))), 0)
	k = outsideMarkers(v, k)
	if segment(v, k) >= segment(v, keep) {
		return keep
	}
	return k
}

// backOff returns a cut of v shorter than keep, which is above zero: before the blanks keep ends in and the
// word before them, so that no key a secret's value follows ("token=", "Authorization:") is left right before
// the note.
func backOff(v string, keep int) int {
	k := keep
	for k > 0 && v[k-1] == ' ' {
		k--
	}
	for k > 0 && v[k-1] != ' ' {
		k--
	}
	return outsideMarkers(v, k)
}

// outsideMarkers moves a cut at k of v, which may fall inside a character, to the start of that character,
// and a cut inside a redaction marker, or anything shaped like one ([redact.MarkerShapes]), to the start of
// the marker.
func outsideMarkers(v string, k int) int {
	for k > 0 && k < len(v) && !utf8.RuneStart(v[k]) {
		k--
	}
	for _, m := range redact.MarkerShapes(v) {
		if m[0] < k && k < m[1] {
			return m[0]
		}
	}
	return k
}

// longestTally is the length of the longest tally [absorbedLines] can write: every reason of the table, the
// largest count and a time of the longest form. The summary keeps this much room whatever the tally holds,
// so where its other lines are cut does not move with the counts.
var longestTally = func() int {
	tally := make([]remediationv1alpha1.AbsorbedTally, 0, len(absorbedReasons))
	at := metav1.NewTime(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))
	for _, r := range absorbedReasons {
		tally = append(tally, remediationv1alpha1.AbsorbedTally{Reason: r, Count: math.MaxInt64, LastSeen: at})
	}
	var l lines
	absorbedLines(&l, tally)
	return l.Len()
}()

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
		if t.scaledObject != nil {
			blk.Provenance = ProvenanceScaledObject
			b.scaledObjectLines(&l, t.scaledObject)
			break
		}
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
			b.containerLine(l, p, cs)
		}
	}
}

// containerLine describes one container of a running pod, with the memory limit of that pod's own spec,
// labelled as the pod's: during a rollout it may differ from the pod template's.
func (b *Builder) containerLine(l *lines, p *corev1.Pod, cs containerStatus) {
	kind := "container"
	if cs.init {
		kind = "init container"
	}
	parts := []string{fmt.Sprintf("ready %t, restarts %d", cs.Ready, cs.RestartCount)}
	parts = append(parts, "now "+b.state(l, cs.State))
	if cs.LastTerminationState.Terminated != nil {
		parts = append(parts, "last "+b.state(l, cs.LastTerminationState))
	}
	if r, ok := podResources(p, cs.Name, cs.init); ok {
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

// absorbedReasons are the consequence reasons the summary shows from status.absorbed, in the order it shows
// them: the values of the AbsorbedReason enum (ADR-0039 point 16), the fixed table of the rule. An entry with
// any other reason is left out, so the lines hold no text from the cluster even if the schema let one in.
var absorbedReasons = []remediationv1alpha1.AbsorbedReason{
	remediationv1alpha1.AbsorbedBackOff,
	remediationv1alpha1.AbsorbedUnhealthy,
}

// absorbedLines writes the absorbed tally of a cause Incident, the last thing in the summary and always whole
// ([Builder.summaryBlock]; ADR-0039 point 18, #796): a heading, then one line per reason of [absorbedReasons]
// that the tally holds, in that order, with its count and the time of its latest observation. Only the reason
// (a code constant), a number and a time are written, and none of it is quotable: it is what the analyzer
// counted, not evidence from a source. An empty tally writes nothing, so the summary of every other Incident
// is unchanged. A reason found twice is shown once, from its first entry, so the lines are at most
// len(absorbedReasons).
func absorbedLines(l *lines, tally []remediationv1alpha1.AbsorbedTally) {
	header := false
	for _, reason := range absorbedReasons {
		i := slices.IndexFunc(tally, func(a remediationv1alpha1.AbsorbedTally) bool { return a.Reason == reason })
		if i < 0 {
			continue
		}
		if !header {
			l.add("consequences counted on this cause, not in its occurrences:")
			header = true
		}
		l.add("  %s: %d occurrences, last seen %s", string(reason), max(tally[i].Count, 0), stamp(tally[i].LastSeen))
	}
}
