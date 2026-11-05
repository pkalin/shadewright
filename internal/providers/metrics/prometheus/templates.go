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

// Parameter names.
const (
	// ParamNamespace is the namespace of the target, a DNS-1123 label. Every template takes it.
	ParamNamespace = "namespace"
	// ParamWorkload is the name of a workload, a DNS-1123 subdomain. Only a template that matches a workload by
	// its own label takes it (deployment_replicas_unavailable); the pod templates take the pods instead.
	ParamWorkload = "workload"
)

// Bounds of MetricsQuery.Pods for the pod templates.
const (
	// MaxPods bounds the pods of one query. It equals MaxSeries: every pod with data adds at least one series
	// to the result, so a larger set could not be answered within MaxSeries anyway.
	MaxPods = MaxSeries
	// MaxPodName bounds the name of each pod. The pods of a Deployment (through its ReplicaSet) and of a
	// DaemonSet get their names from generateName, which the API server cuts to 58 characters before it adds 5
	// random ones; a StatefulSet pod is named "<name>-<ordinal>", and a StatefulSet name is in practice at most
	// 52 characters long (its controller-revision-hash label must fit 63). So the pods of these kinds are never
	// longer than 63 characters, and the bound keeps the query text, which lists every name, short.
	MaxPodName = validation.DNS1123LabelMaxLength
)

// TemplateInfo describes an approved query template, for building a tool schema or documentation.
type TemplateInfo struct {
	// Name is what MetricsQuery.Template names.
	Name string
	// Description says what the result means and what the workload parameter, if any, names.
	Description string
	// Params are the parameter names the template requires; no other is accepted.
	Params []string
	// Pods says the template selects exactly the pods named by MetricsQuery.Pods, which it then requires (1 to
	// MaxPods names); a template without it refuses any pod.
	Pods bool
	// Labels are the labels a result series may carry; a response with any other is refused.
	Labels []string
}

// template is an approved query. render gets values that are already validated: workload is empty unless the
// template takes it, and pods is sorted, without duplicates, and empty unless info.Pods is set.
type template struct {
	info   TemplateInfo
	render func(namespace, workload string, pods []string) string
}

// Rate windows are fixed: a query never takes a duration from its parameters.
const rateWindow = "[5m]"

// podsOf selects exactly the given pods of the namespace. The names are joined as anchored alternatives
// ("^(web-7d9c8b6f4-k8m2p|web-7d9c8b6f4-x2x9q)$"), each with its regular expression metacharacters escaped, so
// a pod of another workload whose name extends a target pod's name never matches. Prometheus anchors a
// regular expression matcher anyway; the anchors only make it explicit.
func podsOf(namespace string, pods []string) string {
	quoted := make([]string, len(pods))
	for i, p := range pods {
		quoted[i] = regexp.QuoteMeta(p)
	}
	return eq("namespace", namespace) + "," + matchRegexp("pod", "^("+strings.Join(quoted, "|")+")$")
}

// containersOf is podsOf without the pod-level cgroup series of cAdvisor (no container label, or "POD").
func containersOf(namespace string, pods []string) string {
	return podsOf(namespace, pods) + `,container!="",container!="POD"`
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
			Description: "Memory working set of each container of the given pods, in bytes (cAdvisor).",
			Pods:        true,
			Labels:      podContainer,
		},
		render: func(ns, _ string, pods []string) string {
			return "sum by (pod, container) (container_memory_working_set_bytes{" + containersOf(ns, pods) + "})"
		},
	},
	{
		info: TemplateInfo{
			Name:        "container_memory_limit_bytes",
			Description: "Memory limit of each container of the given pods, in bytes (kube-state-metrics).",
			Pods:        true,
			Labels:      podContainer,
		},
		render: func(ns, _ string, pods []string) string {
			return "max by (pod, container) (kube_pod_container_resource_limits{" + podsOf(ns, pods) +
				`,resource="memory"})`
		},
	},
	{
		info: TemplateInfo{
			Name:        "container_cpu_usage_cores",
			Description: "CPU used by each container of the given pods, in cores, averaged over 5 minutes (cAdvisor).",
			Pods:        true,
			Labels:      podContainer,
		},
		render: func(ns, _ string, pods []string) string {
			return "sum by (pod, container) (rate(container_cpu_usage_seconds_total{" + containersOf(ns, pods) +
				"}" + rateWindow + "))"
		},
	},
	{
		info: TemplateInfo{
			Name: "container_cpu_throttled_ratio",
			Description: "Share of CFS periods in which each container of the given pods was throttled, " +
				"0 to 1, over 5 minutes (cAdvisor).",
			Pods:   true,
			Labels: podContainer,
		},
		render: func(ns, _ string, pods []string) string {
			sel := containersOf(ns, pods)
			return "sum by (pod, container) (rate(container_cpu_cfs_throttled_periods_total{" + sel + "}" +
				rateWindow + ")) / sum by (pod, container) (rate(container_cpu_cfs_periods_total{" + sel + "}" +
				rateWindow + "))"
		},
	},
	{
		info: TemplateInfo{
			Name:        "container_restarts_total",
			Description: "Restart count of each container of the given pods (kube-state-metrics).",
			Pods:        true,
			Labels:      podContainer,
		},
		render: func(ns, _ string, pods []string) string {
			return "max by (pod, container) (kube_pod_container_status_restarts_total{" + podsOf(ns, pods) + "})"
		},
	},
	{
		info: TemplateInfo{
			Name: "container_last_terminated_oomkilled",
			Description: "1 for each container of the given pods whose last termination was OOMKilled " +
				"(kube-state-metrics).",
			Pods:   true,
			Labels: podContainer,
		},
		render: func(ns, _ string, pods []string) string {
			return "max by (pod, container) (kube_pod_container_status_last_terminated_reason{" +
				podsOf(ns, pods) + `,reason="OOMKilled"})`
		},
	},
	{
		info: TemplateInfo{
			Name: "container_waiting_reason",
			Description: "1 for each container of the given pods that is waiting, with the reason, such as " +
				"CrashLoopBackOff or ImagePullBackOff (kube-state-metrics).",
			Pods:   true,
			Labels: []string{"pod", "container", "reason"},
		},
		render: func(ns, _ string, pods []string) string {
			return "max by (pod, container, reason) (kube_pod_container_status_waiting_reason{" +
				podsOf(ns, pods) + "} == 1)"
		},
	},
	{
		info: TemplateInfo{
			Name:        "deployment_replicas_unavailable",
			Description: "Unavailable replicas of the Deployment named by the workload parameter (kube-state-metrics).",
			Params:      []string{ParamWorkload},
			Labels:      []string{"deployment"},
		},
		render: func(ns, w string, _ []string) string {
			return "max by (deployment) (kube_deployment_status_replicas_unavailable{" + eq("namespace", ns) +
				"," + eq("deployment", w) + "})"
		},
	},
})

// buildRegistry indexes the templates by name and puts ParamNamespace first in each one's parameters.
func buildRegistry(ts []template) map[string]template {
	m := make(map[string]template, len(ts))
	for _, t := range ts {
		if _, dup := m[t.info.Name]; dup {
			panic("duplicate template " + t.info.Name)
		}
		if t.info.Pods != slices.Contains(t.info.Labels, "pod") {
			panic("template " + t.info.Name + " must select pods exactly when its series carry the pod label")
		}
		t.info.Params = append([]string{ParamNamespace}, t.info.Params...)
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

// renderQuery validates params and pods against the template and returns the query and, for a pod template,
// the pods it selects (sorted, without duplicates). Errors name the parameter only when it is one of the
// template's, and the position of a refused pod only, never a value or an unknown key.
func (t template) renderQuery(params map[string]string, pods []string) (string, []string, error) {
	for k := range params {
		if !slices.Contains(t.info.Params, k) {
			return "", nil, fmt.Errorf("unexpected parameter: %w", ErrInvalidQuery)
		}
	}
	ns, ok := params[ParamNamespace]
	if !ok {
		return "", nil, fmt.Errorf("parameter %s is required: %w", ParamNamespace, ErrInvalidQuery)
	}
	if len(validation.IsDNS1123Label(ns)) > 0 {
		return "", nil, fmt.Errorf("parameter %s is not a DNS-1123 label: %w", ParamNamespace, ErrInvalidQuery)
	}
	var w string
	if slices.Contains(t.info.Params, ParamWorkload) {
		if w, ok = params[ParamWorkload]; !ok {
			return "", nil, fmt.Errorf("parameter %s is required: %w", ParamWorkload, ErrInvalidQuery)
		}
		if len(validation.IsDNS1123Subdomain(w)) > 0 {
			return "", nil, fmt.Errorf("parameter %s is not a DNS-1123 subdomain: %w", ParamWorkload, ErrInvalidQuery)
		}
	}
	selected, err := t.validPods(pods)
	if err != nil {
		return "", nil, err
	}
	return t.render(ns, w, selected), selected, nil
}

// validPods checks the pods of a query: none for a template that does not select pods, 1 to MaxPods otherwise,
// each a DNS-1123 subdomain of at most MaxPodName characters. It returns them sorted and without duplicates,
// so that the same set always gives the same query text.
func (t template) validPods(pods []string) ([]string, error) {
	if !t.info.Pods {
		if len(pods) > 0 {
			return nil, fmt.Errorf("the template takes no pods: %w", ErrInvalidQuery)
		}
		return nil, nil
	}
	switch {
	case len(pods) == 0:
		return nil, fmt.Errorf("the template requires the target's pods: %w", ErrInvalidQuery)
	case len(pods) > MaxPods:
		return nil, fmt.Errorf("more than %d pods: %w", MaxPods, ErrInvalidQuery)
	}
	for i, p := range pods {
		if len(p) > MaxPodName || len(validation.IsDNS1123Subdomain(p)) > 0 {
			return nil, fmt.Errorf("pod %d is not a DNS-1123 subdomain of at most %d characters: %w", i, MaxPodName,
				ErrInvalidQuery)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(pods))), nil
}
