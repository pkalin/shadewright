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

import "github.com/pkalin/shadewright/internal/modules"

// MaxReportResults is the most failing results one policy report gives as symptoms: the first ones in the
// report's order, which Kyverno sorts by policy and rule (kyverno v1.19.2 pkg/utils/report/results.go,
// SortReportResults). A report on a resource checked by many policies can hold thousands of results; the rest
// are not read.
const MaxReportResults = 32

// Reasons the rules write. ReasonPolicyViolation and ReasonPolicyError are the event reasons Kyverno itself uses
// for a rule that failed and a rule that could not be evaluated (kyverno v1.19.2 pkg/event/reason.go); the
// reports controller emits them for the same results it writes to a report
// (pkg/controllers/report/utils/events.go). ReasonPolicyNotReady and ReasonAdmissionRefused are written here:
// Kyverno's own reasons for them (Failed on the Ready condition, PolicyViolation on the event) are evidence.
const (
	ReasonPolicyViolation  = "PolicyViolation"
	ReasonPolicyError      = "PolicyError"
	ReasonPolicyNotReady   = "PolicyNotReady"
	ReasonAdmissionRefused = "AdmissionRefused"
)

// Fixed summaries. Each is written here, never taken from the cluster. None suggests changing the policy: the
// module only explains what Kyverno decided (design 2.5).
const (
	summaryViolation = "Kyverno reports that this resource fails a rule of a policy: the resource does not meet what the rule requires."
	summaryError     = "Kyverno could not evaluate a rule of a policy for this resource and reports an error for it."
	summaryNotReady  = "Kyverno reports this policy not Ready: its admission webhooks do not serve it yet, so requests may not be checked against it."
)

// Values Kyverno writes, read from kyverno v1.19.2.
const (
	// conditionReady is the policy condition (api/kyverno/v1/policy_status.go, PolicyConditionReady).
	conditionReady = "Ready"
	// resultFail and resultError are the policy report results of a rule that failed and of one that could not
	// be evaluated (api/policyreport/v1alpha2/common.go, StatusFail and StatusError). pass, warn and skip show
	// nothing.
	resultFail  = "fail"
	resultError = "error"
)

// kyvernoSource reports whether results[].source names a Kyverno policy (kyverno v1.19.2
// pkg/utils/report/source.go, set in pkg/utils/report/results.go): "kyverno" (kyverno.ValueKyvernoApp, for
// ClusterPolicy and Policy) and the policies.kyverno.io kinds. Two values Kyverno also writes are left out on
// purpose: ValidatingAdmissionPolicy and MutatingAdmissionPolicy are Kubernetes' own policies, which Kyverno
// only reports on, so a summary about "a policy Kyverno enforces" would misattribute them. A result from any
// other engine that writes the same report kinds is not read either.
func kyvernoSource(source string) bool {
	switch source {
	case "kyverno", "KyvernoValidatingPolicy", "KyvernoImageValidatingPolicy", "KyvernoGeneratingPolicy",
		"KyvernoMutatingPolicy":
		return true
	}
	return false
}

// Evidence fields of a policy report result.
const (
	fieldPolicy   = "results.policy"
	fieldRule     = "results.rule"
	fieldMessage  = "results.message"
	fieldSeverity = "results.severity"
	// The resource a result applies to is read from results[].resources[0] when it is set, otherwise from the
	// report's scope, which is where Kyverno names it (pkg/utils/report/results.go ToPolicyReportResult passes no
	// resource, pkg/utils/report/new.go NewPolicyReport sets the scope).
	prefixResultResource = "results.resources"
	prefixScope          = "scope"
)

// clusterPolicySymptoms and policySymptoms read a ClusterPolicy or a Policy.
func clusterPolicySymptoms(obj map[string]any) []modules.Symptom {
	return policyConditionSymptoms(obj, KindClusterPolicy)
}

func policySymptoms(obj map[string]any) []modules.Symptom {
	return policyConditionSymptoms(obj, KindPolicy)
}

// policyConditionSymptoms reports a policy whose first Ready condition is False. Kyverno sets no
// observedGeneration on it (api/kyverno/v1/policy_status.go SetReady), so none is compared.
func policyConditionSymptoms(obj map[string]any, kind string) []modules.Symptom {
	if !isKind(obj, PolicyGroup+"/"+PolicyVersion, kind) {
		return nil
	}
	status, _ := obj["status"].(map[string]any)
	cond := modules.FirstCondition(status, conditionReady)
	if st, _ := cond["status"].(string); st != "False" {
		return nil
	}
	s := modules.Symptom{Reason: ReasonPolicyNotReady, Summary: summaryNotReady}
	s.Evidence = appendString(s.Evidence, "status.conditions.Ready.reason", cond["reason"])
	s.Evidence = appendString(s.Evidence, "status.conditions.Ready.message", cond["message"])
	return []modules.Symptom{s}
}

// policyReportSymptoms and clusterPolicyReportSymptoms read a PolicyReport or a ClusterPolicyReport.
func policyReportSymptoms(obj map[string]any) []modules.Symptom {
	return reportSymptoms(obj, KindPolicyReport)
}

func clusterPolicyReportSymptoms(obj map[string]any) []modules.Symptom {
	return reportSymptoms(obj, KindClusterPolicyReport)
}

// reportSymptoms gives one symptom per result that Kyverno wrote with result fail or error, in the report's
// order, at most MaxReportResults. Evidence is the policy, the rule, the resource the result applies to, the
// message and the severity, each copied as it is and left for the evaluator to redact and bound.
func reportSymptoms(obj map[string]any, kind string) []modules.Symptom {
	if !isKind(obj, ReportGroup+"/"+ReportVersion, kind) {
		return nil
	}
	results, _ := obj["results"].([]any)
	scope, _ := obj["scope"].(map[string]any)
	var out []modules.Symptom
	for _, r := range results {
		res, _ := r.(map[string]any)
		if src, _ := res["source"].(string); !kyvernoSource(src) {
			continue
		}
		var s modules.Symptom
		switch v, _ := res["result"].(string); v {
		case resultFail:
			s = modules.Symptom{Reason: ReasonPolicyViolation, Summary: summaryViolation}
		case resultError:
			s = modules.Symptom{Reason: ReasonPolicyError, Summary: summaryError}
		default:
			continue
		}
		s.Evidence = appendString(s.Evidence, fieldPolicy, res["policy"])
		s.Evidence = appendString(s.Evidence, fieldRule, res["rule"])
		resources, _ := res["resources"].([]any)
		if len(resources) > 0 {
			ref, _ := resources[0].(map[string]any)
			s.Evidence = appendRef(s.Evidence, prefixResultResource, ref)
		} else {
			s.Evidence = appendRef(s.Evidence, prefixScope, scope)
		}
		s.Evidence = appendString(s.Evidence, fieldMessage, res["message"])
		s.Evidence = appendString(s.Evidence, fieldSeverity, res["severity"])
		out = append(out, s)
		if len(out) == MaxReportResults {
			break
		}
	}
	return out
}

// isKind reports whether obj has the given apiVersion and kind.
func isKind(obj map[string]any, apiVersion, kind string) bool {
	av, _ := obj["apiVersion"].(string)
	k, _ := obj["kind"].(string)
	return av == apiVersion && k == kind
}

// appendRef adds the apiVersion, kind, namespace and name of an object reference that are set, at most four
// entries.
func appendRef(ev []modules.Evidence, prefix string, ref map[string]any) []modules.Evidence {
	for _, f := range []string{"apiVersion", "kind", "namespace", "name"} {
		ev = appendString(ev, prefix+"."+f, ref[f])
	}
	return ev
}

// appendString adds an evidence entry when v is a non-empty string.
func appendString(ev []modules.Evidence, field string, v any) []modules.Evidence {
	if s, _ := v.(string); s != "" {
		ev = append(ev, modules.Evidence{Field: field, Value: s})
	}
	return ev
}
