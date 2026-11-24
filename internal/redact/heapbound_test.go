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
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// The heap one entry holds. Two figures are checked against it (TestHeapPerByteOfEveryShape): the bound of one
// entry (HeapPerByte and HeapFixed, budget.go; #873, decision of 2026-11-21 on the heap), at the largest entry,
// on the shapes heapShapes does not mark over it; and the weight of an entry in the limit on redactions at once
// (HeapWeightPerByte, heaplimit.go; #1087, decision of 2026-11-22), on every shape at every size. A shape found
// later is added to the list.

// The largest entries the entry points get (budget.go names the sources).
const (
	// largestObject is the largest object a read tool or a context block passes: about 1.5 MiB, etcd's default
	// request limit (LargestEntry, which a read tool's answer never passes since #1106).
	largestObject = LargestEntry
	// largestLog is the most the log source reads of one stream (kube.MaxReadBytes).
	largestLog = 1 << 20
)

// heapShape is a shape of an entry: text returns n bytes of it.
type heapShape struct {
	name string
	text func(n int) string
	// overAt names the entry points at which the shape holds more than the bound of one entry (HeapPerByte) at
	// the largest entry, as measured on 2026-11-22 (#1087): there it is held only to its weight in the limit on
	// redactions at once (HeapWeightPerByte).
	overAt []string
}

// over reports whether sh is held only to its weight, not to the bound of one entry, at the entry point ep.
func (sh heapShape) over(ep string) bool { return slices.Contains(sh.overAt, ep) }

// repeatTo returns unit repeated to n bytes.
func repeatTo(unit string, n int) string { return strings.Repeat(unit, n/len(unit)+1)[:n] }

// pieces are the Pieces entry points with the answer laid out with fields, where the shapes dense in matches or
// in characters JSON escapes hold the most.
var pieces = []string{epPiecesWithin, epPiecesFitting}

// heapShapes are every shape known to hold much heap for its bytes: the heap tests measure each at every entry
// point.
var heapShapes = []heapShape{
	// Characters JSON writes as six-byte escapes: the answer is six times the value.
	{name: "escapes", text: func(n int) string { return repeatTo("\x01", n) }},
	// A letter and such a character (#1087, 2026-11-22): a match of the long-token rule every two bytes.
	{name: "letter and control", text: func(n int) string { return repeatTo("x\x01", n) }, overAt: pieces},
	// A letter and a next-line character (U+0085), which Go's expressions read as a space.
	{name: "letter and next line", text: func(n int) string { return repeatTo("x\u0085", n) }, overAt: pieces},
	// A token with such a character between its letters.
	{name: "token with controls", text: func(n int) string { return repeatTo("Ab3\x01dE5\x01gH7\x01 ", n) }, overAt: pieces},
	// Words and blanks on one line, nothing any rule matches.
	{name: "no match", text: func(n int) string { return repeatTo("lorem ipsum dolor sit amet ", n) }},
	// One long line with no blank: one token as long as the entry.
	{name: "long line", text: func(n int) string { return repeatTo("a", n) }},
	// Many short lines, nothing any rule matches: as many line breaks, and items of a sequence, as lines.
	{name: "short lines", text: func(n int) string { return repeatTo("lorem ipsum dol\n", n) }},
	// Line breaks alone: as many empty items of a sequence, or empty values of an answer, as bytes.
	{name: "blank lines", text: func(n int) string { return repeatTo("\n", n) }},
	// Lines of one letter: two bytes a line, as many pieces or strings as the gates let through.
	{name: "letters", text: func(n int) string { return repeatTo("x\n", n) }},
	// Lines of a key and a secret: two matches a line of 17 bytes.
	{name: "key lines", text: func(n int) string { return repeatTo("password=hunter2\n", n) }, overAt: []string{epStrings}},
	// The same with a word after each secret: two matches a line of 21 bytes.
	{name: "key lines with a word", text: func(n int) string { return repeatTo("password=hunter2 abc\n", n) }},
	// The densest secrets found (#1087): a short secret key and a value of one character, every six bytes, on
	// lines, on one line, and as a query string.
	{name: "dense secrets", text: func(n int) string { return repeatTo("pwd=a\n", n) },
		overAt: append([]string{epString, epStringTwice, epStringKeeping, epReplaceFrom}, pieces...)},
	{name: "dense secrets on a line", text: func(n int) string { return repeatTo("pwd=a ", n) }},
	{name: "query", text: func(n int) string { return repeatTo("key=1&", n) },
		overAt: append([]string{epString, epStringTwice, epStringKeeping, epStrings}, pieces...)},
	// Quoted pairs of a secret key and a one-letter value, as JSON writes them (#1087, 2026-11-22).
	{name: "quoted pairs", text: func(n int) string { return repeatTo(`"pwd":"a",`, n) },
		overAt: append([]string{epStrings}, pieces...)},
	// A list of addresses, and of IPv4 addresses: a match every few bytes.
	{name: "addresses", text: func(n int) string { return repeatTo("ab"+"@"+"cd.ef, ", n) }, overAt: pieces},
	// Addresses each followed by a control character (#1087, fifth pass of 2026-11-22): the heaviest shape measured,
	// 71.7 bytes per byte past HeapFixed through PiecesWithin at the largest object (111.6 MiB).
	{name: "addresses and control", text: func(n int) string { return repeatTo("ab"+"@"+"cd.ef\x01", n) }, overAt: pieces},
	{name: "ipv4 addresses", text: func(n int) string { return repeatTo("10.0.0.1\n", n) }},
	// A run of slashes alone: the long-token rule reads it as empty parts between slashes (base64Windows), each
	// of which it held in 56 bytes before #1087.
	{name: "slashes", text: func(n int) string { return repeatTo("/", n) }},
	// Parts of one character between slashes: the most parts base64Windows keeps for the bytes.
	{name: "short parts", text: func(n int) string { return repeatTo("a/", n) }},
	// Look-alike colons after secret keys on every line: a fold every few bytes, so the folded views hold a
	// segment for each.
	{name: "dense folds", text: func(n int) string { return repeatTo("pwd\uff1aa\n", n) }, overAt: []string{epPiecesValues}},
	{name: "folds with words", text: func(n int) string { return repeatTo("pwd\uff1aabcd\n", n) }},
	// Shapes of the controls view (controls.go, #852, pass of 2026-11-22). One control makes the whole text be read
	// again without it; before the view kept only the matches the other readings do not hold whole, its matches
	// were held a second time (StringChecked 62.8 MiB at the largest object). A control on every line of keys and
	// one-letter values (over the bound at the Pieces entry points on origin/main too, 58.7 MiB). A key a control
	// splits on every line, whose matches only the view finds.
	{name: "control then key lines", text: func(n int) string { return "\x01" + repeatTo("password=hunter2\n", n-1) },
		overAt: []string{epStrings}},
	{name: "token and control lines", text: func(n int) string { return repeatTo("token:\x01a\n", n) },
		overAt: append([]string{epPiecesValues}, pieces...)},
	{name: "split keys", text: func(n int) string { return repeatTo("pass\x00word=hunter2\n", n) }},
	// Shapes of the shown view (controls.go, decision of 2026-11-23): invalid bytes make the whole text be read
	// again with each run of them as one "?". Invalid bytes alone, one after every letter (a run and a gap every two
	// bytes), lone surrogates, dense secrets and addresses with an invalid byte after each, and a key an invalid
	// byte splits on every line. Measured on 2026-11-23 at the largest object, before and with the shown view: the
	// same peaks within 0.1 MiB at every entry point, the heaviest addresses and an invalid byte through
	// PiecesWithin (109.1 MiB, 70.1 bytes per byte past HeapFixed); the entry points of overAt are over the bound of
	// one entry before it too.
	{name: "invalid bytes", text: func(n int) string { return repeatTo("\xff", n) }},
	{name: "letter and invalid byte", text: func(n int) string { return repeatTo("x\x80", n) }},
	{name: "lone surrogates", text: func(n int) string { return repeatTo("a\xed\xa0\x80", n) },
		overAt: append([]string{epStrings}, pieces...)},
	{name: "dense secrets and invalid bytes", text: func(n int) string { return repeatTo("pwd=a\xff\n", n) },
		overAt: append([]string{epString, epStringTwice, epStringKeeping, epReplaceFrom, epPiecesValues}, pieces...)},
	{name: "addresses and invalid byte", text: func(n int) string { return repeatTo("ab"+"@"+"cd.ef\xff", n) },
		overAt: pieces}, // as "addresses and control": PiecesFitting takes the largest entry too since #1106
	{name: "keys split by invalid bytes", text: func(n int) string { return repeatTo("pass\xffword:a\n", n) }},
}

// heapEntryPoint is an entry point of the package, with the largest entry it can get.
type heapEntryPoint struct {
	name    string
	largest int
	// call returns the call of the entry point on an entry made of the text s, its input (the pieces, the
	// strings) built before, so that only what the redaction holds is measured, and the size of that entry as its
	// budget counts it. The call reports whether the entry was decided.
	call func(r *Redactor, s string) (run func() bool, size int)
}

// fieldPieces lays s out as a tool's answer at its densest: each line of s a data piece written as JSON under a
// one-letter key, after the key and a colon the tool writes, so that an entry of many short lines is an answer of
// many pieces.
func fieldPieces(s string) (ps []Piece, size int) {
	lines := strings.Split(s, "\n")
	ps = make([]Piece, 0, 2*len(lines))
	for _, l := range lines {
		ps = append(ps, Piece{Text: "\nk: "}, Piece{Text: l, Data: true, Form: JSON, Key: "k"})
		size += 4 + len(l) + 1
	}
	return ps, size
}

// valuePieces lays s out as an answer of values alone: each line a data piece under a one-letter key, with
// nothing written between them, the most pieces an answer can hold for its bytes.
func valuePieces(s string) (ps []Piece, size int) {
	lines := strings.Split(s, "\n")
	ps = make([]Piece, 0, len(lines))
	for _, l := range lines {
		ps = append(ps, Piece{Text: l, Data: true, Form: JSON, Key: "k"})
		size += len(l) + 1
	}
	return ps, size
}

// text is an entry point that takes one text.
func text(f func(r *Redactor, s string) bool) func(r *Redactor, s string) (func() bool, int) {
	return func(r *Redactor, s string) (func() bool, int) { return func() bool { return f(r, s) }, len(s) }
}

var heapEntryPoints = []heapEntryPoint{
	{epString, largestObject, text(func(r *Redactor, s string) bool { _, ok := r.StringChecked(s); return ok })},
	{epStringTwice, largestObject, text(func(r *Redactor, s string) bool {
		_, ok := r.StringTwiceChecked(s, nil)
		return ok
	})},
	{epStringKeeping, largestObject, text(func(r *Redactor, s string) bool {
		_, ok := r.StringKeepingChecked(s, "hunter")
		return ok
	})},
	{epStrings, largestObject, func(r *Redactor, s string) (func() bool, int) {
		items := strings.Split(s, "\n")
		return func() bool { _, ok := r.StringsChecked(items); return ok }, len(s) + 1
	}},
	{epPiecesWithin, largestObject, func(r *Redactor, s string) (func() bool, int) {
		ps, size := fieldPieces(s)
		return func() bool { _, ok := r.PiecesWithin(ps, -1); return ok }, size
	}},
	{epPiecesValues, largestObject, func(r *Redactor, s string) (func() bool, int) {
		ps, size := valuePieces(s)
		return func() bool { _, ok := r.PiecesWithin(ps, -1); return ok }, size
	}},
	{epPiecesFitting, largestObject, func(r *Redactor, s string) (func() bool, int) {
		ps, size := fieldPieces(s)
		return func() bool { _, ok := r.PiecesFitting(ps, func([]string) bool { return true }); return ok }, size
	}},
	{epReplaceFrom, largestLog, text(func(r *Redactor, s string) bool {
		_, ok := r.ReplaceFrom(s, len(s)/2, nil, func(kind, _ string) string { return kind }, func(t string) string { return t })
		return ok
	})},
}

// The names of the entry points in the heap tests.
const (
	epString        = "Redactor.StringChecked"
	epStringTwice   = "Redactor.StringTwiceChecked"
	epStringKeeping = "Redactor.StringKeepingChecked"
	epStrings       = "Redactor.StringsChecked"
	epPiecesWithin  = "Redactor.PiecesWithin"
	epPiecesValues  = epPiecesWithin + "/values"
	epPiecesFitting = "Redactor.PiecesFitting"
	epReplaceFrom   = "Redactor.ReplaceFrom"
)

// heapBound is the bound of the decision for an entry of n bytes.
func heapBound(n int) uint64 { return uint64(HeapFixed + HeapPerByte*int64(n)) } // #nosec G115 -- n is a size, never negative

// peakLiveHeap is redacttest.PeakLiveHeap: the most heap f held at once over the heap before it.
func peakLiveHeap(f func()) uint64 { return redacttest.PeakLiveHeap(f) }

// overTestLine reports whether h holds more than the line every heap test of a shape holds an entry to: more
// than heapWeightRoom percent of HeapWeightPerByte per byte past HeapFixed, at the entry's own size.
// TestHeapPerByteOfEveryShape and FuzzHeapWithinWeight both fail on it.
func (h heapRatio) overTestLine() bool { return h.perByte() > HeapWeightPerByte*heapWeightRoom/100 }

// heapSizes are the sizes of entry TestHeapPerByteOfEveryShape measures each shape at: small, middle and the
// largest object (the largest log for ReplaceFrom).
var heapSizes = []int{64 << 10, 512 << 10, largestObject}

// heapRatio is one measurement of TestHeapPerByteOfEveryShape.
type heapRatio struct {
	ep, shape string
	size      int
	peak      uint64
}

// perByte is the heap the entry held per byte past the fixed part (HeapFixed) of its weight, none when below it.
func (h heapRatio) perByte() float64 {
	return float64(max(int64(h.peak)-HeapFixed, 0)) / float64(h.size) // #nosec G115 -- a peak is far below 2^63
}

// The heap of one redaction, measured at every entry point on every shape known (heapShapes) at each of
// heapSizes, per byte past HeapFixed, is at most heapWeightRoom percent of HeapWeightPerByte: the weight each entry
// takes in the limit on redactions at once covers what it holds with a margin of 25 percent (#1087, decision of
// 2026-11-22). At the largest entry, a shape not marked over at that entry point (heapShape.overAt) is also held
// to the bound of one entry (HeapPerByte, decision of 2026-11-21 on #873). A reading over either is taken again, up
// to three runs, and the smallest counts (redacttest.SmallestPeak, #1112). The ratios are logged, the worst first.
// The entry points are measured in four tests, so that each runs in a binary of its own (three to five minutes
// each on the test runner); TestHeapPerByteCoversEveryEntryPoint checks that the four take them all. They run
// without the race detector only (the detector's shadow memory is no heap of the product).
func TestHeapPerByteOfEveryShapeStrings(t *testing.T) { heapPerByteOf(t, heapPerByteGroups[0]) }

func TestHeapPerByteOfEveryShapeTwice(t *testing.T) { heapPerByteOf(t, heapPerByteGroups[1]) }

func TestHeapPerByteOfEveryShapeSequences(t *testing.T) { heapPerByteOf(t, heapPerByteGroups[2]) }

func TestHeapPerByteOfEveryShapePieces(t *testing.T) { heapPerByteOf(t, heapPerByteGroups[3]) }

// heapPerByteGroups are the entry points of the four TestHeapPerByteOfEveryShape tests.
var heapPerByteGroups = [4][]string{
	{epString, epStringKeeping},
	{epStringTwice, epReplaceFrom},
	{epStrings, epPiecesValues},
	{epPiecesWithin, epPiecesFitting},
}

// The four TestHeapPerByteOfEveryShape tests measure every entry point of heapEntryPoints, each once.
func TestHeapPerByteCoversEveryEntryPoint(t *testing.T) {
	names, grouped := make([]string, 0, len(heapEntryPoints)), []string{}
	for _, ep := range heapEntryPoints {
		names = append(names, ep.name)
	}
	for _, g := range heapPerByteGroups {
		grouped = append(grouped, g...)
	}
	slices.Sort(names)
	slices.Sort(grouped)
	if !slices.Equal(names, grouped) {
		t.Errorf("the heap tests measure %v, the entry points are %v", grouped, names)
	}
}

// heapPerByteOf is the TestHeapPerByteOfEveryShape check on the entry points named.
func heapPerByteOf(t *testing.T, names []string) {
	if raceDetector {
		t.Skip("measures the heap of the product build")
	}
	r := defaultRedactor
	var ratios []heapRatio
	for _, ep := range heapEntryPoints {
		if !slices.Contains(names, ep.name) {
			continue
		}
		for _, sh := range heapShapes {
			for _, n := range heapSizes {
				n = min(n, ep.largest) // the log source's entry is at most largestLog
				t.Run(fmt.Sprintf("%s/%s/%d", ep.name, sh.name, n), func(t *testing.T) {
					if strings.HasSuffix(ep.name, "/values") && !strings.Contains(sh.text(64), "\n") {
						t.Skip("one line is one piece, as in the answer laid out with fields")
					}
					run, size := heapEntry(r, ep, sh, n)
					var ok bool
					oneEntryBound := n == ep.largest && !sh.over(ep.name)
					peak := redacttest.SmallestPeak(func() { ok = run() }, func(p uint64) bool {
						return heapRatio{size: size, peak: p}.overTestLine() || oneEntryBound && p > heapBound(size)
					})
					h := heapRatio{ep: ep.name, shape: sh.name, size: size, peak: peak}
					ratios = append(ratios, h)
					t.Logf("an entry of %d bytes: peak %.1f MiB, %.1f bytes per byte past HeapFixed, decided %v", size,
						mib(peak), h.perByte(), ok)
					if h.overTestLine() {
						t.Errorf("%s on an entry of %d bytes of %s held %.1f MiB, %.1f bytes per byte past HeapFixed: over %d percent of HeapWeightPerByte (%d)",
							ep.name, size, sh.name, mib(peak), h.perByte(), heapWeightRoom, HeapWeightPerByte)
					}
					if oneEntryBound && peak > heapBound(size) {
						t.Errorf("%s on an entry of %d bytes of %s held %.1f MiB, above the bound of one entry of %.1f MiB (%d per byte + %.0f MiB)",
							ep.name, size, sh.name, mib(peak), mib(heapBound(size)), HeapPerByte, mib(HeapFixed))
					}
				})
			}
		}
	}
	slices.SortFunc(ratios, func(a, b heapRatio) int { return int(b.perByte()*1000 - a.perByte()*1000) })
	var table strings.Builder
	for _, h := range ratios {
		fmt.Fprintf(&table, "%-34s %-24s %8d bytes  peak %6.1f MiB  %5.1f per byte\n", h.ep, h.shape, h.size, mib(h.peak), h.perByte())
	}
	t.Logf("heap per byte past HeapFixed, the worst first (HeapWeightPerByte %d, room %d percent):\n%s",
		HeapWeightPerByte, heapWeightRoom, table.String())
}

// heapEntry returns the call of ep on an entry of shape sh scaled so that the entry, as its budget counts it, is
// at most n bytes, and that size.
func heapEntry(r *Redactor, ep heapEntryPoint, sh heapShape, n int) (run func() bool, size int) {
	s := sh.text(n)
	if _, size := ep.call(r, s); size > n {
		s = sh.text(int(int64(n) * int64(len(s)) / int64(size)))
	}
	return ep.call(r, s)
}

func mib(n uint64) float64 { return float64(n) / (1 << 20) }

// A data piece the object hides whole (a secret key) of characters JSON writes as six-byte escapes, at the largest
// entry: the answer is six times the value, and cutting the spans at the hidden piece's end holds no index of
// every byte of the answer.
func TestHeapOfAHiddenPieceIsBounded(t *testing.T) {
	if raceDetector {
		t.Skip("measures the heap of the product build")
	}
	ps := []Piece{{Text: "password: "}, {Text: repeatTo("\x01", largestObject-10), Data: true, Form: JSON, Key: passwordKey}}
	var ok bool
	bound := heapBound(largestObject)
	peak := redacttest.SmallestPeak(func() { _, ok = defaultRedactor.PiecesWithin(ps, -1) }, func(p uint64) bool { return p > bound })
	t.Logf("a hidden piece of %d bytes: peak %.1f MiB (bound %.1f MiB), decided %v", largestObject, mib(peak), mib(bound), ok)
	if peak > bound {
		t.Errorf("a hidden piece of %d bytes of escapes held %.1f MiB, above the bound of %.1f MiB", largestObject,
			mib(peak), mib(bound))
	}
}

// measuredHeapUnits are the units of the shapes the passes on #1087 measured at every entry point (2026-11-21 and
// 2026-11-22), the heaviest of them an address followed by a control byte (the shape "addresses and control" of the test table) (71.7 bytes per byte past HeapFixed through PiecesWithin).
// Each is a shape of heapShapes, so the table test measures it in every run.
var measuredHeapUnits = []string{
	"\x01", "x\x01", "x\u0085", "Ab3\x01dE5\x01gH7\x01 ", "lorem ipsum dolor sit amet ", "/", `"pwd":"a",`,
	"ab" + "@" + "cd.ef, ", "ab" + "@" + "cd.ef\x01", "password=hunter2\n", "token:\x01a\n", "pass\x00word=hunter2\n",
}

func TestHeapShapesHoldTheMeasuredOnes(t *testing.T) {
	for _, unit := range measuredHeapUnits {
		found := false
		for _, sh := range heapShapes {
			if sh.text(4*len(unit)) == repeatTo(unit, 4*len(unit)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the measured shape %q repeated is not in heapShapes", unit)
		}
	}
}
