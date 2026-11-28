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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/logs/kube"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

// javaOOM is a crash log whose newest lines are the frames of one stack trace: the first line of the entry, then
// many "\tat" frames.
func javaOOM(frames int) (log, header, lastFrame string) {
	header = "Exception in thread \"main\" java.lang.OutOfMemoryError: Java heap space"
	var b strings.Builder
	b.WriteString("starting app\n" + header + "\n")
	for i := range frames {
		lastFrame = fmt.Sprintf("\tat com.example.Cache.fill(Cache.java:%d)", i)
		b.WriteString(lastFrame + "\n")
	}
	return b.String(), header, lastFrame
}

// A log that ends in the frames of a stack trace gives an excerpt that starts at the first line of their entry
// and ends with the newest frame, the frames between left out, on the source's cut and on RenderLog's; when that
// first line does not fit with a frame, the newest frames are kept rather than nothing (decision of 2026-11-28
// on #1055). Before, the cut kept only frames and the reader dropped them all: an empty excerpt.
func TestStackTraceTailKeepsItsFirstLine(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	log, header, last := javaOOM(200)
	const title = "pod web-abc-1 container app, previous instance"
	for _, keep := range []int{200, 1000} {
		ex, err := kube.ExcerptOf(red, []byte(log), false, keep)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(ex.Text, header+"\n\tat ") || !strings.HasSuffix(ex.Text, last+"\n") || len(ex.Text) > keep ||
			!ex.HeaderKept || ex.HeaderLeftOut {
			t.Errorf("keep %d: source excerpt %q, header kept %t", keep, clip(ex.Text), ex.HeaderKept)
		}
		ex.CollectedAt = testNow
		v := RenderLog(red, false, title, ex, nil, keep, 4<<10)
		if body := v.Text[v.HeadLen:]; !strings.HasPrefix(body, header+"\n") || !strings.HasSuffix(body, last+"\n") ||
			!v.HeaderKept || v.LeadingDropped {
			t.Errorf("keep %d: RenderLog body %q, header kept %t, leading dropped %t", keep, clip(body), v.HeaderKept, v.LeadingDropped)
		}
		// RenderLog's own cut of a whole answer within twice what was asked.
		whole, _, wholeLast := javaOOM(keep * 3 / 2 / 42)
		if len(whole) <= keep || len(whole) > 2*keep {
			t.Fatalf("keep %d: whole answer of %d bytes", keep, len(whole))
		}
		w := RenderLog(red, false, title, providers.LogExcerpt{Text: whole, CollectedAt: testNow}, nil, keep, 4<<10)
		if body := w.Text[w.HeadLen:]; !strings.HasPrefix(body, header+"\n") || !strings.HasSuffix(body, wholeLast+"\n") || !w.HeaderKept {
			t.Errorf("keep %d: RenderLog cut %q", keep, clip(body))
		}
	}
	// The first line does not fit with a frame: the newest frames, kept by the reader.
	keep := len(header) + 20
	ex, err := kube.ExcerptOf(red, []byte(log), false, keep)
	if err != nil {
		t.Fatal(err)
	}
	ex.CollectedAt = testNow
	v := RenderLog(red, false, title, ex, nil, keep, 4<<10)
	if body := v.Text[v.HeadLen:]; !ex.HeaderLeftOut || !strings.HasSuffix(body, last+"\n") || !v.HeaderLeftOut || v.LeadingDropped {
		t.Errorf("header left out: source %q (left out %t), body %q", ex.Text, ex.HeaderLeftOut, body)
	}
}

// A withheld second read keeps its notice within the container's share even when the first excerpt's header
// and the withheld title together are longer than the share: the bodyless first excerpt of the current instance
// gives way (decision of 2026-11-28 on #1055: the section never exceeds Limits.Logs). Before, the notice was
// added whole over the share.
func TestWithheldNoticeStaysWithinTheShare(t *testing.T) {
	const share = 512
	b := newBuilder(t, reader(t), sources{}, nil)
	pod := crashPod + "-" + strings.Repeat("a", 190)
	pick := logPick{namespace: testNS, pod: pod, container: appName, other: true, currentDead: true}
	first, _ := b.renderBlock(pick, providers.LogExcerpt{Text: strings.Repeat("line\n", 40) + "exit 1\n", CollectedAt: testNow},
		nil, share-100)
	if first.headLen == 0 || !strings.HasSuffix(first.Text, "exit 1\n") {
		t.Fatalf("first excerpt %q", clip(first.Text))
	}
	r := containerReads{pick: pick, budget: share, left: share - len(first.Text), askLeft: previousReserve(share), reads: 1,
		out: []Block{first}}
	b.recordRead(t.Context(), &r, true, providers.LogExcerpt{}, fmt.Errorf("the log excerpt: %w", providers.ErrWithheld),
		r.queryBudget())
	text := ""
	for _, blk := range r.blocks() {
		text += blk.Text
	}
	if len(text) > share || r.left < 0 || !strings.Contains(text, withheldText) {
		t.Errorf("container %d bytes over its share %d (left %d), or no notice:\n%s", len(text), share, r.left, text)
	}
}

// A second read that fails or is withheld after a previous instance's excerpt never cuts that excerpt: the
// previous instance ranks higher (decision of 2026-11-26), and the notice keeps its room another way. Before,
// the room came from the previous body.
func TestSecondReadNoticeLeavesThePreviousExcerpt(t *testing.T) {
	const share = 1024
	for name, readErr := range map[string]error{
		"withheld read": fmt.Errorf("the log excerpt: %w", providers.ErrWithheld),
		"failed read":   errors.New("connection reset"),
	} {
		t.Run(name, func(t *testing.T) {
			b := newBuilder(t, reader(t), sources{}, nil)
			pick := logPick{namespace: testNS, pod: crashPod, container: appName, previous: true, other: true, currentDead: true}
			prev := pick
			prev.previous = true
			first, _ := b.renderBlock(prev, providers.LogExcerpt{Text: strings.Repeat("line of the previous instance\n", 25) +
				"exit 1\n", CollectedAt: testNow}, nil, share-previousReserve(share))
			first.previous = true
			body := first.Text[first.headLen:]
			if !strings.HasSuffix(body, "exit 1\n") {
				t.Fatalf("first excerpt %q", clip(first.Text))
			}
			// Less left than the notice with its title needs (the reads leave at least previousReserve before a
			// second read, wantsSecond; the cut of the room is what is checked here).
			r := containerReads{pick: pick, budget: share, left: share - len(first.Text), askLeft: previousReserve(share), reads: 1,
				out: []Block{first}}
			r.left = min(r.left, 60)
			b.recordRead(t.Context(), &r, false, providers.LogExcerpt{}, readErr, r.queryBudget())
			blks := r.blocks()
			text := ""
			for _, blk := range blks {
				text += blk.Text
			}
			if len(text) > share || r.left < 0 {
				t.Errorf("container %d bytes, over its share %d", len(text), share)
			}
			if got := blks[0].Text[blks[0].headLen:]; got != body {
				t.Errorf("previous excerpt cut from %d to %d bytes", len(body), len(got))
			}
			if name == "withheld read" && !strings.Contains(text, withheldText) {
				t.Errorf("notice missing:\n%s", text)
			}
		})
	}
}

// The floor of a previous instance's final lines compares the evidence the cut kept, without trailing blank
// lines, with the floor: a cut that kept the last line and many blank lines after it does not count as holding
// the final lines (finding of 2026-11-28 on #1055). Before, the blank bytes made the kept body longer than the
// floor, and none of the nine lines before the last survived.
func TestPreviousFloorIgnoresTrailingBlankLines(t *testing.T) {
	const head = "pod web-abc-1 container app, previous instance\n"
	body := strings.Repeat("older noise line\n", 30)
	for i := range 9 {
		body += fmt.Sprintf("line%d\n", i)
	}
	body += "exit\n" + strings.Repeat("\n", 64)
	blk := Block{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available, Text: head + body,
		headLen: len(head), previous: true}
	current := renderedBlockLen(0, blk)
	budget := len(renderBody(head)) + len("exit\n") + 64
	if !trimLogBlock(0, &blk, current, budget, 4<<10, previousTailLines) {
		t.Fatal("block not trimmed")
	}
	kept := blk.Text[blk.headLen:]
	for i := range 9 {
		if !strings.Contains(kept, fmt.Sprintf("line%d\n", i)) {
			t.Fatalf("final line %d lost: kept %q", i, kept)
		}
	}
}
