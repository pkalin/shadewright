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

// Package models wires the analysis to its model provider (design 11.1, #76): the operator's allow-list of
// model endpoint host suffixes, the model names the replies must give, the policy per action type, and the
// factory that builds the provider of a deployment of RemediationSettings by its provider field: Microsoft
// Foundry only after its endpoint passed the allow-list, the Anthropic API only at its fixed host and with the
// analyzer's key file (ADR-0037). It is apart from package analyzer so that the collector's tests, which import
// that package, do not import the analysis.
package models

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/validation"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/model/anthropic"
	"github.com/pkalin/shadewright/internal/providers/model/foundry"
)

// suffixLabel is one DNS label of a host suffix.
var suffixLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ParseEndpointSuffixes parses the comma-separated allow-list of model endpoint host suffixes. Each starts
// with a dot and has at least two DNS labels of lower-case letters, digits and '-', so a suffix such as
// ".com" or "azure.com" (no dot: it would also match "evilazure.com") is refused. The list may not be empty.
func ParseEndpointSuffixes(flag string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(flag, ",") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		labels := strings.Split(strings.TrimPrefix(s, "."), ".")
		if !strings.HasPrefix(s, ".") || len(labels) < 2 || len(s) > 253 ||
			slices.ContainsFunc(labels, func(l string) bool { return !suffixLabel.MatchString(l) }) {
			return nil, fmt.Errorf("model endpoint suffix %q is not a dot followed by two or more lower-case DNS labels", s)
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the model endpoint suffix list is empty")
	}
	return out, nil
}

// CheckEndpoint reports whether a model endpoint from RemediationSettings may be called: an HTTPS URL
// (parsed with url.Parse) with no user info, query or fragment, on port 443 (none given, or 443, the only
// port the chart's NetworkPolicy opens for it), whose host is one of suffixes without its leading dot or
// ends with one, so ".lab.services.ai.azure.com" allows that one resource only.
// The error wraps [analysis.ErrEndpointNotAllowed] and never repeats the endpoint.
func CheckEndpoint(endpoint string, suffixes []string) error {
	refuse := func(why string) error {
		return fmt.Errorf("model endpoint %s: %w", why, analysis.ErrEndpointNotAllowed)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Opaque != "" || u.Host == "" {
		return refuse("is not an HTTPS URL without user info, query or fragment")
	}
	if p := u.Port(); p != "" && p != "443" {
		return refuse("names a port other than 443")
	}
	host := u.Hostname()
	if host != strings.ToLower(host) || strings.HasSuffix(host, ".") {
		return refuse("host is not in lower case without a trailing dot")
	}
	for _, s := range suffixes {
		if host == s[1:] || strings.HasSuffix(host, s) {
			return nil
		}
	}
	return refuse("host is not in the operator's allow-list of suffixes")
}

// modelName is the form of an expected model ID: the form the Foundry adapter accepts in a reply.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// ParseModelName checks the model ID a role's replies must name (--triage-model, --escalation-model), such
// as gpt-5.4-mini-2026-03-17. Empty is allowed: the role is not configured.
func ParseModelName(name string) (string, error) {
	if name != "" && !modelName.MatchString(name) {
		return "", errors.New("a model name is 1 to 128 letters, digits, '.', '_', ':', '/' or '-', starting with a letter or digit")
	}
	return name, nil
}

// ParseActionPolicies parses --action-policies: comma-separated ActionType=policy pairs naming the
// RemediationPolicy a proposed request of each action type refers to. Each type must be in the closed catalog
// and appear once, and each policy name must be a DNS subdomain name. Empty gives an empty map: no action
// type has a policy, so the analysis proposes nothing and hands every result off (NoPolicyForAction).
func ParseActionPolicies(flag string) (map[remediationv1alpha1.ActionType]remediationv1alpha1.Name, error) {
	out := map[remediationv1alpha1.ActionType]remediationv1alpha1.Name{}
	for part := range strings.SplitSeq(flag, ",") {
		pair := strings.TrimSpace(part)
		if pair == "" {
			continue
		}
		typ, name, ok := strings.Cut(pair, "=")
		at := remediationv1alpha1.ActionType(strings.TrimSpace(typ))
		name = strings.TrimSpace(name)
		if !ok || !slices.Contains(remediationv1alpha1.ActionTypes(), at) {
			return nil, fmt.Errorf("action policy %q does not start with an action type of the catalog and '='", pair)
		}
		if _, dup := out[at]; dup {
			return nil, fmt.Errorf("action type %s has more than one policy", at)
		}
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			return nil, fmt.Errorf("the policy of action type %s is not a valid name: %s", at, strings.Join(errs, "; "))
		}
		out[at] = remediationv1alpha1.Name(name)
	}
	return out, nil
}

// anthropicHost is the host of the Anthropic API. The Foundry path refuses it whatever the allow-list says, so
// the workload identity token never goes there (ADR-0037 decision 2).
const anthropicHost = "api.anthropic.com"

// ModelsConfig configures [NewModelFactory].
type ModelsConfig struct {
	// Suffixes is the allow-list of endpoint host suffixes ([ParseEndpointSuffixes]) of Foundry deployments.
	// Set together with Tokens or not at all. Empty refuses every Foundry deployment
	// (analysis.ErrProviderNotConfigured): an analyzer that only calls the Anthropic API needs no Azure
	// settings (#814).
	Suffixes []string
	// Tokens is the workload identity token source of Foundry deployments. Set together with Suffixes.
	Tokens foundry.TokenSource
	// AnthropicKeyFile is the path of the file that holds the Anthropic API key (--anthropic-api-key-file).
	// Empty refuses every Anthropic deployment (analysis.ErrProviderNotConfigured).
	AnthropicKeyFile string
	// Expected names, per role, the model a reply must name (--triage-model, --escalation-model). The
	// deployment of an Anthropic deployment is the model ID, so it must equal its role's entry.
	Expected map[analysis.Role]string
	// Log gets one line per refused deployment, including one the adapter refuses to build, and one per
	// failed call of an Anthropic provider with the adapter's fixed error text; never the endpoint, the key
	// or the key file.
	Log logr.Logger
}

// NewModelFactory returns the analysis's [analysis.ModelFactory]. It builds the provider a deployment's
// provider field names and makes no network call:
//   - Foundry (the default): it refuses the deployment when the analyzer has no allow-list and token source
//     (analysis.ErrProviderNotConfigured, naming --model-endpoint-suffixes), and the Anthropic API's host
//     whatever the allow-list says, then checks the endpoint against the allow-list ([CheckEndpoint]), so a
//     refused endpoint gets no request and no token, then builds a Foundry provider bound to that endpoint,
//     deployment, API version and inference API (chat completions, or the Responses API with inferenceAPI
//     Responses, #501), with the workload
//     identity token source (Cognitive Services audience only; the adapter refuses private addresses at dial
//     time).
//   - Anthropic: it refuses the deployment when the analyzer has no key file
//     (analysis.ErrProviderNotConfigured), when the endpoint is not the API's fixed one, and when the
//     deployment is not the model its role expects (analysis.ErrDeploymentNotExpected); the allow-list is not
//     consulted. It then builds an Anthropic provider for that model that reads the key file at every attempt
//     and logs every failed call.
//   - Any other provider is refused (analysis.ErrProviderNotConfigured).
func NewModelFactory(cfg ModelsConfig) (analysis.ModelFactory, error) {
	foundryOn := len(cfg.Suffixes) > 0
	if foundryOn != (cfg.Tokens != nil) {
		return nil, errors.New("the model factory needs an endpoint allow-list and a token source together, or neither")
	}
	if !foundryOn && cfg.AnthropicKeyFile == "" {
		return nil, errors.New("the model factory needs an endpoint allow-list and a token source, or an Anthropic key file")
	}
	refused := func(role analysis.Role, d remediationv1alpha1.ModelDeployment, err error) error {
		cfg.Log.Info("Refused a model deployment from RemediationSettings", "role", role,
			"provider", d.ProviderName(), "deployment", d.Deployment, "reason", err.Error())
		return err
	}
	return func(role analysis.Role, d remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		switch d.ProviderName() {
		case remediationv1alpha1.ModelProviderFoundry:
			if !foundryOn {
				return nil, refused(role, d, fmt.Errorf("the analyzer runs without --model-endpoint-suffixes and its "+
					"Azure workload identity: %w", analysis.ErrProviderNotConfigured))
			}
			if u, err := url.Parse(d.Endpoint); err == nil && strings.TrimSuffix(strings.ToLower(u.Hostname()), ".") == anthropicHost {
				return nil, refused(role, d, fmt.Errorf("model endpoint host is the Anthropic API's, not a Foundry "+
					"resource: %w", analysis.ErrEndpointNotAllowed))
			}
			if err := CheckEndpoint(d.Endpoint, cfg.Suffixes); err != nil {
				return nil, refused(role, d, err)
			}
			p, err := foundry.New(foundry.Config{Endpoint: d.Endpoint, Deployment: d.Deployment,
				APIVersion: d.APIVersion, API: foundry.API(d.InferenceAPI), Tokens: cfg.Tokens})
			if err != nil {
				return nil, refused(role, d, err)
			}
			return p, nil
		case remediationv1alpha1.ModelProviderAnthropic:
			if cfg.AnthropicKeyFile == "" {
				return nil, refused(role, d, fmt.Errorf("the analyzer runs without --anthropic-api-key-file: %w",
					analysis.ErrProviderNotConfigured))
			}
			if d.Endpoint != anthropic.Endpoint {
				return nil, refused(role, d, fmt.Errorf("model endpoint is not the Anthropic API's fixed endpoint, "+
					"the only one this provider calls: %w", analysis.ErrEndpointNotAllowed))
			}
			if want := cfg.Expected[role]; want == "" || d.Deployment != want {
				return nil, refused(role, d, fmt.Errorf("the deployment is not the model of the role's flag: %w",
					analysis.ErrDeploymentNotExpected))
			}
			log := cfg.Log.WithValues("role", role, "provider", d.ProviderName(), "deployment", d.Deployment)
			p, err := anthropic.New(anthropic.Config{Model: d.Deployment, KeyFile: cfg.AnthropicKeyFile,
				// The adapter's errors are fixed text that never holds the key, the key file's path or the
				// reply; logging them tells a key file that cannot be used from an outage of the API.
				OnCall: func(r anthropic.CallReport) {
					if r.Err != nil {
						log.Info("A call to the Anthropic API failed", "attempts", r.Attempts,
							"notSent", providers.IsNotSent(r.Err), "error", r.Err.Error())
					}
				}})
			if err != nil {
				return nil, refused(role, d, err)
			}
			return p, nil
		default:
			return nil, refused(role, d, fmt.Errorf("unknown model provider: %w", analysis.ErrProviderNotConfigured))
		}
	}, nil
}
