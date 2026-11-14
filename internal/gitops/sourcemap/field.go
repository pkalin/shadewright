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
package sourcemap

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/quantity"
)

// keySpec is the spec field of every object.
const keySpec = "spec"

// Step is one step from an object's root towards a field.
type Step struct {
	// Key is the map key to follow.
	Key string
	// Name, when set, means the value at Key is a list and selects its only element whose "name" equals Name.
	Name string
}

// Field locates the value an action changes, so the mapping can check that the file holds the live value.
type Field struct {
	// Steps lead from the object root to the value.
	Steps []Step
	// Quantity compares the values as Kubernetes resource quantities, so 256Mi equals 268435456.
	Quantity bool
}

// FieldFor returns the field an action of the catalog changes, after checking that the target has a kind
// the action may change: a keda.sh ScaledObject for AdjustKedaMaxReplicas, an apps/v1 Deployment or
// StatefulSet for SetContainerMemoryLimit.
func FieldFor(action v1alpha1.Action, target v1alpha1.ObjectRef) (Field, error) {
	gv, err := schema.ParseGroupVersion(target.APIVersion)
	if err != nil {
		return Field{}, fmt.Errorf("invalid apiVersion")
	}
	if err := checkActionKind(action.Type, gv.WithKind(target.Kind)); err != nil {
		return Field{}, err
	}
	switch action.Type {
	case v1alpha1.ActionAdjustKedaMaxReplicas:
		return Field{Steps: []Step{{Key: keySpec}, {Key: "maxReplicaCount"}}}, nil
	case v1alpha1.ActionSetContainerMemoryLimit:
		if action.SetContainerMemoryLimit == nil || action.SetContainerMemoryLimit.Container == "" {
			return Field{}, fmt.Errorf("%s has no container", action.Type)
		}
		return Field{
			Steps: []Step{
				{Key: keySpec}, {Key: "template"}, {Key: keySpec},
				{Key: "containers", Name: action.SetContainerMemoryLimit.Container},
				{Key: "resources"}, {Key: "limits"}, {Key: "memory"},
			},
			Quantity: true,
		}, nil
	default:
		return Field{}, fmt.Errorf("unknown action type")
	}
}

// checkActionKind is the allow-list of the kinds each action of the catalog may change: a keda.sh
// ScaledObject for AdjustKedaMaxReplicas, an apps/v1 Deployment or StatefulSet for SetContainerMemoryLimit.
// [FieldFor] checks a target against it, and [FindManifestCandidate] takes only a kind some action may change.
func checkActionKind(action v1alpha1.ActionType, gvk schema.GroupVersionKind) error {
	switch action {
	case v1alpha1.ActionAdjustKedaMaxReplicas:
		if gvk.Group != "keda.sh" || gvk.Kind != "ScaledObject" {
			return fmt.Errorf("%s changes only a keda.sh ScaledObject", action)
		}
	case v1alpha1.ActionSetContainerMemoryLimit:
		if gvk.Group != "apps" || gvk.Version != "v1" || (gvk.Kind != "Deployment" && gvk.Kind != "StatefulSet") {
			return fmt.Errorf("%s changes only an apps/v1 Deployment or StatefulSet", action)
		}
	default:
		return fmt.Errorf("unknown action type")
	}
	return nil
}

// actionKind reports whether some action of the catalog may change an object of this kind.
func actionKind(gvk schema.GroupVersionKind) bool {
	for _, a := range v1alpha1.ActionTypes() {
		if checkActionKind(a, gvk) == nil {
			return true
		}
	}
	return false
}

// String returns the field in a readable form, for example spec.template.spec.containers[name=web].resources.
func (f Field) String() string {
	var b strings.Builder
	for i, s := range f.Steps {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.Key)
		if s.Name != "" {
			fmt.Fprintf(&b, "[name=%s]", s.Name)
		}
	}
	return b.String()
}

// Get returns the value of the field in an object decoded from JSON or YAML. It reports false when any step
// is missing, has the wrong type, or a list selection matches no element or more than one.
func (f Field) Get(obj map[string]any) (any, bool) {
	var cur any = obj
	for _, s := range f.Steps {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[s.Key]; !ok {
			return nil, false
		}
		if s.Name == "" {
			continue
		}
		list, ok := cur.([]any)
		if !ok {
			return nil, false
		}
		var found []any
		for _, el := range list {
			if em, ok := el.(map[string]any); ok && em["name"] == s.Name {
				found = append(found, el)
			}
		}
		if len(found) != 1 {
			return nil, false
		}
		cur = found[0]
	}
	return cur, cur != nil
}

// Equals reports whether two decoded values are the same value of this field: as quantities for a Quantity
// field (256Mi equals 268435456), otherwise by their JSON encoding. A value that is not Comparable equals
// nothing, itself included.
func (f Field) Equals(a, b any) bool { return f.equal(a, b) }

// Comparable reports whether Equals can read a value of this field. A Quantity field reads an integer
// number, or a string in the bounded form of quantity.Parse; a string outside it is refused, never parsed.
// Every value is comparable for any other field.
func (f Field) Comparable(v any) bool {
	if !f.Quantity {
		return true
	}
	_, ok := toQuantity(v)
	return ok
}

// Compare orders two decoded values of this field: -1, 0 or 1 as a is below, equal to or above b. A Quantity
// field compares as quantities (only Comparable values); any other field compares integer numbers (int64, or a
// whole float64 within 2^53, the forms PreviousValue and JSON decoding give). It reports false for a pair it
// cannot order, which a caller treats as not known, never as equal.
func (f Field) Compare(a, b any) (int, bool) {
	if f.Quantity {
		qa, okA := toQuantity(a)
		qb, okB := toQuantity(b)
		if !okA || !okB {
			return 0, false
		}
		return qa.Cmp(qb), true
	}
	ia, okA := toInteger(a)
	ib, okB := toInteger(b)
	if !okA || !okB {
		return 0, false
	}
	return cmp.Compare(ia, ib), true
}

// toInteger reads an integer number: an int64, or a whole float64 within 2^53.
func toInteger(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > 1<<53 {
			return 0, false
		}
		return int64(x), true
	default:
		return 0, false
	}
}

// equal reports whether two field values are the same. Without Quantity they must have the same JSON form,
// so the number 15 equals 15 decoded either as an integer or a float, but not the string "15".
func (f Field) equal(a, b any) bool {
	if f.Quantity {
		qa, okA := toQuantity(a)
		qb, okB := toQuantity(b)
		return okA && okB && qa.Cmp(qb) == 0
	}
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// toQuantity reads a value as a quantity: an integer number, or a string in the bounded form of
// quantity.Parse. Content of the GitOps repository and the live values Flux applied from it are untrusted
// (invariant 4), and resource.Quantity has arbitrary precision: a string outside that form is refused, never
// parsed, because comparing it can compute a power of ten of billions of digits (#593).
func toQuantity(v any) (resource.Quantity, bool) {
	switch x := v.(type) {
	case string:
		return quantity.Parse(x)
	case int64:
		return *resource.NewQuantity(x, resource.DecimalSI), true
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > 1<<53 {
			return resource.Quantity{}, false
		}
		return *resource.NewQuantity(int64(x), resource.DecimalSI), true
	default:
		return resource.Quantity{}, false
	}
}

// PreviousValue returns the previous value an action claims for its field, in the form Field.Equals compares:
// a canonical quantity string for a memory limit, an int64 for a replica count. It reports false for an action
// of no known type.
func PreviousValue(action v1alpha1.Action) (any, bool) {
	switch {
	case action.SetContainerMemoryLimit != nil:
		q := action.SetContainerMemoryLimit.Previous.DeepCopy() // String caches into its receiver
		return q.String(), true
	case action.AdjustKedaMaxReplicas != nil:
		return int64(action.AdjustKedaMaxReplicas.PreviousValue), true
	default:
		return nil, false
	}
}

// DesiredValue returns the desired value an action sets for its field, in the same form as PreviousValue.
// It reports false for an action of no known type.
func DesiredValue(action v1alpha1.Action) (any, bool) {
	switch {
	case action.SetContainerMemoryLimit != nil:
		q := action.SetContainerMemoryLimit.Desired.DeepCopy() // String caches into its receiver
		return q.String(), true
	case action.AdjustKedaMaxReplicas != nil:
		return int64(action.AdjustKedaMaxReplicas.DesiredValue), true
	default:
		return nil, false
	}
}
