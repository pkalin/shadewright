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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents/aggregation"
)

// TestResolvedBeforeRollout checks the rule of ADR-0030 section 4: ResolvedBeforeRollout only with both
// readings, both of spec.incidentRef.uid, equal counts below the saturation value and a span of at least
// stableFor plus two analyzer windows on the executor's clock.
func TestResolvedBeforeRollout(t *testing.T) {
	const uid = "incident-uid"
	stableFor := 10 * time.Minute
	minSpan := stableFor + 2*aggregation.DefaultWindow
	merged := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	reading := func(u string, count int64, at time.Time) *remediationv1alpha1.IncidentCountReading {
		return &remediationv1alpha1.IncidentCountReading{UID: u, Count: count, ReadAt: metav1.NewTime(at)}
	}
	request := func(m, a *remediationv1alpha1.IncidentCountReading) *remediationv1alpha1.RemediationRequest {
		rr := &remediationv1alpha1.RemediationRequest{}
		rr.Spec.IncidentRef.UID = uid
		rr.Status.Verification = &remediationv1alpha1.VerificationWindow{
			IncidentCount: &remediationv1alpha1.IncidentCountReadings{AtMerge: m, AtApplied: a}}
		return rr
	}
	saturated := int64(remediationv1alpha1.MaxIncidentCount)
	cases := []struct {
		name string
		rr   *remediationv1alpha1.RemediationRequest
		want bool
	}{
		{"equal counts over exactly the minimum span", request(reading(uid, 3, merged), reading(uid, 3, merged.Add(minSpan))), true},
		{"one second short of the minimum span", request(reading(uid, 3, merged), reading(uid, 3, merged.Add(minSpan-time.Second))), false},
		{"a count that rose", request(reading(uid, 3, merged), reading(uid, 4, merged.Add(time.Hour))), false},
		{"a count that went down", request(reading(uid, 3, merged), reading(uid, 2, merged.Add(time.Hour))), false},
		{"a saturated count", request(reading(uid, saturated, merged), reading(uid, saturated, merged.Add(time.Hour))), false},
		{"another UID at the merge", request(reading("other", 3, merged), reading(uid, 3, merged.Add(time.Hour))), false},
		{"another UID at the applied time", request(reading(uid, 3, merged), reading("other", 3, merged.Add(time.Hour))), false},
		{"no reading at the applied time", request(reading(uid, 3, merged), nil), false},
		{"no readings", &remediationv1alpha1.RemediationRequest{}, false},
		{"a reading taken before the merge reading", request(reading(uid, 3, merged.Add(time.Hour)), reading(uid, 3, merged)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := resolvedBeforeRollout(tc.rr, stableFor); got != tc.want {
				t.Errorf("resolvedBeforeRollout = %v, want %v", got, tc.want)
			}
			reason, _ := clearedReason(tc.rr, stableFor, "quiet")
			want := ReasonNoRecurrence
			if tc.want {
				want = ReasonResolvedBeforeRollout
			}
			if reason != want {
				t.Errorf("clearedReason = %q, want %q", reason, want)
			}
		})
	}
	noUID := request(reading("", 3, merged), reading("", 3, merged.Add(time.Hour)))
	noUID.Spec.IncidentRef.UID = ""
	if _, got := resolvedBeforeRollout(noUID, stableFor); got {
		t.Error("a request without spec.incidentRef.uid got ResolvedBeforeRollout")
	}
}

// TestIncidentCountUnusableIsLogged: an Incident without a UID or with a count outside the accepted range gives
// no reading, and each says so in the log with the Incident's name (and the count), so an absent reading is
// never silent.
func TestIncidentCountUnusableIsLogged(t *testing.T) {
	const name = "incident-a"
	cases := []struct {
		name  string
		uid   types.UID
		count int64
		want  []string
	}{
		{"no UID", "", 3, []string{"has no UID", name}},
		{"a negative count", "uid-a", -1, []string{"outside the accepted range", name, `"count"=-1`}},
		{"a count above the maximum", "uid-a", remediationv1alpha1.MaxIncidentCount + 1,
			[]string{"outside the accepted range", name}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inc := &remediationv1alpha1.Incident{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, UID: tc.uid}}
			inc.Status.Count = tc.count
			c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithObjects(inc).Build()
			r := &RemediationRequestReconciler{Client: c}
			rr := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "rr"}}
			rr.Spec.IncidentRef.Name = name
			var mu sync.Mutex
			var lines []string
			ctx := logf.IntoContext(context.Background(), funcr.New(func(_, args string) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, args)
			}, funcr.Options{}))
			if got := r.readIncidentCount(ctx, rr, countAtMerge); got != nil {
				t.Fatalf("readIncidentCount = %+v, want no reading", got)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, l := range lines {
				found := true
				for _, w := range tc.want {
					found = found && strings.Contains(l, w)
				}
				if found {
					return
				}
			}
			t.Fatalf("no log line with %q in %q", tc.want, lines)
		})
	}
}

// TestCountReadTimeout: the read of the Incident's count is taken only when the step's deadline leaves the
// read's own deadline plus countWriteReserve (the API server's default request timeout), so the status write
// keeps what it had without the read.
func TestCountReadTimeout(t *testing.T) {
	cases := []struct {
		name       string
		left       time.Duration
		configured time.Duration
		want       time.Duration
		ok         bool
	}{
		{"no deadline", 0, 0, DefaultIncidentReadTimeout, true},
		{"a full step", 2 * time.Minute, 0, DefaultIncidentReadTimeout, true},
		{"a configured read", 2 * time.Minute, 2 * time.Second, 2 * time.Second, true},
		{"half a second left", 500 * time.Millisecond, 0, 0, false},
		{"one second short of read plus reserve", DefaultIncidentReadTimeout + countWriteReserve - time.Second, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.left > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.left)
				defer cancel()
			}
			got, ok := countReadTimeout(ctx, tc.configured)
			if got != tc.want || ok != tc.ok {
				t.Errorf("countReadTimeout = %v, %v, want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestMergeReadingsTook: only the reading at the merge this process took counts, for the same request: not a
// reading another process wrote after this one's write was lost, and not one of an earlier request with the
// same name.
func TestMergeReadingsTook(t *testing.T) {
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	mine := &remediationv1alpha1.IncidentCountReading{UID: "incident-uid", Count: 3, ReadAt: metav1.NewTime(at)}
	request := func(uid types.UID, m *remediationv1alpha1.IncidentCountReading) *remediationv1alpha1.RemediationRequest {
		rr := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "rr", UID: uid}}
		rr.Status.Verification = &remediationv1alpha1.VerificationWindow{
			IncidentCount: &remediationv1alpha1.IncidentCountReadings{AtMerge: m}}
		return rr
	}
	var k mergeReadings
	if k.took(request("a", mine)) {
		t.Error("took a reading this process never noted")
	}
	k.note(request("a", mine), mine)
	if !k.took(request("a", mine.DeepCopy())) {
		t.Error("did not recognise the reading this process took")
	}
	other := mine.DeepCopy()
	other.ReadAt = metav1.NewTime(at.Add(30 * time.Minute))
	if k.took(request("a", other)) {
		t.Error("took a reading another process wrote")
	}
	if k.took(request("b", mine.DeepCopy())) {
		t.Error("took the reading of an earlier request with the same name")
	}
	if k.took(request("a", mine.DeepCopy())) {
		t.Error("kept the entry of an earlier request after a request with another UID was seen")
	}
}

// The entry kept for the reading at the applied time is of use only while that reading can still be taken.
func TestAwaitsCountAtApplied(t *testing.T) {
	at := metav1.Now()
	merged := func(mut func(*remediationv1alpha1.RemediationRequest)) *remediationv1alpha1.RemediationRequest {
		rr := &remediationv1alpha1.RemediationRequest{}
		rr.Status.Phase = remediationv1alpha1.PhaseMerged
		rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{}
		if mut != nil {
			mut(rr)
		}
		return rr
	}
	for name, tc := range map[string]struct {
		rr   *remediationv1alpha1.RemediationRequest
		want bool
	}{
		"merged, not applied": {merged(nil), true},
		"waiting for delivery": {merged(func(rr *remediationv1alpha1.RemediationRequest) {
			rr.Status.Phase = remediationv1alpha1.PhaseWaitingForDelivery
		}), true},
		"applied time recorded": {merged(func(rr *remediationv1alpha1.RemediationRequest) {
			rr.Status.PullRequest.AppliedObservedAt = &at
		}), false},
		"rollout check ended, still Merged": {merged(func(rr *remediationv1alpha1.RemediationRequest) {
			rr.Status.Rollout = &remediationv1alpha1.RolloutStatus{Ended: true}
		}), false},
		"ended by another writer": {merged(func(rr *remediationv1alpha1.RemediationRequest) {
			rr.Status.Phase = remediationv1alpha1.PhaseSucceeded
		}), false},
	} {
		if got := awaitsCountAtApplied(tc.rr); got != tc.want {
			t.Errorf("%s: awaitsCountAtApplied = %v, want %v", name, got, tc.want)
		}
	}
}
