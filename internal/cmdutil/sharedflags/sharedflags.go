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

// Package sharedflags describes the flags the operator binaries share without importing controller-runtime: the
// names of the boolean flags of cmdutil.Options and the parser of --watch-namespaces. kubectl-shadewright reads
// the analyzer's arguments with them, so it does not link the manager, metrics and logging stack of
// internal/cmdutil. A test in internal/cmdutil ties [BoolFlags] to cmdutil.Options.BindFlags.
package sharedflags

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// BoolFlags returns the names of the boolean flags cmdutil.Options.BindFlags registers, sorted. A boolean flag
// takes no separate value, so a parser of the binaries' arguments needs the list.
func BoolFlags() []string {
	return []string{"enable-http2", "leader-elect", "metrics-secure", "zap-devel"}
}

// ParseNamespaces reads a --watch-namespaces flag: a comma-separated list of namespace names. It trims spaces,
// drops duplicates and returns the names sorted. An empty list means no namespace at all, never "all
// namespaces". A name that is not a valid namespace name, such as "*", is an error.
func ParseNamespaces(value string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(value, ",") {
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
