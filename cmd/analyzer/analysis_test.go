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
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/providers"
	providersfake "github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/providers/model/foundry"
	"github.com/pkalin/shadewright/internal/providers/store/azureblob"
	"github.com/pkalin/shadewright/internal/redact"
)

type noBuilder struct{}

func (noBuilder) Build(context.Context, *remediationv1alpha1.Incident) (*incidentcontext.Context, error) {
	return nil, errors.New("not in this test")
}

type noTokens struct{}

func (noTokens) Token(context.Context) (string, error) { return "", errors.New("not in this test") }

func testDeps(t *testing.T) analysisDeps {
	t.Helper()
	s := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	return analysisDeps{cache: c, apiReader: c, client: c, builder: noBuilder{}, sources: analyzer.Sources{},
		redactor: r, log: logr.Discard()}
}

// operatorNamespace is the namespace the analyzer creates requests in.
const operatorNamespace = "shadewright-system"

func validFlags() analysisFlags {
	return analysisFlags{namespace: operatorNamespace, watched: []string{"shop"},
		triageModel:      "gpt-4.1-mini-2025-04-14",
		endpointSuffixes: ".lab.services.ai.azure.com", actionPolicies: "SetContainerMemoryLimit=memory",
		claimTimeout: incidents.DefaultAnalysisTimeout}
}

// Without --triage-model the analysis is off: no controller, and no workload identity is read.
func TestAnalysisIsOffWithoutATriageModel(t *testing.T) {
	asked := false
	tokens := func() (foundry.TokenSource, error) { asked = true; return noTokens{}, nil }
	f := validFlags()
	f.triageModel = ""
	c, err := buildAnalysis(f, testDeps(t), tokens)
	if err != nil || c != nil || asked {
		t.Errorf("controller %v, error %v, identity read %v", c, err, asked)
	}
	f.escalationModel = "gpt-4.1-2025-04-14"
	if _, err := buildAnalysis(f, testDeps(t), tokens); err == nil {
		t.Error("--escalation-model without --triage-model was accepted")
	}
}

func TestAnalysisIsOnWithATriageModel(t *testing.T) {
	c, err := buildAnalysis(validFlags(), testDeps(t), func() (foundry.TokenSource, error) { return noTokens{}, nil })
	if err != nil || c == nil || c.Reader == nil {
		t.Fatalf("controller %v, error %v", c, err)
	}
}

// A wrong value stops the start instead of failing each analysis.
func TestAnalysisFlagsAreCheckedAtStart(t *testing.T) {
	ok := func() (foundry.TokenSource, error) { return noTokens{}, nil }
	for name, mutate := range map[string]func(*analysisFlags){
		"triage model":     func(f *analysisFlags) { f.triageModel = "gpt 4" },
		"escalation model": func(f *analysisFlags) { f.escalationModel = "-x" },
		"suffixes":         func(f *analysisFlags) { f.endpointSuffixes = ".com" },
		"empty suffixes":   func(f *analysisFlags) { f.endpointSuffixes = "" },
		"policies":         func(f *analysisFlags) { f.actionPolicies = "Exec=all" },
	} {
		f := validFlags()
		mutate(&f)
		if _, err := buildAnalysis(f, testDeps(t), ok); err == nil {
			t.Errorf("%s: a wrong value was accepted", name)
		}
	}
	missing := func() (foundry.TokenSource, error) { return nil, errors.New("AZURE_CLIENT_ID is not set") }
	if _, err := buildAnalysis(validFlags(), testDeps(t), missing); err == nil {
		t.Error("a missing workload identity was accepted")
	}
}

// --triage-model alone does not start the analysis: no model endpoint host is allowed by default, so the
// operator names its own resource in --model-endpoint-suffixes.
func TestAnalysisNeedsExplicitEndpointSuffixes(t *testing.T) {
	fs := flag.NewFlagSet("analyzer", flag.ContinueOnError)
	var f analysisFlags
	f.bind(fs)
	if err := fs.Parse([]string{"--triage-model=gpt-4.1-mini-2025-04-14",
		"--action-policies=SetContainerMemoryLimit=memory"}); err != nil {
		t.Fatal(err)
	}
	f.namespace, f.watched, f.claimTimeout = "shadewright-system", []string{"shop"}, incidents.DefaultAnalysisTimeout
	ok := func() (foundry.TokenSource, error) { return noTokens{}, nil }
	if _, err := buildAnalysis(f, testDeps(t), ok); err == nil {
		t.Error("the analysis started without --model-endpoint-suffixes")
	}
	f.endpointSuffixes = ".lab.services.ai.azure.com"
	if _, err := buildAnalysis(f, testDeps(t), ok); err != nil {
		t.Errorf("with a suffix: %v", err)
	}
}

// The claim lasts as long as the trigger's analysis timeout, so the time budget is checked against that value.
func TestAnalysisClaimFollowsTheTriggersTimeout(t *testing.T) {
	ok := func() (foundry.TokenSource, error) { return noTokens{}, nil }
	f := validFlags()
	f.claimTimeout = 8 * time.Minute
	if _, err := buildAnalysis(f, testDeps(t), ok); err == nil {
		t.Error("a claim shorter than the analysis deadline was accepted")
	}
	f.claimTimeout = 0
	if _, err := buildAnalysis(f, testDeps(t), ok); err == nil {
		t.Error("a missing claim timeout was accepted")
	}
	f.claimTimeout = 12 * time.Minute
	c, err := buildAnalysis(f, testDeps(t), ok)
	if err != nil || c.ClaimTimeout != 12*time.Minute {
		t.Errorf("controller %v, error %v, want a claim of 12m", c, err)
	}
}

// Settings of the analysis record store in the tests.
const (
	storeAccount   = "stswanabc"
	storeContainer = "analyses"
)

// The analysis record store is configured by its two flags together, only with the analysis on, and its
// settings are validated before anything starts.
func TestAnalysisStoreFlags(t *testing.T) {
	ok := func() (foundry.TokenSource, error) { return noTokens{}, nil }
	for name, edit := range map[string]func(*analysisFlags){
		"without a triage model": func(f *analysisFlags) {
			f.triageModel, f.storeAccount, f.storeContainer = "", storeAccount, storeContainer
		},
		"account only":    func(f *analysisFlags) { f.storeAccount = storeAccount },
		"container only":  func(f *analysisFlags) { f.storeContainer = storeContainer },
		"invalid account": func(f *analysisFlags) { f.storeAccount, f.storeContainer = "st.evil.example", storeContainer },
		"invalid container": func(f *analysisFlags) {
			f.storeAccount, f.storeContainer = storeAccount, "../analyses"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := validFlags()
			edit(&f)
			if _, err := buildAnalysis(f, testDeps(t), ok); err == nil {
				t.Error("buildAnalysis accepted the store flags")
			}
		})
	}

	f := validFlags()
	c, err := buildAnalysis(f, testDeps(t), ok)
	if err != nil || c == nil || c.Store != nil {
		t.Fatalf("without the store flags: controller %v, error %v; want one without a store", c, err)
	}
	f.storeAccount, f.storeContainer = storeAccount, storeContainer
	d := testDeps(t)
	var got azureblob.Config
	d.newStore = func(cfg azureblob.Config) (providers.AnalysisStore, error) {
		got = cfg
		return &providersfake.Store{}, nil
	}
	c, err = buildAnalysis(f, d, ok)
	if err != nil || c.Store == nil || got != (azureblob.Config{Account: storeAccount, Container: storeContainer}) {
		t.Fatalf("with the store flags: config %+v, error %v; want a store", got, err)
	}
}

// The context builder reads the policies the analysis puts in its requests, by name through the API reader,
// for the operator namespace (#493); with no policy or no model it reads none.
func TestContextPoliciesFollowTheFlags(t *testing.T) {
	r := testDeps(t).apiReader
	var cfg incidentcontext.Config
	if err := contextPolicies(validFlags(), r, operatorNamespace, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PolicyReader != r || cfg.RequestNamespace != operatorNamespace ||
		cfg.Policies[remediationv1alpha1.ActionSetContainerMemoryLimit] != "memory" || len(cfg.Policies) != 1 {
		t.Errorf("config %+v", cfg)
	}
	for name, mutate := range map[string]func(*analysisFlags){
		"no policy": func(f *analysisFlags) { f.actionPolicies = "" },
		"no model":  func(f *analysisFlags) { f.triageModel = "" },
	} {
		f := validFlags()
		mutate(&f)
		var cfg incidentcontext.Config
		err := contextPolicies(f, r, operatorNamespace, &cfg)
		if err != nil || cfg.PolicyReader != nil || cfg.Policies != nil {
			t.Errorf("%s: config %+v, error %v", name, cfg, err)
		}
	}
	f := validFlags()
	f.actionPolicies = "Exec=all"
	if err := contextPolicies(f, r, operatorNamespace, &cfg); err == nil {
		t.Error("an invalid --action-policies was accepted")
	}
}

// forbidOne answers Forbidden for the policy named forbidden, as the API server does for a name the role's
// resourceNames leave out, and NotFound for every other.
type forbidOne struct {
	client.Reader
	forbidden string
}

func (f forbidOne) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	gr := schema.GroupResource{Group: remediationv1alpha1.GroupVersion.Group, Resource: "remediationpolicies"}
	if key.Name == f.forbidden {
		return apierrors.NewForbidden(gr, key.Name, errors.New("not in resourceNames"))
	}
	return apierrors.NewNotFound(gr, key.Name)
}

// A role that does not name every --action-policies policy stops the start; a policy that does not exist
// yet, or no policy reader at all, does not.
func TestCheckPolicyReadsRefusesARoleWithoutTheName(t *testing.T) {
	cfg := incidentcontext.Config{PolicyReader: forbidOne{forbidden: "keda"},
		Policies: map[remediationv1alpha1.ActionType]remediationv1alpha1.Name{
			remediationv1alpha1.ActionSetContainerMemoryLimit: "memory",
			remediationv1alpha1.ActionAdjustKedaMaxReplicas:   "keda",
		}}
	if err := checkPolicyReads(cfg); err == nil || !strings.Contains(err.Error(), `"keda"`) {
		t.Errorf("a forbidden policy read: %v", err)
	}
	cfg.PolicyReader = forbidOne{forbidden: "other"}
	if err := checkPolicyReads(cfg); err != nil {
		t.Errorf("missing policies: %v", err)
	}
	if err := checkPolicyReads(incidentcontext.Config{}); err != nil {
		t.Errorf("no policy reader: %v", err)
	}
}
