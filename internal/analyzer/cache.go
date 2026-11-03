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

package analyzer

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// ParseNamespaces reads the --watch-namespaces flag: a comma-separated list of namespace names. It trims
// spaces, drops duplicates and returns the names sorted. An empty list is an error, not "all namespaces":
// an unrestricted cache would ask for more than the analyzer's RoleBindings grant (design 10.1).
func ParseNamespaces(flag string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(flag, ",") {
		ns := strings.TrimSpace(part)
		if ns == "" {
			continue
		}
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return nil, fmt.Errorf("watch namespace %q is not a valid namespace name: %s", ns, strings.Join(errs, "; "))
		}
		if !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no namespace to watch: set --watch-namespaces to at least one namespace")
	}
	slices.Sort(out)
	return out, nil
}

// CacheOptions bounds the analyzer's cache to what its RBAC grants: namespaced objects only in the watched
// namespaces, Incidents and RemediationRequests only in the operator namespace. Cluster-scoped objects
// (RemediationSettings) are not affected by namespace restrictions.
func CacheOptions(operatorNamespace string, watched []string) cache.Options {
	defaults := make(map[string]cache.Config, len(watched))
	for _, ns := range watched {
		defaults[ns] = cache.Config{}
	}
	operator := map[string]cache.Config{operatorNamespace: {}}
	return cache.Options{
		DefaultNamespaces: defaults,
		ByObject: map[client.Object]cache.ByObject{
			&remediationv1alpha1.Incident{}:           {Namespaces: operator},
			&remediationv1alpha1.RemediationRequest{}: {Namespaces: operator},
		},
	}
}
