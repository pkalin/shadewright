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

// Command verifier is shadewright-verifier, the third identity (ADR-0030 section 7). For every RemediationRequest
// in Verifying it lists the pods of the target, proves their owners and writes the request's VerificationReport.
// It has no model access, holds no Git App credentials and never writes request status.
package main

import (
	"errors"
	"flag"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/cmdutil"
	"github.com/pkalin/shadewright/internal/verifier"
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
		"The operator namespace: the only namespace whose RemediationRequests are sampled and where the "+
			"VerificationReports are written. Defaults to the POD_NAMESPACE environment variable.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated namespaces whose pods the verifier may list (one RoleBinding of verifier-watch-role "+
			"each). A request whose target is in another namespace gets no report. Required: the verifier "+
			"does not start without one.")
	flag.Parse()

	ctrl.SetLogger(cmdutil.NewLogger(&opts))

	if namespace == "" {
		setupLog.Error(nil, "The operator namespace is not set: use --namespace or POD_NAMESPACE")
		os.Exit(1)
	}
	watched, err := parseWatched(watchNamespaces)
	if err != nil {
		setupLog.Error(err, "Invalid --watch-namespaces")
		os.Exit(1)
	}

	mgrOpts := opts.ManagerOptions(setupLog, scheme, cmdutil.VerifierLease)
	mgrOpts.Cache = verifier.CacheOptions(namespace)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	r := &verifier.Reconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Namespace: namespace,
		Watched:   watched,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "verifier")
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

	setupLog.Info("Starting manager", "namespace", namespace, "watchNamespaces", strings.Join(watched, ","))
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// errNoWatchedNamespace is returned for an empty --watch-namespaces: a verifier that may read no pod could
// write no report, so it does not start. The chart does not deploy it without a namespace.
var errNoWatchedNamespace = errors.New("--watch-namespaces is empty: the verifier needs at least one namespace " +
	"whose pods it may list, so it does not start without one")

// parseWatched parses --watch-namespaces and refuses an empty list.
func parseWatched(s string) ([]string, error) {
	watched, err := cmdutil.ParseNamespaces(s)
	if err != nil {
		return nil, err
	}
	if len(watched) == 0 {
		return nil, errNoWatchedNamespace
	}
	return watched, nil
}
