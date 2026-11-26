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

// heapGarbageSink forces short-lived allocations in TestPeakLiveHeapTightExcludesGarbageUnderLargeBaseline onto
// the heap while keeping only the latest slice reachable.
var heapGarbageSink []byte

// Short-lived garbage allocated while the test binary holds a large unrelated baseline heap must not inflate
// PeakLiveHeapTight above a 1 MiB bound, whereas a real live hold above 1 MiB must still be caught.
func TestPeakLiveHeapTightExcludesGarbageUnderLargeBaseline(t *testing.T) {
	if RaceDetector {
		t.Skip("measures the heap of the product build")
	}
	ballast := make([][]byte, 2048)
	for i := range ballast {
		ballast[i] = make([]byte, 10<<10)
		ballast[i][0] = byte(i)
	}
	defer runtime.KeepAlive(ballast)

	const oneMiB = 1 << 20
	churn := func() {
		// Allocate 4 MiB of 4 KiB slices over ~30 ms (~130 MiB/s, faster than redaction's ~55 MiB/s) so the
		// collector can pace under PeakLiveHeapTight. Under GOGC 10 on a 20 MiB baseline, 2+ MiB of garbage
		// accumulates before collection; PeakLiveHeapTight keeps it well below 1 MiB.
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
	peak := SmallestPeakTight(churn, func(p uint64) bool { return p > oneMiB })
	t.Logf("churn under 20 MiB baseline: PeakLiveHeapTight=%.3f MiB", float64(peak)/mb)
	if peak > oneMiB {
		t.Errorf("4 MiB of 4 KiB garbage under a 20 MiB baseline read %.3f MiB, want <= 1.00 MiB", float64(peak)/mb)
	}

	holdOver := func() {
		b := make([]byte, oneMiB+200<<10)
		for i := range b {
			b[i] = 1
		}
		runtime.KeepAlive(b)
	}
	held := SmallestPeakTight(holdOver, func(p uint64) bool { return p > oneMiB })
	if held <= oneMiB {
		t.Errorf("holding 1.20 MiB live read %.3f MiB, want > 1.00 MiB", float64(held)/mb)
	}
}

// smallPtrNode is a 64-byte pointer-bearing heap object used to verify that PeakLiveHeapTight
// cannot under-read small scanned allocations.
type smallPtrNode struct {
	next *smallPtrNode
	pad  [56]byte
}

func allocSmallPtrList(count int) *smallPtrNode {
	var head *smallPtrNode
	for i := range count {
		n := &smallPtrNode{next: head}
		n.pad[0] = byte(i)
		head = n
	}
	return head
}

func holdSmallPtrList(count int, hold time.Duration) byte {
	head := allocSmallPtrList(count)
	start := time.Now()
	var sum byte
	for {
		for n := head; n != nil; n = n.next {
			sum ^= n.pad[0]
		}
		if hold == 0 || time.Since(start) >= hold {
			break
		}
	}
	runtime.KeepAlive(head)
	return sum
}

// PeakLiveHeapTight and SmallestPeakTight must not under-read a function that holds about 1.1 MiB of
// small pointer-containing objects live, whether kept for the whole run or only briefly at its peak.
func TestSmallestPeakTightDoesNotUnderRead(t *testing.T) {
	if RaceDetector {
		t.Skip("measures the heap of the product build")
	}
	const (
		oneMiB = 1 << 20
		// 18,048 x 64-byte pointer-bearing nodes = 1.102 MiB live.
		nodes = 18048
		reps  = 10
		brief = MinTightPeakDuration
	)
	var sink byte
	cases := []struct {
		name string
		fn   func()
	}{
		{
			name: "whole_run",
			fn: func() {
				sink ^= holdSmallPtrList(nodes, 0)
			},
		},
		{
			name: "brief_peak",
			fn: func() {
				sink ^= holdSmallPtrList(nodes, brief)
				// Force collection before returning so only an in-flight sample can observe the peak.
				runtime.GC()
				runtime.GC()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			minRep := ^uint64(0)
			for i := range reps {
				got := PeakLiveHeapTight(tc.fn)
				minRep = min(minRep, got)
				if got < oneMiB {
					t.Errorf("repetition %d/%d read %.3f MiB, want >= 1.000 MiB", i+1, reps, float64(got)/mb)
				}
			}
			var calls int
			got := SmallestPeakTight(func() {
				calls++
				tc.fn()
			}, func(p uint64) bool {
				return p >= oneMiB
			})
			t.Logf("%s: min of %d reps = %.3f MiB, SmallestPeakTight (%d runs) = %.3f MiB (sink=%d)",
				tc.name, reps, float64(minRep)/mb, calls, float64(got)/mb, sink)
			if got < oneMiB {
				t.Errorf("SmallestPeakTight read %.3f MiB after %d runs, want >= 1.000 MiB", float64(got)/mb, calls)
			}
			if calls != PeakRuns {
				t.Errorf("SmallestPeakTight ran %d times, want %d", calls, PeakRuns)
			}
		})
	}
}

// SmallestPeakTightRuns reports each run's reading so callers can notice large first-run drops.
func TestSmallestPeakTightRunsReportsEachRun(t *testing.T) {
	if RaceDetector {
		t.Skip("measures the heap of the product build")
	}
	const oneMiB = 1 << 20
	f, calls := holding(2*mb, 256<<10)
	peak, runs := SmallestPeakTightRuns(f, func(p uint64) bool { return p > oneMiB })
	if *calls != 2 || len(runs) != 2 {
		t.Fatalf("got %d calls and %d runs, want 2", *calls, len(runs))
	}
	if runs[0] <= oneMiB || runs[1] > oneMiB {
		t.Errorf("runs = [%.3f MiB, %.3f MiB], want run 1 > 1.00 MiB and run 2 <= 1.00 MiB",
			float64(runs[0])/mb, float64(runs[1])/mb)
	}
	if peak != min(runs[0], runs[1]) {
		t.Errorf("peak %.3f MiB != min(runs) %.3f MiB", float64(peak)/mb, float64(min(runs[0], runs[1]))/mb)
	}
}
