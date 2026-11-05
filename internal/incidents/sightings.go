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

// When a symptom opens an Incident (design 3.2, #267). A symptom that has no Incident yet opens it at once
// when its observation proves on its own that it recurs ([recurs]) or is immediate
// (collector.Observation.Immediate). Any other is held as a first sighting. It opens its Incident when it
// is seen again at least RecurAfter and at most RecurWithin after the first sighting, with every sighting
// held counted. A sighting before RecurAfter is held and keeps waiting; one after RecurWithin starts over as
// a new first sighting. So a Warning seen once while the cluster starts (a dependency not ready yet) opens
// nothing, and a symptom that persists opens its Incident within minutes.
const (
	// RecurAfter is the least time between the first sighting of a symptom and the sighting that opens its
	// Incident, and the least time between the start of an occurrence and its latest repeat for the
	// observation to open its Incident at once.
	RecurAfter = 2 * time.Minute
	// RecurWithin is the most time between the first sighting of a symptom and the sighting that opens its
	// Incident. A first sighting older than this is forgotten.
	RecurWithin = 15 * time.Minute
	// DefaultMaxFirstSightings bounds the fingerprints held as first sightings. When it is full, the oldest
	// first sighting is forgotten to make room; that symptom then starts over, which delays its Incident
	// but never counts it twice. It is the collector's owner cache size (collector.DefaultMaxOwners), well
	// within the event series the collector remembers (collector.DefaultMaxEvents).
	DefaultMaxFirstSightings = collector.DefaultMaxOwners
	// maxHeldPerFingerprint bounds the occurrences held per first sighting. A further sighting of an
	// occurrence already held is merged into it; one of a new occurrence before RecurAfter is not held, so
	// it is not counted when the Incident opens (an undercount), unless the collector reports it again. The
	// later observations share the first one's symptom (up to 1 KiB), so one first sighting holds about
	// 3 KiB at most and all of them together about 12 MiB, within the analyzer's 128 Mi.
	maxHeldPerFingerprint = 4
)

// recurs reports whether an observation proves on its own that its symptom recurs: the occurrence's latest
// repeat came at least RecurAfter after it began. Such a symptom has been seen again already, whenever its
// observation arrives: an events.k8s.io series refreshed about every 30 minutes, or a replay after a restart.
// A total of 2 or more alone is not enough: a recorder that repeats an event within seconds (a burst, or a
// controller's poll interval) makes a series too, and a burst within seconds is not a recurrence.
func recurs(o collector.Observation) bool {
	return !o.Began.IsZero() && o.ObservedAt.Sub(o.Began) >= RecurAfter
}

// sighting is a fingerprint without an Incident, seen at first and not yet seen again in the window that
// would open its Incident. held has one observation per occurrence.
type sighting struct {
	fingerprint string
	first       time.Time
	held        []collector.Observation
}

// heldTotal is the highest total held for an occurrence, and the fingerprint whose first sighting holds it.
type heldTotal struct {
	fingerprint string
	total       int64
}

// sightings holds first sightings in memory only, bounded and ordered by first sighting, oldest at the back.
// A restart forgets them, which is safe: a real symptom recurs. It is not safe for concurrent use; the
// Controller holds mu around every call.
type sightings struct {
	max   int
	order *list.List // of *sighting, newest first sighting at the front
	items map[string]*list.Element
	// totals holds, per occurrence held, its highest total, so that Handled answers for it.
	totals map[string]heldTotal
}

func newSightings(capacity int) *sightings {
	return &sightings{max: max(capacity, 1), order: list.New(), items: map[string]*list.Element{},
		totals: map[string]heldTotal{}}
}

// admit decides what to count of an observation of a fingerprint without an Incident, seen at now: nothing
// while it waits to recur, or every observation held for it and o when it opens its Incident. o.Count is
// what is new of it, net of what is held.
func (s *sightings) admit(o collector.Observation, now time.Time) []collector.Observation {
	defer s.report()
	e, ok := s.items[o.Fingerprint]
	if ok {
		h := e.Value.(*sighting)
		switch since := now.Sub(h.first); {
		case since > RecurWithin:
			s.remove(e)
		case since < RecurAfter:
			s.hold(h, o)
			return nil
		default:
			s.remove(e)
			return append(h.held, o)
		}
	}
	h := &sighting{fingerprint: o.Fingerprint, first: now}
	s.hold(h, o)
	s.items[o.Fingerprint] = s.order.PushFront(h)
	for s.order.Len() > s.max {
		s.remove(s.order.Back())
	}
	return nil
}

// hold adds o to h: merged into the observation of its occurrence when h holds one (the highest total, the
// counts added, the earliest start and the latest time), else as a new one while there is room.
func (s *sightings) hold(h *sighting, o collector.Observation) {
	for i := range h.held {
		if p := &h.held[i]; p.Occurrence == o.Occurrence {
			p.Total = max(p.Total, o.Total)
			p.Count = min(p.Count+o.Count, collector.MaxCount)
			p.Began, p.ObservedAt = earliest(p.Began, o.Began), latest(p.ObservedAt, o.ObservedAt)
			s.note(h.fingerprint, p.Occurrence, p.Total)
			return
		}
	}
	if len(h.held) >= maxHeldPerFingerprint {
		return
	}
	if len(h.held) > 0 {
		// The same fingerprint hashes the same normalized symptom: keep one copy of it.
		o.Symptom = h.held[0].Symptom
	}
	h.held = append(h.held, o)
	s.note(h.fingerprint, o.Occurrence, o.Total)
}

// note records total as held for occurrence by fingerprint, keeping the highest.
func (s *sightings) note(fingerprint, occurrence string, total int64) {
	if cur, ok := s.totals[occurrence]; ok && cur.total > total {
		total = cur.total
	}
	s.totals[occurrence] = heldTotal{fingerprint: fingerprint, total: total}
}

// total returns the highest total held for an occurrence, and whether one is held.
func (s *sightings) total(occurrence string) (int64, bool) {
	t, ok := s.totals[occurrence]
	return t.total, ok
}

// take forgets the first sighting of fingerprint and returns the observations held for it, if any.
func (s *sightings) take(fingerprint string) []collector.Observation {
	e, ok := s.items[fingerprint]
	if !ok {
		return nil
	}
	defer s.report()
	s.remove(e)
	return e.Value.(*sighting).held
}

// expire forgets the first sightings older than RecurWithin at now.
func (s *sightings) expire(now time.Time) {
	defer s.report()
	for e := s.order.Back(); e != nil && now.Sub(e.Value.(*sighting).first) > RecurWithin; e = s.order.Back() {
		s.remove(e)
	}
}

func (s *sightings) remove(e *list.Element) {
	h := e.Value.(*sighting)
	s.order.Remove(e)
	delete(s.items, h.fingerprint)
	for _, o := range h.held {
		if t, ok := s.totals[o.Occurrence]; ok && t.fingerprint == h.fingerprint {
			delete(s.totals, o.Occurrence)
		}
	}
}

// len returns the number of fingerprints held.
func (s *sightings) len() int {
	return s.order.Len()
}

// report sets the gauge of first sightings held.
func (s *sightings) report() {
	firstSightingsHeld.Set(float64(s.order.Len()))
}
