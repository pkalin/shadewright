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
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// confusablesColon is the extract of Unicode's confusables.txt that foldColons is made from: its header and
// every line whose target is one or more U+003A COLON.
const confusablesColon = "testdata/confusables-colon.txt"

// readConfusablesColon returns the version the extract's header names and the source code points of its
// lines, in order, and fails when a line's target is not made of colons only.
func readConfusablesColon(t *testing.T) (version string, runes []rune) {
	t.Helper()
	f, err := os.Open(confusablesColon)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "# Version: "); ok {
			version = strings.TrimSpace(v)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ";")
		if len(fields) < 3 {
			t.Fatalf("%s: malformed line %q", confusablesColon, line)
		}
		for target := range strings.FieldsSeq(fields[1]) {
			if target != "003A" {
				t.Fatalf("%s: line %q maps to more than colons", confusablesColon, line)
			}
		}
		r, err := strconv.ParseInt(strings.TrimSpace(fields[0]), 16, 32)
		if err != nil {
			t.Fatalf("%s: line %q: %v", confusablesColon, line, err)
		}
		runes = append(runes, rune(r))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return version, runes
}

// foldColons is every character the confusables list maps to colons, from the version its comment names, and
// nothing else (#845, third round: the comment named the list while the table held five of its characters).
func TestFoldColonsAreTheConfusablesOfColon(t *testing.T) {
	version, want := readConfusablesColon(t)
	if version == "" || len(want) == 0 {
		t.Fatalf("%s names no version or holds no line", confusablesColon)
	}
	if !slices.Equal(foldColons, want) {
		t.Errorf("foldColons = %U, the confusables of ':' in %s are %U", foldColons, confusablesColon, want)
	}
	src, err := os.ReadFile("fold.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`confusables\.txt\s*(?://\s*)?version (\S+) `).FindSubmatch(src)
	if m == nil || string(m[1]) != version {
		t.Errorf("the comment of foldColons names confusables.txt version %q, %s holds version %s", m, confusablesColon, version)
	}
	for _, r := range want {
		if !isFoldColon(nil, r) {
			t.Errorf("isFoldColon(%U) = false", r)
		}
	}
	for _, r := range []rune{':', 0x205D, 0x2254, 0x00B7, 0x003B} {
		if isFoldColon(nil, r) {
			t.Errorf("isFoldColon(%U) = true, it is not in the list", r)
		}
	}
}

// confusablesEqualsArrows is the extract of confusables.txt that foldEquals and foldArrows are made from: its
// header and every line whose target is one or more U+003D EQUALS SIGN, U+2192 or U+21D2.
const confusablesEqualsArrows = "testdata/confusables-equals-arrows.txt"

// foldEquals is every character the confusables list maps to equals signs, and foldArrows the two arrows and
// every character the list maps to one of them, from the version the comment names, and nothing else.
func TestFoldMarksAreTheConfusablesOfEqualsAndArrows(t *testing.T) {
	f, err := os.Open(confusablesEqualsArrows)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var version string
	var equals []rune
	arrows := map[rune]string{0x2192: "->", 0x21D2: "=>"}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "# Version: "); ok {
			version = strings.TrimSpace(v)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ";")
		if len(fields) < 3 {
			t.Fatalf("malformed line %q", line)
		}
		r, err := strconv.ParseInt(strings.TrimSpace(fields[0]), 16, 32)
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		switch targets := strings.Fields(fields[1]); {
		case len(targets) == 1 && (targets[0] == "2192" || targets[0] == "21D2"):
			arrows[rune(r)] = map[string]string{"2192": "->", "21D2": "=>"}[targets[0]]
		default:
			for _, tg := range targets {
				if tg != "003D" {
					t.Fatalf("line %q maps to more than equals signs", line)
				}
			}
			equals = append(equals, rune(r))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if version == "" || len(equals) == 0 {
		t.Fatalf("%s names no version or holds no line", confusablesEqualsArrows)
	}
	if !slices.Equal(foldEquals, equals) {
		t.Errorf("foldEquals = %U, the confusables of '=' in %s are %U", foldEquals, confusablesEqualsArrows, equals)
	}
	if !maps.Equal(foldArrows, arrows) {
		t.Errorf("foldArrows = %v, want %v", foldArrows, arrows)
	}
	src, err := os.ReadFile("fold.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`confusables\.txt version (\S+) whose target is\s*(?://\s*)?one or more U\+003D`).FindSubmatch(src)
	if m == nil || string(m[1]) != version {
		t.Errorf("the comment of foldEquals names confusables.txt version %q, %s holds version %s", m, confusablesEqualsArrows, version)
	}
	for r, want := range arrows {
		if got, ok := foldMark(nil, r); !ok || got != want {
			t.Errorf("foldMark(%U) = %q, %v, want %q", r, got, ok, want)
		}
	}
	for _, r := range []rune{'=', '>', 0x2190, 0x21D0, 0x2261, 0x2260, 0x27F6} {
		if _, ok := foldMark(nil, r); ok {
			t.Errorf("foldMark(%U) folds, it is not in the list", r)
		}
	}
}
