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
)

// In a sequence, the string after a secret flag is its value whatever it looks like (#845, third decision of
// 2026-11-18, which corrects the second): ["--token", "--verbose"] hides "--verbose", as main's second pass over
// a tool's whole answer hid it. A string that is no flag is the value too. The inputs are built from the parts
// below, so the answer is known by construction.

// flagNames and flagValues make the flags that stand after a secret flag: a name, with "=" and a value or
// without.
var (
	flagNames  = []string{"verbose", "v", "port", "log-level", "x", "dry-run", "n", "insecure-skip-tls-verify"}
	flagValues = []string{"", "=8080", "=info", "=true", "=a=b"}
)

// flagAfterFlagCases returns the generated sequences: a secret flag (with an argument glued in front or not),
// a flag of flagNames, and a third string that is a secret after a secret flag, a plain word, or nothing.
func flagAfterFlagCases() (cases [][]string, flags []string) {
	for _, key := range canonFlagKeys {
		for _, front := range []string{"", "app "} {
			for _, dashes := range []string{"-", "--"} {
				for _, name := range flagNames {
					for _, v := range flagValues {
						flag := dashes + name + v
						cases = append(cases, []string{front + key, flag})
						flags = append(flags, flag)
					}
				}
			}
		}
	}
	return cases, flags
}

func TestStringsTakesAFlagAfterAFlagAsItsValue(t *testing.T) {
	cases, flags := flagAfterFlagCases()
	failed := 0
	for i, items := range cases {
		got := Strings(items)
		if got[0] != items[0] || strings.Contains(got[1], flags[i]) || strings.ContainsAny(got[1], "=-") {
			if failed++; failed <= 10 {
				t.Errorf("Strings(%q) = %q: the flag after the secret flag is not hidden whole", items, got)
			}
		}
		// A secret flag after the flag takes the string after it, and a secret after the first flag is hidden.
		v := fmt.Sprintf("Qz%05dxK9w", 50000+i)
		for _, seq := range [][]string{{items[0], items[1], canonFlagKeys[i%len(canonFlagKeys)], v}, {items[0], v}} {
			out := Strings(seq)
			if strings.Contains(out[len(out)-1], v) {
				failed++
				t.Errorf("Strings(%q) = %q shows the secret", seq, out)
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d sequences fail", failed, len(cases))
	}
	t.Logf("%d sequences of a flag after a secret flag", len(cases))
}
