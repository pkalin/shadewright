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

package eval

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// The oom grouping case with the consequences reaching the Incident controller before their cause (#820). The
// collector's pod and event reconcilers deliver concurrently, so in a cluster a kubelet event can be consumed
// a moment before the pod status that shows its cause: the consequence, seen once, is held as a first
// sighting, and is counted on the cause's Incident only when the cause is consumed. A relay between the
// collector and the Incident controller makes that order certain instead of leaving it to the scheduler.

// consequenceFirst is the relay of the cases. It holds back what the collector reports until it has a cause
// observation and one consequence linked to a cause for each reason in want; it then delivers each
// consequence and waits until the Incident controller holds it (Handled answers for its whole total), and
// only then delivers the causes. Every later consequence observation is dropped, so each is seen once and no
// repeat absorbs it on the cause's Incident; so is one with no cause fingerprint, which the collector holds
// itself (ADR-0039 point 19). Other observations pass at once.
type consequenceFirst struct {
	want []string
	// unlinkedFirst delivers, before each consequence, a copy of it with no cause fingerprint and one less in
	// its total: the collector's reading before it linked the event to the pod's cause. The consequence then
	// arrives as a repeat of a held first sighting that names its cause (the merge path of #820).
	unlinkedFirst bool

	mu sync.Mutex
	// delivered are the consequence observations delivered, by reason, the linked one last.
	delivered map[string][]collector.Observation
	err       error
}

// isCauseObservation tells the cause observations the Incident controller acts on: a pod-status reading of
// class cause.
func isCauseObservation(o collector.Observation) bool {
	return o.Class == collector.ClassCause && o.Origin == fingerprint.SourcePodStatus
}

func (r *consequenceFirst) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = err
	}
}

// check fails t with the relay's error, if any.
func (r *consequenceFirst) check(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		t.Fatalf("the relay: %v", r.err)
	}
}

func (r *consequenceFirst) run(ctx context.Context, inc *incidents.Controller, in <-chan collector.Observation,
	out chan<- collector.Observation) {
	send := func(o collector.Observation) bool {
		select {
		case out <- o:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var causes []collector.Observation
	consequences := map[string]collector.Observation{}
	released := false
	for {
		var o collector.Observation
		select {
		case o = <-in:
		case <-ctx.Done():
			return
		}
		switch {
		case released && o.Class == collector.ClassConsequence:
			continue
		case released:
		case isCauseObservation(o):
			causes = append(causes, o)
		case o.Class == collector.ClassConsequence && o.CauseFingerprint != "":
			consequences[o.Reason] = o
		}
		if released || (!isCauseObservation(o) && o.Class != collector.ClassConsequence) {
			if !send(o) {
				return
			}
			continue
		}
		if len(causes) == 0 || len(consequences) < len(r.want) {
			continue
		}
		for _, reason := range r.want {
			c, ok := consequences[reason]
			if !ok {
				break
			}
			if !r.deliver(ctx, inc, c, send) {
				return
			}
		}
		if len(r.delivered) < len(r.want) {
			continue
		}
		for _, c := range causes {
			if !send(c) {
				return
			}
		}
		released = true
	}
}

// deliver sends consequence c (after its unlinked copy, when the case asks for one) and waits until the
// Incident controller holds each. It reports false when the relay must stop.
func (r *consequenceFirst) deliver(ctx context.Context, inc *incidents.Controller, c collector.Observation,
	send func(collector.Observation) bool) bool {
	r.mu.Lock()
	_, done := r.delivered[c.Reason]
	r.mu.Unlock()
	if done {
		return true
	}
	steps := []collector.Observation{c}
	if r.unlinkedFirst {
		if c.Total < 2 || c.Count < 2 || c.MayRepeat {
			r.fail(fmt.Errorf("consequence %s has total %d, count %d and may repeat %t: no smaller first reading",
				c.Reason, c.Total, c.Count, c.MayRepeat))
			return false
		}
		first := c
		first.CauseFingerprint, first.Total, first.Count = "", c.Total-1, c.Count-1
		steps = []collector.Observation{first, c}
	}
	for _, o := range steps {
		if !send(o) || !r.held(ctx, inc, o) {
			return false
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.delivered == nil {
		r.delivered = map[string][]collector.Observation{}
	}
	r.delivered[c.Reason] = steps
	return true
}

// held waits until the Incident controller answers for the whole total of o's occurrence: it has consumed o.
func (r *consequenceFirst) held(ctx context.Context, inc *incidents.Controller, o collector.Observation) bool {
	ctx, cancel := context.WithTimeout(ctx, groupingWait)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		total, found, err := inc.Handled(ctx, o)
		if err == nil && found && total >= o.Total {
			return true
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			//nolint:errorlint // err may be nil here, which %w prints as %!w(<nil>)
			r.fail(fmt.Errorf("the Incident controller did not consume consequence %s (total %d) in %s: %v",
				o.Reason, o.Total, groupingWait, err))
			return false
		}
	}
}

// linked returns the delivered consequence of reason that names its cause, and whether it was delivered.
func (r *consequenceFirst) linked(reason string) (collector.Observation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	steps := r.delivered[reason]
	if len(steps) == 0 {
		return collector.Observation{}, false
	}
	return steps[len(steps)-1], true
}

// runConsequenceFirst runs the oom fault with relay r and checks the outcome: one Incident, the OOMKilled
// cause's, with each consequence counted in its absorbed tally as many times as its event's series reports
// (its occurrence's total), analyzed once with one request, and no Incident of a consequence.
func runConsequenceFirst(t *testing.T, r *consequenceFirst) {
	s, cluster, events, shift := oomFixture(t)
	g := startGrouping(t, s, nil, r.run)
	// The cluster and the events at once: the relay decides the order the Incident controller sees.
	g.create(t, append(slices.Clone(cluster), events...), shift)

	g.waitFor(t, "the cause analyzed, with both consequences counted on it", func(c counts) bool {
		r.check(t)
		for _, inc := range c.incidents {
			abs := absorbedOf(inc)
			if inc.Status.Cause && abs[remediationv1alpha1.AbsorbedBackOff] > 0 &&
				abs[remediationv1alpha1.AbsorbedUnhealthy] > 0 && c.analyses > 0 {
				return true
			}
		}
		return false
	})
	time.Sleep(groupingSettle)
	r.check(t)
	c := g.counts(t, oomSubject)
	if len(c.incidents) != 1 {
		t.Fatalf("the oom fault opened %d Incidents for %s, want 1 (none for a consequence):\n%s",
			len(c.incidents), oomSubject, c)
	}
	cause := checkOneAnalysis(t, c)
	if len(c.requests) != 1 {
		t.Fatalf("the oom fault gave %d requests, want the one of the recorded reply:\n%s", len(c.requests), c)
	}
	// The series count of each kubelet event in grouping.yaml, the total of its occurrence.
	series := map[string]int64{}
	for _, e := range events {
		reason, _, _ := unstructured.NestedString(e.Object, "reason")
		// The decoder reads a YAML number as a JSON one.
		switch n, _, _ := unstructured.NestedFieldNoCopy(e.Object, "series", "count"); v := n.(type) {
		case int64:
			series[reason] = v
		case float64:
			series[reason] = int64(v)
		}
	}
	abs := absorbedOf(cause)
	for _, reason := range r.want {
		o, ok := r.linked(reason)
		if !ok {
			t.Fatalf("the relay delivered no %s consequence", reason)
		}
		if o.CauseFingerprint != cause.Spec.Fingerprint {
			t.Fatalf("the %s consequence names cause %s, not the Incident's %s", reason, o.CauseFingerprint,
				cause.Spec.Fingerprint)
		}
		if o.Total != series[reason] {
			t.Fatalf("the %s consequence has total %d, want the event's series count %d", reason, o.Total,
				series[reason])
		}
		if got := abs[remediationv1alpha1.AbsorbedReason(reason)]; got != o.Total {
			t.Fatalf("the cause counts %d %s in its absorbed tally, want %d (the event's series):\n%s",
				got, reason, o.Total, c)
		}
	}
	if len(cause.Status.Absorbed) != len(r.want) {
		t.Fatalf("the cause's absorbed tally has %d reasons, want %d:\n%s", len(cause.Status.Absorbed), len(r.want), c)
	}
}

// oomConsequences are the reasons of the kubelet events in the oom scenario's grouping.yaml.
var oomConsequences = []string{
	string(remediationv1alpha1.AbsorbedBackOff), string(remediationv1alpha1.AbsorbedUnhealthy),
}

// TestGroupingOOMConsequenceFirst is the oom case with the kubelet's BackOff and readiness probe events, each
// seen once, consumed before the OOM kill that causes them (#820). Each is held as a first sighting, and the
// cause's observation counts what is held for its consequences on its Incident when it opens it: one
// Incident, both consequences in its absorbed tally, none of their own.
func TestGroupingOOMConsequenceFirst(t *testing.T) {
	runConsequenceFirst(t, &consequenceFirst{want: oomConsequences})
}

// TestGroupingOOMConsequenceRepeatNamesCause is the oom case with each consequence first read without its
// cause, then read again, before the cause is consumed, as a repeat of its occurrence that names the cause
// (#820). The held first sighting takes the cause from the repeat, so the cause's observation finds it and
// counts it on its Incident.
func TestGroupingOOMConsequenceRepeatNamesCause(t *testing.T) {
	runConsequenceFirst(t, &consequenceFirst{want: oomConsequences, unlinkedFirst: true})
}
