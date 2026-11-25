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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/fake"
)

const testSidecar = "sidecar"

func TestKeepTailRenderedKeepsLongestFittingSuffix(t *testing.T) {
	const head = "hdr\n"
	want := "certificate expired\ncontext\n" + strings.Repeat(strings.Repeat(" ", 100)+"\n", 2) + "block\nend\n"
	rest := strings.Repeat("<", 50) + "\n" + want
	const budget = 42
	if n := len(renderBody(head + want)); n > budget {
		t.Fatalf("fixture renders to %d bytes, budget %d", n, budget)
	}
	if got := keepTailRendered(head, rest, budget); got != want {
		t.Errorf("kept suffix %q, want it to start with the certificate error", got)
	}
}

type logsWithWithheldRead struct {
	source *fake.Logs
	key    fake.LogKey
}

func (l logsWithWithheldRead) Logs(ctx context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
	if q.Namespace == l.key.Namespace && q.Pod == l.key.Pod && q.Container == l.key.Container && q.Previous == l.key.Previous {
		return providers.LogExcerpt{}, providers.ErrWithheld
	}
	return l.source.Logs(ctx, q)
}

func TestTotalCutKeepsWithheldLogTitleAndNotice(t *testing.T) {
	src := webSources()
	key := fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true}
	src.logs.Set(key, strings.Repeat(strings.Repeat("<", 50)+"\n", 100)+"tail-marker\n")
	objs := webObjects()
	crash := objs[2].(*corev1.Pod)
	crash.Status.ContainerStatuses = append(crash.Status.ContainerStatuses, corev1.ContainerStatus{Name: testSidecar})
	b := newBuilder(t, reader(t, objs...), src, func(c *Config) {
		c.Logs = logsWithWithheldRead{src.logs, fake.LogKey{Namespace: testNS, Pod: crashPod, Container: testSidecar}}
	})
	c, err := b.BuildWithin(t.Context(), incident(deploymentRef()), MinTotalBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Truncated || len(c.Render()) > MinTotalBytes {
		t.Errorf("rendered size %d, truncated %t", len(c.Render()), c.Truncated)
	}
	for _, blk := range c.Blocks {
		if blk.Section == SectionLogs && hasLabel(blk, LabelWithheld) {
			want := "pod " + crashPod + " container " + testSidecar + ", current instance\ncould not fetch: withheld by redaction\n"
			if blk.Text != want || blk.Availability != CouldNotFetch || len(blk.Data()) != 0 {
				t.Errorf("withheld log block has text %q, availability %q and %d evidence entries", blk.Text, blk.Availability, len(blk.Data()))
			}
			return
		}
	}
	t.Fatal("missing withheld log block")
}

func TestSectionCutKeepsWithheldLogTitleAndNotice(t *testing.T) {
	const budget = MinSectionBytes / MaxLogExcerpts
	const notice = "pod " + crashPod + " container " + appName + ", previous instance\ncould not fetch: withheld by redaction\n"
	logs := &fake.Logs{Now: func() time.Time { return testNow }}
	key := fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName}
	logs.Set(key, "")
	logs.SetInstance(key, providers.LogInstance{ContainerID: strings.Repeat("test-id.", 44)}, false)
	key.Previous = true
	b := newBuilder(t, reader(t), sources{}, func(c *Config) { c.Logs = logsWithWithheldRead{logs, key} })
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
	if !strings.HasSuffix(blk.Text, notice) || len(blk.Text) > budget || !hasLabel(blk, LabelWithheld) {
		t.Errorf("withheld block has %d bytes, labels %v, text %q", len(blk.Text), blk.Labels, blk.Text)
	}
}

// #1054: Context.Size equals len(Context.Render()), and BuildWithin bounds the rendered message even when
// escaping angle brackets expands the text about four times.
func TestRenderedSizeAndAngleExpansion(t *testing.T) {
	src := webSources()
	key := fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true}
	src.logs.Set(key, strings.Repeat(strings.Repeat("<", 50)+"\n", 400)+"tail-marker\n")
	b := newBuilder(t, reader(t, webObjects()...), src, nil)
	inc := incident(deploymentRef())

	for _, total := range []int{MinTotalBytes, 8192, DefaultTotalBytes} {
		c, err := b.BuildWithin(context.Background(), inc.DeepCopy(), total)
		if err != nil {
			t.Fatalf("total %d: %v", total, err)
		}
		rendered := c.Render()
		if c.Size() != len(rendered) {
			t.Errorf("total %d: Size() = %d, len(Render()) = %d", total, c.Size(), len(rendered))
		}
		if len(rendered) > total {
			t.Errorf("total %d: rendered context is %d bytes, over bound", total, len(rendered))
		}
		if !c.Truncated {
			t.Errorf("total %d: expected truncated context", total)
		}
		logs := one(t, c, SectionLogs)
		if !hasLabel(logs, LabelContextCut) {
			t.Errorf("total %d: logs missing LabelContextCut: %v", total, logs.Labels)
		}
		if !strings.HasPrefix(logs.Text, "pod web-abc-1 container app, previous instance\n") {
			t.Errorf("total %d: log cut lost header: %q", total, logs.Text)
		}
		if !strings.HasSuffix(logs.Text, "tail-marker\n") {
			t.Errorf("total %d: log cut lost tail marker: %q", total, logs.Text)
		}
	}
}

// #1054: fitTotal accounts for the bytes LabelContextCut adds to the opening tag, keeps the section cut order
// (logs, metrics, status, git, policy, summary; latest block of a section first), handles text without a
// trailing newline, and stays within the bound even when cutting splits a multi-line delimiter spelling.
func TestFitTotalRenderedEdgeCases(t *testing.T) {
	t.Run("cut label overhead stays within bound", func(t *testing.T) {
		c := &Context{Blocks: []Block{
			{Section: SectionSummary, Provenance: ProvenanceIncident, Availability: Available, Text: "summary\n"},
			{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available,
				Text: "hdr\n" + strings.Repeat("abcdefghij\n", 10), headLen: 4},
		}}
		// Set the bound 1 byte below the unlabelled rendered size so adding LabelContextCut (+24 bytes)
		// must also be paid for out of the log body.
		limit := c.Size() - 1
		b := &Builder{limits: Limits{Total: limit}}
		b.fitTotal(c)
		if !c.Truncated {
			t.Fatal("expected truncated")
		}
		if got := len(c.Render()); got != c.Size() || got > limit {
			t.Fatalf("rendered %d bytes (Size %d), over bound %d", got, c.Size(), limit)
		}
		if !hasLabel(c.Blocks[1], LabelContextCut) || hasLabel(c.Blocks[0], LabelContextCut) {
			t.Errorf("unexpected labels: summary=%v logs=%v", c.Blocks[0].Labels, c.Blocks[1].Labels)
		}
	})

	t.Run("cut order and latest block first", func(t *testing.T) {
		line := strings.Repeat("x", 99) + "\n"
		c := &Context{Blocks: []Block{
			{Section: SectionSummary, Provenance: ProvenanceIncident, Availability: Available, Text: strings.Repeat(line, 10)},
			{Section: SectionStatus, Provenance: ProvenanceObjectStatus, Availability: Available, Text: strings.Repeat(line, 10)},
			{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available, Text: "h1\n" + strings.Repeat(line, 10), headLen: 3},
			{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available, Text: "h2\n" + strings.Repeat(line, 10), headLen: 3},
			{Section: SectionMetrics, Provenance: ProvenanceMetric, Availability: Available, Text: strings.Repeat(line, 10)},
			{Section: SectionGit, Provenance: ProvenanceGit, Availability: NotAvailable, Text: strings.Repeat(line, 10)},
			{Section: SectionPolicy, Provenance: ProvenanceConfiguration, Availability: Available, Text: strings.Repeat(line, 10)},
		}}
		// Cut just enough to trim the second log block only.
		limit := c.Size() - 300
		b := &Builder{limits: Limits{Total: limit}}
		b.fitTotal(c)
		if got := len(c.Render()); got > limit {
			t.Fatalf("step 1: rendered %d over %d", got, limit)
		}
		if !hasLabel(c.Blocks[3], LabelContextCut) {
			t.Errorf("expected latest log block cut, labels=%v", c.Blocks[3].Labels)
		}
		for _, idx := range []int{0, 1, 2, 4, 5, 6} {
			if hasLabel(c.Blocks[idx], LabelContextCut) {
				t.Errorf("block %d (%s) cut too early", idx, c.Blocks[idx].Section)
			}
		}

		// Now tighten to MinTotalBytes: logs and metrics must be cut before summary is touched.
		b2 := &Builder{limits: Limits{Total: MinTotalBytes}}
		b2.fitTotal(c)
		if got := len(c.Render()); got > MinTotalBytes {
			t.Fatalf("step 2: rendered %d over %d", got, MinTotalBytes)
		}
		if !hasLabel(c.Blocks[2], LabelContextCut) || !hasLabel(c.Blocks[4], LabelContextCut) {
			t.Errorf("expected both log blocks and metrics cut: log1=%v metrics=%v", c.Blocks[2].Labels, c.Blocks[4].Labels)
		}
		if hasLabel(c.Blocks[0], LabelContextCut) {
			t.Errorf("summary cut while earlier sections still had room")
		}
	})

	t.Run("delimiter spellings and missing trailing newline", func(t *testing.T) {
		// Multi-line "context\n...\nblock\n" collapses when intact and expands if the closing line is dropped;
		// "contextblock" expands by 1 byte; text without trailing newline gets 1 byte added by RenderBlock.
		multi := "context\n" + strings.Repeat("   \n", 80) + "block\n" + strings.Repeat("contextblock\n", 100) + "no-newline"
		c := &Context{Blocks: []Block{
			{Section: SectionSummary, Provenance: ProvenanceIncident, Availability: Available, Text: "summary\n"},
			{Section: SectionStatus, Provenance: ProvenanceObjectStatus, Availability: Available, Text: multi},
		}}
		limit := len(ContextPreamble) + 1200
		b := &Builder{limits: Limits{Total: limit}}
		b.fitTotal(c)
		if !c.Truncated {
			t.Fatal("expected truncated")
		}
		if got := len(c.Render()); got != c.Size() || got > limit {
			t.Fatalf("rendered %d (Size %d) over %d", got, c.Size(), limit)
		}
	})

	t.Run("worst case empty wrappers fit MinTotalBytes", func(t *testing.T) {
		// Each section carries every label the builder can attach to it, plus LabelContextCut, with the
		// maximum number of log blocks (2 containers x 2 instances = 4).
		logLabels := []Label{
			LabelSectionCut, LabelContextCut, LabelLeadingLinesDropped,
			LabelLogTruncated, LabelLogEndMissing, LabelLogInstanceUncertain,
		}
		c := &Context{Blocks: []Block{
			{Section: SectionSummary, Provenance: ProvenanceIncident, Availability: Available,
				Labels: []Label{LabelSectionCut, LabelContextCut}},
			{Section: SectionStatus, Provenance: ProvenanceScaledObject, Availability: Available,
				Labels: []Label{LabelSectionCut, LabelContextCut, LabelPodsUncertain, LabelPodsCapped, LabelIncomplete}},
			{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available, Labels: logLabels},
			{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available, Labels: logLabels},
			{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available, Labels: logLabels},
			{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available, Labels: logLabels},
			{Section: SectionMetrics, Provenance: ProvenanceMetric, Availability: Available,
				Labels: []Label{LabelSectionCut, LabelContextCut, LabelMetricsPartial, LabelPodsUncertain, LabelPodsCapped, LabelIncomplete}},
			{Section: SectionGit, Provenance: ProvenanceGit, Availability: NotAvailable,
				Labels: []Label{LabelContextCut}},
			{Section: SectionPolicy, Provenance: ProvenanceConfiguration, Availability: Available,
				Labels: []Label{LabelSectionCut, LabelContextCut, LabelIncomplete}},
		}}
		if got := c.Size(); got >= MinTotalBytes {
			t.Fatalf("empty wrappers take %d bytes, must stay below MinTotalBytes (%d)", got, MinTotalBytes)
		}
	})
}
