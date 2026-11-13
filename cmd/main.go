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

// Command manager is the executor: it reconciles RemediationRequests in its own namespace, decides with the
// policy and acts through Git. It never creates requests or writes incidents; the analyzer does that.
package main

import (
	"context"
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/cmdutil"
	"github.com/pkalin/shadewright/internal/controller"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fluxdelivery "github.com/pkalin/shadewright/internal/providers/delivery/flux"
	"github.com/pkalin/shadewright/internal/providers/registry"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(remediationv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// executorFlags are the executor's own flags that set fields of the RemediationRequest reconciler.
type executorFlags struct {
	pollInterval   time.Duration
	reviewerHints  bool
	nodeGrace      time.Duration
	verifyOutcomes bool
}

// bindExecutorFlags registers the executor's reconciler flags on fs.
func bindExecutorFlags(fs *flag.FlagSet) *executorFlags {
	f := &executorFlags{}
	fs.DurationVar(&f.pollInterval, "pull-request-poll-interval", controller.DefaultPullRequestPollInterval,
		"How often the executor looks at a pull request that waits for review, so a merge or a close is seen "+
			"within it. Between "+controller.ShortDuration(controller.MinPullRequestPollInterval)+" and "+
			controller.ShortDuration(controller.MaxPullRequestPollInterval)+" (0 uses the default); "+
			"the full recheck of the plan stays every "+controller.ShortDuration(controller.FullRecheckInterval)+".")
	fs.BoolVar(&f.reviewerHints, "reviewer-hints", false,
		"Read reviewer hints (/shadewright reanalyze:) from the comments of the pull requests the executor opened "+
			"(ADR-0028). Off by default. Turn it on only when the target branch of the GitOps repository requires "+
			"the check shadewright/review with the Shadewright App as its source: without that rule the merge "+
			"gate only informs and does not block.")
	fs.DurationVar(&f.nodeGrace, "node-grace", controller.DefaultNodeGrace,
		"How long past the stable window a verification verdict waits, so the pods of a node that stopped "+
			"reporting are marked not Ready first (ADR-0030). Set it to at least the cluster's node monitor grace "+
			"period (kube-controller-manager --node-monitor-grace-period). Part of the verification timeout; 0 uses "+
			"the default, a negative value refuses to start.")
	fs.BoolVar(&f.verifyOutcomes, "verify-outcomes", false,
		"Move merged requests through WaitingForDelivery and Verifying to Failed or Inconclusive (ADR-0030). Off "+
			"by default until the executor reads the verifier's report (#683): before that no request can end "+
			"Succeeded, so every merge would end Inconclusive or Failed and the circuit breaker would not close "+
			"again. Off, a merged request keeps Merged with its rollout report.")
	return f
}

// newReconciler builds the RemediationRequest reconciler with the fields the flags hold. main builds it only
// here, so a test of this function covers the wiring of the flags.
func (f *executorFlags) newReconciler(c client.Client, s *runtime.Scheme, namespace string, apiReader client.Reader,
	open freshness.RepositoryOpener, recorder record.EventRecorder) *controller.RemediationRequestReconciler {
	return &controller.RemediationRequestReconciler{
		Client:                  c,
		Scheme:                  s,
		Namespace:               namespace,
		APIReader:               apiReader,
		OpenRepository:          open,
		Recorder:                recorder,
		PullRequestPollInterval: f.pollInterval,
		ReviewerHints:           f.reviewerHints,
		NodeGrace:               f.nodeGrace,
		VerifyOutcomes:          f.verifyOutcomes,
	}
}

func main() {
	var opts cmdutil.Options
	var namespace string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	opts.BindFlags(flag.CommandLine)
	ef := bindExecutorFlags(flag.CommandLine)
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"The operator namespace: the only namespace whose RemediationRequests are reconciled. "+
			"Defaults to the POD_NAMESPACE environment variable.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
	flag.Parse()

	ctrl.SetLogger(cmdutil.NewLogger(&opts))

	if namespace == "" {
		setupLog.Error(nil, "The operator namespace is not set: use --namespace or POD_NAMESPACE")
		os.Exit(1)
	}

	webhookServerOptions := webhook.Options{
		TLSOpts: opts.TLSOpts(setupLog),
		Port:    webhookPort,
	}
	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)
		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	mgrOpts := opts.ManagerOptions(setupLog, scheme, cmdutil.ExecutorLease)
	mgrOpts.WebhookServer = webhook.NewServer(webhookServerOptions)
	mgrOpts.Cache = controller.CacheOptions(namespace)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// One registry for the source mapping and the freshness check, so the installation token cache is shared.
	// It builds a GitHub provider per settings entry on first use, signs through Key Vault with workload
	// identity and reaches neither Azure nor GitHub at startup. Entries that left the settings are dropped.
	repos := &registry.Registry{Current: func(ctx context.Context) ([]remediationv1alpha1.GitRepository, error) {
		settings := &remediationv1alpha1.RemediationSettings{}
		key := client.ObjectKey{Name: remediationv1alpha1.SettingsName}
		if err := mgr.GetAPIReader().Get(ctx, key, settings); err != nil {
			return nil, err
		}
		return settings.Spec.Repositories, nil
	}}

	// The core/v1 recorder writes with the grant on core Events that the executor's leader election Role
	// already holds in the operator namespace, where the requests are, as the analyzer's recorder does.
	recorder := mgr.GetEventRecorderFor("shadewright-executor") //nolint:staticcheck // see above
	reconciler := ef.newReconciler(mgr.GetClient(), mgr.GetScheme(), namespace, mgr.GetAPIReader(), repos.Open, recorder)
	// Logged once, so the setting of a running executor can be read from its log. The merge gate of ADR-0028
	// holds only with the repository rule this binary cannot verify, hence the reminder when hints are on.
	hintsLog := []any{"enabled", ef.reviewerHints}
	if ef.reviewerHints {
		hintsLog = append(hintsLog, "requiredCheck", "shadewright/review from the Shadewright App on the target branch")
	}
	setupLog.Info("Configured reviewer hints", hintsLog...)
	setupLog.Info("Configured outcome verification", "enabled", ef.verifyOutcomes)
	// After the merge the executor reads the Flux Kustomization that applies the target, with the get it holds
	// for the source mapping, and asks the plan's Git host (a read) whether a later applied commit holds the
	// merge commit; then it reports the target's rollout (#532).
	reconciler.Delivery = func(ancestry providers.AncestryChecker) (controller.DeliveryVerifier, error) {
		v, err := fluxdelivery.New(mgr.GetAPIReader(), ancestry)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	// The policy check and the freshness rechecks share one resolver. Its cache keeps file reads at a commit,
	// which never change, so a plan rechecked every five minutes does not scan the same tree again. It only
	// reads, but it opens its providers through the kill switch's gate too, like every provider of the executor.
	reconciler.Source = &sourcemap.Resolver{
		Client:         mgr.GetAPIReader(),
		OpenRepository: reconciler.GatedRepository,
		Cache:          sourcemap.NewReadCache(sourcemap.DefaultCacheEntries, sourcemap.DefaultCacheBytes),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "remediationrequest")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager", "namespace", namespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
