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
// controller that is not a component name, or no Symptoms function.
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
	return nil
}

// validateSources checks every symptom source of a module and that no kind is watched twice.
func validateSources(sources []SymptomSource) error {
	seen := map[string]bool{}
	var errs []error
	for _, s := range sources {
		if err := s.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		key := s.Group + "/" + s.Kind
		if seen[key] {
			errs = append(errs, fmt.Errorf("kind %s %q is watched twice", s.Group, s.Kind))
		}
		seen[key] = true
	}
	return errors.Join(errs...)
}
