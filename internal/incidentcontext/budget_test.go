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

	"github.com/shadewright-labs/shadewright/internal/providers"
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
	// previousText, when set, is the answer of a previous instance instead of the fixed excerpt.
	previousText string
	calls        []providers.LogQuery
	first        time.Time
	cutAt        []time.Time
	// deadlines are the deadlines of the calls' contexts, and starts the times the calls reached the source,
	// in the order of calls.
	deadlines []time.Time
	starts    []time.Time
	// ends are the times the calls returned, in the order they returned (the reads are sequential).
	ends []time.Time
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
	defer func() {
		l.mu.Lock()
		l.ends = append(l.ends, time.Now())
		l.mu.Unlock()
	}()
	answer := providers.LogExcerpt{Text: q.Pod + " " + instanceName(q.Previous) + " failed\n", CollectedAt: testNow}
	if q.Previous && l.previousText != "" {
		answer.Text = l.previousText
	}
	switch o {
	case slow:
		// Decided by the call's deadline, not by racing two timers: a goroutine that a loaded machine wakes
		// after both have fired would pick either. The answer comes when the deadline leaves slowFor from the
		// start of the call, however late the wait ends.
		if d, ok := ctx.Deadline(); ok && d.Before(now.Add(l.slowFor)) {
			<-ctx.Done()
			return l.cut(ctx)
		}
		time.Sleep(l.slowFor)
		return answer, nil
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
// timeouts. It returns the context, the metric reads, and whether the build reached the log section (made a log
// call; see [underLoad]).
func timedBuild(t *testing.T, l *timedLogs, call, whole time.Duration) (*Context, int, time.Duration, bool) {
	t.Helper()
	return timedBuildOf(t, bothExited(), l, call, whole)
}

// timedBuildOf is timedBuild over the given objects. Besides the context and the metric reads it returns how
// long the build took to reach the log section (from its start to the first log call), and false when it made
// no log call: the reads of the target ran out of their call timeout on a loaded machine, or the build
// deadline passed before the section ([underLoad]).
func timedBuildOf(t *testing.T, objs []client.Object, l *timedLogs, call, whole time.Duration) (*Context, int, time.Duration, bool) {
	t.Helper()
	src := webSources()
	var mu sync.Mutex
	var metricReads int
	b := newBuilder(t, reader(t, objs...), src, func(c *Config) {
		c.SourceTimeout, c.BuildTimeout = call, whole
		c.Logs = l
		c.Metrics = metricsFunc(func(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
			mu.Lock()
			metricReads++
			mu.Unlock()
			return src.metrics.Query(ctx, q)
		})
	})
	start := time.Now()
	c := build(t, b, incident(deploymentRef()))
	mu.Lock()
	defer mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.first.IsZero() {
		return c, metricReads, 0, false
	}
	return c, metricReads, l.first.Sub(start), true
}

// timedRun is what one attempt of a timing test observed: the context, its log source and the metric reads.
type timedRun struct {
	c           *Context
	l           *timedLogs
	metricReads int
}

// maxLoadScale is the largest factor [underLoad] multiplies the durations of a test by.
const maxLoadScale = 8

// underLoad runs attempt with the durations of a timing test multiplied by k = 1, then 2, 4 and 8, until
// attempt reports that the build reached the source calls the test is about before its deadlines, and returns
// that attempt's result and k. The builder's deadlines are wall-clock times set when a build or a call starts,
// with no clock to inject, and a build does CPU work before the log section (the summary's redaction, the reads
// of the target), which a loaded machine stretches: under -race on a runner whose four cores the other packages'
// tests share, that work alone took longer than a deadline of 500 ms (#873). The attempt reports only whether
// the build got where the test starts; the test checks what it asserts on the attempt returned, with the
// durations scaled by k, so no assertion is relaxed. A build that never gets there at maxLoadScale fails the
// test.
func underLoad[T any](t testing.TB, attempt func(k time.Duration) (T, bool)) (T, time.Duration) {
	t.Helper()
	var r T
	for k := time.Duration(1); k <= maxLoadScale; k *= 2 {
		var reached bool
		if r, reached = attempt(k); reached {
			return r, k
		}
		t.Logf("the build did not reach the log section in time with the durations times %d: trying again with "+
			"twice the durations", k)
	}
	t.Fatalf("the build did not reach the log section in time with the durations times %d", maxLoadScale)
	return r, 0
}

// B1, B3: the case the second review of the fix proved. Two exited containers, each answering its current
// instance slowly and hanging on its previous one: both current instances are read first, the second read
// that follows stops at the section's budget without marking the log excerpt failed, both excerpts stay, and
// the metrics are still read within the build deadline.
func TestLogSectionKeepsItsTimeBudget(t *testing.T) {
	// A slow answer comes 150 ms before the call timeout (decided by the call's deadline), and the build
	// deadline is 1.2 s after the section's end. The section's time is checked by the calls' deadlines, which
	// load does not move, not by the clock. Reading the first container's previous instance before the second
	// container's current one would cut that read at 800 ms and lose its excerpt. The build must reach the
	// section within a quarter of its deadline, or it runs again with longer durations (underLoad).
	r, k := underLoad(t, func(k time.Duration) (timedRun, bool) {
		l := &timedLogs{
			plan:    map[string][2]outcome{crashPod: {slow, hangs}, secondPod: {slow, hangs}},
			slowFor: 250 * time.Millisecond * k,
		}
		c, metricReads, reach, reached := timedBuild(t, l, 400*time.Millisecond*k, 2*time.Second*k)
		return timedRun{c, l, metricReads}, reached && reach < 500*time.Millisecond*k
	})
	c, l, metricReads, call := r.c, r.l, r.metricReads, 400*time.Millisecond*k
	if metricReads == 0 {
		t.Errorf("no metric reads: the log section used up the build deadline")
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
	if len(l.calls) > 2*MaxLogExcerpts {
		t.Errorf("%d log calls, want at most %d", len(l.calls), 2*MaxLogExcerpts)
	}
	checkDeadlines(t, l, call, MaxLogExcerpts)
	checkOwnTimeout(t, l, MaxLogExcerpts, call)
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
