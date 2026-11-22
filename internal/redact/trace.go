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
	"strings"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// trace is a text as the release before held it at one of its passes, traced back to the text decided on: byte
// i stands for the bytes [lo(i), hi(i)) of that text. A marker stands for all the bytes it replaced. lo and hi
// never decrease along the text.
//
// What each byte stands for is kept as segments of equal units, not as two offsets a byte: a text copied as it is
// is one segment, and so is a run of characters JSON writes as escapes of one length, each standing for one
// character of the same length. A segment takes sixteen bytes, where two offsets a byte held eight times the
// text, and more than forty-eight times the value for a value of six-byte escapes (#873, decision of 2026-11-21 on
// the heap). What each byte stands for is the same.
type trace struct {
	text string
	// segs map the text that off starts in: a trace cut from the end of another (from) keeps its segments.
	segs []traceSeg
	off  int
	// cur is the segment the last lookup found, so that lookups along the text take constant time.
	cur *int
}

// traceSeg is a run of units of w bytes, from byte at on: byte at+k stands for d bytes from lo + (k/w)*d. Its end
// is the next segment's at, or the end of the text. A segment holds whole units.
type traceSeg struct {
	at, lo, w, d int32
}

// traceOf returns s, which starts at byte at of the text decided on, as a trace of itself.
func traceOf(wm *meter.Meter, s string, at int) trace {
	tick(wm, len(s)) // as many steps as two offsets for each byte took
	t := trace{text: s, cur: new(int)}
	if s != "" {
		t.segs = []traceSeg{{at: 0, lo: int32(at), w: 1, d: 1}} // #nosec G115 -- an entry is at most MaxEntry bytes
	}
	return t
}

// from returns the trace of t's text from byte i on.
func (t trace) from(i int) trace {
	return trace{text: t.text[i:], segs: t.segs, off: t.off + i, cur: new(int)}
}

// end returns where segment k of t ends.
func (t trace) end(k int) int {
	if k+1 < len(t.segs) {
		return int(t.segs[k+1].at)
	}
	return t.off + len(t.text)
}

// index returns the segment that holds byte j of the text t's segments map: the one found last or the next,
// otherwise by a binary search.
func (t trace) index(j int) int {
	if k := *t.cur; k < len(t.segs) && int(t.segs[k].at) <= j {
		for ; k < len(t.segs) && int(t.segs[k].at) <= j && k < *t.cur+4; k++ {
			if j < t.end(k) {
				*t.cur = k
				return k
			}
		}
	}
	lo, hi := 0, len(t.segs) // the last segment at or before j is in [lo, hi)
	for hi-lo > 1 {
		if m := (lo + hi) / 2; int(t.segs[m].at) <= j {
			lo = m
		} else {
			hi = m
		}
	}
	*t.cur = lo
	return lo
}

// lo is where the bytes byte i of t stands for start.
func (t trace) lo(i int) int32 {
	j := t.off + i
	s := t.segs[t.index(j)]
	return s.lo + (int32(j)-s.at)/s.w*s.d // #nosec G115 -- as in traceOf
}

// hi is where the bytes byte i of t stands for end.
func (t trace) hi(i int) int32 { return t.lo(i) + t.segs[*t.cur].d }

// traceBuilder writes a trace.
type traceBuilder struct {
	b    strings.Builder
	segs []traceSeg
}

// push adds a segment of units of w bytes, each standing for d bytes, from byte at of the text written and lo of
// the text decided on: to the last segment when it goes on with it.
func (tb *traceBuilder) push(at int, lo, w, d int32) {
	a := int32(at) // #nosec G115 -- as in traceOf
	if n := len(tb.segs); n > 0 {
		if l := tb.segs[n-1]; l.w == w && l.d == d && (a-l.at)%w == 0 && l.lo+(a-l.at)/w*d == lo {
			return
		}
	}
	tb.segs = append(tb.segs, traceSeg{at: a, lo: lo, w: w, d: d})
}

// add writes s, every byte of which stands for [lo, hi).
func (tb *traceBuilder) add(wm *meter.Meter, s string, lo, hi int32) {
	tick(wm, len(s))
	if s == "" {
		return
	}
	at := tb.b.Len()
	tb.b.WriteString(s)
	tb.push(at, lo, int32(len(s)), hi-lo) // #nosec G115 -- as in traceOf
}

// copy writes the bytes [from, to) of t with what they stand for.
func (tb *traceBuilder) copy(wm *meter.Meter, t trace, from, to int) {
	tick(wm, to-from)
	if to <= from {
		return
	}
	dst := tb.b.Len()
	tb.b.WriteString(t.text[from:to])
	a, e := t.off+from, t.off+to
	for k := t.index(a); a < e; {
		s := t.segs[k]
		end := t.end(k)
		stop := min(end, e)
		w := int(s.w)
		u, r := (a-int(s.at))/w, (a-int(s.at))%w
		lo := s.lo + int32(u)*s.d // #nosec G115 -- as in traceOf
		var m int
		switch {
		case r != 0: // the end of a unit cut at its start: its bytes all stand for the unit's bytes
			m = min(w-r, stop-a)
			tb.push(dst, lo, int32(m), s.d) // #nosec G115 -- as in traceOf
		case stop-a >= w:
			m = (stop - a) / w * w
			tb.push(dst, lo, s.w, s.d)
		default: // the start of a unit cut at its end
			m = stop - a
			tb.push(dst, lo, int32(m), s.d) // #nosec G115 -- as in traceOf
		}
		a, dst = a+m, dst+m
		if a == end {
			k++
		}
	}
}

func (tb *traceBuilder) trace(_ *meter.Meter) trace {
	return trace{text: tb.b.String(), segs: tb.segs, cur: new(int)}
}
