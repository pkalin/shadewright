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
	"sync"
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
	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	opCommit = "CommitFile"
	opOpen   = "OpenChangeRequest"
)

var _ = Describe("RemediationRequest pull request step", func() {
	ctx := context.Background()
	var (
		seq        int
		deployment *appsv1.Deployment
		policy     *remediationv1alpha1.RemediationPolicy
		settings   *remediationv1alpha1.RemediationSettings
		request    *remediationv1alpha1.RemediationRequest
		git        *fakegit.Git
		clock      time.Time
		build      remediationv1alpha1.GitDirPath
	)

	manifest := func(limit string) []byte {
		return deploymentManifest(deployment.Name, deployment.Namespace, workerContainer, limit)
	}
	reconciler := func() *RemediationRequestReconciler {
		return &RemediationRequestReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Source: sourceFunc(func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
				res := resolved()
				res.Source.BaseCommit, _ = git.BranchHead(ctx, mainBranch)
				res.Source.BuildPath = build
				return res, nil
			}),
			OpenRepository: func(_ context.Context, repo remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
				if repo.URL != settings.Spec.Repositories[0].URL {
					return nil, errors.New("unknown repository")
				}
				return git, nil
			},
			Now: func() time.Time { return clock },
		}
	}
	key := func() reconcile.Request {
		return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)}
	}
	// reconcileOnce runs one reconcile with a new reconciler, as after a restart.
	reconcileOnce := func() error {
		_, err := reconciler().Reconcile(ctx, key())
		return err
	}
	fetch := func() *remediationv1alpha1.RemediationRequest {
		got := &remediationv1alpha1.RemediationRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), got)).To(Succeed())
		return got
	}
	// drive reconciles until the request reaches phase or stops changing, ignoring retryable errors, as the
	// controller's work queue would.
	drive := func(phase remediationv1alpha1.RemediationPhase) *remediationv1alpha1.RemediationRequest {
		for range 20 {
			_ = reconcileOnce()
			if got := fetch(); got.Status.Phase == phase {
				return got
			}
		}
		return fetch()
	}
	openRequests := func() []providers.ChangeRequest {
		var out []providers.ChangeRequest
		for _, cr := range git.ChangeRequests() {
			if cr.HeadBranch == freshness.BranchName(fetch()) {
				out = append(out, cr)
			}
		}
		return out
	}

	BeforeEach(func() {
		seq++
		clock = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
		build = buildPath
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fx.TargetNamespace}})
		Expect(client.IgnoreAlreadyExists(err)).To(Succeed())

		deployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: fx.TargetNamespace, Name: fmt.Sprintf("pr-%d", seq)},
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
		policy = fx.RemediationPolicy(fmt.Sprintf("pr-policy-%d", seq))
		policy.Spec.Actions = []remediationv1alpha1.PolicyAction{fx.MemoryLimitPolicyAction()}
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 100
		policy.Spec.RequestNamespaces = []remediationv1alpha1.Name{requestNamespace}
		policy.Spec.GitOps.AllowedPaths = []remediationv1alpha1.GitPathPattern{appsGlob}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		settings = fx.RemediationSettings()
		settings.Spec.Repositories[0].TargetBranch = mainBranch
		Expect(k8sClient.Create(ctx, settings)).To(Succeed())
		request = fx.MemoryLimitRequest(requestNamespace, fmt.Sprintf("pr-rr-%d", seq))
		request.Spec.PolicyRef = remediationv1alpha1.Name(policy.Name)
		request.Spec.TargetRef.Name = deployment.Name
		request.Spec.TargetRef.UID = remediationv1alpha1.UID(deployment.UID)
		Expect(k8sClient.Create(ctx, request)).To(Succeed())

		git = fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: manifest("256Mi"), buildKustomize: buildFile, "README.yaml": []byte("a: 1\n")})

		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
	})
	AfterEach(func() {
		got := &remediationv1alpha1.RemediationRequest{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(request), got); err == nil &&
			controllerutil.ContainsFinalizer(got, FinalizerName) {
			base := got.DeepCopy()
			controllerutil.RemoveFinalizer(got, FinalizerName)
			Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
		}
		for _, o := range []client.Object{request, policy, settings, deployment} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: SettingsName},
				&remediationv1alpha1.RemediationSettings{}))
		}).Should(BeTrue())
	})

	It("turns an allowed request into one branch, one commit and one pull request", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(controllerutil.ContainsFinalizer(got, FinalizerName)).To(BeTrue())
		Expect(got.Status.Deadline).To(BeNil(), "the deadline of the step is cleared once the pull request is open")
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)).To(BeTrue())
		approved := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(approved).NotTo(BeNil())
		Expect(approved.Reason).To(Equal(ReasonWaitingForReview))

		crs := openRequests()
		Expect(crs).To(HaveLen(1))
		cr := crs[0]
		Expect(cr.State).To(Equal(providers.ChangeRequestOpen))
		Expect(cr.BaseBranch).To(Equal(mainBranch))
		Expect(cr.Title).To(HavePrefix("fix: set the memory limit of container worker of "))
		Expect(cr.Body).To(ContainSubstring(fmt.Sprintf("<!-- shadewright request-uid=%s plan-digest=%s -->",
			got.UID, got.Status.PlanDigest)))
		for _, part := range []string{"**Change:**", "**Scope:**", "**Evidence:**", "**Expected effect:**",
			"**Risk:**", "**Rollback:**"} {
			Expect(cr.Body).To(ContainSubstring(part))
		}
		Expect(got.Status.PullRequest).To(Equal(&remediationv1alpha1.PullRequestStatus{
			Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA}))

		file, err := git.ReadFile(ctx, cr.HeadSHA, sourcePath)
		Expect(err).NotTo(HaveOccurred())
		Expect(file.Data).To(Equal(manifest("384Mi")), "one value changed, nothing else")
		main, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		onMain, err := git.ReadFile(ctx, main, sourcePath)
		Expect(err).NotTo(HaveOccurred())
		Expect(onMain.Data).To(Equal(manifest("256Mi")), "the target branch is never written")

		// More reconciles change nothing.
		before := fetch().ResourceVersion
		for range 3 {
			Expect(reconcileOnce()).To(Succeed())
		}
		Expect(fetch().ResourceVersion).To(Equal(before))
		Expect(openRequests()).To(HaveLen(1))
	})

	It("finds the existing work after a restart between any two steps", func() {
		// Every Git write fails once after it took effect, as if the executor crashed before it could go on.
		crashed := map[string]bool{}
		git.SetAfter(func(op string) error {
			if (op == "CreateBranch" || op == opCommit || op == opOpen) && !crashed[op] {
				crashed[op] = true
				return errors.New("crash after the write")
			}
			return nil
		})
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(crashed).To(HaveLen(3))
		Expect(openRequests()).To(HaveLen(1))
		head, err := git.BranchHead(ctx, freshness.BranchName(got))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status.PullRequest.HeadSHA).To(Equal(head))
	})

	It("adopts a pull request whose status write was lost", func() {
		// Drive up to the open, then lose the status write that records the pull request.
		failStatus := true
		r := reconciler()
		r.Client = &statusFailer{Client: k8sClient, fail: func() bool { return failStatus && len(git.ChangeRequests()) > 0 }}
		for range 10 {
			_, _ = r.Reconcile(ctx, key())
			if len(openRequests()) == 1 {
				break
			}
		}
		Expect(openRequests()).To(HaveLen(1))
		Expect(fetch().Status.PullRequest).To(BeNil())
		failStatus = false
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.PullRequest).NotTo(BeNil())
		Expect(got.Status.PullRequest.Number).To(Equal(openRequests()[0].Number))
		Expect(openRequests()).To(HaveLen(1))
	})

	It("opens one pull request when reconciled concurrently", func() {
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				defer GinkgoRecover()
				for range 15 {
					_ = reconcileOnce()
				}
			})
		}
		wg.Wait()
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseAwaitingApproval))
		Expect(openRequests()).To(HaveLen(1))
		file, err := git.ReadFile(ctx, got.Status.PullRequest.HeadSHA, sourcePath)
		Expect(err).NotTo(HaveOccurred())
		Expect(file.Data).To(Equal(manifest("384Mi")))
	})

	It("stops automation when someone else pushes to the branch", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved)
		Expect(cond.Reason).To(Equal(ReasonHeadMoved))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)).
			To(BeTrue(), "the pull request is still open")
		Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen), "the pull request is left for a person")
	})

	It("stops before the commit when the change renders into another object too", func() {
		// The patched file is a base that a second overlay builds under another name: the change would reach
		// b-<name> as well, which the plan does not cover.
		_, err := git.Push(mainBranch, "apps/kustomization.yaml", []byte(
			"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - orders\n  - copy\n"))
		Expect(err).NotTo(HaveOccurred())
		_, err = git.Push(mainBranch, "apps/copy/kustomization.yaml", []byte(
			"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamePrefix: b-\nresources:\n  - ../orders\n"))
		Expect(err).NotTo(HaveOccurred())
		// The mapping now names the parent directory as the build, as it would for a Kustomization at apps.
		build = "apps"
		got := fetch()
		got.Status.Source.BuildPath = build
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		drive(remediationv1alpha1.PhaseNeedsIntervention)
		got = fetch()
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(string(semdiff.ReasonOtherObjectChanged)))
		head, err := git.BranchHead(ctx, freshness.BranchName(got))
		Expect(err).NotTo(HaveOccurred())
		base, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		Expect(head).To(Equal(base), "nothing was committed")
		Expect(openRequests()).To(BeEmpty())
	})

	It("does not commit on a branch that holds something else", func() {
		// The branch exists with another value in the file, as if someone created it by hand.
		Expect(reconcileOnce()).To(Succeed()) // deadline
		Expect(reconcileOnce()).To(Succeed()) // finalizer
		base, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		branch := freshness.BranchName(fetch())
		Expect(git.CreateBranch(ctx, branch, base)).To(Succeed())
		_, err = git.Push(branch, sourcePath, manifest("1Gi"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(ReasonBranchDiverged))
		Expect(openRequests()).To(BeEmpty())
	})

	It("leaves a pull request without its marker alone", func() {
		Expect(reconcileOnce()).To(Succeed()) // deadline
		Expect(reconcileOnce()).To(Succeed()) // finalizer
		base, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		branch := freshness.BranchName(fetch())
		Expect(git.CreateBranch(ctx, branch, base)).To(Succeed())
		_, err = git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: mainBranch, Title: "someone"})
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened).Reason).
			To(Equal(ReasonForeignChangeRequest))
		Expect(openRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
	})

	It("records a merge by a person as Merged, before the freshness check sees the changed file", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Merge(got.Status.PullRequest.Number)
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, remediationv1alpha1.ConditionApproved)).To(BeTrue())
		Expect(reconcileOnce()).To(Succeed(), "a merged request takes no further step yet")
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
	})

	It("stops when a person closes the pull request without a merge", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(git.CloseChangeRequest(ctx, got.Status.PullRequest.Number)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		got = fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionApproved).Reason).
			To(Equal(ReasonChangeRequestClosed))
	})

	It("expires a request whose freshness check keeps failing", func() {
		Expect(reconcileOnce()).To(Succeed()) // deadline
		Expect(reconcileOnce()).To(Succeed()) // finalizer
		Expect(reconcileOnce()).To(Succeed()) // branch
		git.SetFail(func(op string) error {
			if op == opBranchHead { // the freshness check reads it; expiring does not
				return providers.ErrRetryable
			}
			return nil
		})
		Expect(reconcileOnce()).NotTo(Succeed(), "a failing freshness check is retried")
		clock = clock.Add(DefaultPullRequestTimeout + time.Minute)
		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
	})

	It("expires a request that did not open its pull request before the deadline", func() {
		Expect(reconcileOnce()).To(Succeed()) // deadline
		Expect(reconcileOnce()).To(Succeed()) // finalizer
		Expect(reconcileOnce()).To(Succeed()) // branch
		clock = clock.Add(DefaultPullRequestTimeout + time.Minute)
		Expect(reconcileOnce()).To(Succeed())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseExpired))
		Expect(openRequests()).To(BeEmpty())
	})

	It("closes the pull request when the request is deleted, and only then lets it go", func() {
		drive(remediationv1alpha1.PhaseAwaitingApproval)
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(fetch().DeletionTimestamp).NotTo(BeNil(), "the finalizer holds the request")
		Expect(reconcileOnce()).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(request),
				&remediationv1alpha1.RemediationRequest{}))
		}).Should(BeTrue())
		crs := git.ChangeRequests()
		Expect(crs).To(HaveLen(1))
		Expect(crs[0].State).To(Equal(providers.ChangeRequestClosed))
		got := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(deployment), got)).To(Succeed())
		Expect(got.DeletionTimestamp).To(BeNil(), "deleting a request never touches the cluster")
	})

	gone := func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(request),
			&remediationv1alpha1.RemediationRequest{}))
	}

	It("closes its own pull request on deletion after automation stopped", func() {
		got := drive(remediationv1alpha1.PhaseAwaitingApproval)
		_, err := git.Push(freshness.BranchName(got), "README.yaml", []byte("a: 3\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
	})

	It("never closes a pull request without its marker, also on deletion", func() {
		Expect(reconcileOnce()).To(Succeed()) // deadline
		Expect(reconcileOnce()).To(Succeed()) // finalizer
		base, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		branch := freshness.BranchName(fetch())
		Expect(git.CreateBranch(ctx, branch, base)).To(Succeed())
		_, err = git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: mainBranch, Title: "someone"})
		Expect(err).NotTo(HaveOccurred())
		Expect(reconcileOnce()).To(Succeed()) // ForeignChangeRequest
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
	})

	It("waits with the deletion while writes are paused", func() {
		drive(remediationv1alpha1.PhaseAwaitingApproval)
		live := &remediationv1alpha1.RemediationSettings{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: SettingsName}, live)).To(Succeed())
		live.Spec.Paused = new(true)
		Expect(k8sClient.Update(ctx, live)).To(Succeed())
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed(), "a pause is a wait, not an error")
		Consistently(gone, "300ms").Should(BeFalse())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestOpen))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: SettingsName}, live)).To(Succeed())
		live.Spec.Paused = new(false)
		Expect(k8sClient.Update(ctx, live)).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
		Eventually(gone).Should(BeTrue())
		Expect(git.ChangeRequests()[0].State).To(Equal(providers.ChangeRequestClosed))
	})

	It("writes nothing to Git while writes are paused", func() {
		live := &remediationv1alpha1.RemediationSettings{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: SettingsName}, live)).To(Succeed())
		live.Spec.Paused = new(true)
		Expect(k8sClient.Update(ctx, live)).To(Succeed())
		for range 5 {
			_ = reconcileOnce()
		}
		_, err := git.BranchHead(ctx, freshness.BranchName(fetch()))
		Expect(errors.Is(err, providers.ErrNotFound)).To(BeTrue())
		Expect(openRequests()).To(BeEmpty())
	})
})

// statusFailer fails status writes while fail returns true, as a lost write after a successful Git call.
type statusFailer struct {
	client.Client
	fail func() bool
}

func (s *statusFailer) Status() client.SubResourceWriter {
	return &failingStatus{SubResourceWriter: s.Client.Status(), fail: s.fail}
}

type failingStatus struct {
	client.SubResourceWriter
	fail func() bool
}

func (f *failingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if f.fail() {
		return errors.New("status write lost")
	}
	return f.SubResourceWriter.Update(ctx, obj, opts...)
}

var _ = Describe("Pull request body", func() {
	It("carries no secret and keeps the marker exact", func() {
		rr := fx.MemoryLimitRequest(requestNamespace, "body")
		rr.UID = "11111111-1111-4111-8111-111111111111"
		// Names are validated by the schema; the body is redacted anyway, so a token that slipped through a
		// future field never reaches the Git host.
		rr.Spec.IncidentRef.Name = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
		rr.Status.PlanDigest = "sha256:" + strings.Repeat("a", 64)
		rr.Status.PolicyRevision = "p/1"
		rr.Status.Source = &remediationv1alpha1.ResolvedSource{Path: sourcePath, TargetBranch: mainBranch}
		body := pullRequestBody(rr)
		Expect(body).NotTo(ContainSubstring("eyJhbGciOiJIUzI1NiJ9"))
		Expect(body).To(ContainSubstring("[REDACTED"))
		Expect(hasMarker(body, rr)).To(BeTrue())
		Expect(body).NotTo(ContainSubstring("https://"), "no repository URL in the body")

		other := rr.DeepCopy()
		other.Status.PlanDigest = "sha256:" + strings.Repeat("b", 64)
		Expect(hasMarker(body, other)).To(BeFalse(), "a marker for another plan is not this plan's")
	})
})
