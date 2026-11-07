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

package v1alpha1

import (
	"strings"
	"testing"
)

// Occurrence 1 keeps the name every Incident had before occurrences; n >= 2 adds "-<n>", and parsing gives
// back exactly what made the name (design 3.2, #251).
func TestIncidentNameRoundTrips(t *testing.T) {
	hex := strings.Repeat("9f86d081", 8)
	fp := "sha256:" + hex
	for _, tc := range []struct {
		n    int32
		want string
		back int32
	}{
		{0, "incident-" + hex, 1},
		{1, "incident-" + hex, 1},
		{2, "incident-" + hex + "-2", 2},
		{10, "incident-" + hex + "-10", 10},
		{MaxIncidentOccurrence, "incident-" + hex + "-999999", MaxIncidentOccurrence},
	} {
		got := IncidentName(fp, tc.n)
		if got != tc.want {
			t.Errorf("IncidentName(%d) = %q, want %q", tc.n, got, tc.want)
		}
		gotFP, gotN, ok := ParseIncidentName(got)
		if !ok || gotFP != fp || gotN != tc.back {
			t.Errorf("ParseIncidentName(%q) = %q, %d, %v; want %q, %d, true", got, gotFP, gotN, ok, fp, tc.back)
		}
	}
}

// Every name the analyzer would not give is refused, so a chosen suffix never passes as an occurrence.
func TestParseIncidentNameRefuses(t *testing.T) {
	base := "incident-" + strings.Repeat("9f86d081", 8)
	for _, name := range []string{
		base + "-0", base + "-1", base + "-02", base + "-ab", base + "--2", base + "-1000000", base + "-2-3",
		base + "-", base + "-2 ", "incident-" + strings.Repeat("9F86D081", 8), base[:len(base)-1], "orders-scaling",
	} {
		if _, _, ok := ParseIncidentName(name); ok {
			t.Errorf("ParseIncidentName(%q) accepted it", name)
		}
	}
}

// HasDerivedName holds only when the suffix is the occurrence number the spec records.
func TestHasDerivedName(t *testing.T) {
	hex := strings.Repeat("9f86d081", 8)
	inc := func(name string, n int32) *Incident {
		i := &Incident{Spec: IncidentSpec{Fingerprint: "sha256:" + hex, OccurrenceNumber: n}}
		i.Name = name
		return i
	}
	base := "incident-" + hex
	for _, tc := range []struct {
		inc  *Incident
		want bool
	}{
		{inc(base, 0), true},
		{inc(base+"-2", 2), true},
		{inc(base+"-2", 0), false},
		{inc(base, 2), false},
		{inc(base+"-3", 2), false},
		{inc(base+"-1", 1), false},
	} {
		if got := tc.inc.HasDerivedName(); got != tc.want {
			t.Errorf("HasDerivedName(%q, %d) = %v, want %v", tc.inc.Name, tc.inc.Spec.OccurrenceNumber, got, tc.want)
		}
	}
}
