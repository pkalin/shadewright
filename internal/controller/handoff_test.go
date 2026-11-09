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

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
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
			ReviewerHints: true,
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
		p := get(pred)
		repl = &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace,
			Name: fmt.Sprintf("handoff-repl-%d", seq)}, Spec: *p.Spec.DeepCopy()}
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
		hook, repl = nil, nil
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
		pollUntil(func(rr *remediationv1alpha1.RemediationRequest) bool { return tookOver(rr) })
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
		Expect(r.Status.Handoff.From.PullRequestUpdated).To(BeTrue())
		Expect(r.Status.Handoff.From.CheckRunCreated).To(BeTrue())
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
			if op == opFind && !merged {
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
})
