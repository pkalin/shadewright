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
// factory that builds a Microsoft Foundry provider for a deployment of RemediationSettings only after its
// endpoint passed the allow-list. It is apart from package analyzer so that the collector's tests, which import
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

// ModelsConfig configures [NewModelFactory].
type ModelsConfig struct {
	// Suffixes is the allow-list of endpoint host suffixes ([ParseEndpointSuffixes]). Required.
	Suffixes []string
	// Tokens is the workload identity token source. Required.
	Tokens foundry.TokenSource
	// Log gets one line per refused endpoint, without the endpoint.
	Log logr.Logger
}

// NewModelFactory returns the analysis's [analysis.ModelFactory]: it checks a deployment's endpoint against
// the allow-list ([CheckEndpoint]) before anything else, so a refused endpoint gets no request and no token,
// then builds a Foundry provider bound to that endpoint, deployment and API version, with the workload
// identity token source (Cognitive Services audience only; the adapter refuses private addresses at dial
// time). It makes no network call.
func NewModelFactory(cfg ModelsConfig) (analysis.ModelFactory, error) {
	if len(cfg.Suffixes) == 0 || cfg.Tokens == nil {
		return nil, errors.New("the model factory needs an endpoint allow-list and a token source")
	}
	return func(role analysis.Role, d remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		if err := CheckEndpoint(d.Endpoint, cfg.Suffixes); err != nil {
			cfg.Log.Info("Refused a model endpoint from RemediationSettings", "role", role,
				"deployment", d.Deployment, "reason", err.Error())
			return nil, err
		}
		return foundry.New(foundry.Config{Endpoint: d.Endpoint, Deployment: d.Deployment, APIVersion: d.APIVersion,
			Tokens: cfg.Tokens})
	}, nil
}
