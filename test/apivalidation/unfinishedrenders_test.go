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

package apivalidation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// status.unfinishedRenders, the executor's mark of render checks that started and did not finish (#714,
// ADR-0035).
var _ = Describe("RemediationRequest status.unfinishedRenders", func() {
	mark := func(digest string, count int32) *v1alpha1.UnfinishedRenders {
		return &v1alpha1.UnfinishedRenders{PlanDigest: testDigest(digest), Count: count}
	}
	// marked creates a request and stores the mark m in its status.
	marked := func(m *v1alpha1.UnfinishedRenders) *v1alpha1.RemediationRequest {
		rr := createdRequest(false)
		rr.Status.UnfinishedRenders = m
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, rr)).To(Succeed())
		return reread(rr)
	}

	It("keeps a request stored without it valid", func() {
		rr := createdRequest(false)
		rr.Status.Phase = v1alpha1.PhasePending
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	It("accepts counts from 1 to MaxUnfinishedRenders, one write at a time", func() {
		rr := createdRequest(false)
		for n := int32(1); n <= v1alpha1.MaxUnfinishedRenders; n++ {
			rr.Status.UnfinishedRenders = mark("a", n)
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed(), "count %d", n)
		}
	})

	DescribeTable("refuses a malformed mark",
		func(m *v1alpha1.UnfinishedRenders, msg string) {
			rr := createdRequest(false)
			rr.Status.UnfinishedRenders = m
			expectStatusRefused(rr, msg)
		},
		Entry("a count of 0", mark("a", 0), "status.unfinishedRenders.count"),
		Entry("a count over the bound", mark("a", v1alpha1.MaxUnfinishedRenders+1), "status.unfinishedRenders.count"),
		Entry("a digest without its algorithm", &v1alpha1.UnfinishedRenders{PlanDigest: "not-a-digest", Count: 1},
			"status.unfinishedRenders.planDigest"),
	)

	It("refuses a lower count for the same plan digest", func() {
		rr := marked(mark("a", 2))
		rr.Status.UnfinishedRenders = mark("a", 1)
		expectStatusRefused(rr, "status.unfinishedRenders.count never decreases for the same planDigest")
	})

	It("accepts removing the mark, and a lower count for another plan digest", func() {
		rr := marked(mark("a", 3))
		rr.Status.UnfinishedRenders = mark("b", 1)
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		rr = reread(rr)
		rr.Status.UnfinishedRenders = nil
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		rr = reread(rr)
		Expect(rr.Status.UnfinishedRenders).To(BeNil())
		rr.Status.UnfinishedRenders = mark("b", 1)
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed(), "a new mark after the removal")
	})
})
