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
