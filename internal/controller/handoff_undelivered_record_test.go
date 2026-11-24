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
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// A replacement that took the pull request over records its merge with another head as a foreign change
// (NeedsIntervention, Approved False with ReasonMergedWithForeignChange): that write stores no mergedAt and no
// receipt. It still holds the record of the merge (holdsMergeRecord through undeliveredMerge), so the predecessor's
// rule gives the merge to the replacement (#949). The handoff tests reach this through whole reconciles; this test
// pins the two predicates directly.
func TestUndeliveredMergeWithoutMergedAtIsTheReplacementsRecord(t *testing.T) {
	r, g, p, rr, cr, _ := tookOverWorld(t)
	ctx := context.Background()
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if !tookOver(rr) {
		t.Fatalf("handoff %+v: the replacement did not take the pull request over", rr.Status.Handoff)
	}
	// A person pushes on top of the replacement's commit; the merge of that head is not the replacement's commit.
	merged, err := g.Push("shadewright/"+string(chainRootOf(p)), "person.yaml", []byte("by: a person\n"))
	if err != nil {
		t.Fatal(err)
	}
	rr.Status.Phase = api.PhaseNeedsIntervention
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved,
		Status: metav1.ConditionFalse, Reason: ReasonMergedWithForeignChange,
		Message: "The merged head is not the commit Shadewright pushed for this request"})
	rr.Status.PullRequest.MergedAt, rr.Status.MergeReceipt = nil, nil
	mustStatus(t, r, rr)

	if !holdsMergeRecord(rr, cr.Number, merged) {
		t.Fatalf("holdsMergeRecord = false for a replacement in %s with Approved reason %s and no mergedAt; want "+
			"the record of the merge of #%d", rr.Status.Phase, ReasonMergedWithForeignChange, cr.Number)
	}
	if holdsMergeClaim(rr, cr.Number, merged) {
		t.Fatal("holdsMergeClaim = true for a replacement that holds the record; want the record, not a claim")
	}
	owner, err := r.predecessorMergeOwner(ctx, g, p, merged)
	if err != nil || owner != ownerReplacement {
		t.Fatalf("predecessorMergeOwner = %v, %v; want ownerReplacement (%v): the replacement holds the record, "+
			"so the predecessor records nothing", owner, err, ownerReplacement)
	}
}
