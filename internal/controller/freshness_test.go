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
	"sync/atomic"
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
	crevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	appLabel        = "app"
	appName         = "web"
	workerContainer = "worker"
	mainBranch      = "main"
	opClose         = "CloseChangeRequest"
	// freshOtherBranch is a target branch the settings name after the plan was made for mainBranch.
	freshOtherBranch = "release"
	opBranchHead     = "BranchHead"
	workerImage      = "example.invalid/worker:1"
	appsGlob         = "apps/**/*.yaml"
)

var _ = Describe("RemediationRequest plan freshness", func() {
	ctx := context.Background()
	var (
		seq        int
		deployment *appsv1.Deployment
		policy     *remediationv1alpha1.RemediationPolicy
		settings   *remediationv1alpha1.RemediationSettings
		request    *remediationv1alpha1.RemediationRequest
		git        *fakegit.Git
		// byURL holds one fake repository per URL, so a call can only reach the repository it names.
		byURL  map[string]*fakegit.Git
		closes atomic.Int32
		cr     providers.ChangeRequest
	)

	manifest := func(limit string) []byte {
		return deploymentManifest(deployment.Name, deployment.Namespace, workerContainer, limit)
	}
	reconciler := func() *RemediationRequestReconciler {
		return &RemediationRequestReconciler{
			Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Source: sourceFunc(func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
				// Like the real resolver, report the head of the branch as it is now.
				res := resolved()
				res.Source.BaseCommit, _ = git.BranchHead(ctx, mainBranch)
				return res, nil
			}),
			OpenRepository: func(_ context.Context, repo remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
				g, ok := byURL[repo.URL]
				if !ok {
					return nil, errors.New("unknown repository")
				}
				return g, nil
			},
		}
	}
	reconcileOnce := func() (reconcile.Result, error) {
		return reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
	}
	fetch := func() *remediationv1alpha1.RemediationRequest {
		got := &remediationv1alpha1.RemediationRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), got)).To(Succeed())
		return got
	}

	// BeforeEach builds the cluster, runs the policy check and opens the pull request, so each spec starts
	// from an approved plan with an open change request.
	BeforeEach(func() {
		seq++
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fx.TargetNamespace}})
		Expect(client.IgnoreAlreadyExists(err)).To(Succeed())

		deployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: fx.TargetNamespace, Name: fmt.Sprintf("fresh-%d", seq)},
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
		policy = fx.RemediationPolicy(fmt.Sprintf("fresh-policy-%d", seq))
		policy.Spec.Actions = []remediationv1alpha1.PolicyAction{fx.MemoryLimitPolicyAction()}
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 100
		policy.Spec.RequestNamespaces = []remediationv1alpha1.Name{requestNamespace}
		policy.Spec.GitOps.AllowedPaths = []remediationv1alpha1.GitPathPattern{appsGlob}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		settings = fx.RemediationSettings()
		settings.Spec.Repositories[0].TargetBranch = mainBranch
		Expect(k8sClient.Create(ctx, settings)).To(Succeed())
		request = fx.MemoryLimitRequest(requestNamespace, fmt.Sprintf("fresh-rr-%d", seq))
		request.Spec.PolicyRef = remediationv1alpha1.Name(policy.Name)
		request.Spec.TargetRef.Name = deployment.Name
		request.Spec.TargetRef.UID = remediationv1alpha1.UID(deployment.UID)
		Expect(k8sClient.Create(ctx, request)).To(Succeed())

		git = fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: manifest("256Mi"), buildKustomize: buildFile, "README.yaml": []byte("a: 1\n")})
		byURL = map[string]*fakegit.Git{settings.Spec.Repositories[0].URL: git}
		closes.Store(0)
		git.SetFail(func(op string) error {
			if op == opClose {
				closes.Add(1)
			}
			return nil
		})

		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))

		// The state machine's part, played by hand: branch, commit, pull request.
		branch := freshness.BranchName(got)
		head, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		Expect(git.CreateBranch(ctx, branch, head)).To(Succeed())
		_, err = git.CommitFile(ctx, branch, head, providers.FileChange{Path: sourcePath, Content: manifest("384Mi")})
		Expect(err).NotTo(HaveOccurred())
		cr, err = git.OpenChangeRequest(ctx, providers.NewChangeRequest{
			HeadBranch: branch, BaseBranch: mainBranch, Title: "t", Body: pullRequestBody(got, "", nil),
		})
		Expect(err).NotTo(HaveOccurred())
		got.Status.Phase = remediationv1alpha1.PhaseAwaitingApproval
		got.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{
			Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA,
		}
		// As recordPullRequest writes it.
		meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
			Type: remediationv1alpha1.ConditionPROpened, Status: metav1.ConditionTrue,
			Reason: ReasonPullRequestOpened, Message: fmt.Sprintf("Pull request #%d is open", cr.Number),
		})
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
	})
	AfterEach(func() {
		for _, o := range []client.Object{request, policy, settings, deployment} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName},
				&remediationv1alpha1.RemediationSettings{}))
		}).Should(BeTrue())
	})

	changeRequest := func() providers.ChangeRequest {
		got, err := git.FindChangeRequest(ctx, freshness.BranchName(fetch()))
		Expect(err).NotTo(HaveOccurred())
		return got
	}

	It("records the plan digest and the repository it covers with the decision", func() {
		got := fetch()
		Expect(got.Status.PlanRepository).To(Equal(&remediationv1alpha1.PlanRepository{
			URL: settings.Spec.Repositories[0].URL, InstallationID: 1}))
		Expect(got.Status.PlanDigest).To(MatchRegexp(`^sha256:[0-9a-f]{64}$`))
		want, err := freshness.ComputeDigest(got, got.Status.Source, policy, settings)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status.PlanDigest).To(Equal(want))
	})

	It("leaves a fresh plan alone, also after a merge that does not touch the file", func() {
		moved, err := git.Push(mainBranch, "README.yaml", []byte("a: 2\n"))
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		// The moved head is rendered once and recorded; the plan itself is left as it is.
		before := fetch()
		Expect(before.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(before.Status.PullRequest.RenderedTargetHead).To(Equal(moved))
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().ResourceVersion).To(Equal(before.ResourceVersion))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		Expect(closes.Load()).To(BeZero())
		Expect(reconciler().freshErr(ctx, fetch())).To(Succeed())
	})

	// closedForStalePlan checks that PullRequestOpened of a superseded request says its pull request is closed
	// and why, with its number.
	closedForStalePlan := func(got *remediationv1alpha1.RemediationRequest, reason string) {
		GinkgoHelper()
		opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(opened).NotTo(BeNil())
		Expect(opened.Status).To(Equal(metav1.ConditionTrue), "the pull request was opened")
		Expect(opened.Reason).To(Equal(ReasonPullRequestClosed))
		Expect(opened.Message).To(Equal(fmt.Sprintf("Pull request #%d is closed: the plan is stale (%s)",
			got.Status.PullRequest.Number, reason)))
	}

	// stillOpen checks that PullRequestOpened of a request that stopped with its pull request left open still
	// says the pull request is open.
	stillOpen := func() {
		GinkgoHelper()
		opened := meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(opened).NotTo(BeNil())
		Expect(opened.Status).To(Equal(metav1.ConditionTrue))
		Expect(opened.Reason).To(Equal(ReasonPullRequestOpened), "the pull request was not closed")
		Expect(opened.Message).To(Equal(fmt.Sprintf("Pull request #%d is open", cr.Number)))
	}

	supersedes := func(reason string) {
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(reason))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
		closedForStalePlan(got, reason)
	}

	It("closes the change request once when the policy is edited, and not again", func() {
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		supersedes(freshness.ReasonPolicyChanged)
		Expect(closes.Load()).To(Equal(int32(1)))

		// A repeated reconcile, and a repeated freshness check, find the request decided and call nothing.
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(reconciler().freshErr(ctx, fetch())).To(MatchError(ErrSuperseded))
		Expect(closes.Load()).To(Equal(int32(1)))
	})

	// fluxWaitCondition returns the PlanFresh condition of the request as stored.
	fluxWaitCondition := func() *metav1.Condition {
		return meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	}

	// Flux reconciling the delivery stops the freshness check without a verdict; the retries are bounded by
	// TargetStateTimeout also once the pull request is open, when no deadline bounds them (#556 review). Then
	// the plan is taken as stale, as it was at once before #556.
	It("bounds the retries while Flux reconciles, then supersedes", func() {
		start := time.Now()
		reconcileAt := func(now time.Time) (reconcile.Result, error) {
			r := reconciler()
			r.Now = func() time.Time { return now }
			r.Source = sourceFunc(func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
				return sourcemap.Result{Outcome: sourcemap.NeedsIntervention,
					Reason: sourcemap.ReasonKustomizationReconciling, Message: ksNotReadyMessage}, nil
			})
			return r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		}
		for _, after := range []time.Duration{0, time.Minute, DefaultTargetStateTimeout - time.Second} {
			_, err := reconcileAt(start.Add(after))
			Expect(err).To(MatchError(freshness.ErrReconciling))
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			c := fluxWaitCondition()
			Expect(c).NotTo(BeNil())
			Expect(c.Status).To(Equal(metav1.ConditionUnknown))
			Expect(c.Reason).To(Equal(string(sourcemap.ReasonKustomizationReconciling)))
			Expect(c.LastTransitionTime.Time).To(BeTemporally("~", start, time.Second))
		}
		Expect(closes.Load()).To(BeZero())
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))

		_, err := reconcileAt(start.Add(DefaultTargetStateTimeout))
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(fluxWaitCondition().Reason).To(Equal(freshness.ReasonSourceChanged))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
	})

	It("clears the wait for Flux once the freshness check passes", func() {
		r := reconciler()
		r.Source = sourceFunc(func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
			return sourcemap.Result{Outcome: sourcemap.NeedsIntervention, Reason: sourcemap.ReasonKustomizationReconciling}, nil
		})
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).To(MatchError(freshness.ErrReconciling))
		Expect(fluxWaitCondition()).NotTo(BeNil())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fluxWaitCondition()).To(BeNil())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
	})

	It("supersedes when the policy is deleted", func() {
		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		supersedes(freshness.ReasonPolicyChanged)
		Expect(closes.Load()).To(Equal(int32(1)))
	})

	It("supersedes when the target file changed on the target branch", func() {
		_, err := git.Push(mainBranch, sourcePath, manifest("512Mi"))
		Expect(err).NotTo(HaveOccurred())
		supersedes(freshness.ReasonTargetFileChanged)
		Expect(closes.Load()).To(Equal(int32(1)))
	})

	It("never rebuilds or force-pushes the branch of a superseded request", func() {
		branch := freshness.BranchName(fetch())
		headBefore, err := git.BranchHead(ctx, branch)
		Expect(err).NotTo(HaveOccurred())
		_, err = git.Push(mainBranch, sourcePath, manifest("512Mi"))
		Expect(err).NotTo(HaveOccurred())
		supersedes(freshness.ReasonTargetFileChanged)
		headAfter, err := git.BranchHead(ctx, branch)
		Expect(err).NotTo(HaveOccurred())
		Expect(headAfter).To(Equal(headBefore))
	})

	It("closes the change request on a retry when the first close was cut short", func() {
		_, err := git.Push(mainBranch, sourcePath, manifest("512Mi"))
		Expect(err).NotTo(HaveOccurred())
		crash := true
		git.SetAfter(func(op string) error {
			if op == opClose && crash {
				crash = false
				return errors.New("connection lost after the close")
			}
			return nil
		})

		_, err = reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not decided before the close was confirmed")

		supersedes(freshness.ReasonTargetFileChanged)
		Expect(closes.Load()).To(Equal(int32(1)), "the closed pull request is known to be ours and not closed again")
	})

	// planFresh returns the PlanFresh condition of the request as stored.
	planFresh := func() *metav1.Condition {
		return meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	}
	// cutShortClose makes the plan stale and lets its close go through at the host while the answer is lost
	// (the deadline, a restart), then makes the plan fresh again.
	cutShortClose := func() {
		_, err := git.Push(mainBranch, sourcePath, manifest("512Mi"))
		Expect(err).NotTo(HaveOccurred())
		lost := true
		git.SetAfter(func(op string) error {
			if op == opClose && lost {
				lost = false
				return context.DeadlineExceeded
			}
			return nil
		})
		_, err = reconcileOnce()
		Expect(err).To(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).NotTo(Equal(remediationv1alpha1.PhaseSuperseded))
		c := planFresh()
		Expect(c).NotTo(BeNil(), "the cleanup is marked before the close")
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(freshness.ReasonTargetFileChanged))
		_, err = git.Push(mainBranch, sourcePath, manifest("256Mi"))
		Expect(err).NotTo(HaveOccurred())
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
	}

	It("ends Superseded when its own close went through unanswered and the plan looks fresh again", func() {
		cutShortClose()
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded), "our close, not a person's rejection")
		Expect(planFresh().Reason).To(Equal(freshness.ReasonTargetFileChanged))
		closedForStalePlan(got, freshness.ReasonTargetFileChanged)
	})

	It("finishes the cleanup after a restart between the mark and the close, though the plan looks fresh again", func() {
		_, err := git.Push(mainBranch, sourcePath, manifest("512Mi"))
		Expect(err).NotTo(HaveOccurred())
		failed := false
		git.SetFail(func(op string) error {
			if op == opClose && !failed {
				failed = true
				return &providers.RetryableError{Err: errors.New("connection reset")}
			}
			if op == opClose {
				closes.Add(1)
			}
			return nil
		})
		_, err = reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen), "the close did not happen")
		_, err = git.Push(mainBranch, sourcePath, manifest("256Mi"))
		Expect(err).NotTo(HaveOccurred())
		supersedes(freshness.ReasonTargetFileChanged)
		Expect(closes.Load()).To(Equal(int32(1)))
	})

	// markCleanup writes the mark of a started cleanup (PlanFresh False with a stale reason, phase unchanged),
	// after edit has changed the stored request.
	markCleanup := func(edit func(*remediationv1alpha1.RemediationRequest)) {
		got := fetch()
		edit(got)
		meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
			Type: remediationv1alpha1.ConditionPlanFresh, Status: metav1.ConditionFalse,
			Reason: freshness.ReasonTargetFileChanged, Message: "The target file changed",
		})
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
	}

	It("honours the delay a Git host asks for while a marked cleanup waits, also near the deadline", func() {
		_, err := git.Push(mainBranch, sourcePath, manifest("512Mi"))
		Expect(err).NotTo(HaveOccurred())
		markCleanup(func(got *remediationv1alpha1.RemediationRequest) {
			got.Status.Phase = remediationv1alpha1.PhasePolicyChecked
			got.Status.PullRequest = nil
			got.Status.Deadline = &metav1.Time{Time: time.Now().Add(20 * time.Second)}
		})
		git.SetFail(func(op string) error {
			if op == opClose {
				return &providers.RetryableError{RetryAfter: 10 * time.Minute, Err: errors.New("rate limited")}
			}
			return nil
		})
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(10*time.Minute), "a marked request never expires, so the delay is not cut")
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
	})

	It("does not close a recorded pull request that is not the branch's own when it resumes a marked cleanup", func() {
		head, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		// Another branch of the agent's prefix, so the host answers about the number rather than with a 404 (a 404
		// would wait for a second look, #688).
		const elsewhere = "shadewright/someone-else"
		Expect(git.CreateBranch(ctx, elsewhere, head)).To(Succeed())
		foreign, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{
			HeadBranch: elsewhere, BaseBranch: mainBranch, Title: "f", Body: pullRequestBody(fetch(), "", nil),
		})
		Expect(err).NotTo(HaveOccurred())
		markCleanup(func(got *remediationv1alpha1.RemediationRequest) {
			got.Status.PullRequest.Number = foreign.Number
		})
		// The request's own pull request lost its marker (a person edited it out): a listing that shows a pull
		// request of this request is never gone (#688).
		Expect(git.SetBody(cr.Number, unmarkedBody)).To(Succeed())
		// The look at the recorded pull request fails once, so the cleanup is reached with the forged number.
		finds := 0
		git.SetFail(func(op string) error {
			switch op {
			case opGet:
				if finds++; finds == 1 {
					return &providers.RetryableError{Err: errors.New("connection reset")}
				}
			case opClose:
				closes.Add(1)
			}
			return nil
		})
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(planFresh().Reason).To(Equal(ReasonForeignChangeRequest))
		Expect(closes.Load()).To(BeZero())
		other, err := git.FindChangeRequest(ctx, elsewhere)
		Expect(err).NotTo(HaveOccurred())
		Expect(other.State).To(Equal(providers.ChangeRequestOpen))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		stillOpen()
	})

	// #290: a new installation of the GitHub App for the same repository URL makes the plan stale (the digest covers
	// the installation). The cleanup reaches the pull request through the new installation, checks its marker and
	// closes it, as for any stale plan.
	reinstall := func() {
		GinkgoHelper()
		live := &remediationv1alpha1.RemediationSettings{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(settings), live)).To(Succeed())
		live.Spec.Repositories[0].GitHub.InstallationID = 2
		Expect(k8sClient.Update(ctx, live)).To(Succeed())
	}

	It("supersedes and closes the pull request when the repository installation changes in the settings (#290)", func() {
		reinstall()
		supersedes(freshness.ReasonPlanChanged)
		Expect(closes.Load()).To(Equal(int32(1)))
		Expect(fetch().Status.PlanRepository.InstallationID).To(Equal(int64(1)), "the plan's record is not rewritten")
	})

	It("releases the target lock of a request superseded through the new installation (#290)", func() {
		Expect(fetch().Status.HoldsTarget).To(HaveValue(BeTrue()), "the approved plan holds the lock")
		reinstall()
		supersedes(freshness.ReasonPlanChanged)
		got := fetch()
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()), "released with the end, not left to a later look")
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)).
			To(BeNil())
		// A later look of the final phase needs no new-installation lookup and writes nothing.
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().ResourceVersion).To(Equal(got.ResourceVersion))
	})

	It("ends a request closed through the new installation only once its target lock is settled (#290)", func() {
		reinstall()
		listFails := atomic.Bool{}
		listFails.Store(true)
		git.SetFail(func(op string) error {
			switch {
			case op == opClose:
				closes.Add(1)
			case op == opList && listFails.Load() && closes.Load() > 0:
				return fmt.Errorf("listing: %w", providers.ErrRetryable)
			}
			return nil
		})
		waits := func() {
			GinkgoHelper()
			res, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0), "looked at again, without a hot requeue")
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not ended yet")
			Expect(planFresh().Reason).To(Equal(freshness.ReasonPlanChanged), "the mark stays")
			Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()))
		}
		// The close goes through, but the listing of the branch fails: the request does not end with the lock
		// it could no longer settle.
		waits()
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
		Expect(closes.Load()).To(Equal(int32(1)))

		// Meanwhile someone opens another pull request from the branch: it keeps the target locked, and says so.
		listFails.Store(false)
		other, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{
			HeadBranch: freshness.BranchName(fetch()), BaseBranch: mainBranch, Title: "o", Body: "reopened by hand",
		})
		Expect(err).NotTo(HaveOccurred())
		waits()
		held := meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionTargetHeld)
		Expect(held).NotTo(BeNil())
		Expect(held.Message).To(ContainSubstring(fmt.Sprintf("#%d", other.Number)))
		Expect(closes.Load()).To(Equal(int32(1)), "the other pull request is not closed")

		// Once it is closed, the next look releases the lock in the write that ends the request.
		Expect(git.CloseChangeRequest(ctx, other.Number)).To(Succeed())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)).To(BeNil())
	})

	It("releases the target lock of a request superseded with an unchanged installation", func() {
		Expect(fetch().Status.HoldsTarget).To(HaveValue(BeTrue()))
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		supersedes(freshness.ReasonPolicyChanged)
		Expect(fetch().Status.HoldsTarget).To(HaveValue(BeFalse()))
	})

	It("fails the review check through the new installation before it closes the pull request (#290)", func() {
		reinstall()
		r := reconciler()
		r.ReviewerHints = true
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
		runs := git.CheckRuns(cr.HeadSHA)
		Expect(runs).NotTo(BeEmpty())
		Expect(runs[len(runs)-1].Status).To(Equal(providers.CheckRunFailure))
	})

	It("keeps watching, and reports it, while the new installation is refused the pull request (#290)", func() {
		reinstall()
		recorder := record.NewFakeRecorder(10)
		forbidden := atomic.Bool{}
		forbidden.Store(true)
		git.SetFail(func(op string) error {
			switch {
			case op == opClose:
				closes.Add(1)
			case forbidden.Load() && (op == opGet || op == opList):
				return fmt.Errorf("installation not granted the repository: %w", providers.ErrForbidden)
			}
			return nil
		})
		look := func() reconcile.Result {
			GinkgoHelper()
			r := reconciler()
			r.Recorder = recorder
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
			Expect(err).NotTo(HaveOccurred())
			return res
		}
		for range 2 {
			res := look()
			Expect(res.RequeueAfter).To(BeNumerically(">", 0), "watched, not dropped")
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not a final phase")
			Expect(planFresh().Reason).To(Equal(freshness.ReasonPlanChanged), "the mark stays")
			c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
			Expect(c).NotTo(BeNil())
			Expect(c.Status).To(Equal(metav1.ConditionTrue))
			Expect(c.Reason).To(Equal(ReasonGitForbidden))
			Expect(c.Message).To(ContainSubstring("another installation of the GitHub App"))
			Expect(closes.Load()).To(BeZero())
			Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		}
		notClosedEvents := 0
		for len(recorder.Events) > 0 {
			if strings.HasPrefix(<-recorder.Events, corev1.EventTypeWarning+" "+EventPullRequestNotClosed) {
				notClosedEvents++
			}
		}
		Expect(notClosedEvents).To(Equal(1), "one Event while the refusal lasts")

		// Once the installation is granted the repository, the next look closes the pull request.
		forbidden.Store(false)
		look()
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
		Expect(closes.Load()).To(Equal(int32(1)))
	})

	// #797: a failure of the new installation other than a refusal (a token the host answers with 404, an
	// unclassified one, a server error) is said in PullRequestNotClosed as a refusal is, with one Event per reason,
	// and retried as before.
	DescribeTable("says why on PullRequestNotClosed while the new installation fails without a refusal (#797)",
		func(failure error, reason string) {
			reinstall()
			recorder := record.NewFakeRecorder(10)
			failing := atomic.Bool{}
			failing.Store(true)
			git.SetFail(func(op string) error {
				switch {
				case op == opClose:
					closes.Add(1)
				case failing.Load() && (op == opGet || op == opList):
					return failure
				}
				return nil
			})
			r := reconciler()
			r.Recorder = recorder
			for range 2 {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
				Expect(err).To(HaveOccurred(), "retried with the error backoff, as before")
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not a final phase")
				Expect(planFresh().Reason).To(Equal(freshness.ReasonPlanChanged), "the mark stays")
				c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
				Expect(c).NotTo(BeNil())
				Expect(c.Status).To(Equal(metav1.ConditionTrue))
				Expect(c.Reason).To(Equal(reason))
				Expect(c.Message).To(ContainSubstring("another installation of the GitHub App"))
				Expect(c.Message).To(Or(ContainSubstring("the read of the recorded pull request"),
					ContainSubstring("the listing of the pull requests of branch")), "names the step")
				Expect(c.Message).To(ContainSubstring(fmt.Sprintf("#%d", cr.Number)))
				Expect(c.Message).NotTo(ContainSubstring("secret-host-text"), "no text of the host")
				Expect(closes.Load()).To(BeZero())
			}
			events := 0
			for len(recorder.Events) > 0 {
				if strings.HasPrefix(<-recorder.Events, corev1.EventTypeWarning+" "+EventPullRequestNotClosed+" ") {
					events++
				}
			}
			Expect(events).To(Equal(1), "one Event while the reason stays")

			failing.Store(false)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
			Expect(err).NotTo(HaveOccurred())
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
			Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
			Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
				To(BeNil(), "removed with the end")
		},
		Entry("a token the host answers with 404", fmt.Errorf("installation token unavailable: %w",
			providers.ErrNotFound), ReasonGitNotFound),
		Entry("an unclassified token failure", errors.New("installation token unavailable: secret-host-text"),
			ReasonGitFailed),
		Entry("a server error", fmt.Errorf("listing: %w", providers.ErrRetryable), ReasonGitFailed),
	)

	It("keeps the delay the host asked for while the new installation fails (#797)", func() {
		reinstall()
		git.SetFail(func(op string) error {
			if op == opGet || op == opList {
				return &providers.RetryableError{RetryAfter: time.Hour, Err: errors.New("rate limited")}
			}
			return nil
		})
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(time.Hour), "the Retry-After, not the poll interval or at once")
		c := meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal(ReasonGitFailed))
	})

	It("writes PullRequestNotClosed again after a write that lost to another one (#797)", func() {
		reinstall()
		git.SetFail(func(op string) error {
			if op == opGet || op == opList {
				return fmt.Errorf("installation token unavailable: %w", providers.ErrNotFound)
			}
			return nil
		})
		r := reconciler()
		bumped := atomic.Bool{}
		r.Client = &bumpBeforeStatusUpdate{Client: k8sClient, bump: func(obj client.Object) {
			if bumped.Swap(true) {
				return
			}
			live := &remediationv1alpha1.RemediationRequest{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), live)).To(Succeed())
			base := live.DeepCopy()
			live.Annotations = map[string]string{"example.com/touched": "797"}
			Expect(k8sClient.Patch(ctx, live, client.MergeFrom(base))).To(Succeed())
		}}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "the lost write is returned: %v", err)
		Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
			To(BeNil(), "lost")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).To(HaveOccurred())
		got := fetch()
		Expect(got.Annotations).To(HaveKeyWithValue("example.com/touched", "797"))
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
		Expect(c).NotTo(BeNil(), "written by the next look")
		Expect(c.Reason).To(Equal(ReasonGitNotFound))
	})

	It("says why the lock waits while the branch cannot be read through the new installation after the close (#797)", func() {
		reinstall()
		recorder := record.NewFakeRecorder(10)
		listFails := atomic.Bool{}
		listFails.Store(true)
		git.SetFail(func(op string) error {
			switch {
			case op == opClose:
				closes.Add(1)
			case op == opList && listFails.Load() && closes.Load() > 0:
				return fmt.Errorf("listing: %w", providers.ErrRetryable)
			}
			return nil
		})
		r := reconciler()
		r.Recorder = recorder
		for i := range 2 {
			// The first look closes the pull request and cannot settle the lock; the next one cannot read the
			// pull request either, and is retried with the error backoff.
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
			if i == 0 {
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			}
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not ended yet")
			c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
			Expect(c).NotTo(BeNil(), "the wait is visible")
			Expect(c.Reason).To(Equal(ReasonGitFailed))
			Expect(c.Message).To(ContainSubstring("new installation of the GitHub App"))
		}
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
		events := 0
		for len(recorder.Events) > 0 {
			if strings.HasPrefix(<-recorder.Events, corev1.EventTypeWarning+" "+EventPullRequestUnreadable+" ") {
				events++
			}
		}
		Expect(events).To(Equal(1))

		listFails.Store(false)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)).
			To(BeNil(), "removed by the read that worked")
	})

	// #797: a token, read or listing of the new installation that runs past the cleanup's deadline is said in
	// PullRequestNotClosed (GitFailed) as any other failure of that step, not left as a bare timeout.
	It("says why on PullRequestNotClosed when the new installation runs past the cleanup's deadline (#797)", func() {
		reinstall()
		recorder := record.NewFakeRecorder(10)
		git.SetFail(func(op string) error {
			switch op {
			case opClose:
				closes.Add(1)
			case opGet, opList:
				time.Sleep(300 * time.Millisecond)
				return fmt.Errorf("listing: %w", providers.ErrRetryable)
			}
			return nil
		})
		r := reconciler()
		r.Recorder = recorder
		r.CleanupTimeout = 50 * time.Millisecond
		for range 2 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
			Expect(err).To(MatchError(ErrTimeout), "retried with the error backoff")
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not a final phase")
			c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
			Expect(c).NotTo(BeNil(), "the retry is visible")
			Expect(c.Reason).To(Equal(ReasonGitFailed))
			Expect(c.Message).To(Or(ContainSubstring("the read of the recorded pull request"),
				ContainSubstring("the listing of the pull requests of branch")), "names the step")
		}
		Expect(closes.Load()).To(BeZero())
		events := 0
		for len(recorder.Events) > 0 {
			if strings.HasPrefix(<-recorder.Events, corev1.EventTypeWarning+" "+EventPullRequestNotClosed+" ") {
				events++
			}
		}
		Expect(events).To(Equal(1))
	})

	// #797: the token of the new installation that settles the target lock after the close fails: the wait is said
	// in PullRequestUnreadable, as a failed read there is.
	It("says why the lock waits while the new installation's token fails after the close (#797)", func() {
		reinstall()
		tokenFails := atomic.Bool{}
		tokenFails.Store(true)
		git.SetFail(func(op string) error {
			if op == opClose {
				closes.Add(1)
			}
			return nil
		})
		r := reconciler()
		open := r.OpenRepository
		r.OpenRepository = func(ctx context.Context, repo remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			if tokenFails.Load() && closes.Load() > 0 {
				return nil, fmt.Errorf("installation token: %w", providers.ErrRetryable)
			}
			return open(ctx, repo)
		}
		for range 2 {
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not ended yet")
			Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()))
			c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
			Expect(c).NotTo(BeNil(), "the wait is visible")
			Expect(c.Reason).To(Equal(ReasonGitFailed))
			Expect(c.Message).To(ContainSubstring("open the repository"))
			Expect(c.Message).To(ContainSubstring("new installation of the GitHub App"))
		}
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))

		tokenFails.Store(false)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)).
			To(BeNil(), "removed by the read that worked")
	})

	// #797: PullRequestNotClosed set by an earlier look through the new installation is removed by the look that
	// closes the pull request, in the same write as the closed record, also while the target lock waits; the end then
	// says that Shadewright closed it.
	It("records the close through the new installation while the lock waits after an earlier failure (#797)", func() {
		reinstall()
		getFails, listFails := atomic.Bool{}, atomic.Bool{}
		getFails.Store(true)
		listFails.Store(true)
		git.SetFail(func(op string) error {
			switch {
			case op == opClose:
				closes.Add(1)
			case op == opGet && getFails.Load():
				return fmt.Errorf("installation token unavailable: %w", providers.ErrNotFound)
			case op == opList && listFails.Load() && closes.Load() > 0:
				return fmt.Errorf("listing: %w", providers.ErrRetryable)
			}
			return nil
		})
		_, err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
			NotTo(BeNil(), "the failed read is said")

		getFails.Store(false)
		before := fetch()
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval), "the poll interval is kept")
		Expect(closes.Load()).To(Equal(int32(1)))
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "the lock waits")
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
			To(BeNil(), "removed by the close that went through")
		opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(opened.Reason).To(Equal(ReasonPullRequestClosed), "the close is recorded in the same write")
		Expect(skipOwnMarks.Update(crevent.UpdateEvent{ObjectOld: before, ObjectNew: got})).To(BeFalse(),
			"the write wakes no reconcile of its own, so the poll interval stands")

		listFails.Store(false)
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
		opened = meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(opened.Reason).To(Equal(ReasonPullRequestClosed))
		Expect(opened.Message).NotTo(ContainSubstring("not known whether"), "Shadewright closed it")
	})

	It("writes the close through the new installation again after a write that lost to another one (#797)", func() {
		reinstall()
		getFails, listFails := atomic.Bool{}, atomic.Bool{}
		getFails.Store(true)
		listFails.Store(true)
		git.SetFail(func(op string) error {
			switch {
			case op == opClose:
				closes.Add(1)
			case op == opGet && getFails.Load():
				return fmt.Errorf("installation token unavailable: %w", providers.ErrNotFound)
			case op == opList && listFails.Load() && closes.Load() > 0:
				return fmt.Errorf("listing: %w", providers.ErrRetryable)
			}
			return nil
		})
		_, err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		getFails.Store(false)
		r := reconciler()
		bumps := atomic.Int32{}
		r.Client = &bumpBeforeStatusUpdate{Client: k8sClient, bump: func(obj client.Object) {
			rr := obj.(*remediationv1alpha1.RemediationRequest)
			if notClosed(rr) != nil || bumps.Add(1) > 1 {
				return
			}
			live := &remediationv1alpha1.RemediationRequest{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), live)).To(Succeed())
			base := live.DeepCopy()
			live.Annotations = map[string]string{"example.com/touched": "797"}
			Expect(k8sClient.Patch(ctx, live, client.MergeFrom(base))).To(Succeed())
		}}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval))
		Expect(bumps.Load()).To(BeNumerically(">=", 1), "the write met a change")
		got := fetch()
		Expect(got.Annotations).To(HaveKeyWithValue("example.com/touched", "797"))
		Expect(notClosed(got)).To(BeNil(), "written again after the conflict")
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(ReasonPullRequestClosed))
	})

	// #797: every failure of a look through the new installation, the close included, moves PullRequestNotClosed to
	// that step, with one Event per change of reason or step, and keeps the delay the host asked for.
	DescribeTable("names the latest failed step of the new installation on PullRequestNotClosed (#797)",
		func(first error, firstReason string, closeErr error, closeReason string, delay time.Duration) {
			reinstall()
			recorder := record.NewFakeRecorder(10)
			getFails, closeFails := atomic.Bool{}, atomic.Bool{}
			getFails.Store(true)
			git.SetFail(func(op string) error {
				switch {
				case (op == opGet || op == opList) && getFails.Load():
					return first
				case op == opClose && closeFails.Load():
					closes.Add(1)
					return closeErr
				}
				return nil
			})
			r := reconciler()
			r.Recorder = recorder
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
			c := notClosed(fetch())
			Expect(c).NotTo(BeNil())
			Expect(c.Reason).To(Equal(firstReason))
			Expect(c.Message).To(Or(ContainSubstring("the read of the recorded pull request"),
				ContainSubstring("the listing of the pull requests of branch")))

			getFails.Store(false)
			closeFails.Store(true)
			for range 2 {
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
				if delay > 0 {
					Expect(err).NotTo(HaveOccurred())
					Expect(res.RequeueAfter).To(Equal(delay), "the Retry-After is kept")
				} else {
					Expect(err).To(HaveOccurred(), "retried with the error backoff")
				}
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
				c = notClosed(got)
				Expect(c).NotTo(BeNil())
				Expect(c.Reason).To(Equal(closeReason))
				Expect(c.Message).To(ContainSubstring("the close of pull request"), "names the latest step")
				Expect(c.Message).NotTo(Or(ContainSubstring("the read of the recorded pull request"),
					ContainSubstring("the listing of the pull requests of branch")))
				Expect(c.Message).NotTo(ContainSubstring("secret-host-text"))
			}
			Expect(closes.Load()).To(Equal(int32(2)))
			events := 0
			for len(recorder.Events) > 0 {
				if strings.HasPrefix(<-recorder.Events, corev1.EventTypeWarning+" "+EventPullRequestNotClosed+" ") {
					events++
				}
			}
			Expect(events).To(Equal(2), "one Event for each step")
		},
		Entry("a server error on the close after a 404 on the read",
			fmt.Errorf("read: %w", providers.ErrNotFound), ReasonGitNotFound,
			fmt.Errorf("close: secret-host-text: %w", providers.ErrRetryable), ReasonGitFailed, time.Duration(0)),
		Entry("a server error on the close after one on the read",
			fmt.Errorf("read: %w", providers.ErrRetryable), ReasonGitFailed,
			fmt.Errorf("close: %w", providers.ErrRetryable), ReasonGitFailed, time.Duration(0)),
		Entry("a rate limit on the close",
			fmt.Errorf("read: %w", providers.ErrRetryable), ReasonGitFailed,
			&providers.RetryableError{RetryAfter: time.Hour, Err: errors.New("rate limited")}, ReasonGitFailed, time.Hour),
		Entry("an unclassified failure of the close",
			fmt.Errorf("read: %w", providers.ErrNotFound), ReasonGitNotFound,
			errors.New("secret-host-text"), ReasonGitFailed, time.Duration(0)),
	)

	It("names the close on PullRequestNotClosed when it runs past the cleanup's deadline (#797)", func() {
		reinstall()
		getFails, slowClose := atomic.Bool{}, atomic.Bool{}
		getFails.Store(true)
		git.SetFail(func(op string) error {
			switch {
			case op == opGet && getFails.Load():
				return fmt.Errorf("read: %w", providers.ErrRetryable)
			case op == opClose && slowClose.Load():
				closes.Add(1)
				time.Sleep(300 * time.Millisecond)
				return fmt.Errorf("close: %w", providers.ErrRetryable)
			}
			return nil
		})
		r := reconciler()
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(notClosed(fetch())).NotTo(BeNil())
		getFails.Store(false)
		slowClose.Store(true)
		r.CleanupTimeout = 100 * time.Millisecond
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).To(MatchError(ErrTimeout), "retried with the error backoff")
		Expect(closes.Load()).To(Equal(int32(1)))
		c := notClosed(fetch())
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal(ReasonGitFailed))
		Expect(c.Message).To(ContainSubstring("the close of pull request"), "names the latest step")
	})

	// #797: the check right before the close that the request still owns its pull request (stillOwns) reads the
	// request from the Kubernetes API, not from the Git host: an intent to hand it over written since the reconcile
	// read it, or a failed read, is retried as before and never said in PullRequestNotClosed as a failure of the new
	// installation.
	DescribeTable("says nothing on PullRequestNotClosed for a Kubernetes failure of a cleanup through the new installation (#797)",
		func(fail error, edit func(*remediationv1alpha1.RemediationRequest), want error) {
			reinstall()
			recorder := record.NewFakeRecorder(10)
			armed := atomic.Bool{}
			git.SetFail(func(op string) error {
				switch op {
				case opGet:
					armed.Store(true)
				case opClose:
					closes.Add(1)
				}
				return nil
			})
			r := reconciler()
			r.Recorder = recorder
			r.APIReader = &armedRequestReader{Reader: k8sClient, armed: &armed, fail: fail, edit: edit}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
			Expect(armed.Load()).To(BeTrue(), "the pull request was read before the check")
			Expect(err).To(MatchError(want), "retried as before")
			Expect(closes.Load()).To(BeZero())
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not a final phase")
			Expect(notClosed(got)).To(BeNil(), "not a failure of the Git host")
			for len(recorder.Events) > 0 {
				Expect(<-recorder.Events).NotTo(ContainSubstring(EventPullRequestNotClosed))
			}
		},
		Entry("an intent to hand the pull request over", nil, func(rr *remediationv1alpha1.RemediationRequest) {
			rr.Status.Handoff = &remediationv1alpha1.Handoff{To: &remediationv1alpha1.OutgoingHandoff{
				Phase: remediationv1alpha1.HandoffIntent}}
		}, errHandoffInFlight),
		Entry("a failed read of the request", apierrors.NewServiceUnavailable("the server is unavailable"), nil,
			apierrors.NewServiceUnavailable("the server is unavailable")),
	)

	It("closes nothing through the new installation when the pull request lacks this request's marker (#290)", func() {
		reinstall()
		Expect(git.SetBody(cr.Number, "Someone else's pull request")).To(Succeed())
		r := reconciler()
		r.ReviewerHints = true
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(planFresh().Reason).To(Equal(ReasonForeignChangeRequest))
		Expect(closes.Load()).To(BeZero())
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		Expect(git.CheckRuns(cr.HeadSHA)).To(BeEmpty(), "no check run on a pull request that is not this request's")
		stillOpen()
	})

	// cancelIt sets spec.cancel on the stored request with a merge patch, as `kubectl patch --type merge` does.
	cancelIt := func() {
		GinkgoHelper()
		got := fetch()
		base := got.DeepCopy()
		got.Spec.Cancel = true
		Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
	}

	It("releases the target lock of a stale request cancelled and closed through the new installation (#290)", func() {
		reinstall()
		forbidden := atomic.Bool{}
		forbidden.Store(true)
		git.SetFail(func(op string) error {
			switch {
			case op == opClose:
				closes.Add(1)
			case forbidden.Load() && (op == opGet || op == opList):
				return fmt.Errorf("installation not granted the repository: %w", providers.ErrForbidden)
			}
			return nil
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(planFresh().Reason).To(Equal(freshness.ReasonPlanChanged), "watched with the mark")
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))

		// A person cancels the request; then the installation is granted the repository.
		cancelIt()
		forbidden.Store(false)
		for range 4 {
			if fetch().Status.Phase == remediationv1alpha1.PhaseCancelled {
				break
			}
			_, err = reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
		}
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()), "released with the end, not left to a later look")
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)).
			To(BeNil())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().ResourceVersion).To(Equal(got.ResourceVersion), "a later look writes nothing")
	})

	It("ends a stale request cancelled with its pull request closed through the new installation only once its target lock is settled (#290)", func() {
		reinstall()
		forbidden := atomic.Bool{}
		forbidden.Store(true)
		git.SetFail(func(op string) error {
			switch {
			case op == opClose:
				closes.Add(1)
			case forbidden.Load() && (op == opGet || op == opList):
				return fmt.Errorf("installation not granted the repository: %w", providers.ErrForbidden)
			}
			return nil
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(planFresh().Reason).To(Equal(freshness.ReasonPlanChanged), "watched with the mark")

		// A person closes the pull request, opens another one from the branch and cancels the request.
		Expect(git.CloseChangeRequest(ctx, cr.Number)).To(Succeed())
		other, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{
			HeadBranch: freshness.BranchName(fetch()), BaseBranch: mainBranch, Title: "o", Body: "opened by hand",
		})
		Expect(err).NotTo(HaveOccurred())
		cancelIt()
		forbidden.Store(false)
		for range 2 {
			res, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0), "looked at again, without a hot requeue")
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not ended yet")
			Expect(got.Status.HoldsTarget).To(HaveValue(BeTrue()))
			held := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)
			Expect(held).NotTo(BeNil(), "the wait is visible")
			Expect(held.Message).To(ContainSubstring(fmt.Sprintf("#%d", other.Number)))
		}

		// Once the other pull request is closed, the next look releases the lock in the write that ends the request.
		Expect(git.CloseChangeRequest(ctx, other.Number)).To(Succeed())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseCancelled))
		Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)).To(BeNil())
	})

	It("records the merge of a pull request merged before the new installation's cleanup closed it (#290)", func() {
		reinstall()
		sha, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		// The first look cannot read the pull request through the old installation; the cleanup through the new
		// one finds it merged and is refused the close (a conflict), which is retried.
		_, err = reconcileOnce()
		Expect(err).To(MatchError(providers.ErrConflict))
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		// The retry looks through the new installation, as the mark allows, and records the merge as a stale plan's.
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.PullRequest.MergeCommitSHA).To(Equal(sha))
		Expect(planFresh().Reason).To(Equal(freshness.ReasonPlanChanged))
		Expect(stalePlanMerged(got)).To(BeTrue())
		Expect(closes.Load()).To(Equal(int32(1)), "the one close the host refused as a conflict")
	})

	It("closes nothing when the entry names another URL and another installation (#290)", func() {
		const moved = "https://github.com/example/moved"
		byURL[moved] = git
		live := &remediationv1alpha1.RemediationSettings{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(settings), live)).To(Succeed())
		live.Spec.Repositories[0].URL = moved
		live.Spec.Repositories[0].GitHub.InstallationID = 2
		Expect(k8sClient.Update(ctx, live)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(planFresh().Reason).To(Equal(reasonRepositoryUnavailable))
		Expect(closes.Load()).To(BeZero())
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
	})

	It("keeps the mark when the pull request merges before the cleanup closes it, so the outcome is capped (#199)", func() {
		markCleanup(func(*remediationv1alpha1.RemediationRequest) {})
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(planFresh()).NotTo(BeNil(), "the plan was known stale when it merged")
		Expect(stalePlanMerged(got)).To(BeTrue())
	})

	It("records the merge commit in the same write that ends the request in Merged", func() {
		Expect(fetch().Status.PullRequest.MergeCommitSHA).To(BeEmpty())
		sha, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.PullRequest.MergeCommitSHA).To(Equal(sha))
	})

	// The lab case of #737: the policy is edited after the plan, and a person merges before any full recheck saw
	// it. The look at the pull request checks the plan at the merge, so the request carries the stale mark and
	// never ends as a success.
	It("marks a plan stale when the policy changed before a merge that no full recheck saw (#737)", func() {
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 90
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		sha, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.PullRequest.MergeCommitSHA).To(Equal(sha), "the merge is recorded in the same write")
		Expect(got.Status.MergeReceipt).NotTo(BeNil())
		c := planFresh()
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(freshness.ReasonPolicyChanged))
		Expect(stalePlanMerged(got)).To(BeTrue())
		Expect(breakerOutcome(got)).To(Equal(outcomeNone), "a stale plan merged is never a success")
		Expect(closes.Load()).To(BeZero(), "a merged pull request is not closed")

		// The cap ends it Inconclusive where it would have ended Succeeded (or kept Merged with verification off).
		done, err := reconciler().capStaleSuccess(ctx, got)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		ended := fetch()
		Expect(ended.Status.Phase).To(Equal(remediationv1alpha1.PhaseInconclusive))
		verified := meta.FindStatusCondition(ended.Status.Conditions, remediationv1alpha1.ConditionVerified)
		Expect(verified).NotTo(BeNil())
		Expect(verified.Reason).To(Equal(ReasonStalePlanMerged))
		Expect(verified.Message).To(Equal(fmt.Sprintf("Pull request #%d was merged with a plan that was stale "+
			"when the merge was recorded (%s, see PlanFresh); the change is not reported as a success", cr.Number,
			freshness.ReasonPolicyChanged)))
		Expect(breakerOutcome(ended)).To(Equal(outcomeNone))
	})

	It("records a merge as fresh when nothing changed, although Git and the live target hold the new value", func() {
		// After the merge the target file holds the new value, which the full check would call stale.
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(planFresh()).To(BeNil())
		Expect(stalePlanMerged(got)).To(BeFalse())
	})

	It("records nothing while the plan cannot be checked at the merge, then checks it again (#737)", func() {
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 90
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		var fail atomic.Bool
		fail.Store(true)
		r := reconciler()
		r.APIReader = failingPolicyReader{Reader: k8sClient, fail: fail.Load}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).To(MatchError(errMergeCheck))
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(got.Status.MergeReceipt).To(BeNil())
		Expect(got.Status.MergeCommitAwaitedSince).NotTo(BeNil(), "only the start of the wait is written")
		Expect(planFresh()).To(BeNil())
		Expect(closes.Load()).To(BeZero())

		fail.Store(false)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(planFresh().Reason).To(Equal(freshness.ReasonPolicyChanged))
	})

	// The check at the merge runs within half of the look's deadline (mergeCheckContext): when it runs out of time,
	// the look still writes the start of the wait, and the merge must not reach the full check, which calls a
	// merged change stale (TargetFileChanged) and would cap a fresh plan.
	It("never hands a merge to the full check when the check at the merge runs out of time (#737)", func() {
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		var blocked atomic.Bool
		r := reconciler()
		r.WriteTimeout = 300 * time.Millisecond
		r.APIReader = blockingPolicyReader{Reader: k8sClient, block: func() bool { return blocked.CompareAndSwap(false, true) }}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).To(MatchError(errMergeCheck))
		Expect(err).To(MatchError(context.DeadlineExceeded))
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(planFresh()).To(BeNil(), "a fresh plan is never marked stale by the full check after a merge")
		Expect(got.Status.MergeCommitAwaitedSince).NotTo(BeNil(), "the wait bounds a check that always runs out of time")
		Expect(closes.Load()).To(BeZero())

		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(planFresh()).To(BeNil())
	})

	It("keeps a merge seen as not recorded when the reconcile is cancelled during the check at the merge (#737)", func() {
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		parent, cancel := context.WithCancel(ctx)
		defer cancel()
		r := reconciler()
		r.APIReader = blockingPolicyReader{Reader: k8sClient, block: func() bool { cancel(); return true }}
		outcome, err := r.watchPullRequest(parent, fetch())
		Expect(outcome).To(Equal(prMergeUnchecked))
		Expect(err).To(MatchError(errMergeCheck))
		Expect(err).To(MatchError(context.Canceled))
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(planFresh()).To(BeNil())
	})

	// A read that keeps failing (here forbidden) is retried within the wait of the merge commit; at its end the
	// merge is recorded as not shown fresh, the target lock goes, and the cap applies.
	It("records the merge as not shown fresh when the check fails until the end of the wait (#737)", func() {
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		start := time.Now()
		events := record.NewFakeRecorder(10)
		reconcileAt := func(now time.Time) error {
			r := reconciler()
			r.Now = func() time.Time { return now }
			r.Recorder = events
			r.APIReader = forbiddenPolicyReader{Reader: k8sClient}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
			return err
		}
		for _, after := range []time.Duration{0, mergeCommitWait - time.Minute} {
			Expect(reconcileAt(start.Add(after))).To(MatchError(errMergeCheck))
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			Expect(got.Status.MergeCommitAwaitedSince).NotTo(BeNil())
			Expect(got.Status.MergeCommitAwaitedSince.Time).To(BeTemporally("~", start, time.Second))
			Expect(planFresh()).To(BeNil())
		}
		Expect(reconcileAt(start.Add(mergeCommitWait + time.Second))).To(Succeed())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.MergeReceipt).NotTo(BeNil())
		c := planFresh()
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(freshness.ReasonPlanChanged))
		Expect(stalePlanMerged(got)).To(BeTrue())
		Expect(breakerOutcome(got)).To(Equal(outcomeNone))
		Expect(events.Events).To(Receive(HavePrefix(corev1.EventTypeWarning + " " + ReasonStalePlanMerged)))
	})

	// The check at the merge decides from the state when the merge is seen, not from a mark an earlier check left
	// and that is gone by then.
	It("decides at the merge from the current state after a stale mark was cleared (#737)", func() {
		markCleanup(func(*remediationv1alpha1.RemediationRequest) {})
		Expect(planFresh().Reason).To(Equal(freshness.ReasonTargetFileChanged))
		got := fetch()
		meta.RemoveStatusCondition(&got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 90
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(planFresh().Reason).To(Equal(freshness.ReasonPolicyChanged), "the current state, not the old mark")
	})

	It("decides at the merge from the current state after the periodic check cleared its wait (#737)", func() {
		r := reconciler()
		r.Source = sourceFunc(func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
			return sourcemap.Result{Outcome: sourcemap.NeedsIntervention, Reason: sourcemap.ReasonKustomizationReconciling}, nil
		})
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).To(MatchError(freshness.ErrReconciling))
		Expect(planFresh()).NotTo(BeNil())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(planFresh()).To(BeNil(), "the periodic check passed and cleared its mark")
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 90
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		_, err = git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(planFresh().Reason).To(Equal(freshness.ReasonPolicyChanged))
	})

	It("marks a stale plan when a merge of a pull request never recorded is found (#737)", func() {
		// Its open lost its status write (#524): no pull request recorded.
		got := fetch()
		got.Status.Phase = remediationv1alpha1.PhasePolicyChecked
		got.Status.PullRequest = nil
		meta.RemoveStatusCondition(&got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 90
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		merged := changeRequest()
		Expect(merged.State).To(Equal(providers.ChangeRequestMerged))
		err = reconciler().endMerged(ctx, git, fetch(), merged)
		Expect(err).To(MatchError(errStopped))
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(planFresh().Reason).To(Equal(freshness.ReasonPolicyChanged))
		Expect(stalePlanMerged(got)).To(BeTrue())
	})

	// A pull request never recorded (#524), found merged, whose check at the merge fails: the pull request is
	// recorded, so the next reconcile looks at it and checks at the merge again, never runs the full check, which
	// calls the merged change stale (TargetFileChanged) and would end a fresh plan capped.
	It("records a merged pull request never recorded when its check at the merge fails, and checks it again (#737)", func() {
		got := fetch()
		got.Status.Phase = remediationv1alpha1.PhasePolicyChecked
		got.Status.PullRequest = nil
		meta.RemoveStatusCondition(&got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		merged := changeRequest()
		r := reconciler()
		r.APIReader = failingPolicyReader{Reader: k8sClient, fail: func() bool { return true }}
		err = r.endMerged(ctx, git, fetch(), merged)
		Expect(err).To(MatchError(errMergeCheck))
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePRCreated))
		Expect(got.Status.PullRequest).NotTo(BeNil(), "the pull request is recorded for the next look")
		Expect(got.Status.PullRequest.Number).To(Equal(merged.Number))
		Expect(got.Status.MergeReceipt).To(BeNil())
		Expect(planFresh()).To(BeNil())

		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.MergeReceipt).NotTo(BeNil())
		Expect(planFresh()).To(BeNil(), "an unchanged plan is not marked by the full check after the merge")
		Expect(stalePlanMerged(got)).To(BeFalse(), "a fresh plan is never capped")
	})

	// forgetPullRequest stands for a request whose pull request open lost its status write (#524): no pull
	// request is recorded, though the branch's pull request carries the request's marker.
	forgetPullRequest := func(rr *remediationv1alpha1.RemediationRequest) {
		rr.Status.Phase = remediationv1alpha1.PhasePolicyChecked
		rr.Status.PullRequest = nil
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	}
	// unrecordedMerge marks a started cleanup of such a request and has a person merge its pull request.
	unrecordedMerge := func() string {
		markCleanup(forgetPullRequest)
		sha, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		return sha
	}

	It("records the merge commit when a stale plan's cleanup finds its unrecorded pull request merged (#561)", func() {
		sha := unrecordedMerge()
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(closes.Load()).To(BeZero(), "a merged pull request is not closed")
		Expect(got.Status.PullRequest).NotTo(BeNil(), "the pull request is recorded")
		Expect(got.Status.PullRequest.Number).To(Equal(cr.Number))
		Expect(got.Status.PullRequest.MergeCommitSHA).To(Equal(sha), "delivery is checked against this commit")
	})

	It("records the merge commit when the pull request merges between a stale plan's look-up and its close (#561)", func() {
		markCleanup(forgetPullRequest)
		var sha string
		git.SetFail(func(op string) error {
			if op != opClose {
				return nil
			}
			// A person merges right before the close, which the host then refuses as a conflict.
			var err error
			sha, err = git.Merge(cr.Number)
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(sha).NotTo(BeEmpty(), "the close was tried")
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.PullRequest).NotTo(BeNil(), "the pull request is recorded")
		Expect(got.Status.PullRequest.MergeCommitSHA).To(Equal(sha), "delivery is checked against this commit")
	})

	It("records the merge of the pull request it tried to close, not a newer one of the branch (#688)", func() {
		markCleanup(forgetPullRequest)
		var sha string
		var next providers.ChangeRequest
		git.SetFail(func(op string) error {
			if op != opClose || sha != "" {
				return nil
			}
			// A person merges right before the close, and a newer pull request is opened on the same branch.
			var err error
			sha, err = git.Merge(cr.Number)
			Expect(err).NotTo(HaveOccurred())
			next, err = git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: cr.HeadBranch,
				BaseBranch: mainBranch, Title: "opened after the merge", Body: cr.Body})
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(next.Number).NotTo(Equal(cr.Number))
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.PullRequest).NotTo(BeNil(), "the pull request is recorded")
		Expect(got.Status.PullRequest.Number).To(Equal(cr.Number))
		Expect(got.Status.PullRequest.MergeCommitSHA).To(Equal(sha))
	})

	It("records an unrecorded merge that a newer closed pull request of the branch hides (#688)", func() {
		markCleanup(forgetPullRequest)
		sha, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		next, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: cr.HeadBranch,
			BaseBranch: mainBranch, Title: "opened after the merge", Body: cr.Body})
		Expect(err).NotTo(HaveOccurred())
		Expect(git.CloseChangeRequest(ctx, next.Number)).To(Succeed())
		newest, err := git.FindChangeRequest(ctx, cr.HeadBranch)
		Expect(err).NotTo(HaveOccurred())
		Expect(newest.Number).To(Equal(next.Number), "the newest of the branch is the closed one")
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(got.Status.PullRequest).NotTo(BeNil())
		Expect(got.Status.PullRequest.Number).To(Equal(cr.Number))
		Expect(got.Status.PullRequest.MergeCommitSHA).To(Equal(sha))
	})

	It("binds the look-up of an unrecorded merge to the number it tried and to the target branch (#688)", func() {
		markCleanup(forgetPullRequest)
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		for name, c := range map[string]struct {
			bend func(*providers.ChangeRequest)
			// retried: an answer about another pull request is a failed read, never an answer.
			retried bool
		}{
			"another number":      {func(c *providers.ChangeRequest) { c.Number++ }, true},
			"another base branch": {func(c *providers.ChangeRequest) { c.BaseBranch = "maintenance" }, false},
		} {
			r := reconciler()
			r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
				return &bentAnswer{Git: git, bend: c.bend}, nil
			}
			closeErr := &closeConflictError{number: cr.Number, err: fmt.Errorf("close: %w", providers.ErrConflict)}
			err := r.mergedUnrecorded(ctx, fetch(), closeErr)
			if c.retried {
				Expect(isUnknownChange(err)).To(BeTrue(), name)
				Expect(errors.Is(err, closeErr)).To(BeTrue(), name)
			} else {
				Expect(err).To(BeIdenticalTo(error(closeErr)), name)
			}
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked), name)
			Expect(got.Status.PullRequest).To(BeNil(), name)
		}
	})

	It("retries an unrecorded merge whose number the Git host answers 404 for while the branch lists it (#688)", func() {
		markCleanup(forgetPullRequest)
		_, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		git.SetFail(func(op string) error {
			if op == opGet {
				return fmt.Errorf("pull request: %w", providers.ErrNotFound)
			}
			return nil
		})
		defer git.SetFail(nil)
		closeErr := &closeConflictError{number: cr.Number, err: fmt.Errorf("close: %w", providers.ErrConflict)}
		err = reconciler().mergedUnrecorded(ctx, fetch(), closeErr)
		Expect(isUnknownChange(err)).To(BeTrue(), "a 404 the branch does not confirm is a failed read")
		Expect(errors.Is(err, errUnconfirmedNotFound)).To(BeTrue())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
		// A 404 the branch confirms is an answer once it was seen before (#688): no merged pull request of this
		// request with that number. The first such look is only kept in memory.
		// The merged one loses the marker, since a listing that shows a pull request of this request is never gone.
		Expect(git.SetBody(cr.Number, unmarkedBody)).To(Succeed())
		gone := &closeConflictError{number: 9999, err: fmt.Errorf("close: %w", providers.ErrConflict)}
		r := reconciler()
		err = r.mergedUnrecorded(ctx, fetch(), gone)
		Expect(errors.Is(err, errGoneUnconfirmed)).To(BeTrue(), "a first confirmed 404 waits: %v", err)
		first, ok := r.looks().first(fetch(), 9999)
		Expect(ok).To(BeTrue(), "the first look is kept in this process's memory")
		// The same process looks again once the wait is over.
		r.looks().note(fetch(), 9999, first.Add(-confirmWait-time.Second))
		Expect(r.mergedUnrecorded(ctx, fetch(), gone)).To(BeIdenticalTo(error(gone)))
	})

	It("still ends a fresh request whose pull request a person closed in NeedsIntervention", func() {
		Expect(git.CloseChangeRequest(ctx, cr.Number)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal(ReasonChangeRequestClosed))
		Expect(planFresh()).To(BeNil())
	})

	It("does not supersede and does not close when an input cannot be read", func() {
		git.SetFail(func(op string) error {
			if op == opBranchHead {
				return &providers.RetryableError{Err: errors.New("host down")}
			}
			return nil
		})
		_, err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		Expect(reconciler().freshErr(ctx, fetch())).NotTo(Succeed())
	})

	// A stale plan whose close the Git host refuses for good (#199): the request keeps its phase and the mark, says
	// why on PullRequestNotClosed, and watches the pull request, so a merge or a close by a person is recorded.
	Context("when the Git host refuses the close of a stale plan for good (#199)", func() {
		var (
			events *record.FakeRecorder
			refuse error // the error of every close; nil lets it through
			hints  bool  // the executor flag --reviewer-hints
		)
		BeforeEach(func() { hints = false })
		watcher := func() (reconcile.Result, error) {
			r := reconciler()
			r.Recorder = events
			r.ReviewerHints = hints
			return r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		}
		notClosedEvents := func() int {
			n := 0
			for len(events.Events) > 0 {
				if strings.HasPrefix(<-events.Events, corev1.EventTypeWarning+" "+EventPullRequestNotClosed+" ") {
					n++
				}
			}
			return n
		}
		// refused makes the plan stale and every close fail with class, and runs one reconcile.
		refused := func(class error) {
			GinkgoHelper()
			events = record.NewFakeRecorder(20)
			refuse = fmt.Errorf("status 403 from https://github.com/example/fleet with token ghs_leaked: %w", class)
			git.SetFail(func(op string) error {
				if op != opClose {
					return nil
				}
				closes.Add(1)
				return refuse
			})
			// A policy edit makes the plan stale and leaves the pull request mergeable.
			policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
			Expect(k8sClient.Update(ctx, policy)).To(Succeed())
			res, err := watcher()
			Expect(err).NotTo(HaveOccurred(), "a final refusal is not handed back for a retry")
			Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval), "the pull request is looked at again")
		}
		// expectStale checks the mark: PlanFresh False with the stale reason.
		expectStale := func(got *remediationv1alpha1.RemediationRequest) {
			GinkgoHelper()
			c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
			Expect(c).NotTo(BeNil())
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal(freshness.ReasonPolicyChanged))
		}

		DescribeTable("keeps the phase, says why on PullRequestNotClosed and tries the close again on every look",
			func(class error, reason string) {
				refused(class)
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not NeedsIntervention")
				expectStale(got)
				c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
				Expect(c).NotTo(BeNil())
				Expect(c.Status).To(Equal(metav1.ConditionTrue))
				Expect(c.Reason).To(Equal(reason))
				Expect(c.Message).To(ContainSubstring(fmt.Sprintf("#%d (%s)", cr.Number, cr.URL)))
				Expect(c.Message).NotTo(ContainSubstring("ghs_leaked"))
				Expect(c.Message).NotTo(ContainSubstring("status 403"))
				Expect(notClosedEvents()).To(Equal(1))
				stillOpen()
				Expect(closes.Load()).To(Equal(int32(1)))

				// Each look tries the close again and writes nothing else: no status write, no Event, no commit.
				before := fetch().ResourceVersion
				for i := range 3 {
					res, err := watcher()
					Expect(err).NotTo(HaveOccurred())
					Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval))
					Expect(closes.Load()).To(Equal(int32(2 + i)))
				}
				Expect(fetch().ResourceVersion).To(Equal(before))
				Expect(notClosedEvents()).To(BeZero())
				Expect(changeRequest().HeadSHA).To(Equal(cr.HeadSHA), "nothing is pushed to the pull request")
				Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
				Expect(git.CheckRuns(cr.HeadSHA)).To(BeEmpty(), "reviewer hints off: no check run is written")
			},
			Entry("403", providers.ErrForbidden, ReasonGitForbidden),
			Entry("422", providers.ErrRejected, ReasonGitRejected),
		)

		// The check run shadewright/review of a stale plan (#713): with reviewer hints on it ends failed once, whatever
		// becomes of the close, and a refused write of it changes nothing else and is tried again with the close.
		Context("with reviewer hints on (#713)", func() {
			// green turns reviewer hints on and lets the merge gate pass the check on the head, on an approval by
			// someone who wrote no hint, while the plan is still fresh.
			green := func() {
				GinkgoHelper()
				hints = true
				approve(git, cr.Number, 1, 4242, cr.HeadSHA)
				_, err := watcher()
				Expect(err).NotTo(HaveOccurred())
				status, found, err := git.ReviewCheckRun(ctx, cr.HeadSHA)
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue())
				Expect(status).To(Equal(providers.CheckRunSuccess), "the check is green before the plan goes stale")
				Expect(fetch().Status.PullRequest.ReviewCheck.State).To(Equal(remediationv1alpha1.ReviewCheckPassed))
			}
			// expectStaleCheck checks that the latest run on the head is the failed check of a stale plan, that n
			// runs were written by the executor in all, and that Stale is recorded.
			expectStaleCheck := func(n int) {
				GinkgoHelper()
				runs := git.CheckRuns(cr.HeadSHA)
				Expect(runs).To(HaveLen(n))
				last := runs[len(runs)-1]
				Expect(last.Name).To(Equal(providers.CheckRunName))
				Expect(last.Status).To(Equal(providers.CheckRunFailure))
				Expect(last.Title).To(Equal(staleCheckTitle))
				Expect(last.Summary).To(ContainSubstring("stale (" + freshness.ReasonPolicyChanged + ")"))
				status, found, err := git.ReviewCheckRun(ctx, cr.HeadSHA)
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue())
				Expect(status).To(Equal(providers.CheckRunFailure))
				rc := fetch().Status.PullRequest.ReviewCheck
				Expect(rc).NotTo(BeNil())
				Expect(rc.Head).To(Equal(cr.HeadSHA))
				Expect(rc.State).To(Equal(remediationv1alpha1.ReviewCheckStale))
			}

			It("fails a green check once when the close is refused, and never writes it again", func() {
				green()
				runs := len(git.CheckRuns(cr.HeadSHA))
				refused(providers.ErrForbidden)
				// The look before the freshness check left the green check as it was; the cleanup failed it.
				runs++
				expectStaleCheck(runs)
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
				expectStale(got)
				Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
					NotTo(BeNil())
				stillOpen()

				// Each later look tries the close again and nothing else: no check run, no status write.
				before := got.ResourceVersion
				for i := range 3 {
					_, err := watcher()
					Expect(err).NotTo(HaveOccurred())
					Expect(closes.Load()).To(Equal(int32(2 + i)))
				}
				expectStaleCheck(runs)
				Expect(fetch().ResourceVersion).To(Equal(before))
			})

			It("fails the check when the close goes through", func() {
				green()
				policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
				Expect(k8sClient.Update(ctx, policy)).To(Succeed())
				runs := len(git.CheckRuns(cr.HeadSHA))
				_, err := watcher()
				Expect(err).NotTo(HaveOccurred())
				Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
				Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
				expectStaleCheck(runs + 1)
			})

			It("changes nothing else when the check write is refused, and tries it again with the close", func() {
				green()
				checkRefused := fmt.Errorf("status 403: %w", providers.ErrForbidden)
				checkWrites := 0
				events = record.NewFakeRecorder(20)
				refuse = fmt.Errorf("status 403: %w", providers.ErrForbidden)
				git.SetFail(func(op string) error {
					switch op {
					case opClose:
						closes.Add(1)
						return refuse
					case opCreateCheckRun:
						checkWrites++
						return checkRefused
					}
					return nil
				})
				policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
				Expect(k8sClient.Update(ctx, policy)).To(Succeed())
				res, err := watcher()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval))
				Expect(checkWrites).To(BeNumerically(">=", 1))
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
				expectStale(got)
				c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
				Expect(c).NotTo(BeNil())
				Expect(c.Reason).To(Equal(ReasonGitForbidden))
				if rc := got.Status.PullRequest.ReviewCheck; rc != nil {
					Expect(rc.State).NotTo(Equal(remediationv1alpha1.ReviewCheckStale))
				}
				Expect(closes.Load()).To(Equal(int32(1)))

				// The next look tries the check again with the close; once the host takes it, it is recorded.
				written := checkWrites
				_, err = watcher()
				Expect(err).NotTo(HaveOccurred())
				Expect(checkWrites).To(Equal(written + 1))
				Expect(closes.Load()).To(Equal(int32(2)))
				checkRefused = nil
				_, err = watcher()
				Expect(err).NotTo(HaveOccurred())
				Expect(closes.Load()).To(Equal(int32(3)))
				expectStaleCheck(len(git.CheckRuns(cr.HeadSHA)))
			})

			It("fails the check though the Git host never answers a read of it", func() {
				green()
				runs := len(git.CheckRuns(cr.HeadSHA))
				events = record.NewFakeRecorder(20)
				git.SetFail(func(op string) error {
					switch {
					case op == opClose:
						closes.Add(1)
						return fmt.Errorf("status 403: %w", providers.ErrForbidden)
					case op == opReviewCheckRun && staleMark(fetch()) != nil:
						// A list of more runs than the provider reads, say: every read of the stale plan's check fails.
						return fmt.Errorf("check runs: %w", providers.ErrTooLarge)
					}
					return nil
				})
				policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
				Expect(k8sClient.Update(ctx, policy)).To(Succeed())
				_, err := watcher()
				Expect(err).NotTo(HaveOccurred())
				git.SetFail(nil)
				expectStaleCheck(runs + 1)
			})

			It("writes the stale check over a failed run the request does not record as its own", func() {
				green()
				runs := len(git.CheckRuns(cr.HeadSHA))
				// Once the mark is written, and before the cleanup reaches the Git host, a failed run of the App
				// appears on the head that rr does not record (it records Passed): another executor's gate, say.
				injected := false
				git.SetFail(func(op string) error {
					if !injected && staleMark(fetch()) != nil {
						injected = true
						Expect(git.CreateCheckRun(ctx, cr.HeadSHA, providers.CheckRun{Name: providers.CheckRunName,
							Status: providers.CheckRunFailure, Title: "Too many reviews"})).To(Succeed())
					}
					if op == opClose {
						closes.Add(1)
						return fmt.Errorf("status 403: %w", providers.ErrForbidden)
					}
					return nil
				})
				events = record.NewFakeRecorder(20)
				policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
				Expect(k8sClient.Update(ctx, policy)).To(Succeed())
				_, err := watcher()
				Expect(err).NotTo(HaveOccurred())
				Expect(injected).To(BeTrue())
				expectStaleCheck(runs + 2)
			})

			It("does not end Superseded while the check of a closed pull request is not failed yet", func() {
				green()
				checkRefused := fmt.Errorf("status 403: %w", providers.ErrForbidden)
				git.SetFail(func(op string) error {
					if op == opCreateCheckRun {
						return checkRefused
					}
					return nil
				})
				policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
				Expect(k8sClient.Update(ctx, policy)).To(Succeed())
				for range 2 {
					res, err := watcher()
					Expect(err).NotTo(HaveOccurred())
					Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval), "the check is tried again")
					Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
					got := fetch()
					Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "not Superseded yet")
					expectStale(got)
					Expect(got.Status.PullRequest.ReviewCheck.State).To(Equal(remediationv1alpha1.ReviewCheckPassed))
				}

				// Once the Git host takes the failed check, the request records it and ends.
				checkRefused = nil
				runs := len(git.CheckRuns(cr.HeadSHA))
				_, err := watcher()
				Expect(err).NotTo(HaveOccurred())
				expectStaleCheck(runs + 1)
				Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
			})

			// #809: a close that goes through while the check is still pending removes PullRequestNotClosed of an
			// earlier refused close in the same write that records the close, so the end says Shadewright closed it.
			// refusedThenPending makes the plan stale with the close and the check refused, then lets the close
			// through while the check stays refused; it returns the request as stored before that look.
			refusedThenPending := func(closeRefused *atomic.Bool) *remediationv1alpha1.RemediationRequest {
				GinkgoHelper()
				green()
				closeRefused.Store(true)
				git.SetFail(func(op string) error {
					switch op {
					case opClose:
						closes.Add(1)
						if closeRefused.Load() {
							return fmt.Errorf("status 403: %w", providers.ErrForbidden)
						}
					case opCreateCheckRun:
						return fmt.Errorf("status 403: %w", providers.ErrForbidden)
					}
					return nil
				})
				events = record.NewFakeRecorder(20)
				policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
				Expect(k8sClient.Update(ctx, policy)).To(Succeed())
				_, err := watcher()
				Expect(err).NotTo(HaveOccurred())
				Expect(notClosed(fetch())).NotTo(BeNil(), "the refused close is said")
				closeRefused.Store(false)
				return fetch()
			}

			It("records the close while the check is pending after a refused close (#809)", func() {
				closeRefused := atomic.Bool{}
				before := refusedThenPending(&closeRefused)
				res, err := watcher()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval), "the poll interval is kept")
				Expect(changeRequest().State).To(Equal(providers.ChangeRequestClosed))
				got := fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "the check is pending")
				Expect(notClosed(got)).To(BeNil(), "removed by the close that went through")
				opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
				Expect(opened.Reason).To(Equal(ReasonPullRequestClosed), "the close is recorded in the same write")
				Expect(skipOwnMarks.Update(crevent.UpdateEvent{ObjectOld: before, ObjectNew: got})).To(BeFalse(),
					"the write wakes no reconcile of its own, so the poll interval stands")

				git.SetFail(nil)
				_, err = watcher()
				Expect(err).NotTo(HaveOccurred())
				got = fetch()
				Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
				opened = meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
				Expect(opened.Reason).To(Equal(ReasonPullRequestClosed))
				Expect(opened.Message).NotTo(ContainSubstring("not known whether"), "Shadewright closed it")
			})

			It("records the close while the check is pending again after a write that lost to another one (#809)", func() {
				closeRefused := atomic.Bool{}
				refusedThenPending(&closeRefused)
				r := reconciler()
				r.Recorder = events
				r.ReviewerHints = hints
				bumps := atomic.Int32{}
				r.Client = &bumpBeforeStatusUpdate{Client: k8sClient, bump: func(obj client.Object) {
					rr := obj.(*remediationv1alpha1.RemediationRequest)
					if notClosed(rr) != nil || bumps.Add(1) > 1 {
						return
					}
					live := &remediationv1alpha1.RemediationRequest{}
					Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), live)).To(Succeed())
					base := live.DeepCopy()
					live.Annotations = map[string]string{"example.com/edited": "809"}
					Expect(k8sClient.Patch(ctx, live, client.MergeFrom(base))).To(Succeed())
				}}
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval))
				Expect(bumps.Load()).To(BeNumerically(">=", 1), "the write met a change")
				got := fetch()
				Expect(got.Annotations).To(HaveKeyWithValue("example.com/edited", "809"))
				Expect(notClosed(got)).To(BeNil(), "written again after the conflict")
				Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
					To(Equal(ReasonPullRequestClosed))
			})

			It("clears a reason of Approved about the check once it records Stale", func() {
				green()
				checkRefused := fmt.Errorf("status 403: %w", providers.ErrForbidden)
				git.SetFail(func(op string) error {
					switch op {
					case opClose:
						closes.Add(1)
						return fmt.Errorf("status 403: %w", providers.ErrForbidden)
					case opCreateCheckRun:
						return checkRefused
					}
					return nil
				})
				events = record.NewFakeRecorder(20)
				policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
				Expect(k8sClient.Update(ctx, policy)).To(Succeed())
				_, err := watcher()
				Expect(err).NotTo(HaveOccurred())
				// An earlier refusal of the check left its reason on Approved.
				got := fetch()
				setApprovedFalse(got, ReasonReviewCheckRefused, "The Git host refused the check run")
				Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

				checkRefused = nil
				_, err = watcher()
				Expect(err).NotTo(HaveOccurred())
				expectStaleCheck(len(git.CheckRuns(cr.HeadSHA)))
				c := meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionApproved)
				Expect(c).NotTo(BeNil())
				Expect(c.Reason).To(Equal(ReasonWaitingForReview))
			})
		})

		It("records a merge by a person as Merged and keeps the stale reason", func() {
			refused(providers.ErrForbidden)
			_, err := git.Merge(cr.Number)
			Expect(err).NotTo(HaveOccurred())
			_, err = watcher()
			Expect(err).NotTo(HaveOccurred())
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
			expectStale(got)
			Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
				To(BeNil(), "the pull request is merged now")
			Expect(stalePlanMerged(got)).To(BeTrue(), "the outcome after the merge is capped")
		})

		It("keeps the stale reason when a person merges between the look and a retried close", func() {
			refused(providers.ErrForbidden)
			merged := false
			git.SetFail(func(op string) error {
				if op == opClose && !merged {
					merged = true
					_, err := git.Merge(cr.Number)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil
			})
			_, err := watcher()
			Expect(err).To(HaveOccurred(), "the close of a merged pull request is a conflict, retried")
			Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
				NotTo(BeNil(), "a conflict closed nothing")
			git.SetFail(nil)
			_, err = watcher()
			Expect(err).NotTo(HaveOccurred())
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
			expectStale(got)
		})

		// expectClosedUnknown checks the end of a stale plan whose pull request was found closed while the executor
		// kept trying to close it: Superseded with the stale reason, as a close by the executor, PROpened saying it is
		// not known who closed it, and no failure for the circuit breaker.
		expectClosedUnknown := func(got *remediationv1alpha1.RemediationRequest) {
			GinkgoHelper()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
			expectStale(got)
			opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
			Expect(opened).NotTo(BeNil())
			Expect(opened.Reason).To(Equal(ReasonPullRequestClosed))
			Expect(opened.Message).To(ContainSubstring(fmt.Sprintf("#%d is closed", cr.Number)))
			Expect(opened.Message).To(ContainSubstring("not known whether its retried close or a person closed it"))
			Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
				To(BeNil())
			Expect(breakerOutcome(got)).To(Equal(outcomeNone), "no failure for the circuit breaker")
		}

		It("records a close by a person as the end of a stale plan whose pull request is closed, and keeps the stale reason", func() {
			refused(providers.ErrForbidden)
			refuse = nil // a person's close goes through
			Expect(git.CloseChangeRequest(ctx, cr.Number)).To(Succeed())
			refuse = fmt.Errorf("refused: %w", providers.ErrForbidden)
			_, err := watcher()
			Expect(err).NotTo(HaveOccurred())
			expectClosedUnknown(fetch())
		})

		It("posts no comment when someone else pushes to the pull request, and stops with BranchDiverged", func() {
			refused(providers.ErrForbidden)
			_, err := git.Push(freshness.BranchName(fetch()), "README.yaml", []byte("a: 3\n"))
			Expect(err).NotTo(HaveOccurred())
			_, err = watcher()
			Expect(err).NotTo(HaveOccurred())
			Expect(git.Comments(cr.Number)).To(BeEmpty(), "nothing but the close is written to a stale plan's pull request")
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
			Expect(c).NotTo(BeNil())
			Expect(c.Reason).To(Equal(ReasonBranchDiverged))
			Expect(c.Message).To(ContainSubstring("without a comment"))
			Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen), "left open for a person")
		})

		It("ends Superseded once a retried close goes through", func() {
			refused(providers.ErrRejected)
			refuse = nil
			supersedes(freshness.ReasonPolicyChanged)
			Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
				To(BeNil())
		})

		It("keeps the refusal across a failure worth a retry, so a person's merge then never ends Succeeded", func() {
			refused(providers.ErrForbidden)
			// One retried close fails with a server error and closes nothing.
			refuse = &providers.RetryableError{Err: errors.New("bad gateway")}
			_, err := watcher()
			Expect(err).To(HaveOccurred(), "retried")
			got := fetch()
			Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
				NotTo(BeNil(), "a failed call never clears the refusal")
			expectStale(got)
			// A person merges; the request is Merged with the mark, and its outcome is capped.
			_, err = git.Merge(cr.Number)
			Expect(err).NotTo(HaveOccurred())
			_, err = watcher()
			Expect(err).NotTo(HaveOccurred())
			got = fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
			expectStale(got)
			// Without a delivery check the rollout is not followed, so Merged is the final word: capped.
			_, err = watcher()
			Expect(err).NotTo(HaveOccurred())
			got = fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseInconclusive))
			Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionVerified).Reason).
				To(Equal(ReasonStalePlanMerged))
		})

		It("never takes a pull request found closed after a close whose answer was lost as a person's rejection", func() {
			refused(providers.ErrForbidden)
			// The close takes effect, but its answer is a server error.
			refuse = nil
			git.SetAfter(func(op string) error {
				if op == opClose {
					return &providers.RetryableError{Err: errors.New("bad gateway")}
				}
				return nil
			})
			DeferCleanup(func() { git.SetAfter(nil) })
			_, err := watcher()
			Expect(err).To(HaveOccurred(), "retried")
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)).
				NotTo(BeNil(), "a failed call never clears the refusal")
			git.SetAfter(nil)
			_, err = watcher()
			Expect(err).NotTo(HaveOccurred())
			got := fetch()
			if approved := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved); approved != nil {
				Expect(approved.Reason).NotTo(Equal(ReasonChangeRequestClosed), "the executor's own close may have closed it")
			}
			expectClosedUnknown(got)
		})
	})

	It("keeps watching the pull request when the Git host refuses the freshness check's reads", func() {
		git.SetFail(func(op string) error {
			if op == opBranchHead {
				return fmt.Errorf("status 403 from https://github.com/example/fleet: %w", providers.ErrForbidden)
			}
			return nil
		})
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval), "the pull request is looked at again at the next poll")
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval), "a refusal does not end the watch")
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionUnknown))
		Expect(c.Reason).To(Equal(ReasonGitForbidden))
		Expect(c.Message).NotTo(ContainSubstring("https://"))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))

		// A repeated refusal writes nothing new; a merge while it lasts is still recorded.
		before := fetch().ResourceVersion
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().ResourceVersion).To(Equal(before))
		_, err = git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
	})

	It("clears the refusal once the freshness check passes again", func() {
		refuse := true
		git.SetFail(func(op string) error {
			if op == opBranchHead && refuse {
				return fmt.Errorf("refused: %w", providers.ErrRejected)
			}
			return nil
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPlanFresh)).NotTo(BeNil())
		refuse = false
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)).To(BeNil())
	})

	It("does not touch the change request of a request that is not stale while writes are paused", func() {
		settings.Spec.Paused = new(true)
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(freshRecheckDelay))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		Expect(reconciler().freshErr(ctx, fetch())).To(MatchError(freshness.ErrPaused))
	})

	It("fails closed in the freshness check when no Git provider is configured", func() {
		r := reconciler()
		r.OpenRepository = nil
		Expect(r.freshErr(ctx, fetch())).NotTo(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
	})

	// other opens a pull request with the same number in another repository, which must never be touched.
	otherRepo := func(url string) *fakegit.Git {
		other := fakegit.NewGit("main", map[string][]byte{sourcePath: manifest("256Mi")})
		head, err := other.BranchHead(ctx, "main")
		Expect(err).NotTo(HaveOccurred())
		Expect(other.CreateBranch(ctx, "unrelated", head)).To(Succeed())
		_, err = other.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: "unrelated", BaseBranch: "main", Title: "u"})
		Expect(err).NotTo(HaveOccurred())
		other.SetFail(func(op string) error {
			if op == opClose {
				closes.Add(1)
			}
			return nil
		})
		byURL[url] = other
		return other
	}

	It("closes nothing when the settings entry now points to another repository", func() {
		const moved = "https://github.com/example/moved"
		other := otherRepo(moved)
		settings.Spec.Repositories[0].URL = moved
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())

		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal("RepositoryUnavailable"))
		Expect(closes.Load()).To(BeZero())
		Expect(other.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen), "the unrelated pull request stays open")
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		stillOpen()
	})

	It("ends in NeedsIntervention, without closing or retrying, when the repository settings cannot be used", func() {
		r := reconciler()
		r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return nil, fmt.Errorf("https://x-access-token:ghp_secret@github.com/example/fleet: %w", providers.ErrInvalidConfig)
		}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal("RepositoryUnusable"))
		Expect(c.Message).NotTo(ContainSubstring("ghp_secret"))
		Expect(c.Message).NotTo(ContainSubstring("github.com"))
		Expect(closes.Load()).To(BeZero())
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		stillOpen()
	})

	It("closes nothing when the repository entry now names another target branch", func() {
		settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal("TargetBranchChanged"))
		Expect(closes.Load()).To(BeZero())
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		stillOpen()
	})

	// #754: the target branch changes in the settings after the pull request was opened, and a person merges it
	// before the executor looks. The merge is read on the recorded branch and recorded as a stale merge, with its
	// receipt, never ended NeedsIntervention as a failure for the circuit breaker.
	It("records a merge after the target branch changed as a merged stale plan, with its receipt (#754)", func() {
		settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		sha, err := git.Merge(cr.Number)
		Expect(err).NotTo(HaveOccurred())
		var opened []remediationv1alpha1.GitBranch
		r := reconciler()
		open := r.OpenRepository
		r.OpenRepository = func(ctx context.Context, repo remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			opened = append(opened, repo.TargetBranch)
			return open(ctx, repo)
		}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(opened).To(ConsistOf(remediationv1alpha1.GitBranch(mainBranch)), "read on the recorded branch only")
		Expect(got.Status.PullRequest.MergeCommitSHA).To(Equal(sha))
		Expect(got.Status.MergeReceipt).NotTo(BeNil())
		Expect(got.Status.MergeReceipt.TargetBranch).To(Equal(remediationv1alpha1.GitBranch(mainBranch)))
		Expect(got.Status.MergeReceipt.PullRequest).To(Equal(cr.Number))
		Expect(got.Status.MergeReceipt.MergeCommitSHA).To(Equal(sha))
		c := planFresh()
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(freshness.ReasonSettingsChanged))
		Expect(stalePlanMerged(got)).To(BeTrue())
		Expect(breakerOutcome(got)).To(Equal(outcomeNone))
		Expect(closes.Load()).To(BeZero())

		done, err := reconciler().capStaleSuccess(ctx, got)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		ended := fetch()
		Expect(ended.Status.Phase).To(Equal(remediationv1alpha1.PhaseInconclusive))
		verified := meta.FindStatusCondition(ended.Status.Conditions, remediationv1alpha1.ConditionVerified)
		Expect(verified).NotTo(BeNil())
		Expect(verified.Reason).To(Equal(ReasonStalePlanMerged))
		Expect(breakerOutcome(ended)).To(Equal(outcomeNone))
	})

	// A failed read of the pull request after the target branch changed decides nothing: it may have been merged.
	It("retries, and ends nothing, while the pull request cannot be read after the target branch changed (#754)", func() {
		settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		var fail atomic.Bool
		fail.Store(true)
		git.SetFail(func(op string) error {
			switch {
			case op == opClose:
				closes.Add(1)
			case fail.Load() && (op == opGet || op == opList || op == opFind):
				return &providers.RetryableError{Err: errors.New("connection reset")}
			}
			return nil
		})
		for range 2 {
			_, err := reconcileOnce()
			Expect(err).To(HaveOccurred())
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
			Expect(planFresh()).To(BeNil(), "no full check on an unread pull request")
		}
		Expect(closes.Load()).To(BeZero())

		// Read again, open: as before #754.
		fail.Store(false)
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(planFresh().Reason).To(Equal("TargetBranchChanged"))
		Expect(closes.Load()).To(BeZero())
		stillOpen()
	})

	// unreadable returns condition PullRequestUnreadable of the request as stored.
	unreadable := func() *metav1.Condition {
		return meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	}

	// A read that keeps failing after the target branch changed is retried without an end, so a person must see it:
	// condition PullRequestUnreadable on every failed read, one Warning Event, an Info log line, and the condition
	// removed by the read that works.
	It("shows a pull request it cannot read after the target branch changed, and clears it once read (#754)", func() {
		settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		var fail atomic.Bool
		fail.Store(true)
		git.SetFail(func(op string) error {
			if fail.Load() && (op == opGet || op == opList || op == opFind) {
				return &providers.RetryableError{Err: errors.New("bad gateway")}
			}
			return nil
		})
		recorder := record.NewFakeRecorder(10)
		r := reconciler()
		r.Recorder = recorder
		logCtx, out := infoLog()
		for range 2 {
			_, err := r.Reconcile(logCtx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
			Expect(err).To(HaveOccurred())
			c := unreadable()
			Expect(c).NotTo(BeNil())
			Expect(c.Status).To(Equal(metav1.ConditionTrue))
			Expect(c.Reason).To(Equal(reasonTargetBranchChanged))
			Expect(c.Message).To(ContainSubstring("could not be read on the branch the plan was made for"))
			Expect(c.Message).NotTo(ContainSubstring("bad gateway"), "no text of the Git host")
		}
		Expect(recorder.Events).To(HaveLen(1), "one Warning Event per transition, not per retry")
		Expect(<-recorder.Events).To(HavePrefix(corev1.EventTypeWarning + " " + EventPullRequestUnreadable))
		Expect(out.String()).To(ContainSubstring("Could not read the pull request of RemediationRequest on its "+
			"recorded target branch"), "logged at Info")

		fail.Store(false)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		Expect(unreadable()).To(BeNil(), "removed by the read that works")
		Expect(<-recorder.Events).To(HavePrefix(corev1.EventTypeNormal + " " + EventPullRequestReadable))
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
	})

	It("shows a recorded repository it cannot open after the target branch changed, and retries (#754)", func() {
		settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		r := reconciler()
		r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return nil, &providers.RetryableError{Err: errors.New("token expired")}
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).To(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		c := unreadable()
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal(reasonTargetBranchChanged))
		Expect(c.Message).NotTo(ContainSubstring("token expired"))
	})

	It("waits at most the full recheck's period when the host asks for a longer delay after the target branch "+
		"changed (#754)", func() {
		settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		git.SetFail(func(op string) error {
			if op == opGet || op == opList || op == opFind {
				return &providers.RetryableError{Err: errors.New("rate limited"), RetryAfter: time.Hour}
			}
			return nil
		})
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(freshRecheckInterval))
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
	})

	It("removes WritesPaused once writes are allowed while the pull request cannot be read after the target "+
		"branch changed (#754)", func() {
		settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		got := fetch()
		meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
			Type: remediationv1alpha1.ConditionWritesPaused, Status: metav1.ConditionTrue,
			Reason: ReasonWritesPaused, Message: messageWritesPaused, ObservedGeneration: got.Generation,
		})
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		git.SetFail(func(op string) error {
			if op == opGet || op == opList || op == opFind {
				return &providers.RetryableError{Err: errors.New("bad gateway")}
			}
			return nil
		})
		_, err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(meta.FindStatusCondition(fetch().Status.Conditions, remediationv1alpha1.ConditionWritesPaused)).To(BeNil())
	})

	It("logs that a merge was not looked up when the recorded repository can never be opened after the target "+
		"branch changed (#754)", func() {
		settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		r := reconciler()
		r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return nil, fmt.Errorf("no signer: %w", providers.ErrInvalidConfig)
		}
		logCtx, out := infoLog()
		_, err := r.Reconcile(logCtx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(out.String()).To(ContainSubstring("a merge of its pull request, if any, was not looked up"))
	})

	It("ends in NeedsIntervention, without closing or retrying, when a stale plan's repository settings cannot be used", func() {
		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		r := reconciler()
		r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return nil, fmt.Errorf("https://x-access-token:ghp_secret@github.com/example/fleet: %w", providers.ErrInvalidConfig)
		}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal("RepositoryUnusable"))
		Expect(c.Message).NotTo(ContainSubstring("ghp_secret"))
		Expect(closes.Load()).To(BeZero())
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		stillOpen()
	})

	It("closes nothing and does not block when the repository is removed from the settings", func() {
		settings.Spec.Repositories = nil
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(closes.Load()).To(BeZero())
		stillOpen()
	})

	It("closes in the recorded repository when the entry is only renamed", func() {
		settings.Spec.Repositories[0].Name = "renamed"
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		supersedes(freshness.ReasonSettingsChanged)
		Expect(closes.Load()).To(Equal(int32(1)))
	})

	It("does nothing while writes are paused, even for a stale plan", func() {
		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		settings.Spec.Paused = new(true)
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(freshRecheckDelay))
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(closes.Load()).To(BeZero())
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		Expect(reconciler().freshErr(ctx, fetch())).To(MatchError(freshness.ErrPaused))
	})

	It("does not close when writes are paused between the check and the close", func() {
		// The check sees a stale plan on an unpaused read; the close reads the settings again.
		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		r := reconciler()
		calls := 0
		r.APIReader = readerFunc(func(ctx context.Context, key client.ObjectKey, obj client.Object) error {
			if _, ok := obj.(*remediationv1alpha1.RemediationSettings); ok {
				calls++
				if calls > 1 {
					settings.Spec.Paused = new(true)
					Expect(k8sClient.Update(ctx, settings)).To(Succeed())
				}
			}
			return k8sClient.Get(ctx, key, obj)
		})
		Expect(r.freshErr(ctx, fetch())).To(MatchError(freshness.ErrPaused))
		Expect(closes.Load()).To(BeZero())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
	})

	It("keeps credentials out of the error when the cleanup call fails", func() {
		const secret = "ghs_supersecrettoken"
		_, err := git.Push(mainBranch, sourcePath, manifest("512Mi"))
		Expect(err).NotTo(HaveOccurred())
		git.SetFail(func(op string) error {
			if op == opClose {
				return &providers.RetryableError{RetryAfter: 9 * time.Second,
					Err: fmt.Errorf("PATCH https://x-access-token:%s@github.com/example/fleet: 502", secret)}
			}
			return nil
		})
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(9*time.Second), "the delay the host asked for is honoured")

		err = reconciler().freshErr(ctx, fetch())
		Expect(err).To(HaveOccurred())
		for e := err; e != nil; e = errors.Unwrap(e) {
			Expect(e.Error()).NotTo(ContainSubstring(secret))
		}
		Expect(errors.Is(err, providers.ErrRetryable)).To(BeTrue())
	})

	It("honours the delay a Git host asks for while checking", func() {
		git.SetFail(func(op string) error {
			if op == opBranchHead {
				return &providers.RetryableError{RetryAfter: 7 * time.Second, Err: errors.New("rate limited")}
			}
			return nil
		})
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(7 * time.Second))
	})

	It("schedules the next check of a fresh plan", func() {
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(DefaultPullRequestPollInterval), "its pull request is looked at at the next poll")
	})

	It("does not schedule anything for a superseded request", func() {
		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{}))
	})
})

// readerFunc adapts a function to the part of client.Reader the controller uses.
type readerFunc func(ctx context.Context, key client.ObjectKey, obj client.Object) error

func (f readerFunc) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	return f(ctx, key, obj)
}

func (readerFunc) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("not used")
}

// sourceFunc adapts a function to SourceResolver.
type sourceFunc func(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error)

func (f sourceFunc) Resolve(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error) {
	return f(ctx, in)
}

// freshErr runs the freshness check and returns only its error.
func (r *RemediationRequestReconciler) freshErr(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	_, err := r.ensureFresh(ctx, rr)
	return err
}

// bentAnswer answers GetChangeRequest with what the fake holds, changed by bend: a Git host whose answer does not
// match the question.
type bentAnswer struct {
	*fakegit.Git
	bend func(*providers.ChangeRequest)
}

func (b *bentAnswer) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	cr, err := b.Git.GetChangeRequest(ctx, n)
	if err == nil {
		b.bend(&cr)
	}
	return cr, err
}

// bumpBeforeStatusUpdate is a client whose status updates first call bump with the object about to be written, so
// a test can change the stored object between the reconcile's read and its write.
type bumpBeforeStatusUpdate struct {
	client.Client
	bump func(client.Object)
}

func (c *bumpBeforeStatusUpdate) Status() client.SubResourceWriter {
	return &bumpingStatus{SubResourceWriter: c.Client.Status(), bump: c.bump}
}

type bumpingStatus struct {
	client.SubResourceWriter
	bump func(client.Object)
}

func (s *bumpingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	s.bump(obj)
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

// armedRequestReader is a reader whose reads of a RemediationRequest, once armed, fail with fail or answer with the
// stored request changed by edit, so a test can change what stillOwns reads without a write to the request.
type armedRequestReader struct {
	client.Reader
	armed *atomic.Bool
	fail  error
	edit  func(*remediationv1alpha1.RemediationRequest)
}

func (a *armedRequestReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	rr, ok := obj.(*remediationv1alpha1.RemediationRequest)
	if !ok || !a.armed.Load() {
		return a.Reader.Get(ctx, key, obj, opts...)
	}
	if a.fail != nil {
		return a.fail
	}
	if err := a.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	a.edit(rr)
	return nil
}
