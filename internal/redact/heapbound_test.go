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
}

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
	// Many short lines, each with a secret: two matches a line of 17 bytes, more than the gate allows
	// (FreeMatches), so withheld at the largest entry.
	{name: "secrets", text: func(n int) string { return repeatTo("password=hunter2\n", n) }},
	// The densest lines of secrets the gate lets through: two matches a line of 21 bytes.
	{name: "secrets under the gate", text: func(n int) string { return repeatTo("password=hunter2 abc\n", n) }},
	// The densest secrets found (#1087): a short secret key and a value of one character, every six bytes, on
	// lines, on one line, and as a query string. The reading of the release before holds what it finds for each
	// match, so these are withheld before it (FreeMatches); the heap of the reading up to there is measured.
	{name: "dense secrets", text: func(n int) string { return repeatTo("pwd=a\n", n) }},
	{name: "dense secrets on a line", text: func(n int) string { return repeatTo("pwd=a ", n) }},
	{name: "query", text: func(n int) string { return repeatTo("key=1&", n) }},
	// A run of slashes alone: the long-token rule reads it as empty parts between slashes (base64Windows), each
	// of which it held in 56 bytes before #1087.
	{name: "slashes", text: func(n int) string { return repeatTo("/", n) }},
	// Parts of one character between slashes: the most parts base64Windows keeps for the bytes.
	{name: "short parts", text: func(n int) string { return repeatTo("a/", n) }},
	// Look-alike colons after secret keys on every line: a fold every few bytes, so the folded views hold a
	// segment for each.
	{name: "dense folds", text: func(n int) string { return repeatTo("pwd\uff1aa\n", n) }},
	// The same with four letters after each secret: about as dense as the gate lets through.
	{name: "folds under the gate", text: func(n int) string { return repeatTo("pwd\uff1aabcd\n", n) }},
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
	epPiecesWithin  = "Redactor.PiecesWithin"
	epPiecesFitting = "Redactor.PiecesFitting"
)

// heapBound is the bound of the decision for an entry of n bytes.
func heapBound(n int) uint64 { return uint64(HeapFixed + HeapPerByte*int64(n)) } // #nosec G115 -- n is a size, never negative

// heapSample is the metric the probe samples: the bytes of heap objects, live or not yet swept.
const heapSample = "/memory/classes/heap/objects:bytes"

// heapProcs is the GOMAXPROCS peakLiveHeap measures under: CI's hosted runners have four cores, and the pacing of
// the collector, so the garbage a sample still holds, depends on how many run (#1087: ReplaceFrom on secrets held
// 34.9 MiB on 24 cores and less on four). A fixed count makes the figure the same on every machine.
const heapProcs = 4

// peakLiveHeap returns the most heap f held at once over the heap before it, sampled every half millisecond
// while the collector runs at GOGC 10 under a soft limit of 96 MiB and GOMAXPROCS heapProcs, so that a sample
// stays close to what f keeps alive.
func peakLiveHeap(f func()) uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(heapProcs))
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
