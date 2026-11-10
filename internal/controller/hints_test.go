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
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
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
		// writeTimeout is the reconciler's WriteTimeout; zero is the default.
		writeTimeout time.Duration
		// recorder is the reconciler's Event recorder; nil records none.
		recorder *record.FakeRecorder
	)

	reconciler := func() *RemediationRequestReconciler {
		c := k8sClient
		if hook != nil {
			c = &statusHook{Client: k8sClient, fail: hook}
		}
		r := &RemediationRequestReconciler{
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
			WriteTimeout:  writeTimeout,
		}
		if recorder != nil {
			r.Recorder = recorder
		}
		return r
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
		hook, hintsOff, writeTimeout, recorder = nil, false, 0, nil
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

	It("records no refusal of a read-only or outside author, refuses a writer's empty one, and ignores bots, "+
		"the App, reviews and anything that is not the command", func() {
		accepted := make([]int64, 0, remediationv1alpha1.MaxHints+1)
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
		accepted = append(accepted, byWriter("/shadewright reanalyze: three"), byWriter("/shadewright reanalyze: four"))
		poll()

		got := fetch()
		ids := make([]int64, 0, len(got.Status.Hints))
		for _, h := range got.Status.Hints {
			ids = append(ids, h.CommentID)
		}
		Expect(ids).To(Equal(accepted))
		Expect(refusals()).To(Equal(map[int64]remediationv1alpha1.HintRefusal{
			empty: remediationv1alpha1.HintRefusalEmpty,
		}), "a non-writer's refusal is counted, not recorded: it cannot push a record out")
		Expect(refusals()).NotTo(HaveKey(readOnly))
		Expect(refusals()).NotTo(HaveKey(outside))
		Expect(asks()).To(Equal(3), "one question per new author with the command; none for bots or Apps")
	})

	It("asks a non-writer first, and records none of the refusals a writer's command would get", func() {
		comment(providers.AuthorPerson, outsideID, "stranger", "/shadewright reanalyze:   ")
		comment(providers.AuthorPerson, readerID, "reader", "/shadewright reanalyze: "+
			strings.Repeat("a", remediationv1alpha1.MaxHintTextBytes+1))
		poll()
		Expect(fetch().Status.RefusedHints).To(BeEmpty())
		Expect(fetch().Status.Hints).To(BeEmpty())
		Expect(asks()).To(Equal(2))
	})

	It("judges a writer's comment after more than twice maxPermissionAsks commands of non-writers", func() {
		for i := range 2*maxPermissionAsks + 1 {
			comment(providers.AuthorPerson, 3000+int64(i), fmt.Sprintf("s%d", i), "/shadewright reanalyze: x")
		}
		late := byWriter("/shadewright reanalyze: after the crowd")
		for range 4 {
			poll()
			if len(fetch().Status.Hints) > 0 {
				break
			}
		}
		got := fetch()
		Expect(got.Status.Hints).To(HaveLen(1))
		Expect(got.Status.Hints[0].CommentID).To(Equal(late))
		Expect(got.Status.RefusedHints).To(BeEmpty())
		Expect(asks()).To(Equal(2*maxPermissionAsks+2), "each author was asked once: every look went on")
	})

	It("declines a Pending hint when someone else pushes to the pull request", func() {
		byWriter("/shadewright reanalyze: the sidecar")
		poll()
		got := fetch()
		Expect(got.Status.Hints).To(HaveLen(1))
		_, err := git.Push(freshness.BranchName(got), sourcePath, []byte("kind: Deployment\n"))
		Expect(err).NotTo(HaveOccurred())
		poll()
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(got.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintDeclined))
		Expect(got.Status.Hints[0].Reason).To(Equal(ReasonHintRequestEnded))
	})

	It("refuses a hint once MaxHints are Consumed or Reserved, and counts no Pending or Declined one", func() {
		for i := range remediationv1alpha1.MaxHints + 1 {
			byWriter(fmt.Sprintf("/shadewright reanalyze: hint %d", i))
		}
		poll()
		got := fetch()
		Expect(got.Status.Hints).To(HaveLen(remediationv1alpha1.MaxHints+1), "Pending hints do not count")
		for i := range remediationv1alpha1.MaxHints {
			got.Status.Hints[i].State = remediationv1alpha1.HintReserved
		}
		got.Status.Hints[remediationv1alpha1.MaxHints].State = remediationv1alpha1.HintDeclined
		got.Status.Hints[remediationv1alpha1.MaxHints].Reason = ReasonHintNoAnswer
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		next := byWriter("/shadewright reanalyze: one too many")
		poll()
		Expect(refusals()).To(HaveKeyWithValue(next, remediationv1alpha1.HintRefusalCapReached))
		Expect(fetch().Status.Hints).To(HaveLen(remediationv1alpha1.MaxHints + 1))
	})

	It("stores no hint once the list is full, refuses none, and passes the comment", func() {
		for i := range remediationv1alpha1.MaxStoredHints {
			byWriter(fmt.Sprintf("/shadewright reanalyze: hint %d", i))
		}
		poll()
		Expect(fetch().Status.Hints).To(HaveLen(remediationv1alpha1.MaxStoredHints))
		before := fetch().Status.CommentCursor

		clock = clock.Add(time.Minute)
		extra := byWriter("/shadewright reanalyze: no room")
		poll()
		got := fetch()
		Expect(got.Status.Hints).To(HaveLen(remediationv1alpha1.MaxStoredHints))
		Expect(refusals()).NotTo(HaveKey(extra), "a comment with no room is not refused")
		Expect(got.Status.CommentCursor).NotTo(Equal(before), "the cursor passes it")
		Expect(asks()).To(Equal(1), "no question for a comment that cannot be stored")
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
		Expect(asks()).To(Equal(2), "the author is asked first, once per look")
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
		Expect(refusals()).NotTo(HaveKey(readOnly))
		Expect(asks()).To(Equal(2), "no comment was judged twice: the seen comments of the cursor")
		Expect(got.ResourceVersion).To(Equal(first.ResourceVersion), "a look that finds nothing new writes nothing")
	})

	It("ignores edits after acceptance and records them once, on the hint", func() {
		id := byWriter("/shadewright reanalyze: the sidecar")
		poll()
		stored := fetch().Status.Hints[0]
		for i := range remediationv1alpha1.MaxRefusedHints + 2 {
			clock = clock.Add(time.Minute)
			Expect(git.EditComment(number, id, fmt.Sprintf("/shadewright reanalyze: edit %d", i), clock)).To(Succeed())
			poll()
		}
		got := fetch()
		Expect(got.Status.Hints).To(HaveLen(1))
		Expect(got.Status.Hints[0].EditedAfterAccept).To(BeTrue())
		got.Status.Hints[0].EditedAfterAccept = false
		Expect(got.Status.Hints[0]).To(Equal(stored), "the stored hint stays as it was")
		Expect(got.Status.RefusedHints).To(BeEmpty(), "an edit of a stored hint takes no record from the ring")
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
		Expect(asks()).To(Equal(1), "the author is asked first")
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
			Expect(refusals()).NotTo(HaveKey(readOnly))
			Expect(got.Status.CommentCursor).NotTo(BeNil())
			if after {
				Expect(asks()).To(Equal(2), "the write went through: nothing is judged again")
			} else {
				Expect(asks()).To(Equal(4), "the write was lost: the same comments are handled again")
			}
		},
		Entry("before the write", false),
		Entry("after the write, the answer lost", true),
	)

	It("warns once per comment with an Event while the permission question keeps failing", func() {
		recorder = record.NewFakeRecorder(10)
		id := byWriter("/shadewright reanalyze: the sidecar")
		git.SetFail(func(op string) error {
			if op == opCanWrite {
				return &providers.RetryableError{Err: errors.New("GitHub author permission: status 502")}
			}
			return nil
		})
		r := reconciler()
		for range 3 {
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		}
		Expect(asks()).To(Equal(3), "every look asks again: the cursor stays at the comment")
		Expect(recorder.Events).To(HaveLen(1))
		Expect(<-recorder.Events).To(And(HavePrefix("Warning "+ReasonHintPermissionUnknown),
			ContainSubstring(fmt.Sprintf("comment %d", id))))
		Expect(fetch().Status.Hints).To(BeEmpty())
	})

	It("stores what came before a permission question that failed, and the rest on the next look", func() {
		readOnly := comment(providers.AuthorPerson, readerID, "reader", "/shadewright reanalyze: me too")
		id := byWriter("/shadewright reanalyze: the sidecar")
		git.SetFail(func(op string) error {
			if op == opCanWrite && asks() == 2 {
				return &providers.RetryableError{Err: errors.New("GitHub author permission: status 502")}
			}
			return nil
		})
		poll()
		got := fetch()
		Expect(got.Status.Hints).To(BeEmpty())
		Expect(refusals()).NotTo(HaveKey(readOnly))
		Expect(got.Status.CommentCursor).NotTo(BeNil(), "the cursor passes the comment judged before the failure")
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		git.SetFail(nil)
		poll()
		got = fetch()
		Expect(got.Status.Hints).To(HaveLen(1))
		Expect(got.Status.Hints[0].CommentID).To(Equal(id))
		Expect(asks()).To(Equal(3), "the refused comment was not asked about again")
	})

	It("asks about at most maxPermissionAsks authors in one look, and judges the rest on a later look", func() {
		crowd := make([]int64, 0, maxPermissionAsks)
		for i := range maxPermissionAsks {
			crowd = append(crowd, comment(providers.AuthorPerson, 3000+int64(i), fmt.Sprintf("s%d", i),
				"/shadewright reanalyze: x"))
		}
		late := byWriter("/shadewright reanalyze: after the crowd")
		before := fetch().Status.CommentCursor
		poll()
		Expect(asks()).To(Equal(maxPermissionAsks))
		got := fetch()
		Expect(got.Status.Hints).To(BeEmpty())
		Expect(got.Status.RefusedHints).To(BeEmpty(), "neither the non-writers nor the comment not judged")
		Expect(got.Status.CommentCursor).NotTo(Equal(before), "the cursor passes the comments judged")
		Expect(got.Status.CommentCursor.AfterID).To(Equal(crowd[len(crowd)-1]), "and stops before the one not judged")

		poll()
		Expect(asks()).To(Equal(maxPermissionAsks+1), "only the author not asked yet is asked")
		got = fetch()
		Expect(got.Status.Hints).To(HaveLen(1))
		Expect(got.Status.Hints[0].CommentID).To(Equal(late))
		Expect(got.Status.CommentCursor).NotTo(BeNil(), "the look is handled in full and the cursor moves on")
	})

	It("gives the look at the comments its own deadline, apart from the check of the pull request", func() {
		byWriter("/shadewright reanalyze: the sidecar")
		writeTimeout = 2 * time.Second
		git.SetFail(func(op string) error {
			switch op {
			case opFind:
				time.Sleep(1400 * time.Millisecond)
			case "ListComments":
				time.Sleep(900 * time.Millisecond)
			}
			return nil
		})
		poll()
		git.SetFail(nil)
		Expect(fetch().Status.Hints).To(HaveLen(1), "the look did not run out of the check's time")
	})

	It("declines a Pending hint when the pull request is merged", func() {
		byWriter("/shadewright reanalyze: the sidecar")
		poll()
		Expect(fetch().Status.Hints).To(HaveLen(1))
		_, err := git.Merge(number)
		Expect(err).NotTo(HaveOccurred())
		poll()
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintDeclined))
		Expect(got.Status.Hints[0].Reason).To(Equal(ReasonHintPullRequestMerged))
	})

	It("declines a Pending hint when the pull request is closed without a merge", func() {
		byWriter("/shadewright reanalyze: the sidecar")
		poll()
		Expect(fetch().Status.Hints).To(HaveLen(1))
		Expect(git.CloseChangeRequest(ctx, number)).To(Succeed())
		poll()
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(got.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintDeclined))
		Expect(got.Status.Hints[0].Reason).To(Equal(ReasonHintPullRequestClosed))
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
			if op == opCanWrite {
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
			Entry("cooldown", "CoolingDown"),
			Entry("budget", "BudgetSpent"),
			Entry("no proposal", "NoProposal"),
			Entry("plan unchanged", "PlanUnchanged"),
			Entry("model unavailable", "ModelUnavailable"),
		)

		It("leaves a hint Pending while it is analyzed, after a proposal, or when another hint was handled", func() {
			answer(remediationv1alpha1.IncidentAnalyzing, "NoProposal", refOf(stored))
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending))
			other := refOf(stored)
			other.Digest = "sha256:" + strings.Repeat("0", 64)
			answer(remediationv1alpha1.IncidentProposed, "NoProposal", other)
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending))
			answer(remediationv1alpha1.IncidentProposed, outcomeProposed, refOf(stored))
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending),
				"a replacement answers it (the handoff)")
			clock = clock.Add(2 * HintAnswerTimeout)
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending),
				"an outcome with a replacement is an answer: no timeout")
		})

		It("keeps a hint answered with a replacement answered after the Incident handles another one", func() {
			answer(remediationv1alpha1.IncidentProposed, outcomeProposed, refOf(stored))
			poll()
			Expect(fetch().Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending))
			other := refOf(stored)
			other.CommentID++
			answer(remediationv1alpha1.IncidentProposed, "NoProposal", other)
			clock = clock.Add(2 * HintAnswerTimeout)
			poll()
			h := fetch().Status.Hints[0]
			Expect(h.State).To(Equal(remediationv1alpha1.HintPending), "the answer was stored when it was seen")
			Expect(h.Outcome).To(Equal(outcomeProposed))
		})

		It("declines a hint with no answer once HintAnswerTimeout has passed, also when the comments are not read",
			func() {
				git.SetFail(func(op string) error {
					if op == opFind {
						return &providers.RetryableError{Err: errors.New("GitHub find pull request: status 502")}
					}
					return nil
				})
				clock = clock.Add(HintAnswerTimeout)
				poll()
				h := fetch().Status.Hints[0]
				Expect(h.State).To(Equal(remediationv1alpha1.HintDeclined))
				Expect(h.Reason).To(Equal(ReasonHintNoAnswer))
			})

		// Hints off fails closed (ADR-0028 amendment of 2026-11-10): a hint stored while they were on is declined
		// on the first look, answered by a replacement or not, so neither the analysis nor a handoff acts on it.
		DescribeTable("declines a stored hint at once when the executor runs with reviewer hints off",
			func(answered bool) {
				if answered {
					answer(remediationv1alpha1.IncidentProposed, outcomeProposed, refOf(stored))
					poll()
					Expect(fetch().Status.Hints[0].Outcome).To(Equal(outcomeProposed))
				}
				hintsOff = true
				poll()
				h := fetch().Status.Hints[0]
				Expect(h.State).To(Equal(remediationv1alpha1.HintDeclined))
				Expect(h.Reason).To(Equal(remediationv1alpha1.ReasonHintsTurnedOff))
			},
			Entry("not answered yet", false),
			Entry("answered with a replacement", true),
		)

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
			answer(remediationv1alpha1.IncidentProposed, "NoProposal", refOf(stored))
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

// The executor keeps copies of the analyzer's Analyzed condition and outcomes; importing the analyzer here would
// register its metrics in this binary. TestOutcomesTheExecutorCopies in internal/analysis pins the same values.
func TestHintOutcomesMatchTheAnalyzer(t *testing.T) {
	if conditionIncidentAnalyzed != "Analyzed" || outcomeProposed != "Proposed" || outcomeRunbookMatched != "RunbookMatched" {
		t.Fatal("the executor's copies of the Analyzed condition and its outcomes changed")
	}
}

// Every path that ends a request goes through setPhase, so a Pending hint never outlives its request.
func TestSetPhaseDeclinesPendingHintsWhenTheRequestEnds(t *testing.T) {
	closedBy := func(reason string) []metav1.Condition {
		return []metav1.Condition{{Type: remediationv1alpha1.ConditionPROpened, Status: metav1.ConditionTrue, Reason: reason}}
	}
	for _, tc := range []struct {
		name       string
		phase      remediationv1alpha1.RemediationPhase
		conditions []metav1.Condition
		want       string
	}{
		{name: "still waiting", phase: remediationv1alpha1.PhaseAwaitingApproval},
		{name: "a merge", phase: remediationv1alpha1.PhaseMerged, want: ReasonHintPullRequestMerged},
		{name: "merged with a foreign change", phase: remediationv1alpha1.PhaseNeedsIntervention,
			conditions: closedBy(ReasonChangeRequestMerged), want: ReasonHintPullRequestMerged},
		{name: "closed", phase: remediationv1alpha1.PhaseNeedsIntervention,
			conditions: closedBy(ReasonChangeRequestClosed), want: ReasonHintPullRequestClosed},
		{name: "closed by Shadewright", phase: remediationv1alpha1.PhaseSuperseded,
			conditions: closedBy(ReasonPullRequestClosed), want: ReasonHintPullRequestClosed},
		{name: "head moved", phase: remediationv1alpha1.PhaseNeedsIntervention, want: ReasonHintRequestEnded},
		{name: "superseded", phase: remediationv1alpha1.PhaseSuperseded, want: ReasonHintRequestEnded},
		{name: "cancelled", phase: remediationv1alpha1.PhaseCancelled, want: ReasonHintRequestEnded},
		{name: "expired", phase: remediationv1alpha1.PhaseExpired, want: ReasonHintRequestEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := &remediationv1alpha1.RemediationRequest{}
			rr.Status.Phase = remediationv1alpha1.PhaseAwaitingApproval
			rr.Status.Conditions = tc.conditions
			rr.Status.Hints = []remediationv1alpha1.Hint{
				{CommentID: 1, State: remediationv1alpha1.HintPending},
				{CommentID: 2, State: remediationv1alpha1.HintReserved},
				{CommentID: 3, State: remediationv1alpha1.HintDeclined, Reason: "BudgetSpent"},
			}
			(&RemediationRequestReconciler{}).setPhase(rr, tc.phase)
			h := rr.Status.Hints
			if tc.want == "" {
				if h[0].State != remediationv1alpha1.HintPending {
					t.Fatalf("a request still waiting declined its hint: %+v", h[0])
				}
				return
			}
			if h[0].State != remediationv1alpha1.HintDeclined || h[0].Reason != tc.want {
				t.Fatalf("Pending hint is %s %q; want Declined %q", h[0].State, h[0].Reason, tc.want)
			}
			if h[1].State != remediationv1alpha1.HintReserved || h[2].Reason != "BudgetSpent" {
				t.Fatalf("only Pending hints change: %+v", h)
			}
		})
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

// slowCommentsGit is a provider whose comment list answers only when its context ends.
type slowCommentsGit struct{ *fakegit.Git }

func (g *slowCommentsGit) ListComments(ctx context.Context, _ int64, _ providers.FeedbackCursor) (
	providers.FeedbackPage, error) {
	<-ctx.Done()
	return providers.FeedbackPage{}, ctx.Err()
}

// A look at the comments that keeps using up its deadline must not keep an overdue hint Pending: the answers,
// the kill switch read and the status update get a deadline of their own. The interceptors refuse a call on an
// ended context, as the real client does.
func TestSlowCommentsDoNotStarveHintAnswers(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	r, rr := answerFixture(t, now.Add(-2*HintAnswerTimeout), "")
	rr.Status.Phase = remediationv1alpha1.PhaseAwaitingApproval
	rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 1}
	refuseEnded := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	c := clientfake.NewClientBuilder().WithScheme(r.APIReader.(client.Client).Scheme()).
		WithStatusSubresource(rr).WithObjects(rr, fx.RemediationSettings()).
		WithInterceptorFuncs(refuseEnded).Build()
	r.Client, r.APIReader = c, c
	r.ReviewerHints = true
	r.WriteTimeout = 20 * time.Millisecond
	r.Now = func() time.Time { return now }
	look := hintLook{git: &gatedGit{git: &slowCommentsGit{Git: fakegit.NewGit(mainBranch, nil)}},
		host: remediationv1alpha1.GitProviderGitHub}
	for range 2 {
		// The reconcile's own context; the guard only keeps a broken build from hanging.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		r.watchHints(ctx, look, rr)
		cancel()
	}
	stored := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(rr), stored); err != nil {
		t.Fatal(err)
	}
	if h := stored.Status.Hints[0]; h.State != remediationv1alpha1.HintDeclined || h.Reason != ReasonHintNoAnswer {
		t.Fatalf("overdue hint stayed %s (%q) after slow looks at the comments; want Declined/NoAnswer", h.State,
			h.Reason)
	}
}
