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

// Package quiet holds what the analyzer's closure rules and the executor share to tell that an Incident went
// quiet: how long it must, and which Incidents cannot be told quiet at all. It is a package of its own so that
// the executor uses the same values (ADR-0030 section 4, amended 2026-11-15) without importing the analyzer's
// incident package, whose metrics would register in the executor's registry.
package quiet

import (
	"strings"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// BeforeResolved is how long an Incident must go quiet before it counts as stopped: for the analyzer's closure
// rules, an Incident no active request names moves to Resolved after it (design 3.2, #251); for the executor,
// a request whose pull request waits for review is marked IncidentStopped once its Incident's count has not
// changed for it (#715).
const BeforeResolved = time.Hour

// StatePrefix is the prefix of the occurrence ID of a symptom state read from an object's status by a module's
// symptom source (design 3.1).
const StatePrefix = "condition:"

// LastingState reports whether the incident's newest occurrence (by begin time; the first record when none
// has one) is a symptom state of a module's symptom source (StatePrefix). Such a state is emitted once while it
// lasts (design 3.1), so neither lastSeen nor the count moves while the problem is still present, and the
// status records no end of it: an ongoing state cannot be told from one that ended.
func LastingState(st *remediationv1alpha1.IncidentStatus) bool {
	if len(st.Occurrences) == 0 {
		return false
	}
	newest := st.Occurrences[0]
	for _, r := range st.Occurrences[1:] {
		if r.Began != nil && (newest.Began == nil || r.Began.After(newest.Began.Time)) {
			newest = r
		}
	}
	return strings.HasPrefix(newest.ID, StatePrefix)
}
