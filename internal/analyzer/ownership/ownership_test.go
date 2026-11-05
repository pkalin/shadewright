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

package ownership

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSelectorBounds(t *testing.T) {
	big := map[string]string{}
	for i := range MaxSelectorTerms + 1 {
		big[fmt.Sprintf("k%d", i)] = "v"
	}
	for name, ls := range map[string]*metav1.LabelSelector{
		"nil":       nil,
		"empty":     {},
		"too large": {MatchLabels: big},
		"too many values": {MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: "k", Operator: metav1.LabelSelectorOpIn, Values: make([]string, MaxSelectorTerms),
		}}},
		"invalid": {MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "Near"}}},
	} {
		if _, ok := Selector(ls); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	sel, ok := Selector(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}})
	if !ok {
		t.Fatal("refused a plain selector")
	}
	obj := &metav1.ObjectMeta{Labels: map[string]string{"app": "web"}}
	if !Selects(sel, obj) {
		t.Error("does not select a matching object")
	}
	for i := range MaxLabels {
		obj.Labels[fmt.Sprintf("extra%d", i)] = "x"
	}
	if Selects(sel, obj) {
		t.Errorf("selects an object with %d labels", len(obj.Labels))
	}
}
