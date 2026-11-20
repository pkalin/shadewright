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
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

// TestCRDOutdatedRefusedKeepsTheAnswer pins that the log text of a refused write carries the API server's
// answer, so a second cause in the same 422 is not hidden, while the Event stays fixed text.
func TestCRDOutdatedRefusedKeepsTheAnswer(t *testing.T) {
	second := "status.verification.windowStart: Invalid value: transition rule failed"
	e := &CRDOutdatedError{Refused: true, Fields: []string{"status.verification.lastReset.reason"},
		Request: types.NamespacedName{Namespace: "ns", Name: "r"}, Err: errors.New(second)}
	if !strings.Contains(e.Error(), second) {
		t.Fatalf("Error() hides the API server's answer: %q", e.Error())
	}
	if strings.Contains(e.eventMessage(), second) {
		t.Fatalf("the Event must keep to fixed text: %q", e.eventMessage())
	}
	pruned := &CRDOutdatedError{Fields: []string{"status.x"}, Request: types.NamespacedName{Namespace: "ns", Name: "r"}}
	if strings.Contains(pruned.Error(), "answered") {
		t.Fatalf("a pruned field has no answer to show: %q", pruned.Error())
	}
}
