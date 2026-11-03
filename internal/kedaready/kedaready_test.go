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

package kedaready_test

import (
	"maps"
	"reflect"
	"testing"

	"github.com/pkalin/shadewright/internal/kedaready"
	"github.com/pkalin/shadewright/internal/testutil/importguard"
)

const (
	condReady    = kedaready.ConditionReady
	condActive   = kedaready.ConditionActive
	condFallback = kedaready.ConditionFallback
	condPaused   = kedaready.ConditionPaused
	condHPA      = kedaready.ConditionHPAActive
	condTrue     = kedaready.StatusTrue
	condFalse    = kedaready.StatusFalse
	condUnknown  = kedaready.StatusUnknown
	keyType      = "type"
	keyStatus    = "status"
	keyConds     = "conditions"
	keyMeta      = "metadata"
)

// scaledObject returns a ready ScaledObject as decoded from JSON, with the given condition statuses replacing
// the defaults by type; a nil status removes the condition.
func scaledObject(override map[string]any) map[string]any {
	order := []string{condReady, condActive, condFallback, condPaused, condHPA}
	status := map[string]any{condReady: condTrue, condActive: condTrue, condFallback: condFalse, condPaused: condFalse, condHPA: condTrue}
	maps.Copy(status, override)
	conds := []any{}
	for _, typ := range order {
		if st, ok := status[typ].(string); ok {
			conds = append(conds, map[string]any{keyType: typ, keyStatus: st})
		}
	}
	return map[string]any{
		"apiVersion": "keda.sh/v1alpha1",
		"kind":       "ScaledObject",
		keyMeta:      map[string]any{"name": "worker", "namespace": "demo"},
		keyStatus:    map[string]any{keyConds: conds},
	}
}

func with(o map[string]any, change func(o map[string]any)) map[string]any {
	change(o)
	return o
}

func annotated(key, value string) map[string]any {
	return with(scaledObject(nil), func(o map[string]any) {
		o[keyMeta].(map[string]any)["annotations"] = map[string]any{"team": "orders", key: value}
	})
}

func TestReadiness(t *testing.T) {
	const (
		ready    = kedaready.Ready
		pending  = kedaready.Pending
		notReady = kedaready.NotReady
	)
	cases := []struct {
		name string
		obj  map[string]any
		want kedaready.State
	}{
		{"ready", scaledObject(nil), ready},
		{"HPAActive Unknown", scaledObject(map[string]any{condHPA: condUnknown}), ready},
		{"no HPAActive condition", scaledObject(map[string]any{condHPA: nil}), ready},
		{"unrelated annotation", with(scaledObject(nil), func(o map[string]any) {
			o[keyMeta].(map[string]any)["annotations"] = map[string]any{"team": "orders"}
		}), ready},
		{"no metadata", with(scaledObject(nil), func(o map[string]any) { delete(o, keyMeta) }), ready},

		{"trigger error", scaledObject(map[string]any{condReady: condFalse, condActive: condFalse}), notReady},
		{"partial trigger error", with(scaledObject(nil), func(o map[string]any) {
			c := o[keyStatus].(map[string]any)[keyConds].([]any)[0].(map[string]any)
			c[keyStatus], c["reason"] = condUnknown, "PartialTriggerError"
		}), notReady},
		{"on fallback, though Ready", scaledObject(map[string]any{condFallback: condTrue}), notReady},
		{"paused, though Ready", scaledObject(map[string]any{condPaused: condTrue}), notReady},
		{"HPA cannot read its metrics, though Ready", scaledObject(map[string]any{condHPA: condFalse}), notReady},
		{"pause annotation", annotated("autoscaling.keda.sh/paused", "true"), notReady},
		{"pause annotation false", annotated("autoscaling.keda.sh/paused", "false"), notReady},
		{"paused-replicas annotation", annotated("autoscaling.keda.sh/paused-replicas", "0"), notReady},
		{"paused-scale-in annotation", annotated("autoscaling.keda.sh/paused-scale-in", "true"), notReady},
		{"paused-scale-out annotation", annotated("autoscaling.keda.sh/paused-scale-out", ""), notReady},
		{"being deleted", with(scaledObject(nil), func(o map[string]any) {
			o[keyMeta].(map[string]any)["deletionTimestamp"] = "2026-11-03T10:00:00Z"
		}), notReady},
		{"nil object", nil, notReady},
		{"metadata not an object", with(scaledObject(nil), func(o map[string]any) { o[keyMeta] = "worker" }), notReady},
		{"annotations not an object", with(scaledObject(nil), func(o map[string]any) {
			o[keyMeta].(map[string]any)["annotations"] = []any{"autoscaling.keda.sh/paused"}
		}), notReady},
		{"status not an object", with(scaledObject(nil), func(o map[string]any) { o[keyStatus] = "Ready" }), notReady},
		{"conditions not a list", with(scaledObject(nil), func(o map[string]any) {
			o[keyStatus] = map[string]any{keyConds: map[string]any{keyType: condReady, keyStatus: condTrue}}
		}), notReady},
		{"Ready status not a string", with(scaledObject(nil), func(o map[string]any) {
			o[keyStatus] = map[string]any{keyConds: []any{map[string]any{keyType: condReady, keyStatus: true}}}
		}), notReady},
		{"a second Ready entry is not read", with(scaledObject(map[string]any{condReady: condFalse}), func(o map[string]any) {
			st := o[keyStatus].(map[string]any)
			st[keyConds] = append(st[keyConds].([]any), map[string]any{keyType: condReady, keyStatus: condTrue})
		}), notReady},
		{"a ScaledJob", with(scaledObject(nil), func(o map[string]any) { o["kind"] = "ScaledJob" }), notReady},
		{"another version", with(scaledObject(nil), func(o map[string]any) { o["apiVersion"] = "keda.sh/v1" }), notReady},
		{"no apiVersion", with(scaledObject(nil), func(o map[string]any) { delete(o, "apiVersion") }), notReady},
		{"paused while KEDA has not reported", with(annotated("autoscaling.keda.sh/paused", "true"),
			func(o map[string]any) { delete(o, keyStatus) }), notReady},

		{"no status yet", with(scaledObject(nil), func(o map[string]any) { delete(o, keyStatus) }), pending},
		{"no conditions yet", with(scaledObject(nil), func(o map[string]any) { o[keyStatus] = map[string]any{} }), pending},
		{"no Ready condition", scaledObject(map[string]any{condReady: nil}), pending},
		{"Ready Unknown, as KEDA initialises it", scaledObject(map[string]any{condReady: condUnknown}), pending},
		{"non-object entries are skipped", with(scaledObject(nil), func(o map[string]any) {
			st := o[keyStatus].(map[string]any)
			st[keyConds] = append([]any{"Ready=False"}, st[keyConds].([]any)...)
		}), ready},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := kedaready.Readiness(c.obj); got != c.want {
				t.Errorf("Readiness = %v, want %v", got, c.want)
			}
			if got := kedaready.IsReady(c.obj); got != (c.want == ready) {
				t.Errorf("IsReady = %v, want %v", got, c.want == ready)
			}
		})
	}
}

func TestReadinessDoesNotModifyTheObject(t *testing.T) {
	obj, before := annotated("autoscaling.keda.sh/paused", "true"), annotated("autoscaling.keda.sh/paused", "true")
	kedaready.Readiness(obj)
	if !reflect.DeepEqual(obj, before) {
		t.Error("Readiness changed the object")
	}
}

// TestNoVendorImports enforces ADR-0013: the package imports only the standard library and this module.
func TestNoVendorImports(t *testing.T) {
	importguard.DirectImportsOnlyStandardOrModule(t, ".")
	importguard.NoForbiddenDeps(t, ".", ".")
}
