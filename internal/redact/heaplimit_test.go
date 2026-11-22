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
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// analyzerGoMemLimit is the Go memory limit both install paths give the analyzer: 90 percent of its 256 Mi.
const analyzerGoMemLimit = MeasuredGoMemoryLimit

// withHeapLimiter runs f with the process-wide limit replaced by l.
func withHeapLimiter(l *heapLimiter, f func()) {
	was := redactionHeap
	redactionHeap = l
	defer func() { redactionHeap = was }()
	f()
}

// The limit is half of the Go memory limit; without one it is fallbackHeapLimit, never off.
func TestHeapLimitIsNeverOff(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(analyzerGoMemLimit))
	l := &heapLimiter{}
	l.size()
	if want := int64(analyzerGoMemLimit / heapShareOfLimit); l.limit != want || l.sem == nil {
		t.Errorf("limit %d bytes from a Go memory limit of %d, want %d", l.limit, analyzerGoMemLimit, want)
	}
	debug.SetMemoryLimit(math.MaxInt64) // no Go memory limit
	l = &heapLimiter{}
	l.size()
	if l.limit != fallbackHeapLimit || l.sem == nil {
		t.Fatalf("limit %d bytes without a Go memory limit, want %d", l.limit, fallbackHeapLimit)
	}
	held, ok := l.acquire(context.Background(), largestObject)
	if !ok {
		t.Fatal("the first entry did not run")
	}
	defer held()
	if _, ok := l.acquire(ctxDone(), 0); ok {
		t.Error("an entry ran beside one that holds the whole limit")
	}
}

// ctxDone returns a context that has ended.
func ctxDone() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// heavyAtOnce is one entry of TestRedactionsAtOnceStayUnderTheLimit: a shape and entry point among those that
// hold the most per byte (TestHeapPerByteOfEveryShape), and a size.
type heavyAtOnce struct {
	ep, shape string
	size      int
}

// The heavy entries of TestRedactionsAtOnceStayUnderTheLimit: six of 200 KiB, eight of 512 KiB and two of the
// largest object, on the shapes and entry points that hold the most per byte.
func heavyEntries(scale int) []heavyAtOnce {
	heavy := []struct{ ep, shape string }{
		{epPiecesWithin, "letter and control"}, {epPiecesFitting, "quoted pairs"}, {epPiecesWithin, "addresses"},
		{epPiecesFitting, "query"}, {epStringTwice, "dense secrets"}, {epPiecesWithin, "letter and next line"},
	}
	var out []heavyAtOnce
	add := func(n, size int) {
		for i := range n {
			h := heavy[i%len(heavy)] // each size starts with the heaviest shape
			out = append(out, heavyAtOnce{ep: h.ep, shape: h.shape, size: size / scale})
		}
	}
	add(6, 200<<10)
	add(8, 512<<10)
	add(2, largestObject)
	return out
}

// Sixteen redactions at once of the heavy shapes and of mixed sizes, under a limit sized as the analyzer's Go
// memory limit sizes it, hold less than the limit plus the weight of the heaviest entry among them, and less than
// the larger of the limit and that entry's measured worst case (#1087, decision of 2026-11-22). Under the race
// detector the sizes and the limit are an eighth, so that the race run stays short (under 100 s on the test runner).
func TestRedactionsAtOnceStayUnderTheLimit(t *testing.T) {
	scale := 1
	if raceDetector {
		scale = 8
	}
	entries := heavyEntries(scale)
	runs := make([]func() bool, len(entries))
	heaviest := int64(0)
	for i, e := range entries {
		ep, sh := heapEntryPointNamed(t, e.ep), heapShapeNamed(t, e.shape)
		run, size := heapEntry(defaultRedactor, ep, sh, e.size)
		runs[i] = run
		heaviest = max(heaviest, heapWeight(size))
	}
	l := newHeapLimiter(int64(analyzerGoMemLimit / heapShareOfLimit / scale))
	// This test is of the heap held, not of the wait: under the race detector the heavy entries run alone for minutes.
	l.maxWait = time.Hour
	var peak uint64
	withHeapLimiter(l, func() {
		peak = peakLiveHeap(func() {
			var wg sync.WaitGroup
			for _, run := range runs {
				wg.Go(func() {
					if !run() {
						t.Error("an entry was withheld")
					}
				})
			}
			wg.Wait()
		})
	})
	bound := uint64(l.limit + heaviest) // #nosec G115 -- both positive
	// What the limit gives: the entries running together hold their weights within the limit, and an entry over
	// it runs alone, holding at most its measured worst case (heapWeightRoom of its weight past HeapFixed).
	worst := HeapFixed + (heaviest-HeapFixed)*heapWeightRoom/100
	held := uint64(max(l.limit, worst)) // #nosec G115 -- positive
	t.Logf("%d entries at once: peak %.1f MiB, limit %.1f MiB, heaviest weight %.1f MiB, its measured worst %.1f MiB",
		len(entries), mib(peak), mib(uint64(l.limit)), mib(uint64(heaviest)), mib(uint64(worst))) // #nosec G115 -- positive
	if peak >= bound {
		t.Errorf("%d redactions at once held %.1f MiB, at or above the limit plus the heaviest weight, %.1f MiB",
			len(entries), mib(peak), mib(bound))
	}
	if peak >= held {
		t.Errorf("%d redactions at once held %.1f MiB, at or above the larger of the limit and the heaviest entry's measured worst, %.1f MiB",
			len(entries), mib(peak), mib(held))
	}
}

// heapEntryPointNamed and heapShapeNamed find an entry point and a shape of the heap tests by name.
func heapEntryPointNamed(t *testing.T, name string) heapEntryPoint {
	t.Helper()
	for _, ep := range heapEntryPoints {
		if ep.name == name {
			return ep
		}
	}
	t.Fatalf("no entry point %q", name)
	return heapEntryPoint{}
}

func heapShapeNamed(t *testing.T, name string) heapShape {
	t.Helper()
	for _, sh := range heapShapes {
		if sh.name == name {
			return sh
		}
	}
	t.Fatalf("no shape %q", name)
	return heapShape{}
}

// Two entries whose weights fit the limit together run together: the second takes its share while the first
// holds its own, without waiting; a third that does not fit waits.
func TestEntriesThatFitRunTogether(t *testing.T) {
	l := newHeapLimiter(2 * heapWeight(1<<10))
	waits := promtest.ToFloat64(heapWaitsTotal)
	first, ok1 := l.acquire(context.Background(), 1<<10)
	second, ok2 := l.acquire(ctxDone(), 1<<10) // a done context: it may not wait at all
	if !ok1 || !ok2 {
		t.Fatalf("two entries that fit the limit: ran %v and %v, want both together", ok1, ok2)
	}
	if got := promtest.ToFloat64(heapWaitsTotal) - waits; got != 0 {
		t.Errorf("%v waits counted for two entries that fit, want none", got)
	}
	if _, ok := l.acquire(ctxDone(), 0); ok {
		t.Error("a third entry ran beside two that hold the whole limit")
	}
	if got := promtest.ToFloat64(heapWaitsTotal) - waits; got != 1 {
		t.Errorf("%v waits counted for the third entry, want 1", got)
	}
	first()
	second()
}

// An entry whose weight is over the whole limit takes all of it: it waits for the ones running, runs alone, and
// the ones after it wait for it; no wait is left without an end.
func TestLargeEntryRunsAlone(t *testing.T) {
	l := newHeapLimiter(heapWeight(1 << 10))
	small, _ := l.acquire(context.Background(), 0)
	large := make(chan func())
	go func() { r, _ := l.acquire(context.Background(), MaxEntry); large <- r }()
	select {
	case <-large:
		t.Fatal("the large entry ran beside a small one")
	case <-time.After(20 * time.Millisecond):
	}
	after := make(chan func())
	go func() {
		time.Sleep(5 * time.Millisecond) // after the large one in the queue
		r, _ := l.acquire(context.Background(), 0)
		after <- r
	}()
	small()
	releaseLarge := <-large
	select {
	case <-after:
		t.Fatal("a small entry ran beside the large one")
	case <-time.After(20 * time.Millisecond):
	}
	releaseLarge()
	(<-after)()
}

// A wait ends with the redactor's context (WithContext), or after the limiter's longest wait without one: the
// entry is withheld with the reason wait, nothing of it is returned, and the wait is counted.
func TestWaitEndsWithTheContext(t *testing.T) {
	l := newHeapLimiter(heapWeight(1 << 10))
	l.maxWait = 30 * time.Millisecond
	held, _ := l.acquire(context.Background(), 0)
	defer held()
	reason := func() float64 { return promtest.ToFloat64(withheldTotal.WithLabelValues(ReasonWait)) }
	withHeapLimiter(l, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		for _, c := range []struct {
			name string
			r    *Redactor
		}{{"a redactor with a context", defaultRedactor.WithContext(ctx)}, {"a redactor without one", defaultRedactor}} {
			before, waits := reason(), promtest.ToFloat64(heapWaitsTotal)
			start := time.Now()
			out, ok := c.r.StringChecked("password=hunter2")
			if ok || out != Withheld {
				t.Errorf("%s: ok %v, %q; want withheld", c.name, ok, out)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("%s: waited %v", c.name, took)
			}
			if got := reason() - before; got != 1 {
				t.Errorf("%s: %v entries counted with reason wait, want 1", c.name, got)
			}
			if got := promtest.ToFloat64(heapWaitsTotal) - waits; got != 1 {
				t.Errorf("%s: %v waits counted, want 1", c.name, got)
			}
		}
	})
}

// The share of an entry is given back when its decision panics, so the limit is whole again.
func TestShareGivenBackOnPanic(t *testing.T) {
	l := newHeapLimiter(heapWeight(1 << 10))
	withHeapLimiter(l, func() {
		func() {
			defer func() { _ = recover() }()
			meteredOn(context.Background(), 1<<10, budgetOf(1<<10), func(*meter.Meter) string { panic("boom") })
		}()
	})
	if !l.sem.TryAcquire(l.limit) {
		t.Error("the share of an entry that panicked was not given back")
	}
}

// The analyzer logs one warning when its Go memory limit is under the one the heap limit was measured for (the
// 256 Mi install), and none at or above it or without one (#1087, decision of 2026-11-22).
func TestWarningUnderTheMeasuredMemory(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(math.MaxInt64))
	for _, c := range []struct {
		name  string
		limit int64
		want  int
	}{
		{"128Mi install", 128 << 20 * 9 / 10, 1},
		{"one byte under", MeasuredGoMemoryLimit - 1, 1},
		{"256Mi install", MeasuredGoMemoryLimit, 0},
		{"512Mi install", 512 << 20 * 9 / 10, 0},
		{"no Go memory limit", math.MaxInt64, 0},
	} {
		debug.SetMemoryLimit(c.limit)
		var lines []string
		log := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
		warned := WarnIfMemoryUnderMeasured(log)
		if len(lines) != c.want || warned != (c.want == 1) {
			t.Errorf("%s: %d lines, warned %v, want %d: %v", c.name, len(lines), warned, c.want, lines)
		}
		if c.want == 1 && len(lines) == 1 && !strings.Contains(lines[0], "killed for its memory") {
			t.Errorf("%s: the warning does not name the risk: %s", c.name, lines[0])
		}
	}
}
