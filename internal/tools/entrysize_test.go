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

package tools

import (
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// recordingRedactor is the package's redactor that records the size of every answer it is asked to decide
// (redact.PiecesSize), so that a test sees the entry the redaction got.
type recordingRedactor struct {
	r     *redact.Redactor
	mu    sync.Mutex
	sizes []int
}

func (c *recordingRedactor) record(ps []redact.Piece) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sizes = append(c.sizes, redact.PiecesSize(ps))
}

func (c *recordingRedactor) String(s string) string                { return c.r.String(s) }
func (c *recordingRedactor) StringChecked(s string) (string, bool) { return c.r.StringChecked(s) }
func (c *recordingRedactor) PiecesWithin(ps []redact.Piece, limit int) ([]string, bool) {
	c.record(ps)
	return c.r.PiecesWithin(ps, limit)
}
func (c *recordingRedactor) PiecesFitting(ps []redact.Piece, fits func([]string) bool) ([]string, bool) {
	c.record(ps)
	return c.r.PiecesFitting(ps, fits)
}

// recordingSet is a tool set over scene sources with a recordingRedactor.
func recordingSet(t *testing.T) (*Set, *sceneSources, *recordingRedactor) {
	t.Helper()
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingRedactor{r: red}
	src := &sceneSources{}
	set, err := New(Config{Pods: src, Events: src, Objects: src, Logs: src, Metrics: src, Delivery: src, Redactor: rec,
		Namespaces: []string{testNS}, DeliveryNamespaces: []string{fluxNS}, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	return set, src, rec
}

var allEvents = map[string]any{argNamespace: testNS, argLimit: 50}

// skipUnderRace skips a test that decides an answer of about 1.5 MiB under the race detector: the tools run again
// without it in the run of the tests with the tag redactwork.
func skipUnderRace(t *testing.T) {
	t.Helper()
	if raceDetector {
		t.Skip("decides an answer of about 1.5 MiB: checked without the race detector")
	}
}

// list_events never hands redaction an answer larger than the largest entry the heap bound was measured for
// (#1106, decision of 2026-11-22): its notes are kept newest first while they fit, each other one is shown as its
// length and nothing of it, and the answer says how many were left out. The notes kept are as many as fit: the
// first note left out would take the answer past the bound. Each note starts with a secret, which the answer never
// shows.
func TestListEventsKeepsItsAnswerWithinTheLargestEntry(t *testing.T) {
	skipUnderRace(t)
	set, src, rec := recordingSet(t)
	for _, c := range []struct {
		name          string
		n, size, kept int
	}{
		{"50 notes of 1 MiB", 50, 1 << 20, 1},
		{"50 notes of 80 KiB", 50, 80 << 10, 19},
	} {
		evs := denseEvents(c.n, c.size, "/")
		rec.sizes = nil
		r := src.run(set, diffScene{events: evs}, NameListEvents, allEvents)
		if r.Status != StatusOK || len(r.Values) != 5*c.n {
			t.Fatalf("%s: status %s, %d values: %q", c.name, r.Status, len(r.Values), r.Text[:min(len(r.Text), 200)])
		}
		if len(rec.sizes) != 1 || rec.sizes[0] > redact.LargestEntry {
			t.Errorf("%s: redaction got answers of %v bytes, want one of at most %d", c.name, rec.sizes, redact.LargestEntry)
		}
		left := c.n - c.kept
		if !strings.Contains(r.Text, notesLeftOut(left)) {
			t.Errorf("%s: the answer does not say %q", c.name, notesLeftOut(left))
		}
		if strings.Contains(r.Text, leakPassword) {
			t.Errorf("%s: the answer shows a secret of a note", c.name)
		}
		for i := range evs {
			note := r.Values[5*i+4]
			if marked := note == longNote(c.size); marked != (i >= c.kept) {
				t.Errorf("%s: note %d shown as %q; want the newest %d decided, the others their length", c.name, i,
					note, c.kept)
			}
		}
		// The first note left out would not have fit.
		if more := c.size + len(noteKey) - len(longNote(c.size)); rec.sizes[0]+more <= redact.LargestEntry {
			t.Errorf("%s: answer of %d bytes left out note %d, which fits in %d more", c.name, rec.sizes[0], c.kept, more)
		}
	}
}

// A newer note left out does not leave out an older one that fits, and a note that fits is decided like any
// other: its secret is hidden.
func TestListEventsKeepsAnOlderNoteThatFits(t *testing.T) {
	skipUnderRace(t)
	set, src := diffSet(t)
	evs := denseEvents(3, 1<<20, "/")
	evs[1].Note = "dial password=" + leakPassword
	r := src.run(set, diffScene{events: evs}, NameListEvents, allEvents)
	if r.Status != StatusOK || len(r.Values) != 15 {
		t.Fatalf("status %s, %d values", r.Status, len(r.Values))
	}
	if v := r.Values[9]; !strings.HasPrefix(v, "dial password=") || strings.Contains(v, leakPassword) {
		t.Errorf("the short note is shown as %q, want it decided", r.Values[9])
	}
	if r.Values[14] != longNote(1<<20) || !strings.Contains(r.Text, notesLeftOut(1)) {
		t.Errorf("the oldest note is shown as %q in %q, want its length and one note left out", r.Values[14], r.Text[:200])
	}
}

// The size of an answer is counted on its bytes as read, before anything is made valid UTF-8 or written in its
// form: a note of invalid bytes one byte over the bound is left out, although it would be one "?" once made valid.
func TestListEventsCountsTheBytesOfANoteAsRead(t *testing.T) {
	set, src := diffSet(t)
	evs := denseEvents(1, 64, "/")
	evs[0].Note = strings.Repeat("\xff", redact.LargestEntry+1)
	r := src.run(set, diffScene{events: evs}, NameListEvents, allEvents)
	if r.Status != StatusOK || len(r.Values) != 5 || r.Values[4] != longNote(redact.LargestEntry+1) ||
		!strings.Contains(r.Text, notesLeftOut(1)) {
		t.Errorf("status %s, values %q, text %q; want the note left out as its length", r.Status, r.Values,
			r.Text[:min(len(r.Text), 300)])
	}
}

// withheldForSize returns the count of entries redaction withheld with the reason size.
func withheldForSize(t *testing.T) float64 {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "shadewright_redaction_withheld_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == redact.ReasonSize {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatal("no series of shadewright_redaction_withheld_total with the reason size")
	return 0
}

// Any other answer larger than the largest entry is withheld whole before the redaction reads it, with the reason
// size (#1106, decision of 2026-11-22): get_resource on a pod with an argument as long as the bound. The same pod
// with an argument of 64 KiB is decided.
func TestGetResourceOverTheLargestEntryIsWithheld(t *testing.T) {
	skipUnderRace(t) // its JSON of 1.5 MiB is walked before the size is checked: 11 s under the detector
	set, src, rec := recordingSet(t)
	podWith := func(n int) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNS},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: container, Args: []string{strings.Repeat("a", n)}}}}}
	}
	args := map[string]any{argKind: kindPod, argNamespace: testNS, argName: podName}
	before := withheldForSize(t)
	checkWithheld(t, NameGetResource, src.run(set, diffScene{pod: podWith(redact.LargestEntry)}, NameGetResource, args))
	if got := withheldForSize(t) - before; got != 1 {
		t.Errorf("withheld for size counted %v times, want once", got)
	}
	if len(rec.sizes) != 0 {
		t.Errorf("redaction got answers of %v bytes, want none", rec.sizes)
	}
	if r := src.run(set, diffScene{pod: podWith(64 << 10)}, NameGetResource, args); r.Status != StatusOK {
		t.Errorf("a pod of 64 KiB: status %s, want ok", r.Status)
	}
}

// The marker of a note left out is the form the collector shows a long note in.
func TestLongNoteIsTheCollectorsForm(t *testing.T) {
	n := redact.MaxShortEntry + 1
	if got, want := longNote(n), fingerprint.Symptom(fingerprint.Observation{Note: strings.Repeat("a", n)}); got != want {
		t.Errorf("longNote %q, the collector shows %q", got, want)
	}
}

// notesWithin, on answers whose right choice is known by construction: the notes kept are, in order, those whose
// bytes past their marker still fit; the answer is at most the bound whenever the rest and the markers fit; and no
// note left out would have fit in the room left.
func TestNotesWithin(t *testing.T) {
	m := func(n int) int { return len(longNote(n)) }
	for _, c := range []struct {
		name         string
		bound, fixed int
		lens         []int
		keep         []bool
	}{
		{"all fit", 1000, 100, []int{10, 20, 30}, []bool{true, true, true}},
		{"none", 100, 100, []int{500, 600}, []bool{false, false}},
		{"the newest only", 1000 + m(900) + m(950), 0, []int{900, 950}, []bool{true, false}},
		{"an older short note after a newer long one",
			m(5000) + 200 + 4, 0, []int{5000, 200}, []bool{false, true}},
		{"exactly at the bound", 100 + 4 + m(1000), 0, []int{100, 1000}, []bool{true, false}},
		{"one byte over", 100 + 3 + m(1000), 0, []int{100, 1000}, []bool{false, false}},
	} {
		keep, left := notesWithin(c.bound, c.fixed, c.lens)
		if !slices.Equal(keep, c.keep) {
			t.Errorf("%s: kept %v, want %v", c.name, keep, c.keep)
		}
		want := 0
		for _, k := range c.keep {
			if !k {
				want++
			}
		}
		if left != want {
			t.Errorf("%s: %d left out, want %d", c.name, left, want)
		}
	}
	rng := rand.New(rand.NewPCG(1106, 1)) // #nosec G404 -- a fixed sequence of test inputs
	for range 2000 {
		lens := make([]int, rng.IntN(51))
		for i := range lens {
			lens[i] = rng.IntN(1 << uint(rng.IntN(22)))
		}
		bound, fixed := rng.IntN(4<<20), rng.IntN(4096)
		keep, left := notesWithin(bound, fixed, lens)
		size, markers, n := fixed, fixed, 0
		for i, l := range lens {
			markers += m(l)
			if keep[i] {
				size += l + len(noteKey)
			} else {
				size += m(l)
				n++
			}
		}
		if n != left {
			t.Fatalf("lens %v: %d left out, said %d", lens, n, left)
		}
		if markers <= bound && size > bound {
			t.Fatalf("lens %v bound %d fixed %d: answer of %d bytes", lens, bound, fixed, size)
		}
		for i, l := range lens {
			if !keep[i] && markers <= bound && size+l+len(noteKey)-m(l) <= bound {
				t.Fatalf("lens %v bound %d fixed %d: note %d of %d bytes left out but fits", lens, bound, fixed, i, l)
			}
		}
	}
}

// The heap of list_events on notes of the heaviest shapes stays within what the heap limit counts the largest
// entry for (redact.HeapFixed plus redact.HeapWeightPerByte for each byte of redact.LargestEntry): 50 notes of
// 1 MiB of the heap table's heaviest shape (addresses each followed by a control character), and 50 of 80 KiB of
// the shape that held the most in this answer on main (a short secret key and a one-letter value on each line),
// about 4 MB together. Without the race detector only (its shadow memory is no heap of the product).
func TestListEventsHeapStaysWithinTheLargestEntry(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector's shadow memory is no heap of the product")
	}
	set, src := diffSet(t)
	line := uint64(redact.HeapFixed + redact.HeapWeightPerByte*redact.LargestEntry)
	for _, c := range []struct {
		name, unit string
		size       int
	}{
		{"50 notes of 1 MiB, addresses and control", "ab" + "@" + "cd.ef\x01", 1 << 20},
		{"50 notes of 80 KiB, dense secrets", "pwd=a\n", 80 << 10},
	} {
		evs := denseEvents(50, c.size, c.unit)
		for i := range evs {
			evs[i].Note = strings.Repeat(c.unit, c.size/len(c.unit))
		}
		var r Result
		start := time.Now()
		peak := redacttest.PeakLiveHeap(func() { r = src.run(set, diffScene{events: evs}, NameListEvents, allEvents) })
		t.Logf("%s: peak heap %.1f MiB, status %s, %s", c.name, float64(peak)/(1<<20), r.Status, time.Since(start))
		if r.Status != StatusOK || !strings.Contains(r.Text, "are left out unread") {
			t.Errorf("%s: status %s, want an answer with notes left out", c.name, r.Status)
		}
		if peak > line {
			t.Errorf("%s: peak heap %d bytes, over %d", c.name, peak, line)
		}
	}
}
