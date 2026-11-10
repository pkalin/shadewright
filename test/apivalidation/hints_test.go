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
	It("accepts MaxStoredHints hints at their bounds, MaxHints of them Consumed or Reserved", func() {
		rr := createdRequest(false)
		states := []v1alpha1.HintState{v1alpha1.HintConsumed, v1alpha1.HintConsumed, v1alpha1.HintReserved}
		for i := range v1alpha1.MaxStoredHints {
			state := v1alpha1.HintDeclined
			if i < len(states) {
				state = states[i]
			} else if i%2 == 0 {
				state = v1alpha1.HintPending
			}
			h := testHint(int64(i+1), state)
			h.Text = strings.Repeat("a", v1alpha1.MaxHintTextBytes)
			h.Author.Login = strings.Repeat("b", 100)
			rr.Status.Hints = append(rr.Status.Hints, h)
		}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	It("refuses an eleventh hint", func() {
		rr := createdRequest(false)
		for i := range v1alpha1.MaxStoredHints + 1 {
			rr.Status.Hints = append(rr.Status.Hints, testHint(int64(i+1), v1alpha1.HintPending))
		}
		expectStatusRefused(rr, "status.hints: Too many")
	})

	It("refuses a fourth Consumed or Reserved hint", func() {
		rr := createdRequest(false)
		for i := range v1alpha1.MaxHints {
			rr.Status.Hints = append(rr.Status.Hints, testHint(int64(i+1), v1alpha1.HintConsumed))
		}
		rr.Status.Hints = append(rr.Status.Hints, testHint(int64(v1alpha1.MaxHints+1), v1alpha1.HintReserved))
		expectStatusRefused(rr, "at most 3 hints are Consumed or Reserved")
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
		Entry("a reason the code never writes", func(h *v1alpha1.Hint) {
			h.State, h.Reason = v1alpha1.HintDeclined, "BecauseISaidSo"
		}, "status.hints[0].reason"),
	)

	It("accepts every reason in HintDeclineReasons", func() {
		for _, reason := range v1alpha1.HintDeclineReasons {
			rr := createdRequest(false)
			h := testHint(1, v1alpha1.HintDeclined)
			h.Reason = reason
			rr.Status.Hints = []v1alpha1.Hint{h}
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed(), reason)
		}
	})

	It("never unsets editedAfterAccept", func() {
		rr := createdRequest(false)
		h := testHint(1, v1alpha1.HintPending)
		h.EditedAfterAccept = true
		rr.Status.Hints = []v1alpha1.Hint{h}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		got := reread(rr)
		got.Status.Hints[0].EditedAfterAccept = false
		expectStatusRefused(got, "editedAfterAccept is never unset")
	})

	It("never changes or unsets a hint's outcome, and accepts only a replacement's outcome", func() {
		rr := createdRequest(false)
		h := testHint(1, v1alpha1.HintPending)
		h.Outcome = "NoProposal"
		rr.Status.Hints = []v1alpha1.Hint{h}
		expectStatusRefused(rr, "status.hints[0].outcome")

		rr = createdRequest(false)
		h.Outcome = "Proposed"
		rr.Status.Hints = []v1alpha1.Hint{h}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		changed := reread(rr)
		changed.Status.Hints[0].Outcome = "RunbookMatched"
		expectStatusRefused(changed, "the outcome of a hint never changes once set")
		unset := reread(rr)
		unset.Status.Hints[0].Outcome = ""
		expectStatusRefused(unset, "the outcome of a hint never changes once set")
	})

	It("records a reply only on an answered hint, once, and never changes it", func() {
		rr := createdRequest(false)
		h := testHint(1, v1alpha1.HintPending)
		h.Reply = v1alpha1.HintReplyPosted
		rr.Status.Hints = []v1alpha1.Hint{h}
		expectStatusRefused(rr, "only a Consumed or Declined hint has a reply")

		rr = createdRequest(false)
		h = testHint(1, v1alpha1.HintDeclined)
		h.Reply = "Shouted"
		rr.Status.Hints = []v1alpha1.Hint{h}
		expectStatusRefused(rr, "status.hints[0].reply")

		rr = createdRequest(false)
		for _, rec := range []v1alpha1.HintReply{v1alpha1.HintReplyPosted, v1alpha1.HintReplyNotRedactable,
			v1alpha1.HintReplyTooManyComments, v1alpha1.HintReplyRefused, v1alpha1.HintReplyGaveUp} {
			h = testHint(1, v1alpha1.HintDeclined)
			h.Reply = rec
			rr.Status.Hints = []v1alpha1.Hint{h}
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed(), string(rec))
			rr = createdRequest(false)
		}

		rr = createdRequest(false)
		rr.Status.Hints = []v1alpha1.Hint{testHint(1, v1alpha1.HintDeclined)}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		got := reread(rr)
		got.Status.Hints[0].Reply = v1alpha1.HintReplyGaveUp
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed(), "a reply is recorded once")
		changed := reread(rr)
		changed.Status.Hints[0].Reply = v1alpha1.HintReplyPosted
		expectStatusRefused(changed, "the reply of a hint never changes once set")
		unset := reread(rr)
		unset.Status.Hints[0].Reply = ""
		expectStatusRefused(unset, "the reply of a hint never changes once set")
	})

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

	It("keeps the reason of a terminal hint", func() {
		for _, state := range []v1alpha1.HintState{v1alpha1.HintDeclined, v1alpha1.HintConsumed} {
			rr := createdRequest(false)
			rr.Status.Hints = []v1alpha1.Hint{testHint(1, state)}
			rr.Status.Hints[0].Reason = "BudgetSpent"
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
			for name, reason := range map[string]string{"changed": "NoProposal", "removed": ""} {
				if reason == "" && state == v1alpha1.HintDeclined {
					continue // refused by the rule that a Declined hint needs a reason
				}
				got := reread(rr)
				got.Status.Hints[0].Reason = reason
				err := k8sClient.Status().Update(ctx, got)
				Expect(err).To(HaveOccurred(), "the API server accepted: %s %s", state, name)
				Expect(err.Error()).To(ContainSubstring("the reason of a Consumed or Declined hint never changes"), name)
			}
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

func testRefused(commentID int64) v1alpha1.RefusedHint {
	return v1alpha1.RefusedHint{
		Source: v1alpha1.HintSourceGitHub, CommentID: commentID, Author: v1alpha1.HintAuthor{ID: 42, Login: "octo-cat"},
		Reason: v1alpha1.HintRefusalCapReached, RefusedAt: metav1.NewTime(time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)),
	}
}

var _ = Describe("RemediationRequest status.refusedHints", func() {
	It("accepts MaxRefusedHints refusals, each reason, and drops the oldest", func() {
		rr := createdRequest(false)
		reasons := []v1alpha1.HintRefusal{v1alpha1.HintRefusalCapReached,
			v1alpha1.HintRefusalTooLong, v1alpha1.HintRefusalNotRedactable, v1alpha1.HintRefusalEmpty,
			v1alpha1.HintRefusalEditedAfterSeen}
		for i := range v1alpha1.MaxRefusedHints {
			r := testRefused(int64(i + 1))
			r.Reason = reasons[i%len(reasons)]
			rr.Status.RefusedHints = append(rr.Status.RefusedHints, r)
		}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		got := reread(rr)
		got.Status.RefusedHints = append(got.Status.RefusedHints[1:], testRefused(99))
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed(), "a refusal is not pinned: the oldest goes")
	})

	It("refuses more than MaxRefusedHints", func() {
		rr := createdRequest(false)
		for i := range v1alpha1.MaxRefusedHints + 1 {
			rr.Status.RefusedHints = append(rr.Status.RefusedHints, testRefused(int64(i+1)))
		}
		expectStatusRefused(rr, "status.refusedHints: Too many")
	})

	It("records the reply to a refusal once, and never changes it", func() {
		rr := createdRequest(false)
		rr.Status.RefusedHints = []v1alpha1.RefusedHint{testRefused(1)}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		got := reread(rr)
		got.Status.RefusedHints[0].Reply = v1alpha1.HintReplyNotRedactable
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		changed := reread(rr)
		changed.Status.RefusedHints[0].Reply = v1alpha1.HintReplyPosted
		expectStatusRefused(changed, "the reply to a refused hint never changes once set")
		unset := reread(rr)
		unset.Status.RefusedHints[0].Reply = ""
		expectStatusRefused(unset, "the reply to a refused hint never changes once set")
	})

	It("refuses two refusals of the same comment", func() {
		rr := createdRequest(false)
		rr.Status.RefusedHints = []v1alpha1.RefusedHint{testRefused(1), testRefused(1)}
		expectStatusRefused(rr, "Duplicate value")
	})

	DescribeTable("refuses a refusal out of its rules",
		func(change func(*v1alpha1.RefusedHint), msg string) {
			rr := createdRequest(false)
			r := testRefused(1)
			change(&r)
			rr.Status.RefusedHints = []v1alpha1.RefusedHint{r}
			expectStatusRefused(rr, msg)
		},
		Entry("an unknown reason", func(r *v1alpha1.RefusedHint) { r.Reason = "Rude" }, "status.refusedHints[0].reason"),
		Entry("a non-writer's refusal, never recorded", func(r *v1alpha1.RefusedHint) { r.Reason = "NoWriteAccess" },
			"status.refusedHints[0].reason"),
		Entry("an unknown source", func(r *v1alpha1.RefusedHint) { r.Source = "Email" }, "status.refusedHints[0].source"),
		Entry("comment ID 0", func(r *v1alpha1.RefusedHint) { r.CommentID = 0 }, "status.refusedHints[0].commentID"),
		Entry("author ID 0", func(r *v1alpha1.RefusedHint) { r.Author.ID = 0 }, "status.refusedHints[0].author.id"),
		Entry("a login with a space", func(r *v1alpha1.RefusedHint) { r.Author.Login = "octo cat" }, "status.refusedHints[0].author.login"),
	)
})

var _ = Describe("RemediationRequest status.commentCursor", func() {
	at := metav1.NewTime(time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC))
	It("accepts a cursor at its bounds", func() {
		for _, c := range []v1alpha1.CommentCursor{
			{},
			{Since: &at, ETag: `W/"` + strings.Repeat("a", 250) + `"`},
			{Since: &at, Page: 100000, AfterID: 1 << 40, SweepStart: &at},
			{ETag: `""`},
		} {
			rr := createdRequest(false)
			rr.Status.CommentCursor = &c
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed(), "%+v", c)
		}
	})

	DescribeTable("refuses a cursor out of its rules",
		func(c v1alpha1.CommentCursor, msg string) {
			rr := createdRequest(false)
			rr.Status.CommentCursor = &c
			expectStatusRefused(rr, msg)
		},
		Entry("a negative page", v1alpha1.CommentCursor{Page: -1}, "status.commentCursor.page"),
		Entry("a page past the bound", v1alpha1.CommentCursor{Page: 100001}, "status.commentCursor.page"),
		Entry("a negative afterID", v1alpha1.CommentCursor{AfterID: -1}, "status.commentCursor.afterID"),
		Entry("an unquoted etag", v1alpha1.CommentCursor{ETag: "abc"}, "status.commentCursor.etag"),
		Entry("an etag with a line break", v1alpha1.CommentCursor{ETag: "\"a\"\r\nX-Evil: 1"}, "status.commentCursor.etag"),
		Entry("an etag with a blank", v1alpha1.CommentCursor{ETag: `"a b"`}, "status.commentCursor.etag"),
		Entry("an etag over 256 bytes", v1alpha1.CommentCursor{ETag: `"` + strings.Repeat("a", 255) + `"`}, "status.commentCursor.etag"),
		Entry("an etag on a later page", v1alpha1.CommentCursor{ETag: `"t"`, Page: 2},
			"etag is kept only on the first page of a new sweep"),
		Entry("an etag after an ID", v1alpha1.CommentCursor{ETag: `"t"`, AfterID: 7},
			"etag is kept only on the first page of a new sweep"),
		Entry("too many seen comments", v1alpha1.CommentCursor{Seen: seenComments(v1alpha1.MaxSeenComments+1, at)},
			"status.commentCursor.seen: Too many"),
		Entry("a seen comment ID 0", v1alpha1.CommentCursor{Seen: []v1alpha1.SeenComment{{CommentID: 0, UpdatedAt: at}}},
			"status.commentCursor.seen[0].commentID"),
		Entry("evictedAt without evictedID", v1alpha1.CommentCursor{EvictedAt: &at},
			"evictedAt and evictedID are set together"),
		Entry("evictedID without evictedAt", v1alpha1.CommentCursor{EvictedID: 7},
			"evictedAt and evictedID are set together"),
		Entry("a negative evictedID", v1alpha1.CommentCursor{EvictedAt: &at, EvictedID: -1},
			"status.commentCursor.evictedID"),
	)

	It("accepts the watermark of evicted comments", func() {
		rr := createdRequest(false)
		rr.Status.CommentCursor = &v1alpha1.CommentCursor{Since: &at, EvictedAt: &at, EvictedID: 7}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	It("accepts MaxSeenComments seen comments", func() {
		rr := createdRequest(false)
		rr.Status.CommentCursor = &v1alpha1.CommentCursor{Since: &at, Seen: seenComments(v1alpha1.MaxSeenComments, at)}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	It("keeps the cursor once set, and never moves since back", func() {
		rr := createdRequest(false)
		later := metav1.NewTime(at.Add(time.Hour))
		rr.Status.CommentCursor = &v1alpha1.CommentCursor{Since: &later}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())

		gone := reread(rr)
		gone.Status.CommentCursor = nil
		expectStatusRefused(gone, "status.commentCursor is kept once set")

		back := reread(rr)
		back.Status.CommentCursor.Since = &at
		expectStatusRefused(back, "status.commentCursor.since never decreases")

		dropped := reread(rr)
		dropped.Status.CommentCursor.Since = nil
		expectStatusRefused(dropped, "status.commentCursor.since never decreases")

		forward := reread(rr)
		ahead := metav1.NewTime(at.Add(2 * time.Hour))
		forward.Status.CommentCursor.Since = &ahead
		Expect(k8sClient.Status().Update(ctx, forward)).To(Succeed())
	})
})

// seenComments is n seen comments with IDs from 1.
func seenComments(n int, at metav1.Time) []v1alpha1.SeenComment {
	out := make([]v1alpha1.SeenComment, n)
	for i := range out {
		out[i] = v1alpha1.SeenComment{CommentID: int64(i + 1), UpdatedAt: at}
	}
	return out
}

var _ = Describe("RemediationRequest status.handoff", func() {
	const nextUID = "55555555-5555-4555-8555-555555555555"
	const laterUID = "66666666-6666-4666-8666-666666666666"
	incoming := func(phase v1alpha1.HandoffPhase) *v1alpha1.IncomingHandoff {
		return &v1alpha1.IncomingHandoff{Name: testPredecessor, UID: testRootUID, Hint: testHintRef(), Phase: phase}
	}
	outgoing := func(phase v1alpha1.HandoffPhase, commentID int64) *v1alpha1.OutgoingHandoff {
		ref := testHintRef()
		ref.CommentID = commentID
		return &v1alpha1.OutgoingHandoff{
			Name: "remediation-next", UID: nextUID, Hint: ref, ExpectedHead: testHeadSHA, Phase: phase,
		}
	}
	// predecessor creates a request that holds hint 1 in state and returns it.
	predecessor := func(state v1alpha1.HintState) *v1alpha1.RemediationRequest {
		rr := createdRequest(false)
		rr.Status.Hints = []v1alpha1.Hint{testHint(1, state)}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, rr)).To(Succeed())
		return rr
	}

	It("accepts the replacement's side from Reserved to Done", func() {
		rr := createdRequest(true)
		rr.Status.Handoff = &v1alpha1.Handoff{From: incoming(v1alpha1.HandoffReserved)}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		rr.Status.Handoff.From.Phase = v1alpha1.HandoffDone
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	It("accepts the predecessor's side from Intent to Done, with its hint Reserved then Consumed", func() {
		rr := predecessor(v1alpha1.HintPending)
		rr.Status.Hints[0].State = v1alpha1.HintReserved
		rr.Status.Handoff = &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffIntent, 1)}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		rr.Status.Hints[0].State = v1alpha1.HintConsumed
		rr.Status.Handoff.To.Phase = v1alpha1.HandoffDone
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	It("lets a chain run from A to B to C", func() {
		// B replaced A (from, Done), then a hint on B is answered by C (to, Intent, then Done).
		b := createdRequest(true)
		b.Status.Handoff = &v1alpha1.Handoff{From: incoming(v1alpha1.HandoffReserved)}
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		b.Status.Handoff.From.Phase = v1alpha1.HandoffDone
		b.Status.Hints = []v1alpha1.Hint{testHint(1, v1alpha1.HintConsumed), testHint(2, v1alpha1.HintPending)}
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		b.Status.Hints[1].State = v1alpha1.HintReserved
		b.Status.Handoff.To = outgoing(v1alpha1.HandoffIntent, 2)
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed(), "the middle request records its outgoing side")
		b.Status.Hints[1].State = v1alpha1.HintConsumed
		b.Status.Handoff.To.Phase = v1alpha1.HandoffDone
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		Expect(reread(b).Status.Handoff.From.Phase).To(Equal(v1alpha1.HandoffDone))
	})

	DescribeTable("refuses a record out of its rules",
		func(replacement bool, hints []v1alpha1.Hint, handoff func() *v1alpha1.Handoff, msg string) {
			rr := createdRequest(replacement)
			rr.Status.Hints = hints
			rr.Status.Handoff = handoff()
			expectStatusRefused(rr, msg)
		},
		Entry("neither from nor to", false, nil, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{}
		}, "a handoff has from, to or both"),
		Entry("from created in Done", true, nil, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{From: incoming(v1alpha1.HandoffDone)}
		}, "status.handoff.from starts in Reserved"),
		Entry("from in Intent", true, nil, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{From: incoming(v1alpha1.HandoffIntent)}
		}, "an incoming handoff is Reserved, Done or Aborted"),
		Entry("to created in Done", false, []v1alpha1.Hint{testHint(1, v1alpha1.HintConsumed)}, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffDone, 1)}
		}, "status.handoff.to starts in Intent"),
		Entry("to in Reserved", false, []v1alpha1.Hint{testHint(1, v1alpha1.HintReserved)}, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffReserved, 1)}
		}, "an outgoing handoff is Intent, Done or Aborted"),
		Entry("to without expectedHead", false, []v1alpha1.Hint{testHint(1, v1alpha1.HintReserved)}, func() *v1alpha1.Handoff {
			h := outgoing(v1alpha1.HandoffIntent, 1)
			h.ExpectedHead = ""
			return &v1alpha1.Handoff{To: h}
		}, "status.handoff.to.expectedHead"),
		Entry("to without the successor's UID", false, []v1alpha1.Hint{testHint(1, v1alpha1.HintReserved)}, func() *v1alpha1.Handoff {
			h := outgoing(v1alpha1.HandoffIntent, 1)
			h.UID = ""
			return &v1alpha1.Handoff{To: h}
		}, "status.handoff.to.uid"),
		Entry("an expectedHead that is not a full SHA", false, []v1alpha1.Hint{testHint(1, v1alpha1.HintReserved)}, func() *v1alpha1.Handoff {
			h := outgoing(v1alpha1.HandoffIntent, 1)
			h.ExpectedHead = "0123abc"
			return &v1alpha1.Handoff{To: h}
		}, "status.handoff.to.expectedHead"),
		Entry("to naming a hint the request does not hold", false, nil, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffIntent, 1)}
		}, "status.handoff.to must name a hint in status.hints"),
		Entry("to naming another comment", false, []v1alpha1.Hint{testHint(1, v1alpha1.HintReserved)}, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffIntent, 2)}
		}, "status.handoff.to must name a hint in status.hints"),
		Entry("Intent while the hint is Pending", false, []v1alpha1.Hint{testHint(1, v1alpha1.HintPending)}, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffIntent, 1)}
		}, "status.handoff.to must name a hint in status.hints"),
		Entry("Intent while the hint is Consumed", false, []v1alpha1.Hint{testHint(1, v1alpha1.HintConsumed)}, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffIntent, 1)}
		}, "status.handoff.to must name a hint in status.hints"),
		Entry("from on a request that replaces nothing", false, nil, func() *v1alpha1.Handoff {
			return &v1alpha1.Handoff{From: incoming(v1alpha1.HandoffReserved)}
		}, "status.handoff.from must match spec.replaces"),
		Entry("from naming another request", true, nil, func() *v1alpha1.Handoff {
			h := incoming(v1alpha1.HandoffReserved)
			h.Name = otherRequest
			return &v1alpha1.Handoff{From: h}
		}, "status.handoff.from must match spec.replaces"),
		Entry("from with another UID", true, nil, func() *v1alpha1.Handoff {
			h := incoming(v1alpha1.HandoffReserved)
			h.UID = nextUID
			return &v1alpha1.Handoff{From: h}
		}, "status.handoff.from must match spec.replaces"),
		Entry("from with another hint", true, nil, func() *v1alpha1.Handoff {
			h := incoming(v1alpha1.HandoffReserved)
			h.Hint.CommentID = 2
			return &v1alpha1.Handoff{From: h}
		}, "status.handoff.from must match spec.replaces"),
	)

	It("moves each side only once", func() {
		b := createdRequest(true)
		b.Status.Handoff = &v1alpha1.Handoff{From: incoming(v1alpha1.HandoffReserved)}
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		b.Status.Handoff.From.Phase = v1alpha1.HandoffAborted
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		for _, phase := range []v1alpha1.HandoffPhase{v1alpha1.HandoffReserved, v1alpha1.HandoffDone} {
			got := reread(b)
			got.Status.Handoff.From.Phase = phase
			expectStatusRefused(got, "an incoming handoff moves only from Reserved to Done or Aborted")
		}

		a := predecessor(v1alpha1.HintReserved)
		a.Status.Handoff = &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffIntent, 1)}
		Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
		a.Status.Hints[0].State = v1alpha1.HintDeclined
		a.Status.Hints[0].Reason = "RequestEnded" // the reason of a hint whose handoff aborted is #539's to choose
		a.Status.Handoff.To.Phase = v1alpha1.HandoffAborted
		Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
		for _, phase := range []v1alpha1.HandoffPhase{v1alpha1.HandoffIntent, v1alpha1.HandoffDone} {
			got := reread(a)
			got.Status.Handoff.To.Phase = phase
			expectStatusRefused(got, "an outgoing handoff moves only from Intent to Done or Aborted")
		}
	})

	It("accepts an Aborted intent replaced by the Intent of another replacement for another hint", func() {
		a := createdRequest(false)
		a.Status.Hints = []v1alpha1.Hint{testHint(1, v1alpha1.HintReserved), testHint(2, v1alpha1.HintPending)}
		a.Status.Handoff = &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffIntent, 1)}
		Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
		a.Status.Hints[0].State, a.Status.Hints[0].Reason = v1alpha1.HintDeclined, v1alpha1.ReasonHandoffAborted
		a.Status.Handoff.To.Phase = v1alpha1.HandoffAborted
		Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
		a.Status.Hints[1].State = v1alpha1.HintReserved
		a.Status.Handoff.To = outgoing(v1alpha1.HandoffIntent, 2)
		a.Status.Handoff.To.UID = laterUID
		Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
	})

	// Each phase of the outgoing side is tied to the state of its hint (#592): Intent to Reserved, Done to
	// Consumed, Aborted to Declined. Every pair below is reachable from Intent with the hint Reserved by moves
	// that each rule of the hint and of the side allows on its own.
	DescribeTable("refuses an outgoing side whose phase contradicts its hint",
		func(phase v1alpha1.HandoffPhase, state v1alpha1.HintState) {
			a := predecessor(v1alpha1.HintReserved)
			a.Status.Handoff = &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffIntent, 1)}
			Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
			a.Status.Hints[0].State = state
			if state == v1alpha1.HintDeclined {
				a.Status.Hints[0].Reason = v1alpha1.ReasonHandoffAborted
			}
			a.Status.Handoff.To.Phase = phase
			expectStatusRefused(a, "status.handoff.to must name a hint in status.hints")
		},
		Entry("Done with the hint Declined", v1alpha1.HandoffDone, v1alpha1.HintDeclined),
		Entry("Done with the hint still Reserved", v1alpha1.HandoffDone, v1alpha1.HintReserved),
		Entry("Aborted with the hint still Reserved", v1alpha1.HandoffAborted, v1alpha1.HintReserved),
		Entry("Aborted with the hint Consumed", v1alpha1.HandoffAborted, v1alpha1.HintConsumed),
	)

	It("refuses an Aborted intent replaced by the Intent of another replacement for the same hint", func() {
		a := predecessor(v1alpha1.HintReserved)
		a.Status.Handoff = &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffIntent, 1)}
		Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
		a.Status.Hints[0].State, a.Status.Hints[0].Reason = v1alpha1.HintDeclined, v1alpha1.ReasonHandoffAborted
		a.Status.Handoff.To.Phase = v1alpha1.HandoffAborted
		Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
		got := reread(a)
		got.Status.Handoff.To.Phase = v1alpha1.HandoffIntent
		got.Status.Handoff.To.UID = laterUID
		expectStatusRefused(got, "or from Aborted to the Intent of another replacement for another hint")
	})

	It("keeps each side once written", func() {
		const toKept, toPinned = "status.handoff.to is never removed", "an outgoing handoff is pinned"
		a := predecessor(v1alpha1.HintReserved)
		a.Status.Handoff = &v1alpha1.Handoff{To: outgoing(v1alpha1.HandoffIntent, 1)}
		Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
		for name, c := range map[string]struct {
			change func(*v1alpha1.RemediationRequest)
			msg    string
		}{
			"handoff removed": {func(r *v1alpha1.RemediationRequest) { r.Status.Handoff = nil }, toKept},
			"to removed": {func(r *v1alpha1.RemediationRequest) {
				r.Status.Handoff.To = nil
				r.Status.Handoff.From = &v1alpha1.IncomingHandoff{Name: testPredecessor, UID: testRootUID, Hint: testHintRef(), Phase: v1alpha1.HandoffReserved}
			}, toKept},
			statusEmptied:              {func(r *v1alpha1.RemediationRequest) { r.Status = v1alpha1.RemediationRequestStatus{} }, toKept},
			"to's name changed":        {func(r *v1alpha1.RemediationRequest) { r.Status.Handoff.To.Name = otherRequest }, toPinned},
			"to's UID changed":         {func(r *v1alpha1.RemediationRequest) { r.Status.Handoff.To.UID = testRootUID }, toPinned},
			"expectedHead changed":     {func(r *v1alpha1.RemediationRequest) { r.Status.Handoff.To.ExpectedHead = strings.Repeat("c", 40) }, toPinned},
			"to's hint digest changed": {func(r *v1alpha1.RemediationRequest) { r.Status.Handoff.To.Hint.Digest = testDigest("b") }, toPinned},
		} {
			got := reread(a)
			c.change(got)
			err := k8sClient.Status().Update(ctx, got)
			Expect(err).To(HaveOccurred(), "the API server accepted: "+name)
			Expect(err.Error()).To(ContainSubstring(c.msg), name)
		}

		b := createdRequest(true)
		b.Status.Handoff = &v1alpha1.Handoff{From: incoming(v1alpha1.HandoffReserved)}
		Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		for name, c := range map[string]struct {
			change func(*v1alpha1.RemediationRequest)
			msg    string
		}{
			"handoff removed":     {func(r *v1alpha1.RemediationRequest) { r.Status.Handoff = nil }, "status.handoff.from is never removed"},
			statusEmptied:         {func(r *v1alpha1.RemediationRequest) { r.Status = v1alpha1.RemediationRequestStatus{} }, "status.handoff.from is never removed"},
			"from's UID changed":  {func(r *v1alpha1.RemediationRequest) { r.Status.Handoff.From.UID = nextUID }, "an incoming handoff is pinned"},
			"from's hint changed": {func(r *v1alpha1.RemediationRequest) { r.Status.Handoff.From.Hint.Digest = testDigest("b") }, "an incoming handoff is pinned"},
		} {
			got := reread(b)
			c.change(got)
			err := k8sClient.Status().Update(ctx, got)
			Expect(err).To(HaveOccurred(), "the API server accepted: "+name)
			Expect(err.Error()).To(ContainSubstring(c.msg), name)
		}
	})
})

var _ = Describe("RemediationRequest status.pullRequest.reviewCheck", func() {
	const newHead = "89abcdef0123456789abcdef0123456789abcdef"
	// waiting creates a request whose pull request has head with the check in state on it.
	waiting := func(head string, state v1alpha1.ReviewCheckState) *v1alpha1.RemediationRequest {
		rr := createdRequest(false)
		rr.Status.PullRequest = &v1alpha1.PullRequestStatus{Number: 1, URL: testPRURL, HeadSHA: head,
			ReviewCheck: &v1alpha1.ReviewCheck{Head: head, State: state}}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, rr)).To(Succeed())
		return rr
	}

	It("moves from InProgress to Passed, Failed or Refused on one head, and starts again on a new head", func() {
		for _, end := range []v1alpha1.ReviewCheckState{v1alpha1.ReviewCheckPassed, v1alpha1.ReviewCheckFailed,
			v1alpha1.ReviewCheckRefused} {
			rr := waiting(testHeadSHA, v1alpha1.ReviewCheckInProgress)
			rr.Status.PullRequest.ReviewCheck.State = end
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
			rr.Status.PullRequest = &v1alpha1.PullRequestStatus{Number: 1, URL: testPRURL, HeadSHA: newHead,
				ReviewCheck: &v1alpha1.ReviewCheck{Head: newHead, State: v1alpha1.ReviewCheckInProgress}}
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed(), "a new head starts in progress")
		}
	})

	It("refuses a state that names another head", func() {
		rr := createdRequest(false)
		rr.Status.PullRequest = &v1alpha1.PullRequestStatus{Number: 1, URL: testPRURL, HeadSHA: newHead,
			ReviewCheck: &v1alpha1.ReviewCheck{Head: testHeadSHA, State: v1alpha1.ReviewCheckPassed}}
		expectStatusRefused(rr, "reviewCheck names the recorded head")
	})

	It("refuses a head that is not a full SHA", func() {
		rr := createdRequest(false)
		rr.Status.PullRequest = &v1alpha1.PullRequestStatus{Number: 1, URL: testPRURL, HeadSHA: testHeadSHA,
			ReviewCheck: &v1alpha1.ReviewCheck{Head: "0123abc", State: v1alpha1.ReviewCheckInProgress}}
		expectStatusRefused(rr, "status.pullRequest.reviewCheck.head")
	})

	It("never moves back on one head", func() {
		for _, from := range []v1alpha1.ReviewCheckState{v1alpha1.ReviewCheckPassed, v1alpha1.ReviewCheckFailed,
			v1alpha1.ReviewCheckRefused} {
			rr := waiting(testHeadSHA, from)
			for _, to := range []v1alpha1.ReviewCheckState{v1alpha1.ReviewCheckInProgress, v1alpha1.ReviewCheckPassed,
				v1alpha1.ReviewCheckFailed, v1alpha1.ReviewCheckRefused} {
				if to == from {
					continue
				}
				got := reread(rr)
				got.Status.PullRequest.ReviewCheck.State = to
				expectStatusRefused(got, "on one head the check run moves only from InProgress to Passed, Failed or Refused")
			}
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
