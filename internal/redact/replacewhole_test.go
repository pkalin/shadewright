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

import "testing"

// ReplaceWhole replaces what Clean's keep list takes as a whole occurrence, and nothing else: a key next to a
// token byte (a letter, a digit or one of "+/_-=.") is left as it is, the longer key wins at one offset.
func TestReplaceWhole(t *testing.T) {
	words := map[string]string{"incident-ab12": "<incident>", "incident-ab12-2": "<later>", "ab12": "<fp>"}
	for in, want := range map[string]string{
		"see incident-ab12 now":       "see <incident> now",
		"(incident-ab12)":             "(<incident>)",
		"\"incident-ab12\",":          "\"<incident>\",",
		"incident-ab12":               "<incident>",
		"incident-ab12-2 then":        "<later> then",
		"incident-ab12-3 then":        "incident-ab12-3 then",
		"incident-ab12.":              "incident-ab12.",
		"xincident-ab12":              "xincident-ab12",
		"ab12 and ab123 and 0ab12":    "<fp> and ab123 and 0ab12",
		"incident-ab12 incident-ab12": "<incident> <incident>",
		"nothing here":                "nothing here",
	} {
		if got := ReplaceWhole(in, words); got != want {
			t.Errorf("ReplaceWhole(%q) = %q, want %q", in, got, want)
		}
	}
}
