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
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// The revert link of ADR-0030 section 8: spec.reverts.

const testFailedUID = "66666666-6666-4666-8666-666666666666"

// revertRequest is the fixture request as a revert of the request with testFailedUID.
func revertRequest() *v1alpha1.RemediationRequest {
	rr := fx.RemediationRequest("default", unique("revert"))
	rr.Spec.Reverts = &v1alpha1.RequestRef{Name: testPredecessor, UID: testFailedUID}
	return rr
}

var _ = Describe("RemediationRequest reverts", func() {
	It("accepts a revert with approvalRequired true", func() {
		rr := revertRequest()
		rr.Spec.ApprovalRequired = new(true)
		expectAccepted(rr)
	})

	It("accepts a revert without approvalRequired, which defaults to true", func() {
		rr := revertRequest()
		rr.Spec.ApprovalRequired = nil
		expectAccepted(rr)
		Expect(*reread(rr).Spec.ApprovalRequired).To(BeTrue())
	})

	It("refuses a revert with approvalRequired false", func() {
		rr := revertRequest()
		rr.Spec.ApprovalRequired = new(false)
		expectRefused(rr, "a revert (spec.reverts) requires approvalRequired: true")
	})

	It("refuses a revert that also replaces a request", func() {
		rr := revertRequest()
		rr.Spec.Replaces = &v1alpha1.RequestReplacement{Name: testPredecessor, UID: testRootUID, Hint: testHintRef()}
		rr.Spec.ChainRoot = testRootUID
		expectRefused(rr, "a revert (spec.reverts) never replaces another request")
	})

	DescribeTable("refuses a malformed link",
		func(change func(*v1alpha1.RequestRef), field string) {
			rr := revertRequest()
			change(rr.Spec.Reverts)
			expectRefused(rr, field)
		},
		Entry("a name that is not a DNS name", func(r *v1alpha1.RequestRef) { r.Name = "Remediation_A" }, "spec.reverts.name"),
		Entry("an empty name", func(r *v1alpha1.RequestRef) { r.Name = "" }, "spec.reverts.name"),
		Entry("a UID that is not one", func(r *v1alpha1.RequestRef) { r.UID = "failed" }, "spec.reverts.uid"),
	)

	It("refuses adding, changing or removing it after creation", func() {
		plain := fx.RemediationRequest("default", unique("rr"))
		Expect(k8sClient.Create(ctx, plain)).To(Succeed())
		deleteAfterSpec(plain)
		plain.Spec.Reverts = &v1alpha1.RequestRef{Name: testPredecessor, UID: testFailedUID}
		err := k8sClient.Update(ctx, plain)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "adding: unexpected error: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec is immutable"))

		rev := revertRequest()
		Expect(k8sClient.Create(ctx, rev)).To(Succeed())
		deleteAfterSpec(rev)
		for name, change := range map[string]func(*v1alpha1.RemediationRequestSpec){
			"another UID":   func(s *v1alpha1.RemediationRequestSpec) { s.Reverts.UID = testRootUID },
			"another name":  func(s *v1alpha1.RemediationRequestSpec) { s.Reverts.Name = otherRequest },
			"removed again": func(s *v1alpha1.RemediationRequestSpec) { s.Reverts = nil },
		} {
			got := reread(rev)
			change(&got.Spec)
			err := k8sClient.Update(ctx, got)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: unexpected error: %v", name, err)
			Expect(err.Error()).To(ContainSubstring("spec is immutable"), name)
		}
	})
})
