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

// logBudgetCalls is the time budget of the log section in call timeouts: the most it could take with one read
// per container. The first reads take at most one call timeout each; the second reads share what is left.
const logBudgetCalls = MaxLogExcerpts

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
// termination that finished before the window, a container that ran to completion (a finished init
// container) and one stopped by its pod's deletion ([shutdown]) are not trouble. When no container shows
// trouble, the first regular container of the most troubled pod is read.
//
// Which instance is read first: for a container whose current instance has terminated, that instance, the
// newest dead one (the kubelet keeps one terminated instance per container, so the one before it is usually
// gone); for any other container whose last instance terminated inside the window, the previous instance,
// whose tail holds the lines before the termination (while a container waits, the kubelet serves that same
// instance for both), and so for a container whose current instance was stopped by its pod's deletion: the
// instance that terminated before it is the one in trouble; otherwise the current one.
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
				pick.score, pick.previous = 3, !pick.currentDead || shutdown(p, cs)
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "",
				cs.State.Terminated != nil && !settled(p, cs):
				pick.score = 2
			case !cs.Ready && !settled(p, cs):
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

// logBlocks reads and bounds the log excerpts. Every container's first read comes before any second read, in
// the order of [logPicks], each with the call timeout of its own: the reads and deadlines of one read per
// container. The second reads ([containerReads.wantsSecond]) follow in the same order, within what is left of
// a time budget of logBudgetCalls call timeouts for the whole section, counted from its start; none starts
// once that time is up. It reports whether the log source failed, which only a first read decides, and
// returns an error only when ctx is done.
func (b *Builder) logBlocks(ctx context.Context, t *target, w window) ([]Block, bool, error) {
	if b.logs == nil {
		return []Block{notAvailable(SectionLogs, ProvenanceLogExcerpt, "not available: no log source is configured\n")}, false, nil
	}
	picks := logPicks(t, w)
	if t.state != targetResolved || len(picks) == 0 {
		return []Block{notAvailable(SectionLogs, ProvenanceLogExcerpt, "not available: no pod of the target to read logs from\n")}, false, nil
	}
	second := withBudget(ctx, logBudgetCalls*b.timeout)
	budget := b.limits.Logs / len(picks)
	since := w.end.Sub(w.start).Round(time.Second)
	reads := make([]containerReads, len(picks))
	for i, pick := range picks {
		reads[i] = containerReads{pick: pick, left: budget}
		if err := b.readInstance(ctx, &reads[i], pick.previous, since); err != nil {
			return nil, false, err
		}
	}
	for i := range reads {
		if !reads[i].wantsSecond() {
			continue
		}
		if outOfTime(second) {
			break
		}
		if err := b.readInstance(second, &reads[i], !reads[i].pick.previous, since); err != nil {
			return nil, false, err
		}
	}
	blocks := make([]Block, 0, len(picks))
	failed := false
	for i := range reads {
		blks := reads[i].blocks()
		failed = failed || reads[i].failed
		for j := range blks {
			b.podLabels(&blks[j], t)
		}
		blocks = append(blocks, blks...)
	}
	return blocks, failed, nil
}

// containerReads is what the reads of one container gave: at most two reads, within its share of the log
// bound in all.
type containerReads struct {
	pick logPick
	// reads counts the reads made, left is what is left of the container's share of the bound.
	reads, left int
	// out are the excerpts, in the order read; notes the header lines of the reads that gave no lines.
	out   []Block
	notes strings.Builder
	// failed: the first read failed (the source failed, timed out, refused the identity or answered
	// oversized), or a read was withheld by redaction. A second read that failed otherwise is only named in
	// notes.
	failed bool
	// withheld: redaction withheld a read whole (#873); its blocks carry LabelWithheld.
	withheld bool
}

// wantsSecond reports whether the container's other instance is read after its first read: the other
// instance exists, the first read did not fail (a failing source costs one call per container), bytes are
// left, and the first read gave no lines (collected, refused for that instance, pod not found or empty) or
// it gave the excerpt of a terminated current instance and at least minSecondRead bytes are left.
func (r *containerReads) wantsSecond() bool {
	return r.reads == 1 && r.pick.other && !r.failed && r.left > 0 &&
		(len(r.out) == 0 || r.pick.currentDead && r.left >= minSecondRead)
}

// blocks returns the container's blocks: each excerpt in a block whose header names its instance, the header
// lines of the reads that gave no lines before the first excerpt, or, when no read gave lines, a block of
// those lines alone, marked [CouldNotFetch] when the first read failed. A second read that failed is named in
// the header lines; it does not make the block or the section failed, since one read per container would
// not have made it.
func (r *containerReads) blocks() []Block {
	if len(r.out) == 0 {
		blk := Block{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available, Text: r.notes.String()}
		if r.failed {
			blk.Availability = CouldNotFetch
		}
		if r.withheld {
			blk.addLabel(LabelWithheld)
		}
		blk.headLen = len(blk.Text)
		return []Block{blk}
	}
	out := slices.Clone(r.out)
	out[0].Text, out[0].headLen = r.notes.String()+out[0].Text, r.notes.Len()+out[0].headLen
	if r.withheld {
		out[0].addLabel(LabelWithheld)
	}
	return out
}

// readInstance reads one instance of r's container, the previous one when previous is set, asking for no
// more than what is left of its share, and records the answer in r. It returns an error only when ctx is
// done.
func (b *Builder) readInstance(ctx context.Context, r *containerReads, previous bool, since time.Duration) error {
	pick := r.pick
	q := providers.LogQuery{
		Namespace: pick.namespace, Pod: pick.pod, Container: pick.container, Previous: previous,
		MaxBytes: int64(r.left),
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
			return a
		}
	}
	r.reads++
	read := pick
	read.previous = previous
	blk, v := b.renderBlock(read, ex, err, r.left)
	r.left -= len(blk.Text)
	if v.Read == LogReadOK {
		r.out = append(r.out, blk)
		return nil
	}
	reason := logReadReasons[noLinesReason(v.Read, err)]
	logReadsWithoutLines.WithLabelValues(reason).Inc()
	logf.FromContext(ctx).Info("Read no log lines of a container instance for the incident context",
		"namespace", pick.namespace, "pod", pick.pod, "container", pick.container,
		"instance", instanceName(previous), "reason", reason)
	r.notes.WriteString(blk.Text)
	if r.reads == 1 && (v.Read == LogReadFailed || v.Read == LogReadOversized) {
		r.failed = true
	}
	if v.Read == LogReadWithheld {
		// A withheld read is a failed source whichever read it is, as the decision of 2026-11-20 on #873 has
		// it for every caller of the log source.
		r.failed, r.withheld = true, true
	}
	return nil
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
	failed := v.Read == LogReadFailed || v.Read == LogReadOversized || v.Read == LogReadWithheld
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
	title := "pod " + b.shown(pick.pod) + " " + kind + " " + b.shown(pick.container) + ", " + instanceName(pick.previous)
	v := RenderLog(b.redactor, false, title, ex, err, budget, budget)
	if v.Read == LogReadWithheld {
		blk.Availability = CouldNotFetch
		blk.addLabel(LabelWithheld)
	}
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
	reasonWithheld
)

// logReadReasons are every value of the label "reason" of logReadsWithoutLines, by reason.
var logReadReasons = []string{
	reasonLogGone: "collected", reasonPodNotFound: "pod_not_found", reasonRejected: "rejected",
	reasonForbidden: "forbidden", reasonFailed: "failed", reasonOversized: "oversized", reasonEmpty: "empty",
	reasonWithheld: "withheld",
}

// logReasonLabelName is the label of logReadsWithoutLines.
const logReasonLabelName = "reason"

// logReadsWithoutLines counts the log reads of the context builder that gave no lines, by reason. Its series
// start at zero.
var logReadsWithoutLines = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analyzer_context_log_reads_without_lines_total",
	Help: "Log reads of the incident context builder that gave no lines, by reason (collected, pod_not_found, " +
		"rejected, forbidden, failed, oversized, empty, withheld).",
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
	case LogReadWithheld:
		return reasonWithheld
	default:
		return reasonEmpty
	}
}
