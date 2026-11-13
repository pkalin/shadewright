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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/analyzer/models"
	"github.com/pkalin/shadewright/internal/analyzer/revert"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/providers"
	fluxdelivery "github.com/pkalin/shadewright/internal/providers/delivery/flux"
	"github.com/pkalin/shadewright/internal/providers/model/foundry"
	"github.com/pkalin/shadewright/internal/providers/store/azureblob"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// analysisFlags are the command line flags of the analysis.
type analysisFlags struct {
	namespace                    string
	watched                      []string
	triageModel, escalationModel string
	endpointSuffixes             string
	actionPolicies               string
	// storeAccount and storeContainer name the Blob Storage container of the analysis records (ADR-0015).
	storeAccount, storeContainer string
	// claimTimeout is the Incident trigger's AnalysisTimeout: how long a claim lasts (ADR-0025 decision 1).
	claimTimeout time.Duration
	// moduleGrants are the grants of --module-grants, nil when the flag is not given. get_delivery_status gets a
	// delivery system (#562) when they hold the Flux module's grant by the collector's rule (collector.Granted),
	// so the tool and the collector's Flux sources turn on together.
	moduleGrants []string
}

// bind registers the analysis flags on fs.
func (f *analysisFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.triageModel, "triage-model", "",
		"The model ID and version the triage deployment of RemediationSettings serves, as its replies name it, "+
			"such as gpt-5.4-mini-2026-03-17. A reply that names another model is refused (ADR-0023). Empty (the "+
			"default) starts no analysis: no model is called, no Azure token is requested, and the analyzer behaves "+
			"as without a model. Set, the analyzer needs its Azure workload identity (AZURE_CLIENT_ID, "+
			"AZURE_TENANT_ID, AZURE_FEDERATED_TOKEN_FILE) or it refuses to start.")
	fs.StringVar(&f.escalationModel, "escalation-model", "",
		"The model ID and version the escalation deployment serves, as its replies name it. Empty (the default): "+
			"escalation is not configured, whatever RemediationSettings names, and an escalation signal hands the "+
			"incident off. Used only with --triage-model.")
	fs.StringVar(&f.endpointSuffixes, "model-endpoint-suffixes", "",
		"Comma-separated host suffixes a model endpoint in RemediationSettings must end with or, without the "+
			"leading dot, equal, each a dot and two or more DNS labels, such as .<resource>.services.ai.azure.com "+
			"for one resource. Required with --triage-model: there is no default, so no Azure AI host is allowed "+
			"unless named. An endpoint outside them, or on a port other than 443, is refused before any token is "+
			"requested (EndpointNotAllowed).")
	fs.StringVar(&f.actionPolicies, "action-policies", "",
		"Comma-separated ActionType=policy pairs: the RemediationPolicy a proposed RemediationRequest of each "+
			"action type refers to, such as SetContainerMemoryLimit=memory-limits. An action type without one is "+
			"never proposed (NoPolicyForAction). Used only with --triage-model.")
	fs.StringVar(&f.storeAccount, "analysis-store-account", "",
		"The Azure storage account name (3 to 24 lower-case letters and digits) of the Blob Storage container that "+
			"keeps one redacted record of each analysis (ADR-0015), written create-only with the analyzer's workload "+
			"identity (Storage audience, no key or SAS) to https://<account>.blob.core.windows.net. Empty (the "+
			"default) writes no record. Used only with --triage-model, and needs --analysis-store-container.")
	fs.StringVar(&f.storeContainer, "analysis-store-container", "",
		"The container of the analysis records in --analysis-store-account, such as analyses.")
}

// analysisDeps are what the analysis reads and writes with: the manager's cache and API reader, its client,
// the context builder and the sources the read tools share with it.
type analysisDeps struct {
	cache, apiReader client.Reader
	client           client.Client
	builder          analysis.ContextBuilder
	sources          analyzer.Sources
	redactor         *redact.Redactor
	log              logr.Logger
	// tracerProvider makes the analysis spans; nil means no tracing.
	tracerProvider trace.TracerProvider
	// newStore builds the analysis record store; nil means the Blob Storage adapter with the workload identity.
	newStore func(cfg azureblob.Config) (providers.AnalysisStore, error)
}

// contextPolicies configures the context builder's policy bounds section (#493): the policies of
// --action-policies, read by name through reader (the API reader: the analyzer may get those policies, not
// list or watch them), for requests in the operator namespace. With no policy configured, or no model
// (--triage-model empty: nothing is proposed), it returns no reader and the section is "not available". It
// refuses a policy the analyzer may not get (checkPolicyReads).
func contextPolicies(f analysisFlags, reader client.Reader, namespace string, cfg *incidentcontext.Config) error {
	if f.triageModel == "" {
		return nil
	}
	policies, err := models.ParseActionPolicies(f.actionPolicies)
	if err != nil {
		return fmt.Errorf("invalid --action-policies: %w", err)
	}
	if len(policies) == 0 {
		return nil
	}
	cfg.Policies, cfg.PolicyReader, cfg.RequestNamespace = policies, reader, namespace
	return checkPolicyReads(*cfg)
}

// policyCheckTimeout bounds the start's check of the policy reads.
const policyCheckTimeout = 30 * time.Second

// checkPolicyReads gets each policy cfg names once, before anything starts. A Forbidden answer means the
// analyzer's policy reader role does not name that policy: the chart renders it from the same values as the
// flag, but config/analysis-policies lists the names by hand, so a mismatch stops the start here instead of
// leaving every policy bounds block unread. A policy that does not exist yet is fine (the block says so), and
// any other error is left to the block, which reports it as a failed source.
func checkPolicyReads(cfg incidentcontext.Config) error {
	if cfg.PolicyReader == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), policyCheckTimeout)
	defer cancel()
	names := make([]string, 0, len(cfg.Policies))
	for _, n := range cfg.Policies {
		if !slices.Contains(names, string(n)) {
			names = append(names, string(n))
		}
	}
	slices.Sort(names)
	for _, n := range names {
		err := cfg.PolicyReader.Get(ctx, client.ObjectKey{Name: n}, &remediationv1alpha1.RemediationPolicy{})
		if apierrors.IsForbidden(err) {
			return fmt.Errorf("the analyzer may not get RemediationPolicy %q named in --action-policies: the "+
				"analyzer-policy-reader ClusterRole must list every policy of the flag in resourceNames "+
				"(chart value analyzer.analysis.actionPolicies, or config/analysis-policies)", n)
		}
	}
	return nil
}

// buildAnalysis returns the analysis controller, or nil when --triage-model is empty: then no model is
// called, no token source is created and the analyzer runs as without a model. Set, every analysis flag is
// checked and the workload identity read (newTokens) before anything starts, so a wrong value stops the
// start instead of failing each analysis.
func buildAnalysis(f analysisFlags, d analysisDeps,
	newTokens func() (foundry.TokenSource, error)) (*analysis.Controller, error) {
	if f.triageModel == "" {
		if f.escalationModel != "" {
			return nil, errors.New("--escalation-model needs --triage-model")
		}
		if f.storeAccount != "" || f.storeContainer != "" {
			return nil, errors.New("--analysis-store-account and --analysis-store-container need --triage-model")
		}
		return nil, nil
	}
	if (f.storeAccount == "") != (f.storeContainer == "") {
		return nil, errors.New("--analysis-store-account and --analysis-store-container are set together or not at all")
	}
	triage, err := models.ParseModelName(f.triageModel)
	if err != nil {
		return nil, fmt.Errorf("invalid --triage-model: %w", err)
	}
	escalation, err := models.ParseModelName(f.escalationModel)
	if err != nil {
		return nil, fmt.Errorf("invalid --escalation-model: %w", err)
	}
	if f.endpointSuffixes == "" {
		return nil, errors.New("--triage-model needs --model-endpoint-suffixes: no model endpoint host is allowed by default")
	}
	if f.claimTimeout <= 0 {
		return nil, errors.New("the analysis needs the Incident trigger's analysis timeout")
	}
	suffixes, err := models.ParseEndpointSuffixes(f.endpointSuffixes)
	if err != nil {
		return nil, fmt.Errorf("invalid --model-endpoint-suffixes: %w", err)
	}
	policies, err := models.ParseActionPolicies(f.actionPolicies)
	if err != nil {
		return nil, fmt.Errorf("invalid --action-policies: %w", err)
	}
	tokens, err := newTokens()
	if err != nil {
		return nil, fmt.Errorf("the analyzer's Azure workload identity: %w", err)
	}
	factory, err := models.NewModelFactory(models.ModelsConfig{Suffixes: suffixes, Tokens: tokens,
		Log: d.log.WithName("models")})
	if err != nil {
		return nil, err
	}
	set, err := readTools(f, d)
	if err != nil {
		return nil, err
	}
	expected := map[analysis.Role]string{analysis.RoleTriage: triage}
	if escalation != "" {
		expected[analysis.RoleEscalation] = escalation
	}
	a, err := analysis.New(analysis.Config{
		Builder: d.builder, Tools: set, Models: factory, Policies: policies, ExpectedModels: expected,
		Redactor: d.redactor, TracerProvider: d.tracerProvider,
		Reserve: func(ctx context.Context, now time.Time) error {
			return incidents.ReserveAnalysis(ctx, d.apiReader, d.client, f.namespace, now)
		},
	})
	if err != nil {
		return nil, err
	}
	c := &analysis.Controller{Client: d.client, Reader: d.apiReader, Namespace: f.namespace, Analyzer: a,
		ClaimTimeout: f.claimTimeout}
	if f.storeAccount != "" {
		newStore := d.newStore
		if newStore == nil {
			newStore = func(cfg azureblob.Config) (providers.AnalysisStore, error) {
				// The analyzer's redactor runs the store's last check, so operator patterns count there too.
				return azureblob.New(cfg, azureblob.Options{Redactor: d.redactor})
			}
		}
		store, err := newStore(azureblob.Config{Account: f.storeAccount, Container: f.storeContainer})
		if err != nil {
			return nil, fmt.Errorf("the analysis record store: %w", err)
		}
		c.Store = store
	}
	if err := c.CheckTimeBudget(); err != nil {
		return nil, err
	}
	return c, nil
}

// readTools returns the read tools of an escalation, on the same sources and redactor as the context builder,
// bound to the watched namespaces. get_delivery_status gets the Flux adapter's status reader (#562) by the
// collector's rule for the Flux module's grant (collector.Granted): when --module-grants names it, or is not
// given. It then reads one Kustomization or HelmRelease of a watched namespace, and the GitRepository of a
// Kustomization, with the list that grant holds (analyzer.ListGetter on the API reader), no get and no
// informer, so the analyzer gets no new permission; where the grant is not bound after all, the list is
// refused and the tool answers "could not fetch". Its delivery namespaces are the watched ones, where that
// grant is bound. The loop does not offer it yet.
func readTools(f analysisFlags, d analysisDeps) (*tools.Set, error) {
	cfg := tools.Config{Pods: d.cache, Events: d.cache, Objects: d.apiReader, Logs: d.sources.Logs,
		Metrics: d.sources.Metrics, Redactor: d.redactor, Namespaces: f.watched}
	if collector.Granted(f.moduleGrants, collector.GrantFlux) && len(f.watched) > 0 {
		delivery, err := fluxdelivery.NewStatusReader(analyzer.ListGetter{Reader: d.apiReader}, d.redactor)
		if err != nil {
			return nil, fmt.Errorf("the delivery status reader: %w", err)
		}
		cfg.Delivery, cfg.DeliveryNamespaces = delivery, f.watched
	}
	set, err := tools.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("the read tools: %w", err)
	}
	return set, nil
}

// setupAnalysis registers the revert proposer, then builds the analysis controller and registers it with the
// manager, or logs that the analysis is off. The revert proposer (ADR-0030 section 8, #96) is deterministic and
// calls no model, so it runs whether the analysis is on or off: it creates the revert of a request that ended
// Failed with Verified False and records it in the Incident's status.revertsProposed, with the grants the
// analyzer already holds. The trigger (inc) then counts the reviewer hints of the requests the analysis answers them on
// (ADR-0028), selected by the analysis's own predicate; with the analysis off it counts none.
func setupAnalysis(mgr ctrl.Manager, inc *incidents.Controller, f analysisFlags, builder analysis.ContextBuilder,
	sources analyzer.Sources, redactor *redact.Redactor, tp trace.TracerProvider) error {
	if err := (&revert.Proposer{Client: mgr.GetClient(), Namespace: f.namespace}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("the revert proposer: %w", err)
	}
	c, err := buildAnalysis(f, analysisDeps{
		cache: mgr.GetCache(), apiReader: mgr.GetAPIReader(), client: mgr.GetClient(), builder: builder,
		sources: sources, redactor: redactor, log: ctrl.Log.WithName("analysis"), tracerProvider: tp,
	}, func() (foundry.TokenSource, error) { return foundry.NewWorkloadIdentity() })
	if err != nil {
		return err
	}
	if c == nil {
		setupLog.Info("The analysis is off: --triage-model is not set, so no model is called and Incidents are not analyzed")
		return nil
	}
	inc.SetAnalysisRequests(c.IsAnalysisRequest)
	if c.Store != nil {
		// The Warning event of a record not written is about the Incident, in the operator namespace, with the
		// grant on core Events there that the leader election Role holds (see the Incident controller's
		// recorder in main.go).
		c.Recorder = mgr.GetEventRecorderFor("shadewright-analyzer") //nolint:staticcheck // as in main.go
	}
	setupLog.Info("The analysis is on", "triageModel", f.triageModel, "escalationModel", f.escalationModel,
		"endpointSuffixes", f.endpointSuffixes, "actionPolicies", f.actionPolicies,
		"analysisStoreAccount", f.storeAccount, "analysisStoreContainer", f.storeContainer)
	return c.SetupWithManager(mgr)
}
