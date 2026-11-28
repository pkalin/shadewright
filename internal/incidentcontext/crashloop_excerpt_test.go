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
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/fake"
	"github.com/shadewright-labs/shadewright/internal/providers/logs/kube"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

const crashLoopUnmarkedLine = "flag provided but not defined: -retries-per-window"

// noisyCrashLog builds a log with roughly bytes of noisy lines carrying levels and keywords, followed by
// trailer (which may be empty).
func noisyCrashLog(bytes int, trailer string) string {
	const line = "2026-11-26T12:00:00Z ERROR WARN FATAL panic: noisy startup line with keywords\n"
	n := max(1, bytes/len(line))
	return strings.Repeat(line, n) + trailer
}

// Candidate (a): does line selection prefer lines with levels/keywords and spend the budget before the tail?
// Expected on main: passes (ruled out: RenderLog/keepTail keeps the newest whole lines regardless of wording).
func TestCrashLoopExcerptCandidateA_NoisyKeywordsBeforeUnmarkedTail(t *testing.T) {
	l := &instanceLogs{
		previous: noisyCrashLog(DefaultLogBytes+8<<10, crashLoopUnmarkedLine+"\n"),
	}
	c := buildWith(t, func(p *corev1.Pod) *corev1.Pod {
		p = crashing(p, "exited")
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 2
		return p
	}, l)
	logs := one(t, c, SectionLogs)
	if !strings.Contains(logs.Text, crashLoopUnmarkedLine) {
		t.Fatalf("unmarked last line missing from previous instance excerpt:\n%s", logs.Text)
	}
	if len(logs.Text) > DefaultLogBytes {
		t.Errorf("log section %d bytes, over bound %d", len(logs.Text), DefaultLogBytes)
	}
}

// Candidate (b1): is a failed first read of the previous instance treated as empty / "no log"?
// Expected on main: passes (ruled out: RenderLog marks a failed read "could not fetch" and CouldNotFetch).
func TestCrashLoopExcerptCandidateB1_FailedFirstReadIsVisibleNotEmpty(t *testing.T) {
	l := &instanceLogs{
		previousErr: &providers.RetryableError{Err: errors.New("logs request: upstream 502")},
	}
	c := buildWith(t, func(p *corev1.Pod) *corev1.Pod {
		p = crashing(p, "exited")
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 2
		return p
	}, l)
	logs := one(t, c, SectionLogs)
	if logs.Availability != CouldNotFetch {
		t.Errorf("availability %q, want %q", logs.Availability, CouldNotFetch)
	}
	if !strings.Contains(logs.Text, "previous instance\ncould not fetch\n") {
		t.Errorf("failed previous read not visible as could not fetch:\n%s", logs.Text)
	}
	if strings.Contains(logs.Text, "no log") {
		t.Errorf("failed previous read shown as no log:\n%s", logs.Text)
	}
}

// Candidate (b2): when the section time budget runs out before a container's second read (the previous
// instance of an exited container), is the skipped previous-instance read visible as "could not fetch" rather
// than silently omitted?
func TestCrashLoopExcerptCandidateB2_SecondReadSkippedOnTimeBudgetIsVisible(t *testing.T) {
	r, _ := underLoad(t, func(k time.Duration) (timedRun, bool) {
		l := &timedLogs{
			plan:    map[string][2]outcome{crashPod: {slow, hangs}, secondPod: {slow, answers}},
			slowFor: 240 * time.Millisecond * k,
		}
		c, _, _, reached := timedBuildOf(t, bothExited(), l, 400*time.Millisecond*k, 10*time.Second*k)
		return timedRun{c: c, l: l}, reached
	})
	var secondText strings.Builder
	for _, blk := range blocks(r.c, SectionLogs) {
		if strings.Contains(blk.Text, secondPod) {
			secondText.WriteString(blk.Text)
		}
	}
	want := "pod " + secondPod + " container " + appName + ", previous instance\ncould not fetch\n"
	if !strings.Contains(secondText.String(), want) {
		t.Fatalf("second pod's skipped previous read is not visible as could not fetch:\n%s", secondText.String())
	}
}

// Candidate (c1): when a crash-looping container is seen with State.Terminated != nil (currentDead) and its
// current instance fills the container's byte budget with noisy lines, does the excerpt still include the
// previous instance's unmarked last line?
func TestCrashLoopExcerptCandidateC1_CurrentDeadSpendsByteBudgetBeforePrevious(t *testing.T) {
	l := &instanceLogs{
		current:  noisyCrashLog(DefaultLogBytes+8<<10, ""),
		previous: noisyCrashLog(8<<10, crashLoopUnmarkedLine+"\n"),
	}
	c := buildWith(t, func(p *corev1.Pod) *corev1.Pod {
		p = exited(p)
		p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 2
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 2
		return p
	}, l)
	var combined strings.Builder
	for _, blk := range blocks(c, SectionLogs) {
		combined.WriteString(blk.Text)
	}
	if !strings.Contains(combined.String(), crashLoopUnmarkedLine) {
		t.Fatalf("previous instance's unmarked last line missing when currentDead fills budget; reads=%d", len(l.asked))
	}
	if combined.Len() > DefaultLogBytes {
		t.Errorf("log section %d bytes, over bound %d", combined.Len(), DefaultLogBytes)
	}
}

// Candidate (c2): when both current and previous excerpts of an exited container are in the context and
// fitTotal trims SectionLogs to meet Limits.Total, does the previous instance's unmarked last line survive
// before current's noisy bulk?
func TestCrashLoopExcerptCandidateC2_TotalCutDropsPreviousBeforeCurrent(t *testing.T) {
	l := &instanceLogs{
		current:  noisyCrashLog(6<<10, ""),
		previous: noisyCrashLog(2<<10, crashLoopUnmarkedLine+"\n"),
	}
	b := newBuilder(t, reader(t, objectsWith(func(p *corev1.Pod) *corev1.Pod {
		p = exited(p)
		p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 2
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 2
		return p
	})...), sources{}, func(c *Config) { c.Logs = l })
	// Total budget large enough for summary + status + headers + previous tail, but smaller than all of current + previous.
	c, err := b.BuildWithin(t.Context(), incident(deploymentRef()), 5<<10)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(c.Render()); got > 5<<10 {
		t.Fatalf("rendered %d bytes, over 5 KiB bound", got)
	}
	var combined strings.Builder
	for _, blk := range blocks(c, SectionLogs) {
		combined.WriteString(blk.Text)
	}
	if !strings.Contains(combined.String(), crashLoopUnmarkedLine) {
		t.Fatalf("total cut dropped previous instance's unmarked last line before current's noise:\n%s", combined.String())
	}
}

// Candidate (d1): when a container is waiting in CrashLoopBackOff with LastTerminationState.Terminated whose
// FinishedAt is zero or before the window start, or with LastTerminationState.Terminated absent and
// RestartCount > 0, does the excerpt still read the previous instance and include its unmarked last line?
func TestCrashLoopExcerptCandidateD1_CrashLoopBackOffWithoutRecentFinishedAt(t *testing.T) {
	cases := map[string]func(*corev1.Pod) *corev1.Pod{
		"zero finishedAt": func(p *corev1.Pod) *corev1.Pod {
			p = crashing(p, "back-off restarting failed container")
			p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 2
			p.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = metav1.Time{}
			return p
		},
		"pre-window finishedAt": func(p *corev1.Pod) *corev1.Pod {
			p = crashing(p, "back-off restarting failed container")
			p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 2
			p.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = longAgo
			return p
		},
		"nil lastTermination with restarts": func(p *corev1.Pod) *corev1.Pod {
			p = crashing(p, "back-off restarting failed container")
			p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
			p.Status.ContainerStatuses[0].RestartCount = 2
			return p
		},
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			l := &instanceLogs{
				current:  noisyCrashLog(2<<10, ""),
				previous: noisyCrashLog(2<<10, crashLoopUnmarkedLine+"\n"),
			}
			c := buildWith(t, mod, l)
			var combined strings.Builder
			for _, blk := range blocks(c, SectionLogs) {
				combined.WriteString(blk.Text)
			}
			if !strings.Contains(combined.String(), crashLoopUnmarkedLine) {
				t.Fatalf("%s: previous instance's unmarked last line missing; reads=%+v", name, l.asked)
			}
		})
	}
}

// Candidate (d2): when a first read gives no lines with a long header leaving too little room for the second
// read's "could not fetch" notice, is the failed previous-instance read still visible in the context?
func TestCrashLoopExcerptCandidateD2_FailedSecondReadNoticeFitsRemainingBudget(t *testing.T) {
	const budget = MinSectionBytes / MaxLogExcerpts
	const notice = "pod " + crashPod + " container " + appName + ", previous instance\ncould not fetch\n"
	logs := &fake.Logs{Now: func() time.Time { return testNow }}
	key := fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName}
	logs.Set(key, "")
	logs.SetInstance(key, providers.LogInstance{ContainerID: strings.Repeat("test-id.", 44)}, false)
	b := newBuilder(t, reader(t), sources{}, func(c *Config) {
		c.Logs = logFunc(func(ctx context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
			if q.Previous {
				return providers.LogExcerpt{}, &providers.RetryableError{Err: errors.New("upstream 503")}
			}
			return logs.Logs(ctx, q)
		})
	})
	r := containerReads{pick: logPick{namespace: testNS, pod: crashPod, container: appName, other: true}, left: budget}
	if err := b.readInstance(t.Context(), &r, false, DefaultWindow); err != nil {
		t.Fatal(err)
	}
	if !r.wantsSecond() || r.left >= len(notice) {
		t.Fatalf("fixture left %d bytes for a %d-byte notice", r.left, len(notice))
	}
	if err := b.readInstance(t.Context(), &r, true, DefaultWindow); err != nil {
		t.Fatal(err)
	}
	blk := r.blocks()[0]
	if !strings.Contains(blk.Text, notice) || len(blk.Text) > budget {
		t.Fatalf("failed second read notice missing or over budget (%d/%d):\n%q", len(blk.Text), budget, blk.Text)
	}
}

// Redaction still runs before every cut (the one cut and fitTotal's lastNLines floor) on the previous instance's
// ending lines, so a secret on or above the unmarked last line never leaks into the context.
func TestCrashLoopPreviousTailPassesThroughRedaction(t *testing.T) {
	secretPass := "Kx9-" + "synthetic-" + "CrashPw" // #nosec G101 -- not a credential: a synthetic value for redaction check
	dsn := "postgres://app:" + secretPass + "@db.shop.svc:5432/orders"
	l := &instanceLogs{
		current:  noisyCrashLog(DefaultLogBytes+8<<10, ""),
		previous: noisyCrashLog(4<<10, "Authorization: Bearer "+leakJWT+"\n"+crashLoopUnmarkedLine+" "+dsn+"\n"),
	}
	// Sanity check: without redaction, the synthetic secrets appear in the previous-instance tail.
	bRaw := newBuilder(t, reader(t, objectsWith(func(p *corev1.Pod) *corev1.Pod {
		p = exited(p)
		p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 2
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 2
		return p
	})...), sources{}, func(c *Config) {
		c.Logs = l
		c.Redactor = noRedaction{}
	})
	rawCtx, err := bRaw.BuildWithin(t.Context(), incident(deploymentRef()), 5<<10)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{leakJWT, secretPass} {
		if !strings.Contains(rawCtx.Render(), secret) {
			t.Fatalf("without redaction, secret %q did not reach previous-instance tail", secret)
		}
	}

	b := newBuilder(t, reader(t, objectsWith(func(p *corev1.Pod) *corev1.Pod {
		p = exited(p)
		p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 2
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 2
		return p
	})...), sources{}, func(c *Config) { c.Logs = l })
	c, err := b.BuildWithin(t.Context(), incident(deploymentRef()), 5<<10)
	if err != nil {
		t.Fatal(err)
	}
	rendered := c.Render()
	if !strings.Contains(rendered, crashLoopUnmarkedLine) {
		t.Fatalf("unmarked last line missing from rendered context:\n%s", rendered)
	}
	for _, secret := range []string{leakJWT, secretPass} {
		if strings.Contains(rendered, secret) {
			t.Errorf("secret %q leaked into previous-instance tail:\n%s", secret, rendered)
		}
	}
}

func TestLastNLines(t *testing.T) {
	const twoLines = "a\nb\n"
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"", 3, ""},
		{twoLines, 0, ""},
		{twoLines, -1, ""},
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{twoLines, 5, twoLines},
		{"onlyone", 1, "onlyone"},
		{"\n", 1, "\n"},
	} {
		if got := lastNLines(tc.in, tc.n); got != tc.want {
			t.Errorf("lastNLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// Review finding Medium 1: exact field-report shape (CrashLoopBackOff with a recent finishedAt, a noisy
// previous log and a normal-length unmarked last line). This already passes on main because logPicks chooses
// the previous instance first with no window when finishedAt is inside the window, so the exact cause of the
// field report is not proven by this case.
func TestCrashLoopExcerptFieldShapeCLBORecentFinishedAt(t *testing.T) {
	l := &instanceLogs{
		current:  noisyCrashLog(DefaultLogBytes, ""),
		previous: noisyCrashLog(DefaultLogBytes+8<<10, crashLoopUnmarkedLine+"\n"),
	}
	c := buildWith(t, func(p *corev1.Pod) *corev1.Pod {
		p = crashing(p, "back-off restarting failed container")
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 2
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = recent
		return p
	}, l)
	if len(l.asked) != 1 || !l.asked[0].Previous || l.asked[0].Since != 0 {
		t.Fatalf("reads = %+v, want one previous-instance read with no window", l.asked)
	}
	logs := one(t, c, SectionLogs)
	if !strings.Contains(logs.Text, crashLoopUnmarkedLine) {
		t.Fatalf("unmarked last line missing:\n%s", logs.Text)
	}
}

// Review finding Medium 2: a previous-instance last line longer than the reserved 2 KiB budget is cut at a
// UTF-8 rune boundary, marked as cut, stays within the section cap, and is redacted before the cut so a
// secret spanning the cut boundary or inside the kept tail never survives in whole or in part.
func TestCrashLoopExcerptLongLastLineKeptAtRuneBoundaryAndRedacted(t *testing.T) {
	// Build synthetic secrets by concatenation so no key-shaped literal appears in source.
	boundaryPart1 := "BoundaryPartOne"
	boundaryPart2 := "SecretVal99"
	boundarySecret := boundaryPart1 + boundaryPart2
	boundaryDSN := "postgres://app:" + boundarySecret + "@db.shop.svc:5432/orders"
	tailPass := "Tail" + "SecretVal88"
	tailDSN := "postgres://worker:" + tailPass + "@db.shop.svc:5432/orders"

	// Construct a ~3 KiB single line (<= 2*previousReserve so RenderLog does not reject it as oversized)
	// where boundaryDSN sits across the 2 KiB cut boundary (its scheme before the cut, its password tail
	// after the cut), followed by multi-byte UTF-8 runes, a second secret in the kept suffix, and the
	// unmarked exit text at the end. Redaction replaces each DSN in place without swallowing the rest of
	// the line, so the redacted line still exceeds the 2 KiB reserve and must be cut on a rune boundary.
	// If the cut ran before redaction, "postgres://app:" would be cut off and boundaryPart2 would survive.
	longPrevLine := strings.Repeat("語", 350) + " " + boundaryDSN + " " +
		strings.Repeat("🚀", 442) + " " + tailDSN + " " + crashLoopUnmarkedLine + "\n"

	// The source honours the read size as the production source does: without redaction it only cuts
	// (sourceCut), with it it redacts the whole read, then cuts (kube.Excerpt).
	source := func(red *redact.Redactor) providers.LogSource {
		return logFunc(func(_ context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
			text := noisyCrashLog(DefaultLogBytes+8<<10, "")
			if q.Previous {
				text = longPrevLine
			}
			if red == nil {
				text, truncated := sourceCut(text, q.MaxBytes)
				return providers.LogExcerpt{Text: text, Truncated: truncated, CollectedAt: testNow}, nil
			}
			text, truncated, err := kube.Excerpt(red, []byte(text), false, int(q.MaxBytes))
			return providers.LogExcerpt{Text: text, Truncated: truncated, CollectedAt: testNow}, err
		})
	}
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}

	exitedMod := func(p *corev1.Pod) *corev1.Pod {
		p = exited(p)
		p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 2
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 2
		return p
	}

	// Sanity check: without redaction, both synthetic secrets (or the boundary secret's tail) appear when
	// the long line is kept, proving the real-redactor check below exercises redaction.
	bRaw := newBuilder(t, reader(t, objectsWith(exitedMod)...), sources{}, func(c *Config) {
		c.Logs = source(nil)
		c.Redactor = noRedaction{}
	})
	rawCtx := build(t, bRaw, incident(deploymentRef()))
	rawBlks := blocks(rawCtx, SectionLogs)
	if len(rawBlks) != 2 {
		t.Fatalf("raw: got %d log blocks, want 2", len(rawBlks))
	}
	if !strings.Contains(rawBlks[1].Text, boundaryPart2) || !strings.Contains(rawBlks[1].Text, tailPass) {
		t.Fatalf("raw previous block missing boundary or tail secret (body dropped or boundary offset missed):\n%s", rawBlks[1].Text)
	}
	// Real redactor path: redaction runs before the rune-boundary cut.
	b := newBuilder(t, reader(t, objectsWith(exitedMod)...), sources{}, func(c *Config) {
		c.Logs = source(red)
	})
	c := build(t, b, incident(deploymentRef()))
	blks := blocks(c, SectionLogs)
	if len(blks) != 2 {
		t.Fatalf("got %d log blocks, want 2", len(blks))
	}
	prev := blks[1]
	if prev.Availability != Available {
		t.Errorf("previous block availability = %q, want %q", prev.Availability, Available)
	}
	if !hasLabel(prev, LabelSectionCut) {
		t.Errorf("previous block missing %q label: %v", LabelSectionCut, prev.Labels)
	}
	if !utf8.ValidString(prev.Text) {
		t.Errorf("previous block is not valid UTF-8")
	}
	if logBytes := len(blks[0].Text) + len(blks[1].Text); logBytes > DefaultLogBytes {
		t.Errorf("log section %d bytes, over cap %d", logBytes, DefaultLogBytes)
	}
	if !strings.Contains(prev.Text, crashLoopUnmarkedLine) {
		t.Errorf("previous block lost unmarked tail of long last line:\n%s", prev.Text)
	}
	rendered := c.Render()
	for _, secret := range []string{boundarySecret, boundaryPart2, tailPass} {
		if strings.Contains(rendered, secret) {
			t.Errorf("secret %q survived in rendered context:\n%s", secret, rendered)
		}
	}

	// Check every byte offset across a 4-byte rune boundary in RenderLog.
	for delta := range 4 {
		budget := 1024 - delta
		ex := providers.LogExcerpt{Text: strings.Repeat("🚀", 400) + crashLoopUnmarkedLine + "\n", CollectedAt: testNow}
		v := RenderLog(noRedaction{}, false, "pod web-abc-1 container app, previous instance", ex, nil, 1500, budget)
		if !v.Cut {
			t.Errorf("delta %d: Cut = false, want true", delta)
		}
		if len(v.Text) > budget {
			t.Errorf("delta %d: len = %d, over budget %d", delta, len(v.Text), budget)
		}
		if !utf8.ValidString(v.Text) {
			t.Errorf("delta %d: invalid UTF-8", delta)
		}
		if !strings.HasSuffix(v.Text, crashLoopUnmarkedLine+"\n") {
			t.Errorf("delta %d: missing tail:\n%q", delta, v.Text)
		}
	}

	// Also verify fitTotal keeps the rune-boundary tail of a long last line under a tight total bound.
	cTight, err := b.BuildWithin(t.Context(), incident(deploymentRef()), MinTotalBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cTight.Render()); got > MinTotalBytes {
		t.Errorf("tight build rendered %d bytes, over %d", got, MinTotalBytes)
	}
	tightRendered := cTight.Render()
	if !utf8.ValidString(tightRendered) {
		t.Errorf("tight build produced invalid UTF-8")
	}
	if !strings.Contains(tightRendered, crashLoopUnmarkedLine) {
		t.Errorf("tight build dropped long last line's unmarked tail:\n%s", tightRendered)
	}
	for _, secret := range []string{boundarySecret, boundaryPart2, tailPass} {
		if strings.Contains(tightRendered, secret) {
			t.Errorf("tight build leaked secret %q", secret)
		}
	}
}

// Review finding Low 2: a healthy running container with RestartCount > 0 and no LastTerminationState does
// not trigger an extra read of the previous instance when its current log is empty.
func TestHealthyRestartedContainerWithNoLastTerminationDoesNotReadPrevious(t *testing.T) {
	l := &instanceLogs{}
	c := buildWith(t, func(p *corev1.Pod) *corev1.Pod {
		p.Status.ContainerStatuses[0].RestartCount = 2
		p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
		return p
	}, l)
	if len(l.asked) != 1 || l.asked[0].Previous {
		t.Fatalf("reads = %+v, want 1 read of current instance only", l.asked)
	}
	if logs := one(t, c, SectionLogs); !strings.HasSuffix(logs.Text, logEmpty+"\n") {
		t.Errorf("unexpected log block:\n%s", logs.Text)
	}
}

// Review finding Low 4: when a previous-instance block's 10-line floor exceeds what can fit alongside earlier
// log blocks' headers, Pass 1 caps the floor to what can actually fit so earlier log blocks keep their
// headers instead of being crushed to empty requiredText.
func TestFitTotalOversizedPreviousFloorKeepsEarlierHeader(t *testing.T) {
	const curHead = "pod web-abc-1 container app, current instance\n"
	const prevHead = "pod web-abc-1 container app, previous instance\n"
	longLine := strings.Repeat("x", 349) + "\n"
	c := &Context{Blocks: []Block{
		{Section: SectionSummary, Provenance: ProvenanceIncident, Availability: Available, Text: "incident summary\n"},
		{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available,
			Text:    curHead + strings.Repeat("c", 99) + "\n" + strings.Repeat("c", 99) + "\n" + strings.Repeat("c", 1800) + "\n",
			headLen: len(curHead), container: 1},
		{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available,
			Text:    prevHead + strings.Repeat(longLine, 10) + crashLoopUnmarkedLine + "\n",
			headLen: len(prevHead), previous: true, container: 1},
	}}
	// Budget leaves room for summary + both wrappers + both headers + ~1.5 KiB of previous tail, which is
	// less than previous's 10-line floor (~3.5 KiB).
	summaryLen := renderedBlockLen(0, c.Blocks[0])
	limit := len(ContextPreamble) + summaryLen + 2200
	b := &Builder{limits: Limits{Total: limit}}
	b.fitTotal(c)
	if got := len(c.Render()); got > limit {
		t.Fatalf("rendered %d bytes, over limit %d", got, limit)
	}
	if !strings.HasPrefix(c.Blocks[1].Text, curHead) {
		t.Fatalf("earlier log block lost its header due to oversized floor: %q", c.Blocks[1].Text)
	}
	if !strings.HasPrefix(c.Blocks[2].Text, prevHead) || !strings.Contains(c.Blocks[2].Text, crashLoopUnmarkedLine) {
		t.Fatalf("previous log block lost header or tail: %q", c.Blocks[2].Text)
	}
}

// Review 2 Medium 1: trailing blank or whitespace-only lines must not hide a long last line (or a short
// non-blank tail) in cutLog, cutRendered, RenderLog or fitTotal.
func TestKeepLogTailAndRenderedTrailingBlankLines(t *testing.T) {
	longLine := strings.Repeat("A", 3000)
	variants := []struct {
		name string
		in   string
		want string
	}{
		{"double newline", longLine + "\n\n", strings.Repeat("A", 100)},
		{"spaces on blank line", longLine + "\n   \n", strings.Repeat("A", 100)},
		{"crlf blank line", longLine + "\r\n\r\n", strings.Repeat("A", 100)},
		{"crlf spaces tabs and blanks", longLine + "\r\n  \t \r\n\n", strings.Repeat("A", 100)},
		{"several blank lines", longLine + "\n\n\n\n\n", strings.Repeat("A", 100)},
		{"trailing spaces no final newline", longLine + "\n   ", strings.Repeat("A", 100)},
		{"short tail before many blank lines", "older\n" + crashLoopUnmarkedLine + "\n" + strings.Repeat(" \n", 1500), crashLoopUnmarkedLine},
	}
	const capBytes = 2000
	const head = "pod web-abc-1 container app, previous instance\n"

	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := cutLog(tc.in, capBytes)
			if !cut {
				t.Errorf("cutLog: cut = false, want true")
			}
			if len(got) > capBytes {
				t.Errorf("cutLog: len = %d, over cap %d", len(got), capBytes)
			}
			if !utf8.ValidString(got) {
				t.Errorf("cutLog: invalid UTF-8")
			}
			if strings.TrimSpace(got) == "" || !strings.Contains(got, tc.want) {
				t.Errorf("cutLog hid non-blank tail; got %q", got)
			}

			budget := len(head) + capBytes
			renderedTail := cutRendered(head, tc.in, budget)
			if len(renderBody(head+renderedTail)) > budget {
				t.Errorf("cutRendered: rendered len = %d, over budget %d", len(renderBody(head+renderedTail)), budget)
			}
			if !utf8.ValidString(renderedTail) {
				t.Errorf("cutRendered: invalid UTF-8")
			}
			if strings.TrimSpace(renderedTail) == "" || !strings.Contains(renderedTail, tc.want) {
				t.Errorf("cutRendered hid non-blank tail; got %q", renderedTail)
			}
		})
	}

	// End-to-end RenderLog and fitTotal (including >previousTailLines blank lines) on "A"*3000 + "\n\n".
	ex := providers.LogExcerpt{Text: longLine + "\n\n", CollectedAt: testNow}
	v := RenderLog(noRedaction{}, false, strings.TrimSuffix(head, "\n"), ex, nil, 2048, 2048)
	if !v.Cut || len(v.Text) > 2048 || !strings.Contains(v.Text, strings.Repeat("A", 100)) {
		t.Fatalf("RenderLog hid long line behind blank trailing line: cut=%t len=%d text=%q", v.Cut, len(v.Text), v.Text)
	}

	const curHead = "pod web-abc-1 container app, current instance\n"
	c := &Context{Blocks: []Block{
		{Section: SectionSummary, Provenance: ProvenanceIncident, Availability: Available, Text: "incident summary\n"},
		{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available,
			Text: curHead + strings.Repeat("noise line\n", 200), headLen: len(curHead), container: 1},
		{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available,
			Text: head + longLine + strings.Repeat("\n", 15), headLen: len(head), previous: true, container: 1},
	}}
	limit := len(ContextPreamble) + renderedBlockLen(0, c.Blocks[0]) + 2200
	b := &Builder{limits: Limits{Total: limit}}
	b.fitTotal(c)
	if got := len(c.Render()); got > limit {
		t.Fatalf("fitTotal rendered %d bytes, over limit %d", got, limit)
	}
	if !strings.Contains(c.Blocks[2].Text, strings.Repeat("A", 100)) {
		t.Fatalf("fitTotal hid long previous last line behind trailing blank lines: %q", c.Blocks[2].Text)
	}
}

// Review 2 Low 1: a cut in cutLog, cutRendered or cutRendered never lands inside a
// redaction marker ([REDACTED:...]): the marker is kept whole or dropped whole, within the cap.
func TestTailCutNeverSplitsRedactionMarker(t *testing.T) {
	const head = "hdr\n"
	for _, marker := range []string{"[REDACTED:jwt]", "[REDACTED:connection-string]", "[REDACTED]"} {
		t.Run(marker, func(t *testing.T) {
			prefix := strings.Repeat("a", 60)
			suffix := ",tail-after-marker\n"
			line := prefix + marker + suffix
			markerStart := len(prefix)
			markerEnd := markerStart + len(marker)

			// Sweep budgets so the raw cut lands at every offset from before markerStart to after markerEnd.
			for cutPos := markerStart - 2; cutPos <= markerEnd+2; cutPos++ {
				n := len(line) - cutPos
				got, cut := cutLog(line, n)
				if !cut {
					t.Errorf("cutLog(cutPos=%d, n=%d): cut = false, want true", cutPos, n)
				}
				if len(got) > n {
					t.Errorf("cutLog(cutPos=%d, n=%d): len = %d, over cap %d", cutPos, n, len(got), n)
				}
				switch {
				case cutPos <= markerStart:
					if got != line[cutPos:] {
						t.Errorf("cutLog(cutPos=%d, n=%d) = %q, want marker kept whole (%q)", cutPos, n, got, line[cutPos:])
					}
				case cutPos < markerEnd:
					if got != line[markerEnd:] {
						t.Errorf("cutLog(cutPos=%d, n=%d) = %q, want marker dropped whole (%q)", cutPos, n, got, line[markerEnd:])
					}
				case got != line[cutPos:]:
					t.Errorf("cutLog(cutPos=%d, n=%d) = %q, want %q", cutPos, n, got, line[cutPos:])
				}

				budget := len(head) + n
				for name, fn := range map[string]func(string, string, int) string{
					"cutRendered": cutRendered,
				} {
					rgot := fn(head, line, budget)
					if len(renderBody(head+rgot)) > budget {
						t.Errorf("%s(cutPos=%d, budget=%d): rendered %d over %d", name, cutPos, budget, len(renderBody(head+rgot)), budget)
					}
					switch {
					case cutPos <= markerStart:
						if rgot != line[cutPos:] {
							t.Errorf("%s(cutPos=%d) = %q, want marker kept whole (%q)", name, cutPos, rgot, line[cutPos:])
						}
					case cutPos < markerEnd:
						if rgot != line[markerEnd:] {
							t.Errorf("%s(cutPos=%d) = %q, want marker dropped whole (%q)", name, cutPos, rgot, line[markerEnd:])
						}
					case rgot != line[cutPos:]:
						t.Errorf("%s(cutPos=%d) = %q, want %q", name, cutPos, rgot, line[cutPos:])
					}
				}
			}

			// Marker at the very end of the line (followed only by "\n"): never emit a marker fragment.
			endLine := prefix + marker + "\n"
			for n := 1; n <= len(marker)+3; n++ {
				got, _ := cutLog(endLine, n)
				if len(got) > n {
					t.Errorf("end marker cutLog(n=%d): len %d over %d", n, len(got), n)
				}
				if got != "" && !strings.Contains(got, marker) {
					t.Errorf("end marker cutLog(n=%d) = %q, split marker %q", n, got, marker)
				}
				budget := len(head) + n
				rgot := cutRendered(head, endLine, budget)
				if len(renderBody(head+rgot)) > budget {
					t.Errorf("end marker cutRendered(n=%d): rendered over %d", n, budget)
				}
				if rgot != "" && !strings.Contains(rgot, marker) {
					t.Errorf("end marker cutRendered(n=%d) = %q, split marker %q", n, rgot, marker)
				}
			}
		})
	}
}

const (
	testShortLine    = "abc\n"
	testTenDigits    = "0123456789"
	testTenDigitsNL  = testTenDigits + "\n"
	testTwoRocketsNL = "🚀🚀\n"
)

// Review 2 Low 2: direct table tests for cutLog covering exact budget, one byte over, multi-byte runes
// straddling the cut, n smaller than one rune, and newline-only inputs.
func TestKeepLogTailDirect(t *testing.T) {
	cases := []struct {
		name    string
		s       string
		n       int
		want    string
		wantCut bool
	}{
		{"empty string", "", 10, "", false},
		{"zero budget", testShortLine, 0, "", true},
		{"negative budget", testShortLine, -5, "", true},
		{"exact budget with newline", testTenDigitsNL, 11, testTenDigitsNL, false},
		{"exact budget no newline", testTenDigits, 10, testTenDigits, false},
		{"one byte over with newline", testTenDigitsNL, 10, "123456789\n", true},
		{"one byte over no newline", testTenDigits, 9, "123456789", true},
		{"multibyte exact rune start", testTwoRocketsNL, 5, "🚀\n", true},
		{"multibyte offset 1 into rune", testTwoRocketsNL, 6, "🚀\n", true},
		{"multibyte offset 2 into rune", testTwoRocketsNL, 7, "🚀\n", true},
		{"multibyte offset 3 into rune", testTwoRocketsNL, 8, "🚀\n", true},
		{"multibyte fits without newline", testTwoRocketsNL, 4, "🚀", true},
		{"n smaller than one rune", testTwoRocketsNL, 3, "", true},
		{"n smaller than one 3-byte rune", "語語\n", 2, "", true},
		{"newline alone fits", "\n", 1, "\n", false},
		{"newline alone zero budget", "\n", 0, "", true},
		{"only blank lines over budget", "\n\n", 1, "", true},
		{"only whitespace over budget", "   \r\n\t\n", 3, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := cutLog(tc.s, tc.n)
			if got != tc.want || cut != tc.wantCut {
				t.Errorf("cutLog(%q, %d) = (%q, %t), want (%q, %t)", tc.s, tc.n, got, cut, tc.want, tc.wantCut)
			}
			if len(got) > max(tc.n, 0) {
				t.Errorf("len(%q) = %d > %d", got, len(got), tc.n)
			}
			if !utf8.ValidString(got) {
				t.Errorf("invalid UTF-8: %q", got)
			}
		})
	}
}

// Review 2 Low 2: direct table tests for cutRendered covering exact budget, one byte over,
// multi-byte runes straddling the cut, budget smaller than one rune, and newline-only inputs.
func TestKeepLastLineRenderedDirect(t *testing.T) {
	const head = "hdr\n"
	hLen := len(head)
	cases := []struct {
		name   string
		last   string
		budget int
		want   string
	}{
		{"empty last line", "", hLen + 10, ""},
		{"head over budget", testShortLine, hLen - 1, ""},
		{"head exact budget leaves no room", testShortLine, hLen, ""},
		{"exact budget with newline", testTenDigitsNL, hLen + 11, testTenDigitsNL},
		{"exact budget no newline", testTenDigits, hLen + 11, testTenDigits},
		{"one byte over with newline", testTenDigitsNL, hLen + 10, "123456789\n"},
		{"one byte over no newline", testTenDigits, hLen + 10, "123456789"},
		{"multibyte exact rune", testTwoRocketsNL, hLen + 5, "🚀\n"},
		{"multibyte straddle 1", testTwoRocketsNL, hLen + 6, "🚀\n"},
		{"multibyte straddle 2", testTwoRocketsNL, hLen + 7, "🚀\n"},
		{"multibyte straddle 3", testTwoRocketsNL, hLen + 8, "🚀\n"},
		{"budget smaller than one rune", testTwoRocketsNL, hLen + 4, ""},
		// A blank text that fits is kept as it is; one that does not fit gives nothing.
		{"newline alone", "\n", hLen + 10, "\n"},
		{"crlf alone", "\r\n", hLen + 10, "\r\n"},
		{"spaces alone", "   \n", hLen + 10, "   \n"},
		{"spaces alone over budget", "      \n", hLen + 4, ""},
		{"angle bracket escapes expand budget", "<<a\n", hLen + 6, "<a\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cutRendered(head, tc.last, tc.budget)
			if got != tc.want {
				t.Errorf("cutRendered(%q, %q, %d) = %q, want %q", head, tc.last, tc.budget, got, tc.want)
			}
			if got != "" && len(renderBody(head+got)) > tc.budget {
				t.Errorf("rendered %d > budget %d", len(renderBody(head+got)), tc.budget)
			}
			if !utf8.ValidString(got) {
				t.Errorf("invalid UTF-8: %q", got)
			}
		})
	}
}
