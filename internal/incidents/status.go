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

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// occurrenceID matches the occurrence IDs the Incident schema accepts. An ID that does not match is counted
// but not recorded, so a status write never fails on it.
var occurrenceID = regexp.MustCompile(`^(event|termination):[A-Za-z0-9-]{1,64}$`)

// noBegin stands for the time an occurrence began when the cluster did not report one. Such an occurrence
// sorts as the oldest, so it is dropped first, and dropping one still marks that something was dropped. The
// mark never answers for an occurrence without a begin time (Controller.handled).
var noBegin = time.Unix(0, 0).UTC()

// counted is what was counted of one occurrence: the highest total and when it began (zero when unknown).
type counted struct {
	total int64
	began time.Time
}

// batch is what one Incident has gained since its last status write.
type batch struct {
	spec        remediationv1alpha1.IncidentSpec
	count       int64
	first, last time.Time
	occurrences map[string]counted
}

// occurrenceOf returns what b holds of one occurrence. b may be nil.
func (b *batch) occurrenceOf(id string) (counted, bool) {
	if b == nil {
		return counted{}, false
	}
	o, ok := b.occurrences[id]
	return o, ok
}

// add folds other into b. Both belong to the same fingerprint.
func (b *batch) add(other *batch) {
	b.count += other.count
	b.first = earliest(b.first, other.first)
	b.last = latest(b.last, other.last)
	for id, o := range other.occurrences {
		b.occurrences[id] = higher(b.occurrences[id], o)
	}
}

// combine returns one batch holding all of bs, which belong to the same fingerprint, without changing them.
func combine(bs []*batch) *batch {
	out := &batch{spec: bs[0].spec, occurrences: map[string]counted{}}
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
// silent loss.
func applied(status *remediationv1alpha1.IncidentStatus, b *batch) bool {
	recorded := make(map[string]int64, len(status.Occurrences))
	for _, r := range status.Occurrences {
		recorded[r.ID] = r.Total
	}
	droppedThrough := timeFrom(status.OccurrencesDroppedThrough)
	proved := false
	for id, o := range b.occurrences {
		if !occurrenceID.MatchString(id) {
			continue
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
// newest begin time among them.
func merge(status *remediationv1alpha1.IncidentStatus, b *batch) (dropped []string) {
	if status.Phase == "" {
		status.Phase = remediationv1alpha1.IncidentOpen
	}
	status.Count = min(status.Count+b.count, remediationv1alpha1.MaxIncidentCount)
	status.FirstSeen = timeOf(earliest(timeFrom(status.FirstSeen), b.first))
	status.LastSeen = timeOf(latest(timeFrom(status.LastSeen), b.last))

	records := make(map[string]counted, len(status.Occurrences)+len(b.occurrences))
	for _, r := range status.Occurrences {
		records[r.ID] = counted{total: r.Total, began: timeFrom(r.Began)}
	}
	for id, o := range b.occurrences {
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
	dropped = ids[len(keep):]
	status.Occurrences = make([]remediationv1alpha1.OccurrenceRecord, 0, len(keep))
	for _, id := range keep {
		r := records[id]
		status.Occurrences = append(status.Occurrences, remediationv1alpha1.OccurrenceRecord{
			ID: id, Total: max(r.total, 0), Began: timeOf(r.began),
		})
	}
	if len(dropped) > 0 {
		through := beganOrNone(records[dropped[0]].began)
		status.OccurrencesDroppedThrough = timeOf(latest(timeFrom(status.OccurrencesDroppedThrough), through))
	}
	return dropped
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
