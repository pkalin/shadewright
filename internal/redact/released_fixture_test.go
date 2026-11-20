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
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/released"
)

// releasedTable holds, for every input of the corpora of this package (canonCorpus, blockCorpus, formCorpus,
// ordinaryCorpus, blanksCorpus, extentCorpus and the inputs of the stored tables of main's results), a hash of
// the input and a hash of what String of the release before (origin/main at 0701cd3a) returned for it. It was
// written once by running TestWriteReleasedTable in a checkout of that commit with RELEASED_TABLE set to the
// output path; regenerate it only from that commit. TestReleasedReproducesTheReleaseBefore checks the kept
// reading (package released) against it, so the kept code cannot drift without failing.
const releasedTable = "testdata/released-main.tsv"

// releasedInputs returns every input releasedTable is written from.
func releasedInputs(t *testing.T) []string {
	t.Helper()
	var in []string
	for _, c := range canonCorpus() {
		in = append(in, c.in)
	}
	for _, c := range blockCorpus(blockSecretKeys, true) {
		in = append(in, c.in)
	}
	for _, c := range formCorpus() {
		in = append(in, c.in)
	}
	in = append(in, ordinaryCorpus()...)
	in = append(in, blanksCorpus(3000)...)
	in = append(in, extentCorpus(4000)...)
	for _, path := range []string{
		"testdata/main-redaction.jsonl", "testdata/blanks-main.jsonl", "testdata/comma-fields-main.jsonl",
		"testdata/extent-main.jsonl",
	} {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var row struct {
				In string `json:"in"`
			}
			if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			in = append(in, row.In)
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	return in
}

// releasedHash is the first 8 bytes of the SHA-256 of s, in hex.
func releasedHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// TestWriteReleasedTable writes releasedTable from String, when RELEASED_TABLE names the output path; otherwise
// it is skipped. Run it only in a checkout of the commit releasedTable names.
func TestWriteReleasedTable(t *testing.T) {
	path := os.Getenv("RELEASED_TABLE")
	if path == "" {
		t.Skip("RELEASED_TABLE is not set")
	}
	seen := map[string]bool{}
	var b strings.Builder
	b.WriteString("# String of origin/main at 0701cd3a on every input of the corpora (#845, thirteenth decision of 2026-11-18).\n")
	b.WriteString("# input (first 16 hex digits of its SHA-256)\toutput (the same of what String returned)\n")
	for _, in := range releasedInputs(t) {
		h := releasedHash(in)
		if seen[h] {
			continue
		}
		seen[h] = true
		b.WriteString(h + "\t" + releasedHash(String(in)) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}

// The kept reading alone reproduces String of the release before byte for byte on every input of the table: the
// code in package released is that release's, unchanged (#845, thirteenth decision of 2026-11-18).
func TestReleasedReproducesTheReleaseBefore(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	f, err := os.Open(releasedTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	want := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		in, out, _ := strings.Cut(line, "\t")
		want[in] = out
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, in := range releasedInputs(t) {
		h := releasedHash(in)
		w, ok := want[h]
		if !ok || found[h] {
			continue
		}
		found[h] = true
		if got := releasedHash(released.String(in)); got != w {
			t.Errorf("the kept reading differs from the release before on %q: %q", in, released.String(in))
		}
	}
	if len(found) != len(want) {
		t.Errorf("%d of the %d inputs of %s were generated; the corpora changed", len(found), len(want), releasedTable)
	}
}
