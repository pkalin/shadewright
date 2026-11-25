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
	"strings"
	"time"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/shadewright-labs/shadewright/internal/incidents/causereason"
	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
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
	// ClassConsequence is a kubelet BackOff, Unhealthy or image-pull Failed event about a named container of the
	// verified pod (ADR-0039 points 2 and 12, point 12 amended 2026-11-21 on #1066).
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

// reasonFailed is the kubelet's reason for a failed container action, including a failed image pull.
const reasonFailed = "Failed"

// pullFailedPrefix starts the kubelet's note when pulling a container's image fails:
// `Failed to pull image "<image>": <err>`.
const pullFailedPrefix = `Failed to pull image "`

// maxPullImageLen bounds the image reference inspected inside pullFailedPrefix so a hostile multi-megabyte
// note costs bounded work. Valid OCI image references (host, port, path, tag and digest) fit well within it.
const maxPullImageLen = 1024

// isImagePullFailedNote reports whether note of a kubelet Failed event has one of the kubelet's fixed forms
// for a container image pull failure (ADR-0039 point 12, amended 2026-11-21 on #1066):
//   - `Error: ErrImagePull`
//   - `Error: ImagePullBackOff`
//   - `Error: InvalidImageName`
//   - `Failed to pull image "<image>": <err>` with a non-empty printable ASCII image reference (no whitespace,
//     quotes or backslashes, at most maxPullImageLen bytes) and a non-empty error description.
//
// Anything else returns false (fail open: an unrecognised Failed event stays a symptom of its own).
func isImagePullFailedNote(note, image string) bool {
	if image == "" {
		return false
	}
	switch note {
	case "Error: ErrImagePull", "Error: ImagePullBackOff", "Error: InvalidImageName":
		return true
	}
	rest, ok := strings.CutPrefix(note, pullFailedPrefix)
	if !ok {
		return false
	}
	limit := min(len(rest), maxPullImageLen+1)
	closeIdx := -1
	for i := range limit {
		b := rest[i]
		if b == '"' {
			closeIdx = i
			break
		}
		if b <= ' ' || b >= 0x7f || b == '\\' {
			return false
		}
	}
	if closeIdx <= 0 || closeIdx > maxPullImageLen || !samePullImage(rest[:closeIdx], image) {
		return false
	}
	after, ok := strings.CutPrefix(rest[closeIdx+1:], ": ")
	if !ok {
		return false
	}
	for i := range min(len(after), maxPullImageLen) {
		if after[i] != ' ' && after[i] != '\t' && after[i] != '\r' && after[i] != '\n' {
			return true
		}
	}
	return false
}

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

// normalizedPullImage expands the registry, repository and default tag used by the kubelet.
func normalizedPullImage(image string) string {
	if image == "" || len(image) > maxPullImageLen {
		return ""
	}
	ref, err := reference.ParseDockerRef(image)
	if err != nil {
		return ""
	}
	return ref.String()
}

func samePullImage(a, b string) bool {
	ref := normalizedPullImage(a)
	return ref != "" && ref == normalizedPullImage(b)
}

// specPullImage identifies the requested pull even when status still reports a previous image.
func specPullImage(spec []corev1.Container, st corev1.ContainerStatus) string {
	if w := st.State.Waiting; w != nil && causereason.IsImagePull(w.Reason) {
		for _, c := range spec {
			if c.Name == st.Name {
				return c.Image
			}
		}
	}
	return ""
}

func containerSpec(pod *corev1.Pod, init bool) []corev1.Container {
	if init {
		return pod.Spec.InitContainers
	}
	return pod.Spec.Containers
}

// pullIdentity also distinguishes invalid spec values when InvalidImageName is the cause.
func pullIdentity(image string) string {
	if ref := normalizedPullImage(image); ref != "" {
		return ref
	}
	return image
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

// eventClass is the class of an event observation: a consequence when the kubelet reports BackOff, Unhealthy or
// an image-pull Failed event ([isImagePullFailedNote]) on a named container, else none. An event is never a
// cause, whatever its reason (ADR-0039 point 2, point 12 amended 2026-11-21).
func eventClass(ev *eventsv1.Event, pod *corev1.Pod, container string, init bool) Class {
	if container == "" || ev.ReportingController != reportingKubelet {
		return ClassNone
	}
	if slices.Contains(consequenceReasons, ev.Reason) {
		return ClassConsequence
	}
	if ev.Reason == reasonFailed {
		st, _ := containerStatus(pod, container, init)
		image := specPullImage(containerSpec(pod, init), st)
		// InvalidImageName can describe a spec value the reference parser rejects.
		invalid := ev.Note == "Error: InvalidImageName" && image != "" && image == st.Image &&
			st.State.Waiting.Reason == "InvalidImageName"
		if invalid || (samePullImage(image, st.Image) && isImagePullFailedNote(ev.Note, image)) {
			return ClassConsequence
		}
	}
	return ClassNone
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

// pullWaitReason groups the initial failure and the back-off for the same image.
func pullWaitReason(reason string) string {
	if reason == "ErrImagePull" {
		return "ImagePullBackOff"
	}
	return reason
}

// waitingFingerprint groups the two phases of a pull for one container and image. The observation
// keeps the reason and symptom the kubelet reported.
func waitingFingerprint(input fingerprint.Observation, t termination) (string, [2]PreviousPullWait) {
	var previous [2]PreviousPullWait
	if t.image == "" {
		return fingerprint.Fingerprint(input), previous
	}
	reasons := []string{pullWaitReason(input.Reason)}
	if reasons[0] == "ImagePullBackOff" {
		reasons = append(reasons, "ErrImagePull")
	}
	for i, reason := range reasons {
		input.Reason, input.Note = reason, waitingNote(t.container, reason)
		t.id = fmt.Sprintf("waiting-%s-restarts-%d", reason, t.restarts)
		previous[i] = PreviousPullWait{
			Fingerprint: fingerprint.Fingerprint(input),
			Occurrence:  t.occurrence(types.UID(input.ResourceUID)),
		}
	}
	return fingerprint.Scoped(previous[0].Fingerprint, t.container, t.image), previous
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
// the cause only for a held BackOff whose candidates are exits alone (none with fp set), once Config.CauseSeen
// confirmed it (hold.go); otherwise the consequence carries fp, as main links it. An Unhealthy event never gets one: a liveness probe failure makes the kubelet kill the container, so
// its exit 137 or 143 is a consequence of the probe failure, not its cause, and the status does not tell which way
// it went. The exits are those terminations offers with class cause (termination.class): a pod whose status.reason
// is set offers none, and a pod being deleted offers none either (#966).
//
// An image-pull Failed event (reason == reasonFailed) links only to a current image-pull waiting reason of its
// container (causereason.IsImagePull: ImagePullBackOff, ErrImagePull or InvalidImageName), never to a
// configuration error, an OOM kill or a non-zero exit (ADR-0039 point 12, amended 2026-11-21 on #1066).
//
// No clock of the collector takes part. owner is the pod's verified owner UID, as for the pod's own observations,
// and each fingerprint is the one of that cause's own observation. The terminations are matched on the
// container's name alone, across init and app containers: the API server refuses a pod whose init and app
// containers share a name, so a name picks one container of either list.
func (c *Collector) causeFingerprint(pod *corev1.Pod, owner, container string, init bool, at time.Time,
	hasAt bool, reason string) (fp, exit string, previous [2]PreviousPullWait) {
	st, found := containerStatus(pod, container, init)
	if !found {
		return "", "", previous
	}
	if w := st.State.Waiting; w != nil && causereason.IsWaiting(w.Reason) {
		image := pullIdentity(specPullImage(containerSpec(pod, init), st))
		if reason == reasonFailed && image == "" {
			return "", "", previous
		}
		input := c.podStatusInput(pod, owner, w.Reason, waitingNote(container, w.Reason))
		fp, previous = waitingFingerprint(input, termination{container: container, image: image, restarts: st.RestartCount, waiting: true})
		return fp, "", previous
	}
	if reason == reasonFailed {
		return "", "", previous
	}
	if !hasAt {
		return "", "", previous
	}
	kill := killOf(st, at)
	if kill != nil {
		fp = fingerprint.Fingerprint(c.podStatusInput(pod, owner, ReasonOOMKilled,
			terminationNote(container, kill.ExitCode)))
	}
	if reason != reasonBackOff {
		return fp, "", previous
	}
	var newest *termination
	for _, t := range terminations(pod) {
		if t.container != container || t.reason == ReasonOOMKilled || t.finished.IsZero() ||
			t.class(pod) != ClassCause || !exitLinksAt(t, at) {
			continue
		}
		if newest == nil || !t.finished.Before(newest.finished) {
			newest = &t
		}
	}
	if newest == nil || (kill != nil && !newest.finished.After(kill.FinishedAt.Time)) {
		return fp, "", previous
	}
	return fp, fingerprint.Fingerprint(c.podStatusInput(pod, owner, newest.reason,
		terminationNote(container, newest.exitCode))), previous
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
