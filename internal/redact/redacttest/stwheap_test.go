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
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
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
		{HeapBounds{Lower: 100, Upper: 200, Shrank: true}, HeapUndecided},
		{HeapBounds{Lower: 1001, Upper: 1200, Shrank: true}, HeapOver},
	} {
		if got := tc.b.Verdict(1000); got != tc.want {
			t.Errorf("%+v against 1000: %v, want %v", tc.b, got, tc.want)
		}
	}
}

// The count when f starts is not the value recorded last before it (#1204): each processor records the value its
// own change gave the runtime's heap count, after the change, so one that charged 768 KiB and was delayed records
// its value after another processor recorded the 1.25 MiB of its later change. f then holds up to 1.25 MiB in
// spans counted before it, allocating nothing that changes the count; Upper is at least that. Base and the readings
// come from the trace: Lower is what a collection during f found live, and one that found less than Base is
// Shrank.
func TestHeapTraceCountsTheHighestValueSinceTheLastCollection(t *testing.T) {
	const kib = 1 << 10
	var h heapTrace
	h.count(9000 * kib) // before the last collection
	h.gcEnd()
	h.count(2000 * kib) // what the last collection before f found live
	h.gcEnd()
	h.count(3280 * kib) // a second processor's later change, recorded first
	h.count(2768 * kib) // the first processor's earlier change, recorded late
	if err := h.fStarts(); err != nil {
		t.Fatal(err)
	}
	h.fReturned()
	h.count(5000 * kib) // after f
	h.gcEnd()
	b, err := h.readings()
	if err != nil {
		t.Fatal(err)
	}
	if b.Base != 2000*kib || b.Upper != 1280*kib || b.Lower != 0 || b.Cycles != 0 || b.Shrank {
		t.Errorf("readings %+v, want Base 2000 KiB, Upper 1280 KiB and no collection during f", b)
	}
	if v := b.Verdict(1024 * kib); v == HeapWithin {
		t.Errorf("1.25 MiB counted when f starts: %v against 1.00 MiB", v)
	}

	h = heapTrace{}
	h.count(2000 * kib)
	h.gcEnd()
	if err := h.fStarts(); err != nil {
		t.Fatal(err)
	}
	h.count(2600 * kib)
	h.count(2100 * kib) // what a collection during f found live
	h.gcEnd()
	h.count(1900 * kib)
	h.gcEnd() // found less than Base
	h.fReturned()
	if b, err = h.readings(); err != nil {
		t.Fatal(err)
	}
	if b.Base != 2000*kib || b.Upper != 600*kib || b.Lower != 100*kib || b.Cycles != 2 || !b.Shrank {
		t.Errorf("readings %+v, want Upper 600 KiB, Lower 100 KiB, two collections, shrank", b)
	}

	h = heapTrace{}
	h.count(2000 * kib)
	if err := h.fStarts(); err == nil {
		t.Error("f's start with no collection before it is read")
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

// In a stop-the-world process the readings do not depend on how busy the machine is for their soundness (#1204). The
// stress case of the sampling probe before it, 4 MiB of short-lived 4 KiB slices under a 20 MiB baseline, which read
// 1.345 MiB as the smallest of 15 readings under two heavy jobs on 2026-11-27, is within 1 MiB on its upper reading. A
// real hold over 1 MiB is never within: the upper reading comes from the execution trace, which records every value
// of the runtime's heap count. It is over when a collection runs while it is held, which an allocation of 2% of the
// heap during the hold guarantees (one buffer of 1.20 MiB). Small objects held without allocating, until f returns
// or for MinSTWPeakDuration, are seen by a collection only if the sampler starts one in time, which a busy machine
// can delay: undecided is accepted there, and a bound test fails it as well. Before the trace, the upper reading was
// sampled too, and the brief hold read within in 27 of 40 runs beside one or two busy loops per core (2026-11-28).
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

// A peak over the bound is never read within, however busy the machine is (#1204): 1.08 MiB of 64-byte nodes,
// dropped at once and followed by 512 KiB of garbage, is never within 1 MiB, and its upper reading is never below
// what it held. It is read with the sampler kept from running until f returns (the machine at its busiest; before
// the upper reading came from the execution trace, every such run read within), then beside one busy loop per
// processor of the measurement, which delay the sampler.
func TestPeakLiveHeapSTWNeverWithinOverBound(t *testing.T) {
	if RaceDetector {
		t.Skip("measures the heap of the product build")
	}
	if !InSTWProcess() {
		RunInSTWProcess(t, "TestPeakLiveHeapSTWNeverWithinOverBound")
		return
	}
	const oneMiB = 1 << 20
	var sink byte
	probe := func() {
		head := allocSmallPtrList(17695)
		for n := head; n != nil; n = n.next {
			sink ^= n.pad[0]
		}
		runtime.KeepAlive(head)
		for range 128 {
			g := make([]byte, 4<<10)
			g[0] = sink
			heapGarbageSink = g
		}
		heapGarbageSink = nil
	}
	const held = 17695 * 64
	check := func(what string, b HeapBounds) {
		t.Logf("%s: %v", what, b)
		if b.Verdict(oneMiB) == HeapWithin || b.Upper < held {
			t.Errorf("%s: %.3f MiB held, read %v: %v against 1.00 MiB", what, float64(held)/oneMiB, b, b.Verdict(oneMiB))
		}
	}
	starveSampler = true
	for run := range 3 {
		check(fmt.Sprintf("sampler starved, run %d", run+1), PeakLiveHeapSTW(probe))
	}
	starveSampler = false
	var stop atomic.Bool
	var wg sync.WaitGroup
	for range HeapProcs {
		wg.Go(func() {
			for n := 0; !stop.Load(); n++ {
				spinSink.Add(uint64(n & 1))
			}
		})
	}
	defer wg.Wait()
	defer stop.Store(true)
	for run := range 5 {
		check(fmt.Sprintf("beside busy loops, run %d", run+1), PeakLiveHeapSTW(probe))
	}
	_ = sink
}

// spinSink keeps the busy loops of TestPeakLiveHeapSTWNeverWithinOverBound from being optimized away.
var spinSink atomic.Uint64

// heldAfter keeps what a goroutine allocates after f returned (TestPeakLiveHeapSTWIgnoresWhatFollowsF) or what was
// live before f started (TestPeakLiveHeapSTWFreeingWhatWasLiveIsNotWithin).
var heldAfter []byte

// A collection after f returned is not f's (#1204): f releases a goroutine that, a millisecond later, allocates and
// keeps 2 MiB and runs a collection. f itself holds nothing, so the reading is within 1 MiB and Lower stays under
// 64 KiB. Before, the last reading after f returned took any collection newer than the ones before f into Lower and
// read over.
func TestPeakLiveHeapSTWIgnoresWhatFollowsF(t *testing.T) {
	if RaceDetector {
		t.Skip("measures the heap of the product build")
	}
	if !InSTWProcess() {
		RunInSTWProcess(t, "TestPeakLiveHeapSTWIgnoresWhatFollowsF")
		return
	}
	const oneMiB = 1 << 20
	for run := range 5 {
		release, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			<-release
			time.Sleep(time.Millisecond)
			b := make([]byte, 2*oneMiB)
			for i := range b {
				b[i] = 1
			}
			heldAfter = b
			runtime.GC()
		}()
		b := PeakLiveHeapSTW(func() { close(release) })
		<-done
		heldAfter = nil
		t.Logf("run %d: %v", run+1, b)
		if b.Verdict(oneMiB) != HeapWithin || b.Lower >= 64<<10 {
			t.Errorf("run %d: f holds nothing, read %v: %v against 1.00 MiB", run+1, b, b.Verdict(oneMiB))
		}
	}
}

// f's failure does not leave the execution trace or the sampler running (#1204): after f panics (recovered by the
// caller) and after f exits its goroutine (as t.Fatal does), the next measurement works, and the panic reaches the
// caller unchanged.
func TestPeakLiveHeapSTWAfterFFails(t *testing.T) {
	if RaceDetector {
		t.Skip("measures the heap of the product build")
	}
	if !InSTWProcess() {
		RunInSTWProcess(t, "TestPeakLiveHeapSTWAfterFFails")
		return
	}
	measures := func(what string) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("a measurement after %s: %v", what, r)
			}
		}()
		PeakLiveHeapSTW(func() {})
	}
	func() {
		defer func() {
			if r := recover(); r != "f failed" {
				t.Errorf("recovered %v, want f's panic", r)
			}
		}()
		PeakLiveHeapSTW(func() { panic("f failed") })
	}()
	measures("a panic in f")
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		PeakLiveHeapSTW(runtime.Goexit)
	}()
	<-exited
	measures("f exited its goroutine")
}

// The readings are of the process's heap over what it held before f, so they cannot tell f's own heap when f frees
// something that was live before it (#1204): f drops a cache of 2 MiB live before it, runs a collection, then
// allocates and holds 1.25 MiB, under the process's heap before f all along. A collection that finds less live
// than before f shows it, and the reading is then not within.
func TestPeakLiveHeapSTWFreeingWhatWasLiveIsNotWithin(t *testing.T) {
	if RaceDetector {
		t.Skip("measures the heap of the product build")
	}
	if !InSTWProcess() {
		RunInSTWProcess(t, "TestPeakLiveHeapSTWFreeingWhatWasLiveIsNotWithin")
		return
	}
	const oneMiB = 1 << 20
	for run := range 3 {
		heldAfter = make([]byte, 2*oneMiB)
		b := PeakLiveHeapSTW(func() {
			heldAfter = nil
			runtime.GC()
			h := make([]byte, oneMiB+oneMiB/4)
			for i := range h {
				h[i] = 1
			}
			runtime.GC()
			runtime.KeepAlive(h)
		})
		t.Logf("run %d: %v", run+1, b)
		if v := b.Verdict(oneMiB); v == HeapWithin {
			t.Errorf("run %d: 1.25 MiB held after freeing 2 MiB, read %v: within 1.00 MiB", run+1, b)
		}
	}
}
