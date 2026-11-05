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
)

// SourcesConfig configures the metrics and log sources of the context builder (design 2.3 and 3.3).
type SourcesConfig struct {
	// Watched are the watched namespaces (ParseNamespaces of --watch-namespaces): the one set the cache, the
	// RoleBindings, both sources and the context builder use. A query about any other namespace is refused by
	// the source before any request.
	Watched []string
	// PodLogs enables the pod logs source (--pod-logs). It needs Pods.
	PodLogs bool
	// Pods reads pod logs and pods: CoreV1() of a clientset with the analyzer's identity.
	Pods kube.PodsGetter
	// PrometheusURL is the base URL of the Prometheus query API (--prometheus-url); empty means no metrics
	// source.
	PrometheusURL string
	// PrometheusCluster is the cluster label value of a shared Prometheus (--prometheus-cluster); empty means
	// a Prometheus dedicated to this cluster. It needs PrometheusURL.
	PrometheusCluster string
	// Log receives the result of the cluster check.
	Log logr.Logger
}

// Sources are the configured sources. A field is nil when its source is not configured, which the context
// builder reports as "not available".
type Sources struct {
	Metrics providers.MetricsSource
	Logs    providers.LogSource
	// ClusterCheck runs the startup check of PrometheusCluster; nil when no cluster is configured. Add it to
	// the manager.
	ClusterCheck *ClusterCheck
}

// NewSources builds the sources from cfg. It makes no request. It refuses a configuration the sources cannot
// serve: with PodLogs, more watched namespaces than the logs source takes (kube.MaxNamespaces); with a
// Prometheus URL, more than the metrics source takes (prometheus.MaxNamespaces); a cluster without a URL.
// The errors name the flags and the limit, never a namespace or the URL.
func NewSources(cfg SourcesConfig) (Sources, error) {
	var s Sources
	if cfg.PodLogs {
		if len(cfg.Watched) > kube.MaxNamespaces {
			return Sources{}, fmt.Errorf("--pod-logs reads at most %d watched namespaces, and --watch-namespaces "+
				"lists %d: watch fewer namespaces or set --pod-logs=false: %w", kube.MaxNamespaces, len(cfg.Watched),
				providers.ErrInvalidConfig)
		}
		logs, err := kube.New(kube.Config{Pods: cfg.Pods, Namespaces: cfg.Watched})
		if err != nil {
			return Sources{}, fmt.Errorf("pod logs source: %w", err)
		}
		s.Logs = logs
	}
	if cfg.PrometheusURL == "" {
		if cfg.PrometheusCluster != "" {
			return Sources{}, fmt.Errorf("--prometheus-cluster needs --prometheus-url: %w", providers.ErrInvalidConfig)
		}
		return s, nil
	}
	if len(cfg.Watched) > prometheus.MaxNamespaces {
		return Sources{}, fmt.Errorf("--prometheus-url queries at most %d watched namespaces, and "+
			"--watch-namespaces lists %d: watch fewer namespaces or leave --prometheus-url empty: %w",
			prometheus.MaxNamespaces, len(cfg.Watched), providers.ErrInvalidConfig)
	}
	metrics, err := prometheus.New(prometheus.Config{
		URL: cfg.PrometheusURL, Namespaces: cfg.Watched, Cluster: cfg.PrometheusCluster,
	})
	if err != nil {
		return Sources{}, fmt.Errorf("prometheus source (--prometheus-url, --prometheus-cluster): %w", err)
	}
	if cfg.PrometheusCluster == "" {
		s.Metrics = metrics
		return s, nil
	}
	s.ClusterCheck = newClusterCheck(metrics, cfg.Log)
	s.Metrics = s.ClusterCheck
	return s, nil
}

// ClusterRecheck is how long metrics stay unavailable after the cluster check found no series of the
// configured cluster, before the next query checks again.
const ClusterRecheck = 10 * time.Minute

// clusterCheckTimeout bounds the check at startup.
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

// ClusterCheck is the metrics source of a Prometheus with a cluster setting. It checks that the source holds
// series of the cluster (prometheus.Source.CheckCluster) once at startup ([ClusterCheck.Start]) and, until a
// check succeeds, before each query. When a check finds none, it logs an error and refuses every query with
// [ErrMetricsUnavailable] for [ClusterRecheck], then checks again; the analyzer keeps running, so a mistyped
// label never crash-loops it. A check that fails (Prometheus unreachable) refuses that query with its error and
// is tried again on the next one. Once a check succeeds, queries go straight to the source.
type ClusterCheck struct {
	src clusterSource
	log logr.Logger
	now func() time.Time

	// mu serializes the checks, so that concurrent queries do not each send one.
	mu sync.Mutex
	// found: a check succeeded. missingAt: when a check last found no series (zero when none did).
	found     bool
	missingAt time.Time
}

var _ providers.MetricsSource = (*ClusterCheck)(nil)

func newClusterCheck(src clusterSource, log logr.Logger) *ClusterCheck {
	return &ClusterCheck{src: src, log: log, now: time.Now}
}

// Query runs q once the cluster check has succeeded; see [ClusterCheck].
func (c *ClusterCheck) Query(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
	if err := c.check(ctx); err != nil {
		return providers.MetricsResult{}, err
	}
	return c.src.Query(ctx, q)
}

// Start runs the check once, bounded by its own timeout, and logs the result. It never fails: a failed check
// is repeated before the next query. It implements manager.Runnable.
func (c *ClusterCheck) Start(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, clusterCheckTimeout)
	defer cancel()
	err := c.check(ctx)
	if err != nil && !errors.Is(err, ErrMetricsUnavailable) && ctx.Err() == nil {
		c.log.Error(err, "Failed to check the Prometheus cluster label at startup; checking again before the next query")
	}
	return nil
}

// NeedLeaderElection is false: every replica checks the source it will query.
func (c *ClusterCheck) NeedLeaderElection() bool { return false }

func (c *ClusterCheck) check(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.found {
		return nil
	}
	if !c.missingAt.IsZero() && c.now().Sub(c.missingAt) < ClusterRecheck {
		return ErrMetricsUnavailable
	}
	err := c.src.CheckCluster(ctx)
	switch {
	case err == nil:
		c.found, c.missingAt = true, time.Time{}
		c.log.Info("Found series of the configured cluster in Prometheus")
		return nil
	case errors.Is(err, prometheus.ErrClusterNotFound):
		c.missingAt = c.now()
		c.log.Error(err, "Prometheus holds no series with the --prometheus-cluster value from kube-state-metrics "+
			"and cAdvisor: metrics are unavailable, and analyses that query them escalate, until it does; check the value "+
			"and that the label is stored on the series, not added as an external label",
			"recheckAfter", ClusterRecheck)
		return ErrMetricsUnavailable
	default:
		return fmt.Errorf("checking the prometheus cluster label: %w", err)
	}
}
