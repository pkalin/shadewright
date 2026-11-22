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

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/semaphore"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// The heap of the redactions running at once (#1087, decision of 2026-11-22). The analyzer runs up to eight
// analyses at once, and each may redact an entry of up to about 1.5 MiB. Every entry point takes, before its
// decision, a share of a process-wide limit (heapWeight: HeapFixed plus HeapWeightPerByte for each byte of the
// entry) and gives it back when the decision is done, on every return and on a panic.
//
// HeapWeightPerByte is not a bound proven shape by shape: it is the worst heap per byte measured over every entry
// point and every shape known (TestHeapPerByteOfEveryShape), with a margin. A heavier shape found later (the fuzz
// target FuzzHeapWithinWeight looks for one) makes entries wait longer; it does not let the redactions running at
// once hold more than was measured for them.
//
// The limit is half of the Go memory limit of the process (GOMEMLIMIT, which both install paths set to 90 percent
// of the analyzer's memory limit: about 115 MiB of its 256 Mi), read at the first decision; the other half is for
// the rest of the process and for one entry over the limit. Without a Go memory limit the limit is
// fallbackHeapLimit, logged once: it is never off. An entry whose weight is over the whole limit takes all of it:
// it runs alone, after the ones running when it came, and none starts beside it. Waiters are served in the order
// they came, so a heavy entry is not starved by light ones. So the redactions running at once hold at most the
// limit, or one entry's weight when that is more.
//
// A wait ends when the context of the redactor ends (WithContext: the analysis's tool calls, its context build
// and its log reads pass theirs) or after maxHeapWait, whichever is first; the entry points take no context, so
// a redactor without one waits at most maxHeapWait. An entry whose wait ends is withheld (ReasonWait), like an
// entry over its budget: the caller shows the source as failed. A caller must not decide another entry inside a
// decision (a fits function of PiecesFitting that called an entry point would wait on the share its own decision
// holds).

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
	once    sync.Once
	sem     *semaphore.Weighted
	limit   int64
	maxWait time.Duration // maxHeapWait; shorter only in a test
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
		} else {
			logf.Log.WithName("redact").Info("no Go memory limit (GOMEMLIMIT) is set: the redactions running at once share a fixed heap limit",
				"limitBytes", l.limit)
		}
		l.sem, l.maxWait = semaphore.NewWeighted(l.limit), maxHeapWait
	})
}

// heapWeight is the share of the limit an entry of size bytes takes.
func heapWeight(size int) int64 { return HeapFixed + HeapWeightPerByte*int64(size) }

// acquire waits until an entry of size bytes may run, at most until ctx ends or the limiter's longest wait passes,
// and returns the function that gives its share back; ok is false, and nothing is held, when the wait ended first.
func (l *heapLimiter) acquire(ctx context.Context, size int) (release func(), ok bool) {
	l.size()
	w := min(heapWeight(size), l.limit)
	release = func() { l.sem.Release(w) }
	if l.sem.TryAcquire(w) {
		return release, true
	}
	heapWaitsTotal.Inc()
	wctx, cancel := context.WithTimeout(ctx, l.maxWait)
	defer cancel()
	if err := l.sem.Acquire(wctx, w); err != nil {
		return func() {}, false
	}
	return release, true
}

// heapWaitsTotal counts the redactions that waited for the limit.
var heapWaitsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "shadewright_redaction_heap_waits_total",
	Help: "Redactions that waited for others to finish before running, so that the heap of the redactions running " +
		"at once stays within its limit.",
})

func init() { metrics.Registry.MustRegister(heapWaitsTotal) }
