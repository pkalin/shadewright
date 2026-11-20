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
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/pkalin/shadewright/internal/providers"
)

// MaxLogExcerpts bounds the containers whose logs the log section reads; the section's bound is split evenly
// between them.
const MaxLogExcerpts = 2

// minSecondRead is the least of a container's share of the log bound that must be left after the excerpt of
// its terminated current instance for the builder to read its previous instance as well.
const minSecondRead = 1 << 10

// logPick is one container whose logs the builder reads.
type logPick struct {
	namespace, pod, container string
	init                      bool
	// previous: the previous instance is read first; otherwise the current one.
	previous bool
	// other: the instance not read first exists, and is read when the first gives no lines.
	other bool
	// currentDead: the current instance has terminated; its log is the newest dead instance's.
	currentDead bool
	score       int
	restarts    int32
}

// logPicks chooses the containers to read, most relevant first: one whose last instance terminated inside
// the window, then one that waits with a reason or terminated with an error, then one that is not ready. A
// termination that finished before the window, and a container that ran to completion (a finished init
// container), are not trouble. When no container shows trouble, the first regular container of the most
// troubled pod is read.
//
// Which instance is read first: for a container whose current instance has terminated, that instance, the
// newest dead one (the kubelet keeps one terminated instance per container, so the one before it is usually
// gone); for any other container whose last instance terminated inside the window, the previous instance,
// whose tail holds the lines before the termination (while a container waits, the kubelet serves that same
// instance for both); otherwise the current one.
func logPicks(t *target, w window) []logPick {
	var all []logPick
	for i := range t.pods {
		p := &t.pods[i]
		for _, cs := range allStatuses(p) {
			last := cs.LastTerminationState.Terminated
			pick := logPick{
				namespace: p.Namespace, pod: p.Name, container: cs.Name, init: cs.init, restarts: cs.RestartCount,
				currentDead: cs.State.Terminated != nil,
			}
			switch {
			case last != nil && last.FinishedAt.After(w.start):
				pick.score, pick.previous = 3, !pick.currentDead
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "",
				cs.State.Terminated != nil && !completed(cs):
				pick.score = 2
			case !cs.Ready && !completed(cs):
				pick.score = 1
			}
			// The current instance always exists; a previous one only after a termination.
			pick.other = pick.previous || last != nil
			all = append(all, pick)
		}
	}
	// The pods are already most troubled first; the stable sort keeps that order between equal containers.
	slices.SortStableFunc(all, func(a, b logPick) int {
		return cmp.Or(-cmp.Compare(a.score, b.score), -cmp.Compare(a.restarts, b.restarts))
	})
	n := 0
	for n < len(all) && n < MaxLogExcerpts && all[n].score > 0 {
		n++
	}
	if n > 0 {
		return all[:n]
	}
	for _, pick := range all {
		if !pick.init {
			return []logPick{pick}
		}
	}
	return all[:min(1, len(all))]
}

func notAvailable(sec Section, prov Provenance, text string) Block {
	return Block{Section: sec, Provenance: prov, Availability: NotAvailable, Text: text}
}

// logBlocks reads and bounds the log excerpts. It reports whether the log source failed, and returns an
// error only when ctx is done.
func (b *Builder) logBlocks(ctx context.Context, t *target, w window) ([]Block, bool, error) {
	if b.logs == nil {
		return []Block{notAvailable(SectionLogs, ProvenanceLogExcerpt, "not available: no log source is configured\n")}, false, nil
	}
	picks := logPicks(t, w)
	if t.state != targetResolved || len(picks) == 0 {
		return []Block{notAvailable(SectionLogs, ProvenanceLogExcerpt, "not available: no pod of the target to read logs from\n")}, false, nil
	}
	budget := b.limits.Logs / len(picks)
	since := w.end.Sub(w.start).Round(time.Second)
	blocks := make([]Block, 0, len(picks))
	failed := false
	for _, pick := range picks {
		blks, bad, err := b.pickBlocks(ctx, pick, since, budget)
		if err != nil {
			return nil, false, err
		}
		failed = failed || bad
		for i := range blks {
			b.podLabels(&blks[i], t)
		}
		blocks = append(blocks, blks...)
	}
	return blocks, failed, nil
}

// pickBlocks reads the logs of one container in at most two reads, within budget bytes in all: the instance
// [logPicks] chose first, then the other instance when the first gave no lines, or, after the excerpt of a
// terminated current instance, the previous instance when at least minSecondRead bytes are left. Each excerpt
// is a block whose header names its instance. A read that gave no lines leaves its header lines (the instance
// and the fixed reason) before the first excerpt, or, when no read gave lines, a block of those lines alone,
// marked [CouldNotFetch] when a read failed. It reports whether the log source failed: a read failed and none
// gave an excerpt. It returns an error only when ctx is done.
func (b *Builder) pickBlocks(ctx context.Context, pick logPick, since time.Duration, budget int) ([]Block, bool, error) {
	order := []bool{pick.previous}
	if pick.other {
		order = append(order, !pick.previous)
	}
	var out []Block
	var notes strings.Builder
	left, readFailed := budget, false
	for i, previous := range order {
		if i > 0 && (left <= 0 || len(out) > 0 && (!pick.currentDead || left < minSecondRead)) {
			break
		}
		q := providers.LogQuery{
			Namespace: pick.namespace, Pod: pick.pod, Container: pick.container, Previous: previous,
			MaxBytes: int64(left),
		}
		if !previous {
			// The previous instance stopped at some time in the past: no window for it.
			q.Since = since
		}
		var ex providers.LogExcerpt
		err := b.call(ctx, func(ctx context.Context) error {
			var err error
			ex, err = b.logs.Logs(ctx, q)
			return err
		})
		if err != nil {
			if a := abandoned(ctx); a != nil {
				return nil, false, a
			}
		}
		read := pick
		read.previous = previous
		blk, v := b.renderBlock(read, ex, err, left)
		left -= len(blk.Text)
		if v.Read == LogReadOK {
			out = append(out, blk)
			continue
		}
		reason := logReadReasons[noLinesReason(v.Read, err)]
		logReadsWithoutLines.WithLabelValues(reason).Inc()
		logf.FromContext(ctx).Info("Read no log lines of a container instance for the incident context",
			"namespace", pick.namespace, "pod", pick.pod, "container", pick.container,
			"instance", instanceName(previous), "reason", reason)
		readFailed = readFailed || v.Read == LogReadFailed || v.Read == LogReadOversized
		notes.WriteString(blk.Text)
	}
	if len(out) == 0 {
		blk := Block{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available, Text: notes.String()}
		if readFailed {
			blk.Availability = CouldNotFetch
		}
		blk.headLen = len(blk.Text)
		return []Block{blk}, readFailed, nil
	}
	out[0].Text, out[0].headLen = notes.String()+out[0].Text, notes.Len()+out[0].headLen
	return out, false, nil
}

// instanceName names an instance in a block header and in the log.
func instanceName(previous bool) string {
	if previous {
		return "previous instance"
	}
	return "current instance"
}

// logBlock turns one answer of the log source for the instance pick.previous names into a block
// ([Builder.renderBlock]). It reports whether the source failed.
func (b *Builder) logBlock(pick logPick, ex providers.LogExcerpt, err error, budget int) (Block, bool) {
	blk, v := b.renderBlock(pick, ex, err, budget)
	failed := v.Read == LogReadFailed || v.Read == LogReadOversized
	if failed {
		blk.Availability = CouldNotFetch
	}
	return blk, failed
}

// renderBlock turns one answer of the log source for the instance pick.previous names into a block: a header
// naming the container and instance, then the redacted excerpt cut to its newest lines ([RenderLog]).
func (b *Builder) renderBlock(pick logPick, ex providers.LogExcerpt, err error, budget int) (Block, LogView) {
	blk := Block{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available}
	kind := "container"
	if pick.init {
		kind = "init container"
	}
	title := "pod " + b.value(pick.pod) + " " + kind + " " + b.value(pick.container) + ", " + instanceName(pick.previous)
	v := RenderLog(b.redactor.String, title, ex, err, budget, budget)
	for _, m := range []struct {
		on bool
		l  Label
	}{
		{v.LeadingDropped, LabelLeadingLinesDropped}, {v.Truncated, LabelLogTruncated},
		{v.EndMissing, LabelLogEndMissing}, {v.InstanceUncertain, LabelLogInstanceUncertain}, {v.Cut, LabelSectionCut},
	} {
		if m.on {
			blk.addLabel(m.l)
		}
	}
	blk.Text, blk.headLen = v.Text, v.HeadLen
	return blk, v
}

// Reasons of a read without lines, indexes into logReadReasons.
const (
	reasonLogGone = iota
	reasonPodNotFound
	reasonRejected
	reasonForbidden
	reasonFailed
	reasonOversized
	reasonEmpty
)

// logReadReasons are every value of the label "reason" of logReadsWithoutLines, by reason.
var logReadReasons = []string{
	reasonLogGone: "collected", reasonPodNotFound: "pod_not_found", reasonRejected: "rejected",
	reasonForbidden: "forbidden", reasonFailed: "failed", reasonOversized: "oversized", reasonEmpty: "empty",
}

// logReasonLabelName is the label of logReadsWithoutLines.
const logReasonLabelName = "reason"

// logReadsWithoutLines counts the log reads of the context builder that gave no lines, by reason. Its series
// start at zero.
var logReadsWithoutLines = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analyzer_context_log_reads_without_lines_total",
	Help: "Log reads of the incident context builder that gave no lines, by reason (collected, pod_not_found, " +
		"rejected, forbidden, failed, oversized, empty).",
}, []string{logReasonLabelName})

func init() {
	metrics.Registry.MustRegister(logReadsWithoutLines)
	for _, r := range logReadReasons {
		logReadsWithoutLines.WithLabelValues(r)
	}
}

// noLinesReason is the reason of a read that gave no lines, as [RenderLog] classified it: an index into
// logReadReasons.
func noLinesReason(read LogRead, err error) int {
	switch read {
	case LogReadNotFound:
		if errors.Is(err, providers.ErrLogGone) {
			return reasonLogGone
		}
		return reasonPodNotFound
	case LogReadRejected:
		return reasonRejected
	case LogReadFailed:
		if errors.Is(err, providers.ErrForbidden) {
			return reasonForbidden
		}
		return reasonFailed
	case LogReadOversized:
		return reasonOversized
	default:
		return reasonEmpty
	}
}
