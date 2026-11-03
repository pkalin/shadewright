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

package kyverno

import (
	"regexp"
	"strings"

	"github.com/pkalin/shadewright/internal/modules"
)

// RuleAdmissionRefused is the name of the diagnosis rule for a refused admission request.
const RuleAdmissionRefused = "admission-refused"

// The event Kyverno writes when its admission webhook refuses a request (kyverno v1.19.2):
// pkg/webhooks/utils/event.go GenerateEvents calls pkg/event/events.go NewPolicyFailEvent with reason
// PolicyViolation (pkg/event/reason.go) and source kyverno-admission (pkg/event/source.go,
// AdmissionController), which pkg/event/controller.go emitEvent writes as reportingController, with type
// Warning for that reason. The event regards the policy, not the refused resource, and relates the resource.
// emitEvent writes it to the policy's namespace, or to "default" for a ClusterPolicy.
const (
	eventReasonPolicyViolation = "PolicyViolation"
	eventTypeWarning           = "Warning"
	clusterEventNamespace      = "default"
)

// summaryAdmissionRefused is the fixed summary of the rule.
const summaryAdmissionRefused = "Kyverno refused an admission request: a resource failed a rule of an enforcing policy, so the API server did not create or change it."

// refusedNote matches the start of the note of a refused request, built by pkg/event/events.go
// buildPolicyEventMessage: "<kind> <namespace>/<name>" or "<kind> <name>", then ": [<rule>] <status> (blocked)",
// then "; <message>", with status fail or error (pkg/engine/api/rulestatus.go). Group 1 is the resource: a
// name holds no space but may hold colons (system:aggregate-to-edit) or be empty. Group 2 is the rule, at most
// 63 characters (api/kyverno/v1/rule_types.go). The match is anchored at the start, so " (blocked)" in the
// message part of an audit note does not count. It runs on untrusted text and only decides whether the note
// says blocked and finds names for evidence.
var refusedNote = regexp.MustCompile(`^([A-Za-z0-9]{1,63} [^ ]{0,512}): \[([^\]]{1,63})\] (?:fail|error) \(blocked\)`)

// Evidence fields of the rule besides the event's reason, reporting controller and note. The policy is the
// finding's subject, so it is not repeated here.
const (
	fieldEventRule        = "kyverno.rule"
	fieldEventResource    = "kyverno.resource"
	prefixRelated         = "event.related"
	fieldRelatedKind      = prefixRelated + ".kind"
	fieldRelatedNamespace = prefixRelated + ".namespace"
	fieldRelatedName      = prefixRelated + ".name"
)

// admissionRefused finds the event of a request Kyverno's admission webhook refused. The policy is the event's
// regarding object, a ClusterPolicy or Policy, which becomes the finding's subject; the rule is read from the
// note. The refused resource is the event's related object when it has a kind and a name that passed
// sanitizing, otherwise the resource part of the note (a name with a colon, such as a ClusterRole's, does not
// pass the related object's pattern). Reason and summary are fixed; the event's text is only evidence.
//
// The event must sit where Kyverno writes it: in "default" for a ClusterPolicy (which has no namespace) and in
// the policy's own namespace for a Policy, so an event forged in another namespace about a policy is refused.
// Kyverno leaves the regarding apiVersion empty when its policy object has no type metadata, so an empty one is
// accepted and only another group is refused.
func admissionRefused(ev modules.Event) (modules.Symptom, bool) {
	if ev.Type != eventTypeWarning || ev.Reason != eventReasonPolicyViolation || ev.ReportingController != eventAdmission {
		return modules.Symptom{}, false
	}
	reg := ev.Regarding
	if reg.Name == "" || reg.APIVersion != "" && !strings.HasPrefix(reg.APIVersion, PolicyGroup+"/") {
		return modules.Symptom{}, false
	}
	switch reg.Kind {
	case KindClusterPolicy:
		if reg.Namespace != "" || ev.Namespace != clusterEventNamespace {
			return modules.Symptom{}, false
		}
	case KindPolicy:
		if reg.Namespace == "" || ev.Namespace != reg.Namespace {
			return modules.Symptom{}, false
		}
	default:
		return modules.Symptom{}, false
	}
	m := refusedNote.FindStringSubmatch(ev.Note)
	if m == nil {
		return modules.Symptom{}, false
	}
	evidence := []modules.Evidence{
		{Field: modules.EvidenceEventReason, Value: ev.Reason},
		{Field: modules.EvidenceEventReportingController, Value: ev.ReportingController},
		{Field: fieldEventRule, Value: m[2]},
	}
	if rel := ev.Related; rel.Kind != "" && rel.Name != "" {
		evidence = append(evidence, modules.Evidence{Field: fieldRelatedKind, Value: rel.Kind})
		if rel.Namespace != "" {
			evidence = append(evidence, modules.Evidence{Field: fieldRelatedNamespace, Value: rel.Namespace})
		}
		evidence = append(evidence, modules.Evidence{Field: fieldRelatedName, Value: rel.Name})
	} else {
		evidence = append(evidence, modules.Evidence{Field: fieldEventResource, Value: m[1]})
	}
	evidence = append(evidence, modules.Evidence{Field: modules.EvidenceEventNote, Value: ev.Note})
	return modules.Symptom{Reason: ReasonAdmissionRefused, Summary: summaryAdmissionRefused, Evidence: evidence}, true
}
