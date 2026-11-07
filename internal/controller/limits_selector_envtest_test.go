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
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// The limits read requests with field selectors on the CRD's selectable fields (#293, #390). These specs check on
// a real API server that it honours them: the first list returns a request without a phase (just created) and
// one in every phase that has not ended; the lock list returns a stopped holder and requests without
// status.holdsTarget (a missing field is the empty value) but not one that released it; the breaker's list
// returns the requests whose status.breakerCounts is true or missing, in any phase, but not one set to false.
var _ = Describe("RemediationRequest selectable fields", Ordered, func() {
	ctx := context.Background()
	const ns = "limits-selectors"
	notEnded := []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhasePending,
		remediationv1alpha1.PhasePolicyChecked, remediationv1alpha1.PhasePRCreated,
		remediationv1alpha1.PhaseAwaitingApproval, remediationv1alpha1.PhaseAutoMergeQueued}
	all := append(append([]remediationv1alpha1.RemediationPhase{""}, notEnded...), endedPhases...)

	name := func(p remediationv1alpha1.RemediationPhase) string {
		if p == "" {
			return "no-phase"
		}
		return strings.ToLower(string(p))
	}

	BeforeAll(func() {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		for _, p := range all {
			rr := fx.MemoryLimitRequest(ns, name(p))
			Expect(k8sClient.Create(ctx, rr)).To(Succeed())
			if p == "" {
				continue
			}
			rr.Status.Phase = p
			// The breaker's watermark: one it needs, one it no longer needs; the others never had the field.
			switch p {
			case remediationv1alpha1.PhaseMerged:
				rr.Status.BreakerCounts = new(true)
			case remediationv1alpha1.PhaseFailed:
				rr.Status.BreakerCounts = new(false)
			}
			// The lock: a stopped holder, one that released it; the others never had the field.
			switch p {
			case remediationv1alpha1.PhaseCancelled:
				rr.Status.HoldsTarget = new(true)
			case remediationv1alpha1.PhaseSuperseded:
				rr.Status.HoldsTarget = new(false)
			}
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		}
	})

	It("lists each of the three lists, followed over pages", func() {
		// With a field selector the API server fills a page from a scan of limit objects and may return a short
		// (even empty) page with a continue token, so listPages follows the token rather than the page size.
		r := &RemediationRequestReconciler{APIReader: k8sClient}
		sels := limitsSelectors()
		Expect(sels).To(HaveLen(3))
		var read, holders, counted []string
		for _, p := range all {
			if !slices.Contains(endedPhases, p) {
				read = append(read, name(p))
			}
			// Superseded released the lock (false); every other one is true or has no field.
			if p != remediationv1alpha1.PhaseSuperseded {
				holders = append(holders, name(p))
			}
			// Failed is no longer needed by the breaker (false); every other one is true or has no field.
			if p != remediationv1alpha1.PhaseFailed {
				counted = append(counted, name(p))
			}
		}
		for i, want := range [][]string{read, holders, counted} {
			var got []string
			Expect(r.listPages(ctx, ns, sels[i], func(o *remediationv1alpha1.RemediationRequest) {
				got = append(got, o.Name)
			})).To(Succeed(), sels[i].String())
			Expect(got).To(ConsistOf(want), sels[i].String())
		}
	})
})
