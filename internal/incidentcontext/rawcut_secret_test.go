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

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/fake"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

// No partial line of a cut made before redaction reaches redaction or the model (ADR-0034: a cut before
// redaction could keep the tail of a secret). Each secret is matched only whole (a bearer token after its
// scheme, a token with its prefix, a password after its key), and every bound from the whole log down to one byte
// is tried, so the cut boundary falls on each byte of it. The paths: the fake sources' cut (fake.CutLog, which
// test/eval's source also calls) followed by RenderLog, as a context build and get_pod_logs run them; and
// RenderLog's own cut of a whole answer, which runs on redacted text. kube.Excerpt has the same check in its
// package (TestRawCutInsideASecretShowsNoFragment).
func TestRawCutsShowNoSecretFragment(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	jwt := "eyJhbGciOiJIUzI1NiJ9" + "." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0" + "." + "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PSR5wHnYp3qk"
	pat := "ghp_" + strings.Repeat("Q7", 18)
	const title = "pod web-abc-1 container app, previous instance"
	check := func(path string, keep int, got, secret string) {
		t.Helper()
		for i := 0; i+6 <= len(secret); i++ {
			if strings.Contains(got, secret[i:i+6]) {
				t.Fatalf("%s keep %d: fragment %q of the secret in %q", path, keep, secret[i:i+6], got)
			}
		}
	}
	for name, tc := range map[string]struct{ line, secret string }{
		"bearer":            {"Authorization: Bearer " + jwt, jwt},
		"github pat":        {"using token " + pat + " for the clone", pat},
		"connection string": {"Server=db;Database=app;User Id=admin;Password=S3cr3tPassw0rd;", "S3cr3tPassw0rd"},
	} {
		for _, data := range []string{tc.line + "\n", "starting\n" + tc.line + "\n", "starting\n" + tc.line + "\nready\n"} {
			for keep := 1; keep <= len(data); keep++ {
				ex, err := fake.CutLog(data, keep)
				if err != nil {
					t.Fatalf("%s keep %d: %v", name, keep, err)
				}
				ex.CollectedAt = testNow
				for _, refit := range []bool{false, true} {
					v := RenderLog(red, refit, title, ex, nil, keep, 4<<10)
					check(name+" fake source", keep, v.Text, tc.secret)
				}
				v := RenderLog(red, false, title, providers.LogExcerpt{Text: data, CollectedAt: testNow}, nil, keep, 4<<10)
				check(name+" RenderLog", keep, v.Text, tc.secret)
			}
		}
	}
}
