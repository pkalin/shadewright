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

	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// The reproductions of #432 at the Incident controller, with the observations the collector emits in each
// case (internal/analyzer/collector/issue432_test.go shows it emits them so), in their order and at their
// times, t0 standing for the first sighting. Each requires one Incident for one cause; main opens more.

// opened returns the fingerprints with an Incident due to be written.
func opened(c *Controller) []string {
	fps := make([]string, 0, len(c.pending))
	for fp := range c.pending {
		fps = append(fps, fp)
	}
	slices.Sort(fps)
	return fps
}

// TestIssue432CrashLoopOpensTwoIncidents is cases (a) and (c): crashes of container web with exit code 2
// (reason Error, pod status, no class) open the Error Incident on the second sighting, 20:27:28Z; the BackOff
// released by the collector's hold at the end of ConsequenceGrace, 20:29:15Z, recurs and opens its own. It does
// so as emitted on main (no cause fingerprint), and also when the collector would link it to the Error's
// fingerprint: only a cause observation (class cause from pod status, isCause) makes an Incident one that a
// consequence is counted on (causeIncident reads c.causes), and the Error termination is none.
func TestIssue432CrashLoopOpensTwoIncidents(t *testing.T) {
	t.Skip("#432: BackOff of a crash loop opens its own Incident; the Error termination is no cause (collector class none, isCause false)")
	for _, linked := range []bool{false, true} {
		name := map[bool]string{false: "as emitted on main", true: "linked by the collector"}[linked]
		t.Run(name, func(t *testing.T) {
			c, clock := newAt(t)
			crash := func(i int, at time.Time) {
				clock.set(at)
				o := symptomOf(1, "Error", terminationID(i), 1, fingerprint.SourcePodStatus, "web", collector.ClassNone, at)
				o.Began = at
				c.consume(t.Context(), o)
			}
			crash(1, t0.Add(20*time.Second))               // 20:25:20, held as a first sighting
			crash(2, t0.Add(2*time.Minute+28*time.Second)) // 20:27:28, opens the Error Incident
			if got := opened(c); !slices.Equal(got, []string{fp(1)}) {
				t.Fatalf("after the crashes %v are open, want the Error's %s", got, fp(1))
			}
			at := t0.Add(4*time.Minute + 15*time.Second) // 20:29:15, released at the end of the hold
			clock.set(at)
			cause := 0
			if linked {
				cause = 1
			}
			c.consume(t.Context(), consequenceOf(2, cause, "web", reasonBackOff, 3, 6, at))
			if got := opened(c); len(got) != 1 {
				t.Errorf("one crash loop opened %d Incidents %v, want 1", len(got), got)
			}
		})
	}
}

// TestIssue432EvictionOpensThreeIncidents is case (b): pods of one Deployment evicted for ephemeral storage one
// after the other. Each fingerprint (Error and ContainerStatusUnknown terminations from pod status, the
// kubelet's Evicted event, all of no class and of the same Deployment) opens its Incident on its second
// sighting, from the next evicted pod: 21:14:27Z, 21:16:24Z and 21:17:26Z.
func TestIssue432EvictionOpensThreeIncidents(t *testing.T) {
	t.Skip("#432: the terminations and the Evicted event of an evicted pod are each a symptom of their own; no rule links them")
	c, clock := newAt(t)
	const errorF, unknownF, evictedF = 11, 12, 13
	see := func(f int, reason, id string, origin fingerprint.Source, container string, at time.Time) {
		clock.set(at)
		o := symptomOf(f, reason, id, 1, origin, container, collector.ClassNone, at)
		o.Began = at
		c.consume(t.Context(), o)
	}
	see(errorF, "Error", terminationID(1), fingerprint.SourcePodStatus, "web", t0.Add(20*time.Second))
	see(unknownF, "ContainerStatusUnknown", terminationID(2), fingerprint.SourcePodStatus, "web",
		t0.Add(24*time.Second))
	see(evictedF, "Evicted", eventID(1), fingerprint.SourceEvent, "", t0.Add(26*time.Second))
	see(errorF, "Error", terminationID(3), fingerprint.SourcePodStatus, "web", t0.Add(2*time.Minute+27*time.Second))
	see(unknownF, "ContainerStatusUnknown", terminationID(4), fingerprint.SourcePodStatus, "web",
		t0.Add(4*time.Minute+24*time.Second))
	see(evictedF, "Evicted", eventID(2), fingerprint.SourceEvent, "", t0.Add(5*time.Minute+26*time.Second))
	if got := opened(c); len(got) != 1 {
		t.Errorf("one eviction cause opened %d Incidents %v, want 1", len(got), got)
	}
}
