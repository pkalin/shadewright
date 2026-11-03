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

// DetectPlatform returns the platform the cluster runs on. A node is on a platform when it carries every node
// label that platform requires and, if the node has a provider ID and the platform declares a scheme, its
// provider ID has that scheme. The scheme alone never decides: it names the cloud, not the managed service, so
// a self-managed cluster on cloud machines (Cluster API, kubeadm) stays generic.
//
// Nodes no platform could claim, because they have neither a provider ID nor any label a platform requires
// (a node still joining, a virtual node), are left out. The cluster is on a platform when every remaining
// node is on it. The generic platform is the answer when no node remains, when a remaining node is on no
// platform or on more than one, and when nodes are on different platforms: a guess would bring diagnosis that
// does not fit the cluster. The function reads only its input.
func (r *Registry) DetectPlatform(nodes []Node) Platform {
	found := ""
	for _, n := range nodes {
		if r.unclaimable(n) {
			continue
		}
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
	return r.platformByName[found].module
}

// unclaimable reports whether no platform could claim the node: it has no provider ID and no label any
// platform requires.
func (r *Registry) unclaimable(n Node) bool {
	if n.ProviderID != "" {
		return false
	}
	for key := range n.Labels {
		if r.platformLabels[key] {
			return false
		}
	}
	return true
}

// platformOf returns the name of the one platform that claims the node, or "" when none or more than one does.
func (r *Registry) platformOf(n Node) string {
	// A provider ID without "://" has no scheme, so it fits no platform that declares one.
	scheme, _, ok := strings.Cut(n.ProviderID, "://")
	if !ok {
		scheme = ""
	}
	found := ""
	for _, e := range r.platforms {
		if len(e.labels) == 0 || !hasLabels(n, e.labels) {
			continue
		}
		if n.ProviderID != "" && e.scheme != "" && scheme != e.scheme {
			continue
		}
		if found != "" {
			return ""
		}
		found = e.name
	}
	return found
}

// hasLabels reports whether the node has every one of the label keys.
func hasLabels(n Node, keys []string) bool {
	for _, k := range keys {
		if _, ok := n.Labels[k]; !ok {
			return false
		}
	}
	return true
}

// DetectedIntegration is an integration whose add-on the cluster serves, with the group version that showed it.
type DetectedIntegration struct {
	// Integration is the detected module.
	Integration Integration
	// GroupVersion is the first of the module's declared group versions that the cluster serves, in discovery
	// form ("keda.sh/v1alpha1"). It is what RemediationSettings status.integrations[].version reports.
	GroupVersion string
}

// DetectIntegrations returns the integrations whose add-on the cluster serves, sorted by name, each with the
// group version that matched: the first one the module declares that is served. groupVersions are the group
// versions from the discovery API, in the form of its groupVersion field ("keda.sh/v1alpha1", "v1"). The
// function reads only its input.
func (r *Registry) DetectIntegrations(groupVersions []string) []DetectedIntegration {
	served := make(map[string]bool, len(groupVersions))
	for _, gv := range groupVersions {
		served[gv] = true
	}
	var out []DetectedIntegration
	for _, e := range r.integrations {
		for _, gv := range e.detection.GroupVersions {
			if served[gv] {
				out = append(out, DetectedIntegration{Integration: e.module, GroupVersion: gv})
				break
			}
		}
	}
	return out
}
