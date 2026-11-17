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

package incidentcontext

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact"
)

// A value from the cluster is redacted while it still has its lines, and only then are its line breaks turned
// into blanks (#840, decided 2026-11-17). The inputs hold a value the redactor finds only by its layout, so
// the test fails if the order is ever swapped.
func TestValueIsRedactedBeforeItIsFlattened(t *testing.T) {
	for in, secret := range map[string]string{
		"Back-off restarting\ncredentials:\n  user: admin\n":         "admin",
		"Back-off restarting\npassword: hunter2xyzAB\n  Tr0ub4d0r\n": "Tr0ub4d0r",
	} {
		flat := strings.ReplaceAll(in, "\n", " ")
		if !strings.Contains(redact.String(flat), secret) {
			t.Fatalf("%q: the flattened text loses %q too, so this input proves nothing about the order", in, secret)
		}
		if got := Value(redact.String, in); strings.Contains(got, secret) || strings.Contains(got, "\n") {
			t.Errorf("Value(%q) = %q, leaks %q or keeps a line break", in, got, secret)
		}
	}
}

// Redact shows the redactor the text as it was read, before an invalid byte becomes "?" (#840). A "?" ends the
// user information of a URL, so replacing the byte first hides the password from the connection string
// rule; the test fails if the replacement runs first again.
func TestRedactSeesInvalidBytesBeforeTheyAreReplaced(t *testing.T) {
	for in, secret := range map[string]string{
		"dial https://admin:Hunter\xff2Secret@db.shop.svc:5432/x": "Hunter",
		"postgres://app:pw\xff@db/x":                              "pw",
	} {
		if !strings.Contains(redact.String(strings.ToValidUTF8(in, "?")), secret) {
			t.Fatalf("%q: replacing first loses %q too, so this input proves nothing about the order", in, secret)
		}
		if got := Redact(redact.String, in); strings.Contains(got, secret) {
			t.Errorf("Redact(%q) = %q, leaks %q", in, got, secret)
		}
		if got := Value(redact.String, in); strings.Contains(got, secret) {
			t.Errorf("Value(%q) = %q, leaks %q", in, got, secret)
		}
	}
}

// A symptom an Incident stores is one line: a block scalar a source flattened onto it before the collector
// saw it is redacted by the one-line rule (#840). The summary's cut swept across each form, the key cut by
// the note included, never shows a part of the secret, and the note stays intact.
func TestSummaryCutAcrossAFlattenedBlock(t *testing.T) {
	const secret = "Qz10001xK9wQz" // #nosec G101 -- not a credential: a made-up value the test looks for
	b, ub := newBuilder(t, reader(t), sources{}, nil), unbounded(t, nil)
	for name, text := range map[string]string{
		"flattened":      "é config password: |   " + secret,
		"no blank":       "é config password:| " + secret,
		"json string":    `é {"msg":"bad config: password: >- ` + secret + `","ts":"t0"}`,
		"after bracket":  "é retry [3/5 token: |2 " + secret,
		"bare short key": "é auth: | " + secret,
	} {
		sweepCut(t, b, ub, name, text, secret, bothTally(), bothBlock)
		sweepCut(t, b, ub, name+"/no tally", text, secret, nil, "")
	}
}
