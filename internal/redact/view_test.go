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

package redact_test

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact"
)

// Strings decides on a sequence as a whole (#845): a string that ends with a secret key, a block indicator or
// "=", or is a secret flag alone, makes the next string its value; a reference flag, a bare short key with a
// word-like value and a key that is not secret keep it, and every other string is redacted as by String.
func TestStringsSeesAKeyAndItsValueInTwoStrings(t *testing.T) {
	const value = "s3cr3tValue" + "99"
	cases := []struct {
		in     []string
		secret int   // the index of the string that must lose value, -1 for none
		kept   []int // the indexes of the strings that must come out as they went in
	}{
		{[]string{"--password", value, "--port", "8080"}, 1, []int{0, 2, 3}},
		{[]string{"--password=", value}, 1, []int{0}},
		{[]string{"password: |", "  " + value}, 1, nil},
		{[]string{"token:", value, "region"}, 1, []int{2}},
		{[]string{"--password-file", value}, -1, []int{0, 1}},
		{[]string{"pass:", "words"}, -1, []int{1}},
		{[]string{"name:", value}, -1, []int{0, 1}},
		{[]string{"--password"}, -1, []int{0}},
	}
	for _, c := range cases {
		got := redact.Strings(c.in)
		if len(got) != len(c.in) {
			t.Fatalf("Strings(%q) = %q: %d strings, want %d", c.in, got, len(got), len(c.in))
		}
		if c.secret >= 0 && strings.Contains(got[c.secret], value) {
			t.Errorf("Strings(%q) = %q: the value stays", c.in, got)
		}
		if c.secret >= 0 && !strings.HasPrefix(got[c.secret], strings.TrimSuffix(c.in[c.secret], value)) {
			t.Errorf("Strings(%q) = %q: the indentation before the value is lost", c.in, got)
		}
		for _, i := range c.kept {
			if got[i] != c.in[i] {
				t.Errorf("Strings(%q)[%d] = %q, want it kept", c.in, i, got[i])
			}
		}
	}
}

// The forms a review of the second round found leaking (#845, third round), each also on main: Strings reads
// each string in its folded view, a format character between a key and its colon folds away, every colon
// look-alike of the confusables list folds to ":", and a key written as a block scalar below "? |" reads as
// the key.
func TestTheFoldedViewHidesTheReportedForms(t *testing.T) {
	const value = "s3cr3tValue" + "99"
	for _, in := range [][]string{
		{"--pass\u00adword", value},
		{"pass\u200bword=", value},
		{"password\uff1a", value},
		{"password", "\uff1a", value},
		{"api_key\u200b:", value},
	} {
		got := redact.Strings(in)
		if strings.Contains(got[len(got)-1], value) {
			t.Errorf("Strings(%q) = %q: the value stays", in, got)
		}
		for i := range len(in) - 1 {
			if got[i] != in[i] {
				t.Errorf("Strings(%q)[%d] = %q, want it kept", in, i, got[i])
			}
		}
	}
	for _, in := range []string{
		"api_key\u200b: " + value,
		"password\u02d0 " + value,
		"password\u02f8 " + value,
		"password\u0589 " + value,
		"password\u05c3 " + value,
		"password\u0703 " + value,
		"password\u1361 " + value,
		"password\u2e2c " + value,
		"? |\n  password\n: " + value,
		"? |-\n  password\n\n: " + value,
		"spec:\n  ? >\n    password\n  : " + value,
	} {
		if got := redact.String(in); strings.Contains(got, value) {
			t.Errorf("String(%q) = %q: the value stays", in, got)
		}
	}
	// Not a key below "? |": two lines of key, or ":" at another column.
	for _, in := range []string{
		"? |\n  pass\n  word\n: " + value,
		"? |\n  password\n  : " + value,
	} {
		if got := redact.String(in); got != in {
			t.Errorf("String(%q) = %q, want it kept", in, got)
		}
	}
}
