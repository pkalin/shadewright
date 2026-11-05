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
	"regexp"
	"slices"
	"strings"

	"github.com/pkalin/shadewright/internal/groupversion"
)

var (
	// kindPattern is the ObjectRef schema's kind.
	kindPattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)
	// resourcePattern is a plural resource name: a DNS label.
	resourcePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	// controllerPattern is a component name such as kustomize-controller, the collector's rule.
	controllerPattern = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_./]{0,251}[A-Za-z0-9])?$`)
)

// Validate reports why a symptom source cannot be used: no version, a version listed twice, a group version
// that is not valid (an empty Group is the core group), a kind or resource outside the schema, a reporting
// controller that is not a component name, no Symptoms function, or a subject owner that is set (any field)
// with an invalid group version, kind or resource, or that names the watched kind itself.
func (s SymptomSource) Validate() error {
	if len(s.Versions) == 0 {
		return fmt.Errorf("symptom source %s %q lists no version", s.Group, s.Kind)
	}
	for i, v := range s.Versions {
		gv := v
		if s.Group != "" {
			gv = s.Group + "/" + v
		}
		if !groupversion.Valid(gv) || strings.Contains(v, "/") {
			return fmt.Errorf("symptom source %s %q: group version %q is invalid", s.Group, s.Kind, gv)
		}
		if slices.Contains(s.Versions[:i], v) {
			return fmt.Errorf("symptom source %s %q: version %q is listed twice", s.Group, s.Kind, v)
		}
	}
	if !kindPattern.MatchString(s.Kind) {
		return fmt.Errorf("symptom source %s: kind %q is invalid", s.Group, s.Kind)
	}
	if !resourcePattern.MatchString(s.Resource) {
		return fmt.Errorf("symptom source %s %q: resource %q is invalid", s.Group, s.Kind, s.Resource)
	}
	if !controllerPattern.MatchString(s.ReportingController) {
		return fmt.Errorf("symptom source %s %q: reporting controller %q is invalid", s.Group, s.Kind, s.ReportingController)
	}
	if s.Symptoms == nil {
		return fmt.Errorf("symptom source %s %q has no Symptoms function", s.Group, s.Kind)
	}
	return s.Subject.validate(s.Group, s.Kind)
}

// validate reports why a subject owner cannot be used. One that is not set is valid. Otherwise its version,
// group version, kind and resource follow the rules of a symptom source, and it is another kind than the
// watched one.
func (o SubjectOwner) validate(group, kind string) error {
	if !o.IsSet() {
		return nil
	}
	gv := o.APIVersion()
	if o.Version == "" || !groupversion.Valid(gv) || strings.Contains(o.Version, "/") {
		return fmt.Errorf("symptom source %s %q: subject group version %q is invalid", group, kind, gv)
	}
	if !kindPattern.MatchString(o.Kind) {
		return fmt.Errorf("symptom source %s %q: subject kind %q is invalid", group, kind, o.Kind)
	}
	if !resourcePattern.MatchString(o.Resource) {
		return fmt.Errorf("symptom source %s %q: subject resource %q is invalid", group, kind, o.Resource)
	}
	if o.Group == group && o.Kind == kind {
		return fmt.Errorf("symptom source %s %q: the subject is the watched kind itself", group, kind)
	}
	return nil
}

// validateSources checks every symptom source of one module. watched maps each group and kind already watched
// to the module that watches it, across both axes; a kind watched twice, by this module or another one, is
// refused, so no object gives two symptoms from two sources. The module's kinds are added to watched.
func validateSources(module string, sources []SymptomSource, watched map[string]string) error {
	var errs []error
	for _, s := range sources {
		if err := s.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		key := s.Group + "/" + s.Kind
		if other, ok := watched[key]; ok {
			errs = append(errs, fmt.Errorf("kind %s %q is watched twice, by %s and %s", s.Group, s.Kind, other, module))
			continue
		}
		watched[key] = module
	}
	return errors.Join(errs...)
}

// cloneHooks copies what a module contributed, down to the slices in each symptom source, so a module that
// keeps and later changes the slices it returned cannot change the registry.
func cloneHooks(h Hooks) Hooks {
	out := Hooks{
		Symptoms: slices.Clone(h.Symptoms),
		Rules:    slices.Clone(h.Rules),
		Tools:    slices.Clone(h.Tools),
		Actions:  slices.Clone(h.Actions),
		Checks:   slices.Clone(h.Checks),
	}
	for i := range out.Symptoms {
		out.Symptoms[i].Versions = slices.Clone(out.Symptoms[i].Versions)
	}
	return out
}

// SourceFor returns the symptom source of an active module that watches the group and kind, with the module's
// name, and false when no active module watches it. The registry allows one source per kind, so there is at
// most one. The source is a copy, down to its versions.
func (s Selection) SourceFor(group, kind string) (module string, src SymptomSource, ok bool) {
	for _, m := range s.activeList {
		for _, src := range m.Hooks.Symptoms {
			if src.Group == group && src.Kind == kind {
				src.Versions = slices.Clone(src.Versions)
				return m.Name, src, true
			}
		}
	}
	return "", SymptomSource{}, false
}

// EvaluateSource runs a symptom source on one object, as decoded from JSON, and returns its symptoms with
// evidence redacted and bounded ([BoundValue]). A source that panics shows nothing, and a symptom that
// [ValidSymptom] refuses or that has no summary is dropped: both are bugs in the module, which its tests catch.
// Since is kept as the source set it; the caller bounds it by its clock.
func EvaluateSource(src SymptomSource, obj map[string]any) []Symptom {
	if src.Symptoms == nil {
		return nil
	}
	var out []Symptom
	for _, sym := range safeSource(src.Symptoms, obj) {
		if ValidSymptom(sym) != nil || sym.Summary == "" {
			continue
		}
		ev := make([]Evidence, 0, len(sym.Evidence))
		for _, e := range sym.Evidence {
			ev = append(ev, Evidence{Field: e.Field, Value: BoundValue(e.Value)})
		}
		sym.Evidence = ev
		out = append(out, sym)
	}
	return out
}

// safeSource calls a source's Symptoms and treats a panic as no symptom, so a module bug cannot stop the
// analyzer.
func safeSource(f func(map[string]any) []Symptom, obj map[string]any) (out []Symptom) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	return f(obj)
}
