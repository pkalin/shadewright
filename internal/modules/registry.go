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
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// namePattern is the form of a module name: a DNS label that starts with a letter, so it fits a CRD enum, a
// status field and a metric label as it is.
var namePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// schemePattern is the form of a provider ID scheme (RFC 3986, lower case).
var schemePattern = regexp.MustCompile(`^[a-z][a-z0-9+.-]{0,62}$`)

// groupVersionPattern is a group version in discovery form: "version" for the core group or "group/version".
var groupVersionPattern = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?[a-z0-9]{1,63}$`)

// labelKeyPattern is a Kubernetes label key: an optional DNS subdomain prefix and a name.
var labelKeyPattern = regexp.MustCompile(
	`^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

// Registry holds the compiled-in modules. It is immutable once built by [New] and safe for concurrent use.
type Registry struct {
	platforms    []platformEntry
	integrations []integrationEntry
	byScheme     map[string]string // provider ID scheme to platform name
	byLabel      map[string]string // node label key to platform name
}

// platformEntry and integrationEntry keep the name, and for an integration the detection, a module declared
// when the registry was built, so lookups and detection do not depend on a module returning the same values
// twice. Platform detection is kept in byScheme and byLabel.
type platformEntry struct {
	name   string
	module Platform
}

type integrationEntry struct {
	name      string
	module    Integration
	detection IntegrationDetection
}

// New builds a registry from the platform and integration modules compiled into the binary. The generic
// platform is always added and must not be passed. New fails on a nil module, an invalid name or detection,
// a name given twice on one axis, or a provider ID scheme or node label claimed by two platforms. Platform and
// integration names are separate: one of each may share a name.
func New(platforms []Platform, integrations []Integration) (*Registry, error) {
	r := &Registry{byScheme: map[string]string{}, byLabel: map[string]string{}}
	names := map[string]bool{GenericPlatform: true}
	r.platforms = append(r.platforms, platformEntry{name: GenericPlatform, module: generic{}})
	for i, p := range platforms {
		if p == nil {
			return nil, fmt.Errorf("platform module %d is nil", i)
		}
		name := p.Name()
		if err := checkName("platform", name, names); err != nil {
			return nil, err
		}
		if err := r.claim(name, p.Detection()); err != nil {
			return nil, err
		}
		r.platforms = append(r.platforms, platformEntry{name: name, module: p})
	}

	names = map[string]bool{}
	for i, in := range integrations {
		if in == nil {
			return nil, fmt.Errorf("integration module %d is nil", i)
		}
		name := in.Name()
		if err := checkName("integration", name, names); err != nil {
			return nil, err
		}
		d := in.Detection()
		d.GroupVersions = slices.Clone(d.GroupVersions)
		for _, gv := range d.GroupVersions {
			if !groupVersionPattern.MatchString(gv) {
				return nil, fmt.Errorf("integration module %q: group version %q is invalid", name, gv)
			}
		}
		r.integrations = append(r.integrations, integrationEntry{name: name, module: in, detection: d})
	}

	slices.SortFunc(r.platforms, func(a, b platformEntry) int { return strings.Compare(a.name, b.name) })
	slices.SortFunc(r.integrations, func(a, b integrationEntry) int { return strings.Compare(a.name, b.name) })
	return r, nil
}

// checkName validates a module name and records it in seen, failing on a name already there.
func checkName(axis, name string, seen map[string]bool) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%s module name %q is invalid", axis, name)
	}
	if seen[name] {
		return fmt.Errorf("%s module %q is registered twice", axis, name)
	}
	seen[name] = true
	return nil
}

// claim records the provider ID scheme and node labels of a platform, failing on one another platform holds.
func (r *Registry) claim(name string, d PlatformDetection) error {
	if s := d.ProviderIDScheme; s != "" {
		if !schemePattern.MatchString(s) {
			return fmt.Errorf("platform module %q: provider ID scheme %q is invalid", name, s)
		}
		if other, ok := r.byScheme[s]; ok {
			return fmt.Errorf("platform modules %q and %q both claim provider ID scheme %q", other, name, s)
		}
		r.byScheme[s] = name
	}
	for _, l := range d.NodeLabels {
		if !labelKeyPattern.MatchString(l) {
			return fmt.Errorf("platform module %q: node label %q is invalid", name, l)
		}
		if other, ok := r.byLabel[l]; ok {
			return fmt.Errorf("platform modules %q and %q both claim node label %q", other, name, l)
		}
		r.byLabel[l] = name
	}
	return nil
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
	for _, e := range r.platforms {
		if e.name == name {
			return e.module, true
		}
	}
	return nil, false
}

// Integration returns the integration module with the given name.
func (r *Registry) Integration(name string) (Integration, bool) {
	for _, e := range r.integrations {
		if e.name == name {
			return e.module, true
		}
	}
	return nil, false
}
