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
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// invisibleData is the extract of Unicode's DerivedCoreProperties.txt and DerivedGeneralCategory.txt that
// invisibleRanges is made from: its header and every line of Default_Ignorable_Code_Point and of category Cf.
const invisibleData = "testdata/invisible.txt"

// readInvisible returns the version the extract's header names and its ranges, sorted and merged.
func readInvisible(t *testing.T) (version string, ranges [][2]rune) {
	t.Helper()
	f, err := os.Open(invisibleData)
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
		if len(fields) < 2 {
			t.Fatalf("%s: malformed line %q", invisibleData, line)
		}
		if p := strings.Fields(fields[1]); len(p) == 0 || p[0] != "Default_Ignorable_Code_Point" && p[0] != "Cf" {
			t.Fatalf("%s: line %q is of neither property", invisibleData, line)
		}
		lo, hi, _ := strings.Cut(strings.TrimSpace(fields[0]), "..")
		if hi == "" {
			hi = lo
		}
		a, err1 := strconv.ParseInt(lo, 16, 32)
		b, err2 := strconv.ParseInt(hi, 16, 32)
		if err1 != nil || err2 != nil || b < a {
			t.Fatalf("%s: line %q has no range", invisibleData, line)
		}
		ranges = append(ranges, [2]rune{rune(a), rune(b)})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(ranges, func(x, y [2]rune) int { return int(x[0] - y[0]) })
	merged := ranges[:0]
	for _, r := range ranges {
		if n := len(merged); n > 0 && r[0] <= merged[n-1][1]+1 {
			merged[n-1][1] = max(merged[n-1][1], r[1])
			continue
		}
		merged = append(merged, r)
	}
	return version, merged
}

// invisibleRanges is every code point the extract marks default ignorable or puts in category Cf, from the
// version its comment names, and nothing else (#845, fourth round).
func TestInvisibleIsDefaultIgnorableOrFormat(t *testing.T) {
	version, want := readInvisible(t)
	if version == "" || len(want) == 0 {
		t.Fatalf("%s names no version or holds no line", invisibleData)
	}
	if !slices.Equal(invisibleRanges, want) {
		t.Errorf("invisibleRanges = %X, %s holds %X", invisibleRanges, invisibleData, want)
	}
	src, err := os.ReadFile("fold.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`invisible characters of the folded view[^.]*?Unicode\s*(?://\s*)?(\S+) marks`).FindSubmatch(src)
	if m == nil || string(m[1]) != version {
		t.Errorf("the comment of invisibleRanges names Unicode version %q, %s holds version %s", m, invisibleData, version)
	}
	for _, r := range want {
		for _, c := range []rune{r[0], r[1], (r[0] + r[1]) / 2} {
			if !isInvisible(nil, c) {
				t.Errorf("isInvisible(%U) = false", c)
			}
		}
		for _, c := range []rune{r[0] - 1, r[1] + 1} {
			if !slices.ContainsFunc(want, func(x [2]rune) bool { return x[0] <= c && c <= x[1] }) && isInvisible(nil, c) {
				t.Errorf("isInvisible(%U) = true, it is not in the extract", c)
			}
		}
	}
	for _, c := range []rune{'a', ' ', ':', 0x00A0, 0x3000, 0x2028, 0xFFFC} {
		if isInvisible(nil, c) {
			t.Errorf("isInvisible(%U) = true", c)
		}
	}
}
