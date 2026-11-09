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

// Package quantity bounds Kubernetes quantities before they are compared or printed (#593).
//
// resource.ParseQuantity accepts a decimal exponent up to the int32 range, and comparing or printing such a
// value computes a power of ten of billions of digits, ignoring every context and outgrowing any memory
// limit. A quantity reaches Shadewright from untrusted places (invariant 4): a file in the GitOps repository,
// a live object Flux applied from it, a model's reply, a RemediationRequest. Every comparison of such a value
// goes through this package, which refuses a value outside the bounded form instead of computing it.
package quantity

import (
	"regexp"

	"k8s.io/apimachinery/pkg/api/resource"
)

// MaxLength bounds the text of a quantity string. A memory limit of 1Ti written as a byte count has 13
// digits, so 40 characters leave room for every form a person or the API server writes.
const MaxLength = 40

// Form is the strict form a quantity string must have before it is parsed: an optional sign, a decimal
// number, and an optional binary or decimal SI suffix or a decimal exponent of at most two digits. It is the
// pattern the CRD schema gives the RemediationRequest's memory values too.
const Form = `^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([KMGTPE]i|[numkMGTPE]|[eE][+-]?[0-9]{1,2})?$`

var form = regexp.MustCompile(Form)

// maxScale bounds the decimal scale of a bounded quantity: at most MaxLength digits after the point, shifted
// by an exponent of at most two digits. Every string in Form is within it.
const maxScale = MaxLength + 99

// maxBits bounds the unscaled value of a bounded quantity. resource.ParseQuantity rounds a value it cannot
// keep in an int64 to a scale of 9 (nanos), so a string in Form has at most MaxLength + 99 + 9 digits there
// (492 bits); a binary suffix adds at most 60 bits to far fewer digits.
const maxBits = 512

// Parse parses s when it is a quantity in the bounded form (MaxLength, Form). Any other string is refused
// without being parsed.
func Parse(s string) (resource.Quantity, bool) {
	if len(s) > MaxLength || !form.MatchString(s) {
		return resource.Quantity{}, false
	}
	q, err := resource.ParseQuantity(s)
	if err != nil || !Bounded(q) {
		return resource.Quantity{}, false
	}
	return q, true
}

// Bounded reports whether q, however it was decoded, has a value a string in Form can have: a decimal scale
// of at most maxScale either way and an unscaled value of at most maxBits bits. Comparing two bounded
// quantities aligns at most a few hundred digits. It never changes q.
func Bounded(q resource.Quantity) bool {
	c := q.DeepCopy() // AsDec caches a new representation in its receiver
	d := c.AsDec()
	s := int64(d.Scale())
	return s >= -maxScale && s <= maxScale && d.UnscaledBig().BitLen() <= maxBits
}

// Cmp compares a and b by value when both are Bounded, and reports false otherwise. Quantity.Cmp may cache
// a new representation in its receiver and argument, and a copied Quantity shares its decimal with the
// original, so it compares deep copies and never changes either input.
func Cmp(a, b resource.Quantity) (int, bool) {
	if !Bounded(a) || !Bounded(b) {
		return 0, false
	}
	x, y := a.DeepCopy(), b.DeepCopy()
	return x.Cmp(y), true
}
