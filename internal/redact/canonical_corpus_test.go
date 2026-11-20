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
	"strconv"
	"strings"
	"testing"
)

// The canonical view corpus (canonical_gen_test.go) leaks today: #845 is the fix, and this slice adds only
// the corpus. What leaks now is listed, piece by piece, in the expected leak tables below, so make test stays
// green: a leak that is not listed fails, and so does a listed leak that stops leaking, so the fix of #845
// has to shrink the tables as it closes each form (CANON_LEAKS_WRITE=1 writes them again from the full
// corpus). A row whose input is not in the corpus fails too.

// canonStringLeaks is the expected leak table of String (format at ReadCanonTable): the check "secret" when a
// piece of secret keeps a byte, "kept" when a piece of text that must survive loses one.
const canonStringLeaks = "testdata/canonical-leaks-string.tsv"

// canonCheckSecret is the check of String's leak table for a piece of secret that keeps a byte.
const canonCheckSecret = "secret"

// CanonEntryLeaks is the expected leak table of the entry points (package redact_test): for each piece of
// each input, the entry points whose output holds the secret piece, or lost the kept piece.
const CanonEntryLeaks = "testdata/canonical-leaks-entry.tsv"

// CanonMainEntryLeaks is the same table written at the commit of canonMainTable, with every entry point
// main has; its first lines name the commit and list the entry points.
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

// CanonLeak is one leak of one piece: the check (an entry point, or "secret" or "kept" for String), the
// input's hash and name, and the piece (CanonSecretItem or CanonKeptItem).
type CanonLeak struct{ Check, Hash, Name, Item string }

// CanonSecretItem names the secret piece i of an input in a leak table, CanonKeptItem the kept piece i.
func CanonSecretItem(i int) string { return "s" + strconv.Itoa(i) }

// CanonKeptItem names the kept piece i of an input in a leak table.
func CanonKeptItem(i int) string { return "k" + strconv.Itoa(i) }

// parseCanonItem returns the kind ('s' or 'k') and index of an item, and whether it is well formed.
func parseCanonItem(item string) (byte, int, bool) {
	if len(item) < 2 || (item[0] != 's' && item[0] != 'k') {
		return 0, 0, false
	}
	i, err := strconv.Atoi(item[1:])
	if err != nil || i < 0 || strconv.Itoa(i) != item[1:] {
		return 0, 0, false
	}
	return item[0], i, true
}

// CanonInput is what a leak table may name of an input of the full corpus: its name and the number of its
// secret and kept pieces.
type CanonInput struct {
	Name          string
	Secrets, Keep int
}

// canonIndexOf returns the CanonInput of each case, by hash.
func canonIndexOf(cases []canonCase) map[string]CanonInput {
	idx := make(map[string]CanonInput, len(cases))
	for _, c := range cases {
		idx[canonHash(c)] = CanonInput{Name: c.name, Secrets: len(c.secrets), Keep: len(c.keep)}
	}
	return idx
}

// An expected leak table has one line per piece that leaks: "hash<TAB>form<TAB>item<TAB>checks", the checks
// it leaks at joined by commas, sorted by form, hash and item. Lines starting with "#" are comments.

// ReadCanonTable reads an expected leak table, one CanonLeak per piece and check, with the form as the name;
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
		if len(parts) != 4 || parts[3] == "" {
			t.Fatalf("%s: malformed line %q", path, line)
		}
		for check := range strings.SplitSeq(parts[3], ",") {
			rows = append(rows, CanonLeak{Check: check, Hash: parts[0], Name: parts[1], Item: parts[2]})
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
	type piece struct {
		form, hash, item string
		checks           []string
	}
	byKey := map[string]*piece{}
	for _, r := range rows {
		k := r.Hash + "\t" + r.Item
		p := byKey[k]
		if p == nil {
			form, _, _ := strings.Cut(r.Name, ": ")
			p = &piece{form: form, hash: r.Hash, item: r.Item}
			byKey[k] = p
		}
		if !slices.Contains(p.checks, r.Check) {
			p.checks = append(p.checks, r.Check)
		}
	}
	pieces := make([]*piece, 0, len(byKey))
	for _, p := range byKey {
		pieces = append(pieces, p)
	}
	slices.SortFunc(pieces, func(x, y *piece) int {
		if c := strings.Compare(x.form, y.form); c != 0 {
			return c
		}
		if c := strings.Compare(x.hash, y.hash); c != 0 {
			return c
		}
		xk, xi, _ := parseCanonItem(x.item)
		yk, yi, _ := parseCanonItem(y.item)
		if xk != yk {
			return int(yk) - int(xk) // secret pieces first
		}
		return xi - yi
	})
	var b strings.Builder
	for _, c := range comments {
		b.WriteString(c + "\n")
	}
	for _, p := range pieces {
		slices.Sort(p.checks)
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", p.hash, p.form, p.item, strings.Join(p.checks, ","))
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

// CanonTableErrors returns what is wrong with the rows of a leak table against the full corpus: a row whose
// hash is no input of the corpus, whose form is not the input's, or whose piece the input does not have.
func CanonTableErrors(rows []CanonLeak, index map[string]CanonInput) []string {
	var errs []string
	for _, r := range rows {
		in, ok := index[r.Hash]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s %s %s: no input of the corpus has this hash", r.Hash, r.Item, r.Check))
			continue
		}
		if form, _, _ := strings.Cut(in.Name, ": "); form != r.Name && in.Name != r.Name {
			errs = append(errs, fmt.Sprintf("%s: form %q, the input is %q", r.Hash, r.Name, in.Name))
		}
		kind, i, ok := parseCanonItem(r.Item)
		switch {
		case !ok:
			errs = append(errs, fmt.Sprintf("%s: malformed piece %q", r.Hash, r.Item))
		case kind == 's' && i >= in.Secrets, kind == 'k' && i >= in.Keep:
			errs = append(errs, fmt.Sprintf("%s: piece %s, the input %q has %d secret and %d kept pieces", r.Hash,
				r.Item, in.Name, in.Secrets, in.Keep))
		}
	}
	return errs
}

// CanonLeakDiff compares the leaks found now on the checked inputs (checked maps their hashes to their names)
// with the expected rows: added are the leaks not listed, gone the listed leaks of a checked input that no
// longer leak. Both are sorted.
func CanonLeakDiff(checked map[string]string, want, got []CanonLeak) (added, gone []CanonLeak) {
	key := func(r CanonLeak) string { return r.Check + "\t" + r.Hash + "\t" + r.Item }
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
	for k, r := range gotSet {
		if _, ok := wantSet[k]; !ok {
			added = append(added, r)
		}
	}
	for k, r := range wantSet {
		if _, ok := gotSet[k]; !ok {
			gone = append(gone, r)
		}
	}
	cmp := func(x, y CanonLeak) int { return strings.Compare(key(x), key(y)) }
	slices.SortFunc(added, cmp)
	slices.SortFunc(gone, cmp)
	return added, gone
}

// CheckCanonLeaks checks the expected table rows against the full corpus (index) and compares the leaks found
// now on the checked inputs with the rows of the same checks: a row of no input fails, a leak that is not
// listed fails, and so does a listed leak of a checked input that no longer leaks. checked maps the hash of
// each checked input to its name. It then shows by mutation that the comparison tells one piece from another,
// and logs the leaking pieces by check and form.
func CheckCanonLeaks(t *testing.T, path string, index map[string]CanonInput, checked map[string]string, want, got []CanonLeak) {
	t.Helper()
	for i, e := range CanonTableErrors(want, index) {
		if i < 20 {
			t.Errorf("%s: %s", path, e)
		} else {
			t.Errorf("%s: and more rows that name no piece of the corpus", path)
			break
		}
	}
	added, gone := CanonLeakDiff(checked, want, got)
	for i, r := range added {
		if i < 20 {
			t.Errorf("new leak, not in %s: %s: %s, piece %s", path, r.Check, r.Name, r.Item)
		}
	}
	for i, r := range gone {
		if i < 20 {
			t.Errorf("listed in %s but no longer leaks, remove it (CANON_LEAKS_WRITE=1): %s: %s, piece %s", path,
				r.Check, r.Name, r.Item)
		}
	}
	if len(added)+len(gone) > 0 {
		t.Errorf("%d new leaks, %d listed leaks gone, against %s", len(added), len(gone), path)
	}
	checkCanonMutations(t, index, checked, want, got)
	byCheck := map[string]map[string]int{}
	inputs := map[string]map[string]bool{}
	for _, r := range got {
		form, _, _ := strings.Cut(r.Name, ": ")
		check := r.Check + ", secret pieces"
		if strings.HasPrefix(r.Item, "k") {
			check = r.Check + ", kept pieces lost"
		}
		if byCheck[check] == nil {
			byCheck[check], inputs[check] = map[string]int{}, map[string]bool{}
		}
		byCheck[check][form]++
		inputs[check][r.Hash] = true
	}
	checks := make([]string, 0, len(byCheck))
	for c := range byCheck {
		checks = append(checks, c)
	}
	slices.Sort(checks)
	for _, c := range checks {
		t.Logf("%s: %d, in %d of %d inputs, by form: %v", c, sum(byCheck[c]), len(inputs[c]),
			len(checked), byCheck[c])
	}
}

// checkCanonMutations shows that the comparison works piece by piece: for an input that leaks one piece at a
// check and not another of the same kind (secret or kept), dropping the row of the leaking piece must report
// exactly that leak as new, a row for the piece that does not leak must be reported exactly as gone, and a row
// of a hash outside the corpus must be refused. It runs once for each check and kind that has such an input.
func checkCanonMutations(t *testing.T, index map[string]CanonInput, checked map[string]string, want, got []CanonLeak) {
	t.Helper()
	leaks := map[string]bool{}
	for _, r := range got {
		leaks[r.Check+"\t"+r.Hash+"\t"+r.Item] = true
	}
	tried := map[string]bool{}
	for _, r := range got {
		kind, _, _ := parseCanonItem(r.Item)
		if tried[r.Check+"\t"+string(kind)] {
			continue
		}
		n, item := index[r.Hash].Secrets, CanonSecretItem
		if kind == 'k' {
			n, item = index[r.Hash].Keep, CanonKeptItem
		}
		other := ""
		for i := range n {
			if !leaks[r.Check+"\t"+r.Hash+"\t"+item(i)] {
				other = item(i)
				break
			}
		}
		if other == "" {
			continue
		}
		tried[r.Check+"\t"+string(kind)] = true
		drop := slices.DeleteFunc(slices.Clone(want), func(w CanonLeak) bool {
			return w.Check == r.Check && w.Hash == r.Hash && w.Item == r.Item
		})
		added, gone := CanonLeakDiff(checked, drop, got)
		if len(added) != 1 || added[0].Hash != r.Hash || added[0].Item != r.Item || len(gone) != 0 {
			t.Errorf("mutation at %s: the row of %s piece %s dropped, the comparison found %v new and %v gone",
				r.Check, r.Name, r.Item, added, gone)
		}
		extra := append(slices.Clone(want), CanonLeak{Check: r.Check, Hash: r.Hash, Name: r.Name, Item: other})
		added, gone = CanonLeakDiff(checked, extra, got)
		if len(gone) != 1 || gone[0].Hash != r.Hash || gone[0].Item != other || len(added) != 0 {
			t.Errorf("mutation at %s: a row for %s piece %s added, the comparison found %v new and %v gone",
				r.Check, r.Name, other, added, gone)
		}
	}
	if len(got) > 0 && len(tried) == 0 {
		t.Errorf("no input leaks one piece and not another: the mutation check of the comparison did not run")
	}
	stray := []CanonLeak{{Check: canonCheckSecret, Hash: "0000000000000000", Name: "none", Item: CanonSecretItem(0)}}
	if errs := CanonTableErrors(stray, index); len(errs) != 1 {
		t.Errorf("a row of a hash outside the corpus was not refused: %v", errs)
	}
	t.Logf("mutation check of the comparison ran at %d checks and kinds", len(tried))
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// The generator's answers hold: each secret piece is where it says, no two cases share a secret, a generated
// secret's first and last canonPartialMin bytes stand once in its input, each piece is long enough to be found
// by text, a sequence holds each piece in its strings, kept pieces are no secret, and the corpus has every form.
func TestCanonCorpusIsByConstruction(t *testing.T) {
	cases := canonCorpus()
	seen := map[string]string{}
	hashes := map[string]bool{}
	forms := map[string]int{}
	pieces := map[string]int{}
	for _, c := range cases {
		forms[c.form]++
		pieces[c.form] += len(c.secrets)
		h := canonHash(c)
		if hashes[h] {
			t.Errorf("%s: input repeated", c.name)
		}
		hashes[h] = true
		if len(c.secrets) == 0 {
			t.Errorf("%s: no secret", c.name)
		}
		if len(c.keep) == 0 {
			t.Errorf("%s: no text that must stay", c.name)
		}
		for _, r := range c.secrets {
			s := c.in[r[0]:r[1]]
			switch {
			case len(s) == 11 && (strings.HasPrefix(s, "Qz") || strings.HasPrefix(s, "Q!") || strings.Trim(s, "0123456789") == ""):
				// A generated secret is unique in the corpus, and its prefix and suffix tell it from the other
				// pieces of its input.
				if other, ok := seen[s]; ok {
					t.Errorf("%s: secret %q also in %s", c.name, s, other)
				}
				seen[s] = c.name
				for _, part := range []string{s[:canonPartialMin], s[len(s)-canonPartialMin:]} {
					if strings.Count(c.in, part) != 1 {
						t.Errorf("%s: %q of secret %q is in the input more than once", c.name, part, s)
					}
				}
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
			k := c.in[r[0]:r[1]]
			if k == "" || strings.Contains(k, "Qz") {
				t.Errorf("%s: kept range %v holds %q", c.name, r, k)
			}
			for _, s := range c.secrets {
				if r[0] < s[1] && s[0] < r[1] {
					t.Errorf("%s: kept range %v overlaps secret range %v", c.name, r, s)
				}
			}
		}
	}
	for _, f := range canonForms {
		if forms[f] == 0 {
			t.Errorf("no input of the form %q", f)
		}
	}
	t.Logf("%d inputs, by form: %v; secret pieces by form: %v", len(cases), forms, pieces)
}

// canonStringLeaksOf returns the rows of the String leak table for c: each piece of secret that keeps a byte,
// each kept piece that loses one. String replaces only its spans, so a kept piece no span touches survives
// byte for byte.
func canonStringLeaksOf(c canonCase) []CanonLeak {
	red := make([]bool, len(c.in))
	for _, sp := range defaultRedactor.spans(nil, c.in) {
		for i := sp.start; i < sp.end; i++ {
			red[i] = true
		}
	}
	var out []CanonLeak
	h := canonHash(c)
	hit := func(r [2]int, want bool) bool {
		for i := r[0]; i < r[1]; i++ {
			if red[i] == want {
				return true
			}
		}
		return false
	}
	for i, r := range c.secrets {
		if hit(r, false) {
			out = append(out, CanonLeak{canonCheckSecret, h, c.name, CanonSecretItem(i)})
		}
	}
	for i, r := range c.keep {
		if hit(r, true) {
			out = append(out, CanonLeak{"kept", h, c.name, CanonKeptItem(i)})
		}
	}
	return out
}

// Every piece of secret of the corpus is redacted by String and every kept piece survives, except the leaks
// listed in canonStringLeaks.
func TestCanonCorpusThroughString(t *testing.T) {
	all := canonCorpus()
	cases, sampled := canonSample(all)
	checked := map[string]string{}
	got := make([]CanonLeak, 0, len(cases))
	for _, c := range cases {
		checked[canonHash(c)] = c.name
		got = append(got, canonStringLeaksOf(c)...)
	}
	if CanonWriteTables(t, sampled) {
		WriteCanonTable(t, canonStringLeaks, []string{
			"# Expected leaks of String on the canonical view corpus (#845), piece by piece, written by CANON_LEAKS_WRITE=1.",
		}, got)
		return
	}
	want, _ := ReadCanonTable(t, canonStringLeaks)
	CheckCanonLeaks(t, canonStringLeaks, canonIndexOf(all), checked, want, got)
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
		now := defaultRedactor.spans(nil, c.in)
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
