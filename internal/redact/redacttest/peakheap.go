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
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(HeapProcs))
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

// PeakRuns is the most runs SmallestPeak measures.
const PeakRuns = 3

// SmallestPeak returns PeakLiveHeap of f, measured again up to PeakRuns runs in all while the reading is over
// (over reports it), and the smallest reading. A sample counts the heap objects not yet swept, and when the
// collector gets little processor time (a loaded machine, a runner with few cores) it can still count garbage at
// the peak: PiecesWithin on 1.5 MiB of "addresses and control" read 71.7 bytes per byte past HeapFixed in idle
// runs and 74.9 to 80.8 in 5 of 80 runs on two or four busy cores, garbage of at most 9.1 bytes per byte
// (#1112). The live heap of f is the same in every run, so its smallest reading is the closest to it, and a shape
// that really holds more is over in every run. f must hold nothing from one run to the next.
func SmallestPeak(f func(), over func(peak uint64) bool) uint64 {
	peak := PeakLiveHeap(f)
	for range PeakRuns - 1 {
		if !over(peak) {
			break
		}
		peak = min(peak, PeakLiveHeap(f))
	}
	return peak
}
