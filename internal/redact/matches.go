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

package redact

import (
	"github.com/pkalin/shadewright/internal/redact/meter"
)

// The matches of the rules on one text, held as they are found (#1087). Every caller merges them (mergeSpans)
// before it uses them, so they are merged as they come: a text dense in secrets (a match every few bytes) held a
// span of 48 bytes for every match of every rule, and twice that while the list grew, until the end of the
// reading. A match is held in sixteen bytes (its kind is its rule's), and the list is merged each time the
// matches added since the last merge are as many as those it holds merged, so that it holds about two matches
// for each merged range. Merged at once or as they come, the ranges and their names are the same: a merged range
// keeps the end of the match that names it (span.named), by which a later merge orders it on a tie of start and
// rule, as it would order that match.

// mergeEvery is how many matches a matchList holds before it merges them the first time.
const mergeEvery = 1 << 16

// ruleMatch is a match of rule rule of a Redactor, from start to end; named as in span. Its kind is its rule's, or,
// for a span of the reading of the release before (whose rule is after the redactor's own), kinds[kind] of its
// list.
type ruleMatch struct {
	start, end, named int32
	rule, kind        int16
}

// nameEnd is span.nameEnd of m.
func (m ruleMatch) nameEnd() int32 {
	if m.named > 0 {
		return m.named
	}
	return m.end
}

// matchList holds the matches of r's rules on one text, merged as they come.
type matchList struct {
	r      *Redactor
	m      []ruleMatch
	merged int      // the matches at the start of m that are merged
	kinds  []string // the kinds of the spans of the reading of the release before, by index
}

// newMatchList returns an empty list for r's rules.
func (r *Redactor) newMatchList() *matchList { return &matchList{r: r} }

// add adds the match [start, end) of rule rule. The matches it holds are merged when those added since the last
// merge are at least mergeEvery and as many as the merged ones, so the merges sort each match a few times on
// average and the work stays n log n in the matches.
func (l *matchList) add(wm *meter.Meter, start, end, rule int) {
	l.push(wm, ruleMatch{start: int32(start), end: int32(end), rule: int16(rule), kind: -1}) // #nosec G115 -- an entry is at most MaxEntry bytes, an answer six times that; a redactor has far fewer than 32767 rules
}

// addKind adds the span [start, end) of the kind given that the reading of the release before found, with rule
// rule (after the redactor's own, releasedReading.rule).
func (l *matchList) addKind(wm *meter.Meter, start, end, rule int, kind string) {
	l.push(wm, ruleMatch{start: int32(start), end: int32(end), rule: int16(rule), kind: kindIndex(&l.kinds, kind)}) // #nosec G115 -- as in add
}

// push adds m and merges what l holds when it is due.
func (l *matchList) push(wm *meter.Meter, m ruleMatch) {
	l.m = append(l.m, m)
	if n := len(l.m) - l.merged; n >= mergeEvery && n >= l.merged {
		l.merge(wm)
	}
}

// merge merges the matches l holds (as mergeSpans merges spans).
func (l *matchList) merge(wm *meter.Meter) {
	l.m = mergeMatches(wm, l.m)
	l.merged = len(l.m)
}

// mergeMatches sorts ms and merges the overlapping ones in place, as mergeSpans merges spans.
func mergeMatches(wm *meter.Meter, ms []ruleMatch) []ruleMatch {
	if len(ms) == 0 {
		return ms
	}
	sortFunc(wm, ms, func(a, b ruleMatch) int {
		if a.start != b.start {
			return int(a.start - b.start)
		}
		if a.rule != b.rule {
			return int(a.rule) - int(b.rule)
		}
		return int(b.nameEnd() - a.nameEnd())
	})
	merged := ms[:0]
	for i := 0; i < len(ms); {
		tick(wm, 1)
		cur := ms[i]
		cur.named = cur.nameEnd()
		for i++; i < len(ms) && ms[i].start < cur.end; i++ {
			tick(wm, 1)
			cur.end = max(cur.end, ms[i].end)
		}
		merged = append(merged, cur)
	}
	return merged
}

// kindIndex returns the index of kind in kinds, added at the end when it is not there: a list holds the few kinds
// of the rules.
func kindIndex(kinds *[]string, kind string) int16 {
	for i, x := range *kinds {
		if x == kind {
			return int16(i) // #nosec G115 -- a few kinds
		}
	}
	*kinds = append(*kinds, kind)
	return int16(len(*kinds) - 1) // #nosec G115 -- as above
}

// spans returns the matches merged, as spans with the kind of their rule, sorted and disjoint, in a slice of their
// number; l holds nothing after it.
func (l *matchList) spans(wm *meter.Meter) []span {
	if l.merged < len(l.m) {
		l.merge(wm)
	}
	if len(l.m) == 0 {
		return nil
	}
	out := make([]span, len(l.m))
	for i, m := range l.m {
		tick(wm, 1)
		kind := ""
		if m.kind >= 0 {
			kind = l.kinds[m.kind]
		} else {
			kind = l.r.rules[m.rule].kind
		}
		out[i] = span{start: int(m.start), end: int(m.end), kind: kind, rule: int(m.rule), named: m.named}
	}
	l.m, l.merged = nil, 0
	return out
}
