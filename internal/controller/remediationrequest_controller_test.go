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
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	requestNamespace = "default"
	policyBaseName   = "mem-policy"
	baseCommit       = "0123456789abcdef0123456789abcdef01234567"
	sourcePath       = "apps/orders/deployment.yaml"
	buildPath        = "apps/orders"
	buildKustomize   = "apps/orders/kustomization.yaml"
)

// stubResolver returns a canned mapping result and counts the calls.
type stubResolver struct {
	result sourcemap.Result
	err    error
	calls  atomic.Int32
	// during runs inside Resolve, to change the cluster while the mapping runs.
	during func()
}

func (s *stubResolver) Resolve(context.Context, sourcemap.Input) (sourcemap.Result, error) {
	s.calls.Add(1)
	if s.during != nil {
		s.during()
	}
	return s.result, s.err
}

func resolved() sourcemap.Result {
	return sourcemap.Result{
		Outcome: sourcemap.Resolved,
		Reason:  sourcemap.ReasonResolved,
		Source: &remediationv1alpha1.ResolvedSource{
			RepositoryRef: fx.RepositoryName,
			TargetBranch:  "main",
			Path:          sourcePath,
			BuildPath:     buildPath,
			BaseCommit:    baseCommit,
		},
	}
}

// buildFile is the kustomization file of buildPath: it builds the one manifest at sourcePath.
var buildFile = []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n")

var _ = Describe("RemediationRequest Controller", func() {
	ctx := context.Background()
	var (
		seq        int
		deployment *appsv1.Deployment
		policy     *remediationv1alpha1.RemediationPolicy
		settings   *remediationv1alpha1.RemediationSettings
		request    *remediationv1alpha1.RemediationRequest
		stub       *stubResolver
	)

	reconcileOnce := func() (reconcile.Result, error) {
		r := &RemediationRequestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Source: stub}
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
	}
	fetch := func() *remediationv1alpha1.RemediationRequest {
		got := &remediationv1alpha1.RemediationRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), got)).To(Succeed())
		return got
	}
	condition := func(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
		return meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
	}
	// create builds the policy, settings, target and request; each mutator runs on the objects before they
	// are created.
	create := func(mutate func()) {
		seq++
		stub = &stubResolver{result: resolved()}
		deployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: fx.TargetNamespace, Name: fmt.Sprintf("web-%d", seq)},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "worker", Image: workerImage,
						Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						}},
					}}},
				},
			},
		}
		policy = fx.RemediationPolicy(fmt.Sprintf("%s-%d", policyBaseName, seq))
		policy.Spec.Actions = []remediationv1alpha1.PolicyAction{fx.MemoryLimitPolicyAction()}
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 100
		policy.Spec.RequestNamespaces = []remediationv1alpha1.Name{requestNamespace}
		policy.Spec.GitOps.AllowedPaths = []remediationv1alpha1.GitPathPattern{appsGlob}
		settings = fx.RemediationSettings()
		request = fx.MemoryLimitRequest(requestNamespace, fmt.Sprintf("rr-%d", seq))
		request.Spec.PolicyRef = remediationv1alpha1.Name(policy.Name)
		mutate()

		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		if settings != nil {
			Expect(k8sClient.Create(ctx, settings)).To(Succeed())
		}
		request.Spec.TargetRef.Name = deployment.Name
		request.Spec.TargetRef.UID = remediationv1alpha1.UID(deployment.UID)
		Expect(k8sClient.Create(ctx, request)).To(Succeed())
	}

	BeforeEach(func() {
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fx.TargetNamespace}})
		Expect(client.IgnoreAlreadyExists(err)).To(Succeed())
	})
	AfterEach(func() {
		objs := []client.Object{request, policy, deployment}
		if settings != nil {
			objs = append(objs, settings)
		}
		for _, o := range objs {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
		// Wait for the settings to go, so the next spec can create them again.
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: SettingsName},
				&remediationv1alpha1.RemediationSettings{}))
		}).Should(BeTrue())
		request, policy, settings, deployment = nil, nil, nil, nil
	})

	It("moves an allowed request to PolicyChecked and records the decision", func() {
		create(func() {})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())

		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
		Expect(got.Status.EffectiveApprovalRequired).To(HaveValue(BeTrue()))
		Expect(got.Status.Source).To(Equal(resolved().Source))
		Expect(got.Status.PolicyUID).To(Equal(remediationv1alpha1.UID(policy.UID)))
		Expect(got.Status.PolicyRevision).To(Equal(fmt.Sprintf("%s/%d", policy.Name, policy.Generation)))
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		c := condition(got)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal("WithinApprovedBounds"))
	})

	It("lowers approval only when the policy is Optional and the request asks", func() {
		create(func() {
			policy.Spec.Approval = remediationv1alpha1.ApprovalOptional
			request.Spec.ApprovalRequired = new(false)
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.EffectiveApprovalRequired).To(HaveValue(BeFalse()))
	})

	It("keeps approval required when the policy requires it, whatever the request says", func() {
		create(func() { request.Spec.ApprovalRequired = new(false) })
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.EffectiveApprovalRequired).To(HaveValue(BeTrue()))
	})

	It("changes nothing when the same request is reconciled again", func() {
		create(func() {})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		first := fetch()
		calls := stub.calls.Load()

		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		second := fetch()
		Expect(second.ResourceVersion).To(Equal(first.ResourceVersion))
		Expect(second.Status).To(Equal(first.Status))
		Expect(stub.calls.Load()).To(Equal(calls), "a decided request is not mapped or checked again")
	})

	It("rejects a status write made from a stale read", func() {
		create(func() {})
		stale := fetch()
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())

		stale.Status.Phase = remediationv1alpha1.PhaseDenied
		err = k8sClient.Status().Update(ctx, stale)
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
	})

	It("denies a request whose policy does not exist, without retrying", func() {
		create(func() {})
		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{}))
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
		Expect(condition(got).Reason).To(Equal(ReasonPolicyNotFound))
		Expect(stub.calls.Load()).To(BeZero())
	})

	It("denies a request when RemediationSettings does not exist, without retrying", func() {
		create(func() { settings = nil })
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(reconcile.Result{}))
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
		Expect(condition(got).Reason).To(Equal(ReasonSettingsNotFound))
	})

	It("denies a request whose target UID differs from the live object", func() {
		create(func() {})
		Expect(k8sClient.Delete(ctx, request)).To(Succeed())
		request = fx.MemoryLimitRequest(requestNamespace, fmt.Sprintf("rr-uid-%d", seq))
		request.Spec.TargetRef.Name = deployment.Name
		request.Spec.PolicyRef = remediationv1alpha1.Name(policy.Name)
		Expect(k8sClient.Create(ctx, request)).To(Succeed()) // the fixture UID is not the Deployment's

		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
		Expect(condition(got).Reason).To(Equal(ReasonTargetMismatch))
		Expect(stub.calls.Load()).To(BeZero())
	})

	It("denies a request whose previous value is not the live value", func() {
		// Live is 256Mi. Claiming 1000Mi makes 1024Mi look like a 2.4% increase; it is a 300% one.
		create(func() {
			policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 10
			request.Spec.Action.SetContainerMemoryLimit.Previous = resource.MustParse("1000Mi")
			request.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse("1024Mi")
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
		Expect(condition(got).Reason).To(Equal(ReasonPreviousValueMismatch))
		Expect(stub.calls.Load()).To(BeZero())
	})

	It("accepts a previous value written in another unit", func() {
		create(func() {
			request.Spec.Action.SetContainerMemoryLimit.Previous = resource.MustParse("268435456") // 256Mi
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
	})

	It("sends a target without the field to NeedsIntervention", func() {
		create(func() { deployment.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{} })
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(condition(got).Reason).To(Equal(string(sourcemap.ReasonFieldNotFound)))
	})

	It("waits as long as the Git host asked before retrying", func() {
		create(func() {})
		stub.err = &sourcemap.TransientError{Op: "read head of branch main", RetryAfter: 42 * time.Second}
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(42 * time.Second))
		Expect(fetch().Status.Phase).To(BeEmpty())
	})

	It("sends a missing target to NeedsIntervention", func() {
		create(func() {})
		Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		Expect(condition(got).Reason).To(Equal(string(sourcemap.ReasonTargetNotFound)))
	})

	It("sends a NeedsIntervention mapping to NeedsIntervention, not Denied", func() {
		create(func() {})
		stub.result = sourcemap.Result{Outcome: sourcemap.NeedsIntervention, Reason: sourcemap.ReasonHelmManaged,
			Message: "installed by a HelmRelease"}
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
		c := condition(got)
		Expect(c.Status).To(Equal(metav1.ConditionUnknown))
		Expect(c.Reason).To(Equal(string(sourcemap.ReasonHelmManaged)))
		Expect(got.Status.Source).To(BeNil())
	})

	It("denies a request the mapping refuses", func() {
		create(func() {})
		stub.result = sourcemap.Result{Outcome: sourcemap.Denied, Reason: sourcemap.ReasonNotInInventory,
			Message: "not in inventory"}
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
		Expect(condition(got).Reason).To(Equal(string(sourcemap.ReasonNotInInventory)))
	})

	It("checks again when the target changes while the source is resolved", func() {
		// The baseline passes on 256Mi; Flux then sets 512Mi, so 256Mi -> 384Mi in Git would be a decrease.
		create(func() {
			request.Spec.Action.SetContainerMemoryLimit.Previous = resource.MustParse("256Mi")
			request.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse("384Mi")
		})
		stub.during = func() {
			live := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(deployment), live)).To(Succeed())
			live.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
			Expect(k8sClient.Update(ctx, live)).To(Succeed())
		}
		_, err := reconcileOnce()
		Expect(err).To(MatchError(ContainSubstring("changed during the policy check")))
		Expect(fetch().Status.Phase).To(BeEmpty())

		// The next attempt sees 512Mi and refuses the stale baseline.
		stub.during = nil
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
		Expect(condition(got).Reason).To(Equal(ReasonPreviousValueMismatch))
	})

	It("retries a transient mapping error and writes nothing", func() {
		create(func() {})
		stub.err = &sourcemap.TransientError{Op: "read head of branch main"}
		_, err := reconcileOnce()
		Expect(errors.Is(err, sourcemap.ErrTransient)).To(BeTrue())
		Expect(fetch().Status.Phase).To(BeEmpty())
	})

	DescribeTable("denies a plan the policy refuses, with the reason of the check",
		func(reason string, mutate func(), source func(*remediationv1alpha1.ResolvedSource)) {
			create(mutate)
			source(stub.result.Source)
			_, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
			c := condition(got)
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal(reason))
			Expect(got.Status.EffectiveApprovalRequired).To(BeNil())
			Expect(got.Status.PolicyUID).To(Equal(remediationv1alpha1.UID(policy.UID)))
		},
		Entry("request namespace", "RequestNamespaceNotAllowed",
			func() { policy.Spec.RequestNamespaces = []remediationv1alpha1.Name{"elsewhere"} },
			func(*remediationv1alpha1.ResolvedSource) {}),
		Entry("target namespace", "TargetNamespaceNotAllowed",
			func() { policy.Spec.Targets.Namespaces = []remediationv1alpha1.Name{"elsewhere"} },
			func(*remediationv1alpha1.ResolvedSource) {}),
		Entry("labels", "TargetLabelsNotMatched",
			func() {
				policy.Spec.Targets.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}}
			},
			func(*remediationv1alpha1.ResolvedSource) {}),
		Entry("action", "ActionNotAllowed",
			func() {
				policy.Spec.Actions = []remediationv1alpha1.PolicyAction{fx.RemediationPolicy("x").Spec.Actions[0]}
			},
			func(*remediationv1alpha1.ResolvedSource) {}),
		Entry("below minimum", "BelowMinimum",
			func() { policy.Spec.Actions[0].SetContainerMemoryLimit.Min = resource.MustParse("512Mi") },
			func(*remediationv1alpha1.ResolvedSource) {}),
		Entry("above maximum", "AboveMaximum",
			func() { policy.Spec.Actions[0].SetContainerMemoryLimit.Max = resource.MustParse("300Mi") },
			func(*remediationv1alpha1.ResolvedSource) {}),
		Entry("increase too large", "IncreaseTooLarge",
			func() { policy.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 10 },
			func(*remediationv1alpha1.ResolvedSource) {}),
		Entry("repository", "RepositoryNotAllowed",
			func() {},
			func(s *remediationv1alpha1.ResolvedSource) { s.RepositoryRef = "other" }),
		Entry("branch", "BranchNotAllowed",
			func() {},
			func(s *remediationv1alpha1.ResolvedSource) { s.TargetBranch = "dev" }),
		Entry("path", "PathNotAllowed",
			func() {},
			func(s *remediationv1alpha1.ResolvedSource) { s.Path = "clusters/prod/flux.yaml" }),
	)

	It("ignores a request that is already past the policy check", func() {
		create(func() {})
		rr := fetch()
		rr.Status.Phase = remediationv1alpha1.PhasePRCreated
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch().ResourceVersion).To(Equal(rr.ResourceVersion))
		Expect(stub.calls.Load()).To(BeZero())
	})

	It("keeps Git URLs out of the status", func() {
		create(func() {})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		for _, c := range got.Status.Conditions {
			Expect(c.Message).NotTo(ContainSubstring("://"))
		}
	})
})
