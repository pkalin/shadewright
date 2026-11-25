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
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// occurrenceID matches the occurrence IDs the Incident schema accepts. An ID that does not match is counted
// but not recorded, so a status write never fails on it.
var occurrenceID = regexp.MustCompile(`^(event|termination|waiting|condition):[A-Za-z0-9-]{1,64}$`)

// noBegin stands for the time an occurrence began when the cluster did not report one. Such an occurrence
// sorts as the oldest, so it is dropped first, and dropping one still marks that something was dropped. The
// mark never answers for an occurrence without a begin time (Controller.handled).
var noBegin = time.Unix(0, 0).UTC()

// counted is what was counted of one occurrence: the highest total and when it began (zero when unknown).
type counted struct {
	total int64
	began time.Time
}

// heldOccurrence is one occurrence a batch holds: what was counted of it, the namespace it came from (the
// observation's Source), whose share of the occurrences held unwritten it is charged to, and, for a
// consequence counted as evidence of the Incident's cause (ADR-0039 point 16), its reason: such an occurrence
// is recorded in absorbedOccurrences, apart from the Incident's own.
type heldOccurrence struct {
	counted
	source   string
	absorbed remediationv1alpha1.AbsorbedReason
}

// tally is what a batch adds to one entry of status.absorbed: how many occurrences, and the newest.
type tally struct {
	count int64
	last  time.Time
}

// batch is what one Incident has gained since its last status write. count, first and last are its own
// observations only; absorbed holds the consequences counted as evidence of its cause, which change neither
// (ADR-0039 point 16). cause says a pod-status observation of class cause was counted (status.cause),
// consequence and nonConsequence track whether own observations counted on this Incident were classified as
// consequences by the collector (status.consequence, ADR-0039 point 12 amended 2026-11-21), and containers
// are the container names its observations named, already checked to be DNS labels.
type batch struct {
	continuedPull  string
	spec           remediationv1alpha1.IncidentSpec
	count          int64
	first, last    time.Time
	occurrences    map[string]heldOccurrence
	absorbed       map[remediationv1alpha1.AbsorbedReason]tally
	cause          bool
	consequence    bool
	nonConsequence bool
	containers     []string
}

// own reports whether b holds anything of the Incident's own: a count, a time seen or an occurrence that is
// not absorbed. A batch that holds only absorbed consequences is not a recurrence of a Resolved Incident.
func (b *batch) own() bool {
	if b.count > 0 || !b.first.IsZero() || !b.last.IsZero() {
		return true
	}
	for _, o := range b.occurrences {
		if o.absorbed == "" {
			return true
		}
	}
	return false
}

// occurrenceOf returns what b holds of one occurrence. b may be nil.
func (b *batch) occurrenceOf(id string) (counted, bool) {
	if b == nil {
		return counted{}, false
	}
	o, ok := b.occurrences[id]
	return o.counted, ok
}

// add folds other into b. Both belong to the same fingerprint. An occurrence b already holds keeps its
// source: the same occurrence always comes from the same namespace, and keeping the first one keeps the
// namespace it was charged to.
func (b *batch) add(other *batch) {
	if other.continuedPull != "" {
		b.continuedPull = other.continuedPull
	}
	b.count += other.count
	b.first = earliest(b.first, other.first)
	b.last = latest(b.last, other.last)
	b.cause = b.cause || other.cause
	b.consequence = b.consequence || other.consequence
	b.nonConsequence = b.nonConsequence || other.nonConsequence
	for _, name := range other.containers {
		if !slices.Contains(b.containers, name) {
			b.containers = append(b.containers, name)
		}
	}
	for reason, t := range other.absorbed {
		if b.absorbed == nil {
			b.absorbed = map[remediationv1alpha1.AbsorbedReason]tally{}
		}
		cur := b.absorbed[reason]
		b.absorbed[reason] = tally{count: cur.count + t.count, last: latest(cur.last, t.last)}
	}
	for id, o := range other.occurrences {
		cur, ok := b.occurrences[id]
		if !ok {
			cur.source, cur.absorbed = o.source, o.absorbed
		}
		cur.counted = higher(cur.counted, o.counted)
		b.occurrences[id] = cur
	}
}

// combine returns one batch holding all of bs, which belong to the same fingerprint, without changing them.
func combine(bs []*batch) *batch {
	out := &batch{spec: bs[0].spec, occurrences: map[string]heldOccurrence{}}
	for _, b := range bs {
		out.add(b)
	}
	return out
}

// higher returns the record with the higher total, keeping a known begin time.
func higher(a, b counted) counted {
	out := a
	if b.total > a.total {
		out.total = b.total
	}
	if out.began.IsZero() {
		out.began = b.began
	}
	return out
}

func earliest(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// applied reports whether status already holds b. A batch always raises the total of each occurrence it
// holds, so a write that succeeded although its answer was lost (a timeout) is recognised and not added
// twice. Each occurrence is checked on its own:
//
//   - recorded with at least b's total (cut to what the schema stores): it proves b was written;
//   - recorded lower, or not recorded although it began after occurrencesDroppedThrough: b was not written;
//   - not recorded and began no later than occurrencesDroppedThrough, without a known begin time, or with an
//     ID the schema refuses: it cannot tell, because merge drops or skips such a record, so it neither
//     proves nor blocks.
//
// b counts as applied when no occurrence says it was not written and at least one proves it was. A batch
// with no occurrence that can prove it is never taken as applied: a possible overcount is preferred to a
// silent loss. An absorbed occurrence is checked against absorbedOccurrences and its drop mark, an own one
// against occurrences.
func applied(status *remediationv1alpha1.IncidentStatus, b *batch) bool {
	recordedOwn := make(map[string]int64, len(status.Occurrences))
	for _, r := range status.Occurrences {
		recordedOwn[r.ID] = r.Total
	}
	recordedAbsorbed := make(map[string]int64, len(status.AbsorbedOccurrences))
	for _, r := range status.AbsorbedOccurrences {
		recordedAbsorbed[r.ID] = r.Total
	}
	proved := false
	for id, o := range b.occurrences {
		if !occurrenceID.MatchString(id) {
			continue
		}
		recorded, droppedThrough := recordedOwn, timeFrom(status.OccurrencesDroppedThrough)
		if o.absorbed != "" {
			recorded, droppedThrough = recordedAbsorbed, timeFrom(status.AbsorbedOccurrencesDroppedThrough)
		}
		if total, ok := recorded[id]; ok {
			if total < min(o.total, math.MaxInt32) {
				return false
			}
			proved = true
			continue
		}
		if !o.began.IsZero() && (droppedThrough.IsZero() || o.began.After(droppedThrough)) {
			return false
		}
	}
	return proved
}

// merge adds b to status at now: the count saturates at MaxIncidentCount, first and last seen widen, and the
// occurrence records take b's totals. The records are then sorted newest first and cut to
// MaxOccurrenceRecords; the IDs of those dropped are returned, and occurrencesDroppedThrough moves to the
// newest begin time among them. b's absorbed consequences go to absorbed and absorbedOccurrences the same
// way, with their own drop mark, and touch none of count, firstSeen, lastSeen or occurrences (ADR-0039 point
// 16). A batch that counted a cause sets status.cause, which is never cleared. Container names are added by
// [addContainers].
func merge(status *remediationv1alpha1.IncidentStatus, b *batch) (dropped []string) {
	if b.continuedPull != "" && status.ContinuedPullFingerprint == "" {
		status.ContinuedPullFingerprint = b.continuedPull
	}
	if status.Phase == "" {
		status.Phase = remediationv1alpha1.IncidentOpen
	}
	if b.nonConsequence || b.cause || status.Cause {
		status.Consequence = false
	} else if b.consequence && status.Count == 0 {
		status.Consequence = true
	}
	status.Count = min(status.Count+b.count, remediationv1alpha1.MaxIncidentCount)
	status.FirstSeen = timeOf(earliest(timeFrom(status.FirstSeen), b.first))
	status.LastSeen = timeOf(latest(timeFrom(status.LastSeen), b.last))
	if b.cause {
		status.Cause = true
	}
	mergeAbsorbed(status, b.absorbed)

	own := map[string]counted{}
	absorbed := map[string]counted{}
	for id, o := range b.occurrences {
		if o.absorbed != "" {
			absorbed[id] = o.counted
		} else {
			own[id] = o.counted
		}
	}
	var gone []string
	status.Occurrences, gone = mergeRecords(status.Occurrences, &status.OccurrencesDroppedThrough, own)
	dropped = append(dropped, gone...)
	if len(absorbed) > 0 {
		status.AbsorbedOccurrences, gone = mergeRecords(status.AbsorbedOccurrences,
			&status.AbsorbedOccurrencesDroppedThrough, absorbed)
		dropped = append(dropped, gone...)
	}
	return dropped
}

// mergeAbsorbed adds a batch's tallies to status.absorbed: each count saturates at MaxIncidentCount and each
// lastSeen only moves forward. Entries stay sorted by reason; a reason past MaxAbsorbedReasons is left out
// (the enum has three values, so it cannot happen while the schema holds).
func mergeAbsorbed(status *remediationv1alpha1.IncidentStatus, add map[remediationv1alpha1.AbsorbedReason]tally) {
	for reason, t := range add {
		i := slices.IndexFunc(status.Absorbed, func(a remediationv1alpha1.AbsorbedTally) bool { return a.Reason == reason })
		if i < 0 {
			if len(status.Absorbed) >= remediationv1alpha1.MaxAbsorbedReasons {
				continue
			}
			status.Absorbed = append(status.Absorbed, remediationv1alpha1.AbsorbedTally{Reason: reason})
			i = len(status.Absorbed) - 1
		}
		a := &status.Absorbed[i]
		a.Count = min(a.Count+t.count, remediationv1alpha1.MaxIncidentCount)
		if last := timeOf(latest(a.LastSeen.Time, t.last)); last != nil {
			a.LastSeen = *last
		}
	}
	slices.SortFunc(status.Absorbed, func(x, y remediationv1alpha1.AbsorbedTally) int {
		return strings.Compare(string(x.Reason), string(y.Reason))
	})
}

// mergeRecords adds the occurrences of add to the records stored, sorts them newest first and cuts them to
// MaxOccurrenceRecords. It returns the records kept and the IDs of those dropped, and moves the drop mark
// through to the newest begin time among them. An ID the schema refuses is not recorded.
func mergeRecords(stored []remediationv1alpha1.OccurrenceRecord, through **metav1.Time,
	add map[string]counted) ([]remediationv1alpha1.OccurrenceRecord, []string) {
	records := make(map[string]counted, len(stored)+len(add))
	for _, r := range stored {
		records[r.ID] = counted{total: r.Total, began: timeFrom(r.Began)}
	}
	for id, o := range add {
		if occurrenceID.MatchString(id) {
			records[id] = higher(records[id], counted{total: min(o.total, math.MaxInt32), began: o.began})
		}
	}
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(x, y string) int {
		if c := beganOrNone(records[y].began).Compare(beganOrNone(records[x].began)); c != 0 {
			return c
		}
		return strings.Compare(x, y)
	})
	keep := ids[:min(len(ids), remediationv1alpha1.MaxOccurrenceRecords)]
	dropped := ids[len(keep):]
	out := make([]remediationv1alpha1.OccurrenceRecord, 0, len(keep))
	for _, id := range keep {
		r := records[id]
		out = append(out, remediationv1alpha1.OccurrenceRecord{
			ID: id, Total: max(r.total, 0), Began: timeOf(r.began),
		})
	}
	if len(dropped) > 0 {
		*through = timeOf(latest(timeFrom(*through), beganOrNone(records[dropped[0]].began)))
	}
	return out, dropped
}

// addContainers adds the container names of b to status.containers, keeping those it holds, up to
// MaxIncidentContainers. It returns the names left out at the cap. The names were checked to be DNS labels
// when the observation was consumed ([containerName]).
func addContainers(status *remediationv1alpha1.IncidentStatus, b *batch) (left []string) {
	for _, name := range b.containers {
		if slices.Contains(status.Containers, remediationv1alpha1.Name(name)) {
			continue
		}
		if len(status.Containers) >= remediationv1alpha1.MaxIncidentContainers {
			left = append(left, name)
			continue
		}
		status.Containers = append(status.Containers, remediationv1alpha1.Name(name))
	}
	return left
}

// beganOrNone returns t, or noBegin when t is zero.
func beganOrNone(t time.Time) time.Time {
	if t.IsZero() {
		return noBegin
	}
	return t
}

// timeOf returns t cut to whole seconds, as the API stores it, or nil when t is zero.
func timeOf(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	return &metav1.Time{Time: t.Truncate(time.Second)}
}

// timeFrom returns the time t holds, or zero.
func timeFrom(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}
