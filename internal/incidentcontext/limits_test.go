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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// The known limits of the log section (#943, decision of 2026-11-20). The bound of the section is its worst
// case, two call timeouts; two cases take longer than a single read of the instance the kubelet would have
// served before the instance choice of #943. Design 3.3, "Which instance of a container is read", names both.
// These tests pin what happens in them, so that a change to either is seen; they are not properties to keep.

// Known limit (a): an exited container whose current instance hangs while its previous instance would answer.
// The current instance is read first, the read runs out of its call timeout, the log excerpt is a failed
// source, and no second read follows a failed one: the previous instance's excerpt is lost.
func TestKnownLimitHangingCurrentInstanceLosesThePreviousExcerpt(t *testing.T) {
	r, k := underLoad(t, func(k time.Duration) (timedRun, bool) {
		l := &timedLogs{
			plan:         map[string][2]outcome{crashPod: {hangs, answers}},
			previousText: "starting\npanic: boom\n",
		}
		c, _, _, reached := timedBuildOf(t, objectsWith(exited), l, 400*time.Millisecond*k, 10*time.Second*k)
		return timedRun{c: c, l: l}, reached
	})
	c, l, call := r.c, r.l, 400*time.Millisecond*k
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) != 1 || l.calls[0].Pod != crashPod || l.calls[0].Previous {
		t.Fatalf("calls %+v, want one read, of the current instance", l.calls)
	}
	// The read ended at its own call timeout, not at a longer deadline.
	checkOwnTimeout(t, l, 1, call)
	if len(l.cutAt) != 1 {
		t.Errorf("reads cut by their deadline %d, want the one read", len(l.cutAt))
	}
	if !slices.Contains(c.FailedSources, SectionLogs) {
		t.Errorf("failed sources %v, want the log excerpt", c.FailedSources)
	}
	for _, blk := range blocks(c, SectionLogs) {
		if strings.Contains(blk.Text, "panic: boom") {
			t.Errorf("block %q holds the previous instance's lines, which are not read", blk.Text)
		}
	}
	checkStatusAgrees(t, c)
}

// Known limit (b): a container in CrashLoopBackOff whose previous instance answers without lines at once while
// its current instance hangs. The fallback read of the current instance runs out of its call timeout, and
// when the build deadline is shorter than the log section's time plus the metrics' time, the metrics are cut
// and become a failed source. Call timeout 400 ms, build deadline 700 ms, metrics that take 420 ms; the build
// must reach the log section within 300 ms, the build deadline less the call timeout, or it runs again with
// all three durations doubled (underLoad).
func TestKnownLimitHangingFallbackReadCostsTheMetrics(t *testing.T) {
	type run struct {
		c                             *Context
		l                             *timedLogs
		metricStarts, metricDeadlines []time.Time
	}
	r, k := underLoad(t, func(k time.Duration) (*run, bool) {
		call, whole, metricsTake := 400*time.Millisecond*k, 700*time.Millisecond*k, 420*time.Millisecond*k
		r := &run{l: &timedLogs{plan: map[string][2]outcome{crashPod: {hangs, empty}}}}
		var mu sync.Mutex
		b := newBuilder(t, reader(t, objectsWith(crashingNow)...), webSources(), func(c *Config) {
			c.SourceTimeout, c.BuildTimeout = call, whole
			c.Logs = r.l
			c.Metrics = metricsFunc(func(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
				d, _ := ctx.Deadline()
				mu.Lock()
				r.metricStarts = append(r.metricStarts, time.Now())
				r.metricDeadlines = append(r.metricDeadlines, d)
				mu.Unlock()
				select {
				case <-time.After(metricsTake):
					return providers.MetricsResult{}, nil
				case <-ctx.Done():
					return providers.MetricsResult{}, ctx.Err()
				}
			})
		})
		start := time.Now()
		r.c = build(t, b, incident(deploymentRef()))
		r.l.mu.Lock()
		defer r.l.mu.Unlock()
		return r, !r.l.first.IsZero() && r.l.first.Sub(start) < whole-call
	})
	call, metricsTake := 400*time.Millisecond*k, 420*time.Millisecond*k
	c, l, metricStarts, metricDeadlines := r.c, r.l, r.metricStarts, r.metricDeadlines
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) != 2 || !l.calls[0].Previous || l.calls[1].Previous {
		t.Fatalf("calls %+v, want the previous instance, then the current one", l.calls)
	}
	// The fallback read may run for a call timeout from its start (within the section's budget).
	checkDeadlines(t, l, call, 1)
	if d := l.deadlines[1]; d.IsZero() || d.After(l.starts[1].Add(call)) {
		t.Errorf("fallback read may run %s after its start, want at most the call timeout %s", d.Sub(l.starts[1]), call)
	}
	// Decided by deadlines, not by the clock: every metric call that reached the source had less time than the
	// metrics take, so none could answer.
	for i, d := range metricDeadlines {
		if d.IsZero() || !d.Before(metricStarts[i].Add(metricsTake)) {
			t.Errorf("metric call %d may run %s, want less than the %s the metrics take", i, d.Sub(metricStarts[i]), metricsTake)
		}
	}
	if !slices.Contains(c.FailedSources, SectionMetrics) {
		t.Errorf("failed sources %v, want the metrics", c.FailedSources)
	}
	if slices.Contains(c.FailedSources, SectionLogs) {
		t.Errorf("failed sources %v: only the fallback read failed", c.FailedSources)
	}
	checkStatusAgrees(t, c)
}
