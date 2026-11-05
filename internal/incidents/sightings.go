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
	"container/list"
	"time"

	"github.com/pkalin/shadewright/internal/analyzer/collector"
)

// When a symptom opens an Incident (design 3.2, #267). A symptom that has no Incident yet and is not
// immediate (collector.Observation.Immediate) is held as a first sighting. It opens its Incident when it is
// seen again at least RecurAfter and at most RecurWithin after the first sighting, with every sighting held
// counted. A sighting before RecurAfter is held and keeps waiting; one after RecurWithin starts over as a
// new first sighting. So a Warning seen once while the cluster starts (a dependency not ready yet) opens
// nothing, and a symptom that persists opens its Incident within minutes.
const (
	// RecurAfter is the least time between the first sighting of a symptom and the sighting that opens its
	// Incident.
	RecurAfter = 2 * time.Minute
	// RecurWithin is the most time between the first sighting of a symptom and the sighting that opens its
	// Incident. A first sighting older than this is forgotten.
	RecurWithin = 15 * time.Minute
	// DefaultMaxFirstSightings bounds the fingerprints held as first sightings. When it is full, the oldest
	// first sighting is forgotten to make room; that symptom then starts over, which delays its Incident
	// but never counts it twice. It is the collector's owner cache size (collector.DefaultMaxOwners), well
	// within the event series the collector remembers (collector.DefaultMaxEvents).
	DefaultMaxFirstSightings = collector.DefaultMaxOwners
	// maxHeldPerFingerprint bounds the observations held per first sighting. Further sightings before
	// RecurAfter are not held, so their occurrences are not counted when the Incident opens (an
	// undercount), unless the collector reports them again. The later observations share the first one's
	// symptom (up to 1 KiB), so one first sighting holds about 3 KiB at most and all of them together about
	// 12 MiB, within the analyzer's 128 Mi.
	maxHeldPerFingerprint = 4
)

// sighting is a fingerprint without an Incident, seen at first and not yet seen again in the window that
// would open its Incident.
type sighting struct {
	fingerprint string
	first       time.Time
	held        []collector.Observation
}

// sightings holds first sightings in memory only, bounded and ordered by first sighting, oldest at the back.
// A restart forgets them, which is safe: a real symptom recurs. It is not safe for concurrent use; the
// Controller holds mu around every call.
type sightings struct {
	max   int
	order *list.List // of *sighting, newest first sighting at the front
	items map[string]*list.Element
}

func newSightings(capacity int) *sightings {
	return &sightings{max: max(capacity, 1), order: list.New(), items: map[string]*list.Element{}}
}

// admit decides what to count of an observation of a fingerprint without an Incident, seen at now: nothing
// while it waits to recur, or every observation held for it and o when it opens its Incident.
func (s *sightings) admit(o collector.Observation, now time.Time) []collector.Observation {
	defer s.report()
	e, ok := s.items[o.Fingerprint]
	if ok {
		h := e.Value.(*sighting)
		switch since := now.Sub(h.first); {
		case since > RecurWithin:
			s.remove(e)
		case since < RecurAfter:
			if len(h.held) < maxHeldPerFingerprint {
				// The same fingerprint hashes the same normalized symptom: keep one copy of it.
				o.Symptom = h.held[0].Symptom
				h.held = append(h.held, o)
			}
			return nil
		default:
			s.remove(e)
			return append(h.held, o)
		}
	}
	s.items[o.Fingerprint] = s.order.PushFront(&sighting{fingerprint: o.Fingerprint, first: now,
		held: []collector.Observation{o}})
	for s.order.Len() > s.max {
		s.remove(s.order.Back())
	}
	return nil
}

// expire forgets the first sightings older than RecurWithin at now.
func (s *sightings) expire(now time.Time) {
	defer s.report()
	for e := s.order.Back(); e != nil && now.Sub(e.Value.(*sighting).first) > RecurWithin; e = s.order.Back() {
		s.remove(e)
	}
}

func (s *sightings) remove(e *list.Element) {
	s.order.Remove(e)
	delete(s.items, e.Value.(*sighting).fingerprint)
}

// len returns the number of fingerprints held.
func (s *sightings) len() int {
	return s.order.Len()
}

// report sets the gauge of first sightings held.
func (s *sightings) report() {
	firstSightingsHeld.Set(float64(s.order.Len()))
}
