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

package policy_test

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy"
	"github.com/pkalin/shadewright/internal/testutil/deadline"
)

// hugeQuantity is a memory value with a decimal exponent near the int32 limit, which resource.ParseQuantity
// accepts and Quantity.Cmp turns into a power of ten of billions of digits (#593).
const hugeQuantity = "1e2147483647"

// TestCheckActionRefusesUnboundedQuantities is part of #593: a RemediationRequest whose previous or desired
// value has a huge decimal exponent is refused as invalid parameters before it is compared with the catalog's
// bounds.
func TestCheckActionRefusesUnboundedQuantities(t *testing.T) {
	for name, set := range map[string]func(*v1alpha1.SetContainerMemoryLimit){
		"previous": func(p *v1alpha1.SetContainerMemoryLimit) { p.Previous = resource.MustParse(hugeQuantity) },
		"desired":  func(p *v1alpha1.SetContainerMemoryLimit) { p.Desired = resource.MustParse(hugeQuantity) },
	} {
		t.Run(name, func(t *testing.T) {
			deadline.Within(t, 5*time.Second, func(t *testing.T) {
				p := &v1alpha1.SetContainerMemoryLimit{
					Container: "web", Previous: resource.MustParse("256Mi"), Desired: resource.MustParse("512Mi"),
				}
				set(p)
				a := v1alpha1.Action{Type: v1alpha1.ActionSetContainerMemoryLimit, SetContainerMemoryLimit: p}
				if reason, _ := policy.CheckAction(a); reason != policy.ReasonInvalidParameters {
					t.Fatalf("reason %q, want %s", reason, policy.ReasonInvalidParameters)
				}
			})
		})
	}
}

// TestAllowedMemoryRefusesUnboundedQuantities is part of #593: neither a previous value nor policy bounds
// with a huge decimal exponent give a range.
func TestAllowedMemoryRefusesUnboundedQuantities(t *testing.T) {
	deadline.Within(t, 5*time.Second, func(t *testing.T) {
		ok := &v1alpha1.MemoryLimitBounds{Min: resource.MustParse("1Mi"), Max: resource.MustParse("1Gi"), MaxIncreasePercent: 100}
		huge := &v1alpha1.MemoryLimitBounds{Min: resource.MustParse("1Mi"), Max: resource.MustParse(hugeQuantity), MaxIncreasePercent: 100}
		if got := policy.AllowedMemory(ok, resource.MustParse(hugeQuantity)); got != nil {
			t.Errorf("AllowedMemory from a huge previous value = %v, want none", got)
		}
		if got := policy.AllowedMemory(huge, resource.MustParse("256Mi")); got != nil {
			t.Errorf("AllowedMemory under a huge max = %v, want none", got)
		}
	})
}
