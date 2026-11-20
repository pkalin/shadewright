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
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// Generated checks of the fourth round of #845 that are no corpus rows: invisible characters after a value go
// with its replacement, and many bare keys before one block read the block once.

// An invisible character after a value is part of what is replaced (#845, fourth round): for a key written
// plainly or with an invisible character inside it, a value, then a run of one to three invisible characters
// of the class and the end of the text or of the line, every byte of the run is redacted and the next line
// stays.
func TestInvisibleAfterValueGoesWithIt(t *testing.T) {
	n, failed := 0, 0
	for ki, key := range canonGlyphKeys {
		for ci, x := range canonInvisibles {
			run := x
			for j := range ci % 3 {
				run += canonInvisibles[(ci+j+ki+1)%len(canonInvisibles)]
			}
			at := 1 + (ki+ci)%(len(key)-1)
			for _, written := range []string{key, key[:at] + canonInvisibles[(ci+7)%len(canonInvisibles)] + key[at:]} {
				for _, after := range []string{"", "\nnext: ok", "\r\nnext: ok"} {
					n++
					v := fmt.Sprintf("Qz%05dxK9w", 60000+n)
					in := written + ": " + v + run + after
					spans := defaultRedactor.spans(in)
					from := len(written) + 2
					to := from + len(v) + len(run)
					ok := true
					for i := from; i < to; i++ {
						if !coveredBy([2]int{i, i + 1}, spans) {
							ok = false
						}
					}
					if after != "" && !strings.HasSuffix(String(in), after) {
						ok = false
					}
					if !ok {
						if failed++; failed <= 10 {
							t.Errorf("String(%q) = %q: the value and the invisible characters after it are not replaced together", in, String(in))
						}
					}
				}
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d inputs keep an invisible character after the replacement", failed, n)
	}
}

// canonRepeatedKeys are the lines of the repeated-key inputs (#845, fourth round): many bare short keys with a
// block indicator on one line, all claiming the one block below, which does not look like a secret. A rule
// that read the block for each key was quadratic.
var canonRepeatedKeys = []string{"pass: | ", "auth: > ", "sig: |- ", "pass:: | ", "pass\u200b: | ", "pass\uff1a | "}

// canonRepeated returns the repeated-key input for key with n keys and a block of n words.
func canonRepeated(key string, n int) string {
	return strings.Repeat(key, n) + "\n  " + strings.Repeat("garden ", n) + "\nend: ok"
}

// The repeated-key inputs cost the same per byte at 512, 1,024 and 2,048 keys, as checkWork bounds it. On c5daa6f8 "pass: | " cost 2.4M,
// 9.1M and 35.0M steps.
func TestCanonRepeatedKeysWorkIsLinear(t *testing.T) {
	for _, key := range canonRepeatedKeys {
		samples := make([]workSample, 0, 3)
		for _, n := range []int{512, 1024, 2048} {
			samples = append(samples, sampleWork(t, canonRepeated(key, n)))
		}
		checkWork(t, fmt.Sprintf("repeated key %q", key), samples)
	}
}

// The forms a review found on c5daa6f8, each by hand (#845, fourth round): a separator of other characters, an
// invisible character outside category Cf inside a key, a value of user info in angle brackets, a sequence whose
// key, separator and value stand in different strings. No secret piece is left, and the text around it stays.
func TestFourthRoundForms(t *testing.T) {
	const v = "Qz77777xK9w"
	for _, sep := range []string{
		"::", ":::", "==", "=>", "->", "::=", "\uff1a\uff1a", "\u2237\u2237", "\u02d0\u02d0", "\x00:", "\v:",
		"\u00a0:", "\u3000:", "\u200b :", ":\u200b", ":\x00", ":\u3000", ":\u00a0",
	} {
		in := "userPassword" + sep + " " + v + "\nnext: ok"
		if got := String(in); strings.Contains(got, v) || !strings.HasPrefix(got, "userPassword") || !strings.HasSuffix(got, "\nnext: ok") {
			t.Errorf("String(%q) = %q", in, got)
		}
	}
	for _, in := range []string{
		"password\u200b : " + v, "password \u200b: " + v, `"token"` + "\u200b" + `: "` + v + `"`,
		"pass\u034fword: " + v, "pass\u3164word: " + v, "pass\ufe0fword: " + v, "userPassword:: bWFyaWdvbGQ9" + v,
	} {
		if got := String(in); strings.Contains(got, v) {
			t.Errorf("String(%q) = %q", in, got)
		}
	}
	if got := String("pass\u200bword: " + v + "\u200b\u200d"); !strings.HasPrefix(got, "pass\u200bword: [") || !strings.HasSuffix(got, "]") {
		t.Errorf("String = %q, want the value and the invisible characters after it in one marker", got)
	}
	if in, got := "<postgres://alice:mint>,<x@db/app>", String("<postgres://alice:mint>,<x@db/app>"); strings.Contains(got, "mint") {
		t.Errorf("String(%q) = %q", in, got)
	}
	for _, items := range [][]string{
		{"? password", ": " + v}, {"- name: DB_PASSWORD", "  value: " + v}, {`"password":`, "  " + v},
		{"--args=--client-secret", v}, {`"--password`, v}, {":--password", v}, {"|", "=--password", v},
		{passwordKey, "::", v}, {passwordKey, "=:", v}, {passwordKey, ": :", v},
	} {
		got := Strings(items)
		if strings.Contains(strings.Join(got, "\n"), v) {
			t.Errorf("Strings(%q) = %q", items, got)
		}
		if got[0] != items[0] {
			t.Errorf("Strings(%q) = %q: the key's string changed", items, got)
		}
	}
}

// What the classes must not take: a run of blanks alone after a key (a stated limit, as prose), colons and
// their look-alikes between digits or in a URL, and the punctuation of prose in other scripts. And on inputs cut
// anywhere, inside a character too, every redacted range lies inside the input, starts and ends on a character
// of a valid input, and nothing panics.
func TestFourthRoundKeepsText(t *testing.T) {
	for _, in := range []string{
		"userPassword reset failed", "userPassword\treset failed", "12\uff1a30", "3\u22364", "http\u02d0//example.com",
		"\u05e9\u05dc\u05d5\u05dd\u05c3 \u05e2\u05d5\u05dc\u05dd", "\u0532\u0561\u0580\u0565\u0582\u0589 \u0561\u0577\u056d\u0561\u0580\u0570",
		"Note\uff1a token expired",
	} {
		if got := String(in); got != in {
			t.Errorf("String(%q) = %q, want it unchanged", in, got)
		}
	}
	for _, sep := range canonSepFound {
		in := "token" + sep + " Qz12345xK9w\u200b"
		for cut := 1; cut <= len(in); cut++ {
			s := in[:cut]
			for _, sp := range defaultRedactor.spans(s) {
				if sp.start < 0 || sp.end > len(s) || sp.start >= sp.end {
					t.Fatalf("spans(%q) gave %d..%d", s, sp.start, sp.end)
				}
				if utf8.ValidString(s) && (!utf8.RuneStart(s[sp.start]) || sp.end < len(s) && !utf8.RuneStart(s[sp.end])) {
					t.Errorf("spans(%q) gave %d..%d, inside a character", s, sp.start, sp.end)
				}
			}
			_ = Strings([]string{"token", s, s})
		}
	}
}
