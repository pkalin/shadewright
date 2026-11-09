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

// Command analyzer runs the collector, the Incident controller and the analysis. It has its own
// ServiceAccount and identity: it may create RemediationRequests but never act on them.
package main

import (
	"flag"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/analyzer/detection"
	"github.com/pkalin/shadewright/internal/cmdutil"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/modules/aks"
	"github.com/pkalin/shadewright/internal/modules/cloudflared"
	"github.com/pkalin/shadewright/internal/modules/eks"
	"github.com/pkalin/shadewright/internal/modules/flux"
	"github.com/pkalin/shadewright/internal/modules/gke"
	"github.com/pkalin/shadewright/internal/modules/keda"
	"github.com/pkalin/shadewright/internal/modules/kyverno"
	"github.com/pkalin/shadewright/internal/modules/secretsstore"
	"github.com/pkalin/shadewright/internal/redact"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(remediationv1alpha1.AddToScheme(scheme))
}

func main() {
	var opts cmdutil.Options
	var namespace, watchNamespaces, clusterIDFlag, prometheusURL, prometheusCluster string
	var maxIncidents int
	var podLogs bool
	opts.BindFlags(flag.CommandLine)
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"The operator namespace: where Incidents and RemediationRequests are written. "+
			"Defaults to the POD_NAMESPACE environment variable.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated namespaces whose events, pods and workloads the analyzer reads; each one needs a "+
			"RoleBinding to the analyzer-watch-role (config/watch). Empty (the default) watches no namespace, "+
			"never all of them: the analyzer then starts no collector and records no Incident.")
	flag.StringVar(&clusterIDFlag, "cluster-id", "",
		"A name for this cluster, hashed into every fingerprint so Incidents from two clusters never merge. "+
			"Required: 1 to 63 letters, digits, '.', '_' or '-', starting and ending with a letter or digit. "+
			"Changing it starts every Incident afresh.")
	flag.IntVar(&maxIncidents, "max-incidents", incidents.DefaultMaxIncidents,
		"The most Incidents kept in the operator namespace. At the cap a new fingerprint is not stored: it is "+
			"counted in shadewright_incident_observations_refused_total and reported in a Warning event.")
	flag.BoolVar(&podLogs, "pod-logs", true,
		"Read bounded, redacted excerpts of pod logs in the watched namespaces for the analysis context "+
			"(get on pods/log, granted by the analyzer-watch-role). At most 256 watched namespaces: with more, "+
			"the analyzer refuses to start unless this is false.")
	flag.StringVar(&prometheusURL, "prometheus-url", "",
		"Base URL of the Prometheus query API the analysis context reads metrics from, with approved query "+
			"templates only, such as http://prometheus-server.prometheus.svc. Empty (the default): no metrics. "+
			"At most 256 watched namespaces when set.")
	flag.StringVar(&prometheusCluster, "prometheus-cluster", "",
		"The cluster label value of this cluster's series, for a Prometheus shared by several clusters; empty "+
			"for a Prometheus of this cluster alone. Checked at startup and every 10 minutes: while no series "+
			"carries it, metrics are unavailable (analyses that query them escalate) and the analyzer logs an error.")
	// Nil while the flag is not given (an install without the chart): every module kind is then checked.
	var moduleGrants []string
	flag.Func("module-grants",
		"Comma-separated module grants the install rendered ("+strings.Join(collector.ModuleGrants, ", ")+
			"; the chart passes those of its analyzer.modules values). A module kind whose grant is not listed "+
			"is not watched: reported as not granted, or, for the optional kyverno-cluster, left out without a "+
			"report. Not set: every module kind is checked and watched once the analyzer may list it.",
		func(v string) error { moduleGrants = parseList(v); return nil })
	var af analysisFlags
	af.bind(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(cmdutil.NewLogger(&opts))

	if namespace == "" {
		setupLog.Error(nil, "The operator namespace is not set: use --namespace or POD_NAMESPACE")
		os.Exit(1)
	}
	watched, err := analyzer.ParseNamespaces(watchNamespaces)
	if err != nil {
		setupLog.Error(err, "Invalid --watch-namespaces")
		os.Exit(1)
	}
	clusterID, err := analyzer.ParseClusterID(clusterIDFlag)
	if err != nil {
		setupLog.Error(err, "Invalid --cluster-id")
		os.Exit(1)
	}
	if maxIncidents < 1 {
		setupLog.Error(nil, "Invalid --max-incidents: it must be at least 1", "value", maxIncidents)
		os.Exit(1)
	}
	analyzer.SetWatchedNamespaces(len(watched))
	if len(watched) == 0 {
		setupLog.Info("Watching no namespaces: no events or pods are read and no Incident is recorded; "+
			"set --watch-namespaces to watch some", "operatorNamespace", namespace, "clusterID", clusterID)
	} else {
		setupLog.Info("Watching namespaces", "namespaces", watched, "operatorNamespace", namespace,
			"clusterID", clusterID, "maxIncidents", maxIncidents)
	}

	mgrOpts := opts.ManagerOptions(setupLog, scheme, cmdutil.AnalyzerLease)
	mgrOpts.Cache = analyzer.CacheOptions(namespace, watched)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// The Incident controller consumes the collector's observations and tells it what each occurrence has
	// already counted. Its Warning events are about this Pod (POD_NAME, from the downward API), its Normal
	// events about a skipped first analysis about the Incident; without a Pod name (make run) neither is
	// written, and the refusals and skips are only logged or counted. It also marks the Incidents due for
	// analysis (condition AnalysisDue), with the default debounce, settle limit, cooldown, analysis timeout
	// and recent window, and the daily budget kept in the ConfigMap incidents.BudgetName in the operator namespace.
	cfg := incidents.Config{Namespace: namespace, MaxIncidents: maxIncidents}
	if pod := os.Getenv("POD_NAME"); pod != "" {
		// The core/v1 recorder writes with the grant on core Events that the leader election Role already
		// holds in the operator namespace, so the cap event needs no grant of its own. controller-runtime
		// deprecates it in favour of the events.k8s.io recorder; moving to that one needs a new grant on
		// events.k8s.io Events, so it waits for a review of invariant 6 (AGENTS.md).
		cfg.Recorder = mgr.GetEventRecorderFor("shadewright-analyzer") //nolint:staticcheck // see above
		cfg.Regarding = &corev1.Pod{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: pod},
		}
	}
	inc, err := incidents.New(cfg)
	if err != nil {
		setupLog.Error(err, "Failed to create the Incident controller")
		os.Exit(1)
	}
	// The compiled-in modules (ADR-0019). The generic platform is always added by modules.New; a refusal stops
	// the start. A module is active only when RemediationSettings enables it and it is detected.
	registry, err := modules.New([]modules.Platform{aks.Platform{}, eks.Platform{}, gke.Platform{}},
		[]modules.Integration{cloudflared.Module(), flux.Module(), keda.Module(), kyverno.Module(), secretsstore.Module()})
	if err != nil {
		setupLog.Error(err, "Failed to build the module registry")
		os.Exit(1)
	}
	// The collector evaluates the symptom sources of the modules detection makes active (#380): their kinds
	// (collector.SourceKinds) are watched, and a source runs only while its module is enabled and detected.
	// An add-on's own kinds are watched only once served and granted (the chart's analyzer.modules values,
	// #409); the kinds it does not watch are reported by detection in RemediationSettings status, which a
	// change of their state brings forward (recheck). det is set below, before the manager starts.
	var det *detection.Detector
	recheck := make(chan struct{}, 1)
	col, err := collector.New(collector.Config{
		ClusterID: clusterID, Handled: inc.Handled, Namespaces: watched, Grants: moduleGrants,
		Modules:      func() modules.Selection { return det.Selection() },
		WatchChanged: notify(recheck),
	})
	if err != nil {
		setupLog.Error(err, "Failed to create the collector")
		os.Exit(1)
	}
	detCfg := detection.Config{
		Registry:          registry,
		GroupVersions:     detection.DiscoveryGroupVersions(mgr.GetConfig()),
		WatchedNamespaces: len(watched),
	}
	if len(watched) > 0 {
		detCfg.SourcesNotWatched, detCfg.Recheck = notWatched(col), recheck
	}
	det, err = detection.New(detCfg)
	if err != nil {
		setupLog.Error(err, "Failed to create platform detection")
		os.Exit(1)
	}
	// With no watched namespace the collector is not set up, so no informer on events or pods is started: the
	// cache would hold the operator namespace only (analyzer.CacheOptions), where the analyzer has no grant on
	// them, and its sync would fail. Its observation channel then stays empty.
	if len(watched) > 0 {
		if err := col.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to set up the collector")
			os.Exit(1)
		}
	}
	if err := inc.SetupWithManager(mgr, col.Observations()); err != nil {
		setupLog.Error(err, "Failed to set up the Incident controller")
		os.Exit(1)
	}
	if err := det.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to set up platform detection")
		os.Exit(1)
	}

	// The context builder (design 3.3) and its sources. The analysis loop calls the builder; it is built even
	// when the loop does not start, so that the sources' configuration is checked at start. The sources and the
	// builder are given the watched namespaces, the same set as the cache and the RoleBindings, and refuse any
	// other before a request; with none, NewSources builds no source and no cluster check. Pod logs are read with the
	// analyzer's own identity, through the pods/log grant of the watch role, and redacted with the builder's
	// redactor, so that both passes apply one rule set.
	redactor, err := redact.New()
	if err != nil {
		setupLog.Error(err, "Failed to create the redactor")
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "Failed to create the Kubernetes client")
		os.Exit(1)
	}
	sources, err := analyzer.NewSources(analyzer.SourcesConfig{
		Watched: watched, PodLogs: podLogs, Pods: clientset.CoreV1(), Redactor: redactor,
		PrometheusURL: prometheusURL, PrometheusCluster: prometheusCluster, Log: ctrl.Log.WithName("metrics-source"),
	})
	if err != nil {
		setupLog.Error(err, "Invalid context source configuration")
		os.Exit(1)
	}
	if sources.ClusterCheck != nil {
		if err := mgr.Add(sources.ClusterCheck); err != nil {
			setupLog.Error(err, "Failed to set up the Prometheus cluster check")
			os.Exit(1)
		}
	}
	contextConfig := incidentcontext.Config{
		Pods: mgr.GetCache(), Objects: mgr.GetAPIReader(), Namespaces: watched,
		Metrics: sources.Metrics, Logs: sources.Logs, Redactor: redactor,
	}
	if err := contextPolicies(af, mgr.GetAPIReader(), namespace, &contextConfig); err != nil {
		setupLog.Error(err, "Invalid analysis configuration")
		os.Exit(1)
	}
	contextBuilder, err := incidentcontext.New(contextConfig)
	if err != nil {
		setupLog.Error(err, "Failed to create the context builder")
		os.Exit(1)
	}
	setupLog.Info("Context sources", "podLogs", sources.Logs != nil, "metrics", sources.Metrics != nil,
		"prometheusCluster", prometheusCluster != "", "policyBounds", contextConfig.PolicyReader != nil)

	// The analysis traces (#90): off unless the OTEL_* environment turns them on.
	tracerProvider, err := newTracerProvider(ctrl.Log.WithName("tracing"))
	if err != nil {
		setupLog.Error(err, "Invalid tracing configuration")
		os.Exit(1)
	}
	setupLog.Info("Analysis tracing", "enabled", tracerProvider != nil)
	// A nil *TracerProvider in the interface would not be a nil interface: pass none when tracing is off.
	var tp trace.TracerProvider
	if tracerProvider != nil {
		tp = tracerProvider
	}

	af.namespace, af.watched, af.claimTimeout = namespace, watched, inc.AnalysisTimeout()
	if err := setupAnalysis(mgr, inc, af, contextBuilder, sources, redactor, tp); err != nil {
		setupLog.Error(err, "Failed to set up the analysis")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting analyzer")
	err = mgr.Start(ctrl.SetupSignalHandler())
	// The manager has stopped every analysis: flush the spans still queued, bounded. An exit before the start
	// has made no span.
	shutdownTracing(tracerProvider, ctrl.Log.WithName("tracing"))
	if err != nil {
		setupLog.Error(err, "Failed to run analyzer")
		os.Exit(1)
	}
}

// parseList splits a comma-separated flag value, dropping blanks: never nil, so a flag given empty is told
// apart from one not given.
func parseList(v string) []string {
	out := []string{}
	for g := range strings.SplitSeq(v, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// notify returns a function that leaves a notice on ch without blocking; one notice pending is enough.
func notify(ch chan<- struct{}) func() {
	return func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// notWatched adapts the collector's report of the symptom source kinds it does not watch for detection.
func notWatched(col *collector.Collector) func() ([]detection.NotWatched, bool) {
	return func() ([]detection.NotWatched, bool) {
		kinds, complete := col.SourcesNotWatched()
		out := make([]detection.NotWatched, 0, len(kinds))
		for _, w := range kinds {
			out = append(out, detection.NotWatched{
				Group: w.Kind.Group, Version: w.Kind.Version, Kind: w.Kind.Kind, State: string(w.State),
			})
		}
		return out, complete
	}
}
