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

package incidents

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// RecurrenceWindow is how long after resolvedTime a symptom seen again is taken as the same problem coming
// back (a fix that keeps coming undone): the Resolved Incident moves to Escalated for a person, with no
// analysis. Seen again later, the symptom opens the next occurrence of its fingerprint as a new Incident
// (design 3.2, #251). It is a constant for now; making it a setting is a later choice.
const RecurrenceWindow = 24 * time.Hour

// maxRolls bounds how many Resolved occurrences one write steps over to reach the latest. The controller
// starts from the latest occurrence it knows, so it is more than one only when another writer opened one this
// process has not seen.
const maxRolls = 8

// ConditionRecurred is the Incident condition the controller sets when a symptom of a Resolved Incident was
// seen again within the RecurrenceWindow, or when resolvedTime is missing, and so moved it to Escalated.
const ConditionRecurred = "Recurred"

// Reasons of the Recurred condition, always with status True.
const (
	// ReasonRecurredWithinWindow: the symptom was seen again within the RecurrenceWindow after resolvedTime.
	ReasonRecurredWithinWindow = "RecurredWithinWindow"
	// ReasonResolvedTimeUnknown: the Incident was Resolved without a resolvedTime, so the RecurrenceWindow
	// cannot be placed and the recurrence is escalated (fail closed).
	ReasonResolvedTimeUnknown = "ResolvedTimeUnknown"
)

// recurrence is what a symptom seen again does to a Resolved Incident.
type recurrence int

const (
	// notResolved: the Incident is not Resolved; the observation is counted as usual.
	notResolved recurrence = iota
	// escalate: seen again within the RecurrenceWindow, or resolvedTime is missing.
	escalate
	// nextOccurrence: seen again more than the RecurrenceWindow after resolvedTime.
	nextOccurrence
)

// onResolved decides what an observation last seen at seen does to an Incident whose status is st. A missing
// resolvedTime, or a seen time that is unknown, counts as within the window: the recurrence is escalated,
// never analyzed as a new occurrence.
func onResolved(st *remediationv1alpha1.IncidentStatus, seen time.Time) recurrence {
	if st.Phase != remediationv1alpha1.IncidentResolved {
		return notResolved
	}
	if st.ResolvedTime == nil || st.ResolvedTime.IsZero() || seen.IsZero() ||
		seen.Sub(st.ResolvedTime.Time) <= RecurrenceWindow {
		return escalate
	}
	return nextOccurrence
}

// escalateRecurrence moves a Resolved Incident whose symptom came back within the window to Escalated, with
// the condition Recurred saying why. The message holds nothing from the cluster.
func escalateRecurrence(inc *remediationv1alpha1.Incident, now time.Time) {
	reason, msg := ReasonRecurredWithinWindow,
		"The symptom was seen again within 24 hours of resolvedTime; a person decides, no analysis runs"
	if inc.Status.ResolvedTime == nil || inc.Status.ResolvedTime.IsZero() {
		reason, msg = ReasonResolvedTimeUnknown,
			"The symptom was seen again and resolvedTime is missing, so it counts as within 24 hours; a person decides, no analysis runs"
	}
	inc.Status.Phase = remediationv1alpha1.IncidentEscalated
	meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
		Type: ConditionRecurred, Status: metav1.ConditionTrue, Reason: reason, Message: msg,
		ObservedGeneration: inc.Generation, LastTransitionTime: metav1.NewTime(now.UTC().Truncate(time.Second)),
	})
}

// nextSpec returns the spec of occurrence n of a fingerprint, linked to occurrence n-1. Only the occurrence
// number and the previous name are added to what the observations gave; nothing of the earlier Incident's
// outcome is copied.
func nextSpec(spec remediationv1alpha1.IncidentSpec, n int32) remediationv1alpha1.IncidentSpec {
	spec.OccurrenceNumber = n
	spec.PreviousIncident = remediationv1alpha1.IncidentName(spec.Fingerprint, n-1)
	return spec
}

// capError is a new occurrence refused at the cap on Incidents: the batches meant for it are not stored.
// previous is the status of the Resolved Incident it would have followed, as read.
type capError struct {
	name     string
	previous *remediationv1alpha1.IncidentStatus
}

func (e *capError) Error() string {
	return "the cap on Incidents is reached: not opening the next occurrence " + e.name
}
