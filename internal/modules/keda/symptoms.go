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

package keda

import (
	"fmt"
	"strconv"
	"time"

	"github.com/pkalin/shadewright/internal/kedaready"
	"github.com/pkalin/shadewright/internal/modules"
)

// Condition types and statuses the rules read. KEDA's come from internal/kedaready, the one definition the
// policy gate uses too; the HPA's are those of Kubernetes v1.37.2 pkg/controller/podautoscaler/horizontal.go.
const (
	conditionReady          = kedaready.ConditionReady
	conditionFallback       = kedaready.ConditionFallback
	conditionPaused         = kedaready.ConditionPaused
	conditionHPAActive      = kedaready.ConditionHPAActive
	conditionScalingLimited = "ScalingLimited"
	conditionScalingActive  = "ScalingActive"

	statusTrue    = kedaready.StatusTrue
	statusFalse   = kedaready.StatusFalse
	statusUnknown = kedaready.StatusUnknown
)

// KEDA reasons the rules write as they are, read from kedacore/keda v2.21.0 (see [notReadyReasons] for the
// files).
const (
	// ReasonTriggerError: Ready=False, no trigger is active and at least one fails; for a ScaledObject, no
	// fallback is set.
	ReasonTriggerError = "TriggerError"
	// ReasonPartialTriggerError: Ready=Unknown, some triggers fail while another one is active.
	ReasonPartialTriggerError = kedaready.ReasonPartialTriggerError
	// ReasonFallbackExists: Fallback=True, a trigger has failed more often than spec.fallback.failureThreshold
	// (pkg/fallback/fallback.go).
	ReasonFallbackExists = "FallbackExists"
	// ReasonHPAMetricsUnavailable: HPAActive=False on a ScaledObject, the HPA cannot read its metrics and does
	// not scale, while Ready may still be True. KEDA mirrors the HPA's ScalingActive condition into HPAActive
	// and passes the HPA's reason through (pkg/scaling/executor/scale_scaledobjects.go, getHPAHealth);
	// HPAMetricsUnavailable is KEDA's own reason for it (apis/keda/v1alpha1/condition_types.go,
	// ScaledObjectConditionHPAMetricsUnavailableReason), used here for every HPAActive=False. The HPA's reason
	// is evidence.
	ReasonHPAMetricsUnavailable = "HPAMetricsUnavailable"
)

// Reasons the rules write themselves.
const (
	// ReasonNotReady is a Ready=False whose KEDA reason the package does not know; that reason is evidence.
	ReasonNotReady = "NotReady"
	// ReasonReplicaCeiling is an HPA owned by a ScaledObject that runs at its maximum replica count while its
	// metrics ask for more (HPA ScalingLimited=True, reason TooManyReplicas).
	ReasonReplicaCeiling = "ReplicaCeiling"
)

// hpaTooManyReplicas is the ScalingLimited reason the HPA controller sets when the replica count it computed
// is above maxReplicas (Kubernetes v1.37.2 pkg/controller/podautoscaler/horizontal.go,
// convertDesiredReplicasWithBehaviorRate and convertDesiredReplicasWithRules).
const hpaTooManyReplicas = "TooManyReplicas"

// Fixed summaries. Each is written here, never taken from the cluster, and none says what to change: a
// trigger error in particular is never answered by raising replicas (design 7). The trigger error symptoms
// (TriggerError, PartialTriggerError, FallbackExists, HPAMetricsUnavailable) carry no replica count and no
// metric.
const (
	summaryTriggerErrorObject    = "No trigger of this ScaledObject is active and at least one fails, and no fallback is set: KEDA cannot read the metrics of the failing triggers."
	summaryTriggerErrorJob       = "No trigger of this ScaledJob is active and at least one fails: KEDA cannot read the metrics of the failing triggers."
	summaryPartialTriggerError   = "At least one trigger of this object fails while another one is active: KEDA scales only on the triggers that answer."
	summaryFallbackExists        = "A trigger of this ScaledObject has failed more often than its fallback threshold: KEDA scales to the fallback replica count, not on the metric."
	summaryHPAMetricsUnavailable = "The HPA that KEDA manages for this ScaledObject cannot read its metrics, so it does not scale the target, even if the ScaledObject reports Ready."
	summaryNotReady              = "KEDA reports this object not Ready."
	summaryReplicaCeiling        = "The HPA that KEDA manages for this ScaledObject runs at its maximum replica count while its metrics ask for more replicas. The ScaledObject itself is not checked here."
)

// notReadyReasons maps, per kind, the Ready=False reasons KEDA sets to a fixed summary. They were read from
// kedacore/keda v2.21.0:
//
//   - ScaledObject: controllers/keda/scaledobject_controller.go (ScaledObjectCheckFailed) and
//     pkg/scaling/executor/scale_scaledobjects.go (TriggerError, ErrorGettingCurrentReplicas,
//     ErrorScalingTarget).
//   - ScaledJob: controllers/keda/scaledjob_controller.go (ScaledJobCheckFailed),
//     pkg/scaling/executor/scale_jobs.go (TriggerError, and KEDAJobCreateFailed from pkg/eventreason).
//
// PartialTriggerError (Ready=Unknown, both kinds, the same executor files), FallbackExists (Fallback=True)
// and HPAMetricsUnavailable (HPAActive=False) are handled on their own. Active (ScalerActive, ScalerNotActive,
// ScalerCooldown, UnknownState) says whether a trigger is active, which is no failure, and shows nothing.
var notReadyReasons = map[string]map[string]string{
	KindScaledObject: {
		ReasonTriggerError:            summaryTriggerErrorObject,
		"ScaledObjectCheckFailed":     "KEDA refused the ScaledObject: its scale target, replica counts or triggers failed the operator's checks, or its HPA could not be created.",
		"ErrorGettingCurrentReplicas": "KEDA could not read the current replica count of the ScaledObject's scale target.",
		"ErrorScalingTarget":          "KEDA could not change the replica count of the ScaledObject's scale target.",
	},
	KindScaledJob: {
		ReasonTriggerError:     summaryTriggerErrorJob,
		"ScaledJobCheckFailed": "KEDA refused the ScaledJob: its spec or triggers failed the operator's checks.",
		"KEDAJobCreateFailed":  "KEDA could not create the Jobs the ScaledJob asks for.",
	},
}

// Evidence fields the rules write besides the conditions.
const (
	fieldTriggersTypes   = "status.triggersTypes"
	fieldHealthFailing   = "status.health.failing"
	fieldHealthFailures  = "status.health.numberOfFailures"
	fieldOwner           = "metadata.ownerReferences.ScaledObject.name"
	fieldMaxReplicas     = "spec.maxReplicas"
	fieldCurrentReplicas = "status.currentReplicas"
	// maxMetrics is how many external metrics a ceiling symptom carries, two entries each, so that the
	// symptom stays within [modules.MaxEvidence].
	maxMetrics = 2
)

// scaledObjectSymptoms reads a ScaledObject.
func scaledObjectSymptoms(obj map[string]any) []modules.Symptom {
	return kedaSymptoms(obj, KindScaledObject)
}

// scaledJobSymptoms reads a ScaledJob. KEDA has no fallback for a ScaledJob, so Fallback is not read.
func scaledJobSymptoms(obj map[string]any) []modules.Symptom {
	return kedaSymptoms(obj, KindScaledJob)
}

// kedaSymptoms applies the ScaledObject and ScaledJob rules of the package doc. Reason and Summary are always
// written here; cluster text goes only into the evidence, which is read from status only. Fallback and
// HPAActive exist on a ScaledObject only.
func kedaSymptoms(obj map[string]any, kind string) []modules.Symptom {
	if !is(obj, Group+"/"+Version, kind) {
		return nil
	}
	status, _ := obj["status"].(map[string]any)
	if conditionIs(status, conditionPaused, statusTrue) {
		return nil
	}
	var s modules.Symptom
	var cond map[string]any
	var typ string
	switch {
	case conditionIs(status, conditionReady, statusFalse):
		cond, typ = kedaready.FirstCondition(status, conditionReady), conditionReady
		reason, _ := cond["reason"].(string)
		s = modules.Symptom{Reason: ReasonNotReady, Summary: summaryNotReady}
		if summary, ok := notReadyReasons[kind][reason]; ok {
			s = modules.Symptom{Reason: reason, Summary: summary}
		}
	case kind == KindScaledObject && conditionIs(status, conditionFallback, statusTrue):
		cond, typ = kedaready.FirstCondition(status, conditionFallback), conditionFallback
		s = modules.Symptom{Reason: ReasonFallbackExists, Summary: summaryFallbackExists}
	case kind == KindScaledObject && conditionIs(status, conditionHPAActive, statusFalse):
		cond, typ = kedaready.FirstCondition(status, conditionHPAActive), conditionHPAActive
		s = modules.Symptom{Reason: ReasonHPAMetricsUnavailable, Summary: summaryHPAMetricsUnavailable}
	case conditionIs(status, conditionReady, statusUnknown) &&
		kedaready.FirstCondition(status, conditionReady)["reason"] == ReasonPartialTriggerError:
		cond, typ = kedaready.FirstCondition(status, conditionReady), conditionReady
		s = modules.Symptom{Reason: ReasonPartialTriggerError, Summary: summaryPartialTriggerError}
	default:
		return nil
	}
	s.Evidence = conditionEvidence(cond, typ)
	// The trigger type names. status.authenticationsTypes is left out: it holds the names people gave their
	// TriggerAuthentications, which the diagnosis does not need.
	if v, _ := status["triggersTypes"].(string); v != "" {
		s.Evidence = append(s.Evidence, modules.Evidence{Field: fieldTriggersTypes, Value: v})
	}
	s.Evidence = append(s.Evidence, healthEvidence(status)...)
	return []modules.Symptom{s}
}

// healthEvidence counts the triggers status.health marks Failing and gives the highest numberOfFailures among
// them. Only numbers are read: the keys of status.health are metric names, which KEDA derives from trigger
// metadata, so they are left out.
func healthEvidence(status map[string]any) []modules.Evidence {
	health, _ := status["health"].(map[string]any)
	failing, most := 0, int64(0)
	for _, h := range health {
		entry, _ := h.(map[string]any)
		if st, _ := entry["status"].(string); st != "Failing" {
			continue
		}
		failing++
		if n, ok := modules.JSONInteger(entry["numberOfFailures"]); ok && n > most {
			most = n
		}
	}
	if failing == 0 {
		return nil
	}
	return []modules.Evidence{
		{Field: fieldHealthFailing, Value: strconv.Itoa(failing)},
		{Field: fieldHealthFailures, Value: strconv.FormatInt(most, 10)},
	}
}

// hpaSymptoms reads a HorizontalPodAutoscaler and reports a replica ceiling for one that a ScaledObject
// controls: ScalingLimited=True with reason TooManyReplicas, currentReplicas at or above spec.maxReplicas, a
// status for the current spec, and no ScalingActive=False (when metrics fail, the HPA stops updating
// ScalingLimited, so an old TooManyReplicas may remain). Any other HPA shows nothing. The source sees the HPA
// only, not the owning ScaledObject, so the symptom is a candidate: a ScaledObject paused for scale-in or
// scale-out only, or on fallback, still has an HPA that may report the limit. The policy gate for
// AdjustKedaMaxReplicas (#228) checks the ScaledObject itself.
func hpaSymptoms(obj map[string]any) []modules.Symptom {
	if !is(obj, AutoscalingGroup+"/"+AutoscalingVersion, KindHPA) {
		return nil
	}
	meta, _ := obj["metadata"].(map[string]any)
	owner := scaledObjectOwner(meta)
	if owner == "" {
		return nil
	}
	spec, _ := obj["spec"].(map[string]any)
	status, _ := obj["status"].(map[string]any)
	generation, hasGeneration := modules.JSONInteger(meta["generation"])
	if observed, ok := modules.JSONInteger(status["observedGeneration"]); hasGeneration && ok && observed != generation {
		return nil
	}
	if conditionIs(status, conditionScalingActive, statusFalse) || !conditionIs(status, conditionScalingLimited, statusTrue) {
		return nil
	}
	limited := kedaready.FirstCondition(status, conditionScalingLimited)
	if limited["reason"] != hpaTooManyReplicas {
		return nil
	}
	maxReplicas, okMax := modules.JSONInteger(spec["maxReplicas"])
	current, okCurrent := modules.JSONInteger(status["currentReplicas"])
	if !okMax || !okCurrent || current < maxReplicas {
		return nil
	}
	s := modules.Symptom{Reason: ReasonReplicaCeiling, Summary: summaryReplicaCeiling, Evidence: []modules.Evidence{
		{Field: fieldOwner, Value: owner},
		{Field: fieldMaxReplicas, Value: strconv.FormatInt(maxReplicas, 10)},
		{Field: fieldCurrentReplicas, Value: strconv.FormatInt(current, 10)},
		{Field: "status.conditions." + conditionScalingLimited + ".reason", Value: hpaTooManyReplicas},
	}}
	s.Evidence = append(s.Evidence, metricEvidence(spec, status)...)
	s.Since = transitionTime(limited)
	return []modules.Symptom{s}
}

// transitionTime is a condition's lastTransitionTime, zero when it is missing or not an RFC 3339 time.
func transitionTime(cond map[string]any) time.Time {
	v, _ := cond["lastTransitionTime"].(string)
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// claimsHPA reports whether a ScaledObject names the HPA called name as its own: KEDA writes the name of the
// HPA it manages to status.hpaName (apis/keda/v1alpha1/scaledobject_types.go at v2.21.0, set in
// controllers/keda/hpa.go). The owner reference alone is written by whoever can write the HPA; the
// ScaledObject's status is written by KEDA. A ScaledObject whose status.hpaName is missing or empty has not
// been reconciled yet (KEDA sets it when it creates the HPA), so the claim is pending; any other value, or a
// value that is not a string, is no claim.
func claimsHPA(owner map[string]any, name string) modules.Claim {
	if !is(owner, Group+"/"+Version, KindScaledObject) || name == "" {
		return modules.ClaimNotOwned
	}
	status, _ := owner["status"].(map[string]any)
	v := status["hpaName"]
	if v == nil || v == "" {
		return modules.ClaimPending
	}
	hpaName, ok := v.(string)
	if !ok || hpaName != name {
		return modules.ClaimNotOwned
	}
	return modules.ClaimOwned
}

// scaledObjectOwner returns the name of the ScaledObject in metadata.ownerReferences that controls the HPA,
// or "". KEDA sets itself as the controller of the HPAs it creates (controllers/keda/hpa.go,
// SetControllerReference).
func scaledObjectOwner(meta map[string]any) string {
	refs, _ := meta["ownerReferences"].([]any)
	for _, r := range refs {
		ref, _ := r.(map[string]any)
		controller, _ := ref["controller"].(bool)
		if !controller || ref["apiVersion"] != Group+"/"+Version || ref["kind"] != KindScaledObject {
			continue
		}
		name, _ := ref["name"].(string)
		return name
	}
	return ""
}

// metricEvidence gives, for the first [maxMetrics] external metrics in status.currentMetrics, the current
// value (the backlog the HPA sees) and the target of the matching entry of spec.metrics. Metric names are
// used only to match the two lists and are not evidence: KEDA derives them from trigger metadata.
func metricEvidence(spec, status map[string]any) []modules.Evidence {
	var out []modules.Evidence
	current, _ := status["currentMetrics"].([]any)
	targets, _ := spec["metrics"].([]any)
	n := 0
	for i, m := range current {
		if n == maxMetrics {
			break
		}
		name, cur := externalMetric(m, "current")
		field, value := quantity(cur)
		if name == "" || value == "" {
			continue
		}
		n++
		out = append(out, modules.Evidence{Field: fmt.Sprintf("status.currentMetrics.%d.external.current.%s", i, field), Value: value})
		for j, t := range targets {
			if tname, target := externalMetric(t, "target"); tname == name {
				if tfield, tvalue := quantity(target); tvalue != "" {
					out = append(out, modules.Evidence{Field: fmt.Sprintf("spec.metrics.%d.external.target.%s", j, tfield), Value: tvalue})
				}
				break
			}
		}
	}
	return out
}

// externalMetric returns the metric name and the value block (current or target) of an External metric entry.
func externalMetric(m any, block string) (string, map[string]any) {
	entry, _ := m.(map[string]any)
	if entry["type"] != "External" {
		return "", nil
	}
	external, _ := entry["external"].(map[string]any)
	metric, _ := external["metric"].(map[string]any)
	name, _ := metric["name"].(string)
	values, _ := external[block].(map[string]any)
	return name, values
}

// quantity returns the averageValue of a metric value block, or else its value, with the field it came from.
func quantity(values map[string]any) (string, string) {
	for _, f := range []string{"averageValue", "value"} {
		if v, _ := values[f].(string); v != "" {
			return f, v
		}
	}
	return "", ""
}

// conditionEvidence is the reason and message of the condition the symptom reports.
func conditionEvidence(cond map[string]any, typ string) []modules.Evidence {
	var out []modules.Evidence
	prefix := "status.conditions." + typ
	if r, _ := cond["reason"].(string); r != "" {
		out = append(out, modules.Evidence{Field: prefix + ".reason", Value: r})
	}
	if m, _ := cond["message"].(string); m != "" {
		out = append(out, modules.Evidence{Field: prefix + ".message", Value: m})
	}
	return out
}

// is reports whether obj has the apiVersion and kind.
func is(obj map[string]any, apiVersion, kind string) bool {
	av, _ := obj["apiVersion"].(string)
	k, _ := obj["kind"].(string)
	return av == apiVersion && k == kind
}

// conditionIs reports whether the first condition of the type has the status.
func conditionIs(status map[string]any, typ, want string) bool {
	return kedaready.ConditionIs(status, typ, want)
}
