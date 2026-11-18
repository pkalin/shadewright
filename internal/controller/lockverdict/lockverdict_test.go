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

package lockverdict

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

func held() *api.RemediationRequestStatus {
	t := true
	return &api.RemediationRequestStatus{HoldsTarget: &t, Conditions: []metav1.Condition{{
		Type: api.ConditionTargetHeld, Status: metav1.ConditionTrue, Reason: "OpenOnBranch"}}}
}

// A nil or zero verdict releases nothing and is an error, never a silent no-op.
func TestReleaseRefusesNoVerdict(t *testing.T) {
	for name, v := range map[string]*Verdict{"nil": nil, "zero": {}} {
		s := held()
		if err := Release(s, v); !errors.Is(err, ErrNoVerdict) {
			t.Errorf("%s: Release = %v, want ErrNoVerdict", name, err)
		}
		if !*s.HoldsTarget || meta.FindStatusCondition(s.Conditions, api.ConditionTargetHeld) == nil {
			t.Errorf("%s: the lock or its condition changed without a verdict", name)
		}
	}
}

// Each constructor gives a verdict only for the facts that prove it, and Release clears the lock and TargetHeld.
func TestEachVerdictOnlyFromItsProof(t *testing.T) {
	receipt := func(phase api.RemediationPhase) *api.RemediationRequestStatus {
		s := held()
		s.Phase, s.MergeReceipt, s.PullRequest = phase, &api.MergeReceipt{}, &api.PullRequestStatus{Number: 1}
		return s
	}
	type c struct {
		v  *Verdict
		ok bool
	}
	of := func(v *Verdict, ok bool) c { return c{v, ok} }
	for name, tc := range map[string]struct {
		got  c
		want bool
	}{
		"read merged and recorded":         {of(ByRead(FoundMerged, true, true, false)), true},
		"read merged, not recorded":        {of(ByRead(FoundMerged, false, true, false)), false},
		"read closed":                      {of(ByRead(FoundClosed, false, true, false)), true},
		"read gone":                        {of(ByRead(FoundGone, false, false, false)), true},
		"read closed, another open":        {of(ByRead(FoundClosed, false, true, true)), false},
		"read open or unknown":             {of(ByRead(FoundOther, true, true, false)), false},
		"never held":                       {of(NeverHeld(&api.RemediationRequestStatus{})), true},
		"held is not never held":           {of(NeverHeld(held())), false},
		"handoff aborted":                  {of(HandoffAborted(&api.RemediationRequestStatus{Handoff: &api.Handoff{From: &api.IncomingHandoff{}}})), true},
		"handoff with a pull request":      {of(HandoffAborted(&api.RemediationRequestStatus{Handoff: &api.Handoff{From: &api.IncomingHandoff{}}, PullRequest: &api.PullRequestStatus{}})), false},
		"handed over":                      {of(HandedOver(&api.OutgoingHandoff{Phase: api.HandoffDone, UID: "u"}, "u", true)), true},
		"handed over to another":           {of(HandedOver(&api.OutgoingHandoff{Phase: api.HandoffDone, UID: "u"}, "v", true)), false},
		"released before the field":        {of(ReleasedBeforeField(&api.RemediationRequestStatus{}, false)), true},
		"derived hold":                     {of(ReleasedBeforeField(&api.RemediationRequestStatus{}, true)), false},
		"receipt, Merged":                  {of(RecordedMerge(receipt(api.PhaseMerged), false, false, false, false)), true},
		"receipt, Inconclusive":            {of(RecordedMerge(receipt(api.PhaseInconclusive), false, false, false, false)), true},
		"receipt, Succeeded":               {of(RecordedMerge(receipt(api.PhaseSucceeded), false, false, false, false)), true},
		"receipt, Failed":                  {of(RecordedMerge(receipt(api.PhaseFailed), false, false, false, false)), true},
		"receipt, held for verifying":      {of(RecordedMerge(receipt(api.PhaseMerged), false, false, false, true)), false},
		"no receipt, Merged":               {of(RecordedMerge(&api.RemediationRequestStatus{Phase: api.PhaseMerged}, false, false, false, false)), true},
		"no receipt, Inconclusive":         {of(RecordedMerge(&api.RemediationRequestStatus{Phase: api.PhaseInconclusive}, false, false, false, false)), true},
		"no receipt, Failed":               {of(RecordedMerge(&api.RemediationRequestStatus{Phase: api.PhaseFailed}, false, false, false, false)), false},
		"receipt, WaitingForDelivery":      {of(RecordedMerge(receipt(api.PhaseWaitingForDelivery), false, false, false, false)), true},
		"receipt, NeedsIntervention":       {of(RecordedMerge(receipt(api.PhaseNeedsIntervention), false, false, false, false)), false},
		"marked merged, NeedsIntervention": {of(RecordedMerge(&api.RemediationRequestStatus{Phase: api.PhaseNeedsIntervention}, true, false, false, false)), true},
		"marked merged, held":              {of(RecordedMerge(&api.RemediationRequestStatus{Phase: api.PhaseNeedsIntervention}, true, false, false, true)), false},
		"marked merged, undelivered":       {of(RecordedMerge(&api.RemediationRequestStatus{Phase: api.PhaseNeedsIntervention}, true, true, false, false)), false},
		"undelivered, Merged":              {of(RecordedMerge(&api.RemediationRequestStatus{Phase: api.PhaseMerged}, true, true, false, false)), true},
		"marked merged, Superseded":        {of(RecordedMerge(&api.RemediationRequestStatus{Phase: api.PhaseSuperseded}, true, false, false, false)), false},
		"handed off, Superseded":           {of(RecordedMerge(&api.RemediationRequestStatus{Phase: api.PhaseSuperseded}, true, false, true, false)), true},
		"handed off, not marked merged":    {of(RecordedMerge(&api.RemediationRequestStatus{Phase: api.PhaseSuperseded}, false, false, true, false)), false},
		"undelivered, repository gone":     {of(UndeliveredRepositoryGone(true, true)), true},
		"undelivered, repository there":    {of(UndeliveredRepositoryGone(true, false)), false},
		"delivered, repository gone":       {of(UndeliveredRepositoryGone(false, true)), false},
		"merged by the replacement":        {of(ReplacementMerge(FoundMerged, true)), true},
		"merged, not by the replacement":   {of(ReplacementMerge(FoundMerged, false)), false},
		"open, replacement's commit":       {of(ReplacementMerge(FoundOther, true)), false},
	} {
		if tc.got.ok != tc.want || (tc.got.v != nil) != tc.want {
			t.Errorf("%s: verdict %v, ok %t; want ok %t", name, tc.got.v, tc.got.ok, tc.want)
			continue
		}
		if !tc.want {
			continue
		}
		s := held()
		if err := Release(s, tc.got.v); err != nil || *s.HoldsTarget ||
			meta.FindStatusCondition(s.Conditions, api.ConditionTargetHeld) != nil {
			t.Errorf("%s: Release = %v, holds %t", name, err, *s.HoldsTarget)
		}
	}
}
