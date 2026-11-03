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

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/cmdutil"
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
	var namespace, watchNamespaces string
	opts.BindFlags(flag.CommandLine)
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"The operator namespace: where Incidents and RemediationRequests are written. "+
			"Defaults to the POD_NAMESPACE environment variable.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated namespaces whose events, pods and workloads the analyzer reads. Required; each one "+
			"needs a RoleBinding to the analyzer-watch-role (config/watch).")
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
	setupLog.Info("Watching namespaces", "namespaces", watched, "operatorNamespace", namespace)

	mgrOpts := opts.ManagerOptions(setupLog, scheme, cmdutil.AnalyzerLease)
	mgrOpts.Cache = analyzer.CacheOptions(namespace, watched)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// The collector (#41), the Incident controller (#42) and the analysis trigger (#43) are added here.

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
