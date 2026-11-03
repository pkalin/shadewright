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
	"regexp"
	"slices"
	"strings"
)

// Rule names of the generic platform.
const (
	RuleNodePressure         = "node-pressure"
	RuleInsufficientCapacity = "insufficient-capacity"
)

// Symptom reasons of the generic platform.
const (
	ReasonNodeMemoryPressure       = "NodeMemoryPressure"
	ReasonNodeDiskPressure         = "NodeDiskPressure"
	ReasonNodePIDPressure          = "NodePIDPressure"
	ReasonInsufficientNodeCapacity = "InsufficientNodeCapacity"
	reasonFailedScheduling         = "FailedScheduling"
	conditionTrue                  = "True"
)

// Evidence fields that rules of any module share.
const (
	EvidenceEventReason              = "event.reason"
	EvidenceEventNote                = "event.note"
	EvidenceEventReportingController = "event.reportingController"
)

// pressure maps a node pressure condition to its symptom.
var pressure = []struct{ condition, reason, summary string }{
	{"MemoryPressure", ReasonNodeMemoryPressure,
		"The node reports memory pressure: the kubelet evicts pods and the scheduler places no new pods there"},
	{"DiskPressure", ReasonNodeDiskPressure,
		"The node reports disk pressure: the kubelet evicts pods and removes images, and no new pods are placed there"},
	{"PIDPressure", ReasonNodePIDPressure,
		"The node reports PID pressure: it is close to its process limit, and no new pods are placed there"},
}

var (
	// insufficientRe finds the resources the scheduler found short, as in "3 Insufficient cpu",
	// "1 Insufficient hugepages-2Mi", "2 Insufficient nvidia.com/gpu" or "Too many pods". A resource name is
	// a qualified name with an optional DNS prefix (at most 253 + 1 + 63 characters), so the match is bounded.
	insufficientRe = regexp.MustCompile(`\bInsufficient ((?:[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)|\b(Too many pods)\b`)
	// nodesAvailableRe reads "0/5 nodes are available".
	nodesAvailableRe = regexp.MustCompile(`\b([0-9]{1,6})/([0-9]{1,6}) nodes are available\b`)
)

// genericRules are the rules of the generic platform: node pressure and node capacity hints that hold on
// every conformant cluster.
func genericRules() []DiagnosisRule {
	return []DiagnosisRule{
		{Name: RuleNodePressure, Node: nodePressure},
		{Name: RuleInsufficientCapacity, Event: insufficientCapacity},
	}
}

// nodePressure turns each pressure condition that is True into a symptom.
func nodePressure(n NodeState) []Symptom {
	var out []Symptom
	for _, p := range pressure {
		for _, c := range n.Conditions {
			if c.Type != p.condition || c.Status != conditionTrue {
				continue
			}
			field := "node.condition." + p.condition
			out = append(out, Symptom{Reason: p.reason, Summary: p.summary, Evidence: []Evidence{
				{Field: field + ".status", Value: c.Status},
				{Field: field + ".reason", Value: c.Reason},
				{Field: field + ".message", Value: c.Message},
			}})
			break
		}
	}
	return out
}

// insufficientCapacity reads a FailedScheduling event of a pod whose note says that no node has enough of a
// resource: a capacity hint, not a diagnosis of why (requests, quota or node pool size).
func insufficientCapacity(ev Event) (Symptom, bool) {
	if ev.Reason != reasonFailedScheduling || ev.Regarding.Kind != "Pod" {
		return Symptom{}, false
	}
	var resources []string
	for _, m := range insufficientRe.FindAllStringSubmatch(ev.Note, -1) {
		r := strings.TrimRight(m[1], ".")
		if m[4] != "" {
			r = "pods"
		}
		if !slices.Contains(resources, r) {
			resources = append(resources, r)
		}
	}
	if len(resources) == 0 {
		return Symptom{}, false
	}
	slices.Sort(resources)
	evidence := []Evidence{
		{Field: EvidenceEventReason, Value: ev.Reason},
		{Field: "scheduler.insufficient", Value: strings.Join(resources, ", ")},
	}
	if m := nodesAvailableRe.FindStringSubmatch(ev.Note); m != nil {
		evidence = append(evidence, Evidence{Field: "scheduler.nodesAvailable", Value: m[1] + "/" + m[2]})
	}
	evidence = append(evidence, Evidence{Field: EvidenceEventNote, Value: ev.Note})
	return Symptom{
		Reason:   ReasonInsufficientNodeCapacity,
		Summary:  "The scheduler found no node with enough free capacity for the pod",
		Evidence: evidence,
	}, true
}
