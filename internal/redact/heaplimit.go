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

	"golang.org/x/sync/semaphore"
)

// The heap of the redactions running at once (#1087). The bound of one entry (HeapPerByte, HeapFixed) holds per
// entry; the analyzer runs up to eight analyses at once, and two dense entries side by side held more than its
// memory limit. So every entry point takes, before its decision, a share of a process-wide limit as large as the
// bound of its entry (heapWeight), and gives it back when the decision is done: the bounds of the redactions running
// at once never add up to more than the limit.
//
// The limit is half of the Go memory limit of the process (GOMEMLIMIT, which the chart sets to 90 percent of the
// container's memory limit: about 57 MiB of the analyzer's 128 Mi), read when the first entry is decided; the other
// half is for the rest of the process. Without a Go memory limit nothing is limited. An entry whose bound is larger
// than the whole limit takes all of it: it runs alone, after the ones running when it came, and none starts beside
// it. Waiters are served in the order they came, so a large entry is not starved by small ones.
//
// A waiting redaction has no context to stop it: the entry points take none. Its wait is bounded, since each
// redaction running holds the limit only for its decision, and the work budget (WorkPerByte) bounds that decision;
// the wait counts against the deadline of the analysis that asked for it as any slow step does. A caller must not
// decide another entry inside a decision (a fits function of PiecesFitting that called an entry point would wait on
// the share its own decision holds).

// heapShareOfLimit is the share of the Go memory limit the redactions running at once may hold, as a divisor.
const heapShareOfLimit = 2

// heapLimiter is the process-wide limit of the heap of the redactions running at once.
type heapLimiter struct {
	once  sync.Once
	sem   *semaphore.Weighted // nil when nothing is limited
	limit int64
}

// redactionHeap is the limit of this process, sized at the first decision.
var redactionHeap = &heapLimiter{}

// newHeapLimiter returns a limiter of limit bytes, none when limit is not positive.
func newHeapLimiter(limit int64) *heapLimiter {
	l := &heapLimiter{limit: limit}
	l.once.Do(func() {
		if limit > 0 {
			l.sem = semaphore.NewWeighted(limit)
		}
	})
	return l
}

// size sets the limit from the Go memory limit, once.
func (l *heapLimiter) size() {
	l.once.Do(func() {
		if gomem := debug.SetMemoryLimit(-1); gomem > 0 && gomem < math.MaxInt64 {
			l.limit = gomem / heapShareOfLimit
			l.sem = semaphore.NewWeighted(l.limit)
		}
	})
}

// heapWeight is the share of the limit an entry of size bytes takes: the bound of its heap.
func heapWeight(size int) int64 { return HeapFixed + HeapPerByte*int64(size) }

// acquire waits until an entry of size bytes may run and returns the function that gives its share back.
func (l *heapLimiter) acquire(size int) (release func()) {
	l.size()
	if l.sem == nil {
		return func() {}
	}
	w := min(heapWeight(size), l.limit)
	// The background context: a wait ends when a running decision gives its share back (see above).
	if err := l.sem.Acquire(context.Background(), w); err != nil {
		return func() {} // only a done context fails, and the background context is never done
	}
	return func() { l.sem.Release(w) }
}
