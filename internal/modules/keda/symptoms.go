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
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/pkalin/shadewright/internal/modules"
)

// Condition types and statuses the rules read. KEDA's are those of kedacore/keda v2.21.0
// apis/keda/v1alpha1/condition_types.go (ConditionReady, ConditionActive, ConditionFallback, ConditionPaused);
// the HPA's are those of Kubernetes v1.37.2 pkg/controller/podautoscaler/horizontal.go.
const (
	conditionReady          = "Ready"
	conditionFallback       = "Fallback"
	conditionPaused         = "Paused"
	conditionScalingLimited = "ScalingLimited"
	conditionScalingActive  = "ScalingActive"

	statusTrue    = "True"
	statusFalse   = "False"
	statusUnknown = "Unknown"
)

// KEDA reasons the rules write as they are, read from kedacore/keda v2.21.0 (see [notReadyReasons] for the
// files).
const (
	// ReasonTriggerError: Ready=False, every trigger fails and no fallback is set.
	ReasonTriggerError = "TriggerError"
	// ReasonPartialTriggerError: Ready=Unknown, some triggers fail while another one is active.
	ReasonPartialTriggerError = "PartialTriggerError"
	// ReasonFallbackExists: Fallback=True, a trigger has failed more often than spec.fallback.failureThreshold
	// (pkg/fallback/fallback.go).
	ReasonFallbackExists = "FallbackExists"
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
// trigger error in particular is never answered by raising replicas (design 7).
const (
	summaryTriggerError        = "Every trigger of this object fails and no fallback is set: KEDA cannot read the metrics it scales on."
	summaryPartialTriggerError = "Some triggers of this object fail: KEDA scales only on the triggers that still answer."
	summaryFallbackExists      = "A trigger of this ScaledObject has failed more often than its fallback threshold: KEDA scales to the fallback replica count, not on the metric."
	summaryNotReady            = "KEDA reports this object not Ready."
	summaryReplicaCeiling      = "The HPA that KEDA manages for this ScaledObject runs at its maximum replica count while its metrics ask for more replicas."
)

// triggerErrors are the reasons that mean a trigger fails. Their symptoms carry no replica counts: a broken
// trigger is not a capacity problem (design 7).
var triggerErrors = map[string]bool{
	ReasonTriggerError: true, ReasonPartialTriggerError: true, ReasonFallbackExists: true,
}

// notReadyReasons maps, per kind, the Ready=False reasons KEDA sets to a fixed summary. They were read from
// kedacore/keda v2.21.0:
//
//   - ScaledObject: controllers/keda/scaledobject_controller.go (ScaledObjectCheckFailed) and
//     pkg/scaling/executor/scale_scaledobjects.go (TriggerError, ErrorGettingCurrentReplicas,
//     ErrorScalingTarget).
//   - ScaledJob: controllers/keda/scaledjob_controller.go (ScaledJobCheckFailed),
//     pkg/scaling/executor/scale_jobs.go (TriggerError, and KEDAJobCreateFailed from pkg/eventreason).
//
// PartialTriggerError (Ready=Unknown, both kinds, the same executor files) and FallbackExists (Fallback=True)
// are handled on their own. Active (ScalerActive, ScalerNotActive, ScalerCooldown, UnknownState) says whether
// a trigger is active, which is no failure, and shows nothing.
var notReadyReasons = map[string]map[string]string{
	KindScaledObject: {
		ReasonTriggerError:            summaryTriggerError,
		"ScaledObjectCheckFailed":     "KEDA refused the ScaledObject: its scale target, replica counts or triggers failed the operator's checks, or its HPA could not be created.",
		"ErrorGettingCurrentReplicas": "KEDA could not read the current replica count of the ScaledObject's scale target.",
		"ErrorScalingTarget":          "KEDA could not change the replica count of the ScaledObject's scale target.",
	},
	KindScaledJob: {
		ReasonTriggerError:     summaryTriggerError,
		"ScaledJobCheckFailed": "KEDA refused the ScaledJob: its spec or triggers failed the operator's checks.",
		"KEDAJobCreateFailed":  "KEDA could not create the Jobs the ScaledJob asks for.",
	},
}

// Evidence fields the rules write besides the conditions.
const (
	fieldTriggersTypes   = "status.triggersTypes"
	fieldAuthTypes       = "status.authenticationsTypes"
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
// written here; cluster text goes only into the evidence, which is read from status only.
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
		cond, typ = condition(status, conditionReady), conditionReady
		reason, _ := cond["reason"].(string)
		s = modules.Symptom{Reason: ReasonNotReady, Summary: summaryNotReady}
		if summary, ok := notReadyReasons[kind][reason]; ok {
			s = modules.Symptom{Reason: reason, Summary: summary}
		}
	case kind == KindScaledObject && conditionIs(status, conditionFallback, statusTrue):
		cond, typ = condition(status, conditionFallback), conditionFallback
		s = modules.Symptom{Reason: ReasonFallbackExists, Summary: summaryFallbackExists}
	case conditionIs(status, conditionReady, statusUnknown) &&
		condition(status, conditionReady)["reason"] == ReasonPartialTriggerError:
		cond, typ = condition(status, conditionReady), conditionReady
		s = modules.Symptom{Reason: ReasonPartialTriggerError, Summary: summaryPartialTriggerError}
	default:
		return nil
	}
	s.Evidence = conditionEvidence(cond, typ)
	for _, f := range []struct{ key, field string }{
		{"triggersTypes", fieldTriggersTypes}, {"authenticationsTypes", fieldAuthTypes},
	} {
		if v, _ := status[f.key].(string); v != "" {
			s.Evidence = append(s.Evidence, modules.Evidence{Field: f.field, Value: v})
		}
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
		if n, ok := integer(entry["numberOfFailures"]); ok && n > most {
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
// ScalingLimited, so an old TooManyReplicas may remain). Any other HPA shows nothing.
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
	generation, hasGeneration := integer(meta["generation"])
	if observed, ok := integer(status["observedGeneration"]); hasGeneration && ok && observed != generation {
		return nil
	}
	if conditionIs(status, conditionScalingActive, statusFalse) || !conditionIs(status, conditionScalingLimited, statusTrue) {
		return nil
	}
	limited := condition(status, conditionScalingLimited)
	if limited["reason"] != hpaTooManyReplicas {
		return nil
	}
	maxReplicas, okMax := integer(spec["maxReplicas"])
	current, okCurrent := integer(status["currentReplicas"])
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
	return []modules.Symptom{s}
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

// condition returns the first condition of the type in status.conditions, or nil.
func condition(status map[string]any, typ string) map[string]any {
	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		cond, _ := c.(map[string]any)
		if t, _ := cond["type"].(string); t == typ {
			return cond
		}
	}
	return nil
}

// conditionIs reports whether the first condition of the type has the status.
func conditionIs(status map[string]any, typ, want string) bool {
	st, _ := condition(status, typ)["status"].(string)
	return st == want
}

// integer reads a JSON integer as decoded by encoding/json (float64 or json.Number) or by the Kubernetes
// decoder (int64). It reports false for anything else, a fraction included.
func integer(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n != math.Trunc(n) || n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}
