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
	"math"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
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

// The heap one short entry holds, at each short entry point and on every shape known at MaxShortEntry, is at most
// ShortEntryHeap: the figure the room next to the heap limit counts for each of its slots. No shape is withheld for
// its work: the shown short entry point withholds some shapes for what they hold (a control after a token, keys
// invalid bytes split), after its readings ran, and its work on every shape stays within the budget.
func TestHeapOfAShortEntry(t *testing.T) {
	if raceDetector {
		t.Skip("measures the heap of the product build")
	}
	worst := uint64(0)
	entries := []struct {
		name string
		run  func(s string, repl func(kind, text string) string, between func(string) string) (string, bool)
	}{{"ReplaceCheckedShort", ReplaceCheckedShort}, {shownEntry, ReplaceCheckedShortShown}, {shownAndReadEntry,
		func(s string, repl func(kind, text string) string, between func(string) string) (string, bool) {
			out, _, ok := ReplaceCheckedShortShownAndRead(s, repl, between)
			return out, ok
		}}}
	for _, e := range entries {
		for _, sh := range heapShapes {
			s := sh.text(MaxShortEntry)
			var ok bool
			peak := peakLiveHeap(func() { _, ok = e.run(s, keep, same) })
			worst = max(worst, peak)
			if !ok && e.name != shownEntry && e.name != shownAndReadEntry {
				t.Errorf("%s on %s: a short entry was withheld", e.name, sh.name)
			}
			if e.name == shownEntry || e.name == shownAndReadEntry {
				wm := meter.New(math.MaxInt64)
				defaultRedactor.shownAndReadSpans(wm, s)
				if wm.Spent() > budgetOf(len(s)) {
					t.Errorf("%s on %s: %d steps, over the budget of %d", e.name, sh.name, wm.Spent(), budgetOf(len(s)))
				}
			}
			if peak > ShortEntryHeap {
				t.Errorf("%s on %s: a short entry of %d bytes held %.2f MiB, over ShortEntryHeap (%.2f MiB)", e.name,
					sh.name, MaxShortEntry, mib(peak), mib(ShortEntryHeap))
			}
		}
	}
	t.Logf("the heaviest short entry of %d bytes held %.3f MiB; %d slots hold at most %.1f MiB", MaxShortEntry,
		mib(worst), shortEntrySlots, mib(shortEntrySlots*ShortEntryHeap))
}
