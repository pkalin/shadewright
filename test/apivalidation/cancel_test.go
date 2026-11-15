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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

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

// cancelPhases says, for no phase and every phase of the schema, whether spec.cancel may be set in it. A phase the
// schema has and this map lacks fails the test that reads the schema's enum.
var cancelPhases = map[v1alpha1.RemediationPhase]bool{
	"":                               true,
	v1alpha1.PhasePending:            true,
	v1alpha1.PhasePolicyChecked:      true,
	v1alpha1.PhasePRCreated:          true,
	v1alpha1.PhaseAwaitingApproval:   true,
	v1alpha1.PhaseAutoMergeQueued:    true,
	v1alpha1.PhaseMerged:             false,
	v1alpha1.PhaseWaitingForDelivery: false,
	v1alpha1.PhaseVerifying:          false,
	v1alpha1.PhaseSucceeded:          false,
	v1alpha1.PhaseDenied:             false,
	v1alpha1.PhaseFailed:             false,
	v1alpha1.PhaseExpired:            false,
	v1alpha1.PhaseCancelled:          false,
	v1alpha1.PhaseSuperseded:         false,
	v1alpha1.PhaseNeedsIntervention:  false,
	v1alpha1.PhaseInconclusive:       false,
}

// receiptBranch is the target branch of the merge receipt createdWithPhase writes.
const receiptBranch = "main"

// createdWithPhase creates the fixture request and writes phase into its status.
func createdWithPhase(phase v1alpha1.RemediationPhase) *v1alpha1.RemediationRequest {
	rr := plainRequest()
	ExpectWithOffset(1, k8sClient.Create(ctx, rr)).To(Succeed())
	deleteAfterSpec(rr)
	if phase != "" {
		rr.Status.Phase = phase
		if phase == v1alpha1.PhaseWaitingForDelivery || phase == v1alpha1.PhaseVerifying {
			// The schema wants the merge receipt in these phases, and an applied time in Verifying.
			const merge = "89abcdef0123456789abcdef0123456789abcdef"
			mergedAt := metav1.NewMicroTime(time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC))
			applied := metav1.NewTime(mergedAt.Add(time.Minute).Truncate(time.Second))
			rr.Status.PullRequest = &v1alpha1.PullRequestStatus{Number: 1, URL: testPRURL, HeadSHA: testHeadSHA,
				MergeCommitSHA: merge, MergedAt: &mergedAt, AppliedObservedAt: &applied}
			rr.Status.MergeReceipt = &v1alpha1.MergeReceipt{
				Repository:   v1alpha1.PlanRepository{URL: "https://github.com/example/fleet", InstallationID: 7},
				TargetBranch: receiptBranch, PullRequest: 1, HeadSHA: testHeadSHA, MergeCommitSHA: merge, MergedAt: mergedAt,
			}
		}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, rr)).To(Succeed(), "phase %q", phase)
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

	It("classifies every phase of the schema as one the cancel is accepted in or refused in", func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases",
			"remediation.shadewright.io_remediationrequests.yaml"))
		Expect(err).NotTo(HaveOccurred())
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Expect(yaml.Unmarshal(raw, crd)).To(Succeed())
		Expect(crd.Spec.Versions).NotTo(BeEmpty())
		for _, v := range crd.Spec.Versions {
			phase := v.Schema.OpenAPIV3Schema.Properties["status"].Properties["phase"]
			Expect(phase.Enum).NotTo(BeEmpty(), "version %s", v.Name)
			enum := make([]v1alpha1.RemediationPhase, 0, len(phase.Enum))
			for _, e := range phase.Enum {
				var p v1alpha1.RemediationPhase
				Expect(json.Unmarshal(e.Raw, &p)).To(Succeed())
				enum = append(enum, p)
			}
			classified := make([]v1alpha1.RemediationPhase, 0, len(cancelPhases))
			for p := range cancelPhases {
				if p != "" {
					classified = append(classified, p)
				}
			}
			Expect(classified).To(ConsistOf(enum), "a new phase needs an entry in cancelPhases and a decision in "+
				"the cancel rule of RemediationRequest (api/v1alpha1/remediationrequest_types.go)")
		}
	})

	It("is accepted only before the merge and before a final phase", func() {
		for phase, ok := range cancelPhases {
			rr := createdWithPhase(phase)
			err := setCancel(rr, true)
			if ok {
				Expect(err).NotTo(HaveOccurred(), "phase %q", phase)
				continue
			}
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "phase %q: unexpected error: %v", phase, err)
			Expect(err.Error()).To(ContainSubstring("spec.cancel can be set only before the merge and before a final phase"))
			Expect(reread(rr).Spec.Cancel).To(BeFalse(), "phase %q", phase)
		}
	})

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
