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
