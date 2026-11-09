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
		// stale, when set, is what the reconciler's client reads for the predecessor, as a cache behind the API.
		stale *remediationv1alpha1.RemediationRequest
		// replacements counts the replacements of the spec, so each has its own name.
		replacements int
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
		hook, stale, repl = nil, nil, nil
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
		cr, err := reconciler().findOwnChangeRequest(ctx, r)
		Expect(err).NotTo(HaveOccurred(), "the body still carries the predecessor's marker, which stays accepted")
		Expect(cr).NotTo(BeNil())
		Expect(cr.Number).To(Equal(number))
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

	It("keeps a lock while another pull request of the branch may merge the replacement's commit", func() {
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
		drive(3)
		Expect(holdsLock(get(pred)) || holdsLock(get(repl))).To(BeTrue(),
			"an open pull request carries the replacement's commit")
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
			if op == opFind && !closed {
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
