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
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers"
)

// The time budget of the log section (#943). The properties:
//
//   - B1: the log section takes at most two call timeouts in all, whatever the order and outcome of its reads.
//   - B2: it makes at most MaxLogExcerpts x 2 calls, and a failed read is not followed by a second read of
//     that container.
//   - B3: the first reads take one call timeout each, as with one read per container; a second read the
//     budget cut short does not mark the log excerpt failed, and the excerpts read before it are kept (O1 to
//     O3 in order_test.go).

// secondPod is the web fixture's other pod.
const secondPod = "web-abc-2"

// bothExited is the web fixture with the containers of both pods exited, each after an earlier instance.
func bothExited() []client.Object {
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	ref := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	return []client.Object{
		d, rs, exited(pod(crashPod, rs.Labels, longAgo, ref)), exited(pod(secondPod, rs.Labels, longAgo, ref)),
	}
}

// outcome is how a log source answers one read of one instance.
type outcome int

const (
	answers outcome = iota
	slow            // answers after slowFor, within the call timeout
	empty
	collected
	refused   // HTTP 400
	forbidden // HTTP 403
	hangs     // never answers: the call timeout or the budget ends it
)

var outcomeNames = []string{"answers", "slow", "no_lines", "log_gone", "refused_400", "forbidden_403", "hangs"}

func (o outcome) String() string { return outcomeNames[o] }

// fails reports whether a read with this outcome is a failed read, which no second read of the container
// follows.
func (o outcome) fails() bool { return o == forbidden || o == hangs }

// timedLogs answers each read by the outcome of its pod and instance and records the calls, the time of the
// first one and the times at which a read ended because its context did.
type timedLogs struct {
	mu      sync.Mutex
	plan    map[string][2]outcome // pod -> outcome of the current, then the previous instance
	slowFor time.Duration
	calls   []providers.LogQuery
	first   time.Time
	cutAt   []time.Time
	// deadlines are the deadlines of the calls' contexts, and starts the times the calls reached the source,
	// in the order of calls.
	deadlines []time.Time
	starts    []time.Time
}

// cut records a read that its context ended.
func (l *timedLogs) cut(ctx context.Context) (providers.LogExcerpt, error) {
	l.mu.Lock()
	l.cutAt = append(l.cutAt, time.Now())
	l.mu.Unlock()
	return providers.LogExcerpt{}, ctx.Err()
}

func (l *timedLogs) Logs(ctx context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
	l.mu.Lock()
	now := time.Now()
	if l.first.IsZero() {
		l.first = now
	}
	l.starts = append(l.starts, now)
	l.calls = append(l.calls, q)
	d, _ := ctx.Deadline()
	l.deadlines = append(l.deadlines, d)
	o := l.plan[q.Pod][0]
	if q.Previous {
		o = l.plan[q.Pod][1]
	}
	l.mu.Unlock()
	answer := providers.LogExcerpt{Text: q.Pod + " " + instanceName(q.Previous) + " failed\n", CollectedAt: testNow}
	switch o {
	case slow:
		select {
		case <-time.After(l.slowFor):
			return answer, nil
		case <-ctx.Done():
			return l.cut(ctx)
		}
	case empty:
		return providers.LogExcerpt{CollectedAt: testNow}, nil
	case collected:
		return providers.LogExcerpt{}, fmt.Errorf("logs request: %w", providers.ErrLogGone)
	case refused:
		return providers.LogExcerpt{}, fmt.Errorf("logs request: %w", providers.ErrRejected)
	case forbidden:
		return providers.LogExcerpt{}, fmt.Errorf("logs request: %w", providers.ErrForbidden)
	case hangs:
		<-ctx.Done()
		return l.cut(ctx)
	}
	return answer, nil
}

// timedBuild builds the context of two exited containers with the given log source and call and build
// timeouts. It returns the context, the metric reads, how long the log section took (from its first log call
// to the first metric call, which follows the section at once) and how long the build took to reach the log
// section.
func timedBuild(t *testing.T, l *timedLogs, call, whole time.Duration) (*Context, int, time.Duration, time.Duration) {
	t.Helper()
	return timedBuildOf(t, bothExited(), l, call, whole)
}

// timedBuildOf is timedBuild over the given objects.
func timedBuildOf(t *testing.T, objs []client.Object, l *timedLogs, call, whole time.Duration) (*Context, int, time.Duration, time.Duration) {
	t.Helper()
	src := webSources()
	var mu sync.Mutex
	var metricReads int
	var metricsAt time.Time
	b := newBuilder(t, reader(t, objs...), src, func(c *Config) {
		c.SourceTimeout, c.BuildTimeout = call, whole
		c.Logs = l
		c.Metrics = metricsFunc(func(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
			mu.Lock()
			if metricReads == 0 {
				metricsAt = time.Now()
			}
			metricReads++
			mu.Unlock()
			return src.metrics.Query(ctx, q)
		})
	})
	start := time.Now()
	c := build(t, b, incident(deploymentRef()))
	end := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if metricReads > 0 {
		end = metricsAt
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.first.IsZero() {
		t.Fatal("no log call")
	}
	return c, metricReads, end.Sub(l.first), l.first.Sub(start)
}

// B1, B3: the case the second review of the fix proved. Two exited containers, each answering its current
// instance slowly and hanging on its previous one: both current instances are read first, the second read
// that follows stops at the section's budget without marking the log excerpt failed, both excerpts stay, and
// the metrics are still read within the build deadline.
func TestLogSectionKeepsItsTimeBudget(t *testing.T) {
	const call = 400 * time.Millisecond
	l := &timedLogs{
		plan:    map[string][2]outcome{crashPod: {slow, hangs}, secondPod: {slow, hangs}},
		slowFor: 350 * time.Millisecond,
	}
	c, metricReads, took, lead := timedBuild(t, l, call, time.Second)
	// The section ends 800 ms after it starts, so the metrics are read when the build reached it with time to
	// spare; on a loaded machine under -race the cluster reads before it can take longer.
	if metricReads == 0 {
		if lead < 100*time.Millisecond {
			t.Errorf("no metric reads: the log section used up the build deadline")
		} else {
			t.Logf("no metric reads, the build took %s to reach the log section", lead)
		}
	}
	if slices.Contains(c.FailedSources, SectionLogs) {
		t.Errorf("failed sources %v: only a second read was cut short", c.FailedSources)
	}
	var data []string
	for _, blk := range blocks(c, SectionLogs) {
		data = append(data, blk.Data()...)
	}
	for _, p := range []string{crashPod, secondPod} {
		if !slices.Contains(data, p+" current instance failed\n") {
			t.Errorf("the excerpt of %s read before the budget ran out is gone: %q", p, data)
		}
	}
	// The budget is 800 ms; checkDeadlines below checks it exactly, this coarse check by the clock leaves a
	// margin for scheduling under -race that stays below the 1.1 s the section took without a budget.
	if took > 2*call+250*time.Millisecond {
		t.Errorf("log section took %s, budget %s", took, 2*call)
	}
	if len(l.calls) > 2*MaxLogExcerpts {
		t.Errorf("%d log calls, want at most %d", len(l.calls), 2*MaxLogExcerpts)
	}
	checkDeadlines(t, l, call, MaxLogExcerpts)
}

// checkDeadlines checks B1 by the deadlines of the log calls: each of the first reads may run for one call
// timeout from its start, as a read of its own; every later call ends no later than two call timeouts after
// the first call (the section starts before it).
func checkDeadlines(t *testing.T, l *timedLogs, call time.Duration, firstReads int) {
	t.Helper()
	for i, d := range l.deadlines {
		limit := l.first.Add(2 * call)
		if i < firstReads {
			limit = l.starts[i].Add(call)
		}
		if d.IsZero() || d.After(limit) {
			t.Errorf("log call %d (%+v) may run until %s after the first, %s after its start",
				i, l.calls[i], d.Sub(l.first), d.Sub(l.starts[i]))
		}
	}
}
