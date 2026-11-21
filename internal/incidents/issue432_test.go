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

// The reproduction of #432 at the Incident controller, with the observations the collector emits for a crash
// loop since ADR-0039 point 3 was amended on 2026-11-20 (internal/analyzer/collector/issue432_test.go shows it
// emits them so), in their order and at their times, t0 standing for the first sighting. It requires one
// Incident for one cause. On main the collector emitted the crashes with no class and the BackOff with no
// cause: those inputs open two Incidents here, and the fix is in the collector, so this test pins what the
// controller does with the new inputs, not the fix itself.

// opened returns the fingerprints with an Incident due to be written.
func opened(c *Controller) []string {
	fps := make([]string, 0, len(c.pending))
	for fp := range c.pending {
		fps = append(fps, fp)
	}
	slices.Sort(fps)
	return fps
}

// TestIssue432CrashLoopOpensOneIncident is cases (a) and (c) (P1): crashes of container web with exit code 2
// (reason Error, pod status, a cause) open the Error Incident on the second sighting, 20:27:28Z, with
// status.cause; the BackOff, linked by the collector to the Error's fingerprint, counts on it as evidence.
func TestIssue432CrashLoopOpensOneIncident(t *testing.T) {
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

// TestExitCauseDoesNotAbsorbUnhealthy holds that a non-zero exit's Incident never counts an Unhealthy event as
// evidence, also when an observation reaches the controller with the exit's fingerprint as its cause: the
// controller checks the cause's reason itself (causeTakes), as the fold does, and does not rely on the collector
// linking BackOff events only. The Unhealthy event opens its own Incident, as on main; a BackOff still counts.
func TestExitCauseDoesNotAbsorbUnhealthy(t *testing.T) {
	c, clock := newAt(t)
	crash := func(i int, at time.Time) {
		clock.set(at)
		o := symptomOf(1, "Error", terminationID(i), 1, fingerprint.SourcePodStatus, "web", collector.ClassCause, at)
		o.Began = at
		c.consume(t.Context(), o)
	}
	crash(1, t0.Add(20*time.Second))
	crash(2, t0.Add(2*time.Minute+28*time.Second))
	at := t0.Add(3 * time.Minute)
	clock.set(at)
	unhealthy := consequenceOf(2, 1, "web", reasonUnhealthy, 3, 4, at)
	c.mu.Lock()
	seen := c.causeSeen(unhealthy)
	c.mu.Unlock()
	if seen {
		t.Errorf("the controller answers that an Unhealthy event's non-zero exit cause was seen, want not")
	}
	c.consume(t.Context(), unhealthy)
	if n, _ := absorbedOn(c, fp(1)); n != 0 {
		t.Errorf("the exit's Incident holds %d Unhealthy as evidence, want 0", n)
	}
	if got := opened(c); !slices.Equal(got, []string{fp(1), fp(2)}) {
		t.Errorf("open Incidents %v, want the exit's %s and the Unhealthy's own %s", got, fp(1), fp(2))
	}
	c.consume(t.Context(), consequenceOf(3, 1, "web", reasonBackOff, 4, 5, at))
	if n, _ := absorbedOn(c, fp(1)); n != 5 {
		t.Errorf("the exit's Incident holds %d BackOff as evidence, want 5", n)
	}
}
