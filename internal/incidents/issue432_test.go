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
	"slices"
	"testing"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// The reproductions of #432 at the Incident controller, with the observations the collector emits in each
// case since ADR-0039 point 3 was amended on 2026-11-20 (internal/analyzer/collector/issue432_test.go shows it
// emits them so, and holds the properties P1 to P5), in their order and at their times, t0 standing for the
// first sighting. Each requires one Incident for one cause. On main the collector emitted the crashes with no
// class, the BackOff with no cause, and a termination and an Evicted event per evicted pod: those inputs open
// two and three Incidents here, and the fix is in the collector, so these tests pin what the controller does
// with the new inputs, not the fix itself.

// opened returns the fingerprints with an Incident due to be written.
func opened(c *Controller) []string {
	fps := make([]string, 0, len(c.pending))
	for fp := range c.pending {
		fps = append(fps, fp)
	}
	slices.Sort(fps)
	return fps
}

// TestIssue432CrashLoopOpensTwoIncidents is cases (a) and (c) (P1): crashes of container web with exit code 2
// (reason Error, pod status, a cause) open the Error Incident on the second sighting, 20:27:28Z, with
// status.cause; the BackOff, linked by the collector to the Error's fingerprint, counts on it as evidence.
func TestIssue432CrashLoopOpensTwoIncidents(t *testing.T) {
	c, clock := newAt(t)
	crash := func(i int, at time.Time) {
		clock.set(at)
		o := symptomOf(1, "Error", terminationID(i), 1, fingerprint.SourcePodStatus, "web", collector.ClassCause, at)
		o.Began = at
		c.consume(t.Context(), o)
	}
	crash(1, t0.Add(20*time.Second))               // 20:25:20, held as a first sighting
	crash(2, t0.Add(2*time.Minute+28*time.Second)) // 20:27:28, opens the Error Incident
	if got := opened(c); !slices.Equal(got, []string{fp(1)}) {
		t.Fatalf("after the crashes %v are open, want the Error's %s", got, fp(1))
	}
	at := t0.Add(4*time.Minute + 15*time.Second) // 20:29:15
	clock.set(at)
	c.consume(t.Context(), consequenceOf(2, 1, "web", reasonBackOff, 3, 6, at))
	if got := opened(c); len(got) != 1 {
		t.Errorf("one crash loop opened %d Incidents %v, want 1", len(got), got)
	}
	if n, _ := absorbedOn(c, fp(1)); n != 6 {
		t.Errorf("the Error Incident holds %d BackOff as evidence, want 6", n)
	}
}

// TestIssue432EvictionOpensThreeIncidents is case (b) (P3): pods of one Deployment evicted for ephemeral
// storage one after the other. The collector emits one eviction per pod, read from pod status, a cause with no
// container, and nothing for the pods' terminations and Evicted events: the second evicted pod opens the one
// Incident, and the third counts on it.
func TestIssue432EvictionOpensThreeIncidents(t *testing.T) {
	c, clock := newAt(t)
	const evictionF = 11
	evict := func(i int, at time.Time) {
		clock.set(at)
		o := symptomOf(evictionF, "Evicted", terminationID(i), 1, fingerprint.SourcePodStatus, "",
			collector.ClassCause, at)
		o.Container, o.Began = "", at
		c.consume(t.Context(), o)
	}
	evict(1, t0.Add(20*time.Second))
	evict(2, t0.Add(2*time.Minute+27*time.Second))
	evict(3, t0.Add(4*time.Minute+24*time.Second))
	if got := opened(c); !slices.Equal(got, []string{fp(evictionF)}) {
		t.Fatalf("one eviction cause opened %v, want only %s", got, fp(evictionF))
	}
	if b := c.pending[fp(evictionF)]; len(b.containers) != 0 {
		t.Errorf("the eviction Incident names containers %v, want none", b.containers)
	}
}

// TestIssue432EvictionFoldsItsPodsConsequence is the late-cause fold of an eviction (decided 2026-11-21): the
// eviction is a cause of the pod, not of one container, and names every container of the pod
// (Observation.PodContainers), so a BackOff Incident of one of them that opened first is folded into it. The
// eviction named no container before, and the fold, which matches by container, left the BackOff open.
func TestIssue432EvictionFoldsItsPodsConsequence(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r.obs <- consequenceOf(2, 0, foldContainer, reasonBackOff, 1, 2, at)
	eventually(t, ns, 2, countIs(2))
	evict := symptomOf(11, "Evicted", terminationID(1), 1, fingerprint.SourcePodStatus, "", collector.ClassCause,
		at.Add(time.Minute))
	evict.Began = at.Add(time.Minute - RecurAfter)
	evict.PodContainers = []string{"init", foldContainer}
	r.obs <- evict
	cause := eventually(t, ns, 11, func(inc *remediationv1alpha1.Incident) bool { return inc.Status.Cause })
	if !slices.Equal(cause.Status.Containers, []remediationv1alpha1.Name{"init", foldContainer}) {
		t.Errorf("eviction Incident names containers %v, want its pod's init and web", cause.Status.Containers)
	}
	got := eventually(t, ns, 2, func(inc *remediationv1alpha1.Incident) bool { return inc.Status.FoldedInto != nil })
	if got.Status.FoldedInto.Name != cause.Name {
		t.Fatalf("BackOff folded into %s, want the eviction's %s", got.Status.FoldedInto.Name, cause.Name)
	}
}
