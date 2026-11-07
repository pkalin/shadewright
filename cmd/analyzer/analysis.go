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
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/analyzer/models"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/providers"
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
}

// bind registers the analysis flags on fs.
func (f *analysisFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.triageModel, "triage-model", "",
		"The model ID and version the triage deployment of RemediationSettings serves, as its replies name it, "+
			"such as gpt-4.1-mini-2025-04-14. A reply that names another model is refused (ADR-0023). Empty (the "+
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
// (--triage-model empty: nothing is proposed), it returns no reader and the section is "not available".
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
	// The read tools of an escalation, on the same sources and redactor as the context builder, bound to the
	// watched namespaces. get_delivery_status has no delivery system (the analyzer may read Flux objects
	// only for the Flux module's symptom sources, behind a chart value), and the loop does not offer it.
	set, err := tools.New(tools.Config{Pods: d.cache, Events: d.cache, Objects: d.apiReader, Logs: d.sources.Logs,
		Metrics: d.sources.Metrics, Redactor: d.redactor, Namespaces: f.watched})
	if err != nil {
		return nil, fmt.Errorf("the read tools: %w", err)
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

// setupAnalysis builds the analysis controller and registers it with the manager, or logs that the analysis
// is off.
func setupAnalysis(mgr ctrl.Manager, f analysisFlags, builder analysis.ContextBuilder, sources analyzer.Sources,
	redactor *redact.Redactor, tp trace.TracerProvider) error {
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
