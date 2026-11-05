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

// logPicks chooses the containers to read, most relevant first: one whose last instance terminated (its
// previous instance's tail holds the lines before the termination), then one that waits with a reason, then
// one that is not ready. When no container shows trouble, the first container of the first pod is read.
func logPicks(t *target) []logPick {
	var all []logPick
	for i := range t.pods {
		p := &t.pods[i]
		for _, cs := range allStatuses(p) {
			pick := logPick{namespace: p.Namespace, pod: p.Name, container: cs.Name, init: cs.init, restarts: cs.RestartCount}
			switch {
			case cs.LastTerminationState.Terminated != nil:
				pick.score, pick.previous = 3, true
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
				pick.score = 2
			case !cs.Ready:
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
	return all[:max(n, min(1, len(all)))]
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
	picks := logPicks(t)
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
// then the redacted excerpt cut to its newest lines. It reports whether the source failed.
func (b *Builder) logBlock(pick logPick, ex providers.LogExcerpt, err error, budget int) (Block, bool) {
	blk := Block{Section: SectionLogs, Provenance: ProvenanceLogExcerpt, Availability: Available}
	var h lines
	kind, instance := "container", "current instance"
	if pick.init {
		kind = "init container"
	}
	if pick.previous {
		instance = "previous instance"
	}
	h.add("pod %s %s %s, %s", b.value(pick.pod), kind, b.value(pick.container), instance)
	var body string
	failed := false
	switch {
	case errors.Is(err, providers.ErrNotFound):
		h.add("no logs: the pod no longer exists")
	case errors.Is(err, providers.ErrRejected):
		h.add("no logs: the source refused the request (the container has not started, or has no previous instance)")
	case err != nil:
		blk.Availability, failed = CouldNotFetch, true
		h.add("could not fetch")
	case len(ex.Text) > 2*budget:
		// More than was asked for, beyond what redaction markers could add: the source broke its bound, and
		// the text is not read further.
		blk.Availability, failed = CouldNotFetch, true
		h.add("could not fetch: the answer was larger than asked for")
	default:
		in := ex.Instance
		h.add("instance: pod uid %s, container id %s, restarts %d, read at %s", b.value(in.PodUID),
			b.value(in.ContainerID), in.RestartCount, ex.CollectedAt.UTC().Format(time.RFC3339))
		body = ex.Text
		if ex.Truncated {
			blk.addLabel(LabelLogTruncated)
		}
		if ex.EndMissing {
			blk.addLabel(LabelLogEndMissing)
		}
		if ex.InstanceUncertain {
			blk.addLabel(LabelLogInstanceUncertain)
		}
	}
	head := b.redact(h.String())
	if len(head) > budget {
		head, _ = keepHead(head, budget)
	}
	tail := b.redact(body)
	tail, cut := keepTail(tail, budget-len(head))
	if cut {
		blk.addLabel(LabelSectionCut)
	}
	blk.Text, blk.headLen = head+tail, len(head)
	return blk, failed
}
