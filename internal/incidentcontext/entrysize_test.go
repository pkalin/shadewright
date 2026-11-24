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
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// sizeRecorder is the package's redactor that records the size of every entry the build hands it: the pieces of
// a section (redact.PiecesSize) or a text.
type sizeRecorder struct {
	r     *redact.Redactor
	mu    sync.Mutex
	sizes []int
}

func (c *sizeRecorder) record(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sizes = append(c.sizes, n)
}

func (c *sizeRecorder) String(s string) string { c.record(len(s)); return c.r.String(s) }
func (c *sizeRecorder) StringChecked(s string) (string, bool) {
	c.record(len(s))
	return c.r.StringChecked(s)
}
func (c *sizeRecorder) StringTwiceChecked(s string, cut func(string) int) (string, bool) {
	c.record(len(s))
	return c.r.StringTwiceChecked(s, cut)
}
func (c *sizeRecorder) ReleasedText(s string) string { c.record(len(s)); return c.r.ReleasedText(s) }
func (c *sizeRecorder) PiecesWithin(ps []redact.Piece, limit int) ([]string, bool) {
	c.record(redact.PiecesSize(ps))
	return c.r.PiecesWithin(ps, limit)
}

// echoRecorder is a redactor that hides nothing and records the size of every entry it is handed, so that a test
// of the bound does not pay for a redaction of its size.
type echoRecorder struct{ sizes []int }

func (c *echoRecorder) String(s string) string { return s }
func (c *echoRecorder) StringChecked(s string) (string, bool) {
	c.sizes = append(c.sizes, len(s))
	return s, true
}
func (c *echoRecorder) PiecesWithin(ps []redact.Piece, _ int) ([]string, bool) {
	c.sizes = append(c.sizes, redact.PiecesSize(ps))
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Text
	}
	return out, true
}

// imagePullPods returns n pods of the web fixture, each with four containers whose image names of 180 KiB are
// quoted in their waiting messages, as the kubelet quotes the image of a pull that fails: a pod of about 1.5 MiB,
// the size etcd stores at most, with the image names once in its spec and once in its status. Kubernetes
// validates neither the length of an image name nor that of a waiting message (core validation, image: not empty,
// no surrounding blanks), so a tenant who may create pods makes these. Each status line also holds a secret, which
// the context never shows.
func imagePullPods(n int) []client.Object {
	objs := webObjects()
	rs := objs[1]
	ref := objs[2].GetOwnerReferences()
	out := make([]client.Object, 0, 2+n)
	out = append(out, objs[0], rs)
	for i := range n {
		p := pod(fmt.Sprintf("web-abc-%d", i+1), rs.GetLabels(), longAgo, ref...)
		p.Status.ContainerStatuses = nil
		for c := range 4 {
			img := "registry.example/" + strings.Repeat("a", 180<<10)
			name := fmt.Sprintf("c%d", c)
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: name, Image: img})
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: name,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff",
					Message: "Back-off pulling image \"" + img + "\" with " + leakDSN}}})
		}
		out = append(out, p)
	}
	return out
}

// labelReason is the label of the waiting reason in the answer of container_waiting_reason.
const labelReason = "reason"

// widestSources answers every metrics template with MaxSeries-sized answers of label values at the Prometheus
// adapter's bound (253 bytes), and the log source with the most it may return (twice what was asked).
func widestSources() sources {
	s := webSources()
	for _, pl := range metricPlans {
		series := make([]providers.Series, 0, 50)
		for i := range 50 {
			series = append(series, providers.Series{Labels: map[string]string{
				labelPod: fmt.Sprintf("%s%03d", strings.Repeat("p", 250), i), labelContainer: strings.Repeat("c", 253),
				labelReason: strings.Repeat("r", 253)}, Samples: []providers.Sample{{Time: testNow, Value: 1}}})
		}
		s.metrics.Set(pl.template, series)
	}
	return s
}

// widestLimits are the largest section and total bounds a builder accepts.
func widestLimits(c *Config) {
	c.Limits = Limits{Total: MaxLimitBytes, Summary: MaxLimitBytes, Status: MaxLimitBytes, Logs: MaxLimitBytes,
		Metrics: MaxLimitBytes, Git: MaxLimitBytes, Policy: MaxLimitBytes}
}

// An entry of the build at redact.LargestEntry bytes is decided; one byte more is withheld before the redactor
// reads it, with the reason size (#1106, decision of 2026-11-22): the pieces of a section (decidedPieces) and a
// text (checkedTwice, the log header and excerpt).
func TestEntryOverTheLargestEntryIsWithheld(t *testing.T) {
	for _, size := range []int{redact.LargestEntry, redact.LargestEntry + 1} {
		over := size > redact.LargestEntry
		text := strings.Repeat("a", size-1) + "\n"
		rec := &echoRecorder{}
		before := withheldForSize(t)
		_, withheld := decidedPieces(rec, frag{{Piece: redact.Piece{Text: text}}})
		_, ok := checkedTwice(rec, text, nil)
		if withheld != over || ok == over {
			t.Errorf("%d bytes: pieces withheld %v, text decided %v; want withheld %v", size, withheld, ok, over)
		}
		if want := map[bool]int{false: 2, true: 0}[over]; len(rec.sizes) != want {
			t.Errorf("%d bytes: the redactor read %d entries, want %d", size, len(rec.sizes), want)
		}
		if d, want := withheldForSize(t)-before, map[bool]float64{false: 0, true: 2}[over]; d != want {
			t.Errorf("%d bytes: %v entries withheld for size, want %v", size, d, want)
		}
	}
}

// The context build never hands redaction an entry larger than the largest entry the heap of one redaction was
// measured for (redact.LargestEntry; #1106, decision of 2026-11-22). The status section decides the messages of up
// to MaxPods pods at once, each an object of up to about 1.5 MiB, with no bound of the builder before the
// decision: three pods of image pull messages make it about 2.2 MB (MaxPods of them 37 MB, past redact.MaxEntry).
// It is withheld whole before the redactor reads it, with the reason size: the block is could not fetch and the
// status section a failed source. Every other entry of the build stays under the bound, at the widest section
// bounds the builder accepts and the widest answers of the sources.
func TestStatusSectionOverTheLargestEntryIsWithheld(t *testing.T) {
	if raceDetector {
		t.Skip("decides metrics answers of about 330 KB: checked by the run without the race detector")
	}
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	rec := &sizeRecorder{r: red}
	scene := &buildScene{objects: imagePullPods(3)}
	b := newBuilder(t, sceneReader{scene: scene}, widestSources(), func(cfg *Config) {
		cfg.Redactor = rec
		widestLimits(cfg)
	})
	before := withheldForSize(t)
	ctx := build(t, b, incident(deploymentRef()))
	if got := slices.Max(rec.sizes); got > redact.LargestEntry {
		t.Errorf("an entry of %d bytes reached the redactor, over %d", got, redact.LargestEntry)
	}
	blk := one(t, ctx, SectionStatus)
	if blk.Availability != CouldNotFetch || blk.Text != withheldText || !hasLabel(blk, LabelWithheld) ||
		len(blk.Values) != 0 || !slices.Contains(ctx.FailedSources, SectionStatus) || !ctx.Escalate() {
		t.Errorf("status %q, labels %v, failed sources %v, text %q; want withheld", blk.Availability, blk.Labels,
			ctx.FailedSources, blk.Text[:min(len(blk.Text), 200)])
	}
	if d := withheldForSize(t) - before; d != 1 {
		t.Errorf("%v entries withheld for size, want 1", d)
	}
	for _, blk := range ctx.Blocks {
		if strings.Contains(blk.Text, "Zq8-synthetic-Pw0rd") || strings.Contains(blk.Text, "aaaa") {
			t.Errorf("section %q shows the withheld text", blk.Section)
		}
	}
}

// The heap of the largest builds a tenant can cause stays within what the heap limit counts the largest entry for
// (redact.HeapFixed plus redact.HeapWeightPerByte for each byte of redact.LargestEntry): three pods of about
// 1.5 MiB of image pull messages (a status section under redact.MaxEntry, which only this bound withholds) and
// MaxPods of them. The status section is withheld unread. The reader hands out copies of the pods, as the
// analyzer's cache does. Without the race detector only.
func TestLargestBuildHeap(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector's shadow memory is no heap of the product")
	}
	line := uint64(redact.HeapFixed + redact.HeapWeightPerByte*redact.LargestEntry)
	for _, n := range []int{3, MaxPods} {
		scene := &buildScene{objects: imagePullPods(n)}
		b := newBuilder(t, sceneReader{scene: scene}, widestSources(), widestLimits)
		var blk Block
		peak := redacttest.PeakLiveHeap(func() { blk = one(t, build(t, b, incident(deploymentRef())), SectionStatus) })
		t.Logf("%d pods: peak heap %.1f MiB, line %.1f MiB", n, float64(peak)/(1<<20), float64(line)/(1<<20))
		if blk.Availability != CouldNotFetch {
			t.Errorf("%d pods: status %q, want withheld", n, blk.Availability)
		}
		if peak > line {
			t.Errorf("%d pods: peak heap %d bytes, over %d", n, peak, line)
		}
	}
}

// withheldForSize returns the count of entries redaction withheld with the reason size.
func withheldForSize(t *testing.T) float64 {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "shadewright_redaction_withheld_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == redact.ReasonSize {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatal("no series of shadewright_redaction_withheld_total with the reason size")
	return 0
}
