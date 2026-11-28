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
	// Lower is the most heap a collection that ended during f found live. The readings cover the whole process's
	// heap: they bound what f itself held (f held at least this much at once) only when nothing else allocates or
	// frees during f, as in the short entry's child process.
	Lower uint64
	// Upper is the most heap the collector counted during f, as the execution trace recorded it: f held at most
	// this much at once, unless f freed heap that was live before it (see Shrank, which catches only part of that:
	// otherwise Upper can miss the freed amount). It is at least Lower, and over the
	// live peak by at most about 2% of the process's heap (stwGOGC).
	Upper uint64
	// Cycles is the number of collections that ended during f.
	Cycles uint64
	// Base is the heap before f (what the last collection before it found live), which the readings are over.
	Base uint64
	// Shrank is a collection during f that found less live than Base: something live before f was freed during
	// it, and the readings, which are of the whole process's heap, do not bound what f itself held. The verdict is
	// then never within. It is set only when the freed amount exceeds what f holds at that collection; when f holds
	// more than it freed, the collection finds at least Base, Shrank stays false and Upper can miss the freed amount.
	Shrank bool
}

// HeapVerdict is what HeapBounds say about a bound.
type HeapVerdict int

const (
	// HeapWithin is an upper reading not over the bound.
	HeapWithin HeapVerdict = iota
	// HeapOver is a lower reading over the bound: the process's heap held more, which is f's own only when nothing
	// else allocates during f.
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

// Verdict compares b with bound: over when the lower reading is over it, within when the upper reading is not and
// no collection during f found less live than before it (Shrank), undecided otherwise.
func (b HeapBounds) Verdict(bound uint64) HeapVerdict {
	switch {
	case b.Lower > bound:
		return HeapOver
	case b.Upper <= bound && !b.Shrank:
		return HeapWithin
	default:
		return HeapUndecided
	}
}

func (b HeapBounds) String() string {
	shrank := ""
	if b.Shrank {
		shrank = "; a collection found less live than before f"
	}
	return fmt.Sprintf("live at least %.3f MiB, at most %.3f MiB (%d collections over %.2f MiB%s)",
		float64(b.Lower)/(1<<20), float64(b.Upper)/(1<<20), b.Cycles, float64(b.Base)/(1<<20), shrank)
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
// one itself. When f panics or exits its goroutine (t.Fatal), the trace and that goroutine are stopped before the
// panic or the exit goes on.
//
// Every reading comes from the execution trace, between the markers logged just before f is called and just after
// it returns, so a collection after f returned is not f's; none depends on the sampler running in time for its
// soundness (#1204); a busy machine can only widen the gap between them. In that process a collection stops the
// program for its mark and its sweep, so the heap it finds live, the count the runtime sets when its mark ends and
// the last value before the collection's end, is exactly what is reachable at that instant: Base is that of the last
// collection before f, and Lower, the most any collection that ended during f found live, never counts garbage. It
// reads below the live peak for a peak no collection ran during, and a peak held without allocating gets a
// collection only from the sampler. Upper is the highest value the runtime gave its heap count from the end of the
// last collection before f until f returned. Each processor records the value its own change gave the count, after
// the change, so a value can be recorded after a later one of another processor: the count when f starts is not
// the value recorded last before it, but none recorded since that collection is above it and the count's latest
// value is recorded by the processor that set it, before or after f starts. Everything live is counted there (an
// object is live only if the last collection found it live or it was handed out since, and a span is counted whole
// when a processor takes it, which records the change before handing out any of it), so f never holds more than
// Upper. Upper counts garbage too, but a collection starts once the count passes its trigger, about 2% over what the
// last one found live: Upper is the live peak plus at most that.
//
// The readings are of the whole process's heap over Base. They bound what f holds only when f frees nothing that
// was live before it: f could drop a cache, collect and allocate as much again without the count ever passing
// Base. A collection that finds less live than Base shows that (Shrank, and the verdict is never within), but only
// when the freed amount exceeds what f holds at that collection: if f already holds more than it freed, the
// collection finds at least Base and Upper can miss the freed amount. So the caller must know that f frees nothing
// that was live before it (TestHeapOfAShortEntry says why a short entry does not), and that nothing else allocates
// or frees during f, since the readings cover the whole process. A sync.Pool is empty after the two
// collections before f.
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
	if err := peakLiveHeapSTW(f, tf); err != nil {
		panic(fmt.Sprintf("redacttest: %v", err))
	}
	b, err := traceReadings(tf)
	if err != nil {
		panic(fmt.Sprintf("redacttest: %v", err))
	}
	return b
}

// peakLiveHeapSTW runs f, tracing to w. The trace and the sampler stop on every return, also when f panics or
// exits its goroutine.
func peakLiveHeapSTW(f func(), w io.Writer) error {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(HeapProcs))
	defer debug.SetGCPercent(debug.SetGCPercent(stwGOGC))
	// The trace starts before the collections, so what starting it allocates is in the heap before f, and the
	// collections are in the trace: the last one sets Base.
	if err := trace.Start(w); err != nil {
		return fmt.Errorf("starting the execution trace: %w", err)
	}
	defer trace.Stop()
	runtime.GC()
	runtime.GC()
	var (
		stop    atomic.Bool
		started = make(chan struct{})
		done    = make(chan struct{})
	)
	go func() {
		defer close(done)
		x := []metrics.Sample{{Name: heapSample}, {Name: "/gc/heap/live:bytes"}, {Name: "/gc/cycles/total:gc-cycles"}}
		metrics.Read(x)
		cycles0 := x[2].Value.Uint64()
		var objects, lower, forcedAt uint64
		read := func() {
			metrics.Read(x)
			objects = max(objects, x[0].Value.Uint64())
			if x[2].Value.Uint64() > cycles0 {
				lower = max(lower, x[1].Value.Uint64())
			}
		}
		close(started)
		for !stop.Load() {
			if !starveSampler {
				read()
				if objects > max(lower, forcedAt)+stwForceStep {
					// A new high the last collection did not see: collect now, so that a peak f holds without
					// allocating (when no collection would start) is on Lower too. What it finds live is read from
					// the trace; these readings only decide when to collect.
					forcedAt = objects
					runtime.GC()
					read()
				}
			}
			time.Sleep(50 * time.Microsecond)
		}
	}()
	// Runs before trace.Stop (deferred first), on every return.
	defer func() {
		stop.Store(true)
		<-done
	}()
	<-started
	ctx := context.Background()
	trace.Log(ctx, traceCategory, traceStart)
	f()
	trace.Log(ctx, traceCategory, traceEnd)
	return nil
}

// heapTrace takes the events of the execution trace that PeakLiveHeapSTW reads, in the trace's order, and makes
// the readings of them.
type heapTrace struct {
	// last is the heap count recorded last; sinceGC the highest recorded since the last collection ended.
	last, sinceGC  uint64
	collected      bool
	inside, closed bool
	b              HeapBounds
	upper, lower   uint64
}

// count takes a value of the runtime's heap count.
func (h *heapTrace) count(v uint64) {
	h.last, h.sinceGC = v, max(h.sinceGC, v)
	if h.inside {
		h.upper = max(h.upper, v)
	}
}

// gcEnd takes the end of a collection. The value recorded last is what its mark found live: in a stop-the-world
// process nothing allocates between the two, and no processor can have changed the count without recording it.
func (h *heapTrace) gcEnd() {
	switch {
	case h.inside:
		h.lower = max(h.lower, h.last)
		h.b.Cycles++
		h.b.Shrank = h.b.Shrank || h.last < h.b.Base
	case !h.closed:
		h.collected, h.b.Base, h.sinceGC = true, h.last, h.last
	}
}

// fStarts takes the marker before f. The count then is at most the highest value recorded since the last
// collection ended, or a value recorded after the marker.
func (h *heapTrace) fStarts() error {
	if h.inside || h.closed {
		return nil
	}
	if !h.collected {
		return errors.New("the execution trace has no collection before f")
	}
	h.inside, h.upper = true, h.sinceGC
	return nil
}

// fReturned takes the marker after f.
func (h *heapTrace) fReturned() {
	if h.inside {
		h.inside, h.closed = false, true
	}
}

// readings returns the bounds over Base.
func (h *heapTrace) readings() (HeapBounds, error) {
	if !h.closed {
		return HeapBounds{}, errors.New("the execution trace does not hold f's start and return")
	}
	b := h.b
	b.Lower = h.lower - min(b.Base, h.lower)
	b.Upper = max(h.upper-min(b.Base, h.upper), b.Lower)
	return b, nil
}

// traceReadings reads the execution trace in r from its start: the heap count of the runtime at each change, the
// end of each collection and the markers around f (heapTrace).
func traceReadings(r io.ReadSeeker) (HeapBounds, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return HeapBounds{}, fmt.Errorf("reading the execution trace: %w", err)
	}
	tr, err := xtrace.NewReader(bufio.NewReader(r))
	if err != nil {
		return HeapBounds{}, fmt.Errorf("reading the execution trace: %w", err)
	}
	var h heapTrace
	for {
		ev, err := tr.ReadEvent()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return HeapBounds{}, fmt.Errorf("reading the execution trace: %w", err)
		}
		switch ev.Kind() {
		case xtrace.EventRangeEnd:
			if ev.Range().Name == gcRange {
				h.gcEnd()
			}
		case xtrace.EventMetric:
			if m := ev.Metric(); m.Name == heapLiveMetric {
				h.count(m.Value.Uint64())
			}
		case xtrace.EventLog:
			l := ev.Log()
			switch {
			case l.Category != traceCategory:
			case l.Message == traceStart:
				if err := h.fStarts(); err != nil {
					return HeapBounds{}, err
				}
			case l.Message == traceEnd:
				h.fReturned()
			}
		}
	}
	return h.readings()
}
