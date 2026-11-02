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
		closes     atomic.Int32
		cr         providers.ChangeRequest
	)

	manifest := func(limit string) []byte {
		return []byte(fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      containers:
        - name: worker
          resources:
            limits:
              memory: %s
`, deployment.Name, deployment.Namespace, limit))
	}
	reconciler := func() *RemediationRequestReconciler {
		return &RemediationRequestReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Source: &stubResolver{result: resolved()},
			OpenRepository: func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
				return git, nil
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
						Name: workerContainer, Image: "example.invalid/worker:1",
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
		policy.Spec.GitOps.AllowedPaths = []remediationv1alpha1.GitPathPattern{"apps/**/*.yaml"}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		settings = fx.RemediationSettings()
		settings.Spec.Repositories[0].TargetBranch = mainBranch
		Expect(k8sClient.Create(ctx, settings)).To(Succeed())
		request = fx.MemoryLimitRequest(requestNamespace, fmt.Sprintf("fresh-rr-%d", seq))
		request.Spec.PolicyRef = remediationv1alpha1.Name(policy.Name)
		request.Spec.TargetRef.Name = deployment.Name
		request.Spec.TargetRef.UID = remediationv1alpha1.UID(deployment.UID)
		Expect(k8sClient.Create(ctx, request)).To(Succeed())

		git = fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: manifest("256Mi"), "README.yaml": []byte("a: 1\n")})
		closes.Store(0)
		git.SetFail(func(op string) error {
			if op == "CloseChangeRequest" {
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
		got.Status.Phase = remediationv1alpha1.PhasePRCreated
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
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: "default"},
				&remediationv1alpha1.RemediationSettings{}))
		}).Should(BeTrue())
	})

	changeRequest := func() providers.ChangeRequest {
		got, err := git.FindChangeRequest(ctx, freshness.BranchName(fetch()))
		Expect(err).NotTo(HaveOccurred())
		return got
	}

	It("records the plan digest with the decision", func() {
		got := fetch()
		Expect(got.Status.PlanDigest).To(MatchRegexp(`^sha256:[0-9a-f]{64}$`))
		want, err := freshness.ComputeDigest(got, got.Status.Source, policy, settings)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Status.PlanDigest).To(Equal(want))
	})

	It("leaves a fresh plan alone, also after a merge that does not touch the file", func() {
		_, err := git.Push(mainBranch, "README.yaml", []byte("a: 2\n"))
		Expect(err).NotTo(HaveOccurred())
		before := fetch()
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
		supersedes(freshness.ReasonPlanChanged)
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
			if op == "CloseChangeRequest" && crash {
				crash = false
				return errors.New("connection lost after the close")
			}
			return nil
		})

		_, err = reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePRCreated), "not decided before the close was confirmed")

		supersedes(freshness.ReasonTargetFileChanged)
		Expect(closes.Load()).To(Equal(int32(2)), "the retry closes an already closed change request, a no-op")
	})

	It("does not supersede and does not close when an input cannot be read", func() {
		git.SetFail(func(op string) error {
			if op == "BranchHead" {
				return &providers.RetryableError{Err: errors.New("host down")}
			}
			return nil
		})
		_, err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePRCreated))
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
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePRCreated))
	})
})
