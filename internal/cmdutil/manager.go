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

// Package cmdutil holds the manager setup shared by the executor and the analyzer binaries: flags for
// metrics, health probes and leader election, and the TLS defaults.
package cmdutil

import (
	"crypto/tls"
	"flag"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Leader election Lease names. The manifests create both Leases (config/rbac/<id>/leader_election.yaml, after
// the kustomize name prefix) and each identity may only get, update and patch its own: neither may create
// a Lease, so neither can squat the other's before it exists.
const (
	ExecutorLease = "shadewright-executor-leader-election"
	AnalyzerLease = "shadewright-analyzer-leader-election"
)

// Options are the flags both binaries share.
type Options struct {
	MetricsAddr          string
	ProbeAddr            string
	EnableLeaderElection bool
	SecureMetrics        bool
	EnableHTTP2          bool
	MetricsCertPath      string
	MetricsCertName      string
	MetricsCertKey       string
	Zap                  zap.Options
}

// BindFlags registers the shared flags on fs.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.MetricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	fs.StringVar(&o.ProbeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(&o.EnableLeaderElection, "leader-elect", false,
		"Enable leader election, so that only one replica is active at a time.")
	fs.BoolVar(&o.SecureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	fs.StringVar(&o.MetricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	fs.StringVar(&o.MetricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	fs.StringVar(&o.MetricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	fs.BoolVar(&o.EnableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	o.Zap.Development = true
	o.Zap.BindFlags(fs)
}

// TLSOpts returns the TLS options for the metrics and webhook servers. HTTP/2 is off unless enabled, to
// avoid the HTTP/2 stream cancellation and rapid reset vulnerabilities.
func (o *Options) TLSOpts(log logr.Logger) []func(*tls.Config) {
	if o.EnableHTTP2 {
		return nil
	}
	return []func(*tls.Config){func(c *tls.Config) {
		log.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}}
}

// ManagerOptions returns manager options with the shared settings applied. Callers add their own cache
// restrictions and webhook server.
func (o *Options) ManagerOptions(log logr.Logger, scheme *runtime.Scheme, leaderElectionID string) ctrl.Options {
	metrics := metricsserver.Options{
		BindAddress:   o.MetricsAddr,
		SecureServing: o.SecureMetrics,
		TLSOpts:       o.TLSOpts(log),
	}
	if o.SecureMetrics {
		metrics.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if o.MetricsCertPath != "" {
		log.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", o.MetricsCertPath, "metrics-cert-name", o.MetricsCertName,
			"metrics-cert-key", o.MetricsCertKey)
		metrics.CertDir = o.MetricsCertPath
		metrics.CertName = o.MetricsCertName
		metrics.KeyName = o.MetricsCertKey
	}
	return ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metrics,
		HealthProbeBindAddress: o.ProbeAddr,
		LeaderElection:         o.EnableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	}
}

// NewLogger returns the zap logger configured by the flags.
func NewLogger(o *Options) logr.Logger {
	return zap.New(zap.UseFlagOptions(&o.Zap))
}
