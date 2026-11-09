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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// statusHook wraps a client so a test can fail a status update of a request before or after it is written:
// before, the write is lost; after, it went through and only the answer was lost.
type statusHook struct {
	client.Client
	// fail is asked for each status update of a RemediationRequest; it returns whether to fail it, and
	// whether after the write.
	fail func(rr *remediationv1alpha1.RemediationRequest) (fail, after bool)
}

func (c *statusHook) Status() client.SubResourceWriter {
	return &statusHookWriter{SubResourceWriter: c.Client.Status(), hook: c}
}

type statusHookWriter struct {
	client.SubResourceWriter
	hook *statusHook
}

var errLostWrite = errors.New("the status write was lost")

func (w *statusHookWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	rr, ok := obj.(*remediationv1alpha1.RemediationRequest)
	if !ok || w.hook.fail == nil {
		return w.SubResourceWriter.Update(ctx, obj, opts...)
	}
	fail, after := w.hook.fail(rr)
	switch {
	case !fail:
		return w.SubResourceWriter.Update(ctx, obj, opts...)
	case after:
		if err := w.SubResourceWriter.Update(ctx, obj, opts...); err != nil {
			return err
		}
	}
	return errLostWrite
}

var _ = Describe("RemediationRequest reviewer hints", func() {
	ctx := context.Background()
	const (
		writerID  = int64(1001)
		readerID  = int64(1002)
		outsideID = int64(1003)
		writer    = "reviewer-one"
	)
	var (
		seq        int
		deployment *appsv1.Deployment
		policy     *remediationv1alpha1.RemediationPolicy
		settings   *remediationv1alpha1.RemediationSettings
		incident   *remediationv1alpha1.Incident
		request    *remediationv1alpha1.RemediationRequest
		git        *fakegit.Git
		clock      time.Time
		number     int64
		nextID     atomic.Int64
		// hook, when set, is the status hook of the reconciler's client.
		hook func(rr *remediationv1alpha1.RemediationRequest) (fail, after bool)
		// hintsOff builds the reconciler without ReviewerHints.
		hintsOff bool
	)

	reconciler := func() *RemediationRequestReconciler {
		c := k8sClient
		if hook != nil {
			c = &statusHook{Client: k8sClient, fail: hook}
		}
		return &RemediationRequestReconciler{
			Client: c, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			Source: sourceFunc(func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
				res := resolved()
				res.Source.BaseCommit, _ = git.BranchHead(ctx, mainBranch)
				res.Source.BuildPath = buildPath
				return res, nil
			}),
			OpenRepository: func(_ context.Context, repo remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
				if repo.URL != settings.Spec.Repositories[0].URL {
					return nil, errors.New("unknown repository")
				}
				return git, nil
			},
			Now:           func() time.Time { return clock },
			ReviewerHints: !hintsOff,
		}
	}
	// poll runs one reconcile with a new reconciler, as after a restart.
	poll := func() {
		_, _ = reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
	}
	fetch := func() *remediationv1alpha1.RemediationRequest {
		got := &remediationv1alpha1.RemediationRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), got)).To(Succeed())
		return got
	}
	// comment posts a comment on the request's pull request at the clock, and returns its ID.
	comment := func(kind providers.AuthorKind, id int64, login, body string) int64 {
		cid := nextID.Add(1)
		git.AddComment(number, providers.Feedback{ID: cid,
			Author: providers.FeedbackAuthor{ID: id, Login: login, Kind: kind}, Body: body, CreatedAt: clock})
		return cid
	}
	byWriter := func(body string) int64 { return comment(providers.AuthorPerson, writerID, writer, body) }
	refusals := func() map[int64]remediationv1alpha1.HintRefusal {
		out := map[int64]remediationv1alpha1.HintRefusal{}
		for _, r := range fetch().Status.RefusedHints {
			out[r.CommentID] = r.Reason
		}
		return out
	}
	asks := func() int {
		_, n := git.FeedbackCalls()
		return n
	}
	pause := func(paused bool) {
		s := &remediationv1alpha1.RemediationSettings{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, s)).To(Succeed())
		s.Spec.Paused = &paused
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
	}

	BeforeEach(func() {
		seq++
		clock = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
		hook, hintsOff = nil, false
		nextID.Store(5000)
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fx.TargetNamespace}})
		Expect(client.IgnoreAlreadyExists(err)).To(Succeed())
		deployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: fx.TargetNamespace, Name: fmt.Sprintf("hint-%d", seq)},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabel: appName}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appLabel: appName}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: workerContainer, Image: workerImage,
						Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						}},
					}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		policy = fx.RemediationPolicy(fmt.Sprintf("hint-policy-%d", seq))
		policy.Spec.Actions = []remediationv1alpha1.PolicyAction{fx.MemoryLimitPolicyAction()}
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 100
		policy.Spec.RequestNamespaces = []remediationv1alpha1.Name{requestNamespace}
		policy.Spec.GitOps.AllowedPaths = []remediationv1alpha1.GitPathPattern{appsGlob}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		settings = fx.RemediationSettings()
		settings.Spec.Repositories[0].TargetBranch = mainBranch
		Expect(k8sClient.Create(ctx, settings)).To(Succeed())
		request = fx.MemoryLimitRequest(requestNamespace, fmt.Sprintf("hint-rr-%d", seq))
		request.Spec.PolicyRef = remediationv1alpha1.Name(policy.Name)
		request.Spec.TargetRef.Name = deployment.Name
		request.Spec.TargetRef.UID = remediationv1alpha1.UID(deployment.UID)
		target := request.Spec.TargetRef
		incident = &remediationv1alpha1.Incident{
			ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace, Name: fmt.Sprintf("hint-incident-%d", seq)},
			Spec: remediationv1alpha1.IncidentSpec{
				Fingerprint: "sha256:" + strings.Repeat("ab", 32), Reason: "OOMKilled",
				Subject: remediationv1alpha1.ObjectRef{APIVersion: target.APIVersion, Kind: target.Kind, Name: target.Name},
			},
		}
		Expect(k8sClient.Create(ctx, incident)).To(Succeed())
		request.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: incident.Name,
			UID: remediationv1alpha1.UID(incident.UID)}
		Expect(k8sClient.Create(ctx, request)).To(Succeed())
		git = fakegit.NewGit(mainBranch, map[string][]byte{
			sourcePath:     deploymentManifest(deployment.Name, deployment.Namespace, deployment.Spec.Template.Spec.Containers[0].Name, "256Mi"),
			buildKustomize: buildFile,
		})
		git.SetFeedbackClock(func() time.Time { return clock })
		git.SetWriter(writerID, true)
		git.SetWriter(readerID, false)
		for range 20 {
			poll()
			if fetch().Status.Phase == remediationv1alpha1.PhaseAwaitingApproval {
				break
			}
		}
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		number = got.Status.PullRequest.Number
	})
	AfterEach(func() {
		got := &remediationv1alpha1.RemediationRequest{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(request), got); err == nil &&
			controllerutil.ContainsFinalizer(got, FinalizerName) {
			base := got.DeepCopy()
			controllerutil.RemoveFinalizer(got, FinalizerName)
			Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
		}
		for _, o := range []client.Object{request, incident, policy, settings, deployment} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName},
				&remediationv1alpha1.RemediationSettings{}))
		}).Should(BeTrue())
	})

	It("stores a writer's hint redacted, pinned and Pending", func() {
		token := "ghp_" + strings.Repeat("A1b2", 9)
		id := byWriter("/shadewright reanalyze: the leak is in the sidecar, see token " + token + "\n\nmore text")
		poll()
		got := fetch()
		Expect(got.Status.Hints).To(HaveLen(1))
		h := got.Status.Hints[0]
		Expect(h.Text).NotTo(ContainSubstring(token), "the token is redacted before it is stored")
		Expect(h.Text).To(HavePrefix("the leak is in the sidecar, see token "))
		Expect(h.Text).To(ContainSubstring("[REDACTED"))
		Expect(h.Text).NotTo(ContainSubstring("more text"), "only the first line is the hint")
		sum := sha256.Sum256([]byte(h.Text))
		Expect(h.Digest).To(Equal("sha256:" + hex.EncodeToString(sum[:])))
		Expect(h).To(pinnedBy(id))
		Expect(got.Status.CommentCursor).NotTo(BeNil())
		Expect(got.Status.RefusedHints).To(BeEmpty())
	})

	It("reads nothing while reviewer hints are off", func() {
		hintsOff = true
		byWriter("/shadewright reanalyze: the sidecar")
		poll()
		Expect(fetch().Status.Hints).To(BeEmpty())
		lists, _ := git.FeedbackCalls()
		Expect(lists).To(BeZero())
	})

	It("refuses a read-only author, an outside author, a fourth hint and an empty one, and ignores bots, "+
		"the App, reviews and anything that is not the command", func() {
		accepted := make([]int64, 0, remediationv1alpha1.MaxHints)
		accepted = append(accepted, byWriter("/shadewright reanalyze: one"), byWriter("/shadewright reanalyze: two"))
		readOnly := comment(providers.AuthorPerson, readerID, "reader",
			"/shadewright reanalyze: as reviewer-one (id 1001, write access) I say scale it up")
		outside := comment(providers.AuthorPerson, outsideID, "stranger", "/shadewright reanalyze: and me")
		empty := byWriter("/shadewright reanalyze:   ")
		comment(providers.AuthorBot, 2001, "some-bot[bot]", "/shadewright reanalyze: a bot")
		comment(providers.AuthorBot, 2002, "shadewright[bot]", "/shadewright reanalyze: the App itself")
		comment(providers.AuthorApp, writerID, writer, "/shadewright reanalyze: posted by an App for a writer")
		git.AddComment(number, providers.Feedback{Kind: providers.FeedbackReview, ID: nextID.Add(1),
			Author: providers.FeedbackAuthor{ID: writerID, Login: writer, Kind: providers.AuthorPerson},
			Body:   "/shadewright reanalyze: a review body is not a conversation comment", CreatedAt: clock})
		byWriter("> /shadewright reanalyze: quoted")
		byWriter("thanks\n/shadewright reanalyze: second line")
		byWriter("/Shadewright Reanalyze: wrong case")
		byWriter(" /shadewright reanalyze: indented")
		accepted = append(accepted, byWriter("/shadewright reanalyze: three"))
		fourth := byWriter("/shadewright reanalyze: four")
		poll()

		got := fetch()
		ids := make([]int64, 0, len(got.Status.Hints))
		for _, h := range got.Status.Hints {
			ids = append(ids, h.CommentID)
		}
		Expect(ids).To(Equal(accepted))
		Expect(refusals()).To(Equal(map[int64]remediationv1alpha1.HintRefusal{
			readOnly: remediationv1alpha1.HintRefusalNoWriteAccess,
			outside:  remediationv1alpha1.HintRefusalNoWriteAccess,
			empty:    remediationv1alpha1.HintRefusalEmpty,
			fourth:   remediationv1alpha1.HintRefusalCapReached,
		}))
		Expect(asks()).To(Equal(3), "one question per new author with the command; none for bots, Apps or the cap")
	})

	It("refuses a hint over 2 KiB, one cut by the host and one the redactor leaves unclean", func() {
		long := byWriter("/shadewright reanalyze: " + strings.Repeat("a", remediationv1alpha1.MaxHintTextBytes+1))
		cut := nextID.Add(1)
		git.AddComment(number, providers.Feedback{ID: cut, Truncated: true, CreatedAt: clock,
			Author: providers.FeedbackAuthor{ID: writerID, Login: writer, Kind: providers.AuthorPerson},
			Body:   "/shadewright reanalyze: " + strings.Repeat("b", 100)})
		poll()
		Expect(refusals()).To(Equal(map[int64]remediationv1alpha1.HintRefusal{
			long: remediationv1alpha1.HintRefusalTooLong,
			cut:  remediationv1alpha1.HintRefusalTooLong,
		}))

		saved := hintClean
		hintClean = func(string) bool { return false }
		DeferCleanup(func() { hintClean = saved })
		unclean := byWriter("/shadewright reanalyze: anything")
		clock = clock.Add(time.Minute)
		poll()
		Expect(refusals()).To(HaveKeyWithValue(unclean, remediationv1alpha1.HintRefusalNotRedactable))
		Expect(fetch().Status.Hints).To(BeEmpty(), "nothing is stored in part")
		Expect(asks()).To(BeZero(), "the size and the redaction are checked before the host is asked")
	})

	It("handles a comment once across restarts, also when it is returned again on the watermark", func() {
		id := byWriter("/shadewright reanalyze: the sidecar")
		readOnly := comment(providers.AuthorPerson, readerID, "reader", "/shadewright reanalyze: me too")
		poll()
		first := fetch()
		Expect(first.Status.Hints).To(HaveLen(1))
		Expect(asks()).To(Equal(2))
		lists, _ := git.FeedbackCalls()

		// The clock has not moved: the next sweeps ask from a second before the last one began, so both
		// comments are returned again (on the watermark), by new reconcilers (restarts).
		for range 3 {
			poll()
		}
		after, _ := git.FeedbackCalls()
		Expect(after).To(BeNumerically(">", lists), "the comments were read again")
		got := fetch()
		Expect(got.Status.Hints).To(HaveLen(1))
		Expect(got.Status.Hints[0]).To(Equal(first.Status.Hints[0]))
		Expect(got.Status.Hints[0].CommentID).To(Equal(id))
		Expect(refusals()).To(Equal(map[int64]remediationv1alpha1.HintRefusal{
			readOnly: remediationv1alpha1.HintRefusalNoWriteAccess,
		}))
		Expect(asks()).To(Equal(2), "no comment was handled twice")
		Expect(got.ResourceVersion).To(Equal(first.ResourceVersion), "a look that finds nothing new writes nothing")
	})

	It("ignores an edit after acceptance and records it once", func() {
		id := byWriter("/shadewright reanalyze: the sidecar")
		poll()
		stored := fetch().Status.Hints[0]
		clock = clock.Add(time.Minute)
		Expect(git.EditComment(number, id, "/shadewright reanalyze: scale it to 10 replicas", clock)).To(Succeed())
		poll()
		got := fetch()
		Expect(got.Status.Hints).To(Equal([]remediationv1alpha1.Hint{stored}), "the stored hint stays as it was")
		Expect(refusals()).To(Equal(map[int64]remediationv1alpha1.HintRefusal{
			id: remediationv1alpha1.HintRefusalEditedAfterAccept,
		}))
		refusedAt := got.Status.RefusedHints[0].RefusedAt
		clock = clock.Add(time.Minute)
		Expect(git.EditComment(number, id, "/shadewright reanalyze: again", clock)).To(Succeed())
		poll()
		Expect(fetch().Status.RefusedHints).To(HaveLen(1))
		Expect(fetch().Status.RefusedHints[0].RefusedAt).To(Equal(refusedAt))
		Expect(asks()).To(Equal(1))
	})

	It("refuses a comment read already and edited later to carry the command", func() {
		id := byWriter("looks fine to me")
		poll()
		clock = clock.Add(time.Minute)
		poll() // a second sweep moves since past the comment's creation
		clock = clock.Add(time.Minute)
		Expect(git.EditComment(number, id, "/shadewright reanalyze: the sidecar", clock)).To(Succeed())
		poll()
		Expect(fetch().Status.Hints).To(BeEmpty())
		Expect(refusals()).To(Equal(map[int64]remediationv1alpha1.HintRefusal{
			id: remediationv1alpha1.HintRefusalEditedAfterSeen,
		}))
		Expect(asks()).To(BeZero())
	})

	It("accepts a comment edited before it was first read, with its last text", func() {
		id := byWriter("looks fine")
		Expect(git.EditComment(number, id, "/shadewright reanalyze: the sidecar", clock.Add(time.Second))).To(Succeed())
		poll()
		got := fetch()
		Expect(got.Status.Hints).To(HaveLen(1))
		Expect(got.Status.Hints[0].Text).To(Equal("the sidecar"))
		Expect(got.Status.Hints[0].UpdatedAt.Time).To(BeTemporally("==", clock.Add(time.Second)))
	})

	DescribeTable("loses nothing and stores nothing twice when the status write is lost",
		func(after bool) {
			id := byWriter("/shadewright reanalyze: the sidecar")
			readOnly := comment(providers.AuthorPerson, readerID, "reader", "/shadewright reanalyze: me too")
			failed := false
			hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
				if len(rr.Status.Hints) > 0 && !failed {
					failed = true
					return true, after
				}
				return false, false
			}
			poll()
			Expect(failed).To(BeTrue())
			hook = nil
			poll() // a restart after the lost write or the lost answer
			got := fetch()
			Expect(got.Status.Hints).To(HaveLen(1))
			Expect(got.Status.Hints[0].CommentID).To(Equal(id))
			Expect(refusals()).To(Equal(map[int64]remediationv1alpha1.HintRefusal{
				readOnly: remediationv1alpha1.HintRefusalNoWriteAccess,
			}))
			Expect(got.Status.CommentCursor).NotTo(BeNil())
			if after {
				Expect(asks()).To(Equal(2), "the write went through: nothing is asked again")
			} else {
				Expect(asks()).To(Equal(4), "the write was lost: the same comments are handled again")
			}
		},
		Entry("before the write", false),
		Entry("after the write, the answer lost", true),
	)

	It("stores what came before a permission question that failed, and the rest on the next look", func() {
		readOnly := comment(providers.AuthorPerson, readerID, "reader", "/shadewright reanalyze: me too")
		id := byWriter("/shadewright reanalyze: the sidecar")
		git.SetFail(func(op string) error {
			if op == "CanWrite" && asks() == 2 {
				return &providers.RetryableError{Err: errors.New("GitHub author permission: status 502")}
			}
			return nil
		})
		poll()
		got := fetch()
		Expect(got.Status.Hints).To(BeEmpty())
		Expect(refusals()).To(HaveKey(readOnly), "the comment before the failure is handled")
		Expect(got.Status.CommentCursor).To(BeNil(), "the cursor stays until every comment of the look is handled")
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		git.SetFail(nil)
		poll()
		got = fetch()
		Expect(got.Status.Hints).To(HaveLen(1))
		Expect(got.Status.Hints[0].CommentID).To(Equal(id))
		Expect(asks()).To(Equal(3), "the refused comment was not asked about again")
	})

	It("asks about at most maxPermissionAsks authors in one look", func() {
		spam := make([]int64, 0, maxPermissionAsks+2)
		for i := range maxPermissionAsks + 2 {
			spam = append(spam, comment(providers.AuthorPerson, 3000+int64(i), fmt.Sprintf("s%d", i),
				"/shadewright reanalyze: x"))
		}
		poll()
		Expect(asks()).To(Equal(maxPermissionAsks))
		Expect(fetch().Status.RefusedHints).To(HaveLen(remediationv1alpha1.MaxRefusedHints),
			"only the newest refusals are kept")
		Expect(refusals()).To(HaveKeyWithValue(spam[len(spam)-1], remediationv1alpha1.HintRefusalPermissionNotChecked))
		Expect(fetch().Status.CommentCursor).NotTo(BeNil(), "the look is handled in full and the cursor moves on")
	})

	It("reads only the request's own pull request, and goes on when the host refuses the read", func() {
		Expect(git.CreateBranch(ctx, "person/fix", mustHead(git))).To(Succeed())
		foreign, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: "person/fix",
			BaseBranch: mainBranch, Title: "a person's change", Body: "no marker"})
		Expect(err).NotTo(HaveOccurred())
		git.AddComment(foreign.Number, providers.Feedback{ID: nextID.Add(1), CreatedAt: clock,
			Author: providers.FeedbackAuthor{ID: writerID, Login: writer, Kind: providers.AuthorPerson},
			Body:   "/shadewright reanalyze: on someone else's pull request"})
		git.SetFail(func(op string) error {
			if op == "ListComments" {
				return errors.New("GitHub list pull request comments: not a Shadewright pull request")
			}
			return nil
		})
		poll()
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "the watch goes on")
		Expect(got.Status.Hints).To(BeEmpty())
		Expect(got.Status.CommentCursor).To(BeNil())
		git.SetFail(nil)
		poll()
		got = fetch()
		Expect(got.Status.Hints).To(BeEmpty(), "a comment on another pull request is never read")
		Expect(got.Status.CommentCursor).NotTo(BeNil())
	})

	It("stores nothing while writes are paused, also when the pause comes during the look", func() {
		byWriter("/shadewright reanalyze: the sidecar")
		pause(true)
		poll()
		lists, _ := git.FeedbackCalls()
		Expect(lists).To(BeZero(), "a paused executor does not read the pull request")
		Expect(fetch().Status.Hints).To(BeEmpty())

		pause(false)
		git.SetFail(func(op string) error {
			if op == "CanWrite" {
				pause(true) // the pause is set while the look runs
			}
			return nil
		})
		poll()
		Expect(fetch().Status.Hints).To(BeEmpty(), "the kill switch is asked right before the write")
		git.SetFail(nil)
		pause(false)
		poll()
		Expect(fetch().Status.Hints).To(HaveLen(1))
	})

	Describe("answers", func() {
		var stored remediationv1alpha1.Hint
		BeforeEach(func() {
			byWriter("/shadewright reanalyze: the sidecar")
			poll()
			stored = fetch().Status.Hints[0]
		})
		answer := func(phase remediationv1alpha1.IncidentPhase, reason string, ref *remediationv1alpha1.HintRef) {
			inc := &remediationv1alpha1.Incident{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(incident), inc)).To(Succeed())
			inc.Status.Phase = phase
			inc.Status.HandledHint = ref
			meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{Type: conditionIncidentAnalyzed,
				Status: metav1.ConditionTrue, Reason: reason, Message: "fixed text"})
			Expect(k8sClient.Status().Update(ctx, inc)).To(Succeed())
		}
		refOf := func(h remediationv1alpha1.Hint) *remediationv1alpha1.HintRef {
			return &remediationv1alpha1.HintRef{Source: h.Source, CommentID: h.CommentID, Digest: h.Digest}
		}

		DescribeTable("declines a hint the analysis refused, with its reason",
			func(reason string) {
				answer(remediationv1alpha1.IncidentProposed, reason, refOf(stored))
				poll()
				h := fetch().Status.Hints[0]
				Expect(h.State).To(Equal(remediationv1alpha1.HintDeclined))
				Expect(h.Reason).To(Equal(reason))
			},
			Entry("cooldown", incidents.OutcomeHintCoolingDown),
			Entry("budget", incidents.OutcomeHintBudgetSpent),
			Entry("no proposal", string(analysis.ReasonNoProposal)),
			Entry("plan unchanged", string(analysis.ReasonPlanUnchanged)),
			Entry("model unavailable", string(analysis.ReasonModelUnavailable)),
		)

		It("leaves a hint Pending while it is analyzed, after a proposal, or when another hint was handled", func() {
			answer(remediationv1alpha1.IncidentAnalyzing, string(analysis.ReasonNoProposal), refOf(stored))
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending))
			other := refOf(stored)
			other.Digest = "sha256:" + strings.Repeat("0", 64)
			answer(remediationv1alpha1.IncidentProposed, string(analysis.ReasonNoProposal), other)
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending))
			answer(remediationv1alpha1.IncidentProposed, string(analysis.ReasonProposed), refOf(stored))
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending),
				"a replacement answers it (the handoff)")
			clock = clock.Add(2 * HintAnswerTimeout)
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending),
				"an outcome with a replacement is an answer: no timeout")
		})

		It("declines a hint with no answer once HintAnswerTimeout has passed", func() {
			clock = clock.Add(HintAnswerTimeout - time.Second)
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending))
			clock = clock.Add(time.Second)
			poll()
			h := fetch().Status.Hints[0]
			Expect(h.State).To(Equal(remediationv1alpha1.HintDeclined))
			Expect(h.Reason).To(Equal(ReasonHintNoAnswer))
		})

		It("does not read an Incident of another UID as the answer", func() {
			Expect(k8sClient.Delete(ctx, incident)).To(Succeed())
			again := incident.DeepCopy()
			again.ResourceVersion, again.UID = "", ""
			Expect(k8sClient.Create(ctx, again)).To(Succeed())
			incident = again
			answer(remediationv1alpha1.IncidentProposed, string(analysis.ReasonNoProposal), refOf(stored))
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending))
		})
	})
})

// pinnedBy checks the pin of a stored hint: the comment, the writer, Pending, accepted when it was updated.
func pinnedBy(id int64) OmegaMatcher {
	return SatisfyAll(
		WithTransform(func(h remediationv1alpha1.Hint) int64 { return h.CommentID }, Equal(id)),
		WithTransform(func(h remediationv1alpha1.Hint) remediationv1alpha1.HintSource { return h.Source },
			Equal(remediationv1alpha1.HintSourceGitHub)),
		WithTransform(func(h remediationv1alpha1.Hint) remediationv1alpha1.HintState { return h.State },
			Equal(remediationv1alpha1.HintPending)),
		WithTransform(func(h remediationv1alpha1.Hint) int64 { return h.Author.ID }, Equal(int64(1001))),
		WithTransform(func(h remediationv1alpha1.Hint) bool { return h.UpdatedAt.Equal(&h.AcceptedAt) }, BeTrue()),
	)
}

func mustHead(g *fakegit.Git) string {
	head, err := g.BranchHead(context.Background(), mainBranch)
	Expect(err).NotTo(HaveOccurred())
	return head
}

func TestHintOutcomesMatchTheAnalyzer(t *testing.T) {
	if conditionIncidentAnalyzed != incidents.ConditionAnalyzed || outcomeProposed != string(analysis.ReasonProposed) ||
		outcomeRunbookMatched != string(analysis.ReasonRunbookMatched) {
		t.Fatal("the executor's copies of the Analyzed condition and its outcomes differ from the analyzer's")
	}
}

func TestParseHintCommand(t *testing.T) {
	for _, tc := range []struct {
		body      string
		truncated bool
		text      string
		command   bool
		why       remediationv1alpha1.HintRefusal
	}{
		{body: "/shadewright reanalyze: the sidecar", text: "the sidecar", command: true},
		{body: "/shadewright reanalyze:the sidecar\r\nsecond line", text: "the sidecar", command: true},
		{body: "/shadewright reanalyze:  spaced  \n\n/shadewright reanalyze: two", text: "spaced", command: true},
		{body: "/shadewright reanalyze:", command: true, why: remediationv1alpha1.HintRefusalEmpty},
		{body: "/shadewright reanalyze: cut", truncated: true, command: true, why: remediationv1alpha1.HintRefusalTooLong},
		{body: "/shadewright reanalyze: whole\ncut", truncated: true, text: "whole", command: true},
		{body: "> /shadewright reanalyze: quoted"},
		{body: "```\n/shadewright reanalyze: in code"},
		{body: " /shadewright reanalyze: indented"},
		{body: "/shadewright reanalyse: spelt"},
		{body: "/shadewright reanalyze the colon is missing"},
		{body: "/SHADEWRIGHT REANALYZE: case"},
		{body: ""},
	} {
		text, command, why := parseHintCommand(tc.body, tc.truncated)
		if text != tc.text || command != tc.command || why != tc.why {
			t.Errorf("parseHintCommand(%q, %v) = %q, %v, %q; want %q, %v, %q", tc.body, tc.truncated, text, command,
				why, tc.text, tc.command, tc.why)
		}
	}
}

func TestCommentCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for _, c := range []providers.FeedbackCursor{
		{},
		{Since: at, ETag: `W/"abc"`},
		{Since: at, Page: 3, AfterID: 99, SweepStart: at.Add(time.Minute)},
	} {
		if got := cursorOf(cursorStatus(c)); got != c {
			t.Errorf("round trip of %+v gave %+v", c, got)
		}
	}
	// What the schema refuses is not stored: a sweep too long starts again from its since, a bad tag is dropped.
	got := cursorOf(cursorStatus(providers.FeedbackCursor{Since: at, Page: maxCursorPage + 1, AfterID: 7,
		SweepStart: at, ETag: "bad"}))
	if want := (providers.FeedbackCursor{Since: at}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	// A time is kept to the second, earlier, so the next look reads more, never less.
	got = cursorOf(cursorStatus(providers.FeedbackCursor{Since: at.Add(900 * time.Millisecond)}))
	if !got.Since.Equal(at) {
		t.Errorf("since %v, want %v", got.Since, at)
	}
}
