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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/testutil/metricguard"
)

// Which instance of a container the log section reads (#943). The properties:
//
//   - P1: for a container whose current instance has terminated, the context holds that instance's lines.
//   - P3: when one instance's read answers without lines, the other is tried; a read that failed is not
//     followed by another (one call per container while the source fails); each excerpt's header names its
//     instance, and when no read gave lines the block names each instance tried and why, in fixed words.
//   - P4: at most two reads per container, asking for no more bytes in all than its share of the bound.
//   - P5: each read without lines counts once under its reason.

// servingLine is the log of a running instance.
const servingLine = "serving\n"

// exited makes the pod's container exited just now, after an earlier instance that also terminated inside
// the window: the moment before the kubelet moves it to CrashLoopBackOff.
func exited(p *corev1.Pod) *corev1.Pod {
	p.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name: appName, RestartCount: 3, ContainerID: "containerd://cur",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, StartedAt: recent, FinishedAt: recent, ContainerID: "containerd://cur",
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, StartedAt: recent, FinishedAt: recent, ContainerID: "containerd://old",
		}},
	}
	return p
}

// restarted makes the pod's container run again after an instance that terminated inside the window.
func restarted(p *corev1.Pod) *corev1.Pod {
	p.Status.ContainerStatuses[0].RestartCount = 1
	p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 2, StartedAt: recent, FinishedAt: recent,
	}}
	return p
}

// objectsWith is the web fixture with the crashing pod's container status set by f.
func objectsWith(f func(*corev1.Pod) *corev1.Pod) []client.Object {
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	ref := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	return []client.Object{d, rs, f(pod(crashPod, rs.Labels, longAgo, ref)), pod("web-abc-2", rs.Labels, longAgo, ref)}
}

// instanceLogs answers each instance of the crashing container with a text or an error, and records the
// queries.
type instanceLogs struct {
	mu                sync.Mutex
	current, previous string
	currentErr        error
	previousErr       error
	asked             []providers.LogQuery
	// answered are the bytes of each answer's text, in the order of asked.
	answered []int
}

func (l *instanceLogs) Logs(_ context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, q)
	text, err := l.current, l.currentErr
	if q.Previous {
		text, err = l.previous, l.previousErr
	}
	if err != nil {
		l.answered = append(l.answered, 0)
		return providers.LogExcerpt{}, err
	}
	if int64(len(text)) > q.MaxBytes {
		text = text[int64(len(text))-q.MaxBytes:]
	}
	l.answered = append(l.answered, len(text))
	return providers.LogExcerpt{Text: text, CollectedAt: testNow}, nil
}

// buildWith builds the context of the web fixture with the container status set by f and the logs of l.
func buildWith(t *testing.T, f func(*corev1.Pod) *corev1.Pod, l *instanceLogs) *Context {
	t.Helper()
	b := newBuilder(t, reader(t, objectsWith(f)...), sources{}, func(c *Config) { c.Logs = l })
	return build(t, b, incident(deploymentRef()))
}

// checkBounded checks P4: at most two reads of the one container read; each asks for no more than what the
// answers before it left of the log bound, so the excerpts never hold more than the bound in all, and the log
// section stays within it.
func checkBounded(t *testing.T, c *Context, l *instanceLogs) {
	t.Helper()
	const limit = DefaultLogBytes
	if len(l.asked) > 2 {
		t.Errorf("%d reads of one container, want at most 2: %+v", len(l.asked), l.asked)
	}
	left := limit
	for i, q := range l.asked {
		if q.Pod != crashPod || q.Container != appName {
			t.Errorf("read of another container: %+v", q)
		}
		if q.MaxBytes <= 0 || q.MaxBytes > int64(left) {
			t.Errorf("read %d asked for %d bytes, %d left of the bound", i, q.MaxBytes, left)
		}
		left -= l.answered[i]
	}
	if n := sectionSize(c, SectionLogs); n > limit {
		t.Errorf("log section %d bytes, over its bound %d", n, limit)
	}
}

// reasons returns the counts of the reasons of reads without lines.
func reasons() map[int]float64 {
	m := map[int]float64{}
	for i, r := range logReadReasons {
		m[i] = testutil.ToFloat64(logReadsWithoutLines.WithLabelValues(r))
	}
	return m
}

// checkCounted checks P5: the counter rose by want for each reason, and by nothing for any other.
func checkCounted(t *testing.T, before map[int]float64, want map[int]float64) {
	t.Helper()
	for r, n := range reasons() {
		if d := n - before[r]; d != want[r] {
			t.Errorf("reads without lines, reason %s: rose by %v, want %v", logReadReasons[r], d, want[r])
		}
	}
}

// P1: a container that exits at once is read in its current, terminated instance, the newest dead one, whose
// lines name the cause; the previous instance, which the kubelet no longer holds, is read as well within the
// budget and named with its reason.
func TestTerminatedCurrentInstanceIsRead(t *testing.T) {
	l := &instanceLogs{
		current:     "loading /etc/app/config.yaml\nError: required key \"db.url\" is missing\n",
		previousErr: fmt.Errorf("logs request: %w", providers.ErrLogGone),
	}
	before := reasons()
	c := buildWith(t, exited, l)
	logs := one(t, c, SectionLogs)
	if logs.Availability != Available || c.Escalate() {
		t.Fatalf("availability %q, escalate %t", logs.Availability, c.Escalate())
	}
	if len(l.asked) == 0 || l.asked[0].Previous {
		t.Fatalf("first read %+v, want the current instance", l.asked)
	}
	data := logs.Data()
	if len(data) != 1 || !strings.Contains(data[0], `Error: required key "db.url" is missing`) {
		t.Errorf("excerpt %q does not hold the current instance's lines:\n%s", data, logs.Text)
	}
	if !strings.Contains(logs.Text, "pod web-abc-1 container app, current instance\n") ||
		!strings.Contains(logs.Text, "pod web-abc-1 container app, previous instance\n"+logGone+"\n") {
		t.Errorf("header does not name both instances:\n%s", logs.Text)
	}
	checkBounded(t, c, l)
	checkCounted(t, before, map[int]float64{reasonLogGone: 1})
}

// When the previous instance of a terminated current one still has its log, both excerpts are in the context,
// each in its own block naming its instance, the newest dead instance first.
func TestTerminatedCurrentAndPreviousBothRead(t *testing.T) {
	l := &instanceLogs{current: "second failure\n", previous: "first failure\n"}
	c := buildWith(t, exited, l)
	bs := blocks(c, SectionLogs)
	if len(bs) != 2 || len(l.asked) != 2 {
		t.Fatalf("%d blocks, %d reads", len(bs), len(l.asked))
	}
	if !strings.HasPrefix(bs[0].Text, "pod web-abc-1 container app, current instance\n") ||
		!strings.HasSuffix(bs[0].Text, "second failure\n") ||
		!strings.HasPrefix(bs[1].Text, "pod web-abc-1 container app, previous instance\n") ||
		!strings.HasSuffix(bs[1].Text, "first failure\n") {
		t.Errorf("blocks:\n%s\n---\n%s", bs[0].Text, bs[1].Text)
	}
	checkBounded(t, c, l)
}

// A terminated current instance whose excerpt fills the budget leaves no room for a second read.
func TestNoSecondReadWithoutBudget(t *testing.T) {
	l := &instanceLogs{current: strings.Repeat("a line of the log that keeps going\n", 4000), previous: "older\n"}
	c := buildWith(t, exited, l)
	if len(l.asked) != 1 || len(blocks(c, SectionLogs)) != 1 {
		t.Fatalf("reads %+v", l.asked)
	}
}

// A container that runs again after a termination inside the window is read in its previous instance, and
// once only when that read gives lines.
func TestRunningCurrentWithDeadPreviousReadsThePrevious(t *testing.T) {
	l := &instanceLogs{current: servingLine, previous: "panic: boom\n"}
	c := buildWith(t, restarted, l)
	logs := one(t, c, SectionLogs)
	if len(l.asked) != 1 || !l.asked[0].Previous || l.asked[0].Since != 0 {
		t.Fatalf("reads %+v, want one of the previous instance", l.asked)
	}
	if !strings.HasPrefix(logs.Text, "pod web-abc-1 container app, previous instance\n") ||
		!strings.HasSuffix(logs.Text, "panic: boom\n") {
		t.Errorf("block:\n%s", logs.Text)
	}
}

// P3: a previous instance the node no longer holds, or that gave no lines, falls back to the current one.
func TestPreviousWithoutLinesFallsBackToTheCurrent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		l      *instanceLogs
		note   string
		reason int
	}{
		{"collected", &instanceLogs{current: servingLine, previousErr: providers.ErrLogGone}, logGone, reasonLogGone},
		{"no lines", &instanceLogs{current: servingLine}, logEmpty, reasonEmpty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reasons()
			c := buildWith(t, crashingNow, tc.l)
			logs := one(t, c, SectionLogs)
			if len(tc.l.asked) != 2 || !tc.l.asked[0].Previous || tc.l.asked[1].Previous {
				t.Fatalf("reads %+v, want the previous instance then the current one", tc.l.asked)
			}
			if !strings.Contains(logs.Text, "previous instance\n") || !strings.Contains(logs.Text, tc.note+"\n") ||
				!strings.Contains(logs.Text, "current instance\n") || !slices.Equal(logs.Data(), []string{servingLine}) {
				t.Errorf("block:\n%s\ndata %q", logs.Text, logs.Data())
			}
			checkBounded(t, c, tc.l)
			checkCounted(t, before, map[int]float64{tc.reason: 1})
		})
	}
}

// crashingNow is the web fixture's CrashLoopBackOff container.
func crashingNow(p *corev1.Pod) *corev1.Pod { return crashing(p, "exited") }

// P3: when neither instance gives lines, one block names each instance read and its reason, in fixed words; a
// read the identity is refused is a failed source and is not followed by a read of the other instance, an
// instance the node no longer holds is not a failure and is.
func TestNoInstanceGivesLines(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		note    string
		reason  int
		failure bool
		reads   int
	}{
		{"collected", fmt.Errorf("logs request: %w", providers.ErrLogGone), logGone, reasonLogGone, false, 2},
		{"forbidden", fmt.Errorf("logs request: %w", providers.ErrForbidden), logForbidden, reasonForbidden, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &instanceLogs{currentErr: tc.err, previousErr: tc.err}
			before := reasons()
			c := buildWith(t, exited, l)
			logs := one(t, c, SectionLogs)
			want := "pod web-abc-1 container app, current instance\n" + tc.note + "\n"
			if tc.reads == 2 {
				want += "pod web-abc-1 container app, previous instance\n" + tc.note + "\n"
			}
			if len(l.asked) != tc.reads || logs.Text != want || logs.Data() != nil {
				t.Errorf("block %q, want %q", logs.Text, want)
			}
			if got := slices.Contains(c.FailedSources, SectionLogs); got != tc.failure ||
				(logs.Availability == CouldNotFetch) != tc.failure {
				t.Errorf("failed %v, availability %q, want failure %v", c.FailedSources, logs.Availability, tc.failure)
			}
			checkBounded(t, c, l)
			checkCounted(t, before, map[int]float64{tc.reason: float64(tc.reads)})
		})
	}
}

// A container with no previous instance is read once, also when its current instance gives no lines.
func TestNoPreviousInstanceNoSecondRead(t *testing.T) {
	l := &instanceLogs{}
	c := buildWith(t, func(p *corev1.Pod) *corev1.Pod { return p }, l)
	if len(l.asked) != 1 || l.asked[0].Previous {
		t.Fatalf("reads %+v", l.asked)
	}
	if logs := one(t, c, SectionLogs); !strings.HasSuffix(logs.Text, logEmpty+"\n") {
		t.Errorf("block %q", logs.Text)
	}
}

// The counter of reads without lines has one label with a closed set of values, and is documented.
func TestLogReadsMetricIsBounded(t *testing.T) {
	want := map[string]metricguard.Labels{
		"shadewright_analyzer_context_log_reads_without_lines_total": {logReasonLabelName: logReadReasons},
	}
	metricguard.Check(t, metrics.Registry, want)
	metricguard.Documented(t, filepath.Join("..", "..", "docs", "reference", "metrics.md"), want)
}

// A log source that fails costs one read per container: a failed read is not followed by a read of the
// other instance, so a hanging source cannot use up the build deadline and take the metrics with it.
func TestFailedLogReadIsNotReadAgain(t *testing.T) {
	src := webSources()
	var mu sync.Mutex
	var logReads, metricReads int
	b := newBuilder(t, reader(t, webObjects()...), src, func(c *Config) {
		c.SourceTimeout = 400 * time.Millisecond
		c.BuildTimeout = 700 * time.Millisecond
		c.Logs = logFunc(func(ctx context.Context, _ providers.LogQuery) (providers.LogExcerpt, error) {
			mu.Lock()
			logReads++
			mu.Unlock()
			<-ctx.Done()
			return providers.LogExcerpt{}, ctx.Err()
		})
		c.Metrics = metricsFunc(func(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
			mu.Lock()
			metricReads++
			mu.Unlock()
			return src.metrics.Query(ctx, q)
		})
	})
	c := build(t, b, incident(deploymentRef()))
	mu.Lock()
	defer mu.Unlock()
	if logReads != 1 || metricReads == 0 {
		t.Errorf("%d log reads, %d metric reads: want 1 log read and the metrics read", logReads, metricReads)
	}
	if got := sectionNames(c.FailedSources); strings.Join(got, ",") != string(SectionLogs) {
		t.Errorf("failed sources %v, want the log excerpt only", got)
	}
	if logs := one(t, c, SectionLogs); logs.Availability != CouldNotFetch || strings.Count(logs.Text, "instance\n") != 1 {
		t.Errorf("block %q, availability %q: want one failed read", logs.Text, logs.Availability)
	}
}
