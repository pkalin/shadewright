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

import "testing"

// fuzzHeapSize is the size of the entry FuzzHeapWithinWeight builds from a fuzzed unit: large enough that the
// fixed part of an entry's weight (HeapFixed) is less than a fifth of it at HeapWeightPerByte, small enough that
// one input takes well under a second without the fuzzer's instrumentation.
const fuzzHeapSize = 192 << 10

// FuzzHeapWithinWeight looks for a shape heavier than the weight the limit on redactions at once gives an entry
// (#1087, decision of 2026-11-22): a fuzzed unit of at most 64 bytes, repeated to an entry of fuzzHeapSize
// bytes and passed to the entry point the knob picks, must hold at most heapWeight of its size at its peak. A
// shape it finds goes into heapShapes, and HeapWeightPerByte rises with the measured ratio. The seeds are a unit
// of every shape of heapShapes at every entry point. As FuzzDiffStrings does, an input whose redaction does not
// return within productDeadline fails as such, before Go's fuzzing worker kills itself at a minute; the work of
// an entry is bounded by its budget at this size, so no step cap is needed besides. It runs without the race
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
		if weight := uint64(heapWeight(size)); peak > weight { // #nosec G115 -- a weight is positive
			t.Errorf("%s on %q repeated to %d bytes held %.1f MiB, above its weight of %.1f MiB (%.1f per byte past HeapFixed)",
				ep.name, unit, size, mib(peak), mib(weight), heapRatio{size: size, peak: peak}.perByte())
		}
	})
}
