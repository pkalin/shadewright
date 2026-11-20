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
	"fmt"
	"regexp"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// Class is the part an observation plays in the consequence rule of ADR-0039: a cause read from pod status,
// a consequence of a cause on the same container, or none.
type Class string

const (
	// ClassNone is an observation the rule does not touch: it goes through the gate as any other.
	ClassNone Class = ""
	// ClassCause is a pod-status observation that can link consequences: an OOMKilled termination, a non-zero
	// exit, a waiting reason of [WaitingCauseReasons], or the pod's eviction, which names no container (ADR-0039
	// point 3, amended 2026-11-20). Only an observation with [fingerprint.SourcePodStatus] has it.
	ClassCause Class = "cause"
	// ClassConsequence is a kubelet BackOff or Unhealthy event about a named container of the verified pod.
	ClassConsequence Class = "consequence"
)

// KillCauseWindow is how long after an OOM kill's finishedAt a consequence on its container still links to
// it (ADR-0039 point 5), and how far on either side of a non-zero exit's finishedAt (point 3, amended
// 2026-11-20). Both times are written by the kubelet of the pod's node.
const KillCauseWindow = 10 * time.Minute

// WaitingCauseReasons are the waiting reasons of a container's status that are a cause (ADR-0039 point 3): an
// image that cannot be pulled, or a configuration the kubelet cannot create the container from. Any other
// waiting reason is not read, and the waiting message never is: it is free text.
var WaitingCauseReasons = []string{
	"ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError",
}

// consequenceReasons are the reasons of a kubelet event that is a consequence when it names a container.
var consequenceReasons = []string{reasonBackOff, ReasonUnhealthy}

// reasonBackOff is the kubelet's reason for a container it waits to restart, or an image it waits to pull.
const reasonBackOff = "BackOff"

// maxFieldPath bounds the regarding field path the collector parses: the longest valid form is
// "spec.initContainers{" plus a 63 byte label and "}".
const maxFieldPath = len("spec.initContainers{}") + validation.DNS1123LabelMaxLength

// fieldPathRe is the only form of a regarding field path that names a container: the list and the name.
var fieldPathRe = regexp.MustCompile(`^spec\.(containers|initContainers)\{([^{}]+)\}$`)

// namedContainer returns the container an event's regarding field path names, and whether it is an init
// container, when the path is spec.containers{<name>} or spec.initContainers{<name>} with a DNS label name
// that exists in that list of pod's spec. pod must be the Pod the event's regarding reference names, as the
// collector verified it by UID; nil gives no container. Anything else gives "": the field path is untrusted.
func namedContainer(reg corev1.ObjectReference, pod *corev1.Pod) (string, bool) {
	if pod == nil || reg.Kind != kindPod || reg.APIVersion != apiCore || reg.UID == "" || reg.UID != pod.UID ||
		len(reg.FieldPath) > maxFieldPath {
		return "", false
	}
	m := fieldPathRe.FindStringSubmatch(reg.FieldPath)
	if m == nil || len(validation.IsDNS1123Label(m[2])) > 0 {
		return "", false
	}
	init := m[1] == "initContainers"
	list := pod.Spec.Containers
	if init {
		list = pod.Spec.InitContainers
	}
	if !slices.ContainsFunc(list, func(c corev1.Container) bool { return c.Name == m[2] }) {
		return "", false
	}
	return m[2], init
}

// kubeletTime is the event's own time as its reporter wrote it (lastOccurrence without its last fallback),
// and false when it has none: the creation timestamp is the API server's, another clock, so it does not
// count for the kill window (ADR-0039 point 5).
func kubeletTime(ev *eventsv1.Event) (time.Time, bool) {
	switch {
	case ev.Series != nil && !ev.Series.LastObservedTime.IsZero():
		return ev.Series.LastObservedTime.Time, true
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time, true
	case !ev.DeprecatedLastTimestamp.IsZero():
		return ev.DeprecatedLastTimestamp.Time, true
	}
	return time.Time{}, false
}

// eventClass is the class of an event observation: a consequence when the kubelet reports BackOff or Unhealthy
// on a named container, else none. An event is never a cause, whatever its reason (ADR-0039 point 2).
func eventClass(ev *eventsv1.Event, container string) Class {
	if container == "" || ev.ReportingController != reportingKubelet || !slices.Contains(consequenceReasons, ev.Reason) {
		return ClassNone
	}
	return ClassConsequence
}

// terminationNote is the symptom of a container termination read from pod status.
func terminationNote(container string, exitCode int32) string {
	return fmt.Sprintf("Container %s terminated with exit code %d", container, exitCode)
}

// waitingNote is the symptom of a waiting reason read from pod status.
func waitingNote(container, reason string) string {
	return fmt.Sprintf("Container %s is waiting: %s", container, reason)
}

// podStatusInput is the fingerprint input of an observation read from pod status, with owner as the pod's
// verified owner UID. The pod reconciler and the cause of a consequence both build it here, so a
// consequence's cause fingerprint is the fingerprint of the cause's own observation.
func (c *Collector) podStatusInput(pod *corev1.Pod, owner, reason, note string) fingerprint.Observation {
	return fingerprint.Observation{
		ClusterID:           c.cfg.ClusterID,
		Namespace:           pod.Namespace,
		OwnerUID:            owner,
		ResourceUID:         string(pod.UID),
		Reason:              reason,
		Type:                corev1.EventTypeWarning,
		ReportingController: reportingKubelet,
		Note:                note,
		RegardingKind:       kindPod,
		RegardingName:       pod.Name,
		Source:              fingerprint.SourcePodStatus,
	}
}

// causeFingerprint returns the fingerprint of the cause pod's status shows for the named container (init or
// not), or "" when it shows none (ADR-0039 point 3, amended 2026-11-20): a current waiting reason of
// WaitingCauseReasons first, because it is what the container does now; otherwise a termination, current or
// last, that is a cause (an OOM kill or a non-zero exit, as terminations reads them) and that is within
// KillCauseWindow of at, the observation's own time, the newest first. An OOM kill links only when at follows
// its finishedAt (point 5). A non-zero exit links on either side of at: the status keeps only the newest
// termination, and a BackOff or probe failure that the kubelet reported before it was about the termination
// before, of the same crash loop. Without that time (hasAt false), a termination links nothing. No clock of
// the collector takes part. owner is the pod's verified owner UID, as for the pod's own observations.
func (c *Collector) causeFingerprint(pod *corev1.Pod, owner, container string, init bool, at time.Time,
	hasAt bool) string {
	statuses := pod.Status.ContainerStatuses
	if init {
		statuses = pod.Status.InitContainerStatuses
	}
	i := slices.IndexFunc(statuses, func(s corev1.ContainerStatus) bool { return s.Name == container })
	if i < 0 {
		return ""
	}
	st := statuses[i]
	if w := st.State.Waiting; w != nil && slices.Contains(WaitingCauseReasons, w.Reason) {
		return fingerprint.Fingerprint(c.podStatusInput(pod, owner, w.Reason, waitingNote(container, w.Reason)))
	}
	if !hasAt {
		return ""
	}
	var cause *termination
	for _, t := range containerTerminations(st) {
		if t.finished.IsZero() || !linksAt(t, at) {
			continue
		}
		if cause == nil || t.finished.After(cause.finished) {
			cause = &t
		}
	}
	if cause == nil {
		return ""
	}
	return fingerprint.Fingerprint(c.podStatusInput(pod, owner, cause.reason,
		terminationNote(container, cause.exitCode)))
}

// linksAt reports whether termination t is within KillCauseWindow of at, the observation's own time: after
// its finish for an OOM kill (ADR-0039 point 5), on either side of it for a non-zero exit (point 3, amended
// 2026-11-20).
func linksAt(t termination, at time.Time) bool {
	finished := t.finished
	if at.After(finished.Add(KillCauseWindow)) {
		return false
	}
	if t.reason == ReasonOOMKilled {
		return !at.Before(finished)
	}
	return !at.Before(finished.Add(-KillCauseWindow))
}
