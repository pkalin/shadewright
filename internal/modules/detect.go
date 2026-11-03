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

import "strings"

// Node is what platform detection reads from one node: spec.providerID and metadata.labels.
type Node struct {
	// ProviderID is the node's spec.providerID, for example "azure:///subscriptions/...". It may be empty.
	ProviderID string
	// Labels are the node's labels. Only their keys are read.
	Labels map[string]string
}

// DetectPlatform returns the platform every node runs on. A node is on a platform when its provider ID has
// that platform's scheme or, for a node without a provider ID, when exactly one platform claims its labels.
// The generic platform is the answer for no nodes, for a node no platform claims, and for nodes on different
// platforms: a guess would bring diagnosis that does not fit the cluster. The function reads only its input.
func (r *Registry) DetectPlatform(nodes []Node) Platform {
	found := ""
	for _, n := range nodes {
		name := r.platformOf(n)
		if name == "" || (found != "" && name != found) {
			found = ""
			break
		}
		found = name
	}
	if found == "" {
		found = GenericPlatform
	}
	p, _ := r.Platform(found)
	return p
}

// platformOf returns the name of the platform that claims the node, or "" when none or more than one does.
func (r *Registry) platformOf(n Node) string {
	if n.ProviderID != "" {
		scheme, _, ok := strings.Cut(n.ProviderID, "://")
		if !ok {
			return ""
		}
		return r.byScheme[scheme]
	}
	found := ""
	for key := range n.Labels {
		name, ok := r.byLabel[key]
		if !ok {
			continue
		}
		if found != "" && name != found {
			return ""
		}
		found = name
	}
	return found
}

// DetectIntegrations returns the integrations whose add-on the cluster serves, sorted by name. groupVersions
// are the group versions from the discovery API, in the form of its groupVersion field ("keda.sh/v1alpha1",
// "v1"). The function reads only its input.
func (r *Registry) DetectIntegrations(groupVersions []string) []Integration {
	served := make(map[string]bool, len(groupVersions))
	for _, gv := range groupVersions {
		served[gv] = true
	}
	var out []Integration
	for _, e := range r.integrations {
		for _, gv := range e.detection.GroupVersions {
			if served[gv] {
				out = append(out, e.module)
				break
			}
		}
	}
	return out
}
