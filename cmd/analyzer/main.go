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

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/cmdutil"
	"github.com/pkalin/shadewright/internal/incidents"
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
	var namespace, watchNamespaces, clusterIDFlag string
	var maxIncidents int
	opts.BindFlags(flag.CommandLine)
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"The operator namespace: where Incidents and RemediationRequests are written. "+
			"Defaults to the POD_NAMESPACE environment variable.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated namespaces whose events, pods and workloads the analyzer reads. Required; each one "+
			"needs a RoleBinding to the analyzer-watch-role (config/watch).")
	flag.StringVar(&clusterIDFlag, "cluster-id", "",
		"A name for this cluster, hashed into every fingerprint so Incidents from two clusters never merge. "+
			"Required: 1 to 63 letters, digits, '.', '_' or '-', starting and ending with a letter or digit. "+
			"Changing it starts every Incident afresh.")
	flag.IntVar(&maxIncidents, "max-incidents", incidents.DefaultMaxIncidents,
		"The most Incidents kept in the operator namespace. At the cap a new fingerprint is not stored: it is "+
			"counted in shadewright_incident_observations_refused_total and reported in a Warning event.")
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
	setupLog.Info("Watching namespaces", "namespaces", watched, "operatorNamespace", namespace,
		"clusterID", clusterID, "maxIncidents", maxIncidents)

	mgrOpts := opts.ManagerOptions(setupLog, scheme, cmdutil.AnalyzerLease)
	mgrOpts.Cache = analyzer.CacheOptions(namespace, watched)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// The Incident controller consumes the collector's observations and tells it what each occurrence has
	// already counted. Its Warning events are about this Pod (POD_NAME, from the downward API); without a
	// Pod name (make run) the refusals are only logged and counted. It also marks the Incidents due for
	// analysis (condition AnalysisDue), with the default debounce, cooldown and analysis timeout, and the
	// daily budget kept in the status of RemediationSettings default.
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
	col, err := collector.New(collector.Config{ClusterID: clusterID, Handled: inc.Handled})
	if err != nil {
		setupLog.Error(err, "Failed to create the collector")
		os.Exit(1)
	}
	if err := col.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to set up the collector")
		os.Exit(1)
	}
	if err := inc.SetupWithManager(mgr, col.Observations()); err != nil {
		setupLog.Error(err, "Failed to set up the Incident controller")
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
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run analyzer")
		os.Exit(1)
	}
}
