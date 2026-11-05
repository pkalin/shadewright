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
	"slices"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// MaxLogExcerpts bounds the excerpts in the log section; the section's bound is split evenly between them.
const MaxLogExcerpts = 2

// logPick is one container whose logs the builder reads.
type logPick struct {
	namespace, pod, container string
	init, previous            bool
	score                     int
	restarts                  int32
}

// logPicks chooses the containers to read, most relevant first: one whose last instance terminated inside
// the window (its previous instance's tail holds the lines before the termination), then one that waits
// with a reason or terminated with an error, then one that is not ready. A termination that finished before
// the window, and a container that ran to completion (a finished init container), are not trouble. When no
// container shows trouble, the first regular container of the most troubled pod is read.
func logPicks(t *target, w window) []logPick {
	var all []logPick
	for i := range t.pods {
		p := &t.pods[i]
		for _, cs := range allStatuses(p) {
			pick := logPick{namespace: p.Namespace, pod: p.Name, container: cs.Name, init: cs.init, restarts: cs.RestartCount}
			last := cs.LastTerminationState.Terminated
			switch {
			case last != nil && last.FinishedAt.After(w.start):
				pick.score, pick.previous = 3, true
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "",
				cs.State.Terminated != nil && !completed(cs):
				pick.score = 2
			case !cs.Ready && !completed(cs):
				pick.score = 1
			}
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
		q := providers.LogQuery{
			Namespace: pick.namespace, Pod: pick.pod, Container: pick.container, Previous: pick.previous,
			MaxBytes: int64(budget),
		}
		if !pick.previous {
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
		blk, bad := b.logBlock(pick, ex, err, budget)
		failed = failed || bad
		b.podLabels(&blk, t)
		blocks = append(blocks, blk)
	}
	return blocks, failed, nil
}

// logBlock turns one answer of the log source into a block: a header naming the container and instance,
// then the redacted excerpt cut to its newest lines ([RenderLog]). It reports whether the source failed.
func (b *Builder) logBlock(pick logPick, ex providers.LogExcerpt, err error, budget int) (Block, bool) {
	blk := Block{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available}
	kind, instance := "container", "current instance"
	if pick.init {
		kind = "init container"
	}
	if pick.previous {
		instance = "previous instance"
	}
	title := "pod " + b.value(pick.pod) + " " + kind + " " + b.value(pick.container) + ", " + instance
	v := RenderLog(b.redactor.String, title, ex, err, budget, budget)
	failed := v.Read == LogReadFailed || v.Read == LogReadOversized
	if failed {
		blk.Availability = CouldNotFetch
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
	return blk, failed
}
