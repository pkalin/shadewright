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
	"errors"
	"fmt"
	"strings"
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

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/gitops/sourcemap"
	"github.com/shadewright-labs/shadewright/internal/policy/freshness"
	"github.com/shadewright-labs/shadewright/internal/providers"
	fakegit "github.com/shadewright-labs/shadewright/internal/providers/fake"
	fx "github.com/shadewright-labs/shadewright/internal/testfixtures"
)

const (
	// oomReason is the symptom of the Incident the handoff specs answer.
	oomReason = "OOMKilled"
	// opAppend is the fake Git provider's name of the AppendCommit write.
	opAppend = "AppendCommit"
)

var _ = Describe("RemediationRequest supersede handoff", func() {
	ctx := context.Background()
	const writerID = int64(1001)
	var (
		seq        int
		deployment *appsv1.Deployment
		policy     *remediationv1alpha1.RemediationPolicy
		settings   *remediationv1alpha1.RemediationSettings
		incident   *remediationv1alpha1.Incident
		pred       *remediationv1alpha1.RemediationRequest
		repl       *remediationv1alpha1.RemediationRequest
		git        *fakegit.Git
		clock      time.Time
		number     int64
		oldHead    string
		hint       remediationv1alpha1.Hint
		// hook, when set, is the status hook of the reconciler's client.
		hook func(rr *remediationv1alpha1.RemediationRequest) (fail, after bool)
		// stale, when set, is what the reconciler's client reads for the predecessor, as a cache behind the API.
		stale *remediationv1alpha1.RemediationRequest
		// replacements counts the replacements of the spec, so each has its own name.
		replacements int
		// hintsOff builds the reconciler without ReviewerHints, as an executor restarted with the flag unset.
		hintsOff bool
	)

	reconciler := func() *RemediationRequestReconciler {
		c := k8sClient
		if hook != nil {
			c = &statusHook{Client: k8sClient, fail: hook}
		}
		if stale != nil {
			c = &staleRead{Client: c, obj: stale}
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
	// pollOf runs one reconcile of obj with a new reconciler, as after a restart.
	pollOf := func(obj client.Object) {
		_, _ = reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
	}
	get := func(obj *remediationv1alpha1.RemediationRequest) *remediationv1alpha1.RemediationRequest {
		got := &remediationv1alpha1.RemediationRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), got)).To(Succeed())
		return got
	}
	branch := func() string { return "shadewright/" + string(pred.UID) }
	head := func() string {
		h, err := git.BranchHead(ctx, branch())
		Expect(err).NotTo(HaveOccurred())
		return h
	}
	fileAt := func(sha string) string {
		f, err := git.ReadFile(ctx, sha, sourcePath, providers.MaxFileBytes)
		Expect(err).NotTo(HaveOccurred())
		return string(f.Data)
	}
	pause := func(paused bool) {
		s := &remediationv1alpha1.RemediationSettings{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, s)).To(Succeed())
		s.Spec.Paused = &paused
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
	}
	condition := func(rr *remediationv1alpha1.RemediationRequest, t string) *metav1.Condition {
		return meta.FindStatusCondition(rr.Status.Conditions, t)
	}
	ref := func(h remediationv1alpha1.Hint) remediationv1alpha1.HintRef {
		return remediationv1alpha1.HintRef{Source: h.Source, CommentID: h.CommentID, Digest: h.Digest}
	}
	// replace creates the replacement of pred answering h with a new desired limit.
	replace := func(desired string, h remediationv1alpha1.HintRef) {
		replacements++
		p := get(pred)
		repl = &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace,
			Name: fmt.Sprintf("handoff-repl-%d-%d", seq, replacements)}, Spec: *p.Spec.DeepCopy()}
		repl.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse(desired)
		repl.Spec.Replaces = &remediationv1alpha1.RequestReplacement{Name: remediationv1alpha1.Name(p.Name),
			UID: remediationv1alpha1.UID(p.UID), Hint: h}
		repl.Spec.ChainRoot = remediationv1alpha1.UID(p.UID)
		Expect(k8sClient.Create(ctx, repl)).To(Succeed())
	}
	// drive polls the replacement, then the predecessor, n times.
	drive := func(n int) {
		for range n {
			pollOf(repl)
			pollOf(pred)
		}
	}
	// pollUntil polls the replacement until done holds, at most 10 times.
	pollUntil := func(done func(*remediationv1alpha1.RemediationRequest) bool) {
		for range 10 {
			if done(get(repl)) {
				return
			}
			pollOf(repl)
		}
		Expect(done(get(repl))).To(BeTrue(), "the replacement did not reach the expected step")
	}
	reserved := func(rr *remediationv1alpha1.RemediationRequest) bool {
		return rr.Status.Handoff != nil && rr.Status.Handoff.From != nil
	}
	// untilIntent polls the replacement until the predecessor records its intent.
	untilIntent := func() {
		for range 10 {
			if p := get(pred); p.Status.Handoff != nil && p.Status.Handoff.To != nil {
				return
			}
			pollOf(repl)
		}
		Fail("the predecessor recorded no intent")
	}
	// answer makes the Incident say the analysis answered the hint with a replacement.
	answer := func() {
		inc := &remediationv1alpha1.Incident{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(incident), inc)).To(Succeed())
		inc.Status.Phase = remediationv1alpha1.IncidentProposed
		r := ref(hint)
		inc.Status.HandledHint = &r
		meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{Type: conditionIncidentAnalyzed,
			Status: metav1.ConditionTrue, Reason: outcomeProposed, Message: "fixed text"})
		Expect(k8sClient.Status().Update(ctx, inc)).To(Succeed())
	}

	BeforeEach(func() {
		seq++
		clock = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
		hook, stale, repl, hintsOff = nil, nil, nil, false
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fx.TargetNamespace}})
		Expect(client.IgnoreAlreadyExists(err)).To(Succeed())
		deployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: fx.TargetNamespace, Name: fmt.Sprintf("handoff-%d", seq)},
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
		policy = fx.RemediationPolicy(fmt.Sprintf("handoff-policy-%d", seq))
		policy.Spec.Actions = []remediationv1alpha1.PolicyAction{fx.MemoryLimitPolicyAction()}
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 100
		policy.Spec.RequestNamespaces = []remediationv1alpha1.Name{requestNamespace}
		policy.Spec.GitOps.AllowedPaths = []remediationv1alpha1.GitPathPattern{appsGlob}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		settings = fx.RemediationSettings()
		settings.Spec.Repositories[0].TargetBranch = mainBranch
		Expect(k8sClient.Create(ctx, settings)).To(Succeed())
		pred = fx.MemoryLimitRequest(requestNamespace, fmt.Sprintf("handoff-rr-%d", seq))
		pred.Spec.PolicyRef = remediationv1alpha1.Name(policy.Name)
		pred.Spec.TargetRef.Name = deployment.Name
		pred.Spec.TargetRef.UID = remediationv1alpha1.UID(deployment.UID)
		target := pred.Spec.TargetRef
		incident = &remediationv1alpha1.Incident{
			ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace, Name: fmt.Sprintf("handoff-incident-%d", seq)},
			Spec: remediationv1alpha1.IncidentSpec{
				Fingerprint: "sha256:" + strings.Repeat("cd", 32), Reason: oomReason,
				Subject: remediationv1alpha1.ObjectRef{APIVersion: target.APIVersion, Kind: target.Kind, Name: target.Name},
			},
		}
		Expect(k8sClient.Create(ctx, incident)).To(Succeed())
		pred.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: incident.Name,
			UID: remediationv1alpha1.UID(incident.UID)}
		Expect(k8sClient.Create(ctx, pred)).To(Succeed())
		git = fakegit.NewGit(mainBranch, map[string][]byte{
			sourcePath:     deploymentManifest(deployment.Name, deployment.Namespace, workerContainer, "256Mi"),
			buildKustomize: buildFile,
		})
		git.SetFeedbackClock(func() time.Time { return clock })
		git.SetWriter(writerID, true)
		for range 20 {
			pollOf(pred)
			if get(pred).Status.Phase == remediationv1alpha1.PhaseAwaitingApproval {
				break
			}
		}
		got := get(pred)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		number, oldHead = got.Status.PullRequest.Number, got.Status.PullRequest.HeadSHA
		git.AddComment(number, providers.Feedback{ID: 7001,
			Author: providers.FeedbackAuthor{ID: writerID, Login: "reviewer-one", Kind: providers.AuthorPerson},
			Body:   "/shadewright reanalyze: the limit is still too low", CreatedAt: clock})
		pollOf(pred)
		Expect(get(pred).Status.Hints).To(HaveLen(1))
		hint = get(pred).Status.Hints[0]
		Expect(hint.State).To(Equal(remediationv1alpha1.HintPending))
	})
	AfterEach(func() {
		for _, rr := range []*remediationv1alpha1.RemediationRequest{repl, pred} {
			if rr == nil {
				continue
			}
			got := &remediationv1alpha1.RemediationRequest{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rr), got); err == nil &&
				controllerutil.ContainsFinalizer(got, FinalizerName) {
				base := got.DeepCopy()
				controllerutil.RemoveFinalizer(got, FinalizerName)
				Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, rr))).To(Succeed())
		}
		for _, o := range []client.Object{incident, policy, settings, deployment} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName},
				&remediationv1alpha1.RemediationSettings{}))
		}).Should(BeTrue())
	})

	It("takes the pull request over step by step, each step after a restart", func() {
		replace("512Mi", ref(hint))

		By("step 1: the admission takes the lock and names the predecessor, which keeps its own")
		pollUntil(func(rr *remediationv1alpha1.RemediationRequest) bool { return rr.Status.Phase != "" })
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
		Expect(r.Status.HoldsTarget).To(Equal(new(true)))
		Expect(r.Status.Handoff.From.Phase).To(Equal(remediationv1alpha1.HandoffReserved))
		Expect(r.Status.Handoff.From.Hint).To(Equal(ref(hint)))
		Expect(get(pred).Status.HoldsTarget).To(Equal(new(true)))

		By("step 2: the predecessor records the intent and reserves the hint")
		untilIntent()
		p := get(pred)
		Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffIntent))
		Expect(p.Status.Handoff.To.UID).To(Equal(remediationv1alpha1.UID(repl.UID)))
		Expect(p.Status.Handoff.To.ExpectedHead).To(Equal(oldHead))
		Expect(p.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintReserved))
		Expect(head()).To(Equal(oldHead), "nothing is written to Git before the intent")

		By("a look of the predecessor while the intent stands changes nothing")
		pollOf(pred)
		Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))

		By("steps 3 and 4: one commit on top of the old head, and the replacement records the pull request")
		pollUntil(tookOver)
		newHead := head()
		parents, err := git.Parents(ctx, newHead)
		Expect(err).NotTo(HaveOccurred())
		Expect(parents).To(Equal([]string{oldHead}))
		Expect(fileAt(newHead)).To(ContainSubstring("512Mi"))
		r = get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhasePRCreated))
		Expect(r.Status.PullRequest.Number).To(Equal(number))
		Expect(r.Status.PullRequest.HeadSHA).To(Equal(newHead))
		Expect(r.Status.Hints).To(HaveLen(1))
		Expect(r.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintConsumed))
		Expect(r.Status.CommentCursor).NotTo(BeNil(), "the replacement reads comments on from the predecessor's cursor")

		By("the predecessor's watch takes the replacement's commit for the expected head")
		pollOf(pred)
		Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))

		By("steps 5 and 6: the predecessor ends HandedOff, the pull request is rewritten and the check run created")
		pollOf(repl)
		p = get(pred)
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(condition(p, remediationv1alpha1.ConditionApproved).Reason).To(Equal(remediationv1alpha1.ReasonHandedOff))
		Expect(p.Status.HoldsTarget).To(Equal(new(false)))
		Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffDone))
		Expect(p.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintConsumed))
		Expect(prMayBeOpen(p)).To(BeTrue(), "the one Superseded whose pull request may be open")
		r = get(repl)
		Expect(r.Status.Handoff.From.PullRequestUpdate).To(Equal(remediationv1alpha1.HandoffStepDone))
		Expect(r.Status.PullRequest.ReviewCheck).To(Equal(&remediationv1alpha1.ReviewCheck{Head: newHead, State: remediationv1alpha1.ReviewCheckInProgress}))
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(r.Status.HoldsTarget).To(Equal(new(true)))
		crs := git.ChangeRequests()
		Expect(crs).To(HaveLen(1), "a hint never opens another pull request")
		Expect(crs[0].State).To(Equal(providers.ChangeRequestOpen))
		Expect(hasMarker(crs[0].Body, r)).To(BeTrue())
		Expect(crs[0].Body).To(ContainSubstring(marker(r)))
		Expect(crs[0].Title).To(ContainSubstring("512Mi"))
		Expect(git.CheckRuns(newHead)).To(ConsistOf(HaveField("Name", providers.CheckRunName)))

		By("the replacement keeps watching the pull request, its chain of two commits included")
		clock = clock.Add(time.Hour)
		_, err = git.Push(mainBranch, "apps/other.yaml", []byte("a: 1\n"))
		Expect(err).NotTo(HaveOccurred())
		drive(2)
		Expect(get(repl).Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(git.CheckRuns(newHead)).To(HaveLen(1), "the check run is created once")

		By("a merge now is the replacement's")
		_, err = git.Merge(number)
		Expect(err).NotTo(HaveOccurred())
		drive(2)
		Expect(get(repl).Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
	})

	It("recovers a lost status write after the branch update without a second commit", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
			return rr.UID == repl.UID && tookOver(rr), false
		}
		pollOf(repl)
		Expect(tookOver(get(repl))).To(BeFalse(), "the record of step 4 was lost")
		updated := head()
		Expect(updated).NotTo(Equal(oldHead))
		hook = nil
		pollUntil(tookOver)
		Expect(head()).To(Equal(updated), "the restart found its own commit and added none")
		Expect(get(repl).Status.PullRequest.HeadSHA).To(Equal(updated))
	})

	It("recovers a lost answer of the branch update", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		git.SetAfter(func(op string) error {
			if op == opAppend {
				return errors.New("the answer was lost")
			}
			return nil
		})
		pollOf(repl)
		git.SetAfter(nil)
		updated := head()
		Expect(updated).NotTo(Equal(oldHead))
		pollUntil(tookOver)
		Expect(head()).To(Equal(updated))
	})

	It("retries the predecessor's end after a lost write", func() {
		replace("512Mi", ref(hint))
		pollUntil(tookOver)
		hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
			return rr.UID == pred.UID && rr.Status.Phase == remediationv1alpha1.PhaseSuperseded, false
		}
		pollOf(repl)
		Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		hook = nil
		pollOf(repl)
		Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
	})

	It("aborts the handoff when the pull request is merged right before the update", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		// After the freshness check, which a merge seen earlier ends as a stale plan: the read of the pull
		// request right before the update is what finds it merged.
		merged := false
		git.SetFail(func(op string) error {
			if op == opGet && !merged {
				merged = true
				_, err := git.Merge(number)
				return err
			}
			return nil
		})
		pollOf(repl)
		git.SetFail(nil)
		Expect(merged).To(BeTrue())
		drive(2)
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
		Expect(condition(r, remediationv1alpha1.ConditionPROpened).Reason).To(Equal(remediationv1alpha1.ReasonHandoffAborted))
		Expect(r.Status.Handoff.From.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
		Expect(r.Status.HoldsTarget).To(Equal(new(false)))
		Expect(head()).To(Equal(oldHead), "no update after a merge")
		p := get(pred)
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
		Expect(p.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintDeclined))
		Expect(p.Status.Hints[0].Reason).To(Equal(ReasonHintPullRequestMerged))
	})

	It("aborts the handoff when a squash merge of the old head lands during the update", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		git.SetBetween(func(op string) {
			if op == opAppend {
				_, err := git.Merge(number)
				Expect(err).NotTo(HaveOccurred())
			}
		})
		pollOf(repl)
		git.SetBetween(nil)
		drive(2)
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
		Expect(condition(r, remediationv1alpha1.ConditionPROpened).Reason).To(Equal(remediationv1alpha1.ReasonHandoffAborted))
		Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged), "the merged head was the old one")
	})

	It("gives the merge to the replacement when the merged head is its commit", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		git.SetAfter(func(op string) error {
			if op == opAppend {
				_, err := git.Merge(number)
				return err
			}
			return nil
		})
		pollOf(repl)
		git.SetAfter(nil)
		drive(2)
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(r.Status.PullRequest.MergeCommitSHA).NotTo(BeEmpty())
		p := get(pred)
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(condition(p, remediationv1alpha1.ConditionApproved).Reason).To(Equal(remediationv1alpha1.ReasonHandedOff))
	})

	// ADR-0030 section 1 (#678): the merge on the handoff path writes the receipt with the replacement's head.
	It("writes the merge receipt with the replacement's head when the merged head is its commit", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		var mergeSHA string
		git.SetAfter(func(op string) error {
			if op == opAppend {
				var err error
				mergeSHA, err = git.Merge(number)
				return err
			}
			return nil
		})
		pollOf(repl)
		git.SetAfter(nil)
		drive(2)
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		m := r.Status.MergeReceipt
		Expect(m).NotTo(BeNil(), "the receipt is written with Merged")
		Expect(m.HeadSHA).To(Equal(r.Status.PullRequest.HeadSHA))
		Expect(m.HeadSHA).NotTo(Equal(oldHead), "the replacement's commit, not the head it replaced")
		Expect(m.PullRequest).To(Equal(number))
		Expect(m.MergeCommitSHA).To(Equal(mergeSHA))
		Expect(m.Repository).To(Equal(*r.Status.PlanRepository))
		Expect(m.TargetBranch).To(Equal(r.Status.Source.TargetBranch))
		Expect(r.Status.PullRequest.MergedAt).NotTo(BeNil())
		Expect(m.MergedAt.Equal(r.Status.PullRequest.MergedAt)).To(BeTrue(), "the time of the write that set Merged")
		Expect(get(pred).Status.MergeReceipt).To(BeNil(), "the request it replaced did not merge")
	})

	It("records a merged pull request without a merge commit like an open one and waits for the commit (#678)", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		var mergeSHA string
		git.SetAfter(func(op string) error {
			if op == opAppend {
				var err error
				if mergeSHA, err = git.Merge(number); err != nil {
					return err
				}
				return git.SetMergeCommit(number, "")
			}
			return nil
		})
		pollOf(repl)
		git.SetAfter(nil)
		drive(2)
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhasePRCreated), "neither Merged nor ended while it waits")
		Expect(r.Status.MergeCommitAwaitedSince).NotTo(BeNil())
		Expect(r.Status.MergeReceipt).To(BeNil())

		clock = clock.Add(time.Minute)
		Expect(git.SetMergeCommit(number, mergeSHA)).To(Succeed())
		drive(1)
		r = get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(r.Status.MergeReceipt).NotTo(BeNil())
		Expect(r.Status.MergeReceipt.MergeCommitSHA).To(Equal(mergeSHA))
		Expect(r.Status.MergeReceipt.HeadSHA).To(Equal(r.Status.PullRequest.HeadSHA))
	})

	It("keeps the old pull request when policy denies the replacement, and declines the hint", func() {
		answer()
		pollOf(pred)
		Expect(get(pred).Status.Hints[0].Outcome).To(Equal(outcomeProposed))
		replace("1Gi", ref(hint)) // above the policy's 100% step
		drive(3)
		Expect(get(repl).Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
		p := get(pred)
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(p.Status.Handoff).To(BeNil())
		Expect(p.Status.HoldsTarget).To(Equal(new(true)))
		Expect(p.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintDeclined))
		Expect(p.Status.Hints[0].Reason).To(Equal(ReasonHintReplacementDenied))
		Expect(head()).To(Equal(oldHead))
		Expect(git.ChangeRequests()).To(ConsistOf(HaveField("State", providers.ChangeRequestOpen)))
	})

	It("denies a replacement whose chain link does not hold, and touches nothing", func() {
		answer()
		pollOf(pred)
		bad := ref(hint)
		bad.Digest = "sha256:" + strings.Repeat("0", 64)
		replace("512Mi", bad)
		drive(3)
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
		Expect(condition(r, remediationv1alpha1.ConditionPolicyAllowed).Reason).To(Equal(remediationv1alpha1.ReasonChainLinkInvalid))
		Expect(r.Status.Handoff).To(BeNil())
		Expect(r.Status.HoldsTarget).To(Equal(new(false)))
		p := get(pred)
		Expect(p.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintPending), "an invalid link takes not even the hint")
		Expect(p.Status.Handoff).To(BeNil())
		Expect(head()).To(Equal(oldHead))
	})

	// Hints off fails closed (ADR-0028 amendment of 2026-11-10): an executor restarted with the flag unset takes
	// nothing a hint stored before asked for, at whatever step the handoff stood.
	Describe("with reviewer hints turned off", func() {
		// keptAsItWas checks that the predecessor keeps its pull request as it was and declined the hint.
		keptAsItWas := func() {
			p := get(pred)
			Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			Expect(p.Status.HoldsTarget).To(Equal(new(true)))
			Expect(p.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintDeclined))
			Expect(p.Status.Hints[0].Reason).To(Equal(remediationv1alpha1.ReasonHintsTurnedOff))
			Expect(head()).To(Equal(oldHead), "nothing was pushed")
			Expect(git.ChangeRequests()).To(ConsistOf(HaveField("State", providers.ChangeRequestOpen)))
		}
		// cancelled checks that the replacement ended without the handoff, for hints being off.
		cancelled := func() {
			r := get(repl)
			Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
			Expect(condition(r, remediationv1alpha1.ConditionPROpened).Reason).To(
				Equal(remediationv1alpha1.ReasonHintsTurnedOff))
			Expect(tookOver(r)).To(BeFalse())
			Expect(r.Status.HoldsTarget).To(Equal(new(false)))
		}

		It("declines a stored hint and denies its replacement at admission", func() {
			answer()
			pollOf(pred)
			Expect(get(pred).Status.Hints[0].Outcome).To(Equal(outcomeProposed))
			hintsOff = true
			replace("512Mi", ref(hint))
			drive(3)
			r := get(repl)
			Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
			Expect(condition(r, remediationv1alpha1.ConditionPolicyAllowed).Reason).To(
				Equal(remediationv1alpha1.ReasonHintsTurnedOff))
			Expect(r.Status.Handoff).To(BeNil())
			keptAsItWas()
			Expect(get(pred).Status.Handoff).To(BeNil())
		})

		It("ends a replacement admitted before, without the intent", func() {
			replace("512Mi", ref(hint))
			pollUntil(func(rr *remediationv1alpha1.RemediationRequest) bool { return reserved(rr) })
			Expect(get(pred).Status.Handoff).To(BeNil())
			hintsOff = true
			drive(3)
			cancelled()
			keptAsItWas()
		})

		It("ends a replacement whose intent stands, and the predecessor aborts the intent", func() {
			replace("512Mi", ref(hint))
			untilIntent()
			hintsOff = true
			drive(3)
			cancelled()
			keptAsItWas()
			Expect(get(pred).Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
		})

		It("does not record a commit pushed before the restart, and the predecessor stops as for a foreign push", func() {
			replace("512Mi", ref(hint))
			untilIntent()
			hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
				return rr.UID == repl.UID && tookOver(rr), false
			}
			pollOf(repl)
			hook = nil
			Expect(head()).NotTo(Equal(oldHead), "the commit was pushed, its record lost")
			hintsOff = true
			drive(3)
			// Its commit is on the open pull request, so it keeps the lock until a verdict (#786, decision 3).
			r := get(repl)
			Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
			Expect(condition(r, remediationv1alpha1.ConditionPROpened).Reason).To(
				Equal(remediationv1alpha1.ReasonHintsTurnedOff))
			Expect(tookOver(r)).To(BeFalse())
			Expect(r.Status.HoldsTarget).To(Equal(new(true)))
			p := get(pred)
			Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			Expect(git.ChangeRequests()).To(ConsistOf(HaveField("State", providers.ChangeRequestOpen)), "the pull request stays open for a person")
			Expect(p.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintDeclined))
			Expect(git.CheckRuns(head())).To(BeEmpty(), "no check run is created with hints off")
		})

		// Every state a replacement can be left in by a restart, before and after each persisted write of the
		// handoff (handoffStep, recoverStep, afterHandoff), then an executor with hints off: the replacement never
		// reaches AwaitingApproval, never supersedes its predecessor and never rewrites the pull request, whichever
		// of the two is looked at first.
		type offState struct {
			name string
			// setup brings the handoff to the state, with hints on.
			setup func()
			// replPhase is the phase the replacement ends in; predPhase the predecessor's.
			replPhase, predPhase remediationv1alpha1.RemediationPhase
		}
		lose := func(lost func(rr *remediationv1alpha1.RemediationRequest) bool) {
			hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
				return rr.UID == repl.UID && lost(rr), false
			}
			pollOf(repl)
			hook = nil
		}
		states := []offState{
			{
				name: "admitted, before the intent",
				setup: func() {
					pollUntil(func(rr *remediationv1alpha1.RemediationRequest) bool { return reserved(rr) })
				},
				replPhase: remediationv1alpha1.PhaseCancelled, predPhase: remediationv1alpha1.PhaseAwaitingApproval,
			},
			{
				name:      "intent recorded, before the branch update",
				setup:     untilIntent,
				replPhase: remediationv1alpha1.PhaseCancelled, predPhase: remediationv1alpha1.PhaseAwaitingApproval,
			},
			{
				name: "branch updated, its record lost",
				setup: func() {
					untilIntent()
					lose(tookOver)
				},
				replPhase: remediationv1alpha1.PhaseCancelled, predPhase: remediationv1alpha1.PhaseNeedsIntervention,
			},
			{
				name: "takeover recorded, before the predecessor's end",
				setup: func() {
					pollUntil(tookOver)
					Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
				},
				replPhase: remediationv1alpha1.PhaseNeedsIntervention, predPhase: remediationv1alpha1.PhaseNeedsIntervention,
			},
			{
				name: "predecessor ended, the replacement's writes after it lost",
				setup: func() {
					pollUntil(tookOver)
					lose(func(*remediationv1alpha1.RemediationRequest) bool { return true })
					Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
					r := get(repl)
					Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhasePRCreated))
					Expect(r.Status.Handoff.From.PullRequestUpdate).To(BeEmpty())
				},
				replPhase: remediationv1alpha1.PhaseNeedsIntervention, predPhase: remediationv1alpha1.PhaseSuperseded,
			},
			{
				name: "rewritten, the move to AwaitingApproval lost",
				setup: func() {
					pollUntil(tookOver)
					lose(func(rr *remediationv1alpha1.RemediationRequest) bool {
						return rr.Status.Phase == remediationv1alpha1.PhaseAwaitingApproval
					})
					r := get(repl)
					Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhasePRCreated))
					Expect(r.Status.Handoff.From.PullRequestUpdate).To(Equal(remediationv1alpha1.HandoffStepDone))
				},
				replPhase: remediationv1alpha1.PhaseNeedsIntervention, predPhase: remediationv1alpha1.PhaseSuperseded,
			},
			{
				name: "waiting for review with its check run",
				setup: func() {
					pollUntil(func(rr *remediationv1alpha1.RemediationRequest) bool {
						return rr.Status.PullRequest != nil && rr.Status.PullRequest.ReviewCheck != nil
					})
				},
				replPhase: remediationv1alpha1.PhaseNeedsIntervention, predPhase: remediationv1alpha1.PhaseSuperseded,
			},
		}
		entries := make([]TableEntry, 0, 2*len(states))
		for _, s := range states {
			entries = append(entries, Entry(s.name+", the replacement looked at first", s, false),
				Entry(s.name+", the predecessor looked at first", s, true))
		}
		DescribeTable("never adopts a head a hint steered after a restart",
			func(s offState, predFirst bool) {
				replace("512Mi", ref(hint))
				s.setup()
				title, checks := git.ChangeRequests()[0].Title, len(git.CheckRuns(head()))
				wasSuperseded := get(pred).Status.Phase == remediationv1alpha1.PhaseSuperseded
				var awaiting, superseded bool
				hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
					switch {
					case rr.UID == repl.UID && rr.Status.Phase == remediationv1alpha1.PhaseAwaitingApproval:
						awaiting = true
					case rr.UID == pred.UID && rr.Status.Phase == remediationv1alpha1.PhaseSuperseded:
						superseded = true
					}
					return false, false
				}
				hintsOff = true
				if predFirst {
					pollOf(pred)
				}
				drive(3)
				hook = nil
				Expect(awaiting).To(BeFalse(), "the replacement was moved to AwaitingApproval")
				Expect(superseded && !wasSuperseded).To(BeFalse(), "the predecessor was superseded")
				r := get(repl)
				Expect(r.Status.Phase).To(Equal(s.replPhase))
				Expect(get(pred).Status.Phase).To(Equal(s.predPhase))
				if s.replPhase == remediationv1alpha1.PhaseNeedsIntervention {
					Expect(condition(r, remediationv1alpha1.ConditionApproved).Reason).To(
						Equal(remediationv1alpha1.ReasonHintsTurnedOff))
				}
				crs := git.ChangeRequests()
				Expect(crs).To(ConsistOf(HaveField("State", providers.ChangeRequestOpen)),
					"the pull request stays open for a person")
				Expect(crs[0].Title).To(Equal(title), "the pull request was not rewritten")
				Expect(git.CheckRuns(head())).To(HaveLen(checks), "no check run is created with hints off")
			},
			entries,
		)
	})

	It("ends a refused recovery at its bound with its handoff Reserved, and the predecessor stops as for a foreign push (#786)", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
			return rr.UID == repl.UID && tookOver(rr), false
		}
		pollOf(repl)
		hook = nil
		Expect(head()).NotTo(Equal(oldHead), "the commit was pushed, its record lost")
		git.SetFail(func(op string) error {
			if op == opBranchHead {
				return fmt.Errorf("status 403: %w", providers.ErrForbidden)
			}
			return nil
		})
		pollOf(repl)
		Expect(condition(get(repl), remediationv1alpha1.ConditionPullRequestUnreadable).Reason).To(Equal(ReasonGitForbidden))
		clock = clock.Add(recoveryWaitBound + time.Minute)
		pollOf(repl)
		git.SetFail(nil)
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(condition(r, remediationv1alpha1.ConditionPROpened).Reason).To(Equal(ReasonRecoveryWaitExpired))
		// The end is no verdict: a later merge of its commit is still recorded (third decision of 2026-11-18).
		Expect(r.Status.Handoff.From.Phase).To(Equal(remediationv1alpha1.HandoffReserved))
		comments := len(git.Comments(number))
		for range 3 {
			pollOf(pred)
		}
		p := get(pred)
		Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(condition(p, remediationv1alpha1.ConditionApproved).Reason).To(Equal(ReasonBranchDiverged))
		Expect(git.Comments(number)).To(HaveLen(comments+1), "one comment hands the pull request to a person")
		Expect(git.ChangeRequests()).To(ConsistOf(HaveField("State", providers.ChangeRequestOpen)),
			"the pull request stays open for a person")
	})

	// The waits of a recovery whose commit was pushed and whose takeover write was lost (decisions of 2026-11-18 on
	// #786): a 503 past the deadline records the HandoffGrace entry (the deadline plus the grace); the first refusal
	// aborts the handoff and records the HandoffRecovery entry (the refusal plus 24 hours), once, never moved, in the
	// write that removes the grace; the replacement then watches the pull request and ends at the end of that entry
	// (third decision of 2026-11-18).
	Context("when the recovery of a pushed commit cannot read (#786)", func() {
		const (
			fail503 = "503"
			fail403 = "403"
		)
		var failing string
		recoveryEntry := func(rr *remediationv1alpha1.RemediationRequest, k remediationv1alpha1.GitWaitKind) *remediationv1alpha1.GitWait {
			for i := range rr.Status.GitWaits {
				if rr.Status.GitWaits[i].Kind == k {
					return &rr.Status.GitWaits[i]
				}
			}
			return nil
		}
		// sameBound checks that the HandoffRecovery entry w keeps the since and the end of was: the operation names the
		// read that fails last and may change.
		sameBound := func(w, was *remediationv1alpha1.GitWait) {
			Expect(w).NotTo(BeNil())
			Expect(w.Since.Time).To(BeTemporally("==", was.Since.Time))
			Expect(w.Ends.Time).To(BeTemporally("==", was.Ends.Time))
		}
		// reconcileRepl runs one reconcile of the replacement and requires it to return no error: a status write the
		// API server refuses would come back here.
		reconcileRepl := func() reconcile.Result {
			res, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(repl)})
			Expect(err).NotTo(HaveOccurred())
			return res
		}
		// pushed leaves the replacement's commit on the branch with the record of the takeover lost, and the Git host
		// failing the read of the branch head as failing says.
		pushed := func() {
			replace("512Mi", ref(hint))
			untilIntent()
			hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
				return rr.UID == repl.UID && tookOver(rr), false
			}
			pollOf(repl)
			hook = nil
			Expect(head()).NotTo(Equal(oldHead), "the commit was pushed, its record lost")
			Expect(get(repl).Status.Deadline).NotTo(BeNil())
			failing = ""
			// The recovery reads the branch head; the watch after the abort reads the pull request.
			git.SetFail(func(op string) error {
				switch {
				case op != opBranchHead && op != opGet && op != opList:
					return nil
				case failing == fail503:
					return &providers.RetryableError{Err: errors.New("GitHub branch head: status 503")}
				case failing == fail403:
					return fmt.Errorf("status 403: %w", providers.ErrForbidden)
				}
				return nil
			})
		}
		// endsAt checks that the recovery has not ended one minute before end, and has ended one minute after it, with
		// RecoveryWaitExpired, its handoff still Reserved and the lock kept.
		endsAt := func(end time.Time) {
			clock = end.Add(-time.Minute)
			reconcileRepl()
			Expect(get(repl).Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), "ended before %v", end)
			clock = end.Add(time.Minute)
			reconcileRepl()
			r := get(repl)
			Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			Expect(condition(r, remediationv1alpha1.ConditionPROpened).Reason).To(Equal(ReasonRecoveryWaitExpired))
			Expect(r.Status.Handoff.From.Phase).To(Equal(remediationv1alpha1.HandoffReserved))
			Expect(r.Status.HoldsTarget).To(HaveValue(BeTrue()))
			Expect(r.Status.GitWaits).To(BeEmpty())
		}

		It("records the grace of a 503, then aborts at a 403 with its 24 hours, removes the grace, and ends at its own end", func() {
			pushed()
			deadline := get(repl).Status.Deadline.Time
			clock = deadline.Add(time.Minute)
			failing = fail503
			reconcileRepl()
			grace := recoveryEntry(get(repl), remediationv1alpha1.GitWaitHandoffGrace)
			Expect(grace).NotTo(BeNil(), "the 503 past the deadline records the grace")
			Expect(grace.Ends.Time).To(BeTemporally("==", deadline.Add(handoffRecoveryGrace)))

			clock = clock.Add(time.Minute)
			refusedAt := clock
			failing = fail403
			reconcileRepl()
			r := get(repl)
			Expect(abortedOnRefusal(r)).To(BeTrue(), "the 403 aborts the handoff")
			Expect(get(pred).Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
			bound := recoveryEntry(r, remediationv1alpha1.GitWaitHandoffRecovery)
			Expect(bound).NotTo(BeNil(), "the 403 records the 24 hours")
			Expect(bound.Since.Time).To(BeTemporally("==", refusedAt))
			Expect(bound.Ends.Time).To(BeTemporally("==", refusedAt.Add(recoveryWaitBound)))
			Expect(recoveryEntry(r, remediationv1alpha1.GitWaitHandoffGrace)).To(BeNil(),
				"the write of the refusal removes the grace of the intent it aborts")
			bound = bound.DeepCopy()

			for range 10 {
				clock = clock.Add(2 * time.Hour)
				reconcileRepl()
				r := get(repl)
				Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
				Expect(recoveryEntry(r, remediationv1alpha1.GitWaitHandoffGrace)).To(BeNil(), "no grace comes back")
				sameBound(recoveryEntry(r, remediationv1alpha1.GitWaitHandoffRecovery), bound)
			}
			endsAt(bound.Ends.Time)
		})

		It("aborts at a 403 before the deadline with its 24 hours, keeps them through a 503 past it, and ends at its own end", func() {
			pushed()
			deadline := get(repl).Status.Deadline.Time
			refusedAt := clock
			failing = fail403
			reconcileRepl()
			r := get(repl)
			Expect(abortedOnRefusal(r)).To(BeTrue())
			bound := recoveryEntry(r, remediationv1alpha1.GitWaitHandoffRecovery)
			Expect(bound).NotTo(BeNil())
			Expect(bound.Ends.Time).To(BeTemporally("==", refusedAt.Add(recoveryWaitBound)))
			bound = bound.DeepCopy()

			clock = deadline.Add(time.Minute)
			failing = fail503
			for range 10 {
				reconcileRepl()
				r := get(repl)
				Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
				Expect(recoveryEntry(r, remediationv1alpha1.GitWaitHandoffGrace)).To(BeNil(),
					"a 503 after the abort records no grace: the intent it bounds is aborted")
				sameBound(recoveryEntry(r, remediationv1alpha1.GitWaitHandoffRecovery), bound)
				clock = clock.Add(time.Minute)
			}
			failing = fail403
			endsAt(bound.Ends.Time)
		})

		It("keeps the first refusal's 24 hours while the refusal comes and goes", func() {
			pushed()
			refusedAt := clock
			failing = fail403
			reconcileRepl()
			bound := recoveryEntry(get(repl), remediationv1alpha1.GitWaitHandoffRecovery)
			Expect(bound).NotTo(BeNil())
			Expect(bound.Since.Time).To(BeTemporally("==", refusedAt))
			bound = bound.DeepCopy()
			for i := range 12 {
				clock = clock.Add(time.Hour)
				failing = map[bool]string{true: fail503, false: fail403}[i%2 == 0]
				reconcileRepl()
				r := get(repl)
				Expect(condition(r, remediationv1alpha1.ConditionPullRequestUnreadable)).NotTo(BeNil())
				n := 0
				for _, w := range r.Status.GitWaits {
					if w.Kind == remediationv1alpha1.GitWaitHandoffRecovery {
						n++
					}
				}
				Expect(n).To(Equal(1), "the HandoffRecovery entry is recorded once")
				sameBound(recoveryEntry(r, remediationv1alpha1.GitWaitHandoffRecovery), bound)
			}
			endsAt(bound.Ends.Time)
		})

		// Decision of 2026-11-18 on #786, point 3: during and after the abort the predecessor and its replacement hold
		// the target, as the chain of a handoff does, and a third request for it waits Queued with TargetLocked. Once
		// reads answer, the predecessor stops BranchDiverged with one comment and the lock settles by the usual
		// verdict: kept by the chain while the pull request is open, released by both within settleRounds rounds once
		// a person closes it, and the third, which waited behind it, ends Superseded.
		It("keeps the target from a third request through the abort, and settles once reads answer", func() {
			const settleRounds = 3
			pushed()
			p := get(pred)
			third := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace,
				Name: fmt.Sprintf("handoff-third-%d", seq)}, Spec: *p.Spec.DeepCopy()}
			third.Spec.Replaces, third.Spec.ChainRoot = nil, ""
			Expect(k8sClient.Create(ctx, third)).To(Succeed())
			DeferCleanup(func() {
				got := &remediationv1alpha1.RemediationRequest{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(third), got); err == nil &&
					controllerutil.RemoveFinalizer(got, FinalizerName) {
					Expect(k8sClient.Update(ctx, got)).To(Succeed())
				}
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, third))).To(Succeed())
			})
			git.SetFail(func(string) error { return fmt.Errorf("status 403: %w", providers.ErrForbidden) })
			answering := false
			// round reconciles the replacement, the predecessor and the third request once each, and checks that the
			// third never holds the target while the chain does.
			round := func(step string) {
				pollOf(repl)
				pollOf(pred)
				pollOf(third)
				r, p, o := get(repl), get(pred), get(third)
				Expect(holdsLock(r) || holdsLock(p)).To(BeTrue(), "%s: the chain released the target", step)
				Expect(holdsLock(o)).To(BeFalse(), "%s: a third request took the target", step)
				Expect(o.Status.Phase).To(BeEmpty(), "%s: the third request went on", step)
				if answering {
					// While reads are refused it waits on its own read before it asks for the lock.
					q := condition(o, remediationv1alpha1.ConditionQueued)
					Expect(q).NotTo(BeNil(), "%s: the third request is not queued", step)
					Expect(q.Reason).To(Equal(ReasonTargetLocked), step)
				}
			}
			ends := clock.Add(recoveryWaitBound)
			for i := 0; !clock.After(ends.Add(time.Hour)); i++ {
				round(fmt.Sprintf("refused, step %d", i))
				clock = clock.Add(2 * time.Hour)
			}
			round("past the end")
			r := get(repl)
			Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			Expect(condition(r, remediationv1alpha1.ConditionPROpened).Reason).To(Equal(ReasonRecoveryWaitExpired))
			Expect(r.Status.Handoff.From.Phase).To(Equal(remediationv1alpha1.HandoffReserved))

			git.SetFail(nil)
			answering = true
			comments := len(git.Comments(number))
			for i := range settleRounds {
				round(fmt.Sprintf("reads answer, round %d", i))
			}
			p = get(pred)
			Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			Expect(condition(p, remediationv1alpha1.ConditionApproved).Reason).To(Equal(ReasonBranchDiverged))
			Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
			Expect(git.Comments(number)).To(HaveLen(comments+1), "one comment hands the pull request to a person")
			Expect(git.ChangeRequests()).To(ConsistOf(HaveField("State", providers.ChangeRequestOpen)))
			// Settled: nothing changes while the pull request stays open.
			for i := range settleRounds {
				round(fmt.Sprintf("settled, round %d", i))
			}
			Expect(git.Comments(number)).To(HaveLen(comments+1), "no second comment")

			// A person closes the pull request: the chain releases the target, and the third, which waited behind it,
			// ends Superseded.
			Expect(git.CloseChangeRequest(ctx, number)).To(Succeed())
			for range settleRounds {
				pollOf(repl)
				pollOf(pred)
			}
			Expect(holdsLock(get(repl))).To(BeFalse(), "the replacement keeps the target after the close")
			Expect(holdsLock(get(pred))).To(BeFalse(), "the predecessor keeps the target after the close")
			for range settleRounds {
				pollOf(third)
			}
			// The usual verdict on a request that waited behind a pull request closed without a merge.
			o := get(third)
			Expect(holdsLock(o)).To(BeFalse())
			Expect(o.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
			Expect(condition(o, remediationv1alpha1.ConditionPlanFresh).Reason).To(Equal(remediationv1alpha1.ReasonAnotherRequestClosed))
		})
	})

	// The abort at the first refusal (decision of 2026-11-18 on #786, which replaced keeping the intent standing for the
	// 24 hours): the predecessor's intent is aborted and stored before it writes anything again, so its freshness check
	// and merge gate run again, or it stops BranchDiverged with its comment when the branch carries the replacement's
	// commit. The replacement keeps the lock, writes nothing to Git, and records a merge of its commit until the end of
	// its HandoffRecovery wait; it never takes the pull request over again.
	Context("when a read of the recovery is refused (#786, the abort)", func() {
		refuseHead := func() {
			git.SetFail(func(op string) error {
				if op == opBranchHead {
					return fmt.Errorf("status 403: %w", providers.ErrForbidden)
				}
				return nil
			})
		}
		// observing checks what the replacement shows while it watches after the abort: PolicyChecked, its handoff
		// still Reserved, the lock, the HandoffRecovery entry from refusedAt and condition GitReadRetry saying so.
		observing := func(refusedAt time.Time) {
			GinkgoHelper()
			r := get(repl)
			Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
			Expect(r.Status.Handoff.From.Phase).To(Equal(remediationv1alpha1.HandoffReserved))
			Expect(r.Status.HoldsTarget).To(HaveValue(BeTrue()))
			Expect(tookOver(r)).To(BeFalse(), "the takeover is not started again")
			w := gitWait(r, waitHandoffRecovery)
			Expect(w).NotTo(BeNil())
			Expect(w.Since.Time).To(BeTemporally("==", refusedAt.Truncate(time.Second)))
			Expect(w.Ends.Time).To(BeTemporally("==", refusedAt.Truncate(time.Second).Add(recoveryWaitBound)))
			c := condition(r, remediationv1alpha1.ConditionGitReadRetry)
			Expect(c).NotTo(BeNil())
			Expect(c.Reason).To(Equal(ReasonGitReadUntilHandoffRecovery))
			Expect(c.Message).To(ContainSubstring("handoff was aborted"))
		}
		// refused makes the first read of the recovery refused, and checks that the predecessor's intent is aborted by
		// the replacement's reconcile, before the predecessor runs again.
		refused := func() time.Time {
			GinkgoHelper()
			refuseHead()
			at := clock
			res, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(repl)})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(condition(get(repl), remediationv1alpha1.ConditionPullRequestUnreadable).Reason).To(Equal(ReasonGitForbidden))
			p := get(pred)
			Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted), "the intent is aborted at the refusal")
			Expect(p.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintDeclined))
			observing(at)
			return at
		}
		pushedLost := func() {
			replace("512Mi", ref(hint))
			untilIntent()
			hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
				return rr.UID == repl.UID && tookOver(rr), false
			}
			pollOf(repl)
			hook = nil
			Expect(head()).NotTo(Equal(oldHead), "the commit was pushed, its record lost")
		}

		It("lets the predecessor's freshness check fail its check when the plan went stale, before any push", func() {
			// The predecessor's check passes on an approval by someone who wrote no hint.
			git.AddReview(number, providers.Feedback{ID: 9101, Author: providers.FeedbackAuthor{ID: 2002,
				Login: "reviewer-two", Kind: providers.AuthorPerson}, ReviewState: "APPROVED", CommitID: oldHead,
				CreatedAt: clock})
			clock = clock.Add(time.Second)
			pollOf(pred)
			status, found, err := git.ReviewCheckRun(ctx, oldHead)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(status).To(Equal(providers.CheckRunSuccess), "the check passes before the handoff")

			replace("512Mi", ref(hint))
			untilIntent()
			Expect(head()).To(Equal(oldHead), "reserved, not pushed")
			refused()

			// The policy changes while the replacement cannot read: the predecessor's plan is stale now.
			pol := &remediationv1alpha1.RemediationPolicy{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), pol)).To(Succeed())
			pol.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 90
			Expect(k8sClient.Update(ctx, pol)).To(Succeed())
			git.SetFail(nil)
			for range 3 {
				pollOf(pred)
			}
			status, found, err = git.ReviewCheckRun(ctx, oldHead)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(status).To(Equal(providers.CheckRunFailure), "the check of a stale plan does not stay passed")
			Expect(head()).To(Equal(oldHead), "nothing was pushed")
		})

		It("lets the predecessor go on when nothing was pushed, and neither request writes the branch", func() {
			replace("512Mi", ref(hint))
			untilIntent()
			refusedAt := refused()
			git.SetFail(nil)
			comments := len(git.Comments(number))
			for range 4 {
				clock = clock.Add(time.Hour)
				drive(1)
			}
			p := get(pred)
			Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "no BranchDiverged without a push")
			Expect(intent(p)).To(BeNil())
			observing(refusedAt)
			Expect(condition(get(repl), remediationv1alpha1.ConditionPullRequestUnreadable)).To(BeNil(),
				"a read that works removes the condition")
			Expect(head()).To(Equal(oldHead), "neither request wrote the branch")
			// The only comment is the reply to the hint that the new plan did not take over; no BranchDiverged one.
			Expect(git.Comments(number)[comments:]).To(HaveEach(ContainSubstring("shadewright hint-reply")))
			Expect(len(git.Comments(number)) - comments).To(BeNumerically("<=", 1))
		})

		It("stops the predecessor BranchDiverged with one comment when the commit was pushed, and records a later merge", func() {
			pushedLost()
			pushed := head()
			refusedAt := refused()
			git.SetFail(nil)
			comments := len(git.Comments(number))
			for range 3 {
				pollOf(pred)
			}
			p := get(pred)
			Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			Expect(condition(p, remediationv1alpha1.ConditionApproved).Reason).To(Equal(ReasonBranchDiverged))
			Expect(git.Comments(number)).To(HaveLen(comments+1), "one comment hands the pull request to a person")
			for range 3 {
				clock = clock.Add(5 * time.Hour)
				drive(1)
			}
			observing(refusedAt)
			Expect(head()).To(Equal(pushed), "neither request wrote the branch after the abort")
			Expect(git.Comments(number)).To(HaveLen(comments+1), "no second comment")

			// A person merges the pull request 20 hours after the refusal: the replacement records it.
			_, err := git.MergeWithCommit(number)
			Expect(err).NotTo(HaveOccurred())
			pollUntil(func(rr *remediationv1alpha1.RemediationRequest) bool { return rr.Status.MergeReceipt != nil })
			r := get(repl)
			Expect(tookOver(r)).To(BeTrue())
			Expect(r.Status.PullRequest.HeadSHA).To(Equal(pushed))
		})

		It("does not take over again after a refusal that passed, and says why", func() {
			pushedLost()
			refusedAt := refused()
			git.SetFail(nil)
			for range 5 {
				clock = clock.Add(time.Minute)
				pollOf(repl)
			}
			observing(refusedAt)
			r := get(repl)
			Expect(condition(r, remediationv1alpha1.ConditionPullRequestUnreadable)).To(BeNil())
			Expect(r.Status.PullRequest).To(BeNil())
			Expect(git.ChangeRequests()).To(ConsistOf(HaveField("State", providers.ChangeRequestOpen)))
		})

		It("ends at the end of the HandoffRecovery wait with its span, keeping the lock", func() {
			pushedLost()
			refusedAt := refused()
			clock = refusedAt.Add(recoveryWaitBound + time.Minute)
			pollOf(repl)
			r := get(repl)
			Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			c := condition(r, remediationv1alpha1.ConditionPROpened)
			Expect(c.Reason).To(Equal(ReasonRecoveryWaitExpired))
			Expect(c.Message).To(ContainSubstring(recoveryWaitBound.String()))
			Expect(r.Status.HoldsTarget).To(HaveValue(BeTrue()))
		})
	})

	It("aborts on a foreign push during the handoff, and the predecessor stops as before", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		_, err := git.Push(branch(), "apps/orders/extra.yaml", []byte("x: 1\n"))
		Expect(err).NotTo(HaveOccurred())
		drive(3)
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
		Expect(condition(r, remediationv1alpha1.ConditionPROpened).Reason).To(Equal(remediationv1alpha1.ReasonHandoffAborted))
		p := get(pred)
		Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
		Expect(p.Status.Hints[0].State).To(Equal(remediationv1alpha1.HintDeclined))
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
	})

	It("waits at its recorded step while writes are paused", func() {
		replace("512Mi", ref(hint))
		pollUntil(func(rr *remediationv1alpha1.RemediationRequest) bool { return reserved(rr) })
		pause(true)
		drive(3)
		Expect(get(pred).Status.Handoff).To(BeNil(), "no intent is written while paused")
		Expect(get(repl).Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
		Expect(head()).To(Equal(oldHead))
		pause(false)
		pollUntil(tookOver)
		Expect(head()).NotTo(Equal(oldHead))
	})

	It("names the branch after the chain root", func() {
		replace("512Mi", ref(hint))
		Expect(freshness.BranchName(get(repl))).To(Equal(branch()))
		Expect(freshness.BranchName(get(pred))).To(Equal(branch()))
	})

	// remove deletes rr at once, its finalizer included.
	remove := func(rr *remediationv1alpha1.RemediationRequest) {
		got := get(rr)
		base := got.DeepCopy()
		controllerutil.RemoveFinalizer(got, FinalizerName)
		Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
		Expect(k8sClient.Delete(ctx, got)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(rr), &remediationv1alpha1.RemediationRequest{}))
		}).Should(BeTrue())
	}
	// secondHint stores a second Pending hint on the predecessor, after the cooldown of the first.
	secondHint := func() remediationv1alpha1.Hint {
		clock = clock.Add(time.Minute)
		git.AddComment(number, providers.Feedback{ID: 7002,
			Author: providers.FeedbackAuthor{ID: writerID, Login: "reviewer-one", Kind: providers.AuthorPerson},
			Body:   "/shadewright reanalyze: try a little less", CreatedAt: clock})
		pollOf(pred)
		h := findHint(get(pred).Status.Hints, remediationv1alpha1.HintSourceGitHub, 7002)
		Expect(h).NotTo(BeNil())
		Expect(h.State).To(Equal(remediationv1alpha1.HintPending))
		return *h
	}
	prState := func() providers.ChangeRequestState {
		crs := git.ChangeRequests()
		Expect(crs).To(HaveLen(1))
		return crs[0].State
	}

	It("keeps recognising the pull request when the Git host refuses its rewrite", func() {
		git.SetFail(func(op string) error {
			if op == opUpdateChangeRequest || op == opCreateCheckRun {
				return fmt.Errorf("status 403: %w", providers.ErrForbidden)
			}
			return nil
		})
		replace("512Mi", ref(hint))
		pollUntil(tookOver)
		pollOf(repl)
		git.SetFail(nil)
		r := get(repl)
		Expect(r.Status.Handoff.From.PullRequestUpdate).To(Equal(remediationv1alpha1.HandoffStepRefused))
		Expect(r.Status.PullRequest.ReviewCheck.State).To(Equal(remediationv1alpha1.ReviewCheckRefused))
		Expect(condition(r, remediationv1alpha1.ConditionApproved).Reason).To(Equal(ReasonReviewCheckRefused))
		Expect(git.CheckRuns(r.Status.PullRequest.HeadSHA)).To(BeEmpty())
		v, _, err := reconciler().lookHeld(ctx, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(v.state).To(Equal(changeOpen), "the body still carries the predecessor's marker, which stays accepted")
		Expect(v.pr.Number).To(Equal(number))
	})

	It("owns a merge of its commit after a restart that lost the record of the handoff", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
			return rr.UID == repl.UID && tookOver(rr), false
		}
		pollOf(repl)
		hook = nil
		updated := head()
		Expect(updated).NotTo(Equal(oldHead))
		_, err := git.Merge(number)
		Expect(err).NotTo(HaveOccurred())
		drive(3)
		Expect(get(repl).Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		p := get(pred)
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(condition(p, remediationv1alpha1.ConditionApproved).Reason).To(Equal(remediationv1alpha1.ReasonHandedOff))
	})

	It("recovers its own commit after the target file moved on the target branch", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		hook = func(rr *remediationv1alpha1.RemediationRequest) (bool, bool) {
			return rr.UID == repl.UID && tookOver(rr), false
		}
		pollOf(repl)
		hook = nil
		updated := head()
		Expect(updated).NotTo(Equal(oldHead))
		_, err := git.Push(mainBranch, sourcePath, append([]byte("# owned by the orders team\n"),
			deploymentManifest(deployment.Name, deployment.Namespace, workerContainer, "256Mi")...))
		Expect(err).NotTo(HaveOccurred())
		pollUntil(tookOver)
		Expect(head()).To(Equal(updated))
	})

	It("never closes the pull request from the predecessor's finalizer once the replacement holds it", func() {
		replace("512Mi", ref(hint))
		pollUntil(tookOver)
		Expect(k8sClient.Delete(ctx, get(pred))).To(Succeed())
		pollOf(pred)
		pollOf(pred)
		Expect(prState()).To(Equal(providers.ChangeRequestOpen))
		pollOf(repl)
		Expect(get(repl).Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(prState()).To(Equal(providers.ChangeRequestOpen))
	})

	It("never closes the pull request for the predecessor's stale plan while its intent stands", func() {
		replace("512Mi", ref(hint))
		pollUntil(tookOver)
		pol := &remediationv1alpha1.RemediationPolicy{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), pol)).To(Succeed())
		pol.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 150
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())
		clock = clock.Add(time.Hour)
		pollOf(pred)
		pollOf(pred)
		Expect(prState()).To(Equal(providers.ChangeRequestOpen))
	})

	It("never takes over the pull request of a stale plan whose close the Git host refuses (#199)", func() {
		git.SetFail(func(op string) error {
			if op == "CloseChangeRequest" {
				return fmt.Errorf("status 403: %w", providers.ErrForbidden)
			}
			return nil
		})
		pol := &remediationv1alpha1.RemediationPolicy{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), pol)).To(Succeed())
		pol.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 150
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())
		pollOf(pred)
		p := get(pred)
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(meta.IsStatusConditionTrue(p.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
			To(BeTrue())
		replace("512Mi", ref(hint))
		drive(5)
		Expect(get(repl).Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
		Expect(head()).To(Equal(oldHead), "nothing is pushed to the stale plan's pull request")
		p = get(pred)
		Expect(p.Status.Handoff == nil || p.Status.Handoff.To == nil).To(BeTrue(), "no intent recorded")
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
	})

	It("takes a person's one-commit push to the target file for a foreign push", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		_, err := git.Push(branch(), sourcePath,
			deploymentManifest(deployment.Name, deployment.Namespace, workerContainer, "384Mi"))
		Expect(err).NotTo(HaveOccurred())
		pollOf(pred)
		p := get(pred)
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
		drive(2)
		Expect(get(repl).Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
	})

	It("takes the pull request over for a later hint after an earlier handoff aborted", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		remove(repl)
		pollOf(pred)
		p := get(pred)
		Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		second := secondHint()
		replace("384Mi", ref(second))
		pollUntil(tookOver)
		Expect(fileAt(head())).To(ContainSubstring("384Mi"))
		pollOf(repl)
		p = get(pred)
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(p.Status.Handoff.To.UID).To(Equal(remediationv1alpha1.UID(repl.UID)))
	})

	It("carries a second Pending hint over to the replacement instead of losing it", func() {
		second := secondHint()
		replace("512Mi", ref(hint))
		pollUntil(tookOver)
		pollOf(repl)
		Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		h := findHint(get(repl).Status.Hints, second.Source, second.CommentID)
		Expect(h).NotTo(BeNil(), "the replacement reads comments on from a cursor past the second hint")
		Expect(h.State).To(Equal(remediationv1alpha1.HintPending))
	})

	It("ends the predecessor although the cache has not seen its intent yet", func() {
		replace("512Mi", ref(hint))
		before := get(pred)
		pollUntil(tookOver)
		stale = before
		pollOf(repl)
		stale = nil
		Expect(get(pred).Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
	})

	It("keeps a lock while another pull request of the branch may merge the replacement's commit, and reads the recorded one after the update (#688)", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		git.SetAfter(func(op string) error {
			if op == opAppend {
				if err := git.CloseChangeRequest(ctx, number); err != nil {
					return err
				}
				_, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch(),
					BaseBranch: mainBranch, Title: "someone else's", Body: "reopened by hand"})
				return err
			}
			return nil
		})
		pollOf(repl)
		git.SetAfter(nil)
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
		c := condition(r, remediationv1alpha1.ConditionPROpened)
		Expect(c.Reason).To(Equal(remediationv1alpha1.ReasonHandoffAborted))
		Expect(c.Message).To(ContainSubstring("was closed around the handoff"), "the recorded pull request was read")
		drive(3)
		Expect(holdsLock(get(pred)) || holdsLock(get(repl))).To(BeTrue(),
			"an open pull request carries the replacement's commit")
	})

	It("reads the recorded pull request again after a 404 right after the update, and takes it over (#688)", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		appended, missed := false, false
		git.SetAfter(func(op string) error {
			if op == opAppend {
				appended = true
			}
			return nil
		})
		git.SetFail(func(op string) error {
			if op == opGet && appended && !missed {
				missed = true
				return fmt.Errorf("pull request: %w", providers.ErrNotFound)
			}
			return nil
		})
		pollOf(repl)
		git.SetAfter(nil)
		git.SetFail(nil)
		Expect(missed).To(BeTrue())
		Expect(get(repl).Status.Phase).NotTo(Equal(remediationv1alpha1.PhaseCancelled), "a 404 alone aborts nothing")
		updated := head()
		Expect(updated).NotTo(Equal(oldHead))
		pollUntil(tookOver)
		Expect(head()).To(Equal(updated), "no second commit")
		Expect(get(repl).Status.PullRequest.Number).To(Equal(number))
	})

	It("only requeues the predecessor whose intent stands on a 404 its branch does not confirm (#688)", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		git.SetFail(func(op string) error {
			if op == opGet {
				return fmt.Errorf("pull request: %w", providers.ErrNotFound)
			}
			return nil
		})
		pollOf(pred)
		pollOf(pred)
		git.SetFail(nil)
		p := get(pred)
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "the 404 is a failed read")
		Expect(intent(p)).NotTo(BeNil(), "the intent still stands")
		Expect(get(repl).Status.Phase).NotTo(Equal(remediationv1alpha1.PhaseCancelled))
		pollUntil(tookOver)
		Expect(get(repl).Status.PullRequest.Number).To(Equal(number))
	})

	It("reads the recorded pull request again after an answer about another pull request around the update, and takes it over (#688)", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		appended, missedBefore, missedAfter := false, false, false
		git.SetAfter(func(op string) error {
			if op == opAppend {
				appended = true
			}
			return nil
		})
		git.SetFail(func(op string) error {
			switch {
			case op != opGet:
			case !appended && !missedBefore:
				missedBefore = true
				return fmt.Errorf("another pull request: %w", providers.ErrUntrustedAnswer)
			case appended && !missedAfter:
				missedAfter = true
				return fmt.Errorf("another pull request: %w", providers.ErrUntrustedAnswer)
			}
			return nil
		})
		pollOf(repl)
		Expect(missedBefore).To(BeTrue())
		Expect(get(repl).Status.Phase).NotTo(Equal(remediationv1alpha1.PhaseCancelled),
			"an answer about another pull request says nothing about the recorded one")
		Expect(head()).To(Equal(oldHead), "nothing was pushed on it")
		pollOf(repl)
		pollOf(repl)
		git.SetAfter(nil)
		git.SetFail(nil)
		Expect(missedAfter).To(BeTrue())
		Expect(get(repl).Status.Phase).NotTo(Equal(remediationv1alpha1.PhaseCancelled), "not after the update either")
		pollUntil(tookOver)
		Expect(get(repl).Status.PullRequest.Number).To(Equal(number))
	})

	It("reads the recorded pull request again after a 404 before the update, and takes it over (#688)", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		missed := false
		git.SetFail(func(op string) error {
			if op == opGet && !missed {
				missed = true
				return fmt.Errorf("pull request: %w", providers.ErrNotFound)
			}
			return nil
		})
		pollOf(repl)
		git.SetFail(nil)
		Expect(missed).To(BeTrue())
		Expect(get(repl).Status.Phase).NotTo(Equal(remediationv1alpha1.PhaseCancelled), "a 404 alone aborts nothing")
		Expect(head()).To(Equal(oldHead), "nothing was pushed on a 404")
		pollUntil(tookOver)
		Expect(get(repl).Status.PullRequest.Number).To(Equal(number))
	})

	It("reads the recorded pull request before the update, not a newer one of the branch (#688)", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		done := false
		git.SetFail(func(op string) error {
			if (op == opGet || op == opFind) && !done {
				done = true
				if err := git.CloseChangeRequest(ctx, number); err != nil {
					return err
				}
				_, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch(),
					BaseBranch: mainBranch, Title: "newer", Body: "opened by hand"})
				return err
			}
			return nil
		})
		pollOf(repl)
		git.SetFail(nil)
		Expect(done).To(BeTrue())
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
		c := condition(r, remediationv1alpha1.ConditionPROpened)
		Expect(c.Reason).To(Equal(remediationv1alpha1.ReasonHandoffAborted))
		Expect(c.Message).To(ContainSubstring("was closed before the handoff"), "the recorded pull request was read")
		Expect(head()).To(Equal(oldHead), "no update of a closed pull request")
	})

	It("lets one of two replacements racing for one hint take the pull request over", func() {
		replace("512Mi", ref(hint))
		first := repl
		replace("384Mi", ref(hint))
		second := repl
		for range 6 {
			pollOf(first)
			pollOf(second)
			pollOf(pred)
		}
		took := 0
		for _, rr := range []*remediationv1alpha1.RemediationRequest{first, second} {
			if tookOver(get(rr)) {
				took++
			}
		}
		Expect(took).To(Equal(1))
		parents, err := git.Parents(ctx, head())
		Expect(err).NotTo(HaveOccurred())
		Expect(parents).To(Equal([]string{oldHead}), "one commit on top of the old head")
		Expect(git.ChangeRequests()).To(HaveLen(1))
		remove(first)
	})

	It("aborts the handoff when the pull request is closed right before the update", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		closed := false
		git.SetFail(func(op string) error {
			if op == opGet && !closed {
				closed = true
				return git.CloseChangeRequest(ctx, number)
			}
			return nil
		})
		pollOf(repl)
		git.SetFail(nil)
		Expect(closed).To(BeTrue())
		drive(2)
		r := get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
		Expect(condition(r, remediationv1alpha1.ConditionPROpened).Reason).To(Equal(remediationv1alpha1.ReasonHandoffAborted))
		Expect(head()).To(Equal(oldHead), "no update of a closed pull request")
		p := get(pred)
		Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
		Expect(p.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(condition(p, remediationv1alpha1.ConditionPROpened).Reason).To(Equal(ReasonChangeRequestClosed))
	})

	It("aborts the handoff, naming the render, when the new plan's render never finished three times (#714)", func() {
		replace("512Mi", ref(hint))
		untilIntent()
		// Three renders of the replacement's plan that took other executor processes down with them.
		r := get(repl)
		gone := newProcessID()
		for n := int32(1); n <= remediationv1alpha1.MaxUnfinishedRenders; n++ {
			r.Status.UnfinishedRenders = &remediationv1alpha1.UnfinishedRenders{PlanDigest: r.Status.PlanDigest,
				Count: n, Process: gone}
			Expect(k8sClient.Status().Update(ctx, r)).To(Succeed())
		}
		drive(2)
		r = get(repl)
		Expect(r.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
		c := condition(r, remediationv1alpha1.ConditionPROpened)
		Expect(c.Reason).To(Equal(remediationv1alpha1.ReasonHandoffAborted))
		Expect(c.Message).To(ContainSubstring("The render check of this plan started 3 times in a row and never finished"))
		Expect(c.Message).To(ContainSubstring("(" + ReasonRenderNeverFinished + ")"))
		Expect(head()).To(Equal(oldHead), "no update of the branch without a render")
		p := get(pred)
		Expect(p.Status.Handoff.To.Phase).To(Equal(remediationv1alpha1.HandoffAborted))
	})

	Context("the merge gate", func() {
		const otherID = int64(2002)
		review := func(id, author int64, state, commit string) {
			git.AddReview(number, providers.Feedback{ID: id, Author: providers.FeedbackAuthor{ID: author,
				Login: fmt.Sprintf("reviewer-%d", author), Kind: providers.AuthorPerson},
				ReviewState: state, CommitID: commit, CreatedAt: clock})
			clock = clock.Add(time.Second)
		}
		var newHead string
		// waiting takes the pull request over and brings the replacement to AwaitingApproval with its check run
		// in progress.
		waiting := func() {
			replace("512Mi", ref(hint))
			pollUntil(tookOver)
			pollUntil(func(rr *remediationv1alpha1.RemediationRequest) bool {
				return rr.Status.Phase == remediationv1alpha1.PhaseAwaitingApproval
			})
			newHead = get(repl).Status.PullRequest.HeadSHA
			Expect(get(repl).Status.PullRequest.ReviewCheck.State).To(Equal(remediationv1alpha1.ReviewCheckInProgress))
		}
		gateState := func() remediationv1alpha1.ReviewCheckState { return get(repl).Status.PullRequest.ReviewCheck.State }

		It("completes the check run once someone who wrote no hint approves the head", func() {
			waiting()
			review(9001, otherID, "COMMENTED", newHead)
			pollOf(repl)
			Expect(gateState()).To(Equal(remediationv1alpha1.ReviewCheckInProgress))
			review(9002, otherID, "APPROVED", newHead)
			pollOf(repl)
			Expect(gateState()).To(Equal(remediationv1alpha1.ReviewCheckPassed))
			runs := git.CheckRuns(newHead)
			Expect(runs).To(HaveLen(2))
			Expect(runs[1].Status).To(Equal(providers.CheckRunSuccess))
			pollOf(repl)
			Expect(git.CheckRuns(newHead)).To(HaveLen(2), "completed once")
		})

		It("never counts an approval of an earlier head", func() {
			waiting()
			review(9001, otherID, "APPROVED", oldHead)
			pollOf(repl)
			Expect(gateState()).To(Equal(remediationv1alpha1.ReviewCheckInProgress))
			Expect(git.CheckRuns(newHead)).To(HaveLen(1))
		})

		It("never counts a hint author's approval, and says why once per review", func() {
			waiting()
			review(9001, writerID, "APPROVED", newHead)
			pollOf(repl)
			pollOf(repl)
			Expect(gateState()).To(Equal(remediationv1alpha1.ReviewCheckInProgress))
			Expect(git.CheckRuns(newHead)).To(HaveLen(1))
			said := 0
			for _, c := range git.Comments(number) {
				if strings.Contains(c, "review-gate review=9001") {
					said++
				}
			}
			Expect(said).To(Equal(1))
		})

		It("counts the latest review of each person: a later request for changes withdraws an approval", func() {
			waiting()
			review(9001, otherID, "APPROVED", newHead)
			review(9002, otherID, "CHANGES_REQUESTED", newHead)
			pollOf(repl)
			Expect(gateState()).To(Equal(remediationv1alpha1.ReviewCheckInProgress))
		})

		It("records a refused completion without counting it as passed", func() {
			waiting()
			review(9001, otherID, "APPROVED", newHead)
			git.SetFail(func(op string) error {
				if op == opCreateCheckRun {
					return fmt.Errorf("status 403: %w", providers.ErrForbidden)
				}
				return nil
			})
			pollOf(repl)
			git.SetFail(nil)
			Expect(gateState()).To(Equal(remediationv1alpha1.ReviewCheckRefused))
		})
	})
})

// staleRead is a client whose reads of obj's key return obj, as a cache that has not seen the later writes.
type staleRead struct {
	client.Client
	obj *remediationv1alpha1.RemediationRequest
}

func (c *staleRead) Get(ctx context.Context, key client.ObjectKey, out client.Object, opts ...client.GetOption) error {
	if rr, ok := out.(*remediationv1alpha1.RemediationRequest); ok && key == client.ObjectKeyFromObject(c.obj) {
		c.obj.DeepCopyInto(rr)
		return nil
	}
	return c.Client.Get(ctx, key, out, opts...)
}
