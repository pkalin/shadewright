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
	"fmt"
	"testing"
)

// fuzzHeapSize is the size of the entry FuzzHeapWithinWeight builds from a fuzzed unit: large enough that the
// fixed part of an entry's weight (HeapFixed) is less than a fifth of it at HeapWeightPerByte, small enough that
// one input takes well under a second without the fuzzer's instrumentation.
const fuzzHeapSize = 192 << 10

// FuzzHeapWithinWeight looks for a shape heavier than the line of the table test (#1087, decisions of 2026-11-22):
// a fuzzed unit of at most 64 bytes, repeated to an entry of fuzzHeapSize bytes and passed to the entry point the
// knob picks, must not hold more than heapWeightRoom percent of HeapWeightPerByte per byte past HeapFixed at that
// size (heapRatio.overTestLine, the line TestHeapPerByteOfEveryShape fails on). At this size the fixed 4 MiB is
// subtracted from the peak, so a shape with little fixed heap passes up to about 96 bytes per byte; the table
// test at 512 KiB and 1.5 MiB is the one that holds the line of 75. A shape it finds goes into heapShapes, and
// HeapWeightPerByte rises with the measured ratio. The seeds are a unit of every shape of heapShapes, each
// seeded at one entry point (shape i at entry point i modulo the count). As FuzzDiffStrings does, an input whose
// redaction does not return within productDeadline fails as such, before Go's fuzzing worker kills itself at a
// minute; the work of an entry is bounded by its budget at this size, so no step cap is needed besides. It runs without the race
// detector only (the detector's shadow memory is no heap of the product).
func FuzzHeapWithinWeight(f *testing.F) {
	for i, sh := range heapShapes {
		f.Add(sh.text(24), uint8(i)) // #nosec G115 -- fewer than 256 shapes
	}
	f.Fuzz(func(t *testing.T, unit string, at uint8) {
		if raceDetector {
			t.Skip("measures the heap of the product build")
		}
		if unit == "" || len(unit) > 64 {
			t.Skip("a unit of 1 to 64 bytes")
		}
		ep := heapEntryPoints[int(at)%len(heapEntryPoints)]
		run, size := heapEntry(defaultRedactor, ep, heapShape{text: func(n int) string { return repeatTo(unit, n) }}, fuzzHeapSize)
		var peak uint64
		productsRunning.Add(1)
		if !returnsWithin(productDeadline, func() {
			defer productsRunning.Done()
			peak = peakLiveHeap(func() { run() })
		}) {
			t.Fatalf("%s did not return within %v on %q repeated to %d bytes", ep.name, productDeadline, unit, size)
		}
		if err := fuzzHeapVerdict(size, peak); err != nil {
			t.Errorf("%s on %q repeated: %v", ep.name, unit, err)
		}
	})
}

// fuzzHeapVerdict is the check of FuzzHeapWithinWeight on an entry of size bytes that held peak: an error when the
// entry is over the line of the table test (heapRatio.overTestLine).
func fuzzHeapVerdict(size int, peak uint64) error {
	h := heapRatio{size: size, peak: peak}
	if h.overTestLine() {
		return fmt.Errorf("an entry of %d bytes held %.1f MiB, %.1f bytes per byte past HeapFixed: over %d percent of HeapWeightPerByte (%d)",
			size, mib(peak), h.perByte(), heapWeightRoom, HeapWeightPerByte)
	}
	return nil
}

// FuzzHeapWithinWeight fails at its own size on the line of the table test: an entry of fuzzHeapSize bytes that
// held 76 or 90 bytes per byte past HeapFixed fails it, though both are under the entry's weight (100 per byte),
// and one that held 74 passes.
func TestFuzzHeapFailsOnTheTableLine(t *testing.T) {
	peak := func(perByte int) uint64 { return uint64(HeapFixed + perByte*fuzzHeapSize) } // #nosec G115 -- positive
	for _, perByte := range []int{76, 90} {
		if fuzzHeapVerdict(fuzzHeapSize, peak(perByte)) == nil {
			t.Errorf("an entry of %d bytes at %d bytes per byte past HeapFixed passes the fuzz target", fuzzHeapSize, perByte)
		}
	}
	if err := fuzzHeapVerdict(fuzzHeapSize, peak(74)); err != nil {
		t.Errorf("an entry at 74 bytes per byte fails the fuzz target: %v", err)
	}
}
