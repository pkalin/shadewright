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

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/shadewright-labs/shadewright/internal/analysis"
	"github.com/shadewright-labs/shadewright/internal/incidentcontext"
	"github.com/shadewright-labs/shadewright/internal/providers"
)

// promptGolden holds, per scenario whose Incident has an empty absorbed tally, the SHA-256 of the first model
// request's messages (the system prompt and the context, by role): the prompt as the model gets it. The values
// were computed by the code before the absorbed tally block (#796), so a scenario with an empty tally must
// still get the same prompt, byte for byte; unschedulable/over-bound (#841) pins its shortened summary,
// oom/lifetimes (#1005) pins its OOM lifetime summary line, and oom/crashloop-excerpt (#1055) pins its
// previous-instance log tail. A change that is meant to change the prompt of every Incident writes them again:
// SHADEWRIGHT_UPDATE_GOLDEN=1 go test -run TestPromptGolden ./test/eval/
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

// TestPromptShowsTheOOMLifetimes checks that oom/lifetimes exercises the OOM container lifetime summary line
// (#1005, ADR-0023 amendments of 2026-11-21 and 2026-11-26): the oom Incident with termination records
// (six with usable start times including one same-second kill, and one without a start time) replays its
// recorded reply and passes, its summary carries the exact lifetime line (unquotable), and the first prompt
// sent to the model carries the rendered line.
func TestPromptShowsTheOOMLifetimes(t *testing.T) {
	const wantLine = "age at kill (last 5, mixed across pods and revisions, oldest first): " +
		"12s, 20s, 15s, <1s, 30s; 1 other kill without a start time (of at most 32 recorded)\n"
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	var found *scenario
	for _, s := range scenarios {
		if s.name == scnOOM+"/lifetimes" {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("no scenario %s/lifetimes", scnOOM)
	}
	sum := summaryOf(t, found, incidentcontext.DefaultLimits())
	if strings.Count(sum.Text, wantLine) != 1 {
		t.Errorf("%s: the summary does not hold %q once:\n%s", found.name, wantLine, sum.Text)
	}
	for _, part := range sum.Data() {
		if strings.Contains(part, "age at kill") || strings.Contains(part, "<1s") ||
			strings.Contains(part, "without a start time") {
			t.Errorf("%s: the lifetime line is quotable: %q", found.name, part)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), scenarioTimeout)
	res, err := runScenario(ctx, scheme, found, replaySource{})
	cancel()
	if err != nil {
		t.Fatalf("%s: %v", found.name, err)
	}
	if !res.Pass {
		t.Errorf("%s: failed: outcome %+v, diagnosis %+v", found.name, res.Outcome, res.Diagnosis)
	}
	var prompt strings.Builder
	for _, m := range res.prompt {
		prompt.WriteString(m.Content)
	}
	renderedLine := incidentcontext.Neutralise(wantLine)
	if strings.Count(prompt.String(), renderedLine) != 1 {
		t.Errorf("%s: the prompt does not hold %q once:\n%s", found.name, renderedLine, prompt.String())
	}
}

// TestPromptShowsTheCrashLoopLastLine checks that oom/crashloop-excerpt exercises the crash-loop last-line
// preservation of #1055 on the triage path: the previous instance's log holds a single >24 KiB line with a
// run of "*" spanning the cut offset before the deciding cause. Whole-line tail cutting (origin/main's
// keepTail) drops that line and leaves an empty body; the one cut (internal/logcut) skips past the "*" run
// and keeps the tail
// with the cause quotable in the log block and present in the first prompt sent to triage, while the variant's
// refusal reply passes and a reply that misses the cause or proposes a memory increase fails.
func TestPromptShowsTheCrashLoopLastLine(t *testing.T) {
	const wantCause = "ConfigMap web-config mounted at /etc/web has no key cache.yaml; item cache has no size bound"
	scheme, found := loadCrashLoopExcerptVariant(t)
	checkCrashLoopLogBlock(t, found, wantCause)

	spy := &roleSpy{}
	ctx, cancel := context.WithTimeout(t.Context(), scenarioTimeout)
	res, err := runScenario(ctx, scheme, found, spy)
	cancel()
	if err != nil {
		t.Fatalf("%s: %v", found.name, err)
	}
	if !res.Pass || res.Escalated || len(spy.roles) != 1 || spy.roles[0] != analysis.RoleTriage {
		t.Fatalf("%s: pass %t, escalated %t, roles %v, outcome %+v, diagnosis %+v",
			found.name, res.Pass, res.Escalated, spy.roles, res.Outcome, res.Diagnosis)
	}
	var prompt strings.Builder
	for _, m := range res.prompt {
		prompt.WriteString(m.Content)
	}
	if strings.Count(prompt.String(), incidentcontext.Neutralise(wantCause)) != 1 {
		t.Errorf("%s: the first prompt does not hold %q once", found.name, wantCause)
	}
	// A reply without the log-derived fact fails diagnosis, and a memory proposal fails outcome.
	noLogReply := `{"summary":"The web container of Deployment demo/web is OOMKilled (exit code 137) ` +
		`and restarts in CrashLoopBackOff; logs are empty so the cause is unknown.",` +
		`"hypotheses":[{"description":"Memory exhaustion without log evidence."}],` +
		`"evidence":[{"source":"c2","quote":"OOMKilled"}],` +
		`"missingData":["Previous container log lines."],` +
		`"uncertainty":"No log lines were shown.","proposal":null}`
	noLogRes := run(t, scheme, withFinalReply(found, noLogReply))
	if noLogRes.Pass || noLogRes.Diagnosis.Pass {
		t.Errorf("%s: reply without the log cause passed: pass %t, diagnosis %+v",
			found.name, noLogRes.Pass, noLogRes.Diagnosis)
	}
	memPropRes := run(t, scheme, withFinalReply(found, memoryProposal(t, found, "48Mi", "64Mi")))
	if memPropRes.Pass || memPropRes.Outcome.Pass {
		t.Errorf("%s: memory proposal passed: pass %t, outcome %+v", found.name, memPropRes.Pass, memPropRes.Outcome)
	}
}

func loadCrashLoopExcerptVariant(t *testing.T) (*runtime.Scheme, *scenario) {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scenarios {
		if s.name == scnOOM+"/crashloop-excerpt" {
			return scheme, s
		}
	}
	t.Fatalf("no scenario %s/crashloop-excerpt", scnOOM)
	return nil, nil
}

func checkCrashLoopLogBlock(t *testing.T, found *scenario, wantCause string) {
	t.Helper()
	if found.logs == nil || len(found.logs.Excerpts) != 1 {
		t.Fatalf("%s: expected 1 recorded log excerpt, got %+v", found.name, found.logs)
	}
	rawLog := found.logs.Excerpts[0].Text
	def := incidentcontext.DefaultLimits()
	blocks := contextOf(t, found, def)
	var logBlk *incidentcontext.Block
	for i := range blocks {
		b := &blocks[i]
		if b.Section == incidentcontext.SectionLogs {
			logBlk = b
			continue
		}
		if strings.Contains(b.Text, "cache.yaml") || strings.Contains(b.Text, wantCause) {
			t.Errorf("%s: non-log section %q unexpectedly carries the deciding log cause", found.name, b.Section)
		}
	}
	if logBlk == nil {
		t.Fatalf("%s: context has no log block", found.name)
	}
	if logBlk.Availability != incidentcontext.Available {
		t.Fatalf("%s: log block availability %q, want %q", found.name, logBlk.Availability, incidentcontext.Available)
	}
	if !slices.Contains(logBlk.Labels, incidentcontext.LabelSectionCut) ||
		slices.Contains(logBlk.Labels, incidentcontext.LabelContextCut) {
		t.Errorf("%s: log block labels %v, want %q and not %q", found.name, logBlk.Labels,
			incidentcontext.LabelSectionCut, incidentcontext.LabelContextCut)
	}
	parts := logBlk.Data()
	if len(parts) != 1 {
		t.Fatalf("%s: log block has %d quotable parts, want 1", found.name, len(parts))
	}
	body := parts[0]
	if !strings.Contains(body, wantCause) {
		t.Errorf("%s: log block data does not hold %q", found.name, wantCause)
	}
	if strings.HasPrefix(body, "*") {
		t.Errorf("%s: log block tail starts inside the '*' run: %.40q", found.name, body)
	}
	// Prove whole-line tail cutting (origin/main's keepTail on the same body budget) dropped the line whole.
	headLen := len(logBlk.Text) - len(body)
	bodyBudget := def.Logs - headLen
	if len(rawLog) > def.Logs || len(rawLog) <= bodyBudget {
		t.Fatalf("%s: raw log length %d must be in (%d, %d] so replay returns it whole and section cut triggers",
			found.name, len(rawLog), bodyBudget, def.Logs)
	}
	if oldTail, oldCut := newestLines(rawLog, bodyBudget); !oldCut || oldTail != "" {
		t.Errorf("%s: whole-line tail cut on budget %d = (%d bytes, cut %t), want empty so origin/main drops the line",
			found.name, bodyBudget, len(oldTail), oldCut)
	}
}
