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

package prometheus

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Parameter names. Every template takes both.
const (
	// ParamNamespace is the namespace of the workload, a DNS-1123 label.
	ParamNamespace = "namespace"
	// ParamWorkload is the name of the workload, a DNS-1123 subdomain. What it names depends on the template.
	ParamWorkload = "workload"
)

// TemplateInfo describes an approved query template, for building a tool schema or documentation.
type TemplateInfo struct {
	// Name is what MetricsQuery.Template names.
	Name string
	// Description says what the result means and what the workload parameter names.
	Description string
	// Params are the parameter names the template requires; no other is accepted.
	Params []string
	// Labels are the labels a result series may carry; a response with any other is refused.
	Labels []string
}

// template is an approved query. render gets parameters that are already validated.
type template struct {
	info   TemplateInfo
	render func(namespace, workload string) string
}

// Rate windows are fixed: a query never takes a duration from its parameters.
const rateWindow = "[5m]"

// podsOf selects the pods of a Deployment, StatefulSet or DaemonSet by name: the workload name and one or two
// generated segments ("web-7d9c8b6f4-x2x9q", "db-0", "agent-k7v2p"). Some pods of another workload whose name
// extends this one match too (the StatefulSet pod "web-v2-0" for "web"); the pod label of each series tells
// them apart.
func podsOf(namespace, workload string) string {
	return eq("namespace", namespace) + "," +
		matchRegexp("pod", regexp.QuoteMeta(workload)+"-[a-z0-9]+(-[a-z0-9]+)?")
}

// containersOf is podsOf without the pod-level cgroup series of cAdvisor (no container label, or "POD").
func containersOf(namespace, workload string) string {
	return podsOf(namespace, workload) + `,container!="",container!="POD"`
}

// eq and matchRegexp build label matchers. The value is a Go-quoted string, which PromQL reads with the same
// escapes, so no value can end the string; parameters are validated names before they get here anyway.
func eq(label, value string) string { return label + "=" + strconv.Quote(value) }

func matchRegexp(label, re string) string { return label + "=~" + strconv.Quote(re) }

var podContainer = []string{"pod", "container"}

var registry = buildRegistry([]template{
	{
		info: TemplateInfo{
			Name:        "container_memory_working_set_bytes",
			Description: "Memory working set of each container of the workload's pods, in bytes (cAdvisor).",
			Labels:      podContainer,
		},
		render: func(ns, w string) string {
			return "sum by (pod, container) (container_memory_working_set_bytes{" + containersOf(ns, w) + "})"
		},
	},
	{
		info: TemplateInfo{
			Name:        "container_memory_limit_bytes",
			Description: "Memory limit of each container of the workload's pods, in bytes (kube-state-metrics).",
			Labels:      podContainer,
		},
		render: func(ns, w string) string {
			return "max by (pod, container) (kube_pod_container_resource_limits{" + podsOf(ns, w) +
				`,resource="memory"})`
		},
	},
	{
		info: TemplateInfo{
			Name:        "container_cpu_usage_cores",
			Description: "CPU used by each container of the workload's pods, in cores, averaged over 5 minutes (cAdvisor).",
			Labels:      podContainer,
		},
		render: func(ns, w string) string {
			return "sum by (pod, container) (rate(container_cpu_usage_seconds_total{" + containersOf(ns, w) +
				"}" + rateWindow + "))"
		},
	},
	{
		info: TemplateInfo{
			Name: "container_cpu_throttled_ratio",
			Description: "Share of CFS periods in which each container of the workload's pods was throttled, " +
				"0 to 1, over 5 minutes (cAdvisor).",
			Labels: podContainer,
		},
		render: func(ns, w string) string {
			sel := containersOf(ns, w)
			return "sum by (pod, container) (rate(container_cpu_cfs_throttled_periods_total{" + sel + "}" +
				rateWindow + ")) / sum by (pod, container) (rate(container_cpu_cfs_periods_total{" + sel + "}" +
				rateWindow + "))"
		},
	},
	{
		info: TemplateInfo{
			Name:        "container_restarts_total",
			Description: "Restart count of each container of the workload's pods (kube-state-metrics).",
			Labels:      podContainer,
		},
		render: func(ns, w string) string {
			return "max by (pod, container) (kube_pod_container_status_restarts_total{" + podsOf(ns, w) + "})"
		},
	},
	{
		info: TemplateInfo{
			Name: "container_last_terminated_oomkilled",
			Description: "1 for each container of the workload's pods whose last termination was OOMKilled " +
				"(kube-state-metrics).",
			Labels: podContainer,
		},
		render: func(ns, w string) string {
			return "max by (pod, container) (kube_pod_container_status_last_terminated_reason{" + podsOf(ns, w) +
				`,reason="OOMKilled"})`
		},
	},
	{
		info: TemplateInfo{
			Name: "container_waiting_reason",
			Description: "1 for each container of the workload's pods that is waiting, with the reason, such as " +
				"CrashLoopBackOff or ImagePullBackOff (kube-state-metrics).",
			Labels: []string{"pod", "container", "reason"},
		},
		render: func(ns, w string) string {
			return "max by (pod, container, reason) (kube_pod_container_status_waiting_reason{" +
				podsOf(ns, w) + "} == 1)"
		},
	},
	{
		info: TemplateInfo{
			Name:        "deployment_replicas_unavailable",
			Description: "Unavailable replicas of the Deployment named by the workload parameter (kube-state-metrics).",
			Labels:      []string{"deployment"},
		},
		render: func(ns, w string) string {
			return "max by (deployment) (kube_deployment_status_replicas_unavailable{" + eq("namespace", ns) +
				"," + eq("deployment", w) + "})"
		},
	},
})

func buildRegistry(ts []template) map[string]template {
	m := make(map[string]template, len(ts))
	for _, t := range ts {
		if _, dup := m[t.info.Name]; dup {
			panic("duplicate template " + t.info.Name)
		}
		t.info.Params = []string{ParamNamespace, ParamWorkload}
		m[t.info.Name] = t
	}
	return m
}

// Templates lists the approved query templates, sorted by name.
func Templates() []TemplateInfo {
	out := make([]TemplateInfo, 0, len(registry))
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		info := registry[name].info
		info.Params = slices.Clone(info.Params)
		info.Labels = slices.Clone(info.Labels)
		out = append(out, info)
	}
	return out
}

// renderQuery validates params against the template and returns the query. Errors name the parameter only when
// it is one of the template's, never a value or an unknown key.
func (t template) renderQuery(params map[string]string) (string, error) {
	for k := range params {
		if !slices.Contains(t.info.Params, k) {
			return "", fmt.Errorf("unexpected parameter: %w", ErrInvalidQuery)
		}
	}
	ns, ok := params[ParamNamespace]
	if !ok {
		return "", fmt.Errorf("parameter %s is required: %w", ParamNamespace, ErrInvalidQuery)
	}
	if len(validation.IsDNS1123Label(ns)) > 0 {
		return "", fmt.Errorf("parameter %s is not a DNS-1123 label: %w", ParamNamespace, ErrInvalidQuery)
	}
	w, ok := params[ParamWorkload]
	if !ok {
		return "", fmt.Errorf("parameter %s is required: %w", ParamWorkload, ErrInvalidQuery)
	}
	if len(validation.IsDNS1123Subdomain(w)) > 0 {
		return "", fmt.Errorf("parameter %s is not a DNS-1123 subdomain: %w", ParamWorkload, ErrInvalidQuery)
	}
	// Defence in depth: a validated name holds only [a-z0-9.-]. Refuse anything that could end a string.
	if strings.ContainsAny(ns+w, "\"\\\n{}") {
		return "", fmt.Errorf("parameter holds a forbidden character: %w", ErrInvalidQuery)
	}
	return t.render(ns, w), nil
}
