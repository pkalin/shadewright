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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// The symptom is redacted while it still has its lines, before normalize flattens it (#840, decided
// 2026-11-17). Each input holds a value that the redactor finds only by its layout: once flattened, the same
// text keeps the secret, so the test fails if the order is ever swapped.
func TestSymptomIsRedactedBeforeItIsFlattened(t *testing.T) {
	for in, secret := range map[string]string{
		"Back-off restarting\ncredentials:\n  user: admin\n":         "admin",
		"Back-off restarting\ntoken:\n  - hunter2xyzAB\n":            "hunter2xyzAB",
		"Back-off restarting\npassword: hunter2xyzAB\n  Tr0ub4d0r\n": "Tr0ub4d0r",
	} {
		flat := strings.Join(strings.Fields(in), " ")
		if !strings.Contains(redact.String(flat), secret) {
			t.Fatalf("%q: the flattened text loses %q too, so this input proves nothing about the order", in, secret)
		}
		o := Observation{Reason: "BackOff", Note: in}
		for name, got := range map[string]string{"Normalize": Normalize(o), "Symptom": Symptom(o), "Value": Value(in)} {
			if strings.Contains(got, secret) {
				t.Errorf("%s(%q) = %q, leaks %q", name, in, got, secret)
			}
		}
	}
}

// fingerprintMainTable holds, for every string of the recorded scenarios (test/eval/testdata/scenarios) and
// every sample of package redacttest, the fingerprint and the symptom main computes for an observation with
// that note (commit 0dcfec7a). It was written once by running TestWriteFingerprintMainTable in a checkout of
// that commit with FINGERPRINT_MAIN_TABLE set to the output path; regenerate it only from that commit.
const fingerprintMainTable = "testdata/fingerprint-main.jsonl"

// fingerprintMainRow is one line of fingerprintMainTable: the first 8 bytes of the SHA-256 of the note, in
// hex, its fingerprint and its symptom.
type fingerprintMainRow struct {
	Hash        string `json:"h"`
	Fingerprint string `json:"f"`
	Symptom     string `json:"s"`
}

// recordedNotes returns every string scalar of the scenario files and the text of every redacttest sample,
// without duplicates, in a stable order.
func recordedNotes(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	root := filepath.Join("..", "..", "..", "test", "eval", "testdata", "scenarios")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		f, err := os.Open(path) //nolint:gosec // a path under the repository's own test data
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		dec := yaml.NewDecoder(f)
		for {
			var n yaml.Node
			if err := dec.Decode(&n); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return err
			}
			collectScalars(&n, seen)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range redacttest.Samples() {
		seen[s.Text] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		if s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// collectScalars adds the value of every string scalar under n to seen.
func collectScalars(n *yaml.Node, seen map[string]bool) {
	if n.Kind == yaml.ScalarNode && (n.Tag == "!!str" || n.Tag == "") {
		seen[n.Value] = true
	}
	for _, c := range n.Content {
		collectScalars(c, seen)
	}
}

// recordedObservation is the observation the table is computed for: the note and fixed other fields.
func recordedObservation(note string) Observation {
	return Observation{ClusterID: "c", Namespace: "demo", ResourceUID: "u", Reason: "Failed", Type: "Warning", Note: note}
}

// noteHash is the hash of a note that fingerprintMainTable holds.
func noteHash(note string) string {
	sum := sha256.Sum256([]byte(note))
	return hex.EncodeToString(sum[:8])
}

// TestWriteFingerprintMainTable writes fingerprintMainTable from the code it runs with, when
// FINGERPRINT_MAIN_TABLE names the output path; otherwise it is skipped.
func TestWriteFingerprintMainTable(t *testing.T) {
	path := os.Getenv("FINGERPRINT_MAIN_TABLE")
	if path == "" {
		t.Skip("FINGERPRINT_MAIN_TABLE is not set")
	}
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, note := range recordedNotes(t) {
		o := recordedObservation(note)
		if err := enc.Encode(fingerprintMainRow{Hash: noteHash(note), Fingerprint: Fingerprint(o), Symptom: Symptom(o)}); err != nil {
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

// The fingerprint and the symptom of every recorded note are what main computes: the redaction rules of #840
// change no stored fingerprint, so no Incident splits or merges on upgrade (design 3). A note main had and
// the current data no longer has fails too, so the table cannot shrink unnoticed.
func TestRecordedFingerprintsAreUnchanged(t *testing.T) {
	notes := map[string]string{}
	for _, n := range recordedNotes(t) {
		notes[noteHash(n)] = n
	}
	f, err := os.Open(fingerprintMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	rows := 0
	for sc.Scan() {
		var row fingerprintMainRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("line %d: %v", rows+1, err)
		}
		rows++
		note, ok := notes[row.Hash]
		if !ok {
			t.Errorf("%s line %d: no recorded note has hash %s any more", fingerprintMainTable, rows, row.Hash)
			continue
		}
		o := recordedObservation(note)
		if got := Fingerprint(o); got != row.Fingerprint {
			t.Errorf("fingerprint of %q changed: %s, main %s", note, got, row.Fingerprint)
		}
		if got := Symptom(o); got != row.Symptom {
			t.Errorf("symptom of %q changed:\n now:  %q\n main: %q", note, got, row.Symptom)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if rows < 100 {
		t.Fatalf("%s has %d rows: too few to say anything", fingerprintMainTable, rows)
	}
}
