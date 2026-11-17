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

package eval

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// promptGolden holds, per scenario whose Incident has an empty absorbed tally, the SHA-256 of the first model
// request's messages (the system prompt and the context, by role): the prompt as the model gets it. The values
// were computed by the code before the absorbed tally block (#796), so a scenario with an empty tally must
// still get the same prompt, byte for byte. A change that is meant to change the prompt of every Incident
// writes them again: SHADEWRIGHT_UPDATE_GOLDEN=1 go test -run TestPromptGolden ./test/eval/
var promptGolden = filepath.Join("testdata", "prompt.golden")

// promptDigest hashes messages: each one's role and content, length-prefixed so no two lists hash alike.
func promptDigest(messages []providers.Message) string {
	h := sha256.New()
	for _, m := range messages {
		_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(m.Role), m.Role, len(m.Content), m.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestPromptGolden checks that the first prompt of every scenario with an empty absorbed tally is the one
// recorded in testdata/prompt.golden.
func TestPromptGolden(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range scenarios {
		if len(s.incident.Status.Absorbed) > 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(t.Context(), scenarioTimeout)
		res, err := runScenario(ctx, scheme, s, replaySource{})
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if len(res.prompt) == 0 {
			t.Fatalf("%s: no model request", s.name)
		}
		got[s.name] = promptDigest(res.prompt)
	}
	if os.Getenv("SHADEWRIGHT_UPDATE_GOLDEN") == "1" {
		names := slices.Sorted(maps.Keys(got))
		var b strings.Builder
		for _, n := range names {
			fmt.Fprintf(&b, "%s %s\n", n, got[n])
		}
		if err := os.WriteFile(promptGolden, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want := readPromptGolden(t)
	for name, d := range want {
		g, ok := got[name]
		switch {
		case !ok:
			t.Errorf("%s: in %s but not a scenario with an empty absorbed tally", name, promptGolden)
		case g != d:
			t.Errorf("%s: the prompt changed: sha256 %s, golden %s", name, g, d)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: a scenario with an empty absorbed tally that %s does not list", name, promptGolden)
		}
	}
}

func readPromptGolden(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(promptGolden)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		name, digest, ok := strings.Cut(s.Text(), " ")
		if !ok {
			t.Fatalf("%s: malformed line %q", promptGolden, s.Text())
		}
		out[name] = digest
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPromptShowsTheAbsorbedTally is the recorded scenario the evaluation gate of ADR-0025 asks for before the
// absorbed tally reaches the model (ADR-0039 point 18, #796): oom/absorbed, the oom Incident with its tally,
// replays its recorded reply and passes, and its prompt holds one line per entry of the tally.
func TestPromptShowsTheAbsorbedTally(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range scenarios {
		if len(s.incident.Status.Absorbed) == 0 {
			continue
		}
		found = found || s.name == scnOOM+"/absorbed"
		ctx, cancel := context.WithTimeout(t.Context(), scenarioTimeout)
		res, err := runScenario(ctx, scheme, s, replaySource{})
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if !res.Pass {
			t.Errorf("%s: failed: outcome %+v, diagnosis %+v", s.name, res.Outcome, res.Diagnosis)
		}
		var prompt strings.Builder
		for _, m := range res.prompt {
			prompt.WriteString(m.Content)
		}
		for _, a := range s.incident.Status.Absorbed {
			line := fmt.Sprintf("  %s: %d occurrences, last seen %s\n", a.Reason, a.Count,
				a.LastSeen.UTC().Format(time.RFC3339))
			if strings.Count(prompt.String(), line) != 1 {
				t.Errorf("%s: the prompt does not hold %q once:\n%s", s.name, line, prompt.String())
			}
		}
	}
	if !found {
		t.Errorf("no scenario %s/absorbed", scnOOM)
	}
}
