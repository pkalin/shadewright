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

// The reviewer hints and the supersede handoff of ADR-0028: spec.replaces and spec.chainRoot, status.hints,
// status.handoff and the Incident's status.handledHint.

const (
	testRootUID     = "44444444-4444-4444-8444-444444444444"
	testPredecessor = "remediation-0123456789abcdef01234567"
	otherRequest    = "remediation-other"
	statusEmptied   = "status emptied"
)

func testDigest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// testHintRef pins hint 1.
func testHintRef() v1alpha1.HintRef {
	return v1alpha1.HintRef{Source: v1alpha1.HintSourceGitHub, CommentID: 1, Digest: testDigest("a")}
}

func testHint(commentID int64, state v1alpha1.HintState) v1alpha1.Hint {
	at := metav1.NewTime(time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC))
	h := v1alpha1.Hint{
		Source: v1alpha1.HintSourceGitHub, CommentID: commentID,
		Author: v1alpha1.HintAuthor{ID: 42, Login: "octo-cat"},
		Text:   "the leak is in the sidecar, not the app", Digest: testDigest("a"),
		UpdatedAt: at, AcceptedAt: at, State: state,
	}
	if state == v1alpha1.HintDeclined {
		h.Reason = "BudgetSpent"
	}
	return h
}

// replacementRequest is the fixture request as a replacement of testPredecessor, answering hint 1.
func replacementRequest(name string) *v1alpha1.RemediationRequest {
	rr := fx.RemediationRequest("default", name)
	rr.Spec.Replaces = &v1alpha1.RequestReplacement{Name: testPredecessor, UID: testRootUID, Hint: testHintRef()}
	rr.Spec.ChainRoot = testRootUID
	return rr
}

// createdRequest creates the fixture request (a replacement when replacement is true) and returns it.
func createdRequest(replacement bool) *v1alpha1.RemediationRequest {
	rr := fx.RemediationRequest("default", unique("rr"))
	if replacement {
		rr = replacementRequest(unique("rr"))
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, rr)).To(Succeed())
	deleteAfterSpec(rr)
	return rr
}

// expectStatusRefused asserts the API server refuses the status of rr as invalid, with msg in the error.
func expectStatusRefused(rr *v1alpha1.RemediationRequest, msg string) {
	err := k8sClient.Status().Update(ctx, rr)
	ExpectWithOffset(1, err).To(HaveOccurred(), "the API server accepted an invalid status")
	ExpectWithOffset(1, apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
	ExpectWithOffset(1, err.Error()).To(ContainSubstring(msg))
}

// reread returns rr as the API server holds it.
func reread(rr *v1alpha1.RemediationRequest) *v1alpha1.RemediationRequest {
	got := &v1alpha1.RemediationRequest{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(rr), got)).To(Succeed())
	return got
}

var _ = Describe("RemediationRequest replaces and chainRoot", func() {
	It("accepts a replacement with both", func() {
		expectAccepted(replacementRequest(unique("rr")))
	})

	It("refuses replaces without chainRoot", func() {
		rr := replacementRequest(unique("rr"))
		rr.Spec.ChainRoot = ""
		expectRefused(rr, "replaces and chainRoot are set together or not at all")
	})

	It("refuses chainRoot without replaces", func() {
		rr := fx.RemediationRequest("default", unique("rr"))
		rr.Spec.ChainRoot = testRootUID
		expectRefused(rr, "replaces and chainRoot are set together or not at all")
	})

	DescribeTable("refuses a malformed link",
		func(change func(*v1alpha1.RequestReplacement), field string) {
			rr := replacementRequest(unique("rr"))
			change(rr.Spec.Replaces)
			expectRefused(rr, field)
		},
		Entry("an unknown source", func(r *v1alpha1.RequestReplacement) { r.Hint.Source = "Slack" }, "spec.replaces.hint.source"),
		Entry("comment ID 0", func(r *v1alpha1.RequestReplacement) { r.Hint.CommentID = 0 }, "spec.replaces.hint.commentID"),
		Entry("a digest without its algorithm", func(r *v1alpha1.RequestReplacement) { r.Hint.Digest = strings.Repeat("a", 64) }, "spec.replaces.hint.digest"),
		Entry("a short digest", func(r *v1alpha1.RequestReplacement) { r.Hint.Digest = "sha256:abc" }, "spec.replaces.hint.digest"),
		Entry("a name that is not a DNS label", func(r *v1alpha1.RequestReplacement) { r.Name = "Remediation_A" }, "spec.replaces.name"),
		Entry("a UID that is not one", func(r *v1alpha1.RequestReplacement) { r.UID = "root" }, "spec.replaces.uid"),
	)

	It("refuses a chainRoot that is not a UID", func() {
		rr := replacementRequest(unique("rr"))
		rr.Spec.ChainRoot = "root"
		expectRefused(rr, "spec.chainRoot")
	})

	It("refuses adding, changing or removing them after creation", func() {
		plain := createdRequest(false)
		plain.Spec.Replaces = &v1alpha1.RequestReplacement{Name: testPredecessor, UID: testRootUID, Hint: testHintRef()}
		plain.Spec.ChainRoot = testRootUID
		err := k8sClient.Update(ctx, plain)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "adding: unexpected error: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec is immutable"))

		repl := createdRequest(true)
		for name, change := range map[string]func(*v1alpha1.RemediationRequestSpec){
			"another hint":      func(s *v1alpha1.RemediationRequestSpec) { s.Replaces.Hint.CommentID = 2 },
			"another chainRoot": func(s *v1alpha1.RemediationRequestSpec) { s.ChainRoot = "55555555-5555-4555-8555-555555555555" },
			"both removed":      func(s *v1alpha1.RemediationRequestSpec) { s.Replaces, s.ChainRoot = nil, "" },
		} {
			got := reread(repl)
			change(&got.Spec)
			err := k8sClient.Update(ctx, got)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: unexpected error: %v", name, err)
			Expect(err.Error()).To(ContainSubstring("spec is immutable"), name)
		}
	})
})

var _ = Describe("RemediationRequest status.hints", func() {
	It("accepts MaxHints hints at their bounds", func() {
		rr := createdRequest(false)
		for i := range v1alpha1.MaxHints {
			h := testHint(int64(i+1), v1alpha1.HintPending)
			h.Text = strings.Repeat("a", v1alpha1.MaxHintTextBytes)
			h.Author.Login = strings.Repeat("b", 100)
			rr.Status.Hints = append(rr.Status.Hints, h)
		}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	It("refuses a fourth hint", func() {
		rr := createdRequest(false)
		for i := range v1alpha1.MaxHints + 1 {
			rr.Status.Hints = append(rr.Status.Hints, testHint(int64(i+1), v1alpha1.HintConsumed))
		}
		expectStatusRefused(rr, "status.hints: Too many")
	})

	It("refuses text over 2048 bytes in fewer characters", func() {
		rr := createdRequest(false)
		h := testHint(1, v1alpha1.HintPending)
		h.Text = strings.Repeat("ż", 1100) // 1100 characters, 2200 bytes
		rr.Status.Hints = []v1alpha1.Hint{h}
		expectStatusRefused(rr, "text must be at most 2048 bytes")
	})

	It("accepts 2048 bytes of multi-byte text", func() {
		rr := createdRequest(false)
		h := testHint(1, v1alpha1.HintPending)
		h.Text = strings.Repeat("ż", 1024)
		rr.Status.Hints = []v1alpha1.Hint{h}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	DescribeTable("refuses a hint out of its rules",
		func(change func(*v1alpha1.Hint), msg string) {
			rr := createdRequest(false)
			h := testHint(1, v1alpha1.HintPending)
			change(&h)
			rr.Status.Hints = []v1alpha1.Hint{h}
			expectStatusRefused(rr, msg)
		},
		Entry("text over 2048 characters", func(h *v1alpha1.Hint) { h.Text = strings.Repeat("a", 2049) }, "status.hints[0].text"),
		Entry("empty text", func(h *v1alpha1.Hint) { h.Text = "" }, "status.hints[0].text"),
		Entry("an unknown source", func(h *v1alpha1.Hint) { h.Source = "AzureDevOps" }, "status.hints[0].source"),
		Entry("a digest without its algorithm", func(h *v1alpha1.Hint) { h.Digest = strings.Repeat("a", 64) }, "status.hints[0].digest"),
		Entry("a digest in upper case", func(h *v1alpha1.Hint) { h.Digest = "sha256:" + strings.Repeat("A", 64) }, "status.hints[0].digest"),
		Entry("comment ID 0", func(h *v1alpha1.Hint) { h.CommentID = 0 }, "status.hints[0].commentID"),
		Entry("author ID 0", func(h *v1alpha1.Hint) { h.Author.ID = 0 }, "status.hints[0].author.id"),
		Entry("a login with a space", func(h *v1alpha1.Hint) { h.Author.Login = "octo cat" }, "status.hints[0].author.login"),
		Entry("an unknown state", func(h *v1alpha1.Hint) { h.State = "Used" }, "status.hints[0].state"),
		Entry("Declined without a reason", func(h *v1alpha1.Hint) { h.State = v1alpha1.HintDeclined }, "a Declined hint needs a reason"),
		Entry("a reason that is not a CamelCase word", func(h *v1alpha1.Hint) {
			h.State, h.Reason = v1alpha1.HintDeclined, "budget spent"
		}, "status.hints[0].reason"),
	)

	It("refuses two hints for the same comment", func() {
		rr := createdRequest(false)
		rr.Status.Hints = []v1alpha1.Hint{testHint(1, v1alpha1.HintPending), testHint(1, v1alpha1.HintDeclined)}
		expectStatusRefused(rr, "Duplicate value")
	})

	It("moves a hint only forward", func() {
		for _, path := range [][]v1alpha1.HintState{
			{v1alpha1.HintPending, v1alpha1.HintReserved, v1alpha1.HintConsumed},
			{v1alpha1.HintPending, v1alpha1.HintDeclined},
			{v1alpha1.HintPending, v1alpha1.HintReserved, v1alpha1.HintDeclined},
		} {
			rr := createdRequest(false)
			for _, state := range path {
				rr.Status.Hints = []v1alpha1.Hint{testHint(1, state)}
				Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed(), "%v: to %s", path, state)
			}
		}
		for _, step := range [][2]v1alpha1.HintState{
			{v1alpha1.HintPending, v1alpha1.HintConsumed},
			{v1alpha1.HintReserved, v1alpha1.HintPending},
			{v1alpha1.HintConsumed, v1alpha1.HintPending},
			{v1alpha1.HintConsumed, v1alpha1.HintDeclined},
			{v1alpha1.HintDeclined, v1alpha1.HintPending},
			{v1alpha1.HintDeclined, v1alpha1.HintReserved},
		} {
			rr := createdRequest(false)
			rr.Status.Hints = []v1alpha1.Hint{testHint(1, step[0])}
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
			rr.Status.Hints = []v1alpha1.Hint{testHint(1, step[1])}
			expectStatusRefused(rr, "a hint moves only from Pending to Reserved or Declined")
		}
	})

	It("keeps a stored hint pinned", func() {
		rr := createdRequest(false)
		rr.Status.Hints = []v1alpha1.Hint{testHint(1, v1alpha1.HintPending)}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		later := metav1.NewTime(time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC))
		for name, change := range map[string]func(*v1alpha1.Hint){
			"text edited":      func(h *v1alpha1.Hint) { h.Text = "the leak is in the app" },
			"digest changed":   func(h *v1alpha1.Hint) { h.Digest = testDigest("b") },
			"author changed":   func(h *v1alpha1.Hint) { h.Author.ID = 43 },
			"login changed":    func(h *v1alpha1.Hint) { h.Author.Login = "someone-else" },
			"updatedAt moved":  func(h *v1alpha1.Hint) { h.UpdatedAt = later },
			"acceptedAt moved": func(h *v1alpha1.Hint) { h.AcceptedAt = later },
		} {
			got := reread(rr)
			change(&got.Status.Hints[0])
			err := k8sClient.Status().Update(ctx, got)
			Expect(err).To(HaveOccurred(), "the API server accepted: "+name)
			Expect(err.Error()).To(ContainSubstring("a stored hint is pinned"), name)
		}
	})

	It("never removes a stored hint", func() {
		rr := createdRequest(false)
		rr.Status.Hints = []v1alpha1.Hint{testHint(1, v1alpha1.HintConsumed), testHint(2, v1alpha1.HintPending)}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		for name, change := range map[string]func(*v1alpha1.RemediationRequest){
			"one removed": func(r *v1alpha1.RemediationRequest) { r.Status.Hints = r.Status.Hints[:1] },
			"one replaced by another comment": func(r *v1alpha1.RemediationRequest) {
				r.Status.Hints[1] = testHint(3, v1alpha1.HintPending)
			},
			"all removed": func(r *v1alpha1.RemediationRequest) { r.Status.Hints = nil },
			statusEmptied: func(r *v1alpha1.RemediationRequest) { r.Status = v1alpha1.RemediationRequestStatus{} },
		} {
			got := reread(rr)
			change(got)
			err := k8sClient.Status().Update(ctx, got)
			Expect(err).To(HaveOccurred(), "the API server accepted: "+name)
			Expect(err.Error()).To(ContainSubstring("a stored hint is never removed"), name)
		}
	})
})

var _ = Describe("RemediationRequest status.handoff", func() {
	replacementHandoff := func(phase v1alpha1.HandoffPhase) *v1alpha1.Handoff {
		return &v1alpha1.Handoff{From: testPredecessor, Hint: testHintRef(), Phase: phase}
	}
	predecessorHandoff := func(phase v1alpha1.HandoffPhase) *v1alpha1.Handoff {
		return &v1alpha1.Handoff{To: "remediation-next", Hint: testHintRef(), ExpectedHead: testHeadSHA, Phase: phase}
	}

	It("accepts the replacement's record from Reserved to Done", func() {
		rr := createdRequest(true)
		rr.Status.Handoff = replacementHandoff(v1alpha1.HandoffReserved)
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		rr.Status.Handoff.Phase = v1alpha1.HandoffDone
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	It("accepts the predecessor's record from Intent to Aborted", func() {
		rr := createdRequest(false)
		rr.Status.Handoff = predecessorHandoff(v1alpha1.HandoffIntent)
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		rr.Status.Handoff.Phase = v1alpha1.HandoffAborted
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	DescribeTable("refuses a record out of its rules",
		func(replacement bool, handoff func() *v1alpha1.Handoff, msg string) {
			rr := createdRequest(replacement)
			rr.Status.Handoff = handoff()
			expectStatusRefused(rr, msg)
		},
		Entry("neither from nor to", false, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{Hint: testHintRef(), Phase: v1alpha1.HandoffDone}
		}, "exactly one of from and to is set"),
		Entry("both from and to", true, func() *v1alpha1.Handoff {
			h := replacementHandoff(v1alpha1.HandoffReserved)
			h.To = "remediation-next"
			return h
		}, "exactly one of from and to is set"),
		Entry("a replacement in Intent", true, func() *v1alpha1.Handoff {
			return replacementHandoff(v1alpha1.HandoffIntent)
		}, "the replacement's handoff is Reserved, Done or Aborted"),
		Entry("a replacement with expectedHead", true, func() *v1alpha1.Handoff {
			h := replacementHandoff(v1alpha1.HandoffReserved)
			h.ExpectedHead = testHeadSHA
			return h
		}, "expectedHead is recorded on the predecessor only"),
		Entry("a predecessor in Reserved", false, func() *v1alpha1.Handoff {
			return predecessorHandoff(v1alpha1.HandoffReserved)
		}, "the predecessor's handoff is Intent, Done or Aborted, with expectedHead"),
		Entry("a predecessor without expectedHead", false, func() *v1alpha1.Handoff {
			h := predecessorHandoff(v1alpha1.HandoffIntent)
			h.ExpectedHead = ""
			return h
		}, "the predecessor's handoff is Intent, Done or Aborted, with expectedHead"),
		Entry("an expectedHead that is not a full SHA", false, func() *v1alpha1.Handoff {
			h := predecessorHandoff(v1alpha1.HandoffIntent)
			h.ExpectedHead = "0123abc"
			return h
		}, "status.handoff.expectedHead"),
		Entry("an unknown phase", false, func() *v1alpha1.Handoff {
			return predecessorHandoff("Started")
		}, "status.handoff.phase"),
		Entry("from on a request that replaces nothing", false, func() *v1alpha1.Handoff {
			return replacementHandoff(v1alpha1.HandoffReserved)
		}, "status.handoff.from and its hint must match spec.replaces"),
		Entry("from naming another request", true, func() *v1alpha1.Handoff {
			h := replacementHandoff(v1alpha1.HandoffReserved)
			h.From = otherRequest
			return h
		}, "status.handoff.from and its hint must match spec.replaces"),
		Entry("from with another hint", true, func() *v1alpha1.Handoff {
			h := replacementHandoff(v1alpha1.HandoffReserved)
			h.Hint.CommentID = 2
			return h
		}, "status.handoff.from and its hint must match spec.replaces"),
	)

	It("moves only from Reserved or Intent to Done or Aborted", func() {
		for _, step := range []struct {
			replacement bool
			from, to    v1alpha1.HandoffPhase
		}{
			{true, v1alpha1.HandoffDone, v1alpha1.HandoffReserved},
			{true, v1alpha1.HandoffAborted, v1alpha1.HandoffDone},
			{false, v1alpha1.HandoffDone, v1alpha1.HandoffIntent},
			{false, v1alpha1.HandoffAborted, v1alpha1.HandoffIntent},
		} {
			rr := createdRequest(step.replacement)
			rr.Status.Handoff = predecessorHandoff(step.from)
			if step.replacement {
				rr.Status.Handoff = replacementHandoff(step.from)
			}
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
			rr.Status.Handoff.Phase = step.to
			expectStatusRefused(rr, "a handoff moves only from Reserved or Intent to Done or Aborted")
		}
	})

	It("keeps the record once written", func() {
		rr := createdRequest(false)
		rr.Status.Handoff = predecessorHandoff(v1alpha1.HandoffIntent)
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		for name, change := range map[string]func(*v1alpha1.RemediationRequest){
			"removed":      func(r *v1alpha1.RemediationRequest) { r.Status.Handoff = nil },
			statusEmptied:  func(r *v1alpha1.RemediationRequest) { r.Status = v1alpha1.RemediationRequestStatus{} },
			"to changed":   func(r *v1alpha1.RemediationRequest) { r.Status.Handoff.To = otherRequest },
			"hint changed": func(r *v1alpha1.RemediationRequest) { r.Status.Handoff.Hint.CommentID = 2 },
			"turned into from": func(r *v1alpha1.RemediationRequest) {
				r.Status.Handoff.From, r.Status.Handoff.To = otherRequest, ""
			},
		} {
			got := reread(rr)
			change(got)
			err := k8sClient.Status().Update(ctx, got)
			Expect(err).To(HaveOccurred(), "the API server accepted: "+name)
			Expect(err.Error()).To(ContainSubstring("status.handoff is set once"), name)
		}
	})
})

var _ = Describe("Incident status.handledHint", func() {
	update := func(ref v1alpha1.HintRef) error {
		inc := fx.Incident("default", unique("inc"))
		Expect(k8sClient.Create(ctx, inc)).To(Succeed())
		deleteAfterSpec(inc)
		inc.Status.HandledHint = &ref
		return k8sClient.Status().Update(ctx, inc)
	}

	It("accepts a pinned hint", func() {
		Expect(update(testHintRef())).To(Succeed())
	})

	DescribeTable("refuses a malformed pin",
		func(change func(*v1alpha1.HintRef), field string) {
			ref := testHintRef()
			change(&ref)
			err := update(ref)
			Expect(err).To(HaveOccurred(), "the API server accepted an invalid handledHint")
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "unexpected error: %v", err)
			Expect(err.Error()).To(ContainSubstring(field))
		},
		Entry("an unknown source", func(r *v1alpha1.HintRef) { r.Source = "Slack" }, "status.handledHint.source"),
		Entry("comment ID 0", func(r *v1alpha1.HintRef) { r.CommentID = 0 }, "status.handledHint.commentID"),
		Entry("a malformed digest", func(r *v1alpha1.HintRef) { r.Digest = "md5:abc" }, "status.handledHint.digest"),
	)
})
