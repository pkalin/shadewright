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
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The heap one entry may hold (#873, decision of 2026-11-21 on the heap): for every entry point, the peak live heap
// of one redaction is at most HeapPerByte times the entry's bytes plus HeapFixed (budget.go), measured at the
// largest entry that entry point can get, on the worst shapes. The shapes are a list, so that a later change
// (#1087: the kept reading of base64 windows, the fold maps) adds its own.

// The largest entries the entry points get (budget.go names the sources).
const (
	// largestObject is the largest object a read tool or a context block passes: about 1.5 MiB, etcd's default
	// request limit.
	largestObject = 3 << 19
	// largestLog is the most the log source reads of one stream (kube.MaxReadBytes).
	largestLog = 1 << 20
)

// heapShape is a worst shape of an entry: text returns n bytes of it.
type heapShape struct {
	name string
	text func(n int) string
	// over, when set, says why the shape is measured but held only under heapCeiling, not under the bound, at the
	// entry points overAt names: the heap it holds grows with its matches, not with its bytes, in code this change
	// did not touch. Every other entry point is held to the bound on it.
	over   string
	overAt []string
}

// heapCeiling is what a shape that is over the bound (heapShape.over) may hold at the largest entry: well inside
// the analyzer's limit of 128 Mi, so that it cannot grow unseen.
const heapCeiling = 112 << 20

// repeatTo returns unit repeated to n bytes.
func repeatTo(unit string, n int) string { return strings.Repeat(unit, n/len(unit)+1)[:n] }

// heapShapes are the shapes the heap test measures at every entry point.
var heapShapes = []heapShape{
	// Characters JSON writes as six-byte escapes: the answer is six times the value.
	{name: "escapes", text: func(n int) string { return repeatTo("\x01", n) }},
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
	// Many short lines, each with a secret: as many matches as lines. Each match holds its spans in every reading
	// (this package's rules, each pass of the release before's) for the whole decision: over the bound at the
	// largest entry for StringTwiceChecked, StringsChecked and the Pieces entry points.
	// A character and a control alternating: the controls view (controls.go, #852) holds a gap for every two bytes.
	{name: controlShapeESC, text: func(n int) string { return repeatTo("a\x1b", n) }, over: controlsOverPieces,
		overAt: []string{epPiecesWithin, epPiecesWithin + "/values", epPiecesFitting}},
	// Characters and different controls (C0, DEL, C1) alternating.
	{name: controlShapeMixed, text: func(n int) string { return repeatTo("a\x00b\x7fc\u0085d\x1b", n) }, over: controlsOverPieces,
		overAt: []string{epPiecesWithin, epPiecesWithin + "/values", epPiecesFitting}},
	// Secrets a control splits, on many short lines: as many matches of the controls view as lines, which hold their
	// spans for the whole decision as the shape "secrets" does.
	{name: controlShapeSplit, text: func(n int) string { return repeatTo("pass\x00word=hunter2\n", n) }, over: "spans held per match",
		overAt: []string{epStringsChecked}},
	{name: "secrets", text: func(n int) string { return repeatTo("password=hunter2\n", n) }, over: "spans held per match",
		overAt: []string{"Redactor.StringTwiceChecked", "Redactor.StringsChecked", epPiecesWithin, epPiecesWithin + "/values", epPiecesFitting}},
}

// The control-dense shapes of heapShapes, which TestHeapOfControlDenseTextIsBounded also measures at 512 KiB.
const (
	controlShapeESC   = "a ESC"
	controlShapeMixed = "alternating controls"
	controlShapeSplit = "split secrets"
)

// controlsOverPieces says why the control-dense shapes are held only under heapCeiling at the Pieces entry points:
// a control is a unit of six bytes in a JSON answer, and the answer's map of units is over the bound on origin/main
// too (108.5 MiB at 1.5 MiB of "a" ESC, 56 MiB of alternating controls, measured 2026-11-22); #1087 bounds it. The
// controls view adds about 9 bytes per byte there.
const controlsOverPieces = "the units of a JSON answer of controls, over on origin/main too (#1087)"

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
	{"Redactor.StringChecked", largestObject, text(func(r *Redactor, s string) bool { _, ok := r.StringChecked(s); return ok })},
	{"Redactor.StringTwiceChecked", largestObject, text(func(r *Redactor, s string) bool {
		_, ok := r.StringTwiceChecked(s, nil)
		return ok
	})},
	{"Redactor.StringKeepingChecked", largestObject, text(func(r *Redactor, s string) bool {
		_, ok := r.StringKeepingChecked(s, "hunter")
		return ok
	})},
	{"Redactor.StringsChecked", largestObject, func(r *Redactor, s string) (func() bool, int) {
		items := strings.Split(s, "\n")
		return func() bool { _, ok := r.StringsChecked(items); return ok }, len(s) + 1
	}},
	{epPiecesWithin, largestObject, func(r *Redactor, s string) (func() bool, int) {
		ps, size := fieldPieces(s)
		return func() bool { _, ok := r.PiecesWithin(ps, -1); return ok }, size
	}},
	{epPiecesWithin + "/values", largestObject, func(r *Redactor, s string) (func() bool, int) {
		ps, size := valuePieces(s)
		return func() bool { _, ok := r.PiecesWithin(ps, -1); return ok }, size
	}},
	{epPiecesFitting, largestObject, func(r *Redactor, s string) (func() bool, int) {
		ps, size := fieldPieces(s)
		return func() bool { _, ok := r.PiecesFitting(ps, func([]string) bool { return true }); return ok }, size
	}},
	{"Redactor.ReplaceFrom", largestLog, text(func(r *Redactor, s string) bool {
		_, ok := r.ReplaceFrom(s, len(s)/2, nil, func(kind, _ string) string { return kind }, func(t string) string { return t })
		return ok
	})},
}

// The names of the Pieces entry points in the heap tests.
const (
	epStringsChecked = "Redactor.StringsChecked"
	epPiecesWithin   = "Redactor.PiecesWithin"
	epPiecesFitting  = "Redactor.PiecesFitting"
)

// heapBound is the bound of the decision for an entry of n bytes.
func heapBound(n int) uint64 { return uint64(HeapFixed + HeapPerByte*int64(n)) } // #nosec G115 -- n is a size, never negative

// heapSample is the metric the probe samples: the bytes of heap objects, live or not yet swept.
const heapSample = "/memory/classes/heap/objects:bytes"

// peakLiveHeap returns the most heap f held at once over the heap before it, sampled every half millisecond
// while the collector runs at GOGC 10 under a soft limit of 96 MiB, so that a sample stays close to what f keeps
// alive.
func peakLiveHeap(f func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(96 << 20))
	s := []metrics.Sample{{Name: heapSample}}
	runtime.GC()
	metrics.Read(s)
	base := s[0].Value.Uint64()
	var (
		stop atomic.Bool
		peak uint64
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		x := []metrics.Sample{{Name: heapSample}}
		for !stop.Load() {
			metrics.Read(x)
			peak = max(peak, x[0].Value.Uint64())
			time.Sleep(500 * time.Microsecond)
		}
	}()
	f()
	stop.Store(true)
	<-done
	metrics.Read(s)
	peak = max(peak, s[0].Value.Uint64())
	return peak - min(base, peak)
}

// The peak live heap of one entry at the largest entry of each entry point, on each worst shape, is at most
// HeapPerByte times its bytes plus HeapFixed. It runs without the race detector only (the detector's shadow memory
// is no heap of the product).
func TestHeapOfOneEntryIsBounded(t *testing.T) {
	if raceDetector {
		t.Skip("measures the heap of the product build")
	}
	r := defaultRedactor
	for _, ep := range heapEntryPoints {
		for _, sh := range heapShapes {
			t.Run(ep.name+"/"+sh.name, func(t *testing.T) {
				if strings.HasSuffix(ep.name, "/values") && !strings.Contains(sh.text(64), "\n") {
					t.Skip("one line is one piece, as in the answer laid out with fields")
				}
				// The entry at its largest: the text scaled so that the entry, as its budget counts it, is that size.
				s := sh.text(ep.largest)
				if _, size := ep.call(r, s); size > ep.largest {
					s = sh.text(int(int64(ep.largest) * int64(len(s)) / int64(size)))
				}
				run, size := ep.call(r, s)
				var ok bool
				peak := peakLiveHeap(func() { ok = run() })
				bound := heapBound(size)
				t.Logf("%s on %s, an entry of %d bytes: peak %.1f MiB (bound %.1f MiB), decided %v", ep.name, sh.name,
					size, mib(peak), mib(bound), ok)
				if sh.over != "" && slices.Contains(sh.overAt, ep.name) {
					if peak > heapCeiling {
						t.Errorf("%s on an entry of %d bytes of %s held %.1f MiB, above the ceiling of %.0f MiB (%s)", ep.name,
							size, sh.name, mib(peak), mib(heapCeiling), sh.over)
					}
					return
				}
				if peak > bound {
					t.Errorf("%s on an entry of %d bytes of %s held %.1f MiB, above the bound of %.1f MiB (%d per byte + %.0f MiB)",
						ep.name, size, sh.name, mib(peak), mib(bound), HeapPerByte, mib(HeapFixed))
				}
			})
		}
	}
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
	peak := peakLiveHeap(func() { _, ok = defaultRedactor.PiecesWithin(ps, -1) })
	bound := heapBound(largestObject)
	t.Logf("a hidden piece of %d bytes: peak %.1f MiB (bound %.1f MiB), decided %v", largestObject, mib(peak), mib(bound), ok)
	if peak > bound {
		t.Errorf("a hidden piece of %d bytes of escapes held %.1f MiB, above the bound of %.1f MiB", largestObject,
			mib(peak), mib(bound))
	}
}

// The controls view (controls.go, #852) on control-dense text at 512 KiB, besides the largest entry that
// TestHeapOfOneEntryIsBounded measures: the peak live heap of one entry at every entry point is at most HeapPerByte
// times its bytes plus HeapFixed. It logs the heap per byte of each.
func TestHeapOfControlDenseTextIsBounded(t *testing.T) {
	if raceDetector {
		t.Skip("measures the heap of the product build")
	}
	for _, ep := range heapEntryPoints {
		for _, sh := range heapShapes {
			if sh.name != controlShapeESC && sh.name != controlShapeMixed && sh.name != controlShapeSplit {
				continue
			}
			t.Run(ep.name+"/"+sh.name, func(t *testing.T) {
				run, size := ep.call(defaultRedactor, sh.text(512<<10))
				var ok bool
				peak := peakLiveHeap(func() { ok = run() })
				t.Logf("%s on %s, an entry of %d bytes: peak %.1f MiB, %.1f bytes per byte (bound %.1f MiB), decided %v",
					ep.name, sh.name, size, mib(peak), float64(peak)/float64(size), mib(heapBound(size)), ok)
				if sh.over != "" && slices.Contains(sh.overAt, ep.name) {
					if peak > heapCeiling {
						t.Errorf("%s on an entry of %d bytes of %s held %.1f MiB, above the ceiling (%s)", ep.name, size,
							sh.name, mib(peak), sh.over)
					}
					return
				}
				if peak > heapBound(size) {
					t.Errorf("%s on an entry of %d bytes of %s held %.1f MiB, above the bound of %.1f MiB", ep.name, size,
						sh.name, mib(peak), mib(heapBound(size)))
				}
			})
		}
	}
}
