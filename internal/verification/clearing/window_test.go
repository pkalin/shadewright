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

package clearing

import (
	"testing"
	"time"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// A reset is never recorded before the previous one, even when the clock went back to a time between the
// window's start and that reset.
func TestResetNeverMovesBeforeThePreviousReset(t *testing.T) {
	w := Window{}
	w.Good(t0)
	w.Reset(v1alpha1.ResetPodNotReady, t0.Add(10*time.Minute))
	w.Reset(v1alpha1.ResetPartialRollout, t0.Add(5*time.Minute))
	if got, want := w.LastReset.Time.Time, t0.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("lastReset.time = %v, want it kept at %v", got, want)
	}
	if w.Resets != 2 || w.LastReset.Reason != v1alpha1.ResetPartialRollout {
		t.Fatalf("resets %d, reason %q; want 2, PartialRollout", w.Resets, w.LastReset.Reason)
	}
}
