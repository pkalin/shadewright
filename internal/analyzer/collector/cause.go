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

	"github.com/pkalin/shadewright/internal/incidents/causereason"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// Class is the part an observation plays in the consequence rule of ADR-0039: a cause read from pod status,
// a consequence of a cause on the same container, or none.
type Class string

const (
	// ClassNone is an observation the rule does not touch: it goes through the gate as any other.
	ClassNone Class = ""
	// ClassCause is a pod-status observation that can link consequences: a container termination (an OOM kill
	// or a non-zero exit) or a waiting reason of [WaitingCauseReasons] (ADR-0039 point 3, amended 2026-11-20).
	// Only an observation with [fingerprint.SourcePodStatus] has it.
	ClassCause Class = "cause"
	// ClassConsequence is a kubelet BackOff or Unhealthy event about a named container of the verified pod.
	ClassConsequence Class = "consequence"
)

// TerminationCauseWindow bounds the link of a consequence to a termination of its container: at most this long
// after an OOM kill's finishedAt (ADR-0039 point 5), and, for a BackOff only, at most this long on either side of a
// non-zero exit's finishedAt (point 3, amended 2026-11-20 and narrowed 2026-11-21). Both times are written by the
// kubelet of the pod's node. It bounds the link of one observation only. The fold of a consequence Incident into
// its cause's (ADR-0039 point 12, in internal/incidents) has a window of its own, incidents.RecurWithin (15
// minutes), between the creation times of the two Incidents.
const TerminationCauseWindow = 10 * time.Minute

// WaitingCauseReasons returns a copy of the waiting reasons of a container's status that are a cause (ADR-0039
// point 3): an image that cannot be pulled, or a configuration the kubelet cannot create the container from. Any
// other waiting reason is not read, and the waiting message never is: it is free text. The list is
// causereason.Waiting, which the context builder reads too.
func WaitingCauseReasons() []string { return causereason.Waiting() }

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
// not), as main reads it, or "" when it shows none (ADR-0039 point 3): a current waiting reason of
// WaitingCauseReasons first, because it is what the container does now; otherwise an OOM kill, current or last,
// that finished at most TerminationCauseWindow before at, the observation's own time (killOf). Without that time
// (hasAt false), or with a time before the kill, the kill links nothing.
//
// exit is, for a BackOff only (backOff set) and when the status shows no waiting cause, the fingerprint of the
// newest non-zero exit of the container, current or last, within TerminationCauseWindow of at on either side, that
// finished after that OOM kill if there is one; "" otherwise (ADR-0039 point 3, amended 2026-11-20, narrowed
// 2026-11-21 on #432). The status keeps only the newest termination, so a BackOff the kubelet reported before an
// exit was about the exit before, of the same crash loop. exit is a candidate only: the collector delivers it as
// the cause only once Config.CauseSeen confirmed it (hold.go), and until then the consequence carries fp, as main
// links it. An Unhealthy event never gets one: a liveness probe failure makes the kubelet kill the container, so
// its exit 137 or 143 is a consequence of the probe failure, not its cause, and the status does not tell which way
// it went. The exits are those terminations offers with class cause (termination.class): a pod whose status.reason
// is set offers none, and a pod being deleted offers none either (#966).
//
// No clock of the collector takes part. owner is the pod's verified owner UID, as for the pod's own observations,
// and each fingerprint is the one of that cause's own observation. The terminations are matched on the
// container's name alone, across init and app containers: the API server refuses a pod whose init and app
// containers share a name, so a name picks one container of either list.
func (c *Collector) causeFingerprint(pod *corev1.Pod, owner, container string, init bool, at time.Time,
	hasAt, backOff bool) (fp, exit string) {
	statuses := pod.Status.ContainerStatuses
	if init {
		statuses = pod.Status.InitContainerStatuses
	}
	i := slices.IndexFunc(statuses, func(s corev1.ContainerStatus) bool { return s.Name == container })
	if i < 0 {
		return "", ""
	}
	st := statuses[i]
	if w := st.State.Waiting; w != nil && causereason.IsWaiting(w.Reason) {
		return fingerprint.Fingerprint(c.podStatusInput(pod, owner, w.Reason, waitingNote(container, w.Reason))), ""
	}
	if !hasAt {
		return "", ""
	}
	kill := killOf(st, at)
	if kill != nil {
		fp = fingerprint.Fingerprint(c.podStatusInput(pod, owner, ReasonOOMKilled,
			terminationNote(container, kill.ExitCode)))
	}
	if !backOff {
		return fp, ""
	}
	var newest *termination
	for _, t := range terminations(pod) {
		if t.container != container || t.reason == ReasonOOMKilled || t.finished.IsZero() ||
			t.class(pod) != ClassCause || !exitLinksAt(t, at) {
			continue
		}
		if newest == nil || t.finished.After(newest.finished) {
			newest = &t
		}
	}
	if newest == nil || (kill != nil && !newest.finished.After(kill.FinishedAt.Time)) {
		return fp, ""
	}
	return fp, fingerprint.Fingerprint(c.podStatusInput(pod, owner, newest.reason,
		terminationNote(container, newest.exitCode)))
}

// killOf returns the OOM kill of the container status st that a consequence at its own time at links to, as main
// reads it (ADR-0039 point 5): of the state and the last state, the newest OOM kill that finished at most
// TerminationCauseWindow before at, the state's on a tie; nil when none.
func killOf(st corev1.ContainerStatus, at time.Time) *corev1.ContainerStateTerminated {
	var kill *corev1.ContainerStateTerminated
	for _, t := range []*corev1.ContainerStateTerminated{st.State.Terminated, st.LastTerminationState.Terminated} {
		if t == nil || t.Reason != ReasonOOMKilled || t.FinishedAt.IsZero() {
			continue
		}
		finished := t.FinishedAt.Time
		if at.Before(finished) || at.After(finished.Add(TerminationCauseWindow)) {
			continue
		}
		if kill == nil || finished.After(kill.FinishedAt.Time) {
			kill = t
		}
	}
	return kill
}

// exitLinksAt reports whether the non-zero exit t is within TerminationCauseWindow of at, the observation's own
// time, on either side of its finish (point 3, amended 2026-11-20). An OOM kill is killOf's.
func exitLinksAt(t termination, at time.Time) bool {
	return !at.After(t.finished.Add(TerminationCauseWindow)) && !at.Before(t.finished.Add(-TerminationCauseWindow))
}
