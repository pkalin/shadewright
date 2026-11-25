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
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// The heap of the redactions running at once (#1087, decisions of 2026-11-22). The analyzer runs up to eight
// analyses at once, and each may redact an entry of up to about 1.5 MiB. Every ordinary entry point takes, before
// its decision, a share of a process-wide limit (heapWeight: HeapFixed plus HeapWeightPerByte for each byte of the
// entry) and gives it back when the decision is done, on every return and on a panic (budget.go, meteredWhy).
//
// HeapWeightPerByte is not a bound proven shape by shape: the heap tests (TestHeapPerByteOfEveryShape) and the fuzz
// target FuzzHeapWithinWeight fail when an entry holds more than heapWeightRoom percent of it per byte past
// HeapFixed; the worst measured is 71.7 (PiecesWithin on an address followed by a control byte repeated, 111.6 MiB at 1.5 MiB). The
// weight of an entry depends on its size only, not on its content, so a shape heavier than HeapWeightPerByte, not
// yet found, holds more than its share until the constant is raised in a release.
//
// The limit is half of the Go memory limit of the process (GOMEMLIMIT, which both install paths set to 90 percent
// of the analyzer's memory limit: 115.2 MiB of its 256 Mi), read at the first decision. Without a Go memory limit
// the limit is fallbackHeapLimit, reported once to the fallback callback. An entry heavier than the limit
// takes all of it and runs alone after the entries already running finish. Waiters are served in
// the order they came, so a heavy entry is not starved by light ones. So the entries running at once hold at most
// the larger of the limit and one entry at the tests' line (HeapFixed plus 75 bytes per byte: 116.5 MiB at 1.5 MiB),
// as TestRedactionsAtOnceStayUnderTheLimit checks.
//
// Who waits at the limit: every caller of the ordinary entry points in the process. In the analyzer the read
// tools, the context build and the log source pass their context (WithContext), and the analysis store passes one
// of at most a try's timeout for each try of a record's check. A caller that passes none waits at most maxHeapWait.
// The collector never waits: it calls only the short entry points (shortentry.go), and a note or value longer than
// MaxShortEntry is not read (package fingerprint). An entry whose wait ends is withheld (ReasonWait), like an entry
// over its budget. A caller must not decide another entry inside a decision (a fits function of PiecesFitting that
// called an entry point would wait on the share its own decision holds).

const (
	// HeapWeightPerByte is the heap one redaction is counted for in the limit, per byte of its entry, on top of
	// HeapFixed: the worst measured heap per byte over the fixed part (TestHeapPerByteOfEveryShape) is at most
	// heapWeightRoom of it.
	HeapWeightPerByte = 100
	// heapShareOfLimit is the share of the Go memory limit the redactions running at once may hold, as a divisor.
	heapShareOfLimit = 2
	// fallbackHeapLimit is the limit when the process has no Go memory limit: a quarter of the analyzer's 256 Mi,
	// so that an install that sets none still runs the heavy entries one at a time.
	fallbackHeapLimit = 64 << 20
	// maxHeapWait bounds the wait of one redaction for the limit, with or without a context: the context build's
	// own bound (incidentcontext.DefaultBuildTimeout).
	maxHeapWait = 2 * time.Minute
)

// heapWeightRoom is the share of HeapWeightPerByte the worst measured heap per byte may take, in percent: the
// margin is the other 25 percent.
const heapWeightRoom = 75

// heapLimiter is the process-wide limit of the heap of the redactions running at once.
type heapLimiter struct {
	once       sync.Once
	sem        *semaphore.Weighted
	limit      int64
	maxWait    time.Duration // maxHeapWait; shorter only in a test
	onFallback func(limitBytes int64)
}

// redactionHeap is the limit of this process, sized at the first decision.
var redactionHeap = &heapLimiter{}

// newHeapLimiter returns a limiter of limit bytes.
func newHeapLimiter(limit int64) *heapLimiter {
	l := &heapLimiter{}
	l.once.Do(func() { l.limit, l.sem, l.maxWait = limit, semaphore.NewWeighted(limit), maxHeapWait })
	return l
}

// size sets the limit from the Go memory limit, or to fallbackHeapLimit without one, once.
func (l *heapLimiter) size() {
	l.once.Do(func() {
		l.limit = fallbackHeapLimit
		if gomem := debug.SetMemoryLimit(-1); gomem > 0 && gomem < math.MaxInt64 {
			l.limit = gomem / heapShareOfLimit
		} else if l.onFallback != nil {
			l.onFallback(l.limit)
		}
		l.sem, l.maxWait = semaphore.NewWeighted(l.limit), maxHeapWait
	})
}

// heapWeight is the share of the limit an entry of size bytes takes.
func heapWeight(size int) int64 { return HeapFixed + HeapWeightPerByte*int64(size) }

// acquire waits until an entry of size bytes may run, at most until ctx ends or the limiter's longest wait passes,
// and returns the function that gives its share back; ok is false, and nothing is held, when the wait ended first.
func (l *heapLimiter) acquire(ctx context.Context, size int, onWait func()) (release func(), ok bool) {
	l.size()
	w := min(heapWeight(size), l.limit)
	release = func() { l.sem.Release(w) }
	if l.sem.TryAcquire(w) {
		return release, true
	}
	if onWait != nil {
		onWait()
	}
	wctx, cancel := context.WithTimeout(ctx, l.maxWait)
	defer cancel()
	if err := l.sem.Acquire(wctx, w); err != nil {
		return func() {}, false
	}
	return release, true
}
