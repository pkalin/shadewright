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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// censusNotes are the notes of the census: the recorded notes of the evaluation scenarios, the short notes and the
// generated corpus of this package, every string scalar of the detection fixtures, and every string literal of
// at least 20 bytes with a space in the Go files of the analyzer, the incidents packages, the modules, the context
// builder and the evaluation harness, without the samples of redacttest and the inputs of the differential of
// package modules (made-up secrets in every form, written to leak).
func censusNotes(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, n := range recordedNotes(t) {
		seen[n] = true
	}
	for _, n := range shortNotes() {
		seen[n] = true
	}
	for _, o := range corpus() {
		seen[o.Note] = true
	}
	err := filepath.WalkDir("../../analyzer/detection/testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
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
			if err := dec.Decode(&n); err != nil {
				if !errors.Is(err, io.EOF) {
					t.Logf("%s: %v", path, err)
				}
				return nil
			}
			collectScalars(&n, seen)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"../../analyzer", "../../incidents", "../../modules", "../../incidentcontext", "../../../test/eval"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || isDifferentialFile(path) {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil && len(s) >= 20 && strings.Contains(s, " ") {
						seen[s] = true
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range redacttest.Samples() {
		delete(seen, s.Text)
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		if s != "" && len(s) <= redact.MaxShortEntry {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// The census of the stored symptom on the notes and literals of the repository (not on production notes): how
// many notes the stored symptom changes against origin/main's reading, and how many it withholds whole. The
// reading of origin/main is normalizeWith with redact.ReplaceCheckedShort, which decides as origin/main (the
// fingerprint tables). A note "holds a marker" when origin/main's symptom holds <redacted>. It logs the counts and
// at most five examples of each kind. It fails when it reads too few notes, when a note without a marker is
// withheld, when a note without a marker other than censusChanged is changed, and when the count of notes with a
// marker that are withheld, or of those on several lines, is not the one recorded (censusWithheld,
// censusWithheldMulti). A new note in the repository that changes these counts updates them in the same change,
// with the reason.
func TestStoredSymptomCensus(t *testing.T) {
	notes := censusNotes(t)
	if len(notes) < 5000 {
		t.Fatalf("the census read %d notes, too few", len(notes))
	}
	var marker, changed, withheld, withheldMulti, benignWithheld int
	var exChanged, exWithheld, unexpected []string
	for _, s := range notes {
		o := Observation{Note: s, Reason: "BackOff"}
		main := normalizeWith(redact.ReplaceCheckedShort, o, keepAfterKey)
		now := Symptom(o)
		multi := strings.ContainsAny(s, "\n\r")
		if strings.Contains(main, placeholderRedacted) {
			marker++
			if now != main && now == placeholderRedacted {
				withheld++
				if multi {
					withheldMulti++
				}
			}
			continue
		}
		if now == main {
			continue
		}
		changed++
		if !censusChanged[censusHash(s)] {
			unexpected = append(unexpected, censusHash(s)+" "+strconv.Quote(s)+": "+strconv.Quote(main)+" -> "+strconv.Quote(now))
		}
		if now == placeholderRedacted {
			benignWithheld++
			if len(exWithheld) < 5 {
				exWithheld = append(exWithheld, strconv.Quote(s))
			}
		} else if len(exChanged) < 5 {
			exChanged = append(exChanged, strconv.Quote(s)+": "+strconv.Quote(main)+" -> "+strconv.Quote(now))
		}
	}
	t.Logf("%d notes; %d without a marker on origin/main: %d changed, %d of them withheld; %d with a marker: %d withheld, %d of them on several lines",
		len(notes), len(notes)-marker, changed, benignWithheld, marker, withheld, withheldMulti)
	for _, e := range exChanged {
		t.Logf("changed: %s", e)
	}
	for _, e := range exWithheld {
		t.Logf("withheld without a marker: %s", e)
	}
	if benignWithheld != 0 {
		t.Errorf("%d notes without a marker on origin/main are withheld, want 0", benignWithheld)
	}
	if changed > len(censusChanged) {
		t.Errorf("%d notes without a marker on origin/main are changed, want at most %d", changed, len(censusChanged))
	}
	for _, u := range unexpected {
		t.Errorf("a note without a marker on origin/main is changed and is not in censusChanged: %s", u)
	}
	if withheld != censusWithheld || withheldMulti != censusWithheldMulti {
		t.Errorf("%d notes with a marker withheld, %d of them on several lines; want %d and %d", withheld, withheldMulti,
			censusWithheld, censusWithheldMulti)
	}
}

// censusChanged holds the content hashes (censusHash) of the notes without a marker on origin/main whose stored
// symptom differs from origin/main's: "line one\n\tline\x00two\u200b  end " and "podStatus\x00event\x01", where a
// removed control no longer reads as a space ("line two" becomes "linetwo").
var censusChanged = map[string]bool{"665b789f092dcd19": true, "f693a48e94b246a7": true}

const (
	// censusWithheld is the number of notes with a marker on origin/main that the stored symptom withholds whole.
	censusWithheld = 40
	// censusWithheldMulti is how many of them are on several lines.
	censusWithheldMulti = 38
)

// censusHash is the first 16 hex digits of the SHA-256 of a note.
func censusHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// isDifferentialFile reports whether path is a file of the stored-symptom differential of package modules, whose
// inputs hold made-up secrets in every form, written to leak.
func isDifferentialFile(path string) bool {
	b := filepath.Base(path)
	return b == "symptom_differential_test.go" || b == "symptom_inputs_test.go"
}
