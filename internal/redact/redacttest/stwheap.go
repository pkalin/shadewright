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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/trace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xtrace "golang.org/x/exp/trace"
)

// stwChild marks the process RunInSTWProcess starts: its collections stop the world for the mark and the sweep.
const stwChild = "SHADEWRIGHT_HEAP_STW_CHILD"

// stwGOGC is the GOGC of a measurement in a stop-the-world process: a collection starts once the heap has grown
// by at most 2% past what the last one found live, so what a reading can hold of garbage is about 2% of the
// process's heap (Base in HeapBounds), however busy the machine is. GOGC 1 halves that but ran the short entry
// heap test twelve times slower (27.6 s against 2.3 s, 2026-11-28).
const stwGOGC = 2

// MinSTWPeakDuration is how long TestPeakLiveHeapSTW holds a peak without allocating. Upper reads a peak of any
// length; Lower reads one only if a collection runs while it is held, which the sampler starts when it runs in time.
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
	// Lower is the most heap a collection that ended during f found live: f held at least this much at once.
	Lower uint64
	// Upper is the most heap the collector counted during f, as the execution trace recorded it: f held at most
	// this much at once. It is at least Lower, and over the live peak by at most about 2% of the process's heap
	// (stwGOGC).
	Upper uint64
	// Cycles is the number of collections that ran during f.
	Cycles uint64
	// Base is the heap before f (what the last collection before it found live), which the readings are over.
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

// starveSampler keeps the sampler of PeakLiveHeapSTW from reading anything or running a collection until f returns:
// the test of a machine so busy that the sampler never runs in time.
var starveSampler bool

// Markers PeakLiveHeapSTW logs in the execution trace around f.
const (
	traceCategory = "redacttest"
	traceStart    = "f starts"
	traceEnd      = "f returned"
)

// heapLiveMetric is the name the trace reader gives the runtime's count of the heap (what the last collection
// found live plus every span and large object handed out since), recorded at each change.
const heapLiveMetric = "/memory/classes/heap/objects:bytes"

// gcRange is the name the trace reader gives the range of a collection.
const gcRange = "GC concurrent mark phase"

// PeakLiveHeapSTW measures the most heap f holds at once, in a process RunInSTWProcess started (it panics in any
// other, and when the execution trace cannot be taken or read). It runs two collections, then f at GOGC stwGOGC
// and GOMAXPROCS HeapProcs with the execution trace on, while a goroutine samples every 50 microseconds the heap of
// objects and the heap the last collection found live; when the heap of objects is more than stwForceStep above
// both the most any collection found live and the reading of the last collection it started, the goroutine runs
// one itself.
//
// Neither reading depends on the sampler running in time for its soundness (#1204); a busy machine can only widen
// the gap between them. In that process a collection stops the program for its mark and its sweep, so the heap it
// finds live is exactly what is reachable at that instant (no allocation during a mark is counted live): Lower, the
// most any collection that ended during f found live, as the trace or the sampler read it, never counts garbage. It
// reads below the live peak for a peak no collection ran during, and a peak held without allocating gets a
// collection only from the sampler. Upper is the highest value the runtime gave its heap count while f ran, and the
// trace records every change of that count as it happens, so none is missed. Everything live is counted there (an
// object is live only if the last collection found it live or it was handed out since, and a span is counted whole
// when a processor takes it), so f never holds more than Upper. Upper counts garbage too, but a collection starts
// once the count passes its trigger, about 2% over what the last one found live: Upper is the live peak plus at most
// that.
func PeakLiveHeapSTW(f func()) HeapBounds {
	if !InSTWProcess() {
		panic("redacttest: PeakLiveHeapSTW runs only in the process RunInSTWProcess starts")
	}
	tf, err := os.CreateTemp("", "redacttest-stw-*.trace")
	if err != nil {
		panic(fmt.Sprintf("redacttest: a file for the execution trace: %v", err))
	}
	defer func() { _ = os.Remove(tf.Name()) }()
	defer func() { _ = tf.Close() }()
	b, err := peakLiveHeapSTW(f, tf)
	var lower, upper uint64
	if err == nil {
		lower, upper, err = traceReadings(tf)
	}
	if err != nil {
		panic(fmt.Sprintf("redacttest: %v", err))
	}
	b.Lower = max(b.Lower, lower-min(b.Base, lower))
	b.Upper = max(upper-min(b.Base, upper), b.Lower)
	return b
}

// peakLiveHeapSTW runs f, tracing to w, and returns the bounds with Upper not yet set.
func peakLiveHeapSTW(f func(), w io.Writer) (HeapBounds, error) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(HeapProcs))
	defer debug.SetGCPercent(debug.SetGCPercent(stwGOGC))
	// The trace starts before the collections, so what starting it allocates is in the heap before f.
	if err := trace.Start(w); err != nil {
		return HeapBounds{}, fmt.Errorf("starting the execution trace: %w", err)
	}
	s := []metrics.Sample{{Name: heapSample}, {Name: "/gc/heap/live:bytes"}, {Name: "/gc/cycles/total:gc-cycles"}}
	runtime.GC()
	runtime.GC()
	metrics.Read(s)
	base, cycles0 := s[1].Value.Uint64(), s[2].Value.Uint64()
	var (
		stop           atomic.Bool
		objects, lower uint64
		started        = make(chan struct{})
		done           = make(chan struct{})
		readings       = func(x []metrics.Sample) {
			metrics.Read(x)
			objects = max(objects, x[0].Value.Uint64())
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
			if !starveSampler {
				readings(x)
				if objects > max(lower, forcedAt)+stwForceStep {
					// A new high the last collection did not see: collect now, so that a peak f holds without
					// allocating (when no collection would start) is on Lower too.
					forcedAt = objects
					runtime.GC()
					readings(x)
				}
			}
			time.Sleep(50 * time.Microsecond)
		}
	}()
	<-started
	ctx := context.Background()
	trace.Log(ctx, traceCategory, traceStart)
	f()
	trace.Log(ctx, traceCategory, traceEnd)
	stop.Store(true)
	<-done
	trace.Stop()
	readings(s)
	return HeapBounds{Lower: lower - min(base, lower), Cycles: s[2].Value.Uint64() - cycles0, Base: base}, nil
}

// traceReadings reads the execution trace in r from its start. upper is the highest heap count of the runtime
// between the markers around f, counting the value the count had when f started. lower is the most heap a
// collection that ended between them found live: the count a collection sets when its mark ends (what it found
// live), which is the last before the end of its range, since in a stop-the-world process nothing allocates
// between the two. Both are absolute, not over the heap before f.
func traceReadings(r io.ReadSeeker) (lower, upper uint64, err error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, 0, fmt.Errorf("reading the execution trace: %w", err)
	}
	tr, err := xtrace.NewReader(bufio.NewReader(r))
	if err != nil {
		return 0, 0, fmt.Errorf("reading the execution trace: %w", err)
	}
	var (
		last           uint64
		counted        bool
		inside, closed bool
	)
	for {
		ev, err := tr.ReadEvent()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, 0, fmt.Errorf("reading the execution trace: %w", err)
		}
		switch ev.Kind() {
		case xtrace.EventRangeEnd:
			if inside && ev.Range().Name == gcRange {
				lower = max(lower, last)
			}
		case xtrace.EventMetric:
			if m := ev.Metric(); m.Name == heapLiveMetric {
				last, counted = m.Value.Uint64(), true
				if inside {
					upper = max(upper, last)
				}
			}
		case xtrace.EventLog:
			l := ev.Log()
			if l.Category != traceCategory {
				continue
			}
			switch {
			case l.Message == traceStart && !inside && !closed:
				if !counted {
					return 0, 0, errors.New("the execution trace has no heap count before f")
				}
				inside, upper = true, last
			case l.Message == traceEnd && inside:
				inside, closed = false, true
			}
		}
	}
	if !closed {
		return 0, 0, errors.New("the execution trace does not hold f's start and return")
	}
	return lower, upper, nil
}
