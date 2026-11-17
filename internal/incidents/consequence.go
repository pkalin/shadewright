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

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// maxLoggedContainer bounds the bytes of a refused container name that are logged.
const maxLoggedContainer = 80

// isCause reports whether o is a cause of the consequence rule (ADR-0039 points 2 and 8): of class cause and
// read from pod status. Only such an observation sets status.cause; an Event never does, whatever its reason.
func isCause(o collector.Observation) bool {
	return o.Class == collector.ClassCause && o.Origin == fingerprint.SourcePodStatus
}

// absorbedReason returns the reason of the fixed table that a consequence is counted under, or false for a
// reason outside it. The value stored comes from the table, never from the observation's text.
func absorbedReason(reason string) (remediationv1alpha1.AbsorbedReason, bool) {
	switch remediationv1alpha1.AbsorbedReason(reason) {
	case remediationv1alpha1.AbsorbedBackOff:
		return remediationv1alpha1.AbsorbedBackOff, true
	case remediationv1alpha1.AbsorbedUnhealthy:
		return remediationv1alpha1.AbsorbedUnhealthy, true
	}
	return "", false
}

// containerName returns name when the Incident schema accepts it in status.containers (a DNS label), and
// empty otherwise, so a status write never fails on a name read from the cluster. A name left out is logged,
// quoted and cut short; an observation without a container is never counted as evidence (ADR-0039 point 11).
func containerName(ctx context.Context, name string) string {
	if name == "" || len(validation.IsDNS1123Label(name)) == 0 {
		return name
	}
	shown := name
	if len(shown) > maxLoggedContainer {
		shown = shown[:maxLoggedContainer]
	}
	logf.FromContext(ctx).Info("Left out a container name that is not a DNS label", "container", shown,
		"length", len(name))
	return ""
}

// causeIncident is an Incident that counts a cause: the spec it was opened with, and its UID once a write or
// a listing showed it with status.cause. A listing that shows the name with another UID and no cause (the
// Incident was deleted and created again) forgets it ([Controller.noteState]).
type causeIncident struct {
	spec remediationv1alpha1.IncidentSpec
	uid  types.UID
}

// causeFor returns the Incident a consequence observation is counted on as evidence (ADR-0039 point 6): the
// Incident [Controller.causeIncident] finds for its CauseFingerprint, subject UID and time. Not linked, besides,
// is an observation that is not a consequence, names no container or no cause, or carries a reason outside the
// table. The caller holds mu.
func (c *Controller) causeFor(o collector.Observation) (string, remediationv1alpha1.IncidentSpec, remediationv1alpha1.AbsorbedReason, bool) {
	none := remediationv1alpha1.IncidentSpec{}
	if o.Class != collector.ClassConsequence || o.CauseFingerprint == "" || o.Container == "" {
		return "", none, "", false
	}
	reason, ok := absorbedReason(o.Reason)
	if !ok {
		return "", none, "", false
	}
	name, spec, ok := c.causeIncident(o.CauseFingerprint, o.Subject.UID, o.ObservedAt)
	if !ok {
		return "", none, "", false
	}
	return name, spec, reason, true
}

// causeIncident is the one place that decides whether a consequence of cause fingerprint fp, of the subject
// with this UID and observed at, links to a cause's Incident (ADR-0039 point 6): the latest Incident of fp, when
// the controller's own list has it as a cause (status.cause, or a cause observation counted for it and not
// written yet) for that subject UID. The Incident's phase is never read: an Escalated, Proposed or Resolved
// cause takes the evidence for as long as the pod status shows the cause, which the collector decided. Not
// linked: no such Incident (refused at the cap, not counted for want of room, held as a first sighting), an
// empty subject UID or another one, and an Incident Resolved more than RecurrenceWindow before at, which a new
// cause observation opens again as its next occurrence. Both the link ([Controller.causeFor]) and the answer to
// the collector's hold ([Controller.causeSeen], decided 2026-11-17 on #828) go through it, so the hold never
// ends for a cause the consequence would not be counted on. The caller holds mu.
func (c *Controller) causeIncident(fp string, uid remediationv1alpha1.UID, at time.Time) (string, remediationv1alpha1.IncidentSpec, bool) {
	none := remediationv1alpha1.IncidentSpec{}
	if fp == "" || uid == "" {
		return "", none, false
	}
	name := c.nameOf(fp)
	if _, ok := c.names[name]; !ok {
		return "", none, false
	}
	cause, ok := c.causes[name]
	if !ok || cause.spec.Fingerprint != fp || cause.spec.Subject.UID != uid {
		return "", none, false
	}
	if resolved, ok := c.resolvedAt[name]; ok && c.notFuture(at).Sub(resolved) > RecurrenceWindow {
		return "", none, false
	}
	return name, cause.spec, true
}

// CauseSeen answers the collector (collector.CauseSeenFunc, ADR-0039 point 7, decided 2026-11-17 three times): whether
// the controller has seen a cause that consequence o would be counted on ([Controller.causeSeen]). The Incidents
// loaded at start count, so the hold the collector rebuilds after a restart is answered from the stored
// Incidents. A consequence whose container name is not a DNS label is answered true: consume leaves that name
// out (containerName) and then never links it to its cause, so holding it gains nothing. It reads nothing from
// the API server: it costs one lock and a few map lookups. It waits until the Incidents are loaded, or ctx ends,
// which is an error and no answer. A cause seen later is told to the collector through [Controller.OnCauseSeen].
func (c *Controller) CauseSeen(ctx context.Context, o collector.Observation) (bool, error) {
	select {
	case <-c.ready:
	case <-ctx.Done():
		return false, fmt.Errorf("waiting for the Incidents to load: %w", ctx.Err())
	}
	if o.Container != "" && len(validation.IsDNS1123Label(o.Container)) > 0 {
		return true, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.causeSeen(o), nil
}

// causeSeen reports whether the controller has seen a cause consequence o would be counted on: exactly when
// [Controller.causeFor] links it now (decided 2026-11-17, third, on #828). A cause that is only a first sighting
// held by the gate is not seen: a consequence released on it would not be linked, could open an Incident of its
// own, and would not be counted when the cause opens later. Its hold waits until the cause's Incident opens, or
// until ConsequenceGrace ends (cause_unseen), when the late-cause fold takes over. A cause refused at the cap,
// not counted for want of room, of another subject UID, or whose Incident was Resolved more than
// RecurrenceWindow before o was observed, is not seen either. The caller holds mu.
func (c *Controller) causeSeen(o collector.Observation) bool {
	_, _, _, ok := c.causeFor(o)
	return ok
}

// causeOpenedFor reports whether cause observation o, just consumed, has an Incident that a consequence of the
// same subject observed at the same time would be counted on ([Controller.causeIncident]): the test
// [Controller.handle] tells the collector by. A cause held by the gate as a first sighting tells nothing; it is
// told when an observation of it opens its Incident. The collector then asks [Controller.CauseSeen] for each
// hold of that cause, so a hold of another subject, or observed at another time, is decided by its own answer.
// The caller holds mu.
func (c *Controller) causeOpenedFor(o collector.Observation) bool {
	_, _, ok := c.causeIncident(o.Fingerprint, o.Subject.UID, o.ObservedAt)
	return ok
}

// absorbHeld counts on the cause's Incident name, as evidence, the observations of consequence fingerprint
// fp held as a first sighting that are linked to that Incident (ADR-0039 point 16): a consequence seen before
// its cause's Incident was opened was held, and the observation that links it now counts only what is new of
// it ([Controller.increase] reads the held total), so without this the held part would never reach the
// tally. It runs for a later observation of the consequence, and for each cause observation
// ([Controller.absorbHeldFor]). Held observations linked elsewhere, or to nothing, stay held. It reports what consume does. The
// caller holds mu.
func (c *Controller) absorbHeld(ctx context.Context, fp, name string, spec remediationv1alpha1.IncidentSpec) bool {
	held := c.sightings.takeWhere(fp, func(h collector.Observation) bool {
		linked, _, _, ok := c.causeFor(h)
		return ok && linked == name
	})
	due := false
	for _, h := range held {
		reason, _ := absorbedReason(h.Reason)
		due = c.absorb(ctx, h, name, spec, reason) || due
	}
	return due
}

// absorbHeldFor counts on the Incident of cause fingerprint cause, as evidence, what is held as a first
// sighting for its consequences (ADR-0039 point 6, #820): a consequence seen once before its cause was
// consumed is held by the gate, and nothing else would count it if it is never seen again. It runs after
// each cause observation is consumed, so it also covers an Incident that existed before and became a cause
// only now. It finds the held consequences through the sightings' index by cause fingerprint, so it costs
// one map lookup plus, per consequence fingerprint held for that cause, the observations held for it (at most
// maxHeldPerFingerprint); it never scans the first sightings. Each held observation is linked by
// [Controller.causeFor], from the controller's own list, so one of another subject UID, or whose cause has no
// Incident (refused at the cap, held as a first sighting itself), stays held as before. It reports what
// consume does. The caller holds mu.
func (c *Controller) absorbHeldFor(ctx context.Context, cause string) bool {
	fps := c.sightings.consequencesOf(cause)
	if len(fps) == 0 {
		return false
	}
	name := c.nameOf(cause)
	inc, ok := c.causes[name]
	if !ok || inc.spec.Fingerprint != cause {
		return false
	}
	due := false
	for _, fp := range fps {
		due = c.absorbHeld(ctx, fp, name, inc.spec) || due
	}
	return due
}

// absorb counts consequence o as evidence on its cause's Incident name (ADR-0039 point 16): into the pending
// batch of the cause's fingerprint, under reason, as an absorbed occurrence. It changes neither the Incident's
// count nor its first or last seen, so the trigger and the quiet rule never see it, and it takes no place
// under the Incident cap (point 25). A new occurrence over the caps on occurrences held unwritten is not
// counted, as in [Controller.count]. It reports what consume does. The caller holds mu.
func (c *Controller) absorb(ctx context.Context, o collector.Observation, name string,
	spec remediationv1alpha1.IncidentSpec, reason remediationv1alpha1.AbsorbedReason) bool {
	fp := o.CauseFingerprint
	b := c.pending[fp]
	if _, ok := b.occurrenceOf(o.Occurrence); !ok {
		if c.full(fp, o.Source) {
			c.overflow(ctx, fp, o.Source)
			return false
		}
		c.hold(fp, o.Source)
	}
	if b == nil {
		// The spec the cause's Incident was opened with: a write that finds it gone creates nothing for
		// consequences alone ([Controller.write]).
		base := spec
		base.OccurrenceNumber, base.PreviousIncident = 0, ""
		b = &batch{spec: base, occurrences: map[string]heldOccurrence{}}
		c.pending[fp] = b
	}
	_, scheduled := c.nextWrite[fp]
	at := c.notFuture(o.ObservedAt)
	b.add(&batch{
		absorbed:   map[remediationv1alpha1.AbsorbedReason]tally{reason: {count: o.Count, last: at}},
		containers: []string{o.Container},
		occurrences: map[string]heldOccurrence{
			o.Occurrence: {counted: counted{total: o.Total, began: o.Began}, source: o.Source, absorbed: reason},
		},
	})
	if occurrenceID.MatchString(o.Occurrence) {
		cur := c.records[o.Occurrence]
		c.setRecord(o.Occurrence, record{counted: higher(cur.counted, counted{total: o.Total, began: o.Began}), incident: name})
	}
	return !scheduled
}
