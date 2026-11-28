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
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/shadewright-labs/shadewright/internal/logcut"
	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/logs/kube"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

// The property of the decisions of 2026-11-28 on #1055, checked on every place that cuts a container log for the
// excerpt: for any log (any line lengths, any read size, any cap), a non-blank last line gives a non-empty
// excerpt that ends with that line's tail, unless every line of the log is a continuation line (indented by a
// space or a tab, or a "- " item), which the reader drops as before (dropLeadingContinuation, a redaction
// safeguard); the excerpt starts at a line the reader keeps, or is the newest continuation lines of an entry whose
// first line did not fit, so labelled; the log section stays within Limits.Logs; and an earlier, higher-priority
// excerpt is never cut to make room for a later one. The cut paths: the log source's cut of what it read
// (kube.ExcerptOf), RenderLog without and with refit, FitLog, the builder's two reads of a container within its
// share (with a second read that answers, fails or is withheld), and the cut of the whole context. The fake log
// source answers through kube.ExcerptOf with the read size asked, as the production source does.
//
// The generated lines start with a word, or are continuation lines (a tab, spaces, "\tat " and "- " frames), or
// start with what a reader keeps though it looks indented ("at ", Unicode spaces). Logs end in the frames of a
// stack trace after the first line of their entry (two cases in five), in mixed lines, or in continuation lines
// only. Every
// generated last line ends with a word ("exit" and a digit), so a tail that a reader keeps always exists within
// any cap of at least propMinCap bytes.

const propMinCap = 64

// propRuns returns how many generated cases a property test draws: n, a quarter of it under the race detector,
// which slows redaction about tenfold (the seed and the first cases stay the same).
func propRuns(n int) int {
	if raceDetector {
		return n / 4
	}
	return n
}

var propTokens = []string{
	"alpha", "beta", "ok", "error", "retry", "conn", "pool", "GET", "/api/v1", "200", "ms", "done", "42",
	"*", "****", "*******", "-", "- x", "\t", "  ", "[REDACTED" + "]", BlockDelimiter, "context    block",
	"<", ">", "é", "🚀", "\u00a0", "\u2039", "語",
}

// propIndents start continuation lines (the first five) and lines a reader keeps though they look indented (the
// rest).
var propIndents = []string{"\t", "  ", "\tat ", "- ", "-\t", "at ", "\u3000", "\u00a0", "\u2003 "}

// propLine returns one line of about n bytes, without its line ending, that starts with a word.
func propLine(rng *rand.Rand, n int) string { return propLineFrom(rng, "L", n) }

// propCont returns one line of about n bytes that starts with one of propIndents.
func propCont(rng *rand.Rand, n int) string {
	return propLineFrom(rng, propIndents[rng.Intn(len(propIndents))], n)
}

func propLineFrom(rng *rand.Rand, start string, n int) string {
	var b strings.Builder
	b.WriteString(start)
	for b.Len() < n {
		if b.Len() > len(start) {
			b.WriteByte(' ')
		}
		b.WriteString(propTokens[rng.Intn(len(propTokens))])
	}
	return b.String()
}

// propLength draws a line length: mostly short, sometimes far longer than any cap.
func propLength(rng *rand.Rand) int {
	switch rng.Intn(4) {
	case 0:
		return rng.Intn(80)
	case 1:
		return 80 + rng.Intn(600)
	case 2:
		return 600 + rng.Intn(2000)
	default:
		return 2600 + rng.Intn(4000)
	}
}

// propFrameLength draws the length of a frame: mostly short, as stack frames are.
func propFrameLength(rng *rand.Rand) int {
	if rng.Intn(8) == 0 {
		return propLength(rng)
	}
	return 10 + rng.Intn(70)
}

// propLog returns a log whose last non-blank line ends with a word, possibly followed by blanks, a carriage
// return and blank lines, and that line. Its shape is one of: lines that start with a word; the frames of a
// stack trace after the first line of their entry; mixed lines; continuation lines only.
func propLog(rng *rand.Rand) (log, last string) {
	var b strings.Builder
	shape := []int{0, 1, 1, 2, 3}[rng.Intn(5)]
	next := func() string {
		switch shape {
		case 0:
			return propLine(rng, propLength(rng))
		case 1:
			return propLineFrom(rng, propIndents[rng.Intn(5)], propFrameLength(rng))
		case 2:
			if rng.Intn(2) == 0 {
				return propLine(rng, propLength(rng))
			}
			return propCont(rng, propFrameLength(rng))
		default:
			return propLineFrom(rng, propIndents[rng.Intn(5)], propFrameLength(rng))
		}
	}
	n := rng.Intn(4)
	if shape == 1 {
		// The first line of the entry, then many frames.
		b.WriteString(propLine(rng, propLength(rng)) + "\n")
		if rng.Intn(3) == 0 {
			b.WriteString(propLine(rng, propLength(rng)) + "\n")
		} else {
			b.WriteString(propLine(rng, 20+rng.Intn(100)) + "\n")
		}
		n = 8 + rng.Intn(60)
		for range n {
			b.WriteString(next() + "\n")
		}
	} else {
		if shape == 3 {
			n = rng.Intn(80)
		}
		for range n {
			b.WriteString(next() + "\n")
		}
	}
	if shape == 0 {
		last = propLine(rng, propLength(rng))
	} else {
		last = next()
	}
	if last != "" {
		last += " "
	}
	last += fmt.Sprintf("exit%d", rng.Intn(10))
	b.WriteString(last)
	b.WriteString([]string{"", "", " ", "\u00a0\u00a0", "\r"}[rng.Intn(5)])
	if rng.Intn(6) > 0 {
		b.WriteByte('\n')
	}
	for range rng.Intn(3) {
		b.WriteString([]string{"\n", "  \n", "\t\n"}[rng.Intn(3)])
	}
	return b.String(), last
}

// allContinuation reports whether every line of log is a continuation line a reader drops.
func allContinuation(log string) bool { return logcut.LeadingContinuation(log) == len(log) }

// checkStart checks that a non-blank excerpt starts at a line a reader keeps, unless it is the newest
// continuation lines of an entry whose first line was left out (headerLeftOut).
func checkStart(t *testing.T, path string, seed int64, excerpt string, headerLeftOut bool) {
	t.Helper()
	if strings.TrimSpace(excerpt) != "" && !headerLeftOut && logcut.LeadingContinuation(excerpt) > 0 {
		t.Errorf("seed %d, %s: excerpt starts with a continuation line, unlabelled: %q", seed, path, clip(excerpt))
	}
}

// lastNonBlankLine returns the last line of s that is not blank, without trailing whitespace.
func lastNonBlankLine(s string) string {
	for _, line := range slices.Backward(strings.Split(s, "\n")) {
		if l := strings.TrimRightFunc(line, unicode.IsSpace); strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

// checkTail checks that excerpt ends with a non-empty tail of last.
func checkTail(t *testing.T, path string, seed int64, excerpt, last string) {
	t.Helper()
	got := lastNonBlankLine(excerpt)
	if got == "" || !strings.HasSuffix(last, got) {
		t.Errorf("seed %d, %s: excerpt %q (%d bytes) does not end with a tail of the last line %.40q...%q", seed, path,
			clip(excerpt), len(excerpt), last, last[max(0, len(last)-20):])
	}
}

func clip(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:40] + "..." + s[len(s)-60:]
}

func TestCutPathsKeepTheLastLineGenerated(t *testing.T) {
	t.Parallel() // several seconds of redaction, independent of the other tests
	const seed = 1055
	rng := rand.New(rand.NewSource(seed)) // #nosec G404 -- a seeded corpus, reproducible by design
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	const title = "pod web-abc-1 container app, previous instance"
	// Each kind of excerpt the source gives is drawn at least once, also on the smaller run under -race.
	seen := map[string]int{}
	for i := range propRuns(160) {
		s := seed*1000 + int64(i)
		log, last := propLog(rng)
		tails := !allContinuation(log)
		// The log source's cut, as kube.Logs makes it.
		keep := propMinCap + rng.Intn(4<<10)
		ex, err := kube.ExcerptOf(red, []byte(log), false, keep)
		if err != nil {
			t.Fatalf("seed %d: Excerpt: %v", s, err)
		}
		switch {
		case ex.HeaderKept:
			seen["header kept"]++
		case ex.HeaderLeftOut:
			seen["header left out"]++
		case ex.LeadingDropped:
			seen["dropped"]++
		case !tails:
			seen["only continuation lines"]++
		}
		if len(ex.Text) > keep {
			t.Errorf("seed %d, kube.Excerpt: %d bytes over %d", s, len(ex.Text), keep)
		}
		if tails {
			// The source keeps whole continuation lines at its start for the reader to drop; the start is checked
			// on what the reader keeps, below.
			checkTail(t, "kube.Excerpt", s, ex.Text, last)
		}
		ex.CollectedAt = testNow

		// RenderLog without refit, on the source's answer and on the whole log (a source within its bound).
		for _, answer := range []providers.LogExcerpt{ex, {Text: log, CollectedAt: testNow}} {
			asked := max(len(answer.Text)/2+1, propMinCap) + rng.Intn(2<<10)
			budget := len(title) + 200 + propMinCap + rng.Intn(4<<10)
			v := RenderLog(noRedaction{}, false, title, answer, nil, asked, budget)
			if len(v.Text) > budget || v.Read != LogReadOK {
				t.Errorf("seed %d, RenderLog: %d bytes (bound %d), read %d", s, len(v.Text), budget, v.Read)
				continue
			}
			if tails {
				checkTail(t, "RenderLog", s, v.Text[v.HeadLen:], last)
				checkStart(t, "RenderLog", s, v.Text[v.HeadLen:], v.HeaderLeftOut)
			}

			// FitLog of what RenderLog gave.
			n := v.HeadLen + propMinCap + rng.Intn(2<<10)
			fit, head, _, kind := FitLogCut(v.Text, v.HeadLen, n)
			if len(fit) > n {
				t.Errorf("seed %d, FitLog: %d bytes over %d", s, len(fit), n)
			}
			if tails {
				checkTail(t, "FitLog", s, fit[head:], last)
				checkStart(t, "FitLog", s, fit[head:], v.HeaderLeftOut || kind == logcut.HeaderLeftOut)
			}

			// RenderLog with refit, with a redactor that redacts twice (get_pod_logs).
			rv := RenderLog(red, true, title, answer, nil, asked, budget)
			if len(rv.Text) > budget || rv.Read != LogReadOK {
				t.Errorf("seed %d, RenderLog refit: %d bytes (bound %d), read %d", s, len(rv.Text), budget, rv.Read)
				continue
			}
			if tails {
				checkTail(t, "RenderLog refit", s, rv.Text[rv.HeadLen:], last)
				checkStart(t, "RenderLog refit", s, rv.Text[rv.HeadLen:], rv.HeaderLeftOut)
			}
		}
	}
	for _, k := range []string{"header kept", "header left out", "dropped", "only continuation lines"} {
		if seen[k] == 0 {
			t.Errorf("no case drew a source excerpt of kind %q (seen %v)", k, seen)
		}
	}
}

// propSource answers each pod's instances through kube.Excerpt with the read size asked, or with an error.
type propSource struct {
	red  *redact.Redactor
	logs map[string]string
	errs map[string]error
}

func propKey(pod string, previous bool) string { return fmt.Sprintf("%s/%t", pod, previous) }

func (p propSource) Logs(_ context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
	k := propKey(q.Pod, q.Previous)
	if err := p.errs[k]; err != nil {
		return providers.LogExcerpt{}, err
	}
	ex, err := kube.ExcerptOf(p.red, []byte(p.logs[k]), false, int(q.MaxBytes))
	if err != nil {
		return providers.LogExcerpt{}, err
	}
	ex.CollectedAt = testNow
	return ex, nil
}

// propBlock returns the log block whose header names pod and instance, and whether there is one.
func propBlock(blks []Block, pod string, previous bool) (Block, bool) {
	name := "pod " + pod + " container " + appName + ", " + instanceName(previous) + "\n"
	for _, b := range blks {
		if strings.Contains(b.Text[:b.headLen], name) && strings.Contains(b.Text[:b.headLen], "instance: ") {
			return b, true
		}
	}
	return Block{}, false
}

func TestBuilderCutPathsKeepTheLastLineGenerated(t *testing.T) {
	t.Parallel() // several seconds of redaction, independent of the other tests
	const seed = 1055
	rng := rand.New(rand.NewSource(seed)) // #nosec G404 -- a seeded corpus, reproducible by design
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	for i := range propRuns(32) {
		s := seed*1000 + int64(i)
		// Long pod names make long headers, so a notice can need more than what is left of a share.
		first, other := crashPod, secondPod
		if rng.Intn(2) == 0 {
			first += "-" + strings.Repeat("a", 200+rng.Intn(50))
		}
		d := deployment()
		rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
		ref := controllerRef(kindReplicaSet, rs.Name, rs.UID)
		objs := []client.Object{d, rs, exited(pod(first, rs.Labels, longAgo, ref)), exited(pod(other, rs.Labels, longAgo, ref))}
		src := propSource{red: red, logs: map[string]string{}, errs: map[string]error{}}
		lasts := map[string]string{}
		for _, p := range []string{first, other} {
			for _, prev := range []bool{false, true} {
				src.logs[propKey(p, prev)], lasts[propKey(p, prev)] = propLog(rng)
			}
		}
		// The second read of the first container (its previous instance) answers, fails or is withheld.
		secondErr := []error{nil, nil, errors.New("connection reset"), fmt.Errorf("the log excerpt: %w", providers.ErrWithheld)}[rng.Intn(4)]
		if secondErr != nil {
			src.errs[propKey(first, true)] = secondErr
		}
		limits := DefaultLimits()
		limits.Logs = MinSectionBytes + rng.Intn(15<<10)
		b := newBuilder(t, reader(t, objs...), sources{}, func(c *Config) { c.Logs = src; c.Limits = limits })
		blks := blocks(build(t, b, incident(deploymentRef())), SectionLogs)
		checkLogSection(t, s, blks, limits.Logs, secondErr)
		// Below 4 KiB a share (half the bound) cannot hold two headers with long names and a tail; the bound and
		// the notices hold whatever the bound.
		for _, p := range []string{first, other} {
			for _, prev := range []bool{false, true} {
				if p == first && prev && secondErr != nil || limits.Logs < 4<<10 {
					continue
				}
				blk, ok := propBlock(blks, p, prev)
				if !ok {
					t.Errorf("seed %d: no block for %s, %s", s, p, instanceName(prev))
					continue
				}
				if allContinuation(src.logs[propKey(p, prev)]) {
					continue
				}
				checkTail(t, "builder "+p+" "+instanceName(prev), s, blk.Text[blk.headLen:], lasts[propKey(p, prev)])
			}
		}
		total := MinTotalBytes + rng.Intn(16<<10)
		cut, err := b.BuildWithin(t.Context(), incident(deploymentRef()), total)
		if err != nil {
			t.Fatal(err)
		}
		checkTotalCut(t, s, blks, cut, total, first, other, func(prev bool) bool {
			return limits.Logs >= 4<<10 && !allContinuation(src.logs[propKey(other, prev)])
		})
	}
}

// checkLogSection checks the log section of a build: within its bound, and the notice of a second read that
// failed or was withheld.
func checkLogSection(t *testing.T, seed int64, blks []Block, bound int, secondErr error) {
	t.Helper()
	var all strings.Builder
	for _, blk := range blks {
		all.WriteString(blk.Text)
	}
	if all.Len() > bound {
		t.Errorf("seed %d (second read %v): log section %d bytes, over %d", seed, secondErr, all.Len(), bound)
	}
	switch {
	case secondErr == nil:
	case errors.Is(secondErr, providers.ErrWithheld):
		if !strings.Contains(all.String(), withheldText) {
			t.Errorf("seed %d: withheld second read lost its notice:\n%s", seed, clip(all.String()))
		}
	default:
		if !strings.Contains(all.String(), "\ncould not fetch\n") {
			t.Errorf("seed %d: failed second read lost its \"could not fetch\" note:\n%s", seed, clip(all.String()))
		}
	}
}

// checkTotalCut checks the cut of the whole context: never over its bound, a later container's excerpts are
// cut before an earlier one loses any of its excerpt, and, with tails, a kept header keeps a tail when there was
// room for one.
func checkTotalCut(t *testing.T, seed int64, blks []Block, cut *Context, total int, first, other string, tails func(prev bool) bool) {
	t.Helper()
	if n := len(cut.Render()); n > total {
		t.Errorf("seed %d: rendered %d bytes, over %d", seed, n, total)
	}
	cutBlks := blocks(cut, SectionLogs)
	firstCut := false
	for _, prev := range []bool{false, true} {
		before, ok1 := propBlock(blks, first, prev)
		after, ok2 := propBlock(cutBlks, first, prev)
		if ok1 && (!ok2 || len(after.Text[after.headLen:]) < len(before.Text[before.headLen:])) {
			firstCut = true
		}
	}
	for _, p := range []string{first, other} {
		for _, prev := range []bool{false, true} {
			before, ok1 := propBlock(blks, p, prev)
			after, ok2 := propBlock(cutBlks, p, prev)
			if ok1 && ok2 {
				if problem := totalCutTailProblem(before.Text[before.headLen:], after.Text[after.headLen:]); problem != "" {
					t.Errorf("seed %d (total %d): %s %s: %s", seed, total, p, instanceName(prev), problem)
				}
			}
		}
	}
	for _, prev := range []bool{false, true} {
		after, ok := propBlock(cutBlks, other, prev)
		if !ok {
			continue
		}
		body := strings.TrimSpace(after.Text[after.headLen:])
		if firstCut && body != "" {
			t.Errorf("seed %d (total %d): the first container's excerpt was cut while the later %s keeps %d bytes",
				seed, total, instanceName(prev), len(after.Text)-after.headLen)
		}
		if tails(prev) && body == "" && len(cut.Render())+propMinCap <= total {
			t.Errorf("seed %d (total %d): %s %s kept its header and no tail with room left", seed, total, other, instanceName(prev))
		}
	}
}

// totalCutTailProblem returns what is wrong with after, the body of a log block the cut of the whole context
// kept, against before, its body ahead of that cut, or "": a non-blank kept body ends with a tail of before's
// last non-blank line, never a prefix of before.
func totalCutTailProblem(before, after string) string {
	if strings.TrimSpace(after) == "" {
		return ""
	}
	got, want := lastNonBlankLine(after), lastNonBlankLine(before)
	if got == "" || !strings.HasSuffix(want, got) {
		return fmt.Sprintf("kept %q does not end with a tail of the last line %q", clip(after), clip(want))
	}
	return ""
}

// The check of the total cut catches a kept body that is a prefix of the excerpt instead of its tail (finding of
// 2026-11-28 on #1055: it compared lengths only).
func TestTotalCutCheckRejectsAPrefix(t *testing.T) {
	if totalCutTailProblem("older\nexit7\n", "older\n") == "" {
		t.Error("a prefix passed the check")
	}
	if p := totalCutTailProblem("older\nexit7\n", "exit7\n"); p != "" {
		t.Errorf("the tail failed the check: %s", p)
	}
}

// The suffix search of the rendered cut must not assume that the rendered size grows with the text kept:
// a spelling of the delimiter with many separators renders as 13 bytes whole, but a tail that starts inside it
// renders each separator. Here the last line is too long to keep whole; its longest tail that fits rendered
// starts at the spelling, and every shorter tail up to "block" renders larger. A search that takes the size
// as growing settles on a short tail after the separators.
func TestRenderedCutDoesNotAssumeSizeGrows(t *testing.T) {
	const head = "pod web-abc-1 container app, previous instance\n"
	tail := "context" + strings.Repeat(" ", 200) + "block\n"
	line := strings.Repeat("x", 300) + tail
	c := &Context{Blocks: []Block{
		{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available,
			Text: head + strings.Repeat("noise line\n", 50) + line, headLen: len(head)},
	}}
	kept := Block{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available,
		Text: head + tail, headLen: len(head), Labels: []Label{LabelContextCut}}
	limit := len(ContextPreamble) + renderedBlockLen(0, kept)
	b := &Builder{limits: Limits{Total: limit}}
	b.fitTotal(c)
	if got := len(c.Render()); got > limit {
		t.Fatalf("rendered %d bytes, over %d", got, limit)
	}
	if body := c.Blocks[0].Text[c.Blocks[0].headLen:]; body != tail {
		t.Errorf("kept %q, want the longest tail that fits rendered, %q", body, tail)
	}
}

// A second read after the excerpt of a terminated current instance that is withheld or fails keeps its whole
// notice, and the room for it comes from the first excerpt, so the container stays within its share (decision
// of 2026-11-28 on #1055). The first excerpt fills the share but for less than the notice needs.
func TestSecondReadNoticeReclaimsFromTheFirstExcerpt(t *testing.T) {
	const share = 1024
	for name, readErr := range map[string]error{
		"withheld": fmt.Errorf("the log excerpt: %w", providers.ErrWithheld),
		"failed":   errors.New("connection reset"),
	} {
		t.Run(name, func(t *testing.T) {
			b := newBuilder(t, reader(t), sources{}, nil)
			pick := logPick{namespace: testNS, pod: crashPod, container: appName, other: true, currentDead: true}
			first, _ := b.renderBlock(pick, providers.LogExcerpt{Text: strings.Repeat("line of the current instance\n", 60) +
				"exit 1\n", CollectedAt: testNow}, nil, share-20)
			r := containerReads{pick: pick, budget: share, left: share - len(first.Text), askLeft: 20, reads: 1, out: []Block{first}}
			b.recordRead(t.Context(), &r, true, providers.LogExcerpt{}, readErr, r.queryBudget())
			blks := r.blocks()
			text := ""
			for _, blk := range blks {
				text += blk.Text
			}
			if len(text) > share || r.left < 0 {
				t.Errorf("container %d bytes, over its share %d (left %d)", len(text), share, r.left)
			}
			notice := "\ncould not fetch\n"
			if name == "withheld" {
				notice = withheldText
			}
			if !strings.Contains(text, notice) {
				t.Errorf("notice %q missing:\n%s", notice, text)
			}
			if !strings.HasSuffix(blks[0].Text, "exit 1\n") {
				t.Errorf("first excerpt lost its last line:\n%s", clip(blks[0].Text))
			}
		})
	}
}

// renderedSize gives the rendered size of every part the cut may ask about exactly, on text full of spellings of
// the delimiter (with separators, across lines, with a look-alike K), angle brackets and multi-byte runes.
func TestRenderedSizeIsExact(t *testing.T) {
	rng := rand.New(rand.NewSource(1055)) // #nosec G404 -- a seeded corpus, reproducible by design
	tokens := []string{"context", "block", "bloc\u212a", "-", " ", "\n", "\u00ad", "_", "<", ">", "\u2039", "x", "語", BlockDelimiter,
		"context\n\nblock", "CONTEXT  BLOCK"}
	for i := range 2000 {
		var b strings.Builder
		for range rng.Intn(30) {
			b.WriteString(tokens[rng.Intn(len(tokens))])
		}
		rest := b.String()
		head := []string{"", "pod web-abc-1 container app\n", "context\n", "a context"}[rng.Intn(4)]
		size := renderedSize(head, rest)
		var starts []int
		for j := range len(rest) + 1 {
			if j == len(rest) || utf8.RuneStart(rest[j]) {
				starts = append(starts, j)
			}
		}
		for range 20 {
			a, z := starts[rng.Intn(len(starts))], starts[rng.Intn(len(starts))]
			if a > z {
				a, z = z, a
			}
			if got, want := size(a, z), len(renderBody(head+rest[a:z])); got != want {
				t.Fatalf("case %d: renderedSize(%q, %q)(%d, %d) = %d, want %d", i, head, rest, a, z, got, want)
			}
		}
	}
}
