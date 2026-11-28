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
	"testing"
	"time"
)

// The verdict of two readings against a bound: over only on the lower reading, within only on the upper one.
func TestHeapBoundsVerdict(t *testing.T) {
	for _, tc := range []struct {
		b    HeapBounds
		want HeapVerdict
	}{
		{HeapBounds{Lower: 100, Upper: 200}, HeapWithin},
		{HeapBounds{Lower: 100, Upper: 1000}, HeapWithin},
		{HeapBounds{Lower: 900, Upper: 1001}, HeapUndecided},
		{HeapBounds{Lower: 1000, Upper: 1200}, HeapUndecided},
		{HeapBounds{Lower: 1001, Upper: 1200}, HeapOver},
	} {
		if got := tc.b.Verdict(1000); got != tc.want {
			t.Errorf("%+v against 1000: %v, want %v", tc.b, got, tc.want)
		}
	}
}

// PeakLiveHeapSTW outside the process RunInSTWProcess starts would read a concurrent collector: it refuses.
func TestPeakLiveHeapSTWRefusesAnOrdinaryProcess(t *testing.T) {
	if InSTWProcess() {
		t.Skip("runs in the ordinary process")
	}
	defer func() {
		if recover() == nil {
			t.Error("PeakLiveHeapSTW measured in an ordinary process")
		}
	}()
	PeakLiveHeapSTW(func() {})
}

// In a stop-the-world process the readings do not depend on how busy the machine is (#1204). The stress case of
// the sampling probe before it, 4 MiB of short-lived 4 KiB slices under a 20 MiB baseline, which read 1.345 MiB as
// the smallest of 15 readings under two heavy jobs on 2026-11-27, is within 1 MiB on its upper reading. A real hold
// over 1 MiB is never within. It is over when a collection runs while it is held, which an allocation of 2% of the
// heap during the hold guarantees (one buffer of 1.20 MiB). Small objects held without allocating, until f returns
// or for MinSTWPeakDuration, are seen by a collection only if the sampler starts one in time, which a busy machine
// can delay (a 3 ms hold read undecided in 9 of 30 runs beside two loads of one busy loop per core, 2026-11-28): undecided is
// accepted there, and a bound test fails it as well.
func TestPeakLiveHeapSTW(t *testing.T) {
	if RaceDetector {
		t.Skip("measures the heap of the product build")
	}
	if !InSTWProcess() {
		RunInSTWProcess(t, "TestPeakLiveHeapSTW")
		return
	}
	const oneMiB = 1 << 20
	ballast := make([][]byte, 2048)
	for i := range ballast {
		ballast[i] = make([]byte, 10<<10)
		ballast[i][0] = byte(i)
	}
	defer runtime.KeepAlive(ballast)
	churn := func() {
		var acc byte
		for range 1024 {
			b := make([]byte, 4<<10)
			for range 16 {
				for i := range b {
					acc ^= b[i] + byte(i)
				}
			}
			b[0] = acc
			heapGarbageSink = b
		}
		heapGarbageSink = nil
	}
	var sink byte
	cases := []struct {
		name string
		f    func()
		want HeapVerdict
		// orUndecided accepts undecided as well.
		orUndecided bool
	}{
		{"churn under a 20 MiB baseline", churn, HeapWithin, false},
		{"one buffer of 1.20 MiB", func() {
			b := make([]byte, oneMiB+200<<10)
			for i := range b {
				b[i] = 1
			}
			runtime.KeepAlive(b)
		}, HeapOver, false},
		{"1.10 MiB of small objects up to the end", func() { sink ^= holdSmallPtrList(18048, 0) }, HeapOver, true},
		{"1.10 MiB of small objects held briefly", func() {
			sink ^= holdSmallPtrList(18048, MinSTWPeakDuration)
			runtime.GC()
		}, HeapOver, true},
	}
	for _, tc := range cases {
		for run := range 3 {
			start := time.Now()
			b := PeakLiveHeapSTW(tc.f)
			t.Logf("%-40s run %d: %v in %v", tc.name, run+1, b, time.Since(start).Round(time.Millisecond))
			if got := b.Verdict(oneMiB); got != tc.want && (!tc.orUndecided || got != HeapUndecided) {
				t.Errorf("%s run %d: %v against 1.00 MiB, want %v (%v)", tc.name, run+1, got, tc.want, b)
			}
		}
	}
	_ = sink
}
