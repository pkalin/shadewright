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
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stwChild marks the process RunInSTWProcess starts: its collections stop the world for the mark and the sweep.
const stwChild = "SHADEWRIGHT_HEAP_STW_CHILD"

// stwGOGC is the GOGC of a measurement in a stop-the-world process: a collection starts once the heap has grown
// by at most 2% past what the last one found live, so what a reading can hold of garbage is about 2% of the
// process's heap (Base in HeapBounds), however busy the machine is. GOGC 1 halves that but ran the short entry
// heap test twelve times slower (27.6 s against 2.3 s, 2026-11-28).
const stwGOGC = 2

// MinSTWPeakDuration is the shortest peak PeakLiveHeapSTW is tested to read on both readings
// (TestPeakLiveHeapSTW).
const MinSTWPeakDuration = 3 * time.Millisecond

// stwForceStep is how far the heap of objects may rise over what collections found live before PeakLiveHeapSTW
// runs one: about 32 collections for a peak of 1 MiB.
const stwForceStep = 32 << 10

// InSTWProcess reports whether this is the process RunInSTWProcess started.
func InSTWProcess() bool { return os.Getenv(stwChild) == "1" }

// RunInSTWProcess runs the test of this binary named name (a top-level test, matched whole) again in a fresh
// process with GODEBUG gcstoptheworld=2, where each collection marks and sweeps with the program stopped, and
// fails t when that run fails. Its output is logged on t. The runtime reads gcstoptheworld only at start, hence
// the new process; it also holds no heap or goroutines of other tests. The test calls it when InSTWProcess is
// false and returns; in the started process it measures with PeakLiveHeapSTW.
func RunInSTWProcess(t testing.TB, name string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// #nosec G204 -- reruns this test binary
	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+name+"$", "-test.count=1", "-test.v")
	cmd.Env = append(withoutGODEBUG(os.Environ()), stwChild+"=1", "GODEBUG=gcstoptheworld=2")
	out, err := cmd.CombinedOutput()
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		t.Log(line)
	}
	if err != nil {
		t.Fatalf("%s in a stop-the-world process: %v", name, err)
	}
	if !strings.Contains(string(out), "--- PASS: "+name) {
		t.Fatalf("%s did not run in the stop-the-world process", name)
	}
}

func withoutGODEBUG(env []string) []string {
	kept := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GODEBUG=") && !strings.HasPrefix(kv, stwChild+"=") {
			kept = append(kept, kv)
		}
	}
	return kept
}

// HeapBounds are the two readings of PeakLiveHeapSTW, in bytes over the heap before f.
type HeapBounds struct {
	// Lower is the most heap a collection during f found live: f held at least this much at once.
	Lower uint64
	// Upper is the most heap of objects a sample read during f (and at least Lower), live or garbage of less
	// than about 2% of Base: f held at most this much at once, for a peak the sampler caught
	// (MinSTWPeakDuration) and up to the small objects each processor allocated since it last took a span.
	Upper uint64
	// Cycles is the number of collections that ran during f.
	Cycles uint64
	// Base is the heap before f, which the readings are over.
	Base uint64
}

// HeapVerdict is what HeapBounds say about a bound.
type HeapVerdict int

const (
	// HeapWithin is an upper reading not over the bound.
	HeapWithin HeapVerdict = iota
	// HeapOver is a lower reading over the bound: f really held more.
	HeapOver
	// HeapUndecided is a lower reading not over the bound and an upper one over it: f's peak is within the
	// readings' gap of the bound.
	HeapUndecided
)

func (v HeapVerdict) String() string {
	switch v {
	case HeapWithin:
		return "within"
	case HeapOver:
		return "over"
	default:
		return "undecided"
	}
}

// Verdict compares b with bound: over when the lower reading is over it, within when the upper reading is not,
// undecided otherwise.
func (b HeapBounds) Verdict(bound uint64) HeapVerdict {
	switch {
	case b.Lower > bound:
		return HeapOver
	case b.Upper <= bound:
		return HeapWithin
	default:
		return HeapUndecided
	}
}

func (b HeapBounds) String() string {
	return fmt.Sprintf("live at least %.3f MiB, at most %.3f MiB (%d collections over %.2f MiB)",
		float64(b.Lower)/(1<<20), float64(b.Upper)/(1<<20), b.Cycles, float64(b.Base)/(1<<20))
}

// PeakLiveHeapSTW measures the most heap f holds at once, in a process RunInSTWProcess started (it panics in any
// other). It runs two collections, then f at GOGC stwGOGC and GOMAXPROCS HeapProcs while a goroutine samples every
// 50 microseconds the heap of objects and the heap the last collection found live; when the heap of objects is
// more than stwForceStep above both the most any collection found live and the reading of the last collection it
// started, the goroutine runs one itself.
//
// In that process a collection stops the program for its mark and its sweep, so the heap it finds live is exactly
// what is reachable at that instant (no allocation during a mark is counted live), and it starts as soon as the
// heap has grown 2% past that: the collector cannot fall behind f however little processor time the machine
// gives it, it only makes f slower. Hence the two readings: Lower never counts garbage, and Upper counts at most
// the garbage of 2% of the heap (#1204). Neither depends on how busy the machine is. Upper reads below the live
// peak for a peak shorter than the 50-microsecond sampling catches (MinSTWPeakDuration), and by the small objects
// allocated from the span each processor has cached, which the heap of objects counts only when the processor
// takes its next span (tens of KiB, as in PeakLiveHeap; Upper is raised to Lower, which counts them). Lower reads
// below it for a peak no collection ran during.
func PeakLiveHeapSTW(f func()) HeapBounds {
	if !InSTWProcess() {
		panic("redacttest: PeakLiveHeapSTW runs only in the process RunInSTWProcess starts")
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(HeapProcs))
	defer debug.SetGCPercent(debug.SetGCPercent(stwGOGC))
	s := []metrics.Sample{{Name: heapSample}, {Name: "/gc/heap/live:bytes"}, {Name: "/gc/cycles/total:gc-cycles"}}
	runtime.GC()
	runtime.GC()
	metrics.Read(s)
	base, cycles0 := s[0].Value.Uint64(), s[2].Value.Uint64()
	var (
		stop         atomic.Bool
		upper, lower uint64
		started      = make(chan struct{})
		done         = make(chan struct{})
		readings     = func(x []metrics.Sample) {
			metrics.Read(x)
			upper = max(upper, x[0].Value.Uint64())
			if x[2].Value.Uint64() > cycles0 {
				lower = max(lower, x[1].Value.Uint64())
			}
		}
	)
	go func() {
		defer close(done)
		x := []metrics.Sample{{Name: heapSample}, {Name: "/gc/heap/live:bytes"}, {Name: "/gc/cycles/total:gc-cycles"}}
		close(started)
		forcedAt := uint64(0)
		for !stop.Load() {
			readings(x)
			if upper > max(lower, forcedAt)+stwForceStep {
				// A new high the last collection did not see: collect now, so that a peak f holds without
				// allocating (when no collection would start) is on Lower too.
				forcedAt = upper
				runtime.GC()
				readings(x)
			}
			time.Sleep(50 * time.Microsecond)
		}
	}()
	<-started
	f()
	stop.Store(true)
	<-done
	readings(s)
	return HeapBounds{Lower: lower - min(base, lower), Upper: max(upper, lower) - min(base, max(upper, lower)), Cycles: s[2].Value.Uint64() - cycles0,
		Base: base}
}
