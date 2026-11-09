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

package incidents

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// noProposal is the Analyzed reason of the analysis that answered the later hint in these tests.
const noProposal = "NoProposal"

// staleRequests is a cache whose RemediationRequests lag behind the API server: it lists request instead.
type staleRequests struct {
	client.Client
	request remediationv1alpha1.RemediationRequest
}

func (s *staleRequests) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if requests, ok := list.(*remediationv1alpha1.RemediationRequestList); ok {
		requests.Items = []remediationv1alpha1.RemediationRequest{*s.request.DeepCopy()}
		return nil
	}
	return s.Client.List(ctx, list, opts...)
}

// The trigger refuses a hint only after reading its request live: a cache that still shows an earlier hint
// Pending, while the analysis answered a later one, never overwrites that answer (handledHint and its
// outcome), so the later hint is not taken again once the cache catches up. The same holds when the earlier
// hint is still Pending live but handledHint names a later hint of that request.
func TestStaleCacheNeverOverwritesANewerAnswer(t *testing.T) {
	first, second := hint(1, 30*time.Minute), hint(2, 2*time.Minute)
	for _, tt := range []struct {
		name       string
		firstState remediationv1alpha1.HintState
	}{
		{"earlier hint declined live", remediationv1alpha1.HintDeclined},
		{"earlier hint still pending live", remediationv1alpha1.HintPending},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stale := hinted("a", first, second)
			live := *stale.DeepCopy()
			live.Status.Hints[0].State = tt.firstState
			if tt.firstState == remediationv1alpha1.HintDeclined {
				live.Status.Hints[0].Reason = noProposal
			}
			inc := proposed(time.Minute)
			inc.Status.HandledHint = refOf(second)
			inc.Status.Conditions = append(inc.Status.Conditions, metav1.Condition{
				Type: ConditionAnalyzed, Status: metav1.ConditionTrue, Reason: noProposal,
				Message: "The second hint has been answered", ObservedGeneration: inc.Generation,
			})
			c, cl := hintController(t, inc, 10, 0, live)
			c.cache = &staleRequests{Client: cl, request: stale}
			if err := c.markDue(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := getIncident(t, cl)
			if got.Status.HandledHint == nil || *got.Status.HandledHint != *refOf(second) {
				t.Errorf("the answer of the second hint was overwritten: handledHint %+v, outcome %+v",
					got.Status.HandledHint, analyzedCondition(got))
			}
			if cond := analyzedCondition(got); cond == nil || cond.Reason != noProposal {
				t.Errorf("Analyzed %+v, want the second hint's NoProposal", cond)
			}
		})
	}
}

// A hint the cache still shows Pending but the executor already declined is not refused again: the trigger
// writes no answer for it.
func TestStaleCacheRefusesNoDeclinedHint(t *testing.T) {
	h := hint(1, 2*time.Minute)
	stale := hinted("a", h)
	live := *stale.DeepCopy()
	live.Status.Hints[0].State, live.Status.Hints[0].Reason = remediationv1alpha1.HintDeclined, "NoAnswer"
	c, cl := hintController(t, proposed(time.Minute), 10, 0, live)
	c.cache = &staleRequests{Client: cl, request: stale}
	if err := c.markDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getIncident(t, cl); got.Status.HandledHint != nil || analyzedCondition(got) != nil {
		t.Errorf("a declined hint was refused: handledHint %+v, outcome %+v", got.Status.HandledHint, analyzedCondition(got))
	}
}
