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

package analyzer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/metrics/prometheus"
)

const (
	testPromURL = "http://prometheus.example"
	testCluster = "lab"
	watchedNS   = "shop"
	testPod     = "api-0"
)

// countingServer counts every request that reaches it and answers 500.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func podsClient(t *testing.T, url string) kubernetes.Interface {
	t.Helper()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: url})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func namespaces(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("ns-%d", i)
	}
	return out
}

// Acceptance (#371): more watched namespaces than the logs source takes, with logs enabled, refuse the start
// with an error naming the limit; with logs off the same list is accepted.
func TestTooManyNamespacesWithPodLogsRefuseToStart(t *testing.T) {
	srv, _ := countingServer(t)
	cfg := SourcesConfig{Watched: namespaces(257), PodLogs: true, Pods: podsClient(t, srv.URL).CoreV1()}
	_, err := NewSources(cfg)
	if !errors.Is(err, providers.ErrInvalidConfig) || !strings.Contains(err.Error(), "more than 256 namespaces") ||
		!strings.Contains(err.Error(), "--pod-logs") {
		t.Fatalf("got %v, want a configuration error naming --pod-logs and the limit 256", err)
	}
	cfg.Watched = namespaces(256)
	if _, err := NewSources(cfg); err != nil {
		t.Fatalf("256 namespaces: %v", err)
	}
	cfg.Watched, cfg.PodLogs = namespaces(257), false
	if s, err := NewSources(cfg); err != nil || s.Logs != nil {
		t.Fatalf("logs off: %v, logs %v", err, s.Logs)
	}
}

// The metrics source has the same limit, and says which flag needs it.
func TestTooManyNamespacesWithPrometheusRefuseToStart(t *testing.T) {
	_, err := NewSources(SourcesConfig{Watched: namespaces(257), PrometheusURL: testPromURL})
	if !errors.Is(err, providers.ErrInvalidConfig) || !strings.Contains(err.Error(), "more than 256 namespaces") ||
		!strings.Contains(err.Error(), "--prometheus-url") {
		t.Fatalf("got %v", err)
	}
}

func TestSourcesRefuseBadConfig(t *testing.T) {
	for name, cfg := range map[string]SourcesConfig{
		"cluster without URL":               {Watched: []string{watchedNS}, PrometheusCluster: testCluster},
		"cluster without URL, no namespace": {PrometheusCluster: testCluster},
		"bad URL":                           {Watched: []string{watchedNS}, PrometheusURL: "ftp://prometheus.example"},
		"bad cluster":                       {Watched: []string{watchedNS}, PrometheusURL: testPromURL, PrometheusCluster: "a b"},
		"logs without client":               {Watched: []string{watchedNS}, PodLogs: true},
	} {
		if _, err := NewSources(cfg); !errors.Is(err, providers.ErrInvalidConfig) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// Nothing configured gives nil sources (not typed nil pointers), which the builder reports as not available.
func TestNoSourcesConfigured(t *testing.T) {
	s, err := NewSources(SourcesConfig{Watched: []string{watchedNS}})
	if err != nil || s.Metrics != nil || s.Logs != nil || s.ClusterCheck != nil {
		t.Fatalf("got %+v, %v", s, err)
	}
}

// Acceptance (#371): the sources get the watched namespaces, so a query about any other namespace is refused
// before a request reaches the API server or Prometheus.
func TestSourcesRefuseUnwatchedNamespacesBeforeAnyRequest(t *testing.T) {
	kubeSrv, kubeCalls := countingServer(t)
	promSrv, promCalls := countingServer(t)
	s, err := NewSources(SourcesConfig{
		Watched: []string{watchedNS}, PodLogs: true, Pods: podsClient(t, kubeSrv.URL).CoreV1(), PrometheusURL: promSrv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Logs.Logs(ctx, providers.LogQuery{Namespace: "payments", Pod: testPod, Container: "api"}); err == nil {
		t.Error("logs of an unwatched namespace returned")
	}
	_, err = s.Metrics.Query(ctx, providers.MetricsQuery{
		Template: "container_restarts_total", Params: map[string]string{prometheus.ParamNamespace: "payments"},
		Pods: []string{testPod}, End: time.Now(),
	})
	if !errors.Is(err, prometheus.ErrInvalidQuery) {
		t.Errorf("metrics of an unwatched namespace: %v", err)
	}
	if kubeCalls.Load() != 0 || promCalls.Load() != 0 {
		t.Errorf("%d API server and %d Prometheus requests, want none", kubeCalls.Load(), promCalls.Load())
	}
}

// With no watched namespace no source is built and no cluster check runs: nothing could be asked, and a check
// would send a request for nothing. The flag dependency is still checked (TestSourcesRefuseBadConfig).
func TestNoWatchedNamespaceBuildsNoSources(t *testing.T) {
	srv, _ := countingServer(t)
	s, err := NewSources(SourcesConfig{
		PodLogs: true, Pods: podsClient(t, srv.URL).CoreV1(), PrometheusURL: testPromURL, PrometheusCluster: testCluster,
	})
	if err != nil || s.Metrics != nil || s.Logs != nil || s.ClusterCheck != nil {
		t.Fatalf("got %+v, %v; want no sources and no cluster check", s, err)
	}
}

// A watched namespace does reach the sources, so the test above counts the right servers.
func TestSourcesQueryWatchedNamespaces(t *testing.T) {
	kubeSrv, kubeCalls := countingServer(t)
	promSrv, promCalls := countingServer(t)
	s, err := NewSources(SourcesConfig{
		Watched: []string{watchedNS}, PodLogs: true, Pods: podsClient(t, kubeSrv.URL).CoreV1(), PrometheusURL: promSrv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, _ = s.Logs.Logs(ctx, providers.LogQuery{Namespace: watchedNS, Pod: testPod, Container: "api"})
	_, _ = s.Metrics.Query(ctx, providers.MetricsQuery{
		Template: "container_restarts_total", Params: map[string]string{prometheus.ParamNamespace: watchedNS},
		Pods: []string{testPod}, End: time.Now(),
	})
	if kubeCalls.Load() == 0 || promCalls.Load() == 0 {
		t.Fatalf("%d API server and %d Prometheus requests, want some", kubeCalls.Load(), promCalls.Load())
	}
}

// fakeCluster is a clusterSource whose check answers checkErr and counts checks and queries. With block set,
// a check waits until block is closed or its context ends (then it answers the context's error).
type fakeCluster struct {
	mu              sync.Mutex
	checkErr        error
	block           chan struct{}
	checks, queries int
}

func (f *fakeCluster) CheckCluster(ctx context.Context) error {
	f.mu.Lock()
	f.checks++
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkErr
}

func (f *fakeCluster) Query(context.Context, providers.MetricsQuery) (providers.MetricsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	return providers.MetricsResult{}, nil
}

func (f *fakeCluster) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkErr = err
}

func (f *fakeCluster) counts() (checks, queries int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks, f.queries
}

// logSink records the messages of a logger.
type logSink struct {
	mu   sync.Mutex
	msgs []string
}

func (s *logSink) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.msgs = append(s.msgs, args)
	}, funcr.Options{})
}

func (s *logSink) contains(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// eventually polls cond until it holds or a few seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Acceptance (#371, decided 2026-11-05): a configured cluster that no series carries does not stop the
// analyzer; metrics are unavailable (every query fails, which escalates) until a later check finds it.
func TestClusterNotFoundMakesMetricsUnavailable(t *testing.T) {
	src := &fakeCluster{checkErr: fmt.Errorf("check: %w", prometheus.ErrClusterNotFound)}
	c := newClusterCheck(src, logr.Discard())
	clock := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	ctx := context.Background()

	c.checkLogged(ctx, "startup")
	_, err := c.Query(ctx, providers.MetricsQuery{})
	if checks, queries := src.counts(); !errors.Is(err, ErrMetricsUnavailable) || queries != 0 || checks != 1 {
		t.Fatalf("got %v after %d checks and %d queries", err, checks, queries)
	}
	// Found later (the label fixed in the scrape): after ClusterRecheck the next query checks again.
	src.setErr(nil)
	clock = clock.Add(ClusterRecheck)
	if _, err := c.Query(ctx, providers.MetricsQuery{}); err != nil {
		t.Fatal(err)
	}
	if checks, queries := src.counts(); queries != 1 || checks != 2 {
		t.Fatalf("%d checks and %d queries", checks, queries)
	}
	// While the answer is fresh, queries go straight to the source.
	_, _ = c.Query(ctx, providers.MetricsQuery{})
	if checks, queries := src.counts(); checks != 2 || queries != 2 {
		t.Fatalf("%d checks and %d queries", checks, queries)
	}
}

// Review of #406, finding 2: "found" is not permanent. A label that disappears later (a scrape relabeling
// removed) switches metrics off at the next check.
func TestClusterFoundIsRechecked(t *testing.T) {
	src := &fakeCluster{}
	c := newClusterCheck(src, logr.Discard())
	clock := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	ctx := context.Background()

	if _, err := c.Query(ctx, providers.MetricsQuery{}); err != nil {
		t.Fatal(err)
	}
	src.setErr(fmt.Errorf("check: %w", prometheus.ErrClusterNotFound))
	clock = clock.Add(ClusterRecheck)
	_, err := c.Query(ctx, providers.MetricsQuery{})
	if checks, queries := src.counts(); !errors.Is(err, ErrMetricsUnavailable) || checks != 2 || queries != 1 {
		t.Fatalf("got %v after %d checks and %d queries, want metrics unavailable", err, checks, queries)
	}
}

// Review of #406, findings 2 and 3: Start checks again every interval while the manager runs, without any
// query, and returns when its context ends.
func TestClusterCheckRunsPeriodically(t *testing.T) {
	src := &fakeCluster{}
	c := newClusterCheck(src, logr.Discard())
	c.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	eventually(t, "three checks", func() bool { checks, _ := src.counts(); return checks >= 3 })
	// A periodic check that finds no series switches metrics off.
	src.setErr(fmt.Errorf("check: %w", prometheus.ErrClusterNotFound))
	eventually(t, "metrics unavailable", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.state == clusterMissing
	})
	if _, err := c.Query(ctx, providers.MetricsQuery{}); !errors.Is(err, ErrMetricsUnavailable) {
		t.Fatalf("got %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after its context ended")
	}
}

// Review of #406, finding 1: a check in flight does not hold up a query past its own deadline, and the
// queries that arrive meanwhile join that check instead of sending their own.
func TestClusterCheckDoesNotBlockQueriesPastTheirDeadline(t *testing.T) {
	src := &fakeCluster{block: make(chan struct{})}
	c := newClusterCheck(src, logr.Discard())

	first := make(chan error, 1)
	go func() {
		_, err := c.Query(context.Background(), providers.MetricsQuery{})
		first <- err
	}()
	eventually(t, "the first check", func() bool { checks, _ := src.counts(); return checks == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Query(ctx, providers.MetricsQuery{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the query's own deadline", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("the query waited %s past its 50ms deadline", waited)
	}

	close(src.block)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if checks, queries := src.counts(); checks != 1 || queries != 1 {
		t.Fatalf("%d checks and %d queries, want one of each", checks, queries)
	}
}

// Review of #406, finding 5: a startup check cut by the timeout is logged.
func TestClusterCheckTimeoutAtStartupIsLogged(t *testing.T) {
	src := &fakeCluster{block: make(chan struct{})} // never closed: every check runs into its timeout
	var sink logSink
	c := newClusterCheck(src, sink.logger())
	c.timeout, c.interval = 20*time.Millisecond, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	eventually(t, "the timeout logged", func() bool { return sink.contains("at startup") })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A check that fails (Prometheus unreachable) is not "not found": that query fails, and the next one checks
// again, so a Prometheus that is down at startup does not switch metrics off.
func TestClusterCheckErrorIsRetriedOnTheNextQuery(t *testing.T) {
	src := &fakeCluster{checkErr: &providers.RetryableError{Err: errors.New("unreachable")}}
	c := newClusterCheck(src, logr.Discard())
	ctx := context.Background()
	c.checkLogged(ctx, "startup")
	if _, err := c.Query(ctx, providers.MetricsQuery{}); err == nil || errors.Is(err, ErrMetricsUnavailable) {
		t.Fatalf("got %v", err)
	}
	src.setErr(nil)
	_, err := c.Query(ctx, providers.MetricsQuery{})
	if checks, queries := src.counts(); err != nil || checks != 3 || queries != 1 {
		t.Fatalf("got %v after %d checks and %d queries", err, checks, queries)
	}
}

// With a cluster setting, the metrics source is the check, which runs at startup.
func TestClusterSettingWrapsTheSource(t *testing.T) {
	s, err := NewSources(SourcesConfig{
		Watched: []string{watchedNS}, PrometheusURL: testPromURL, PrometheusCluster: testCluster,
	})
	if err != nil || s.ClusterCheck == nil || s.Metrics != providers.MetricsSource(s.ClusterCheck) {
		t.Fatalf("got %+v, %v", s, err)
	}
	if s.ClusterCheck.NeedLeaderElection() {
		t.Error("the check must run on every replica")
	}
}
