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

	"github.com/shadewright-labs/shadewright/internal/redact/released"
)

// The work bound of String since the fourteenth decision of 2026-11-18 (#845). String applies the redactor of
// the release before (package released) as one of its readings, unchanged, and that code is not linear on
// every input. So the linearity tests count, on the same input, the work of String and, with its own counter
// (released.Counter, [WorkOfReleased]), the work that reading took inside it, and bound what the rest of String
// adds: the rule adds only linear work to what the release's reading costs. The staircases also run the
// reading by itself (released.Spans) on the same input, which must cost exactly what it cost inside String.

// ownWorkPerByte bounds, per byte, the work String adds to the reading of the release before on every input
// of the work tests. Measured on the build: at most 283.3 per byte (the extent rule on "extent backticks in
// quotes" in TestRepeatedPatternsStayLinear), 31 to 77 on the staircases.
const ownWorkPerByte = 768

// releaseReadingShare bounds String's work against the release's reading on the same text: at most this many
// times that reading's work plus ownWorkPerByte per byte.
const releaseReadingShare = 1.15

// releasedSuperlinear are the shapes on which the reading of the release before, by itself, costs more than
// maxWorkPerByte per byte or grows faster than linear, each with the issue that makes it linear. A shape that
// does so and is not listed fails its test, and so does a listed shape that no longer does, so that its row
// goes when the issue is fixed. Since #873 none is: the four staircases of a block scalar under a bare short key
// listed here before it cost 505 to 1,952 steps per byte from 64 KiB to 1 MiB.
var releasedSuperlinear = map[string]int{}

// workSample is the work of String on one input, the work of the release's reading alone on the same input,
// and the size of the input.
type workSample struct {
	total, release int64
	size           int
}

func (w workSample) own() int64 { return w.total - w.release }

func perByte(n int64, size int) float64 { return float64(n) / float64(max(size, 1)) }

// sampleWork measures String on text and the part of it the reading of the release before took.
func sampleWork(t *testing.T, text string) workSample {
	t.Helper()
	total, release := countWorkOfReleased(t, func() { String(text) })
	return workSample{total: total, release: release, size: len(text)}
}

// sampleWorkAlone is sampleWork, and also runs the reading of the release before by itself on text: it must
// cost what it cost inside String, apart from the markers String writes for its spans (released.Marker, at most
// four steps each), so that the count inside String is that reading's work and nothing else.
func sampleWorkAlone(t *testing.T, text string) workSample {
	t.Helper()
	var spans []released.Span
	alone := countWork(t, func() { spans = released.Spans(text) })
	w := sampleWork(t, text)
	if w.release < alone || w.release > alone+4*int64(len(spans)) {
		t.Errorf("the release's reading costs %d steps by itself on %d bytes (%d spans), %d inside String", alone,
			w.size, len(spans), w.release)
	}
	return w
}

// grows reports whether per byte at a larger size is more than an eighth and half a step above per byte at
// the smallest: eight or sixteen times the input then costs more than about as much again per byte.
func grows(first, later float64) bool { return later > 1.125*first+0.5 }

// checkWork checks the samples of one shape, smallest input first: what String adds to the release's reading
// is at most ownWorkPerByte per byte and linear, String is at most releaseReadingShare times that reading plus
// that budget, and the reading itself is at most maxWorkPerByte per byte and linear unless the shape is in
// releasedSuperlinear (and then it must not be).
func checkWork(t *testing.T, shape string, samples []workSample) {
	t.Helper()
	if len(samples) < 2 {
		t.Fatalf("%s: %d samples, need two sizes at least", shape, len(samples))
	}
	first := samples[0]
	releaseOver := false
	for _, w := range samples {
		t.Logf("%s at %d bytes: %.1f steps per byte, the release's reading %.1f, the rest %.1f", shape, w.size,
			perByte(w.total, w.size), perByte(w.release, w.size), perByte(w.own(), w.size))
		if w.own() > int64(ownWorkPerByte*w.size) {
			t.Errorf("%s: String adds %d steps to the release's reading on %d bytes, more than %d per byte", shape,
				w.own(), w.size, ownWorkPerByte)
		}
		if float64(w.total) > releaseReadingShare*float64(w.release)+float64(ownWorkPerByte*w.size) {
			t.Errorf("%s: %d steps on %d bytes, more than %.2f times the release's reading (%d) and %d per byte",
				shape, w.total, w.size, releaseReadingShare, w.release, ownWorkPerByte)
		}
		if grows(perByte(first.own(), first.size), perByte(w.own(), w.size)) {
			t.Errorf("%s: String adds %.1f steps per byte at %d bytes, %.1f at %d: more than linear", shape,
				perByte(first.own(), first.size), first.size, perByte(w.own(), w.size), w.size)
		}
		if w.release > int64(maxWorkPerByte*w.size) ||
			grows(perByte(first.release, first.size), perByte(w.release, w.size)) {
			releaseOver = true
		}
	}
	issue, known := releasedSuperlinear[shape]
	switch {
	case releaseOver && !known:
		t.Errorf("%s: the release's reading alone costs more than %d per byte or grows faster than linear, and "+
			"the shape is not in releasedSuperlinear", shape, maxWorkPerByte)
	case !releaseOver && known:
		t.Errorf("%s: the release's reading is now linear and within %d per byte: remove its row from "+
			"releasedSuperlinear (#%d)", shape, maxWorkPerByte, issue)
	case releaseOver:
		t.Logf("%s: the release's reading is superlinear here (#%d)", shape, issue)
	}
}

// Every row of releasedSuperlinear is a shape a work test measures (a staircase of TestCanonStairWorkIsLinear),
// so that a row cannot outlive its shape and stay unchecked.
func TestReleasedSuperlinearShapesAreMeasured(t *testing.T) {
	measured := map[string]bool{}
	for _, line := range canonStairLines {
		measured[fmt.Sprintf("stair %q", line)] = true
	}
	for shape, issue := range releasedSuperlinear {
		if !measured[shape] {
			t.Errorf("releasedSuperlinear names %s (#%d), which no work test measures", shape, issue)
		}
	}
}
