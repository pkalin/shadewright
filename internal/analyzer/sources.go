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
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/logs/kube"
	"github.com/pkalin/shadewright/internal/providers/metrics/prometheus"
	"github.com/pkalin/shadewright/internal/redact"
)

// SourcesConfig configures the metrics and log sources of the context builder (design 2.3 and 3.3).
type SourcesConfig struct {
	// Watched are the watched namespaces (ParseNamespaces of --watch-namespaces): the one set the cache, the
	// RoleBindings, both sources and the context builder use. A query about any other namespace is refused by
	// the source before any request. With none, no source is built.
	Watched []string
	// PodLogs enables the pod logs source (--pod-logs). It needs Pods.
	PodLogs bool
	// Pods reads pod logs and pods: CoreV1() of a clientset with the analyzer's identity.
	Pods kube.PodsGetter
	// Redactor redacts the log excerpts: pass the one the context builder uses, so that both passes apply one
	// rule set. Nil means the built-in rules of package redact.
	Redactor *redact.Redactor
	// PrometheusURL is the base URL of the Prometheus query API (--prometheus-url); empty means no metrics
	// source.
	PrometheusURL string
	// PrometheusCluster is the cluster label value of a shared Prometheus (--prometheus-cluster); empty means
	// a Prometheus dedicated to this cluster. It needs PrometheusURL.
	PrometheusCluster string
	// Log receives the results of the cluster check.
	Log logr.Logger
}

// Sources are the configured sources. A field is nil when its source is not configured, which the context
// builder reports as "not available".
type Sources struct {
	Metrics providers.MetricsSource
	Logs    providers.LogSource
	// ClusterCheck checks PrometheusCluster at startup and every [ClusterRecheck]; nil when no cluster is
	// configured. Add it to the manager.
	ClusterCheck *ClusterCheck
}

// NewSources builds the sources from cfg. It makes no request. It refuses a configuration the sources cannot
// serve, such as more watched namespaces than a source takes (kube.MaxNamespaces, prometheus.MaxNamespaces)
// or a cluster without a URL; the errors name the flags and the limit, never a namespace or the URL. With no
// watched namespace it builds no source and no cluster check: no source could be asked anything.
func NewSources(cfg SourcesConfig) (Sources, error) {
	if cfg.PrometheusURL == "" && cfg.PrometheusCluster != "" {
		return Sources{}, fmt.Errorf("--prometheus-cluster needs --prometheus-url: %w", providers.ErrInvalidConfig)
	}
	if len(cfg.Watched) == 0 {
		return Sources{}, nil
	}
	var s Sources
	if cfg.PodLogs {
		logs, err := kube.New(kube.Config{Pods: cfg.Pods, Namespaces: cfg.Watched, Redactor: cfg.Redactor})
		if err != nil {
			return Sources{}, fmt.Errorf("pod logs source (--pod-logs, --watch-namespaces): %w", err)
		}
		s.Logs = logs
	}
	if cfg.PrometheusURL == "" {
		return s, nil
	}
	metrics, err := prometheus.New(prometheus.Config{
		URL: cfg.PrometheusURL, Namespaces: cfg.Watched, Cluster: cfg.PrometheusCluster,
	})
	if err != nil {
		return Sources{}, fmt.Errorf("prometheus source (--prometheus-url, --prometheus-cluster, "+
			"--watch-namespaces): %w", err)
	}
	if cfg.PrometheusCluster == "" {
		s.Metrics = metrics
		return s, nil
	}
	s.ClusterCheck = newClusterCheck(metrics, cfg.Log)
	s.Metrics = s.ClusterCheck
	return s, nil
}

// ClusterRecheck is the interval of the cluster check after startup, and the age of its last answer after
// which a query checks again itself.
const ClusterRecheck = 10 * time.Minute

// clusterCheckTimeout bounds one check.
const clusterCheckTimeout = 30 * time.Second

// ErrMetricsUnavailable is the error of every metrics query while the cluster check finds no series of the
// configured cluster: the context builder marks the metrics "could not fetch", which escalates (design 3.3),
// instead of reading empty answers as evidence of absence.
var ErrMetricsUnavailable = errors.New("metrics unavailable: no series carries the configured cluster label")

// clusterSource is the part of the Prometheus source the check uses.
type clusterSource interface {
	providers.MetricsSource
	CheckCluster(ctx context.Context) error
}

// clusterState is what the last check that reached an answer found.
type clusterState int

const (
	// clusterUnknown: no check has answered yet (none ran, or each one failed).
	clusterUnknown clusterState = iota
	clusterFound
	clusterMissing
)

// clusterProbe is one check in flight; err is set before done is closed.
type clusterProbe struct {
	done chan struct{}
	err  error
}

// ClusterCheck is the metrics source of a Prometheus with a cluster setting. It checks that the source holds
// series of the cluster (prometheus.Source.CheckCluster) at startup and every [ClusterRecheck] while the
// manager runs ([ClusterCheck.Start]). While the last answer is "found", queries go to the source. While it is
// "not found", it logs an error and refuses every query with [ErrMetricsUnavailable]; the analyzer keeps
// running, so a mistyped label never crash-loops it, and a label that disappears later switches metrics off at
// the next check. A check that fails (Prometheus unreachable) changes nothing it knew. Before any answer, or
// once the last one is older than ClusterRecheck, a query checks first. One check runs at a time, on its own
// goroutine and bounded by its own timeout; a caller waits for it only as long as its own context allows.
type ClusterCheck struct {
	src      clusterSource
	log      logr.Logger
	now      func() time.Time
	interval time.Duration
	timeout  time.Duration

	// mu guards the fields below. It is never held across a request.
	mu        sync.Mutex
	state     clusterState
	checkedAt time.Time     // when the last check answered (found or not found); zero when none did
	inflight  *clusterProbe // the check in flight; nil when none
}

var _ providers.MetricsSource = (*ClusterCheck)(nil)

func newClusterCheck(src clusterSource, log logr.Logger) *ClusterCheck {
	return &ClusterCheck{src: src, log: log, now: time.Now, interval: ClusterRecheck, timeout: clusterCheckTimeout}
}

// Query runs q while the cluster check finds the series; see [ClusterCheck].
func (c *ClusterCheck) Query(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
	if err := c.ready(ctx); err != nil {
		return providers.MetricsResult{}, err
	}
	return c.src.Query(ctx, q)
}

// Start checks once, then every [ClusterRecheck] until ctx ends, and logs the results. A failed check is
// logged and does not stop it. It implements manager.Runnable.
func (c *ClusterCheck) Start(ctx context.Context) error {
	c.checkLogged(ctx, "Failed to check the Prometheus cluster label at startup")
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.checkLogged(ctx, "Failed to check the Prometheus cluster label")
		}
	}
}

// NeedLeaderElection is false: every replica checks the source it will query.
func (c *ClusterCheck) NeedLeaderElection() bool { return false }

// checkLogged runs one check, waiting at most the check timeout, and logs a failure, a timeout included, unless
// ctx ended (a shutdown). "Not found" is logged when the check records it.
func (c *ClusterCheck) checkLogged(ctx context.Context, msg string) {
	waitCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.check(waitCtx); err != nil && !errors.Is(err, ErrMetricsUnavailable) && ctx.Err() == nil {
		c.log.Error(err, msg+"; metrics stay as the last check found them", "timeout", c.timeout)
	}
}

// ready answers from the last check while it is fresh, and checks otherwise.
func (c *ClusterCheck) ready(ctx context.Context) error {
	c.mu.Lock()
	state := c.state
	fresh := !c.checkedAt.IsZero() && c.now().Sub(c.checkedAt) < c.interval
	c.mu.Unlock()
	switch {
	case fresh && state == clusterFound:
		return nil
	case fresh && state == clusterMissing:
		return ErrMetricsUnavailable
	default:
		return c.check(ctx)
	}
}

// check joins the check in flight or starts one, and waits for it until ctx ends. The check runs on its own
// goroutine with its own timeout, detached from ctx, so a caller whose context ends returns at once and the
// result is still recorded for the next caller.
func (c *ClusterCheck) check(ctx context.Context) error {
	c.mu.Lock()
	p := c.inflight
	if p == nil {
		p = &clusterProbe{done: make(chan struct{})}
		c.inflight = p
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		go func() {
			defer cancel()
			c.finish(p, c.src.CheckCluster(probeCtx))
		}()
	}
	c.mu.Unlock()
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		return fmt.Errorf("waiting for the prometheus cluster check: %w", context.Cause(ctx))
	}
}

// finish records the result of p, releases its waiters and logs the result: "not found" each time, "found"
// when it was not found before.
func (c *ClusterCheck) finish(p *clusterProbe, err error) {
	c.mu.Lock()
	prev := c.state
	switch {
	case err == nil:
		c.state, c.checkedAt = clusterFound, c.now()
	case errors.Is(err, prometheus.ErrClusterNotFound):
		c.state, c.checkedAt = clusterMissing, c.now()
		err = ErrMetricsUnavailable
	default:
		err = fmt.Errorf("checking the prometheus cluster label: %w", err)
	}
	c.inflight = nil
	p.err = err
	c.mu.Unlock()
	close(p.done)

	switch {
	case err == nil && prev != clusterFound:
		c.log.Info("Found series of the configured cluster in Prometheus")
	case errors.Is(err, ErrMetricsUnavailable):
		c.log.Error(prometheus.ErrClusterNotFound, "Prometheus holds no series with the --prometheus-cluster value "+
			"from kube-state-metrics and cAdvisor: metrics are unavailable, and analyses that query them escalate, "+
			"until it does; check the value and that the label is stored on the series, not added as an external "+
			"label", "recheckAfter", c.interval)
	}
}
