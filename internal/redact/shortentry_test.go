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
	"context"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// keep is the repl of a test that only reads the flag: it writes the marker of each match.
func keep(kind, text string) string { return Marker(kind, len(text)) }

// same is the between of such a test.
func same(s string) string { return s }

// A short entry runs at once and is decided as on an idle redactor while the limit on the heap of the redactions
// running at once is held whole by another entry: the collector's fingerprint never waits for an analysis (#1087,
// decision of 2026-11-22).
func TestShortEntryDoesNotWaitForTheHeapLimit(t *testing.T) {
	note := `Failed to pull image: password=hunter2 token "Ab3dE5gH7jK9mN1pQ3sT5vX7zB9dF1hJ3" at 10.0.0.1`
	idle, ok := ReplaceCheckedShort(note, keep, same)
	if !ok || strings.Contains(idle, "hunter2") {
		t.Fatalf("on an idle redactor: %q, decided %v", idle, ok)
	}
	l := newHeapLimiter(64 << 20)
	l.maxWait = time.Hour // a wait of the ordinary entry points would outlast the test
	withHeapLimiter(l, func() {
		held, ok := l.acquire(context.Background(), largestObject, nil)
		if !ok {
			t.Fatal("the entry that holds the limit did not run")
		}
		defer held()
		done := make(chan struct{})
		var got string
		go func() {
			defer close(done)
			got, ok = ReplaceCheckedShort(note, keep, same)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a short entry waited for the heap limit")
		}
		if !ok || got != idle {
			t.Errorf("with the limit held: %q, decided %v; on an idle redactor %q", got, ok, idle)
		}
	})
}

// An entry longer than MaxShortEntry is withheld whole by the short entry point, before any work: it cannot be
// used for large text.
func TestShortEntryRefusesLongText(t *testing.T) {
	called := false
	out, ok := ReplaceCheckedShort(strings.Repeat("a", MaxShortEntry+1), func(string, string) string {
		called = true
		return ""
	}, func(string) string { called = true; return "" })
	if ok || out != Withheld || called {
		t.Errorf("an entry of %d bytes: %q, decided %v, callbacks called %v", MaxShortEntry+1, out, ok, called)
	}
	if _, ok := ReplaceCheckedShort(strings.Repeat("a", MaxShortEntry), keep, same); !ok {
		t.Errorf("an entry of MaxShortEntry bytes was withheld")
	}
}

// shortEntryPoints are the short entry points the heap tests measure.
var shortEntryPoints = []struct {
	name string
	run  func(s string, repl func(kind, text string) string, between func(string) string) (string, bool)
}{{"ReplaceCheckedShort", ReplaceCheckedShort}, {shownAndReadEntry,
	func(s string, repl func(kind, text string) string, between func(string) string) (string, bool) {
		out, _, ok := ReplaceCheckedShortShownAndRead(s, repl, between)
		return out, ok
	}}}

// warmShortEntryPoints runs one-time process initialization (released.exprs compiles each kept rule's meter.Expr
// lazily on first use), so that no measurement charges package setup to its first shape.
func warmShortEntryPoints() {
	for _, e := range shortEntryPoints {
		e.run("password=hunter2\x01\xff", keep, same)
	}
}

// checkShortEntryHeap fails t unless b is within ShortEntryHeap. Over (a collection found more live) and
// undecided (none did, but the runtime counted more) both fail: in the stop-the-world process the upper reading is
// over the live peak by at most about 2% of the process's heap (about 0.15 MiB over the 7.3 MiB of this test's
// process) however busy the machine is, so an undecided reading is an entry within that gap of the bound, which has
// lost its margin (#1204).
func checkShortEntryHeap(t interface {
	Helper()
	Errorf(format string, args ...any)
}, what string, b redacttest.HeapBounds) {
	t.Helper()
	switch b.Verdict(ShortEntryHeap) {
	case redacttest.HeapOver:
		t.Errorf("%s: a short entry of %d bytes held over ShortEntryHeap (%.2f MiB): %v", what, MaxShortEntry,
			mib(ShortEntryHeap), b)
	case redacttest.HeapUndecided:
		t.Errorf("%s: undecided, a short entry of %d bytes is within the readings' gap of ShortEntryHeap "+
			"(%.2f MiB), a margin lost: %v", what, MaxShortEntry, mib(ShortEntryHeap), b)
	}
}

// The heap one short entry holds, at each short entry point and on every shape known at MaxShortEntry, is at most
// ShortEntryHeap: the figure the room next to the heap limit counts for each of its slots. No shape is withheld for
// its work: the shown short entry point withholds some shapes for what they hold (a control after a token, keys
// invalid bytes split), after its readings ran, and its work on every shape stays within the budget. The heap is
// measured in a process whose collections stop the world (redacttest.PeakLiveHeapSTW), so garbage the collector has
// not reached on a busy machine cannot be read as held (#1204).
func TestHeapOfAShortEntry(t *testing.T) {
	if raceDetector {
		t.Skip("measures the heap of the product build")
	}
	if !redacttest.InSTWProcess() {
		redacttest.RunInSTWProcess(t, "TestHeapOfAShortEntry")
		return
	}
	worst := uint64(0)
	warmShortEntryPoints()
	for _, e := range shortEntryPoints {
		for _, sh := range heapShapes {
			s := sh.text(MaxShortEntry)
			var ok bool
			b := redacttest.PeakLiveHeapSTW(func() { _, ok = e.run(s, keep, same) })
			t.Logf("%-32s %-32s %v", e.name, sh.name, b)
			worst = max(worst, b.Upper)
			if !ok && e.name != shownAndReadEntry {
				t.Errorf("%s on %s: a short entry was withheld", e.name, sh.name)
			}
			if e.name == shownAndReadEntry {
				wm := meter.New(math.MaxInt64)
				defaultRedactor.shownAndReadSpans(wm, s)
				if wm.Spent() > budgetOf(len(s)) {
					t.Errorf("%s on %s: %d steps, over the budget of %d", e.name, sh.name, wm.Spent(), budgetOf(len(s)))
				}
			}
			checkShortEntryHeap(t, e.name+" on "+sh.name, b)
		}
	}
	t.Logf("the heaviest short entry of %d bytes held at most %.3f MiB; %d slots hold at most %.1f MiB",
		MaxShortEntry, mib(worst), shortEntrySlots, mib(shortEntrySlots*ShortEntryHeap))
}

// The measurement of TestHeapOfAShortEntry catches an entry that holds more than ShortEntryHeap: one of the
// heaviest shapes (dense secrets) at each short entry point, run while a buffer is held that takes the entry 64 KiB
// past the bound by its own lower reading, is never within the bound, and checkShortEntryHeap fails it. It is over
// when a collection runs at the entry's peak while the buffer is held, which a busy machine can move (undecided in 3
// of 20 runs beside one or two busy loops per core on 2026-11-28): the upper reading, from the execution trace, is what
// makes it fail every time.
func TestHeapOfAShortEntryCatchesAHold(t *testing.T) {
	if raceDetector {
		t.Skip("measures the heap of the product build")
	}
	if !redacttest.InSTWProcess() {
		redacttest.RunInSTWProcess(t, "TestHeapOfAShortEntryCatchesAHold")
		return
	}
	warmShortEntryPoints()
	var text string
	for _, sh := range heapShapes {
		if sh.name == shapeDenseSecrets {
			text = sh.text(MaxShortEntry)
		}
	}
	if text == "" {
		t.Fatal("no shape dense secrets in heapShapes")
	}
	for _, e := range shortEntryPoints {
		heaviest := redacttest.PeakLiveHeapSTW(func() { e.run(text, keep, same) })
		if heaviest.Lower >= ShortEntryHeap {
			t.Fatalf("%s on dense secrets alone: %v, over ShortEntryHeap", e.name, heaviest)
		}
		hold := int(ShortEntryHeap-heaviest.Lower) + 64<<10 // #nosec G115 -- under ShortEntryHeap, checked above
		var sink byte
		b := redacttest.PeakLiveHeapSTW(func() {
			held := make([]byte, hold)
			for i := range held {
				held[i] = 1
			}
			e.run(text, keep, same)
			sink ^= held[len(held)-1]
			runtime.KeepAlive(held)
		})
		t.Logf("%s, heaviest alone %v; with %.3f MiB held %v (%d)", e.name, heaviest, mib(uint64(hold)), b, sink)
		if v := b.Verdict(ShortEntryHeap); v == redacttest.HeapWithin {
			t.Errorf("%s with %.3f MiB held: %v, want over or undecided (%v)", e.name, mib(uint64(hold)), v, b)
		}
		var failed errorRecorder
		if checkShortEntryHeap(&failed, e.name, b); len(failed) != 1 {
			t.Errorf("%s with %.3f MiB held: the check reported %q, want one failure", e.name, mib(uint64(hold)), failed)
		}
	}
}

// errorRecorder keeps what checkShortEntryHeap reports.
type errorRecorder []string

func (*errorRecorder) Helper() {}

func (r *errorRecorder) Errorf(format string, args ...any) {
	*r = append(*r, fmt.Sprintf(format, args...))
}
