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
	"reflect"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// spec.cancel (#289): the one field of the spec that can change after create.

// specChange is a change of one spec field after create: base is the request it is made on, change makes it. A nil
// change says that the API server cannot be sent another value of the field at all (why says why).
type specChange struct {
	base   func() *v1alpha1.RemediationRequest
	change func(*v1alpha1.RemediationRequestSpec)
	why    string
}

// plainRequest is the fixture request with a fresh name.
func plainRequest() *v1alpha1.RemediationRequest {
	return fx.RemediationRequest("default", unique("rr"))
}

// specChanges holds, per JSON name of a spec field other than cancel, a change the immutability rule must refuse.
var specChanges = map[string]specChange{
	"incidentRef": {base: plainRequest, change: func(s *v1alpha1.RemediationRequestSpec) { s.IncidentRef.Name = otherRequest }},
	"targetRef":   {base: plainRequest, change: func(s *v1alpha1.RemediationRequestSpec) { s.TargetRef.Name = otherRequest }},
	"mode": {why: "GitOps is the only value of the enum and the default, so no other value passes the schema; " +
		"the rule compares it all the same"},
	"approvalRequired": {base: plainRequest, change: func(s *v1alpha1.RemediationRequestSpec) { s.ApprovalRequired = new(false) }},
	"policyRef":        {base: plainRequest, change: func(s *v1alpha1.RemediationRequestSpec) { s.PolicyRef = otherRequest }},
	"action": {base: plainRequest, change: func(s *v1alpha1.RemediationRequestSpec) {
		s.Action.AdjustKedaMaxReplicas.DesiredValue = 16
	}},
	"verification": {base: plainRequest, change: func(s *v1alpha1.RemediationRequestSpec) {
		s.Verification.Timeout = &metav1.Duration{Duration: 20 * time.Minute}
	}},
	"replaces": {base: func() *v1alpha1.RemediationRequest { return replacementRequest(unique("rr")) },
		change: func(s *v1alpha1.RemediationRequestSpec) { s.Replaces.Name = otherRequest }},
	"chainRoot": {base: func() *v1alpha1.RemediationRequest { return replacementRequest(unique("rr")) },
		change: func(s *v1alpha1.RemediationRequestSpec) { s.ChainRoot = testFailedUID }},
	"reverts": {base: revertRequest, change: func(s *v1alpha1.RemediationRequestSpec) { s.Reverts.UID = testRootUID }},
}

// specFields returns the JSON names of the fields of RemediationRequestSpec.
func specFields() []string {
	t := reflect.TypeFor[v1alpha1.RemediationRequestSpec]()
	out := make([]string, 0, t.NumField())
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out = append(out, name)
	}
	return out
}

// patchSpec changes a fresh read of rr with change and sends the difference as a JSON merge patch, as
// `kubectl patch --type merge` does. A typed Update would send the whole spec, with the durations of
// spec.verification spelt as Go spells them ("10m0s" for the stored "10m"), which the immutability rule refuses
// whatever else changed.
func patchSpec(rr *v1alpha1.RemediationRequest, change func(*v1alpha1.RemediationRequestSpec)) error {
	got := reread(rr)
	base := got.DeepCopy()
	change(&got.Spec)
	return k8sClient.Patch(ctx, got, client.MergeFrom(base))
}

// setCancel sets spec.cancel to cancel (false removes it) with a merge patch and returns its error.
func setCancel(rr *v1alpha1.RemediationRequest, cancel bool) error {
	return patchSpec(rr, func(s *v1alpha1.RemediationRequestSpec) { s.Cancel = cancel })
}

// createdWithPhase creates the fixture request and writes phase into its status.
func createdWithPhase(phase v1alpha1.RemediationPhase) *v1alpha1.RemediationRequest {
	rr := plainRequest()
	ExpectWithOffset(1, k8sClient.Create(ctx, rr)).To(Succeed())
	deleteAfterSpec(rr)
	if phase != "" {
		rr.Status.Phase = phase
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, rr)).To(Succeed())
	}
	return rr
}

var _ = Describe("RemediationRequest spec.cancel", func() {
	It("names a change for every field of the spec but cancel", func() {
		fields := slices.DeleteFunc(specFields(), func(f string) bool { return f == "cancel" })
		Expect(specFields()).To(ContainElement("cancel"))
		keys := make([]string, 0, len(specChanges))
		for k := range specChanges {
			keys = append(keys, k)
		}
		Expect(keys).To(ConsistOf(fields), "a new spec field needs a change here and a term in the spec's "+
			"immutability rule (api/v1alpha1/remediationrequest_types.go)")
	})

	It("refuses a change of any other field, alone or together with cancel, and accepts cancel alone", func() {
		for field, c := range specChanges {
			if c.change == nil {
				Expect(c.why).NotTo(BeEmpty(), field)
				continue
			}
			rr := c.base()
			Expect(k8sClient.Create(ctx, rr)).To(Succeed(), field)
			deleteAfterSpec(rr)
			for _, cancel := range []bool{false, true} {
				err := patchSpec(rr, func(s *v1alpha1.RemediationRequestSpec) {
					c.change(s)
					s.Cancel = cancel
				})
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s (cancel %t): unexpected error: %v", field, cancel, err)
				Expect(err.Error()).To(ContainSubstring("spec is immutable"), field)
			}
			Expect(setCancel(rr, true)).To(Succeed(), field)
			Expect(reread(rr).Spec.Cancel).To(BeTrue(), field)
		}
	})

	It("accepts cancel on create", func() {
		rr := plainRequest()
		rr.Spec.Cancel = true
		expectAccepted(rr)
	})

	It("can be set, never unset", func() {
		rr := createdWithPhase(v1alpha1.PhaseAwaitingApproval)
		Expect(setCancel(rr, false)).To(Succeed(), "false on a request without it changes nothing")
		Expect(setCancel(rr, true)).To(Succeed())
		Expect(setCancel(rr, true)).To(Succeed(), "setting it again changes nothing")
		err := setCancel(rr, false)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.cancel can be set, never unset"))
	})

	DescribeTable("is accepted only before the merge and before a final phase",
		func(phase v1alpha1.RemediationPhase, ok bool) {
			rr := createdWithPhase(phase)
			err := setCancel(rr, true)
			if ok {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
			Expect(err.Error()).To(ContainSubstring("spec.cancel can be set only before the merge and before a final phase"))
			Expect(reread(rr).Spec.Cancel).To(BeFalse())
		},
		Entry("no phase", v1alpha1.RemediationPhase(""), true),
		Entry("Pending", v1alpha1.PhasePending, true),
		Entry("PolicyChecked", v1alpha1.PhasePolicyChecked, true),
		Entry("PRCreated", v1alpha1.PhasePRCreated, true),
		Entry("AwaitingApproval", v1alpha1.PhaseAwaitingApproval, true),
		Entry("AutoMergeQueued", v1alpha1.PhaseAutoMergeQueued, true),
		Entry("Merged", v1alpha1.PhaseMerged, false),
		Entry("Succeeded", v1alpha1.PhaseSucceeded, false),
		Entry("Denied", v1alpha1.PhaseDenied, false),
		Entry("Failed", v1alpha1.PhaseFailed, false),
		Entry("Expired", v1alpha1.PhaseExpired, false),
		Entry("Cancelled", v1alpha1.PhaseCancelled, false),
		Entry("Superseded", v1alpha1.PhaseSuperseded, false),
		Entry("NeedsIntervention", v1alpha1.PhaseNeedsIntervention, false),
		Entry("Inconclusive", v1alpha1.PhaseInconclusive, false),
	)

	It("keeps a cancel set before a final phase, and lets the status reach any phase", func() {
		for _, end := range []v1alpha1.RemediationPhase{v1alpha1.PhaseCancelled, v1alpha1.PhaseMerged} {
			rr := createdWithPhase(v1alpha1.PhaseAwaitingApproval)
			Expect(setCancel(rr, true)).To(Succeed())
			got := reread(rr)
			got.Status.Phase = end
			Expect(k8sClient.Status().Update(ctx, got)).To(Succeed(), "%s: the executor's status write", end)
			// A write of metadata on the ended request still goes through: cancel does not change.
			got = reread(rr)
			base := got.DeepCopy()
			got.Labels = map[string]string{"example": "label"}
			Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed(), end)
			err := setCancel(rr, false)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: unexpected error: %v", end, err)
		}
	})
})
