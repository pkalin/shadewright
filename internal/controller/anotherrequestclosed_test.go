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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// The AnotherRequestClosed message says what happens to the Incident next, as the analyzer's rules do it (#930):
// an analyzed Incident is not analyzed again and, unless a newer request names it, moves to Escalated, so a
// person applies a new request; one not yet analyzed (Open) gets its first analysis. Those rules are held to
// this statement by TestAnotherRequestClosedIncidentIsEscalatedAndNotAnalyzedAgain and
// TestAnotherRequestClosedOpenIncidentGetsItsFirstAnalysis in internal/incidents (not imported here: its
// metrics would join the executor's registry). Before #930 the message promised "a new analysis of the
// Incident" that never came.
func TestAnotherRequestClosedMessageMatchesTheIncidentsNextStep(t *testing.T) {
	holder := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Name: "closed-holder"},
		Status: remediationv1alpha1.RemediationRequestStatus{PullRequest: &remediationv1alpha1.PullRequestStatus{Number: 7}}}
	message := supersedeMessage(remediationv1alpha1.ReasonAnotherRequestClosed, holder)

	for _, want := range []string{"closed-holder", "pull request #7", "was analyzed is not analyzed again", "Escalated",
		"a person", "unless a newer request names it", "new request", "not yet analyzed gets its first analysis"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not say %q", message, want)
		}
	}
	for _, promise := range []string{"new analysis", "next analysis", "proposes again"} {
		if strings.Contains(message, promise) {
			t.Errorf("message %q promises %q, which an Escalated Incident never gets", message, promise)
		}
	}
}
