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
)

// holding returns a function that holds sizes[i] bytes at its i-th call (the last size after that) and counts its
// calls.
func holding(sizes ...int) (f func(), calls *int) {
	calls = new(int)
	return func() {
		b := make([]byte, sizes[min(*calls, len(sizes)-1)])
		for i := range b {
			b[i] = 1
		}
		runtime.KeepAlive(b)
		*calls++
	}, calls
}

const mb = 1 << 20

// over is the line of the tests below: more than 12 MiB.
func over(p uint64) bool { return p > 12*mb }

// A reading under the line is not taken again.
func TestSmallestPeakRunsOnceUnderTheLine(t *testing.T) {
	f, calls := holding(4 * mb)
	if p := SmallestPeak(f, over); over(p) || *calls != 1 {
		t.Errorf("peak %d bytes after %d runs, want under the line after 1", p, *calls)
	}
}

// A reading over the line that a later run does not repeat counts the smallest: a dead buffer a sample counted
// once is not the heap f holds.
func TestSmallestPeakTakesTheSmallestReading(t *testing.T) {
	f, calls := holding(24*mb, 32*mb, 4*mb)
	if p := SmallestPeak(f, over); over(p) || *calls != PeakRuns {
		t.Errorf("peak %d bytes after %d runs, want under the line after %d", p, *calls, PeakRuns)
	}
}

// An entry that holds more in every run stays over the line, and no more than PeakRuns runs are taken.
func TestSmallestPeakKeepsAHeapOverTheLineInEveryRun(t *testing.T) {
	f, calls := holding(24 * mb)
	if p := SmallestPeak(f, over); !over(p) || *calls != PeakRuns {
		t.Errorf("peak %d bytes after %d runs, want over the line after %d", p, *calls, PeakRuns)
	}
}
