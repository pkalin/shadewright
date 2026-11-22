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

package fingerprint

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// longNote returns a note of n bytes, more than redact.MaxShortEntry for the long-note tests, with a secret the
// redactor finds and words that tell it from another note built with another tag.
func longNote(tag string, n int) string {
	head := fmt.Sprintf("Failed to pull image %q: password=%s rejected; ", "registry.example/"+tag+":1.2", "s3cr3t"+tag+"Value")
	return head + strings.Repeat("x", n-len(head))
}

// eventWarning is the type of the events of the long-note tests.
const eventWarning = "Warning"

// longNoteObservation is an event of one subject and reason with the note given.
func longNoteObservation(note string) Observation {
	return Observation{ClusterID: "c", Namespace: "web", OwnerUID: deploymentUID, Reason: reasonBackOff, Type: eventWarning,
		Note: note, RegardingKind: kindPod, RegardingName: "app-7d9f8b6c5-x2k4p", Source: SourceEvent}
}

// A note over 4 KiB is not read for the fingerprint (#1087, decision of 2026-11-22): the fixed marker stands in, so
// its fingerprint returns at once and is the same with the limit on the heap of the redactions running at once
// held whole by an analysis and with it free. The shown symptom still reads the note.
func TestLongNoteFingerprintDoesNotWait(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	o := longNoteObservation(longNote("a", 5<<10))
	idle, idleNote := Fingerprint(o), Normalize(o)
	if idleNote != placeholderLongNote {
		t.Fatalf("Normalize of a 5 KiB note = %q, want %q", idleNote[:min(len(idleNote), 80)], placeholderLongNote)
	}
	if s := Symptom(o); s == placeholderLongNote || !strings.Contains(s, "registry.example/a:1.2") || strings.Contains(s, "s3cr3taValue") {
		t.Fatalf("Symptom of a 5 KiB note on an idle analyzer = %q, want the note read and redacted", s[:min(len(s), 120)])
	}
	redacttest.HoldHeapLimit(t, func(block func()) {
		red.PiecesFitting([]redact.Piece{{Text: "pwd=abcdef", Data: true}}, func([]string) bool { block(); return true })
	}, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, ok := red.WithContext(ctx).ReplaceChecked(o.Note, rawPlaceholder, sanitize)
		return !ok
	})
	got := make(chan [2]string, 1)
	go func() { got <- [2]string{Fingerprint(o), Normalize(o)} }()
	select {
	case g := <-got:
		if g[0] != idle || g[1] != idleNote {
			t.Errorf("with the limit held: fingerprint %s, note %q; with it free %s, %q", g[0], g[1], idle, idleNote)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fingerprint of a 5 KiB note waited for the heap limit")
	}
}

// Two different notes over 4 KiB of one subject and reason have one fingerprint: their text is not part of it.
func TestLongNotesOfOneSubjectShareAFingerprint(t *testing.T) {
	a, b := longNoteObservation(longNote("a", 5<<10)), longNoteObservation(longNote("b", 5<<10+17))
	if Fingerprint(a) != Fingerprint(b) {
		t.Errorf("two 5 KiB notes of one subject and reason: fingerprints %s and %s, want one", Fingerprint(a), Fingerprint(b))
	}
	if Symptom(a) == Symptom(b) {
		t.Errorf("the shown symptoms of two different notes are equal: %q", Symptom(a))
	}
}

// The bound is redact.MaxShortEntry: a note of exactly 4 KiB is fingerprinted from its text as before, one byte
// more takes the marker, and a short note that spells the marker is read like any other.
func TestNoteAtTheShortBoundIsRead(t *testing.T) {
	a, b := longNoteObservation(longNote("a", redact.MaxShortEntry)), longNoteObservation(longNote("b", redact.MaxShortEntry))
	if n := Normalize(a); n == placeholderLongNote || !strings.Contains(n, "registry.example/a:1.2") {
		t.Errorf("Normalize of a 4 KiB note = %q, want its text", n[:min(len(n), 80)])
	}
	if Fingerprint(a) == Fingerprint(b) {
		t.Errorf("two different 4 KiB notes have one fingerprint %s", Fingerprint(a))
	}
	if n := Normalize(longNoteObservation(longNote("a", redact.MaxShortEntry+1))); n != placeholderLongNote {
		t.Errorf("Normalize of a note of 4 KiB and one byte = %q, want %q", n[:min(len(n), 80)], placeholderLongNote)
	}
	if n := Normalize(longNoteObservation(placeholderLongNote)); n == placeholderLongNote {
		t.Errorf("a short note %q normalizes to the long-note marker", placeholderLongNote)
	}
}

// shortNoteMainTable holds, for every note shortNotes generates, the fingerprint origin/main computes (commit 7ba21160). It was written once by copying this file into a checkout of that commit
// and running TestWriteShortNoteMainTable there with SHORT_NOTE_MAIN_TABLE set to the output path.
const shortNoteMainTable = "testdata/short-notes-main.jsonl"

// shortNoteRow is one line of shortNoteMainTable.
type shortNoteRow struct {
	Hash        string `json:"h"`
	Fingerprint string `json:"f"`
}

// shortNotes returns notes of 1 to redact.MaxShortEntry bytes, built from a fixed seed out of the parts the
// fingerprint rules and the redactor act on (secrets, addresses, times, durations, generated names, digests,
// UIDs, markers the input could spell, control characters and line breaks), a quarter of them exactly
// redact.MaxShortEntry bytes long.
func shortNotes() []string {
	r := rand.New(rand.NewPCG(1087, 20261122)) // #nosec G404 -- a fixed corpus, not a secret
	alnum := func(n int) string {
		const letters = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
		b := make([]byte, n)
		for i := range b {
			b[i] = letters[r.IntN(len(letters))]
		}
		return string(b)
	}
	hexs := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = "0123456789abcdef"[r.IntN(16)]
		}
		return string(b)
	}
	parts := []func() string{
		func() string { return "Back-off restarting failed container" },
		func() string { return `Failed to pull image "registry.example/app:` + fmt.Sprint(r.IntN(20)) + `"` },
		func() string { return "password=" + alnum(8+r.IntN(12)) },
		func() string { return "token: " + alnum(20+r.IntN(20)) },
		func() string { return "Authorization: Bearer " + alnum(32) },
		func() string { return "ghp_" + alnum(36) },
		func() string { return "postgres://user:" + alnum(10) + "@db.example:5432/app" },
		func() string { return "AK" + "IA" + strings.ToUpper(alnum(16)) },
		func() string { return "credentials:\n  user: " + alnum(12) + "\n" },
		func() string { return fmt.Sprintf("%d.%d.%d.%d", r.IntN(256), r.IntN(256), r.IntN(256), r.IntN(256)) },
		func() string { return "[::1]:" + fmt.Sprint(r.IntN(65536)) },
		func() string { return "2001:db8::" + hexs(4) },
		func() string { return "2026-11-22T10:" + fmt.Sprintf("%02d:%02dZ", r.IntN(60), r.IntN(60)) },
		func() string { return fmt.Sprint(1700000000 + r.IntN(1e8)) },
		func() string { return fmt.Sprintf("%dm%ds", r.IntN(60), r.IntN(60)) },
		func() string { return fmt.Sprintf("%d minutes", r.IntN(90)) },
		func() string { return "averageValue=" + fmt.Sprint(r.IntN(9000)) + "m" },
		func() string { return "pod/web-7d9f8b6c5-" + alnum(5) },
		func() string { return "replicaset web-7d9f8b6c5" },
		func() string { return "sha256:" + hexs(64) },
		func() string { return hexs(8) + "-" + hexs(4) + "-" + hexs(4) + "-" + hexs(4) + "-" + hexs(12) },
		func() string { return `secret "db-7d4b9" not found` },
		func() string { return "[REDACTED] <ip> *** <long note>" },
		func() string {
			return []string{" ", "\n", "\t", ", ", "; ", "\x01", "\u0085", `"`, "'", "=", ": "}[r.IntN(11)]
		},
		func() string { return "zażółć " + alnum(r.IntN(40)) },
	}
	out := make([]string, 0, 200)
	for i := range 200 {
		n := 1 + r.IntN(redact.MaxShortEntry)
		if i%4 == 0 {
			n = redact.MaxShortEntry
		}
		var b strings.Builder
		for b.Len() < n {
			b.WriteString(parts[r.IntN(len(parts))]())
			b.WriteString(" ")
		}
		out = append(out, b.String()[:n])
	}
	return out
}

// TestWriteShortNoteMainTable writes shortNoteMainTable; it runs only in a checkout of origin/main.
func TestWriteShortNoteMainTable(t *testing.T) {
	path := os.Getenv("SHORT_NOTE_MAIN_TABLE")
	if path == "" {
		t.Skip("SHORT_NOTE_MAIN_TABLE is not set")
	}
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, note := range shortNotes() {
		o := longNoteObservation(note)
		if err := enc.Encode(shortNoteRow{Hash: noteHash(note), Fingerprint: Fingerprint(o)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// The fingerprint of every generated note of at most 4 KiB is what origin/main computes, byte for byte: the
// long-note marker and the short entry points of the redactor change no fingerprint of such a note. Under -race
// every thirteenth note is checked (each takes about 10 ms without it and 300 ms with it), all of them without it.
func TestShortNoteFingerprintsAreMains(t *testing.T) {
	data, err := os.ReadFile(shortNoteMainTable)
	if err != nil {
		t.Fatal(err)
	}
	var rows []shortNoteRow
	for line := range strings.Lines(string(data)) {
		var row shortNoteRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	notes := shortNotes()
	if len(rows) != len(notes) {
		t.Fatalf("the table has %d rows, the generator %d notes", len(rows), len(notes))
	}
	checked, differ := 0, 0
	for i, note := range notes {
		if rows[i].Hash != noteHash(note) {
			t.Fatalf("note %d: hash %s, the table %s: the generator changed", i, noteHash(note), rows[i].Hash)
		}
		if raceDetector && i%13 != 0 {
			continue
		}
		checked++
		if f := Fingerprint(longNoteObservation(note)); f != rows[i].Fingerprint {
			differ++
			t.Errorf("note %d (%d bytes): fingerprint %s, main %s", i, len(note), f, rows[i].Fingerprint)
		}
	}
	t.Logf("%d of %d notes of 1 to %d bytes checked, %d differ from main", checked, len(notes), redact.MaxShortEntry, differ)
}
