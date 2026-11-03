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

// Package kedaready holds what Shadewright knows of KEDA's API and the readiness rule of a ScaledObject: the
// one definition that both the KEDA integration module (its symptom rules and its KedaReady verification
// check) and the policy gate of AdjustKedaMaxReplicas use (design sections 2.5, 5.1 and 6). It lives in a
// package of its own so that the policy and freshness checks can use it without depending on the module
// registry or on an integration module: the core keeps working with no module compiled in (ADR-0018), and
// there is still one copy of the rule and of KEDA's names.
//
// Names were read from kedacore/keda v2.21.0, the release the lab installs: the group version from
// apis/keda/v1alpha1/groupversion_info.go, condition types and reasons from
// apis/keda/v1alpha1/condition_types.go and pkg/scaling/executor/scale_scaledobjects.go, and the pause
// annotations from apis/keda/v1alpha1/scaledobject_types.go (PausedAnnotation, PausedReplicasAnnotation,
// PausedScaleInAnnotation, PausedScaleOutAnnotation).
//
// It imports only the standard library: objects are read as plain JSON, as decoded into a map.
package kedaready

// ModuleName is the name of the KEDA integration module, the name a cluster admin lists in RemediationSettings
// spec.modules.integrations to enable it.
const ModuleName = "keda"

// KEDA's API group, version and kinds. KEDA 2.x serves ScaledObject and ScaledJob in keda.sh/v1alpha1 only.
const (
	Group            = "keda.sh"
	Version          = "v1alpha1"
	APIVersion       = Group + "/" + Version
	KindScaledObject = "ScaledObject"
	KindScaledJob    = "ScaledJob"
)

// Condition types and statuses KEDA writes (kedacore/keda v2.21.0 apis/keda/v1alpha1/condition_types.go).
const (
	ConditionReady     = "Ready"
	ConditionActive    = "Active"
	ConditionFallback  = "Fallback"
	ConditionPaused    = "Paused"
	ConditionHPAActive = "HPAActive"

	StatusTrue    = "True"
	StatusFalse   = "False"
	StatusUnknown = "Unknown"
)

// ReasonPartialTriggerError is the Ready=Unknown reason KEDA sets when a trigger fails while another one is
// active (pkg/scaling/executor/scale_scaledobjects.go and scale_jobs.go). It is a failure, not a ScaledObject
// KEDA has yet to reconcile.
const ReasonPartialTriggerError = "PartialTriggerError"

// PauseAnnotations are the annotations that pause a ScaledObject, fully, at a replica count, or for scale-in or
// scale-out only (apis/keda/v1alpha1/scaledobject_types.go). Any of them, whatever its value, makes the object
// not ready: KEDA sets the Paused condition only once it has reconciled the annotation.
var PauseAnnotations = []string{
	"autoscaling.keda.sh/paused",
	"autoscaling.keda.sh/paused-replicas",
	"autoscaling.keda.sh/paused-scale-in",
	"autoscaling.keda.sh/paused-scale-out",
}

// State is the readiness of a ScaledObject as [Readiness] reads it.
type State int

const (
	// NotReady is a definite state that rules out scaling on the metrics: a trigger error, a fallback, a
	// pause, an HPA that cannot read its metrics, an object being deleted, or an object that is not a
	// keda.sh/v1alpha1 ScaledObject or not of the JSON shape expected.
	NotReady State = iota
	// Pending is a ScaledObject KEDA has not reported on yet: no status, no conditions, no Ready condition, or
	// Ready=Unknown for a reason other than PartialTriggerError (KEDA initialises Ready to Unknown).
	Pending
	// Ready is a ScaledObject that scales on its metrics.
	Ready
)

func (s State) String() string {
	switch s {
	case Ready:
		return "Ready"
	case Pending:
		return "Pending"
	default:
		return "NotReady"
	}
}

// Readiness reads obj and returns:
//
//   - [Ready] when it is a keda.sh/v1alpha1 ScaledObject whose Ready condition is True, which is not paused
//     (no pause annotation, not Paused=True, which KEDA also sets for a scale-in or scale-out pause), not
//     being deleted, whose triggers are not falling back (Fallback=True) and whose HPA can read its metrics
//     (HPAActive is not False). A missing HPAActive (no HPA observed yet, or a KEDA release before 2.21) does
//     not count against it.
//   - [Pending] when nothing rules it out but KEDA has not reported Ready yet (no status, no conditions, no
//     Ready condition, Ready=Unknown other than PartialTriggerError).
//   - [NotReady] otherwise, including a nil object, another kind or version, and a metadata, status or
//     conditions value of another JSON type.
//
// A ScaledObject on fallback, or whose HPA cannot read its metrics, can report Ready=True while it does not
// scale on its metrics, so it is NotReady. KEDA's conditions carry no observedGeneration, so the conditions may
// describe an older spec. Readiness only reads obj, never changes it, and does no I/O.
func Readiness(obj map[string]any) State {
	if av, _ := obj["apiVersion"].(string); av != APIVersion {
		return NotReady
	}
	if k, _ := obj["kind"].(string); k != KindScaledObject {
		return NotReady
	}
	if paused, ok := pausedOrDeleted(obj); !ok || paused {
		return NotReady
	}
	raw, present := obj["status"]
	if !present || raw == nil {
		return Pending
	}
	status, ok := raw.(map[string]any)
	if !ok {
		return NotReady
	}
	switch conds, present := status["conditions"]; {
	case !present || conds == nil:
		return Pending
	default:
		if _, ok := conds.([]any); !ok {
			return NotReady
		}
	}
	if ConditionIs(status, ConditionPaused, StatusTrue) || ConditionIs(status, ConditionFallback, StatusTrue) ||
		ConditionIs(status, ConditionHPAActive, StatusFalse) {
		return NotReady
	}
	ready := FirstCondition(status, ConditionReady)
	if ready == nil {
		return Pending
	}
	switch st, _ := ready["status"].(string); st {
	case StatusTrue:
		return Ready
	case StatusUnknown:
		if r, _ := ready["reason"].(string); r == ReasonPartialTriggerError {
			return NotReady
		}
		return Pending
	default:
		return NotReady
	}
}

// IsReady reports whether [Readiness] gives [Ready]. It is the KedaReady rule.
func IsReady(obj map[string]any) bool { return Readiness(obj) == Ready }

// pausedOrDeleted reports whether obj carries a pause annotation or a deletionTimestamp. ok is false when
// metadata or its annotations are not of the JSON shape expected.
func pausedOrDeleted(obj map[string]any) (paused, ok bool) {
	raw, present := obj["metadata"]
	if !present || raw == nil {
		return false, true
	}
	meta, isMap := raw.(map[string]any)
	if !isMap {
		return false, false
	}
	if ts, set := meta["deletionTimestamp"]; set && ts != nil && ts != "" {
		return true, true
	}
	rawAnn, present := meta["annotations"]
	if !present || rawAnn == nil {
		return false, true
	}
	ann, isMap := rawAnn.(map[string]any)
	if !isMap {
		return false, false
	}
	for _, key := range PauseAnnotations {
		if _, set := ann[key]; set {
			return true, true
		}
	}
	return false, true
}

// FirstCondition returns the first entry of status.conditions whose type is typ, or nil when status, its
// conditions or such an entry are missing or not of the JSON shape expected. Entries that are not objects are
// skipped. status is only read.
func FirstCondition(status map[string]any, typ string) map[string]any {
	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		cond, _ := c.(map[string]any)
		if t, _ := cond["type"].(string); t == typ {
			return cond
		}
	}
	return nil
}

// ConditionIs reports whether the first condition of the type has the status.
func ConditionIs(status map[string]any, typ, want string) bool {
	st, _ := FirstCondition(status, typ)["status"].(string)
	return st == want
}
