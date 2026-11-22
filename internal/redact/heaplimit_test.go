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
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
)

// analyzerGoMemLimit is the Go memory limit the chart gives the analyzer: 90 percent of its 128 Mi.
const analyzerGoMemLimit = 128 << 20 * 9 / 10

// withHeapLimiter runs f with the process-wide limit replaced by l.
func withHeapLimiter(l *heapLimiter, f func()) {
	was := redactionHeap
	redactionHeap = l
	defer func() { redactionHeap = was }()
	f()
}

// Eight redactions at once of the largest entry, on the shape that holds the most for its bytes, hold less than
// the analyzer's memory limit of 128 Mi, with the limit sized as the analyzer's Go memory limit sizes it (#1087).
func TestRedactionsAtOnceStayUnderTheAnalyzerLimit(t *testing.T) {
	if raceDetector {
		t.Skip("measures the heap of the product build")
	}
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(analyzerGoMemLimit))
	l := &heapLimiter{}
	l.size()
	if want := int64(analyzerGoMemLimit / heapShareOfLimit); l.limit != want {
		t.Fatalf("limit %d bytes from a Go memory limit of %d, want %d", l.limit, analyzerGoMemLimit, want)
	}
	// Lines of a key and a secret as the strings of a sequence, as dense as the gate lets through (FreeMatches):
	// the shape and entry point that held the most of those decided (TestHeapOfOneEntryIsBounded).
	items := strings.Split(repeatTo("password=hunter2 abc\n", largestObject), "\n")
	var peak uint64
	withHeapLimiter(l, func() {
		peak = peakLiveHeap(func() {
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					if _, ok := defaultRedactor.StringsChecked(items); !ok {
						t.Error("the strings were withheld")
					}
				})
			}
			wg.Wait()
		})
	})
	t.Logf("eight sequences of %d bytes at once: peak %.1f MiB", largestObject, mib(peak))
	if peak >= 128<<20 {
		t.Errorf("eight redactions at once held %.1f MiB, at or above the analyzer's limit of 128 MiB", mib(peak))
	}
}

// An entry whose bound is larger than the whole limit takes all of it: it waits for the ones running, runs alone,
// and the ones after it wait for it; no wait is left without an end.
func TestLargeEntryRunsAlone(t *testing.T) {
	l := newHeapLimiter(heapWeight(1 << 10))
	small := l.acquire(0)
	large := make(chan func())
	go func() { large <- l.acquire(MaxEntry) }()
	select {
	case <-large:
		t.Fatal("the large entry ran beside a small one")
	case <-time.After(20 * time.Millisecond):
	}
	after := make(chan func())
	go func() {
		time.Sleep(5 * time.Millisecond) // after the large one in the queue
		after <- l.acquire(0)
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

// Without a Go memory limit nothing waits.
func TestNoGoMemoryLimitNoWait(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(math.MaxInt64)) // no Go memory limit
	l := &heapLimiter{}
	for range 64 {
		_ = l.acquire(MaxEntry) // never given back: with no limit, nothing is held
	}
	if l.sem != nil {
		t.Errorf("a limit of %d bytes without a Go memory limit", l.limit)
	}
}
