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

package quantity_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkalin/shadewright/internal/quantity"
	"github.com/pkalin/shadewright/internal/testutil/deadline"
)

// decode reads a quantity as the API types do, from JSON, without the bounds of Parse.
func decode(t *testing.T, s string) resource.Quantity {
	t.Helper()
	var q resource.Quantity
	if err := json.Unmarshal([]byte(`"`+s+`"`), &q); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return q
}

// bounded holds strings in the form at its edges, each of which must parse and be Bounded.
var bounded = []string{
	"0", "1", "256Mi", "1Ti", "1e99", "1E-99", "-1e99", "500m", "1n",
	strings.Repeat("9", quantity.MaxLength),
	strings.Repeat("9", quantity.MaxLength-2) + "Ei",
	"." + strings.Repeat("9", quantity.MaxLength-5) + "e-99",
	strings.Repeat("9", quantity.MaxLength-3) + "e99",
}

func TestParseAcceptsTheBoundedForm(t *testing.T) {
	for _, s := range bounded {
		q, ok := quantity.Parse(s)
		if !ok {
			t.Errorf("Parse(%q) refused", s)
			continue
		}
		if !quantity.Bounded(q) {
			t.Errorf("Bounded(Parse(%q)) = false", s)
		}
	}
}

func TestParseRefusesOutsideTheForm(t *testing.T) {
	for _, s := range []string{
		"", "1e100", "1e2147483647", " 1Gi", "1Gi ", "0x10", "1ki",
		strings.Repeat("1", quantity.MaxLength+1),
		"536870912e000",
	} {
		if _, ok := quantity.Parse(s); ok {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}

// TestBoundedRefusesDecodedHugeExponents is the core of #593 for values already decoded by the API types: a
// RemediationRequest, a live object or a model reply holds a resource.Quantity, not a string.
func TestBoundedRefusesDecodedHugeExponents(t *testing.T) {
	// A negative exponent of that size is left out: resource.ParseQuantity rounds it to nanos while decoding.
	for _, s := range []string{"1e2147483647", "-1e2147483647", "1e1000", "1" + strings.Repeat("0", 200)} {
		if quantity.Bounded(decode(t, s)) {
			t.Errorf("Bounded(%q) = true", s)
		}
	}
	for _, s := range bounded {
		if !quantity.Bounded(decode(t, s)) {
			t.Errorf("Bounded(%q) = false", s)
		}
	}
}

func TestCmpRefusesUnboundedQuickly(t *testing.T) {
	deadline.Within(t, 5*time.Second, func(t *testing.T) {
		huge, mi := decode(t, "1e2147483647"), resource.MustParse("1Mi")
		if _, ok := quantity.Cmp(huge, mi); ok {
			t.Error("Cmp(huge, 1Mi) compared")
		}
		if _, ok := quantity.Cmp(mi, huge); ok {
			t.Error("Cmp(1Mi, huge) compared")
		}
	})
}

func TestCmpComparesByValueWithoutChangingInputs(t *testing.T) {
	a, b := resource.MustParse("1Gi"), resource.MustParse("1024Mi")
	wantA, wantB := a.DeepCopy(), b.DeepCopy()
	if c, ok := quantity.Cmp(a, b); !ok || c != 0 {
		t.Errorf("Cmp(1Gi, 1024Mi) = %d, %v", c, ok)
	}
	if !reflect.DeepEqual(a, wantA) || !reflect.DeepEqual(b, wantB) {
		t.Errorf("inputs changed: %#v, %#v", a, b)
	}
	if c, ok := quantity.Cmp(resource.MustParse("1e99"), resource.MustParse("1E-99")); !ok || c != 1 {
		t.Errorf("Cmp(1e99, 1E-99) = %d, %v", c, ok)
	}
}
