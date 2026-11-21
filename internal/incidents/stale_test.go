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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// failedMountOf returns the observation a restarted collector makes of a kubelet FailedMount event with total
// occurrences, begun 20 minutes before now and last seen 15 minutes before: a series that proves on its own
// that it recurs. Its subject is the pod, as for a pod that is gone and whose owner could not be read. The
// collector settled it with Handled, so it is not MayRepeat.
func failedMountOf(f, i int, total int64, now time.Time) collector.Observation {
	o := observation(f, i, total, total, now.Add(-20*time.Minute))
	o.ObservedAt = now.Add(-15 * time.Minute)
	o.Origin, o.Reason, o.Symptom = fingerprint.SourceEvent, "FailedMount", "MountVolume.SetUp failed for volume config"
	o.Subject = remediationv1alpha1.ObjectRef{
		APIVersion: "v1", Kind: "Pod", Namespace: o.Subject.Namespace, Name: "orders-7fb6899d5f-7pcr8",
		UID: "7a3f0c1e-2b4d-4e6f-9a8b-0c1d2e3f4a5b",
	}
	return o
}

// noIncident fails when the Incident of fp(f) exists.
func noIncident(t *testing.T, ns string, f int) {
	t.Helper()
	err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: Name(fp(f))}, &remediationv1alpha1.Incident{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Incident of fingerprint %d: %v, want not found", f, err)
	}
}

// After a restart, an event from before the start about a pod that is gone (FoldOnly) opens no Incident, even
// with a series that would open one at once, and folds into the Incident of its fingerprint when one exists
// (#996).
func TestFoldOnlyOpensNoIncident(t *testing.T) {
	ns := namespace(t)
	now := time.Now().Truncate(time.Second)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	stale := failedMountOf(1, 1, 14, now)
	if !recurs(stale) {
		t.Fatal("the observation does not recur on its own, so the test proves nothing")
	}
	stale.FoldOnly = true
	dropped := testutil.ToFloat64(droppedBatches.WithLabelValues(dropStaleOnStart))
	r.obs <- stale
	// A later observation that opens its Incident: once it is written, the first one was consumed.
	r.obs <- immediate(observation(2, 2, 1, 1, now))
	eventually(t, ns, 2, countIs(1))
	noIncident(t, ns, 1)
	// Logged with what traces it back to its event: occurrence, fingerprint, namespace and subject.
	s := stale.Subject
	if got := r.logs.with("Dropped an observation of an event from before the start",
		`"occurrence"="`+stale.Occurrence+`"`, `"fingerprint"="`+fp(1)+`"`, `"namespace"="`+s.Namespace+`"`,
		`"kind"="Pod"`, `"name"="`+s.Name+`"`); len(got) != 1 {
		t.Fatalf("the dropped observation was not logged with its occurrence and subject; lines %q", r.logs.with())
	}
	if n := testutil.ToFloat64(droppedBatches.WithLabelValues(dropStaleOnStart)) - dropped; n != 1 {
		t.Fatalf("batches_dropped_total{reason=stale_on_start} rose by %v, want 1", n)
	}
	// The same kind of observation folds into the Incident of its fingerprint, which exists now.
	fold := failedMountOf(2, 4, 14, now)
	fold.FoldOnly = true
	r.obs <- fold
	eventually(t, ns, 2, countIs(15))
	if n := testutil.ToFloat64(droppedBatches.WithLabelValues(dropStaleOnStart)) - dropped; n != 1 {
		t.Fatalf("batches_dropped_total{reason=stale_on_start} rose by %v with the fold, want 1", n)
	}
}

// An event's occurrence that an Incident counted stays on that Incident after a restart, when the collector
// now shows it with another fingerprint: the pod's subject instead of the Deployment it had (the owner chain of
// a deleted pod cannot be read). FoldOnly or not, it opens no Incident with the weaker subject.
func TestOccurrenceStaysOnTheIncidentThatCountedIt(t *testing.T) {
	for _, foldOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "new occurrences", true: "fold only"}[foldOnly], func(t *testing.T) {
			ns := namespace(t)
			now := time.Now().Truncate(time.Second)
			a := start(t, ns, Config{Window: 100 * time.Millisecond})
			// Before the restart: the Deployment's Incident counted 5 of the event.
			before := failedMountOf(1, 1, 5, now)
			before.Subject = observation(1, 1, 5, 5, now).Subject
			a.obs <- immediate(before)
			eventually(t, ns, 1, countIs(5))
			a.stop()

			b := start(t, ns, Config{Window: 100 * time.Millisecond})
			// After it: the same event, now with 9 occurrences, of the pod's fingerprint; 4 are new.
			after := failedMountOf(2, 1, 9, now)
			after.Count, after.FoldOnly = 4, foldOnly
			b.obs <- after
			inc := eventually(t, ns, 1, countIs(9))
			if inc.Spec.Subject.Kind != "Deployment" {
				t.Fatalf("subject %+v, want the Deployment", inc.Spec.Subject)
			}
			b.obs <- immediate(observation(3, 3, 1, 1, now))
			eventually(t, ns, 3, countIs(1))
			noIncident(t, ns, 2)
		})
	}
}

// An occurrence absorbed by a cause's Incident is not moved there as a count of its own: the observation is
// handled as before.
func TestOccurrenceOfACauseIncidentIsNotMoved(t *testing.T) {
	c, err := New(Config{Namespace: "x"})
	if err != nil {
		t.Fatal(err)
	}
	cause := Name(fp(1))
	c.names[cause] = struct{}{}
	c.causes[cause] = causeIncident{}
	c.setRecord(eventID(1), record{counted: counted{total: 5}, incident: cause})
	o := failedMountOf(2, 1, 9, time.Now())
	if _, ok := c.occurrenceIncident(o); ok {
		t.Fatal("an occurrence absorbed by a cause's Incident was moved there")
	}
	delete(c.causes, cause)
	if got, ok := c.occurrenceIncident(o); !ok || got != fp(1) {
		t.Fatalf("occurrence counted by an Incident: moved to %q, %v; want %q", got, ok, fp(1))
	}
	c.opening[cause] = struct{}{}
	if _, ok := c.occurrenceIncident(o); ok {
		t.Fatal("an occurrence was moved to an Incident this process is still opening")
	}
}

// Two guards of occurrenceIncident: a consequence is never moved (it is counted on its cause's Incident as
// evidence, or goes through the gate), and an occurrence is not moved to an Incident it would roll over from (a
// Resolved one past RecurrenceWindow): the gate of a new occurrence decides that, not the move.
func TestOccurrenceIncidentGuards(t *testing.T) {
	now := time.Now()
	setup := func(t *testing.T) (*Controller, collector.Observation) {
		t.Helper()
		c, err := New(Config{Namespace: "x", Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		name := Name(fp(1))
		c.names[name] = struct{}{}
		c.setRecord(eventID(1), record{counted: counted{total: 5}, incident: name})
		o := failedMountOf(2, 1, 9, now)
		if got, ok := c.occurrenceIncident(o); !ok || got != fp(1) {
			t.Fatalf("occurrence counted by an Incident: moved to %q, %v; want %q", got, ok, fp(1))
		}
		return c, o
	}
	t.Run("consequence", func(t *testing.T) {
		c, o := setup(t)
		o.Class = collector.ClassConsequence
		if _, ok := c.occurrenceIncident(o); ok {
			t.Fatal("a consequence was moved to the Incident that counted its occurrence")
		}
	})
	t.Run("rolls over", func(t *testing.T) {
		c, o := setup(t)
		c.resolvedAt[Name(fp(1))] = o.ObservedAt.Add(-RecurrenceWindow - time.Hour)
		if _, ok := c.occurrenceIncident(o); ok {
			t.Fatal("an occurrence was moved to a Resolved Incident past the recurrence window")
		}
		c.resolvedAt[Name(fp(1))] = o.ObservedAt.Add(-time.Hour)
		if _, ok := c.occurrenceIncident(o); !ok {
			t.Fatal("an occurrence was not moved to an Incident Resolved within the recurrence window")
		}
	})
}
