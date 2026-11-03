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

package modules

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/groupversion"
)

// NamePattern is the form of a module name: a DNS label that starts with a letter, at most 63 characters, so
// it fits a settings list, a status field and a metric label as it is. It is the pattern of ModuleName in
// api/v1alpha1/remediationsettings_types.go; a test keeps the two equal.
const NamePattern = `^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`

var namePattern = regexp.MustCompile(NamePattern)

// ValidName reports whether name is a valid module name, on either axis.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// MaxPlatforms and MaxIntegrations are the most platform modules (besides generic) and integration modules one
// registry holds. They are the caps of spec.modules.platforms and spec.modules.integrations in
// RemediationSettings, so every compiled-in module can be enabled, and they bound status.integrations, which
// lists at most every enabled name and every detected (so compiled-in) one: 32 + 32 = 64. A test keeps them
// equal to the CRD.
const (
	MaxPlatforms    = 8
	MaxIntegrations = 32
)

// schemePattern is the form of a provider ID scheme (RFC 3986, lower case).
var schemePattern = regexp.MustCompile(`^[a-z][a-z0-9+.-]{0,62}$`)

// labelKeyPattern is a Kubernetes label key: an optional DNS subdomain prefix and a name.
var labelKeyPattern = regexp.MustCompile(
	`^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

// Registry holds the compiled-in modules. It is immutable once built by [New] and safe for concurrent use.
type Registry struct {
	platforms         []platformEntry // sorted by name
	integrations      []integrationEntry
	platformByName    map[string]platformEntry
	integrationByName map[string]integrationEntry
	// platformLabels holds every node label key a platform requires: a node without a provider ID and
	// without any of them is one no platform could claim.
	platformLabels map[string]bool
}

// platformEntry and integrationEntry keep the name, the detection and the hooks a module declared when the registry
// was built, so lookups and detection do not depend on a module returning the same values twice.
type platformEntry struct {
	name   string
	module Platform
	scheme string
	labels []string // sorted, without duplicates
	hooks  Hooks    // validated copy
}

type integrationEntry struct {
	name      string
	module    Integration
	detection IntegrationDetection
	hooks     Hooks // validated copy
}

// New builds a registry from the platform and integration modules compiled into the binary. The generic
// platform is always added and must not be passed. New fails on more than [MaxPlatforms] platforms or
// [MaxIntegrations] integrations, a nil module (a typed nil included), an invalid name or detection, a name
// given twice on one axis, a node label listed twice by one platform, a provider ID scheme without node labels,
// two platforms with the same provider ID scheme and the same node labels, which could never be told apart, or
// an integration whose detection is core group versions only ([CoreOnly]) that offers tools, actions or
// verification checks, a symptom source that [SymptomSource.Validate] refuses, a kind watched twice, by one module or by two (on
// either axis), a diagnosis rule with an invalid or repeated name or with neither an event nor a node
// function, or a verification check that [VerificationCheck] does not allow or that two modules declare.
// Platform and integration names are separate: one of each may share a name. The registry keeps a copy of
// each module's hooks as they were validated, read once, here; [Registry.PlatformHooks] and
// [Registry.IntegrationHooks] return that copy.
func New(platforms []Platform, integrations []Integration) (*Registry, error) {
	if len(platforms) > MaxPlatforms {
		return nil, fmt.Errorf("%d platform modules given, at most %d are allowed besides generic", len(platforms), MaxPlatforms)
	}
	if len(integrations) > MaxIntegrations {
		return nil, fmt.Errorf("%d integration modules given, at most %d are allowed", len(integrations), MaxIntegrations)
	}
	r := &Registry{
		platformByName:    map[string]platformEntry{},
		integrationByName: map[string]integrationEntry{},
		platformLabels:    map[string]bool{},
	}
	g := generic{}
	gh := cloneHooks(g.Hooks())
	if err := validateRules(gh.Rules); err != nil {
		return nil, fmt.Errorf("platform module %q: %w", GenericPlatform, err)
	}
	r.addPlatform(platformEntry{name: GenericPlatform, module: g, hooks: gh})
	claims := map[string]string{}                     // scheme and labels to platform name
	watched := map[string]string{}                    // group and kind to the module that watches it
	checks := map[v1alpha1.VerificationCheck]string{} // check name to the module that implements it
	for i, p := range platforms {
		if isNil(p) {
			return nil, fmt.Errorf("platform module %d is nil", i)
		}
		name := p.Name()
		if err := checkName("platform", name, r.platformByName); err != nil {
			return nil, err
		}
		e, err := platformClaim(name, p.Detection())
		if err != nil {
			return nil, err
		}
		if len(e.labels) > 0 {
			key := e.scheme + "\x00" + strings.Join(e.labels, "\x00")
			if other, ok := claims[key]; ok {
				return nil, fmt.Errorf("platform modules %q and %q both claim provider ID scheme %q with node labels %q: "+
					"their nodes could not be told apart", other, name, e.scheme, e.labels)
			}
			claims[key] = name
		}
		label := fmt.Sprintf("platform module %q", name)
		e.hooks = cloneHooks(p.Hooks())
		if err := errors.Join(validateSources(label, e.hooks.Symptoms, watched), validateRules(e.hooks.Rules),
			validateChecks(label, e.hooks.Checks, checks)); err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		e.module = p
		r.addPlatform(e)
	}

	for i, in := range integrations {
		if isNil(in) {
			return nil, fmt.Errorf("integration module %d is nil", i)
		}
		name := in.Name()
		if err := checkName("integration", name, r.integrationByName); err != nil {
			return nil, err
		}
		d := in.Detection()
		d.GroupVersions = slices.Clone(d.GroupVersions)
		for _, gv := range d.GroupVersions {
			if !groupversion.Valid(gv) {
				return nil, fmt.Errorf("integration module %q: group version %q is invalid", name, gv)
			}
		}
		label := fmt.Sprintf("integration module %q", name)
		hooks := cloneHooks(in.Hooks())
		if CoreOnly(d) && (len(hooks.Tools) > 0 || len(hooks.Actions) > 0 || len(hooks.Checks) > 0) {
			return nil, fmt.Errorf("%s: detection by core group versions only does not narrow, so the module may "+
				"only diagnose: it offers %d tools, %d actions and %d checks", label, len(hooks.Tools),
				len(hooks.Actions), len(hooks.Checks))
		}
		if err := errors.Join(validateSources(label, hooks.Symptoms, watched), validateRules(hooks.Rules),
			validateChecks(label, hooks.Checks, checks)); err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		e := integrationEntry{name: name, module: in, detection: d, hooks: hooks}
		r.integrations = append(r.integrations, e)
		r.integrationByName[name] = e
	}

	slices.SortFunc(r.platforms, func(a, b platformEntry) int { return strings.Compare(a.name, b.name) })
	slices.SortFunc(r.integrations, func(a, b integrationEntry) int { return strings.Compare(a.name, b.name) })
	return r, nil
}

// CoreOnly reports whether an integration's detection declares only group versions of the core group ("v1"),
// which every cluster serves. Such a module, for an add-on with no API group of its own (cloudflared), is
// detected on every cluster, so for it detection does not narrow and it is active wherever it is enabled
// (ADR-0018 amendment of 2026-11-04). [New] therefore allows it only symptom sources and diagnosis rules: no
// tools, actions or verification checks. A detection with no group versions at all is never detected and is
// not core only.
func CoreOnly(d IntegrationDetection) bool {
	if len(d.GroupVersions) == 0 {
		return false
	}
	for _, gv := range d.GroupVersions {
		if strings.Contains(gv, "/") {
			return false
		}
	}
	return true
}

// validateRules checks the diagnosis rules of one module: each has a valid name, given once, and an event or
// a node function.
func validateRules(rules []DiagnosisRule) error {
	seen := map[string]bool{}
	for _, r := range rules {
		if !ValidName(r.Name) {
			return fmt.Errorf("rule name %q is invalid", r.Name)
		}
		if seen[r.Name] {
			return fmt.Errorf("rule %q is listed twice", r.Name)
		}
		seen[r.Name] = true
		if r.Event == nil && r.Node == nil {
			return fmt.Errorf("rule %q has neither an event nor a node function", r.Name)
		}
	}
	return nil
}

// validateChecks checks the verification checks of one module: each has a Name among the API's
// VerificationCheck values, a Group and a Kind, and a Holds function. implemented maps each check name already
// declared to the module that declared it, across both axes; a name declared twice, by this module or another
// one, is refused, so a check never has two implementations. The module's checks are added to implemented.
func validateChecks(module string, checks []VerificationCheck, implemented map[v1alpha1.VerificationCheck]string) error {
	var errs []error
	for _, c := range checks {
		switch {
		case !slices.Contains(v1alpha1.VerificationChecks(), c.Name):
			errs = append(errs, fmt.Errorf("verification check %q is not one of the API's", c.Name))
		case c.Group == "" || c.Kind == "":
			errs = append(errs, fmt.Errorf("verification check %s names no group or kind", c.Name))
		case c.Holds == nil:
			errs = append(errs, fmt.Errorf("verification check %s has no Holds function", c.Name))
		default:
			if other, ok := implemented[c.Name]; ok {
				errs = append(errs, fmt.Errorf("verification check %s is declared twice, by %s and %s", c.Name, other, module))
				continue
			}
			implemented[c.Name] = module
		}
	}
	return errors.Join(errs...)
}

// addPlatform records a platform entry for listing, lookup and detection.
func (r *Registry) addPlatform(e platformEntry) {
	r.platforms = append(r.platforms, e)
	r.platformByName[e.name] = e
	for _, l := range e.labels {
		r.platformLabels[l] = true
	}
}

// isNil reports whether a module is nil, including a nil pointer, map, slice, func or channel in a non-nil
// interface, whose methods would panic.
func isNil(module any) bool {
	if module == nil {
		return true
	}
	v := reflect.ValueOf(module)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}

// checkName validates a module name and fails on one already in seen.
func checkName[V any](axis, name string, seen map[string]V) error {
	if !ValidName(name) {
		return fmt.Errorf("%s module name %q is invalid", axis, name)
	}
	if _, ok := seen[name]; ok {
		return fmt.Errorf("%s module %q is registered twice", axis, name)
	}
	return nil
}

// platformClaim validates a platform's detection and returns it as an entry without its module.
func platformClaim(name string, d PlatformDetection) (platformEntry, error) {
	e := platformEntry{name: name, scheme: d.ProviderIDScheme}
	if e.scheme != "" && !schemePattern.MatchString(e.scheme) {
		return e, fmt.Errorf("platform module %q: provider ID scheme %q is invalid", name, e.scheme)
	}
	for _, l := range d.NodeLabels {
		if !labelKeyPattern.MatchString(l) {
			return e, fmt.Errorf("platform module %q: node label %q is invalid", name, l)
		}
		if slices.Contains(e.labels, l) {
			return e, fmt.Errorf("platform module %q: node label %q is listed twice", name, l)
		}
		e.labels = append(e.labels, l)
	}
	if e.scheme != "" && len(e.labels) == 0 {
		return e, fmt.Errorf("platform module %q: provider ID scheme %q needs at least one node label: "+
			"a scheme names the cloud, not the platform", name, e.scheme)
	}
	slices.Sort(e.labels)
	return e, nil
}

// Platforms returns every platform module, the generic one included, sorted by name.
func (r *Registry) Platforms() []Platform {
	out := make([]Platform, 0, len(r.platforms))
	for _, e := range r.platforms {
		out = append(out, e.module)
	}
	return out
}

// Integrations returns every integration module, sorted by name.
func (r *Registry) Integrations() []Integration {
	out := make([]Integration, 0, len(r.integrations))
	for _, e := range r.integrations {
		out = append(out, e.module)
	}
	return out
}

// Platform returns the platform module with the given name.
func (r *Registry) Platform(name string) (Platform, bool) {
	e, ok := r.platformByName[name]
	return e.module, ok
}

// Integration returns the integration module with the given name.
func (r *Registry) Integration(name string) (Integration, bool) {
	e, ok := r.integrationByName[name]
	return e.module, ok
}

// PlatformHooks returns a copy of the hooks the platform module with the given name declared when the
// registry was built.
func (r *Registry) PlatformHooks(name string) (Hooks, bool) {
	e, ok := r.platformByName[name]
	return cloneHooks(e.hooks), ok
}

// IntegrationHooks returns a copy of the hooks the integration module with the given name declared when the
// registry was built.
func (r *Registry) IntegrationHooks(name string) (Hooks, bool) {
	e, ok := r.integrationByName[name]
	return cloneHooks(e.hooks), ok
}
