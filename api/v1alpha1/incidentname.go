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
	"regexp"
	"strconv"
	"strings"
)

// MaxIncidentOccurrence is the highest occurrence number an Incident name carries (design 3.2): six digits.
// The cap on Incidents keeps the real numbers far below it.
const MaxIncidentOccurrence = 999999

// incidentName matches the names the analyzer gives Incidents: "incident-", the 64 lower-case hex digits of
// the fingerprint, and for occurrence n >= 2 "-<n>" in decimal without a leading zero, at most six digits.
var incidentName = regexp.MustCompile(`^incident-([0-9a-f]{64})(?:-([2-9]|[1-9][0-9]{1,5}))?$`)

// IncidentName is the name of occurrence n of the Incident of fingerprint fp ("sha256:" and 64 hex digits):
// "incident-" and the hex for the first occurrence (n of 1 or less, the name every Incident had before
// occurrences), and "incident-<hex>-<n>" for n >= 2. A fingerprint and an occurrence number always give the
// same name, so a create that races with another replica or a restart finds AlreadyExists (invariant 5).
func IncidentName(fp string, n int32) string {
	name := "incident-" + strings.TrimPrefix(fp, "sha256:")
	if n >= 2 {
		name += "-" + strconv.Itoa(int(n))
	}
	return name
}

// ParseIncidentName returns the fingerprint ("sha256:" and the hex) and the occurrence number (1 for a name
// without a suffix) of a name [IncidentName] gives, and false for any other name: a suffix of 0 or 1, one with
// a leading zero, one that is not decimal or one longer than six digits.
func ParseIncidentName(name string) (fp string, n int32, ok bool) {
	m := incidentName.FindStringSubmatch(name)
	if m == nil {
		return "", 0, false
	}
	n = 1
	if m[2] != "" {
		v, err := strconv.ParseInt(m[2], 10, 32)
		if err != nil {
			return "", 0, false
		}
		n = int32(v)
	}
	return "sha256:" + m[1], n, true
}

// Occurrence returns the occurrence number the spec records: 1 when it records none.
func (s *IncidentSpec) Occurrence() int32 {
	if s.OccurrenceNumber < 2 {
		return 1
	}
	return s.OccurrenceNumber
}

// HasDerivedName reports whether inc's name is the one [IncidentName] derives from its own spec (its
// fingerprint and its occurrence number) and a name [ParseIncidentName] accepts. A name its creator chose
// proves nothing; this derivation from the immutable spec does.
func (inc *Incident) HasDerivedName() bool {
	if _, _, ok := ParseIncidentName(inc.Name); !ok {
		return false
	}
	return inc.Name == IncidentName(inc.Spec.Fingerprint, inc.Spec.Occurrence())
}
