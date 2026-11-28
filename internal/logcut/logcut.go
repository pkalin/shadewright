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

// Package logcut holds the one cut of a container log to its newest part within a bound. Every place that cuts
// a log for the excerpt calls [Tail]: the log source on the bytes it read (internal/providers/logs/kube), and the
// context builder and get_pod_logs on redacted text (internal/incidentcontext: RenderLog, FitLog, the cut of
// the whole context and the room a second read takes from the first excerpt). The rules that a non-blank last
// line never gives an empty excerpt and that an excerpt starts at a line a reader keeps live only here (decisions
// of 2026-11-28 on #1055).
package logcut

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

// Size returns the size of s[start:end] as a bound counts it. [Tail] calls it only with start and end on rune
// boundaries, start <= end. It need not grow with end-start: a rendered size can shrink when a cut removes
// part of a delimiter that rendering would have replaced by a longer text, so [Tail] never searches it as if
// it did.
type Size func(start, end int) int

// Joined returns the size of s[hs:he] followed by s[start:end], as a bound counts it, where s[hs:he] is one whole
// line with its line ending and he <= start. [TailJoined] calls it to check an excerpt that keeps the first line
// of an entry and its newest lines without the lines between; nil means the size of the two parts is the sum of
// their sizes less the size of nothing (true of [Bytes]).
type Joined func(hs, he, start, end int) int

// Bytes is the [Size] of a text in bytes.
func Bytes(start, end int) int { return end - start }

// Kind says what a [Cut] kept.
type Kind int

// Kinds of a cut.
const (
	// Whole: all of the log fits, nothing is left out.
	Whole Kind = iota
	// Lines: the newest whole lines, from a line a reader keeps (not a continuation line, [LeadingContinuation]).
	// Also the cut of a log of only blank lines, which keeps nothing.
	Lines
	// HeaderKept: the newest whole lines are continuation lines (the frames of a stack trace, say); the nearest
	// line before them that is not one, the first line of their entry, is kept ahead of them, and the lines
	// between are left out.
	HeaderKept
	// HeaderLeftOut: the newest whole lines are continuation lines, and the first line of their entry is in the
	// log but does not fit with them.
	HeaderLeftOut
	// NoHeader: the newest whole lines are continuation lines and no line before them is one a reader keeps.
	NoHeader
	// LastLine: a tail of the last non-blank line, which alone is longer than the bound.
	LastLine
	// Dropped: nothing kept, from [Read] only: what [Tail] keeps is the tail of a continuation line that a
	// reader would drop whole.
	Dropped
)

// Cut is where the excerpt of a log s is: s[HeadStart:HeadEnd] followed by s[Start:End]. HeadStart equals
// HeadEnd unless Kind is HeaderKept.
type Cut struct {
	HeadStart, HeadEnd, Start, End int
	Kind                           Kind
	// Orphan: with Kind LastLine, the last line is a continuation line and no line before it is one a reader
	// keeps. [Read] keeps nothing then.
	Orphan bool
	// fit is the first line start whose newest whole lines fit (steps 2 and 3), before the continuation lines at
	// their start are left out: [Read] keeps them, for the reader to drop.
	fit int
}

// Cut reports whether anything of the log was left out.
func (c Cut) Cut() bool { return c.Kind != Whole }

// Text returns the excerpt of s.
func (c Cut) Text(s string) string { return s[c.HeadStart:c.HeadEnd] + s[c.Start:c.End] }

// First returns where the excerpt starts in s.
func (c Cut) First() int {
	if c.HeadEnd > c.HeadStart {
		return c.HeadStart
	}
	return c.Start
}

// Ranges returns the parts of s the excerpt keeps, in order, each as start and end.
func (c Cut) Ranges() [][2]int {
	if c.HeadEnd > c.HeadStart {
		return [][2]int{{c.HeadStart, c.HeadEnd}, {c.Start, c.End}}
	}
	return [][2]int{{c.Start, c.End}}
}

// maxJoinedChecks bounds the calls of a [Joined] in one cut: each may render the whole excerpt, and a cut that
// finds no fit within them keeps the newest lines without the first line of their entry (HeaderLeftOut).
const maxJoinedChecks = 8

// Tail is [TailJoined] with a nil [Joined].
func Tail(s string, bound int, size Size) Cut { return TailJoined(s, bound, size, nil) }

// TailJoined returns the excerpt of log s that fits in bound, and what it kept. size counts the bound ([Bytes]
// when nil); joined counts an excerpt of two parts (nil: the sum of the parts). In order:
//
//  1. All of s when it fits.
//  2. The longest suffix of whole lines that fits, when it is not blank. A line is whole when it starts s or
//     follows a newline. The excerpt starts at a line that is not a continuation line ([LeadingContinuation]):
//     the continuation lines at the start of that suffix are left out (Lines). When every line of the suffix is
//     a continuation line (a stack trace's "\tat" frames, say), the nearest line before it that is not one, the
//     first line of their entry, is kept ahead of the newest of them that fit with it, and the lines between are
//     left out (HeaderKept). When no newest line fits with that first line, the suffix is kept without it
//     (HeaderLeftOut); when there is no such line, the suffix is kept as it is (NoHeader).
//  3. With the trailing blank lines left out, step 2 again.
//  4. The last non-blank line alone (LastLine): its longest tail that fits and is not blank, ending at the end of
//     the line, then without its line ending, then without trailing blanks (ASCII, then Unicode). Such a tail
//     starts on a rune boundary, never strictly inside a redaction marker shape ([redact.MarkerShapes]), and
//     never on a space, a tab, or a "-" followed by a blank or the end of the line: a reader drops a first line
//     that starts that way as a possible continuation ([LeadingContinuation]), so the tail would be lost.
//  5. The last-line rule: the same tails, now allowed to start inside a run of more than three "*", which is
//     not one marker the redactor writes (it writes at most three) and every part of which is only "*".
//  6. When the last line has no start a reader keeps whose tail fits (a line of only "- " items, say), steps 4
//     and 5 again, now allowed to start on a blank or a "-": the excerpt is then a tail of the last line that a
//     reader drops, never nothing (decision of 2026-11-28 on #1055).
//
// It gives an empty excerpt for a log with a non-blank line only when bound is smaller than the shortest tail
// steps 4 to 6 may keep: the longest marker shape plus one byte for the line, about 45 bytes, or, for a rendered
// size, that plus what rendering adds. It only ever removes text and reads s as bytes, so it may be called on
// text before or after redaction; on a log as read, before a reader drops its leading continuation lines, call
// [Read]. Its work is linear in the size of s, apart from at most maxJoinedChecks calls of joined.
func TailJoined(s string, bound int, size Size, joined Joined) Cut {
	if size == nil {
		size = Bytes
	}
	if s == "" {
		return Cut{}
	}
	if size(0, len(s)) <= bound {
		return Cut{End: len(s)}
	}
	if c, ok := wholeLines(s, len(s), bound, size, joined); ok {
		return c
	}
	t := NonBlankEnd(s)
	if t == 0 {
		return Cut{Start: len(s), End: len(s), Kind: Lines}
	}
	if t < len(s) {
		if c, ok := wholeLines(s, t, bound, size, joined); ok {
			return c
		}
	}
	ls := strings.LastIndexByte(s[:t-1], '\n') + 1
	noNL := ls + len(strings.TrimRight(s[ls:t], "\r\n"))
	noASCII := ls + len(strings.TrimRight(s[ls:noNL], " \t"))
	noSpace := ls + len(strings.TrimRightFunc(s[ls:noASCII], unicode.IsSpace))
	ends := []int{t}
	for _, e := range []int{noNL, noASCII, noSpace} {
		if e > ls && e != ends[len(ends)-1] {
			ends = append(ends, e)
		}
	}
	// The line holds a non-blank rune before noSpace, so s[j:e] is not blank exactly when j < noSpace.
	orphan := LeadingContinuation(s[:t]) == t
	markers := redact.MarkerShapes(s[ls:t])
	for _, step := range []struct{ splitRuns, anyStart bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		for _, e := range ends {
			if j, ok := lineTail(s, ls, e, noSpace, bound, size, markers, step.splitRuns, step.anyStart); ok {
				return Cut{Start: j, End: e, HeadStart: j, HeadEnd: j, Kind: LastLine, Orphan: orphan}
			}
		}
	}
	return Cut{Start: len(s), End: len(s), HeadStart: len(s), HeadEnd: len(s), Kind: LastLine, Orphan: orphan}
}

// Read is [Tail] in bytes for a log as read, before a reader drops its leading continuation lines (the log
// source's cut). The tail of a continuation last line with no line before it that a reader keeps is left out
// (Dropped): the reader drops such a line whole, but its tail no longer starts the way the reader drops, so the
// safeguard against the value of a secret whose key line was not read would not hold. Whole lines are kept as
// the reader gets them: the continuation lines at the start of the newest lines that fit stay (Lines), and so do
// newest lines that are all continuation lines with no line before them a reader keeps (NoHeader); the reader
// drops both, as it did before this cut.
func Read(s string, bound int) Cut {
	c := Tail(s, bound, nil)
	switch {
	case c.Kind == LastLine && c.Orphan:
		return Cut{Start: len(s), End: len(s), HeadStart: len(s), HeadEnd: len(s), Kind: Dropped}
	case c.Kind == Lines && c.fit < c.Start:
		c.Start, c.HeadStart, c.HeadEnd = c.fit, c.fit, c.fit
	}
	return c
}

// Redact returns the excerpt c of s, a log as read, as redaction by r writes it, valid UTF-8, and false when
// redaction withheld the log whole (r's work budget; the text is then empty). Nothing is cut before redaction:
// r reads all of s, and the excerpt keeps the parts of what it wrote that c keeps, so a match that the cut
// boundary splits, or that starts in a line the cut leaves out, is the marker of the part kept, never a fragment
// of what it hides (#840). It also holds what the release before #845 hid (redact.Redactor.ReplaceFrom), from the
// first byte kept on, after the leading continuation lines a reader drops, or from that byte when c keeps such
// lines because their entry's first line was read (HeaderLeftOut). It is the log source's step after its cut
// ([Read]), here so that every source and the fake sources of tests run the same code.
func Redact(r *redact.Redactor, s string, c Cut) (string, bool) {
	ranges := c.Ranges()
	drop := LeadingContinuation
	if c.Kind == HeaderLeftOut {
		drop = func(string) int { return 0 }
	}
	pos := 0
	// part returns the parts of the n bytes at pos that c keeps, through f, and moves pos past them. f gets the
	// bytes skipped at the start and the bytes kept, for each kept range the n bytes meet.
	part := func(n int, f func(skip, kept int) string) string {
		start := pos
		pos += n
		var out strings.Builder
		for _, rg := range ranges {
			if lo, hi := max(start, rg[0]), min(pos, rg[1]); lo < hi {
				out.WriteString(f(lo-start, hi-lo))
			}
		}
		return out.String()
	}
	text, ok := r.ReplaceFrom(s, c.First(), drop,
		func(kind, m string) string {
			return part(len(m), func(_, kept int) string { return redact.Marker(kind, kept) })
		},
		func(seg string) string {
			return part(len(seg), func(skip, kept int) string { return seg[skip : skip+kept] })
		})
	if !ok {
		return "", false
	}
	return strings.ToValidUTF8(text, "?"), true
}

// String is [Tail] in bytes, returning the excerpt itself and whether anything was left out.
func String(s string, n int) (string, bool) {
	c := Tail(s, n, nil)
	return c.Text(s), c.Cut()
}

// wholeLines returns the cut of step 2 of [TailJoined] on s[:end], and whether the newest whole lines that fit
// are not blank. Every later suffix is part of a blank one when it is blank, so the first line start i whose
// suffix fits decides.
func wholeLines(s string, end, bound int, size Size, joined Joined) (Cut, bool) {
	i := -1
	for at := 0; at < end; {
		if size(at, end) <= bound {
			i = at
			break
		}
		nl := strings.IndexByte(s[at:end], '\n')
		if nl < 0 {
			break
		}
		at += nl + 1
	}
	if i < 0 || strings.TrimSpace(s[i:end]) == "" {
		return Cut{}, false
	}
	if h := i + LeadingContinuation(s[i:end]); h < end {
		return Cut{Start: h, End: end, HeadStart: h, HeadEnd: h, Kind: Lines, fit: i}, true
	}
	hs := headerBefore(s, i)
	if hs < 0 {
		return Cut{Start: i, End: end, HeadStart: i, HeadEnd: i, Kind: NoHeader}, true
	}
	he := hs + strings.IndexByte(s[hs:], '\n') + 1 // a line before i ends with a newline
	// s[x:end] is not blank exactly when x is at most the start of the last non-blank rune.
	nonBlank := len(strings.TrimRightFunc(s[:end], unicode.IsSpace))
	checks := 0
	for x := i; x < end && x < nonBlank; {
		if fits(hs, he, x, end, bound, size, joined, &checks) {
			if x == he {
				return Cut{Start: hs, End: end, HeadStart: hs, HeadEnd: hs, Kind: Lines}, true
			}
			return Cut{HeadStart: hs, HeadEnd: he, Start: x, End: end, Kind: HeaderKept}, true
		}
		if checks >= maxJoinedChecks {
			break
		}
		nl := strings.IndexByte(s[x:end], '\n')
		if nl < 0 {
			break
		}
		x += nl + 1
	}
	return Cut{Start: i, End: end, HeadStart: i, HeadEnd: i, Kind: HeaderLeftOut}, true
}

// fits reports whether s[hs:he] followed by s[x:end] fits in bound: by the sum of the sizes of the parts, then,
// with joined, by joined (counted in checks).
func fits(hs, he, x, end, bound int, size Size, joined Joined, checks *int) bool {
	if size(hs, he)+size(x, end)-size(end, end) > bound {
		return false
	}
	if joined == nil {
		return true
	}
	*checks++
	return joined(hs, he, x, end) <= bound
}

// headerBefore returns the start of the nearest line that ends before i (a line start of s) and is not a
// continuation line ([LeadingContinuation]), or -1 when there is none.
func headerBefore(s string, i int) int {
	for end := i; end > 0; {
		start := strings.LastIndexByte(s[:end-1], '\n') + 1
		if LeadingContinuation(s[start:end]) == 0 {
			return start
		}
		end = start
	}
	return -1
}

// LeadingContinuation is providers.LeadingContinuation, the reader's rule: the length of the lines at the start of
// a log excerpt that may continue an entry whose first line is not in it (blank lines, lines that start with a
// space or a tab, "-" alone or followed by a blank). The cut decides with it which lines are continuation lines.
func LeadingContinuation(s string) int { return providers.LeadingContinuation(s) }

// lineTail returns the first start j in the line s[ls:e] (the line start, or a rune start step 4 of [TailJoined]
// allows, or with anyStart also one on a blank or a "-", step 6) whose s[j:e] fits and is not blank: j < noSpace.
// markers are the marker shapes of the line, from ls.
func lineTail(s string, ls, e, noSpace, bound int, size Size, markers [][]int, splitRuns, anyStart bool) (int, bool) {
	mi := 0
	for j := ls; j < e && j < noSpace; {
		for mi < len(markers) && ls+markers[mi][1] <= j {
			mi++
		}
		if mi < len(markers) && ls+markers[mi][0] < j && (!splitRuns || !isLongStarRun(s[ls:], markers[mi])) {
			// Strictly inside a marker shape: the next start is the marker's end.
			j = ls + markers[mi][1]
			continue
		}
		if (anyStart || j == ls || !continuationStart(s, j, e)) && size(j, e) <= bound {
			return j, true
		}
		_, n := utf8.DecodeRuneInString(s[j:e])
		j += n
	}
	return 0, false
}

// continuationStart reports whether a tail s[j:e] starting inside a line would start the way a reader takes for
// the continuation of an entry and drops: a space, a tab, or "-" followed by a blank or the end of the line.
func continuationStart(s string, j, e int) bool {
	switch s[j] {
	case ' ', '\t':
		return true
	case '-':
		return j+1 >= e || s[j+1] == ' ' || s[j+1] == '\t' || s[j+1] == '\r' || s[j+1] == '\n'
	}
	return false
}

// isLongStarRun reports whether m, a range of s from [redact.MarkerShapes], is a run of more than three "*".
func isLongStarRun(s string, m []int) bool { return s[m[0]] == '*' && m[1]-m[0] > len("***") }

// NonBlankEnd returns the end of s without its trailing lines that hold only whitespace, keeping the line
// ending of the last non-blank line; 0 when s is all whitespace.
func NonBlankEnd(s string) int {
	end := len(s)
	for end > 0 {
		lineEnd := end
		if s[lineEnd-1] == '\n' {
			lineEnd--
		}
		lineStart := strings.LastIndexByte(s[:lineEnd], '\n') + 1
		if strings.TrimSpace(s[lineStart:end]) != "" {
			return end
		}
		end = lineStart
	}
	return 0
}
