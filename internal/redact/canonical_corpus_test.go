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
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
)

// The canonical view corpus (canonical_gen_test.go) leaks today: #845 is the fix, and this slice adds only
// the corpus. What leaks now is listed, input by input, in the expected leak tables below, so make test stays
// green: a leak that is not listed fails, and so does a listed leak that stops leaking, so the fix of #845
// has to shrink the tables as it closes each form (CANON_LEAKS_WRITE=1 writes them again from the full
// corpus).

// canonStringLeaks is the expected leak table of String (format at ReadCanonTable): the check "secret" when a
// piece of secret keeps a byte, "kept" when a word that must survive loses one.
const canonStringLeaks = "testdata/canonical-leaks-string.tsv"

// CanonEntryLeaks is the expected leak table of the entry points (package redact_test): for each input, the
// entry points whose output holds a piece of its secret.
const CanonEntryLeaks = "testdata/canonical-leaks-entry.tsv"

// CanonMainEntryLeaks is the same table written at the commit of canonMainTable, with every entry point
// main has (all but kube.Excerpt); its first line lists them.
const CanonMainEntryLeaks = "testdata/canonical-main-entry.tsv"

// canonSampleDivisor is the share of the corpus checked under the race detector and in short mode: one input
// in 32, chosen with a fixed seed. The full corpus runs in the pass without -race (make test).
const canonSampleDivisor = 32

// canonSample returns cases, or under the race detector and in short mode a seeded sample of them, and
// whether it is a sample.
func canonSample(cases []canonCase) ([]canonCase, bool) {
	if !raceDetector && !testing.Short() {
		return cases, false
	}
	rng := rand.New(rand.NewPCG(845, 8)) //nolint:gosec // a fixed seed keeps the sample the same on every run
	var out []canonCase
	for _, c := range cases {
		if rng.IntN(canonSampleDivisor) == 0 {
			out = append(out, c)
		}
	}
	return out, true
}

// CanonLeak is one leak: the check (an entry point, or "secret" or "kept" for String), the input's hash and
// its name.
type CanonLeak struct{ Check, Hash, Name string }

// An expected leak table has one line per input that leaks: "hash<TAB>form<TAB>checks", the checks it leaks
// at joined by commas, sorted by form and hash. Lines starting with "#" are comments.

// ReadCanonTable reads an expected leak table, one CanonLeak per input and check, with the form as the name;
// the comment lines are returned apart.
func ReadCanonTable(t *testing.T, path string) (rows []CanonLeak, comments []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			comments = append(comments, line)
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || parts[2] == "" {
			t.Fatalf("%s: malformed line %q", path, line)
		}
		for check := range strings.SplitSeq(parts[2], ",") {
			rows = append(rows, CanonLeak{Check: check, Hash: parts[0], Name: parts[1]})
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows, comments
}

// WriteCanonTable writes an expected leak table from rows whose names start with the form, with the comment
// lines first.
func WriteCanonTable(t *testing.T, path string, comments []string, rows []CanonLeak) {
	t.Helper()
	type input struct {
		form   string
		checks []string
	}
	byHash := map[string]*input{}
	for _, r := range rows {
		in := byHash[r.Hash]
		if in == nil {
			form, _, _ := strings.Cut(r.Name, ": ")
			in = &input{form: form}
			byHash[r.Hash] = in
		}
		in.checks = append(in.checks, r.Check)
	}
	hashes := make([]string, 0, len(byHash))
	for h := range byHash {
		hashes = append(hashes, h)
	}
	slices.SortFunc(hashes, func(x, y string) int {
		if c := strings.Compare(byHash[x].form, byHash[y].form); c != 0 {
			return c
		}
		return strings.Compare(x, y)
	})
	var b strings.Builder
	for _, c := range comments {
		b.WriteString(c + "\n")
	}
	for _, h := range hashes {
		in := byHash[h]
		slices.Sort(in.checks)
		fmt.Fprintf(&b, "%s\t%s\t%s\n", h, in.form, strings.Join(in.checks, ","))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// CanonWriteTables reports whether the expected leak tables are to be written instead of checked
// (CANON_LEAKS_WRITE=1). They are written only from the full corpus.
func CanonWriteTables(t *testing.T, sampled bool) bool {
	t.Helper()
	if os.Getenv("CANON_LEAKS_WRITE") != "1" {
		return false
	}
	if sampled {
		t.Fatal("CANON_LEAKS_WRITE needs the full corpus: run without -race and -short")
	}
	return true
}

// CheckCanonLeaks compares the leaks found now on the checked inputs with the expected table rows of the same
// checks: a leak that is not listed fails, and so does a listed leak of a checked input that no longer leaks.
// checked maps the hash of each checked input to its name. It logs the leaks by check and form.
func CheckCanonLeaks(t *testing.T, path string, checked map[string]string, want, got []CanonLeak) {
	t.Helper()
	key := func(r CanonLeak) string { return r.Check + "\t" + r.Hash }
	wantSet := map[string]CanonLeak{}
	for _, r := range want {
		if name, ok := checked[r.Hash]; ok {
			r.Name = name
			wantSet[key(r)] = r
		}
	}
	gotSet := map[string]CanonLeak{}
	for _, r := range got {
		gotSet[key(r)] = r
	}
	added, gone := 0, 0
	for k, r := range gotSet {
		if _, ok := wantSet[k]; !ok {
			added++
			if added <= 20 {
				t.Errorf("new leak, not in %s: %s: %s", path, r.Check, r.Name)
			}
		}
	}
	for k, r := range wantSet {
		if _, ok := gotSet[k]; !ok {
			gone++
			if gone <= 20 {
				t.Errorf("listed in %s but no longer leaks, remove it (CANON_LEAKS_WRITE=1): %s: %s", path, r.Check, r.Name)
			}
		}
	}
	if added+gone > 0 {
		t.Errorf("%d new leaks, %d listed leaks gone, against %s", added, gone, path)
	}
	byCheck := map[string]map[string]int{}
	for _, r := range got {
		form, _, _ := strings.Cut(r.Name, ": ")
		if byCheck[r.Check] == nil {
			byCheck[r.Check] = map[string]int{}
		}
		byCheck[r.Check][form]++
	}
	checks := make([]string, 0, len(byCheck))
	for c := range byCheck {
		checks = append(checks, c)
	}
	slices.Sort(checks)
	for _, c := range checks {
		t.Logf("%s leaks on %d of %d inputs, by form: %v", c, sum(byCheck[c]), len(checked), byCheck[c])
	}
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// The generator's answers hold: each secret piece is where it says, no two cases share a secret, each piece is
// long enough to be found by text, a sequence holds each piece in its strings, and the corpus has every form.
func TestCanonCorpusIsByConstruction(t *testing.T) {
	cases := canonCorpus()
	seen := map[string]string{}
	hashes := map[string]bool{}
	forms := map[string]int{}
	for _, c := range cases {
		forms[c.form]++
		h := canonHash(c)
		if hashes[h] {
			t.Errorf("%s: input repeated", c.name)
		}
		hashes[h] = true
		if len(c.secrets) == 0 {
			t.Errorf("%s: no secret", c.name)
		}
		for _, r := range c.secrets {
			s := c.in[r[0]:r[1]]
			switch {
			case strings.HasPrefix(s, "Qz") && len(s) >= 8:
				// A generated secret is unique in the corpus.
				if other, ok := seen[s]; ok {
					t.Errorf("%s: secret %q also in %s", c.name, s, other)
				}
				seen[s] = c.name
			case slices.Contains(canonPhraseWords, s):
				// A word of a phrase stands nowhere else in its input.
				if strings.Count(c.in, s) != 1 {
					t.Errorf("%s: phrase word %q is in the input more than once", c.name, s)
				}
			default:
				t.Errorf("%s: secret range %v holds %q", c.name, r, s)
			}
			if c.args != nil && !strings.Contains(strings.Join(c.args, "\x00"), s) {
				t.Errorf("%s: secret %q not in the strings %q", c.name, s, c.args)
			}
		}
		for _, r := range c.keep {
			if strings.Contains(c.in[r[0]:r[1]], "Qz") {
				t.Errorf("%s: kept range %v overlaps a secret", c.name, r)
			}
		}
	}
	for _, f := range canonForms {
		if forms[f] == 0 {
			t.Errorf("no input of the form %q", f)
		}
	}
	t.Logf("%d inputs, by form: %v", len(cases), forms)
}

// canonStringLeaksOf returns the rows of the String leak table for c: a piece of secret that keeps a byte,
// a kept word that loses one.
func canonStringLeaksOf(c canonCase) []CanonLeak {
	red := make([]bool, len(c.in))
	for _, sp := range defaultRedactor.spans(c.in) {
		for i := sp.start; i < sp.end; i++ {
			red[i] = true
		}
	}
	var out []CanonLeak
	h := canonHash(c)
	hit := func(rs [][2]int, want bool) bool {
		for _, r := range rs {
			for i := r[0]; i < r[1]; i++ {
				if red[i] == want {
					return true
				}
			}
		}
		return false
	}
	if hit(c.secrets, false) {
		out = append(out, CanonLeak{"secret", h, c.name})
	}
	if hit(c.keep, true) {
		out = append(out, CanonLeak{"kept", h, c.name})
	}
	return out
}

// Every secret of the corpus is redacted by String and every kept word survives, except the leaks listed in
// canonStringLeaks.
func TestCanonCorpusThroughString(t *testing.T) {
	cases, sampled := canonSample(canonCorpus())
	checked := map[string]string{}
	got := make([]CanonLeak, 0, len(cases))
	for _, c := range cases {
		checked[canonHash(c)] = c.name
		got = append(got, canonStringLeaksOf(c)...)
	}
	if CanonWriteTables(t, sampled) {
		WriteCanonTable(t, canonStringLeaks, []string{
			"# Expected leaks of String on the canonical view corpus (#845), written by CANON_LEAKS_WRITE=1.",
		}, got)
		return
	}
	want, _ := ReadCanonTable(t, canonStringLeaks)
	CheckCanonLeaks(t, canonStringLeaks, checked, want, got)
}

// Each byte main redacted in the corpus is still redacted (canonMainTable).
func TestCanonCorpusStillRedactsWhatMainRedacted(t *testing.T) {
	all := canonCorpus()
	checked, _ := canonSample(all)
	want := map[string]bool{}
	for _, c := range checked {
		want[canonHash(c)] = true
	}
	f, err := os.Open(canonMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	n, failed, redacted := 0, 0, 0
	for sc.Scan() {
		var row canonMainRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("line %d: %v", n+1, err)
		}
		if n >= len(all) || canonHash(all[n]) != row.Hash {
			t.Fatalf("%s line %d does not match its generator: regenerate it from the commit it names", canonMainTable, n+1)
		}
		c := all[n]
		n++
		if len(row.Spans) > 0 {
			redacted++
		}
		if !want[row.Hash] {
			continue
		}
		now := defaultRedactor.spans(c.in)
		for _, sp := range row.Spans {
			for i := sp[0]; i < sp[1]; i++ {
				if !coveredBy([2]int{i, i + 1}, now) {
					failed++
					if failed <= 20 {
						t.Errorf("%s: main redacted %q, now kept:\n in: %q\n now: %q", c.name, c.in[sp[0]:sp[1]], c.in, String(c.in))
					}
					break
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n != len(all) {
		t.Fatalf("%s has %d lines, the corpus %d inputs: regenerate it from the commit it names", canonMainTable, n, len(all))
	}
	if failed > 0 {
		t.Errorf("%d ranges main redacted are kept now", failed)
	}
	if redacted < len(all)/2 {
		t.Fatalf("table too weak: main redacted %d of %d inputs", redacted, len(all))
	}
}
