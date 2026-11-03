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

//nolint:goconst // the tables spell their values out to read as recorded objects
package aks

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/testutil/importguard"
)

// TestNoVendorImports enforces ADR-0013: no Azure SDK in a platform module.
func TestNoVendorImports(t *testing.T) {
	importguard.DirectImportsOnlyStandardOrModule(t, ".")
	importguard.NoForbiddenDeps(t, ".", ".")
}

// active returns a selection with aks enabled and detected.
func active(t *testing.T) modules.Selection {
	t.Helper()
	r, err := modules.New([]modules.Platform{Platform{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := r.Detect([]modules.Node{{ProviderID: "azure:///subscriptions/s/vm-0", Labels: map[string]string{ClusterLabel: ""}}}, nil)
	s := r.Select(modules.Enabled{Platforms: []string{Name}}, d)
	if s.ActivePlatform() != Name {
		t.Fatalf("active platform = %q, want aks", s.ActivePlatform())
	}
	return s
}

// ofRule returns the findings of one aks rule.
func ofRule(fs []modules.Finding, rule string) []modules.Finding {
	var out []modules.Finding
	for _, f := range fs {
		if f.Module == Name && f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}

func value(f modules.Finding, field string) string {
	for _, e := range f.Evidence {
		if e.Field == field {
			return e.Value
		}
	}
	return ""
}

func TestDetection(t *testing.T) {
	r, err := modules.New([]modules.Platform{Platform{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		node modules.Node
		want string
	}{
		{"AKS node", modules.Node{ProviderID: "azure:///subscriptions/s/vm-0", Labels: map[string]string{ClusterLabel: "", "kubernetes.azure.com/agentpool": ""}}, Name},
		{"Azure VM without the AKS label (Cluster API, kubeadm)", modules.Node{ProviderID: "azure:///subscriptions/s/vm-0", Labels: map[string]string{"kubernetes.io/os": ""}}, modules.GenericPlatform},
		{"AKS label on another cloud", modules.Node{ProviderID: "aws:///eu-west-1a/i-0", Labels: map[string]string{ClusterLabel: ""}}, modules.GenericPlatform},
		{"Kind node", modules.Node{ProviderID: "kind://docker/kind/kind-control-plane", Labels: map[string]string{"kubernetes.io/hostname": ""}}, modules.GenericPlatform},
	}
	for _, c := range cases {
		if got := r.DetectPlatform([]modules.Node{c.node}).Name(); got != c.want {
			t.Errorf("%s: DetectPlatform = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPodIPExhaustion(t *testing.T) {
	pod := modules.ObjectRef{APIVersion: "v1", Kind: "Pod", Namespace: "demo", Name: "web-1"}
	cases := []struct {
		name string
		ev   modules.Event
		want bool
	}{
		{"CNS dynamic allocation", modules.Event{Reason: "FailedCreatePodSandBox", ReportingController: "kubelet", Regarding: pod,
			Note: `Failed to create pod sandbox: rpc error: code = Unknown desc = failed to setup network for sandbox "4f2c": plugin type="azure-vnet" failed (add): IPAM Invoker Add failed with error: Failed to get IP address from CNS: not enough IPs available of type Pod for 8a1c, waiting on Azure CNS to allocate more`}, true},
		{"failed to allocate address", modules.Event{Reason: "FailedCreatePodSandBox", Regarding: pod,
			Note: "Failed to create pod sandbox: plugin type=\"azure-vnet\" failed (add): Failed to allocate address: Failed to delegate: Failed to allocate address: No available addresses"}, true},
		{"InsufficientSubnetSize from a scale-up", modules.Event{Reason: "FailedToScaleUpGroup", ReportingController: "cluster-autoscaler",
			Note: "Code: InsufficientSubnetSize; Message: Pre-allocated IPs 270 exceeds IPs available 251 in Subnet Cidr 10.240.0.0/24"}, true},
		{"other sandbox failure", modules.Event{Reason: "FailedCreatePodSandBox", Regarding: pod,
			Note: "Failed to create pod sandbox: rpc error: code = DeadlineExceeded"}, false},
		{"IP text with another reason", modules.Event{Reason: "BackOff", Regarding: pod, Note: "not enough IPs available"}, false},
	}
	for _, c := range cases {
		fs := ofRule(active(t).EvaluateEvent(c.ev), RulePodIPExhaustion)
		if got := len(fs) == 1; got != c.want {
			t.Errorf("%s: matched = %v, want %v (%+v)", c.name, got, c.want, fs)
			continue
		}
		if c.want && (fs[0].Reason != ReasonPodIPExhaustion || value(fs[0], modules.EvidenceEventNote) == "") {
			t.Errorf("%s: finding = %+v", c.name, fs[0])
		}
	}
}

func TestQuotaExceeded(t *testing.T) {
	note := "Code: QuotaExceeded Message: Operation could not be completed as it results in exceeding approved " +
		"standardDSv3Family Cores quota. Additional details - Deployment Model: Resource Manager, Location: " +
		"swedencentral, Current Limit: 10, Current Usage: 8, Additional Required: 4, (Minimum) New Limit Required: 12."
	ev := modules.Event{Reason: "FailedToScaleUpGroup", ReportingController: "cluster-autoscaler", Note: note}
	fs := ofRule(active(t).EvaluateEvent(ev), RuleQuotaExceeded)
	if len(fs) != 1 || fs[0].Reason != ReasonAzureQuotaExceeded {
		t.Fatalf("findings = %+v", fs)
	}
	for field, want := range map[string]string{
		"azure.errorCode": "QuotaExceeded", "azure.quota": "standardDSv3Family", "azure.location": "swedencentral",
		"azure.CurrentLimit": "10", "azure.CurrentUsage": "8", "azure.AdditionalRequired": "4",
	} {
		if got := value(fs[0], field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	// The documented message fills every evidence slot; the note must still be there.
	if !strings.Contains(value(fs[0], modules.EvidenceEventNote), "exceeding approved standardDSv3Family") {
		t.Errorf("event.note is missing or wrong: %+v", fs[0].Evidence)
	}
	if len(fs[0].Evidence) > modules.MaxEvidence {
		t.Errorf("%d evidence items", len(fs[0].Evidence))
	}
	short := ofRule(active(t).EvaluateEvent(modules.Event{Reason: "FailedToScaleUpGroup",
		ReportingController: "cluster-autoscaler", Note: "Code: QuotaExceeded"}), RuleQuotaExceeded)
	if len(short) != 1 || value(short[0], modules.EvidenceEventReportingController) != "cluster-autoscaler" {
		t.Errorf("a short message keeps the reporter: %+v", short)
	}
	// The ErrCode_ prefix is not part of the code: both spellings give one value.
	for _, note := range []string{
		"Code: ErrCode_InsufficientVCPUQuota Message: Insufficient vcpu quota requested 48, remaining 32",
		"Code: InsufficientVCPUQuota Message: Insufficient vcpu quota requested 48, remaining 32",
	} {
		f := ofRule(active(t).EvaluateEvent(modules.Event{Reason: "FailedToScaleUpGroup", Note: note}), RuleQuotaExceeded)
		if len(f) != 1 || value(f[0], "azure.errorCode") != "InsufficientVCPUQuota" {
			t.Errorf("%q: errorCode = %+v", note, f)
		}
	}

	cases := []struct {
		name string
		note string
		want bool
	}{
		{"vCPU quota", "Code: ErrCode_InsufficientVCPUQuota Message: Insufficient regional vcpu quota left for location swedencentral", true},
		{"allocation failure is not quota", "Code: AllocationFailed Message: Allocation failed", false},
		{"the word quota alone", "pod exceeded its ResourceQuota", false},
	}
	for _, c := range cases {
		fs := ofRule(active(t).EvaluateEvent(modules.Event{Reason: "FailedToScaleUpGroup", Note: c.note}), RuleQuotaExceeded)
		if got := len(fs) == 1; got != c.want {
			t.Errorf("%s: matched = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSpotEviction(t *testing.T) {
	node := modules.ObjectRef{APIVersion: "v1", Kind: "Node", Name: "aks-spot-12345678-vmss000002"}
	evCases := []struct {
		name string
		ev   modules.Event
		want bool
	}{
		{"PreemptScheduled", modules.Event{Namespace: "default", Reason: "PreemptScheduled", ReportingController: "custom-plugin-monitor",
			Regarding: node, Note: "VM is scheduled for Preempt event at Mon, 03 Nov 2026 10:00:00 GMT"}, true},
		{"PreemptScheduled in a workload namespace", modules.Event{Namespace: "demo", Reason: "PreemptScheduled",
			Regarding: node}, false},
		{"PreemptScheduled about a pod", modules.Event{Namespace: "default", Reason: "PreemptScheduled",
			Regarding: modules.ObjectRef{Kind: "Pod", Name: "p"}}, false},
		{"RebootScheduled", modules.Event{Namespace: "default", Reason: "RebootScheduled", Regarding: node}, false},
	}
	for _, c := range evCases {
		fs := ofRule(active(t).EvaluateEvent(c.ev), RuleSpotEviction)
		if got := len(fs) == 1 && fs[0].Reason == ReasonSpotEviction; got != c.want {
			t.Errorf("%s: matched = %v, want %v", c.name, got, c.want)
		}
	}

	spot := map[string]string{ClusterLabel: "mc_rg", ScaleSetPriorityLabel: "spot", PriorityLabel: "spot"}
	nodeCases := []struct {
		name string
		n    modules.NodeState
		want bool
	}{
		{"preempt condition on a Spot node", modules.NodeState{Name: node.Name, Labels: spot, Conditions: []modules.NodeCondition{
			{Type: "VMEventScheduled", Status: "True", Reason: "VMEventScheduled", Message: "VM Preempt event scheduled"}}}, true},
		{"reboot condition", modules.NodeState{Name: node.Name, Labels: spot, Conditions: []modules.NodeCondition{
			{Type: "VMEventScheduled", Status: "True", Reason: "VMEventScheduled", Message: "VM Reboot event scheduled"}}}, false},
		{"no event scheduled", modules.NodeState{Name: node.Name, Labels: spot, Conditions: []modules.NodeCondition{
			{Type: "VMEventScheduled", Status: "False", Reason: "NoVMEventScheduled", Message: "VM has no scheduled event"}}}, false},
	}
	for _, c := range nodeCases {
		fs := ofRule(active(t).EvaluateNode(c.n), RuleSpotEviction)
		if got := len(fs) == 1; got != c.want {
			t.Errorf("%s: matched = %v, want %v", c.name, got, c.want)
			continue
		}
		if c.want && (fs[0].Subject != node || value(fs[0], "node.label."+PriorityLabel) != "spot" || value(fs[0], "node.label."+ScaleSetPriorityLabel) != "spot") {
			t.Errorf("%s: finding = %+v", c.name, fs[0])
		}
	}
}

// TestRulesNeedTheModuleActive checks that a detected but not enabled AKS adds no AKS finding.
func TestRulesNeedTheModuleActive(t *testing.T) {
	r, err := modules.New([]modules.Platform{Platform{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := r.Detect([]modules.Node{{ProviderID: "azure:///x", Labels: map[string]string{ClusterLabel: ""}}}, nil)
	s := r.Select(modules.Enabled{}, d)
	fs := s.EvaluateEvent(modules.Event{Reason: "FailedCreatePodSandBox", Note: "not enough IPs available",
		Regarding: modules.ObjectRef{Kind: "Pod", Name: "p"}})
	for _, f := range fs {
		if f.Module == Name {
			t.Errorf("finding of a module that is not enabled: %+v", f)
		}
	}
	if s.DetectedPlatform() != Name || s.ActivePlatform() != modules.GenericPlatform {
		t.Errorf("detected %q, active %q", s.DetectedPlatform(), s.ActivePlatform())
	}
}
