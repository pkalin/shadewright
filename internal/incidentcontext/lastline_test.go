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
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

// The floors of the last-line rule, one per cut. cutLog keeps the longest marker shape whole: "[REDACTED:"
// plus a 32-byte kind plus "]" is 43 bytes. The rendered cut adds the newline that renderBody appends, and a
// kind may hold "context" and "block" with no separator twice, each of which Neutralise writes one byte longer
// ("context_block"): 43 + 1 + 2 = 46 bytes of budget after the header.
const (
	rawLastLineFloor      = 43
	renderedLastLineFloor = 46
)

// startsInsideMarker reports whether a cut of src at off falls strictly inside a redaction marker: a
// bracket-shaped marker or a run of at most three "*" ([redact.MarkerShapes]). A longer run of "*" is not one
// marker the redactor writes, and every part of it is only "*", so a cut inside it exposes nothing.
func startsInsideMarker(src string, off int) bool {
	for _, m := range redact.MarkerShapes(src) {
		if m[0] < off && off < m[1] {
			return src[m[0]] != '*' || m[1]-m[0] <= len("***")
		}
	}
	return false
}

// Review 3 Low 1: a run of "*" longer than the cap that reaches the end of the last line matched the marker
// shape, the cut moved to its end, and the excerpt came out empty.
func TestLongStarRunAtLineEndKeepsLastLineTail(t *testing.T) {
	const capBytes = 2000
	stars := strings.Repeat("*", 3000)
	for _, line := range []string{
		"ERR " + stars + "\n",
		"ERR " + stars,
		"ERR " + stars + "   \n",
		"ERR " + stars + "\r\n\r\n",
		"earlier line\nERR " + stars + "\n\n",
		stars + "  \n",
		stars + "\u00a0\u00a0\n",
	} {
		got, cut := cutLog(line, capBytes)
		if !cut || strings.Trim(strings.TrimRightFunc(got, unicode.IsSpace), "*") != "" || !strings.Contains(got, "*") || len(got) > capBytes {
			t.Errorf("cutLog(%.20q..., %d) = (%d bytes %.20q..., %t), want a non-empty run of * within the cap",
				line, capBytes, len(got), got, cut)
		}
		const head = "hdr\n"
		budget := len(head) + capBytes
		for name, fn := range map[string]func(string, string, int) string{
			"cutRendered": cutRendered,
			"cutRendered last line": func(h, s string, b int) string {
				return cutRendered(h, lastNLines(nonBlankPart(s), 1), b)
			},
		} {
			rgot := fn(head, line, budget)
			if strings.Trim(strings.TrimRightFunc(rgot, unicode.IsSpace), "*") != "" || !strings.Contains(rgot, "*") || len(renderBody(head+rgot)) > budget {
				t.Errorf("%s(%.20q..., %d) = %d bytes %.20q..., want a non-empty run of * that renders within the budget",
					name, line, budget, len(rgot), rgot)
			}
		}
	}
}

// A last line whose tail within the cap is only Unicode whitespace (here no-break spaces, U+00A0) keeps the text
// before that whitespace rather than coming out empty.
func TestLastLineTrailingUnicodeSpaceKeepsText(t *testing.T) {
	const capBytes = 100
	line := "ERR boom" + strings.Repeat("\u00a0", 200) + "\n"
	got, _ := cutLog(line, capBytes)
	if got != "ERR boom" {
		t.Errorf("cutLog = %q, want %q", got, "ERR boom")
	}
	const head = "hdr\n"
	if rgot := cutRendered(head, line, len(head)+capBytes); rgot != "ERR boom" {
		t.Errorf("cutRendered = %q, want %q", rgot, "ERR boom")
	}
}

// lastLineFragments are the pieces the property test builds lines from: words, markers and marker-shaped text,
// runs of "*" of every length around the redactor's three, multi-byte runes, angle brackets that grow when
// rendered, and ASCII and Unicode whitespace.
var lastLineFragments = []string{
	"ERR", "panic:", "x", "token=", " ", "  ", "\t", "\u00a0", "\u3000",
	"[REDACTED]", "[REDACTED:jwt]", "[jwt]", "[info]", "[REDACTED:" + strings.Repeat("k", 32) + "]", worstKindMarker, "contextblock", "context-block",
	"[" + strings.Repeat("a", 32) + "]", "[REDACTED:", "]", "[",
	"*", "**", "***", "****",
	"é", "語", "🚀", "<", ">", "＜",
}

// randomLastLineLog builds a log of a few lines whose last line is non-blank after the line ending, sometimes
// followed by blank lines, with the last line sometimes longer than capBytes.
func randomLastLineLog(rng *rand.Rand, capBytes int) string {
	var b strings.Builder
	for range rng.IntN(3) {
		for range rng.IntN(8) {
			b.WriteString(lastLineFragments[rng.IntN(len(lastLineFragments))])
		}
		b.WriteString("\n")
	}
	target := rng.IntN(max(capBytes/2, 1))
	if rng.IntN(2) == 0 {
		target = capBytes + rng.IntN(2*capBytes)
	}
	var line strings.Builder
	for line.Len() < target {
		switch rng.IntN(6) {
		case 0:
			line.WriteString(strings.Repeat("*", 1+rng.IntN(2*capBytes)))
		case 1:
			line.WriteString(strings.Repeat(lastLineFragments[rng.IntN(len(lastLineFragments))], 1+rng.IntN(max(capBytes/4, 1))))
		default:
			line.WriteString(lastLineFragments[rng.IntN(len(lastLineFragments))])
		}
	}
	for strings.TrimSpace(line.String()) == "" {
		line.WriteString(lastLineFragments[rng.IntN(len(lastLineFragments))])
	}
	b.WriteString(line.String())
	b.WriteString([]string{"", "\n", "\r\n", "\n\n", "\n  \n", " \t\n"}[rng.IntN(6)])
	return b.String()
}

// checkLastLineExcerpt checks the last-line rule on one kept excerpt of src: not blank (when nonBlank),
// valid UTF-8, a piece of src (up to its final newline), and not starting inside a marker.
func checkLastLineExcerpt(t *testing.T, name, src, got string, nonBlank bool) {
	t.Helper()
	if strings.TrimSpace(got) == "" {
		if nonBlank {
			t.Fatalf("%s(%q) is blank: %q", name, src, got)
		}
		return
	}
	if !utf8.ValidString(got) {
		t.Fatalf("%s(%q) = %q, not valid UTF-8", name, src, got)
	}
	// The rendered cut keeps a last line's own newline but not a "\r" before it, so locate got without it.
	off := strings.LastIndex(src, strings.TrimSuffix(got, "\n"))
	if off < 0 {
		// The first line of an entry kept ahead of its newest lines (logcut.HeaderKept): two pieces, a whole line
		// and a later piece.
		h, rest, _ := strings.Cut(got, "\n")
		hAt := strings.Index(src, h+"\n")
		off = strings.LastIndex(src, strings.TrimSuffix(rest, "\n"))
		if hAt < 0 || (hAt > 0 && src[hAt-1] != '\n') || off < hAt+len(h)+1 {
			t.Fatalf("%s(%q) = %q, not a piece of the log", name, src, got)
		}
	}
	if startsInsideMarker(src, off) {
		t.Fatalf("%s(%q) = %q, starts inside a marker at %d", name, src, got, off)
	}
}

// The last-line rule, generated: for a log whose last line is non-blank, cutLog returns a non-blank
// excerpt within a cap of at least rawLastLineFloor, and cutRendered one within a budget of at least
// renderedLastLineFloor after the header, that does not start inside a marker. A quarter of the cases use a
// smaller cap, where the excerpt may be empty but must still fit and must not start inside a marker.
func TestLastLineExcerptNeverEmptyGenerated(t *testing.T) {
	rng := rand.New(rand.NewPCG(1055, 3)) // #nosec G404 -- a seeded corpus, reproducible by design
	iterations := 3000
	if testing.Short() {
		iterations = 300
	}
	const head = "hdr <x>\n"
	for range iterations {
		capBytes := rawLastLineFloor + rng.IntN(1500)
		if rng.IntN(4) == 0 {
			capBytes = 1 + rng.IntN(renderedLastLineFloor+20)
		}
		src := randomLastLineLog(rng, capBytes)

		got, _ := cutLog(src, capBytes)
		if len(got) > capBytes {
			t.Fatalf("cutLog(%q, %d) = %d bytes, over the cap", src, capBytes, len(got))
		}
		checkLastLineExcerpt(t, "cutLog", src, got, capBytes >= rawLastLineFloor)

		budget := len(renderBody(head)) + capBytes
		rgot := cutRendered(head, src, budget)
		if n := len(renderBody(head + rgot)); n > budget {
			t.Fatalf("cutRendered(%q, %d) renders %d bytes, over the budget", src, budget, n)
		}
		checkLastLineExcerpt(t, "cutRendered", src, rgot, capBytes >= renderedLastLineFloor)
	}
}

// worstKindMarker is the marker shape with the largest rendered size: a 32-byte kind of two "contextblock"
// and padding.
const worstKindMarker = "[REDACTED:contextblockcontextblockaaaaaaaa]"

// Review 4 Medium 1: the rendered cut needs 46 bytes, not 43, for a last line ending in the worst marker
// (and 44 bytes for any plain 43-byte marker because renderBody adds a newline). Below each floor the cut
// may return "" (header only), never more than the cap and never a split marker.
func TestLastLineFloorsAreExact(t *testing.T) {
	if len(worstKindMarker) != rawLastLineFloor {
		t.Fatalf("worst marker is %d bytes, want %d", len(worstKindMarker), rawLastLineFloor)
	}
	plainMarker := "[REDACTED:" + strings.Repeat("a", 32) + "]"
	if len(plainMarker) != rawLastLineFloor {
		t.Fatalf("plain marker is %d bytes, want %d", len(plainMarker), rawLastLineFloor)
	}
	line := "x" + worstKindMarker + "\n"
	if got, cut := cutLog(line, rawLastLineFloor); got != worstKindMarker || !cut {
		t.Errorf("cutLog at %d = (%q, %t), want the marker", rawLastLineFloor, got, cut)
	}
	if got, _ := cutLog(line, rawLastLineFloor-1); got != "" {
		t.Errorf("cutLog below the floor = %q, want empty", got)
	}
	const head = "hdr\n"
	at := func(src string, n int) string {
		got := cutRendered(head, src, len(renderBody(head))+n)
		if len(got) > 0 && len(renderBody(head+got)) > len(renderBody(head))+n {
			t.Errorf("cutRendered at %d = %q, over the budget", n, got)
		}
		return got
	}
	if got := at(line, renderedLastLineFloor); got != worstKindMarker+"\n" {
		t.Errorf("cutRendered at %d = %q, want the marker and its newline", renderedLastLineFloor, got)
	}
	if got := at(line, renderedLastLineFloor-1); got != "" {
		t.Errorf("cutRendered below the floor = %q, want empty", got)
	}
	if got := cutRendered(head, line, len(renderBody(head))+renderedLastLineFloor); got != worstKindMarker+"\n" {
		t.Errorf("cutRendered at %d = %q, want the marker and its newline", renderedLastLineFloor, got)
	}
	if got := cutRendered(head, line, len(renderBody(head))+renderedLastLineFloor-1); got != "" {
		t.Errorf("cutRendered below the floor = %q, want empty", got)
	}
	// Any 43-byte marker fails at 43 rendered bytes because renderBody adds a newline, and fits at 44.
	plainLine := "x" + plainMarker + "\n"
	if got, cut := cutLog(plainLine, rawLastLineFloor); got != plainMarker || !cut {
		t.Errorf("cutLog(plain) at %d = (%q, %t), want the marker", rawLastLineFloor, got, cut)
	}
	if got := at(plainLine, rawLastLineFloor); got != "" {
		t.Errorf("cutRendered(plain) at %d = %q, want empty", rawLastLineFloor, got)
	}
	if got := at(plainLine, rawLastLineFloor+1); got != plainMarker+"\n" {
		t.Errorf("cutRendered(plain) at %d = %q, want the marker and its newline", rawLastLineFloor+1, got)
	}
}

// Real configuration (RemediationSettings maxContextBytes down to MinTotalBytes) can leave fewer than
// renderedLastLineFloor bytes after a log block's header during fitTotal. The block stays within the total
// bound, keeps its header alone when a trailing marker does not fit, and never splits a marker.
func TestFitTotalBelowRenderedFloorReturnsHeaderOnly(t *testing.T) {
	head := "pod p container c, previous instance\ninstance: pod uid u, container id cid, restarts 1, read at 2026-11-27T00:00:00Z\n"
	body := strings.Repeat("earlier noise line\n", 10) + "x" + worstKindMarker + "\n"
	const stub = "s\n"
	c := &Context{
		Blocks: []Block{
			{Section: SectionSummary, Provenance: ProvenanceIncident, Availability: Available, Text: stub},
			{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available, Text: head + body, headLen: len(head), previous: true},
		},
	}
	// Pad summary so fitTotal at MinTotalBytes leaves exactly renderedLastLineFloor-1 bytes after head.
	logWrapper := blockWrapperLenWithCutLabel(1, c.Blocks[1])
	wantLogRendered := logWrapper + len(renderBody(head)) + renderedLastLineFloor - 1
	othersNeed := MinTotalBytes - wantLogRendered
	summaryOverhead := len(ContextPreamble) + renderedBlockLen(0, c.Blocks[0]) - len(stub)
	pad := othersNeed - summaryOverhead
	if pad <= 0 {
		t.Fatalf("unexpected pad %d", pad)
	}
	c.Blocks[0].Text = strings.Repeat("a", pad-1) + "\n"
	if c.Size() <= MinTotalBytes {
		t.Fatalf("expected size > MinTotalBytes before fitTotal, got %d", c.Size())
	}
	if !c.fitTotal(MinTotalBytes) {
		t.Fatalf("fitTotal(%d) returned false, size %d", MinTotalBytes, c.Size())
	}
	if got := c.Size(); got > MinTotalBytes {
		t.Fatalf("size %d exceeds MinTotalBytes %d", got, MinTotalBytes)
	}
	if got := c.Blocks[1].Text; got != head {
		t.Errorf("log block below rendered floor = %q, want header only %q", got, head)
	}
}

// Review 4 Low 2: trailing Unicode whitespace must not enable the cut inside a run of "*" while the text that
// follows the run fits without it.
func TestTrailingUnicodeSpaceDoesNotSplitStarRun(t *testing.T) {
	const (
		head         = "hdr\n"
		wantRaw      = "end"
		wantRendered = wantRaw
	)
	for name, pad := range map[string]string{"ascii": " ", "no-break": "\u00a0", "ideographic": "\u3000"} {
		line := "password=" + strings.Repeat("*", 10) + " " + wantRaw + strings.Repeat(pad, 100) + "\n"
		got, _ := cutLog(line, 8)
		if got != wantRaw {
			t.Errorf("%s: cutLog = %q, want %q", name, got, wantRaw)
		}
		rgot := cutRendered(head, line, len(renderBody(head))+8)
		if rgot != wantRendered {
			t.Errorf("%s: cutRendered = %q, want %q", name, rgot, wantRendered)
		}
	}
	// Exact 4-star shape (a 3-byte "***" marker next to literal "*"): at n=7 (raw) and n=8 (rendered),
	// splitRuns=true would cut "****" into "*** end", while dropping trailing Unicode space first keeps "end" (a
	// tail never starts on a blank, which a reader drops as a continuation).
	fourStar := "password=****" + " " + wantRaw + strings.Repeat("\u00a0", 100) + "\n"
	if got, _ := cutLog(fourStar, 7); got != wantRaw {
		t.Errorf("4-star cutLog = %q, want %q", got, wantRaw)
	}
	if rgot := cutRendered(head, fourStar, len(renderBody(head))+8); rgot != wantRendered {
		t.Errorf("4-star cutRendered = %q, want %q", rgot, wantRendered)
	}
	if rgot := cutRendered(head, fourStar, len(renderBody(head))+8); rgot != wantRendered {
		t.Errorf("4-star cutRendered = %q, want %q", rgot, wantRendered)
	}
}
