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

	"github.com/shadewright-labs/shadewright/internal/providers"
)

// MaxLogExcerpts bounds the containers whose logs the log section reads; the section's bound is split evenly
// between them.
const MaxLogExcerpts = 2

// logBudgetCalls is the time budget of the log section in call timeouts: the most it could take with one read
// per container. The first reads take at most one call timeout each; the second reads share what is left.
const logBudgetCalls = MaxLogExcerpts

// previousTailLines is the number of final lines of a terminated previous instance kept ahead of other log
// lines when the total context bound trims the log section: enough for an unmarked exit message and a short
// usage or panic tail without crowding out the rest of the context.
const previousTailLines = 10

// minSecondRead is the share of a container's log budget reserved for its previous instance when its current
// instance has also terminated (capped at half the share for small bounds). Two KiB leaves room for the
// instance header and the trailing lines of a crash log (or the rune-boundary tail of an over-budget last
// line) so a noisy current instance cannot crowd out the previous instance's ending.
const minSecondRead = 2 << 10

// crashLoopBackOffReason is the kubelet waiting reason for a container in a crash loop.
const crashLoopBackOffReason = "CrashLoopBackOff"

// previousReserve returns the bytes reserved from a container's log share for reading its previous instance
// when the current instance has also terminated.
func previousReserve(share int) int {
	return min(minSecondRead, share/2)
}

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
// container) and one that exited after its pod's deletion began ([shutdown]) are not trouble. When no container shows
// trouble, the first regular container of the most troubled pod is read.
//
// Which instance is read first: for a container whose current instance has terminated, that instance, the
// newest dead one (the kubelet keeps one terminated instance per container, so the one before it is usually
// gone); for any other container whose last instance terminated inside the window, the previous instance,
// whose tail holds the lines before the termination (while a container waits, the kubelet serves that same
// instance for both), and so for a container whose current instance exited after its pod's deletion began: the
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
			case last != nil && (last.FinishedAt.After(w.start) ||
				(cs.State.Waiting != nil && cs.State.Waiting.Reason == crashLoopBackOffReason)):
				pick.score, pick.previous = 3, !pick.currentDead || shutdown(p, cs)
			case cs.State.Waiting != nil && cs.State.Waiting.Reason == crashLoopBackOffReason && cs.RestartCount > 0:
				pick.score, pick.previous = 3, true
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "",
				cs.State.Terminated != nil && !settled(p, cs):
				pick.score = 2
			case !cs.Ready && !settled(p, cs):
				pick.score = 1
			}
			// The current instance always exists; a previous one only when chosen first (such as CrashLoopBackOff
			// with restarts) or after a recorded termination.
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
		reads[i] = containerReads{pick: pick, budget: budget, left: budget, askLeft: budget}
		if err := b.readInstance(ctx, &reads[i], pick.previous, since); err != nil {
			return nil, false, err
		}
	}
	for i := range reads {
		if !reads[i].wantsSecond() {
			continue
		}
		if outOfTime(second) {
			b.recordRead(ctx, &reads[i], !reads[i].pick.previous, providers.LogExcerpt{}, errBuildDeadline, reads[i].left)
			continue
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
			blks[j].container = i + 1
		}
		blocks = append(blocks, blks...)
	}
	return blocks, failed, nil
}

// containerReads is what the reads of one container gave: at most two reads, within its share of the log
// bound in all.
type containerReads struct {
	pick logPick
	// budget is this container's full share of the log bound; reads counts the reads made; left is what is left
	// of the rendered block share; askLeft is what is left of the share after the raw bytes answered so far, so
	// the queries together never ask for more than the share even when the cut drops a partial leading line.
	budget, reads, left, askLeft int
	// out are the excerpts, in the order read; notes the header lines of the reads that gave no lines.
	out   []Block
	notes strings.Builder
	// failed: the first read failed (the source failed, timed out, refused the identity or answered
	// oversized), or a read was withheld by redaction. A second read that failed otherwise is only named in
	// notes.
	failed bool
	// withheld is the title and notice of a read redaction withheld whole (#873).
	withheld string
}

// share returns the container's share of the log bound, falling back to what is already accounted for in tests
// that construct containerReads directly.
func (r *containerReads) share() int {
	if r.budget > 0 {
		return r.budget
	}
	return r.left + r.notes.Len()
}

// queryBudget returns the maximum bytes the next query may ask of the log source without exceeding either the
// rendered share or the raw bytes remaining in the container's share.
func (r *containerReads) queryBudget() int {
	if r.budget == 0 && r.askLeft == 0 {
		return r.left
	}
	return min(r.left, r.askLeft)
}

// wantsSecond reports whether the container's other instance is read after its first read: the other
// instance exists, the first read did not fail (a failing source costs one call per container), bytes are
// left, and the first read gave no lines (collected, refused for that instance, pod not found or empty) or
// it gave the excerpt of a terminated current instance and at least previousReserve bytes are left.
func (r *containerReads) wantsSecond() bool {
	qb := r.queryBudget()
	return r.reads == 1 && r.pick.other && !r.failed && qb > 0 &&
		(len(r.out) == 0 || r.pick.currentDead && qb >= previousReserve(r.share()))
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
		if r.withheld != "" {
			blk.addLabel(LabelWithheld)
			blk.requiredText = r.withheld
		}
		blk.headLen = len(blk.Text)
		return []Block{blk}
	}
	out := slices.Clone(r.out)
	out[0].Text, out[0].headLen = r.notes.String()+out[0].Text, r.notes.Len()+out[0].headLen
	if r.withheld != "" {
		out[0].addLabel(LabelWithheld)
		out[0].requiredText = r.withheld
	}
	return out
}

// readInstance reads one instance of r's container, the previous one when previous is set, asking for no
// more than what is left of its share (leaving previousReserve when a terminated current instance also has a
// previous instance to read), and records the answer in r. It returns an error only when ctx is done.
func (b *Builder) readInstance(ctx context.Context, r *containerReads, previous bool, since time.Duration) error {
	pick := r.pick
	ask := r.queryBudget()
	if r.reads == 0 && pick.currentDead && pick.other && !previous {
		ask -= previousReserve(r.share())
	}
	q := providers.LogQuery{
		Namespace: pick.namespace, Pod: pick.pod, Container: pick.container, Previous: previous,
		MaxBytes: int64(ask),
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
	b.recordRead(ctx, r, previous, ex, err, ask)
	return nil
}

// recordRead renders one log answer (or a skipped second read's deadline error) into r within its share.
func (b *Builder) recordRead(ctx context.Context, r *containerReads, previous bool, ex providers.LogExcerpt, err error, ask int) {
	pick := r.pick
	r.reads++
	read := pick
	read.previous = previous
	blk, v := b.renderBlock(read, ex, err, ask)
	if v.Read == LogReadWithheld && len(blk.Text) > r.left {
		fitWithheld(r, &blk)
	}
	if r.reads > 1 && v.Read == LogReadFailed {
		// A failed second read keeps its "could not fetch" notice: the room comes from the earlier notes and the
		// first excerpt, never by cutting the notice to its title.
		if full, fullV := b.renderBlock(read, providers.LogExcerpt{}, err, r.share()); len(full.Text) > len(blk.Text) &&
			makeRoom(r, len(full.Text)) {
			blk, v = full, fullV
		}
	}
	r.left -= len(blk.Text)
	if r.budget > 0 {
		spent := len(blk.Text)
		if err == nil && len(ex.Text) > spent {
			spent = len(ex.Text)
		}
		r.askLeft = max(0, r.askLeft-spent)
	}
	if v.Read == LogReadOK {
		blk.previous = previous
		r.out = append(r.out, blk)
		return
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
		r.failed, r.withheld = true, blk.Text
	}
}

// reclaimFromNotes trims earlier no-line notes from r so that need bytes fit in r.left.
func reclaimFromNotes(r *containerReads, need int) {
	if r.notes.Len() == 0 || need <= r.left {
		return
	}
	notes, _ := keepHead(r.notes.String(), r.notes.Len()+r.left-need)
	r.left += r.notes.Len() - len(notes)
	r.notes.Reset()
	r.notes.WriteString(notes)
}

// fitWithheld makes room in r for blk, the title and notice of a read redaction withheld whole, so the container
// stays within its share and the section within its bound (decision of 2026-11-28 on #1055), in this order: the
// earlier notes and the body of a current instance's first excerpt ([makeRoom]); that first excerpt whole, when
// only its header is left; the notice alone, without its title. A previous instance's excerpt is never cut for it
// (decision of 2026-11-26: the previous instance's tail is kept first). The notice alone fits then: a share is at
// least half of MinSectionBytes, and a second read after a first excerpt starts with at least previousReserve of
// it left. Should it not (a case the reads do not reach), the previous instance's body gives the room, since the
// section never exceeds its bound.
func fitWithheld(r *containerReads, blk *Block) {
	if makeRoom(r, len(blk.Text)) {
		return
	}
	if len(r.out) > 0 && !r.out[0].previous {
		r.left += len(r.out[0].Text)
		r.out = r.out[1:]
		if makeRoom(r, len(blk.Text)) {
			return
		}
	}
	blk.Text = withheldText
	if !makeRoom(r, len(blk.Text)) && len(r.out) > 0 {
		first := &r.out[0]
		body := first.Text[first.headLen:]
		kept, c := cutLogKind(body, max(len(body)+r.left-len(blk.Text), 0))
		r.left += len(body) - len(kept)
		first.Text = first.Text[:first.headLen] + kept
		first.addLabel(LabelSectionCut)
		addCutLabels(first, c.Kind)
	}
}

// makeRoom frees room in r for need bytes, first from the earlier no-line notes, then from the body of the
// first excerpt when it is a current instance's, and reports whether need now fits in what is left of r's share.
func makeRoom(r *containerReads, need int) bool {
	reclaimFromNotes(r, need)
	reclaimFromFirstExcerpt(r, need)
	return need <= r.left
}

// reclaimFromFirstExcerpt shortens the already-redacted body of r.out[0] with the one cut ([cutLog]) so that need
// bytes fit in r.left, down to no body at all; the excerpt's header is kept whole. A previous instance's excerpt
// is not cut: it ranks above the later read that needs the room (decision of 2026-11-26).
func reclaimFromFirstExcerpt(r *containerReads, need int) {
	if len(r.out) == 0 || need <= r.left || r.out[0].previous {
		return
	}
	first := &r.out[0]
	head, body := first.Text[:first.headLen], first.Text[first.headLen:]
	if body == "" {
		return
	}
	kept, c := cutLogKind(body, max(len(body)+r.left-need, 0))
	r.left += len(body) - len(kept)
	first.Text = head + kept
	first.addLabel(LabelSectionCut)
	addCutLabels(first, c.Kind)
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
		{v.HeaderKept, LabelLogHeaderKept}, {v.HeaderLeftOut, LabelLogHeaderLeftOut},
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
