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

// Package aks is the Azure Kubernetes Service platform module (design 2.4, ADR-0018).
//
// Detection: a node is on AKS when it carries the label kubernetes.azure.com/cluster, which the managed
// service sets on every node, and, when its provider ID is set, that ID has the "azure" scheme. The scheme
// alone is not enough: a Cluster API or kubeadm cluster on Azure machines has it too.
//
// Rules turn known AKS events and node conditions into symptoms with evidence:
//
//   - pod-ip-exhaustion: a FailedCreatePodSandBox event whose note says the Azure CNI found no free pod IP
//     ("not enough IPs available", "Failed to allocate address"), or any event naming InsufficientSubnetSize.
//   - quota-exceeded: an event whose note carries Azure's QuotaExceeded or InsufficientVCPUQuota error code, as
//     the cluster autoscaler or node auto-provisioning report a refused scale-up; the code (without the
//     "ErrCode_" prefix), the quota, the region and the numbers of the message are kept as evidence, and the
//     note itself always is.
//   - spot-eviction: a PreemptScheduled event about a Node, written in the namespace "default" (where the
//     Kubernetes event recorders put events about cluster-scoped objects), or a node whose VMEventScheduled
//     condition is True and names a Preempt event: Azure is about to evict a Spot VM.
//
// Sources are not checked. Microsoft documents PreemptScheduled as a node-problem-detector event but not the
// reporting controller AKS gives it, so the rule does not pin one; the reporter is kept as evidence. Whoever can
// create events in "default" (or in a watched namespace, for the other rules) can forge a matching event, as
// for every event the collector reads (invariant 4).
//
// The identifiers come from Microsoft Learn:
//
//   - kubernetes.azure.com/cluster and the priority labels: https://learn.microsoft.com/azure/aks/use-labels#unavailable-labels
//     and https://learn.microsoft.com/azure/aks/spot-node-pool
//   - VMEventScheduled and PreemptScheduled: https://learn.microsoft.com/azure/aks/node-problem-detector
//   - "not enough IPs available": https://learn.microsoft.com/azure/aks/concepts-network-azure-cni-pod-subnet
//   - "Failed to allocate address": https://learn.microsoft.com/azure/architecture/operator-guides/aks/troubleshoot-network-aks#pod-fails-to-allocate-the-ip-address
//   - InsufficientSubnetSize: https://learn.microsoft.com/troubleshoot/azure/azure-kubernetes/connectivity/insufficientsubnetsize-error-advanced-networking
//   - QuotaExceeded and ErrCode_InsufficientVCPUQuota, with the message format the evidence reads:
//     https://learn.microsoft.com/troubleshoot/azure/azure-kubernetes/create-upgrade-delete/quota-exceeded-during-creation-upgrade
//
// Not documented: the message of a VMEventScheduled condition that is True. The node rule matches "preempt" in
// its reason or message and reports nothing otherwise.
//
// The module contributes no tools and no actions. Like every module it is active only when the cluster admin
// enabled it in RemediationSettings and the cluster is detected as AKS. It imports only the standard library
// and this module, and no Azure SDK (ADR-0013).
package aks

import (
	"regexp"
	"strings"

	"github.com/pkalin/shadewright/internal/modules"
)

// Name is the module's name in RemediationSettings.
const Name = "aks"

// Detection labels and values.
const (
	// ClusterLabel is set by AKS on every node, with the node resource group as its value.
	ClusterLabel = "kubernetes.azure.com/cluster"
	// ScaleSetPriorityLabel is set by AKS on the nodes of a Spot node pool, with the value "spot". Microsoft
	// marks it deprecated in favour of [PriorityLabel]; both are kept as evidence.
	ScaleSetPriorityLabel = "kubernetes.azure.com/scalesetpriority"
	// PriorityLabel is the VM priority, "spot" or "regular".
	PriorityLabel = "kubernetes.azure.com/priority"
	scheme        = "azure"
)

// Rule names.
const (
	RulePodIPExhaustion = "pod-ip-exhaustion"
	RuleQuotaExceeded   = "quota-exceeded"
	RuleSpotEviction    = "spot-eviction"
)

// Symptom reasons.
const (
	ReasonPodIPExhaustion    = "PodIPExhaustion"
	ReasonAzureQuotaExceeded = "AzureQuotaExceeded"
	ReasonSpotEviction       = "SpotEviction"
)

// Platform is the AKS platform module.
type Platform struct{}

// Name returns "aks".
func (Platform) Name() string { return Name }

// Detection claims nodes with the AKS cluster label and, when a provider ID is set, the azure scheme.
func (Platform) Detection() modules.PlatformDetection {
	return modules.PlatformDetection{ProviderIDScheme: scheme, NodeLabels: []string{ClusterLabel}}
}

// Hooks returns the AKS diagnosis rules.
func (Platform) Hooks() modules.Hooks {
	return modules.Hooks{Rules: []modules.DiagnosisRule{
		{Name: RulePodIPExhaustion, Event: podIPExhaustion},
		{Name: RuleQuotaExceeded, Event: quotaExceeded},
		{Name: RuleSpotEviction, Event: spotEvictionEvent, Node: spotEvictionNode},
	}}
}

var (
	// ipExhaustedRe matches the Azure CNI and CNS messages of a node or pod subnet without free IPs.
	ipExhaustedRe = regexp.MustCompile(`(?i)not enough IPs available|failed to allocate address`)
	subnetSizeRe  = regexp.MustCompile(`\bInsufficientSubnetSize\b`)
	// quotaRe matches Azure's quota error codes; the optional "ErrCode_" prefix is not part of the code.
	quotaRe = regexp.MustCompile(`\b(?:ErrCode_)?(QuotaExceeded|InsufficientVCPUQuota)\b`)
	// Parts of a QuotaExceeded message: "exceeding approved standardDSv3Family Cores quota", "Location: eastus",
	// "Current Limit: 1500, Current Usage: 1500, Additional Required: 16".
	quotaNameRe = regexp.MustCompile(`exceeding approved ([A-Za-z0-9 ]{1,64}?) (?:Cores )?quota`)
	locationRe  = regexp.MustCompile(`Location: ([a-z0-9]{1,40})\b`)
	numbersRe   = regexp.MustCompile(`(Current Limit|Current Usage|Additional Required): ([0-9]{1,9})\b`)
	preemptRe   = regexp.MustCompile(`(?i)\bpreempt`)
)

const (
	conditionVMEventScheduled = "VMEventScheduled"
	nodeEventNamespace        = "default"
)

func eventEvidence(ev modules.Event) []modules.Evidence {
	return []modules.Evidence{
		{Field: modules.EvidenceEventReason, Value: ev.Reason},
		{Field: modules.EvidenceEventReportingController, Value: ev.ReportingController},
		{Field: modules.EvidenceEventNote, Value: ev.Note},
	}
}

// podIPExhaustion finds a pod that could not get an IP address.
func podIPExhaustion(ev modules.Event) (modules.Symptom, bool) {
	sandbox := ev.Reason == "FailedCreatePodSandBox" && ipExhaustedRe.MatchString(ev.Note)
	if !sandbox && !subnetSizeRe.MatchString(ev.Note) {
		return modules.Symptom{}, false
	}
	summary := "The Azure CNI could not give the pod an IP address: the node's pod IPs or the subnet are used up"
	if !sandbox {
		summary = "AKS refused to add nodes because the subnet has too few free IP addresses (InsufficientSubnetSize)"
	}
	return modules.Symptom{Reason: ReasonPodIPExhaustion, Summary: summary, Evidence: eventEvidence(ev)}, true
}

// quotaExceeded finds a scale-up that Azure refused for quota. Its evidence is at most modules.MaxEvidence
// entries in order of value: the reason, the error code and the note first, then what the message names.
func quotaExceeded(ev modules.Event) (modules.Symptom, bool) {
	code := quotaRe.FindStringSubmatch(ev.Note)
	if code == nil {
		return modules.Symptom{}, false
	}
	evidence := []modules.Evidence{
		{Field: modules.EvidenceEventReason, Value: ev.Reason},
		{Field: "azure.errorCode", Value: code[1]},
		{Field: modules.EvidenceEventNote, Value: ev.Note},
	}
	if m := quotaNameRe.FindStringSubmatch(ev.Note); m != nil {
		evidence = append(evidence, modules.Evidence{Field: "azure.quota", Value: m[1]})
	}
	if m := locationRe.FindStringSubmatch(ev.Note); m != nil {
		evidence = append(evidence, modules.Evidence{Field: "azure.location", Value: m[1]})
	}
	for _, m := range numbersRe.FindAllStringSubmatch(ev.Note, 3) {
		evidence = append(evidence, modules.Evidence{Field: "azure." + strings.ReplaceAll(m[1], " ", ""), Value: m[2]})
	}
	if len(evidence) < modules.MaxEvidence {
		evidence = append(evidence, modules.Evidence{Field: modules.EvidenceEventReportingController, Value: ev.ReportingController})
	}
	return modules.Symptom{
		Reason:   ReasonAzureQuotaExceeded,
		Summary:  "Azure refused new node capacity because a subscription vCPU quota is used up",
		Evidence: evidence[:min(len(evidence), modules.MaxEvidence)],
	}, true
}

const spotSummary = "Azure scheduled the eviction of this Spot VM: its pods are drained and the node is deleted"

// spotEvictionEvent finds the PreemptScheduled event about a node, in the namespace where events about
// nodes are written.
func spotEvictionEvent(ev modules.Event) (modules.Symptom, bool) {
	if ev.Reason != "PreemptScheduled" || ev.Regarding.Kind != "Node" || ev.Regarding.Name == "" ||
		ev.Namespace != nodeEventNamespace {
		return modules.Symptom{}, false
	}
	return modules.Symptom{Reason: ReasonSpotEviction, Summary: spotSummary, Evidence: eventEvidence(ev)}, true
}

// spotEvictionNode finds a node whose VMEventScheduled condition names a Preempt event.
func spotEvictionNode(n modules.NodeState) []modules.Symptom {
	for _, c := range n.Conditions {
		if c.Type != conditionVMEventScheduled || c.Status != "True" {
			continue
		}
		if !preemptRe.MatchString(c.Reason) && !preemptRe.MatchString(c.Message) {
			return nil
		}
		evidence := []modules.Evidence{
			{Field: "node.condition.VMEventScheduled.reason", Value: c.Reason},
			{Field: "node.condition.VMEventScheduled.message", Value: c.Message},
		}
		for _, l := range []string{PriorityLabel, ScaleSetPriorityLabel} {
			if v, ok := n.Labels[l]; ok {
				evidence = append(evidence, modules.Evidence{Field: "node.label." + l, Value: v})
			}
		}
		return []modules.Symptom{{Reason: ReasonSpotEviction, Summary: spotSummary, Evidence: evidence}}
	}
	return nil
}
