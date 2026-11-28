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

package logcut

import (
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestString(t *testing.T) {
	for _, tc := range []struct {
		name, s string
		n       int
		want    string
		cut     bool
	}{
		{"empty", "", 10, "", false},
		{"fits", "a\nb\n", 4, "a\nb\n", false},
		{"whole lines", "one\ntwo\nthree\n", 8, "three\n", true},
		{"no room", "one\n", 0, "", true},
		{"long last line keeps its tail", "start\n" + strings.Repeat("x", 50) + "end\n", 10, "xxxxxxend\n", true},
		{"trailing blank lines", strings.Repeat("y", 40) + "\n\n  \n", 8, "yyyyyyy\n", true},
		{"tail does not start on a blank", "ab cd ef\n", 4, "ef\n", true},
		{"tail does not start on a dash and blank", "ab - ef\n", 5, "ef\n", true},
		{"tail may start on a dash before text", "ab -ef\n", 4, "-ef\n", true},
		{"marker kept whole or dropped", "x [REDACTED] tail\n", 12, "tail\n", true},
		{"marker kept whole", "x [REDACTED] tail\n", 16, "[REDACTED] tail\n", true},
		{"only a long run of stars", "a=" + strings.Repeat("*", 20) + "\n", 6, "*****\n", true},
		{"rune boundary", "語語語語\n", 7, "語語\n", true},
		{"only blank", "  \n\n", 1, "", true},
		{"indented lines stay whole", "Exception\n  at a.go\n  at b.go\n", 18, "  at b.go\n", true},
		{"only items falls back to a tail", "x\n" + strings.Repeat("- ", 40) + "-\n", 6, "- - -\n", true},
	} {
		got, cut := String(tc.s, tc.n)
		if got != tc.want || cut != tc.cut {
			t.Errorf("%s: String(%q, %d) = (%q, %t), want (%q, %t)", tc.name, tc.s, tc.n, got, cut, tc.want, tc.cut)
		}
	}
}

// Tail never searches the size as if it grew with the text: it keeps the first start that fits, so with a size
// that is larger for a middle tail than for a longer one, it keeps the longer one.
func TestTailSizeNeedNotGrow(t *testing.T) {
	s := "aaaa bbbb cccc\n"
	// Starts 0 and 10 fit, every start between them does not.
	size := func(start, end int) int {
		switch {
		case start == 0:
			return 100
		case start < 10:
			return 1000
		default:
			return end - start
		}
	}
	if c := Tail(s, 100, size); c.Start != 0 || c.End != len(s) {
		t.Errorf("Tail = [%d:%d], want all of s", c.Start, c.End)
	}
	if start := Tail("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", 10, func(start, end int) int {
		if start == 5 {
			return 10
		}
		return 2 * (end - start)
	}).Start; start != 5 {
		t.Errorf("Tail start = %d, want 5, the first start that fits", start)
	}
}

// Generated: a log whose last line ends with a word gives a non-empty excerpt within the bound that ends with
// a tail of that line, for any bound of at least the longest marker shape and a byte.
func TestTailKeepsTheLastLineGenerated(t *testing.T) {
	rng := rand.New(rand.NewSource(1055)) // #nosec G404 -- a seeded corpus, reproducible by design
	tokens := []string{"ok", "error", " ", "\t", "-", "- ", "*", "******", "[REDACTED]", "[REDACTED:jwt]", "語", "é", " "}
	for i := range 3000 {
		var b strings.Builder
		for range rng.Intn(4) {
			for range rng.Intn(40) {
				b.WriteString(tokens[rng.Intn(len(tokens))])
			}
			b.WriteByte('\n')
		}
		last := "L"
		for range rng.Intn(200) {
			last += tokens[rng.Intn(len(tokens))]
		}
		last += "end"
		b.WriteString(last + []string{"\n", "", " \n", "\n\n", "\r\n \n"}[rng.Intn(5)])
		s := b.String()
		n := 45 + rng.Intn(400)
		got, _ := String(s, n)
		trimmed := strings.TrimRight(got, " \t\r\n ")
		lines := strings.Split(trimmed, "\n")
		tail := lines[len(lines)-1]
		if len(got) > n || !utf8.ValidString(got) || tail == "" || !strings.HasSuffix(last, tail) ||
			(len(lines) == 1 && len(got) < len(s) && (tail[0] == ' ' || tail[0] == '\t' || strings.HasPrefix(tail, "- "))) {
			t.Fatalf("case %d: String(%q, %d) = %q", i, s, n, got)
		}
	}
}

// Generated: logs whose every line may be taken for the continuation of an entry (indented by a space or a tab,
// a "- " item, or blank-prefixed), among them last lines of only "- " items with no start a reader keeps. The
// excerpt is a non-empty tail of the last line within the bound, never nothing (decision of 2026-11-28 on
// #1055: the property wins over the start rule of step 4).
func TestTailContinuationLogsGenerated(t *testing.T) {
	rng := rand.New(rand.NewSource(10551)) // #nosec G404 -- a seeded corpus, reproducible by design
	prefixes := []string{" ", "  ", "\t", "- ", "-\t", " - "}
	words := []string{"ok", "error", "at", "main.go:42", "-", "- ", " ", "\t", "語", "[REDACTED]", "******"}
	line := func(n int) string {
		var b strings.Builder
		b.WriteString(prefixes[rng.Intn(len(prefixes))])
		for range n {
			b.WriteString(words[rng.Intn(len(words))])
		}
		return b.String()
	}
	items := 0
	for i := range 3000 {
		var b strings.Builder
		for range rng.Intn(5) {
			b.WriteString(line(rng.Intn(30)))
			b.WriteString([]string{"\n", "\n\n", "\n \n"}[rng.Intn(3)])
		}
		var last string
		switch rng.Intn(3) {
		case 0:
			last = line(rng.Intn(150)) + "end"
		case 1:
			last = line(rng.Intn(150)) + " end"
		default:
			// Only "- " items and blanks after the prefix: no start a reader keeps.
			last = prefixes[rng.Intn(len(prefixes))] + strings.Repeat([]string{"- ", "-\t", " -"}[rng.Intn(3)], 1+rng.Intn(80)) + "-"
			items++
		}
		b.WriteString(last + []string{"\n", "", " \n", "\n\n"}[rng.Intn(4)])
		s := b.String()
		n := 45 + rng.Intn(400)
		got, _ := String(s, n)
		trimmed := strings.TrimRight(got, " \t\r\n")
		tail := trimmed[strings.LastIndexByte(trimmed, '\n')+1:]
		if len(got) > n || !utf8.ValidString(got) || tail == "" || !strings.HasSuffix(strings.TrimRight(last, " \t"), tail) {
			t.Fatalf("case %d: String(%q, %d) = %q, want a non-empty tail of the last line", i, s, n, got)
		}
	}
	if items == 0 {
		t.Fatal("no last line of only items was drawn")
	}
}

// The excerpt starts at a line a reader keeps (decision of 2026-11-28 on #1055): continuation lines at the start
// of the newest lines that fit are left out; when all of them are continuation lines, the first line of their
// entry is kept ahead of the newest of them that fit with it; when none fits with it, the newest lines are kept
// without it; with no such line, as they are. Read keeps whole lines for the reader to drop, and leaves out the
// tail of a continuation line the reader would drop whole.
func TestTailStartsAtALineAReaderKeeps(t *testing.T) {
	trace := "start\nException in main\n\tat a(A.java:1)\n\tat b(B.java:2)\n\tat c(C.java:3)\n"
	for _, tc := range []struct {
		name, s string
		n       int
		want    string
		kind    Kind
	}{
		{"leading continuation left out", "x\nA\n  a1\nB\n  b1\n", 12, "B\n  b1\n", Lines},
		{"header kept, middle frames left out", trace, 34, "Exception in main\n\tat c(C.java:3)\n", HeaderKept},
		{"header kept with two frames", trace, 50, "Exception in main\n\tat b(B.java:2)\n\tat c(C.java:3)\n", HeaderKept},
		{"header and frames contiguous", trace, 66, "Exception in main\n\tat a(A.java:1)\n\tat b(B.java:2)\n\tat c(C.java:3)\n", Lines},
		{"header does not fit", trace, 33, "\tat b(B.java:2)\n\tat c(C.java:3)\n", HeaderLeftOut},
		{"no header", "\tat a\n\tat b\n\tat c\n", 12, "\tat b\n\tat c\n", NoHeader},
		{"items under a key", "env:\n- a: 1\n- b: 2\n- c: 3\n", 13, "env:\n- c: 3\n", HeaderKept},
		{"unicode indentation is not a continuation", "x\n　a\n", 5, "　a\n", Lines},
		{"trailing blank lines", "Err\n\tat a\n\tat b\n\n\n", 13, "Err\n\tat b\n\n\n", HeaderKept},
	} {
		c := Tail(tc.s, tc.n, nil)
		if got := c.Text(tc.s); got != tc.want || c.Kind != tc.kind || len(got) > tc.n {
			t.Errorf("%s: Tail(%q, %d) = %q kind %d, want %q kind %d", tc.name, tc.s, tc.n, got, c.Kind, tc.want, tc.kind)
		}
	}
	for _, tc := range []struct {
		name, s string
		n       int
		want    string
		kind    Kind
	}{
		{"no header", "\tat a\n\tat b\n\tat c\n", 12, "\tat b\n\tat c\n", NoHeader},
		{"leading continuation kept for the reader", "x\nA\n  a1\nB\n  b1\n", 12, "  a1\nB\n  b1\n", Lines},
		{"orphaned indented last line", strings.Repeat(" ", 80) + "sample\n", 64, "", Dropped},
		{"indented last line after a header", "Err\n" + strings.Repeat(" ", 80) + "sample\n", 64, "sample\n", LastLine},
		{"header left out stays", "Exception in main\n\tat a(A.java:1)\n", 20, "\tat a(A.java:1)\n", HeaderLeftOut},
	} {
		c := Read(tc.s, tc.n)
		if got := c.Text(tc.s); got != tc.want || c.Kind != tc.kind {
			t.Errorf("%s: Read(%q, %d) = %q kind %d, want %q kind %d", tc.name, tc.s, tc.n, got, c.Kind, tc.want, tc.kind)
		}
	}
}

// The cut's work is linear in the log: a last line with a long run of Unicode spaces at its end costs no more per
// byte than a short one (finding of 2026-11-28 on #1055: each start rescanned the blank suffix, 19 s for 256 KiB).
// The input is far larger than what a linear cut needs a few milliseconds for.
func TestTailWorkIsLinearOnUnicodeBlanks(t *testing.T) {
	s := "x\nLine" + strings.Repeat("　", 256<<10/3) + "\n"
	start := time.Now()
	c := Tail(s, 64, nil)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Tail took %v on %d bytes", d, len(s))
	}
	if got := c.Text(s); got == "" || len(got) > 64 {
		t.Errorf("Tail = %q", got)
	}
}
