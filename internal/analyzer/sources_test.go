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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
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
	if !errors.Is(err, providers.ErrInvalidConfig) || !strings.Contains(err.Error(), "at most 256") ||
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
	if !errors.Is(err, providers.ErrInvalidConfig) || !strings.Contains(err.Error(), "at most 256") ||
		!strings.Contains(err.Error(), "--prometheus-url") {
		t.Fatalf("got %v", err)
	}
}

func TestSourcesRefuseBadConfig(t *testing.T) {
	for name, cfg := range map[string]SourcesConfig{
		"cluster without URL": {PrometheusCluster: testCluster},
		"bad URL":             {PrometheusURL: "ftp://prometheus.example"},
		"bad cluster":         {PrometheusURL: testPromURL, PrometheusCluster: "a b"},
		"logs without client": {PodLogs: true},
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
// before a request reaches the API server or Prometheus; an empty list refuses every query.
func TestSourcesRefuseUnwatchedNamespacesBeforeAnyRequest(t *testing.T) {
	for name, watched := range map[string][]string{"other namespace": {watchedNS}, "no namespace": nil} {
		kubeSrv, kubeCalls := countingServer(t)
		promSrv, promCalls := countingServer(t)
		s, err := NewSources(SourcesConfig{
			Watched: watched, PodLogs: true, Pods: podsClient(t, kubeSrv.URL).CoreV1(), PrometheusURL: promSrv.URL,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if _, err := s.Logs.Logs(ctx, providers.LogQuery{Namespace: "payments", Pod: testPod, Container: "api"}); err == nil {
			t.Errorf("%s: logs of an unwatched namespace returned", name)
		}
		_, err = s.Metrics.Query(ctx, providers.MetricsQuery{
			Template: "container_restarts_total", Params: map[string]string{prometheus.ParamNamespace: "payments"},
			Pods: []string{testPod}, End: time.Now(),
		})
		if !errors.Is(err, prometheus.ErrInvalidQuery) {
			t.Errorf("%s: metrics of an unwatched namespace: %v", name, err)
		}
		if kubeCalls.Load() != 0 || promCalls.Load() != 0 {
			t.Errorf("%s: %d API server and %d Prometheus requests, want none", name, kubeCalls.Load(), promCalls.Load())
		}
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

// fakeCluster is a clusterSource whose check answers checkErr and counts checks and queries.
type fakeCluster struct {
	checkErr        error
	checks, queries int
}

func (f *fakeCluster) CheckCluster(context.Context) error { f.checks++; return f.checkErr }

func (f *fakeCluster) Query(context.Context, providers.MetricsQuery) (providers.MetricsResult, error) {
	f.queries++
	return providers.MetricsResult{}, nil
}

// Acceptance (#371, decided 2026-11-05): a configured cluster that no series carries does not stop the
// analyzer; metrics are unavailable (every query fails, which escalates) until a later check finds it.
func TestClusterNotFoundMakesMetricsUnavailable(t *testing.T) {
	src := &fakeCluster{checkErr: fmt.Errorf("check: %w", prometheus.ErrClusterNotFound)}
	c := newClusterCheck(src, logr.Discard())
	clock := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	_, err := c.Query(context.Background(), providers.MetricsQuery{})
	if !errors.Is(err, ErrMetricsUnavailable) || src.queries != 0 || src.checks != 1 {
		t.Fatalf("got %v after %d checks and %d queries", err, src.checks, src.queries)
	}
	// Found later (the label fixed in the scrape): after ClusterRecheck the next query checks again.
	src.checkErr = nil
	clock = clock.Add(ClusterRecheck)
	if _, err := c.Query(context.Background(), providers.MetricsQuery{}); err != nil || src.queries != 1 || src.checks != 2 {
		t.Fatalf("got %v after %d checks and %d queries", err, src.checks, src.queries)
	}
	// Once found, never checked again.
	_, _ = c.Query(context.Background(), providers.MetricsQuery{})
	if src.checks != 2 || src.queries != 2 {
		t.Fatalf("%d checks and %d queries", src.checks, src.queries)
	}
}

// A check that fails (Prometheus unreachable) is not "not found": that query fails, and the next one checks
// again, so a Prometheus that is down at startup does not switch metrics off.
func TestClusterCheckErrorIsRetriedOnTheNextQuery(t *testing.T) {
	src := &fakeCluster{checkErr: &providers.RetryableError{Err: errors.New("unreachable")}}
	c := newClusterCheck(src, logr.Discard())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Query(context.Background(), providers.MetricsQuery{}); err == nil || errors.Is(err, ErrMetricsUnavailable) {
		t.Fatalf("got %v", err)
	}
	src.checkErr = nil
	if _, err := c.Query(context.Background(), providers.MetricsQuery{}); err != nil || src.checks != 3 || src.queries != 1 {
		t.Fatalf("got %v after %d checks and %d queries", err, src.checks, src.queries)
	}
}

// With a cluster setting, the metrics source is the check, which runs at startup.
func TestClusterSettingWrapsTheSource(t *testing.T) {
	s, err := NewSources(SourcesConfig{PrometheusURL: testPromURL, PrometheusCluster: testCluster})
	if err != nil || s.ClusterCheck == nil || s.Metrics != providers.MetricsSource(s.ClusterCheck) {
		t.Fatalf("got %+v, %v", s, err)
	}
	if s.ClusterCheck.NeedLeaderElection() {
		t.Error("the check must run on every replica")
	}
}
