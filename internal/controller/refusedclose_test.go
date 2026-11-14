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

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestBoundLeftNeverShortensABound: a transition at 12:00:00.900 is persisted as 12:00:00 (metav1.Time keeps whole
// seconds), and neither deletion bound may end before it has lasted its full length from the transition itself.
func TestBoundLeftNeverShortensABound(t *testing.T) {
	at := time.Date(2026, 11, 14, 12, 0, 0, 900*int(time.Millisecond), time.UTC)
	for _, bound := range []time.Duration{closeRefusedGrace, cleanupFailingBound} {
		for name, c := range map[string]*metav1.Condition{
			"persisted": {LastTransitionTime: metav1.NewTime(at.Truncate(time.Second))},
			"in memory": {LastTransitionTime: metav1.NewTime(at)},
		} {
			if left := boundLeft(c, bound, at.Add(bound-time.Millisecond)); left <= 0 {
				t.Errorf("%s, bound %v: %v left a millisecond before the bound ends, want more than zero", name, bound, left)
			}
			if left := boundLeft(c, bound, at.Add(bound+time.Second)); left > 0 {
				t.Errorf("%s, bound %v: %v left a second after the persisted second plus one, want none", name, bound, left)
			}
		}
	}
}
