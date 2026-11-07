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
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
)

// RecurrenceWindow is how long after resolvedTime a symptom seen again is taken as the same problem coming
// back (a fix that keeps coming undone): the Resolved Incident moves to Escalated for a person, with no
// analysis. Seen again later, the symptom opens the next occurrence of its fingerprint as a new Incident
// (design 3.2, #251). It is a constant for now; making it a setting is a later choice.
const RecurrenceWindow = 24 * time.Hour

// ConditionRecurred is the Incident condition the controller sets when a symptom of a Resolved Incident was
// seen again after resolvedTime and within the RecurrenceWindow, or when resolvedTime is missing, and so moved
// it to Escalated.
const ConditionRecurred = "Recurred"

// Reasons of the Recurred condition, always with status True.
const (
	// ReasonRecurredWithinWindow: the symptom was seen again after resolvedTime, within the RecurrenceWindow,
	// or later while observations of the Resolved Incident were still unwritten (fail closed).
	ReasonRecurredWithinWindow = "RecurredWithinWindow"
	// ReasonResolvedTimeUnknown: the Incident was Resolved without a resolvedTime, so the RecurrenceWindow
	// cannot be placed and the recurrence is escalated (fail closed).
	ReasonResolvedTimeUnknown = "ResolvedTimeUnknown"
)

// recurrence is what a write of observations does to an Incident.
type recurrence int

const (
	// notResolved: the Incident is not Resolved; the observations are counted as usual.
	notResolved recurrence = iota
	// beforeResolution: the Incident is Resolved and every observation was seen no later than resolvedTime,
	// so none is a recurrence: they are counted and the phase stays Resolved.
	beforeResolution
	// escalate: seen again after resolvedTime, or resolvedTime or the time seen is unknown.
	escalate
)

// onResolved decides what observations last seen at seen do to an Incident whose status is st. A missing
// resolvedTime, or a seen time that is unknown, counts as a recurrence within the window: it is escalated,
// never analyzed. The controller sends an observation more than the window after resolvedTime to the next
// occurrence before it is written ([Controller.rolls]); one that still reaches a Resolved Incident (seen
// while observations of it were unwritten) is escalated too.
func onResolved(st *remediationv1alpha1.IncidentStatus, seen time.Time) recurrence {
	if st.Phase != remediationv1alpha1.IncidentResolved {
		return notResolved
	}
	if st.ResolvedTime == nil || st.ResolvedTime.IsZero() || seen.IsZero() {
		return escalate
	}
	if !seen.After(st.ResolvedTime.Time) {
		return beforeResolution
	}
	return escalate
}

// escalateRecurrence moves a Resolved Incident whose symptom came back to Escalated, with the condition
// Recurred saying why. The message holds nothing from the cluster.
func escalateRecurrence(inc *remediationv1alpha1.Incident, now time.Time) {
	reason, msg := ReasonRecurredWithinWindow,
		"The symptom was seen again after resolvedTime and within 24 hours of it; a person decides, no analysis runs"
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

// occurrenceOf returns the occurrence number in name, 1 for a name without one or that is not an Incident
// name.
func occurrenceOf(name string) int32 {
	if _, n, ok := remediationv1alpha1.ParseIncidentName(name); ok {
		return n
	}
	return 1
}

// noteState remembers whether inc is Resolved with a known resolvedTime, which decides whether a symptom seen
// again opens its next occurrence ([Controller.rolls]). The caller holds mu.
func (c *Controller) noteState(inc *remediationv1alpha1.Incident) {
	st := &inc.Status
	if inc.HasDerivedName() && st.Phase == remediationv1alpha1.IncidentResolved && st.ResolvedTime != nil &&
		!st.ResolvedTime.IsZero() {
		c.resolvedAt[inc.Name] = st.ResolvedTime.Time
		return
	}
	delete(c.resolvedAt, inc.Name)
}

// noteStates replaces what the controller remembers of the Resolved Incidents with what items, a listing of
// every Incident in the namespace, shows, so a phase another writer set is seen within a trigger pass and the
// memory holds only Incidents that exist.
func (c *Controller) noteStates(items []remediationv1alpha1.Incident) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.resolvedAt)
	for i := range items {
		c.noteState(&items[i])
	}
}

// rolls reports whether observation o of a fingerprint goes to the next occurrence instead of the latest one:
// the latest is Resolved with a resolvedTime more than RecurrenceWindow before o was observed, and nothing of
// the fingerprint is unwritten (an observation held for the latest one is written there, and escalates it).
// Such an observation passes the same gate as one of a new fingerprint ([Controller.consume]). The caller
// holds mu.
func (c *Controller) rolls(o collector.Observation) bool {
	fp := o.Fingerprint
	resolved, ok := c.resolvedAt[c.nameOf(fp)]
	if !ok || c.pending[fp] != nil || c.failed[fp] != nil || c.heldBy(fp) > 0 {
		return false
	}
	return c.notFuture(o.ObservedAt).Sub(resolved) > RecurrenceWindow
}

// deletedError is a write to a later occurrence this process did not open that no longer exists: someone
// deleted it (the clean-up at the cap). It is never created again from memory; latest is the newest
// occurrence of the fingerprint that still exists, or nil when none does.
type deletedError struct {
	name   string
	latest *remediationv1alpha1.Incident
}

func (e *deletedError) Error() string {
	return fmt.Sprintf("incident %s was deleted; it is not created again without a new recurrence", e.name)
}

// latestOccurrence lists the Incidents from the API server and returns the newest occurrence of fp that
// exists, or nil.
func (c *Controller) latestOccurrence(ctx context.Context, fp string) (*remediationv1alpha1.Incident, error) {
	var list remediationv1alpha1.IncidentList
	if err := c.reader.List(ctx, &list, client.InNamespace(c.cfg.Namespace)); err != nil {
		return nil, fmt.Errorf("listing the Incidents: %w", err)
	}
	var latest *remediationv1alpha1.Incident
	for i := range list.Items {
		inc := &list.Items[i]
		if inc.Spec.Fingerprint == fp && inc.HasDerivedName() &&
			(latest == nil || inc.Spec.Occurrence() > latest.Spec.Occurrence()) {
			latest = inc
		}
	}
	return latest, nil
}

// notResolvedError is an occurrence not opened because the one before it is no longer Resolved past the
// window when read from the API server (an escalation or another write the controller's memory had not
// caught up with). previous is that Incident as read, or nil when it is gone.
type notResolvedError struct {
	previous *remediationv1alpha1.Incident
}

func (e *notResolvedError) Error() string {
	return "the occurrence before is not Resolved more than 24 hours before the observations"
}

// predecessorResolved reads occurrence n-1 of spec (n is spec.OccurrenceNumber) from the API server and
// returns nil only when it is Resolved with a resolvedTime more than RecurrenceWindow before the earliest
// observation in batches, and a [notResolvedError] otherwise.
func (c *Controller) predecessorResolved(ctx context.Context, spec remediationv1alpha1.IncidentSpec,
	batches []*batch) error {
	prev := &remediationv1alpha1.Incident{}
	key := client.ObjectKey{Namespace: c.cfg.Namespace, Name: spec.PreviousIncident}
	if err := c.reader.Get(ctx, key, prev); apierrors.IsNotFound(err) {
		return &notResolvedError{}
	} else if err != nil {
		return fmt.Errorf("reading Incident %s before opening the next occurrence: %w", key.Name, err)
	}
	var first time.Time
	for _, b := range batches {
		first = earliest(first, b.first)
	}
	st := &prev.Status
	if st.Phase != remediationv1alpha1.IncidentResolved || st.ResolvedTime == nil || st.ResolvedTime.IsZero() ||
		first.IsZero() || first.Sub(st.ResolvedTime.Time) <= RecurrenceWindow {
		return &notResolvedError{previous: prev}
	}
	return nil
}

// reopen sends the batches meant for an occurrence that was not opened back to the one before it, which is
// the latest again: their records move with them, they are written there at once (where a write to a
// Resolved Incident escalates it), and the memory takes that Incident's phase as read. The caller holds mu.
func (c *Controller) reopen(ctx context.Context, fp, name string, batches []*batch, back *notResolvedError) {
	c.unopen(fp, name)
	prev := c.nameOf(fp)
	merged := combine(batches)
	if q := c.pending[fp]; q != nil {
		merged.add(q)
	}
	c.pending[fp] = merged
	c.retag(name, prev, merged)
	if back.previous != nil {
		c.noteState(back.previous)
	} else {
		delete(c.resolvedAt, prev)
	}
	delete(c.nextWrite, fp)
	logf.FromContext(ctx).Info("Did not open the next occurrence of an Incident that is no longer Resolved",
		"incident", name, "previous", prev)
}

// retag moves the records of the occurrences b holds from the Incident from to the Incident to. The caller
// holds mu.
func (c *Controller) retag(from, to string, b *batch) {
	for id := range b.occurrences {
		if r, ok := c.records[id]; ok && r.incident == from {
			c.setRecord(id, record{counted: r.counted, incident: to})
		}
	}
}
