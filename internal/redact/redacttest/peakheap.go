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

package redacttest

import (
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync/atomic"
	"time"
)

// heapSample is the metric the probe samples: the bytes of heap objects, live or not yet swept.
const heapSample = "/memory/classes/heap/objects:bytes"

// HeapProcs is the GOMAXPROCS PeakLiveHeap measures under: CI's hosted runners have four cores, and the pacing of
// the collector, so the garbage a sample still holds, depends on how many run (#1087: ReplaceFrom on secrets held
// 34.9 MiB on 24 cores and less on four). A fixed count makes the figure the same on every machine.
const HeapProcs = 4

// PeakLiveHeap returns the most heap f held at once over the heap before it, sampled every half millisecond
// while the collector runs at GOGC 10 under a soft limit of 96 MiB and GOMAXPROCS HeapProcs, so that a sample
// stays close to what f keeps alive. The heap tests of package redact and of the read tools measure with it.
func PeakLiveHeap(f func()) uint64 {
	return peakHeap(f, 10, 96<<20, 1, 500*time.Microsecond)
}

// MinTightPeakDuration is the shortest transient peak PeakLiveHeapTight is tested to catch at its 50-microsecond
// sampling interval (TestSmallestPeakTightDoesNotUnderRead).
const MinTightPeakDuration = 3 * time.Millisecond

// PeakLiveHeapTight is PeakLiveHeap for a small bound (around 1 MiB, such as ShortEntryHeap), where GOGC 10 on
// the test binary's own baseline heap lets hundreds of KiB of unswept garbage accumulate before a collection
// finishes (#1201). It runs two collections before reading base (so sync.Pool victims from earlier calls do not
// inflate base), sets GOGC 1 and the soft memory limit to base so the collector runs continuously (capped at
// about 50% CPU, a tighter regime than production) to reclaim short-lived allocations during f, waits for the
// sampler goroutine to start, and samples every 50 microseconds (catching peaks held for at least
// MinTightPeakDuration).
//
// What one reading guarantees: a reading is never below the true live peak of f (modulo per-P mcache refill slack:
// /memory/classes/heap/objects:bytes is built from smallAllocCount, flushed at mcache.refill and releaseAll, so
// small objects allocated since the last refill can lag by tens of KiB, as in PeakLiveHeap), and may be above it
// by uncollected garbage when the collector is preempted on a slow or busy machine. Use SmallestPeakTight to
// estimate the live peak as the smallest of up to TightPeakRuns readings.
func PeakLiveHeapTight(f func()) uint64 {
	return peakHeap(f, 1, 0, 2, 50*time.Microsecond)
}

func peakHeap(f func(), gogc int, memLimit int64, preGCs int, interval time.Duration) uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(HeapProcs))
	defer debug.SetGCPercent(debug.SetGCPercent(gogc))
	if memLimit > 0 {
		defer debug.SetMemoryLimit(debug.SetMemoryLimit(memLimit))
	}
	s := []metrics.Sample{{Name: heapSample}}
	for range preGCs {
		runtime.GC()
	}
	metrics.Read(s)
	base := s[0].Value.Uint64()
	if memLimit == 0 {
		// #nosec G115 -- heap object bytes stay far below math.MaxInt64
		defer debug.SetMemoryLimit(debug.SetMemoryLimit(int64(base)))
	}
	var (
		stop    atomic.Bool
		peak    uint64
		started = make(chan struct{})
		done    = make(chan struct{})
	)
	go func() {
		defer close(done)
		x := []metrics.Sample{{Name: heapSample}}
		close(started)
		for !stop.Load() {
			metrics.Read(x)
			peak = max(peak, x[0].Value.Uint64())
			time.Sleep(interval)
		}
	}()
	<-started
	f()
	metrics.Read(s)
	stop.Store(true)
	<-done
	peak = max(peak, s[0].Value.Uint64())
	return peak - min(base, peak)
}

// PeakRuns is the most runs SmallestPeak measures.
const PeakRuns = 3

// TightPeakRuns is the most runs SmallestPeakTight measures. On a slow or shared runner, a single
// PeakLiveHeapTight reading can still carry uncollected garbage (over 1 MiB under heavy CPU contention), so
// SmallestPeakTight retries up to TightPeakRuns times and stops at the first reading not over the bound: the
// common idle case takes one run, while even a 50% per-run garbage-spike rate leaves a 0.5^15 (~0.003%) chance
// that every run is over.
const TightPeakRuns = 15

// SmallestPeak returns PeakLiveHeap of f, measured again up to PeakRuns runs in all while the reading is over
// (over reports it), and the smallest reading. A sample counts the heap objects not yet swept, and when the
// collector gets little processor time (a loaded machine, a runner with few cores) it can still count garbage at
// the peak: PiecesWithin on 1.5 MiB of "addresses and control" read 71.7 bytes per byte past HeapFixed in idle
// runs and 74.9 to 80.8 in 5 of 80 runs on two or four busy cores, garbage of at most 9.1 bytes per byte
// (#1112). The live heap of f is the same in every run, so its smallest reading is the closest to it, and a shape
// that really holds more is over in every run. f must hold nothing from one run to the next.
func SmallestPeak(f func(), over func(peak uint64) bool) uint64 {
	return smallestPeak(PeakLiveHeap, PeakRuns, f, over)
}

// SmallestPeakTight returns PeakLiveHeapTight of f, measured again up to TightPeakRuns runs in all while the
// reading is over (over reports it), and the smallest reading.
func SmallestPeakTight(f func(), over func(peak uint64) bool) uint64 {
	return smallestPeak(PeakLiveHeapTight, TightPeakRuns, f, over)
}

// SmallestPeakTightRuns is SmallestPeakTight, also returning the reading of each run it took.
func SmallestPeakTightRuns(f func(), over func(peak uint64) bool) (uint64, []uint64) {
	return smallestPeakRuns(PeakLiveHeapTight, TightPeakRuns, f, over)
}

func smallestPeak(measure func(func()) uint64, maxRuns int, f func(), over func(peak uint64) bool) uint64 {
	peak, _ := smallestPeakRuns(measure, maxRuns, f, over)
	return peak
}

func smallestPeakRuns(measure func(func()) uint64, maxRuns int, f func(), over func(peak uint64) bool) (uint64, []uint64) {
	runs := make([]uint64, 1, maxRuns)
	runs[0] = measure(f)
	peak := runs[0]
	for range maxRuns - 1 {
		if !over(peak) {
			break
		}
		r := measure(f)
		runs = append(runs, r)
		peak = min(peak, r)
	}
	return peak, runs
}
