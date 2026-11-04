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
// spaces, drops duplicates and returns the names sorted. An empty list means no namespace at all, never "all
// namespaces": the analyzer then reads nothing namespaced outside the operator namespace (see [CacheOptions])
// and the binary starts no collector (design 2.2 and 10.1). A name that is not a valid namespace name, such
// as "*", is an error.
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
	slices.Sort(out)
	return out, nil
}

// CacheOptions bounds the analyzer's cache to what its RBAC grants: namespaced objects only in the watched
// namespaces, Incidents and RemediationRequests only in the operator namespace. Cluster-scoped objects
// (RemediationSettings) are not affected by namespace restrictions. Managed fields are dropped before an
// object enters the cache: nothing in the analyzer reads them, and they are often the largest part of an
// event or a pod.
//
// With no watched namespace the default is the operator namespace alone, never an empty map: controller-runtime
// reads an empty DefaultNamespaces as every namespace. The analyzer holds no grant on pods or events there, so
// such a cache lists nothing it could not already read, and the binary starts no informer on them anyway.
func CacheOptions(operatorNamespace string, watched []string) cache.Options {
	defaults := make(map[string]cache.Config, max(len(watched), 1))
	for _, ns := range watched {
		defaults[ns] = cache.Config{}
	}
	if len(defaults) == 0 {
		defaults[operatorNamespace] = cache.Config{}
	}
	operator := map[string]cache.Config{operatorNamespace: {}}
	return cache.Options{
		DefaultNamespaces: defaults,
		DefaultTransform:  cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			&remediationv1alpha1.Incident{}:           {Namespaces: operator},
			&remediationv1alpha1.RemediationRequest{}: {Namespaces: operator},
		},
	}
}
