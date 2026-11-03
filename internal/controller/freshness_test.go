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
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	opBranchHead    = "BranchHead"
	workerImage     = "example.invalid/worker:1"
	appsGlob        = "apps/**/*.yaml"
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
			Client: k8sClient, Scheme: k8sClient.Scheme(), Source: sourceFunc(func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
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
		cr, err = git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: mainBranch, Title: "t"})
		Expect(err).NotTo(HaveOccurred())
		got.Status.Phase = remediationv1alpha1.PhaseAwaitingApproval
		got.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{
			Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA,
		}
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
	})
	AfterEach(func() {
		for _, o := range []client.Object{request, policy, settings, deployment} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: SettingsName},
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
		Expect(reconciler().EnsureFresh(ctx, fetch())).To(Succeed())
	})

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
	}

	It("closes the change request once when the policy is edited, and not again", func() {
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 50
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		supersedes(freshness.ReasonPolicyChanged)
		Expect(closes.Load()).To(Equal(int32(1)))

		// A repeated reconcile, and a repeated EnsureFresh, find the request decided and call nothing.
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(reconciler().EnsureFresh(ctx, fetch())).To(MatchError(ErrSuperseded))
		Expect(closes.Load()).To(Equal(int32(1)))
	})

	It("supersedes when the policy is deleted", func() {
		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		supersedes(freshness.ReasonPolicyChanged)
		Expect(closes.Load()).To(Equal(int32(1)))
	})

	It("supersedes when the repository installation changes in the settings", func() {
		settings.Spec.Repositories[0].GitHub.InstallationID = 2
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		// The installation is part of the repository identity: the old one is not configured any more.
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(closes.Load()).To(BeZero())
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
		Expect(closes.Load()).To(Equal(int32(2)), "the retry closes an already closed change request, a no-op")
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
		Expect(reconciler().EnsureFresh(ctx, fetch())).NotTo(Succeed())
	})

	It("does not touch the change request of a request that is not stale while writes are paused", func() {
		settings.Spec.Paused = new(true)
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(freshRecheckDelay))
		Expect(changeRequest().State).To(Equal(providers.ChangeRequestOpen))
		Expect(reconciler().EnsureFresh(ctx, fetch())).To(MatchError(freshness.ErrPaused))
	})

	It("fails closed in EnsureFresh when no Git provider is configured", func() {
		r := reconciler()
		r.OpenRepository = nil
		Expect(r.EnsureFresh(ctx, fetch())).NotTo(Succeed())
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
	})

	It("closes nothing when the repository entry now names another target branch", func() {
		settings.Spec.Repositories[0].TargetBranch = "release"
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
	})

	It("closes nothing and does not block when the repository is removed from the settings", func() {
		settings.Spec.Repositories = nil
		Expect(k8sClient.Update(ctx, settings)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(closes.Load()).To(BeZero())
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
		Expect(reconciler().EnsureFresh(ctx, fetch())).To(MatchError(freshness.ErrPaused))
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
		Expect(r.EnsureFresh(ctx, fetch())).To(MatchError(freshness.ErrPaused))
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

		err = reconciler().EnsureFresh(ctx, fetch())
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
		Expect(res.RequeueAfter).To(Equal(freshRecheckInterval))
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
