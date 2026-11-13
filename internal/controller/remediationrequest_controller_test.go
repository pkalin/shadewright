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
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/revert"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	requestNamespace = "default"
	policyBaseName   = "mem-policy"
	baseCommit       = "0123456789abcdef0123456789abcdef01234567"
	sourcePath       = "apps/orders/deployment.yaml"
	buildPath        = "apps/orders"
	buildKustomize   = "apps/orders/kustomization.yaml"
	// ksNotReadyMessage is the mapping's message about the Kustomization the fixtures deliver from.
	ksNotReadyMessage = "Kustomization flux-system/apps is not Ready"
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

// failingRequestReader fails every read of the RemediationRequest called name and reads anything else from
// Reader.
type failingRequestReader struct {
	client.Reader
	name string
}

func (f failingRequestReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.RemediationRequest); ok && key.Name == f.name {
		return apierrors.NewServiceUnavailable("the API server is not available")
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// splitReader serves the Flux objects from a fake client (envtest has no Flux CRDs) and everything else from
// the API server.
type splitReader struct {
	flux client.Reader
}

func (s splitReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	switch obj.GetObjectKind().GroupVersionKind().Group {
	case sourcemap.KustomizationGVK.Group, sourcemap.GitRepositoryGVK.Group:
		return s.flux.Get(ctx, key, obj, opts...)
	}
	return k8sClient.Get(ctx, key, obj, opts...)
}

func (s splitReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return errors.New("the resolver does not list")
}

// fluxReader returns a reader with a Ready Kustomization flux-system/apps that delivers dep from path
// apps/orders of GitRepository flux-system/fleet, branch main of url.
func fluxReader(dep *appsv1.Deployment, url string) client.Reader {
	revision := mainBranch + "@sha1:" + baseCommit
	parse := func(doc string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		ExpectWithOffset(2, yaml.Unmarshal([]byte(doc), &u.Object)).To(Succeed())
		return u
	}
	ks := parse(fmt.Sprintf(`apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {namespace: flux-system, name: apps, generation: 1}
spec: {path: ./%s, prune: true, sourceRef: {kind: GitRepository, name: fleet}}
status:
  lastAppliedRevision: %q
  observedGeneration: 1
  conditions: [{type: Ready, status: "True", observedGeneration: 1}]
  inventory: {entries: [{id: %s_%s_apps_Deployment, v: v1}]}
`, buildPath, revision, dep.Namespace, dep.Name))
	src := parse(fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: {namespace: flux-system, name: fleet, generation: 1}
spec: {url: %q, ref: {branch: %s}}
status:
  observedGeneration: 1
  conditions: [{type: Ready, status: "True", observedGeneration: 1}]
  artifact: {revision: %q}
`, url, mainBranch, revision))
	return splitReader{flux: clientfake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(ks, src).Build()}
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
		r := &RemediationRequestReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Source: stub}
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
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName},
				&remediationv1alpha1.RemediationSettings{}))
		}).Should(BeTrue())
		request, policy, settings, deployment = nil, nil, nil, nil
	})

	// The revert link (ADR-0030 section 8, #96). asRevertOf turns request, as created, into the request a revert
	// names: it ended Failed with Verified False. It then creates the revert of it, from previous to desired
	// swapped, under the name derived from its UID, edited by edit before it is created, and makes it the request
	// the spec reconciles. The failed request is deleted after the spec.
	asRevertOf := func(edit func(failed, revert *remediationv1alpha1.RemediationRequest)) *remediationv1alpha1.RemediationRequest {
		failed := request.DeepCopy()
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, failed))).To(Succeed()) })
		failed.Status.Phase = remediationv1alpha1.PhaseFailed
		meta.SetStatusCondition(&failed.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionVerified,
			Status: metav1.ConditionFalse, Reason: ReasonOOMKilled, Message: "OOM killed under the desired limit"})
		Expect(k8sClient.Status().Update(ctx, failed)).To(Succeed())
		revert := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace,
			Name: string(remediationv1alpha1.RevertName(remediationv1alpha1.UID(failed.UID)))}}
		revert.Spec = *failed.Spec.DeepCopy()
		m := revert.Spec.Action.SetContainerMemoryLimit
		m.Previous, m.Desired = m.Desired, m.Previous
		revert.Spec.Reverts = &remediationv1alpha1.RequestRef{Name: failed.Name,
			UID: remediationv1alpha1.UID(failed.UID)}
		if edit != nil {
			edit(failed, revert)
		}
		Expect(k8sClient.Create(ctx, revert)).To(Succeed())
		request = revert
		return failed
	}
	// liveLimit sets the deployment's memory limit before it is created.
	liveLimit := func(q string) {
		deployment.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse(q)
	}

	It("lets a valid revert through the link to the policy, which refuses its decrease by default", func() {
		// The failed request raised the limit from 256Mi to 384Mi, which is live; the revert lowers it again.
		create(func() { liveLimit("384Mi") })
		asRevertOf(nil)
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
		// The link held: the refusal is the policy's (Preflight, before any mapping), not InvalidRevert.
		Expect(condition(got).Reason).To(Equal("DecreaseNotAllowed"))
	})

	// allowDecreases lets the policy allow the revert's decrease (384Mi to 256Mi, or 128Mi), so only the link
	// check can deny it.
	allowDecreases := func() {
		policy.Spec.Actions[0].SetContainerMemoryLimit.MaxDecreasePercent = ptr.To[int32](90)
	}

	It("lets a valid revert through the link to a policy that allows its decrease", func() {
		create(func() { liveLimit("384Mi"); allowDecreases() })
		asRevertOf(nil)
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).NotTo(Equal(remediationv1alpha1.PhaseDenied), "conditions %+v", got.Status.Conditions)
		Expect(condition(got).Status).To(Equal(metav1.ConditionTrue))
	})

	// End to end from the analyzer's side (#96, second part): the revert proposer turns a request that ended
	// Failed with Verified False into a revert, records it on the Incident, and the executor's link check lets
	// that revert through to a policy that allows its decrease.
	It("lets the revert the analyzer's proposer made through the link check", func() {
		inc := fx.Incident(requestNamespace, "incident-proposer")
		create(func() {
			liveLimit("384Mi")
			allowDecreases()
			Expect(k8sClient.Create(ctx, inc)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, inc))).To(Succeed()) })
			request.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: inc.Name, UID: remediationv1alpha1.UID(inc.UID)}
		})
		failed := request.DeepCopy()
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, failed))).To(Succeed()) })
		failed.Status.Phase = remediationv1alpha1.PhaseFailed
		meta.SetStatusCondition(&failed.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionVerified,
			Status: metav1.ConditionFalse, Reason: ReasonOOMKilled, Message: "OOM killed under the desired limit"})
		Expect(k8sClient.Status().Update(ctx, failed)).To(Succeed())

		proposer := &revert.Proposer{Client: k8sClient, Reader: k8sClient, Namespace: requestNamespace}
		_, err := proposer.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(failed)})
		Expect(err).NotTo(HaveOccurred())

		request = &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace,
			Name: string(remediationv1alpha1.RevertName(remediationv1alpha1.UID(failed.UID)))}}
		made := fetch()
		Expect(made.Spec.Reverts).To(Equal(&remediationv1alpha1.RequestRef{Name: failed.Name,
			UID: remediationv1alpha1.UID(failed.UID)}))
		Expect(made.Spec.ApprovalRequired).To(Equal(new(true)))
		recorded := &remediationv1alpha1.Incident{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inc), recorded)).To(Succeed())
		Expect(recorded.Status.RevertsProposed).To(ConsistOf(remediationv1alpha1.ProposedRevert{
			RequestRef: *made.Spec.Reverts, State: remediationv1alpha1.RevertCreated}))

		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).NotTo(Equal(remediationv1alpha1.PhaseDenied), "conditions %+v", got.Status.Conditions)
		Expect(condition(got).Status).To(Equal(metav1.ConditionTrue))
	})

	// The policy allows every one of these as a plain decrease: only the link check denies them.
	DescribeTable("denies a revert whose link does not hold with InvalidRevert, before the policy check",
		func(edit func(failed, revert *remediationv1alpha1.RemediationRequest), why string) {
			create(func() { liveLimit("384Mi"); allowDecreases() })
			failed := asRevertOf(edit)
			if why == "does not exist" {
				Expect(k8sClient.Delete(ctx, failed)).To(Succeed())
			}
			_, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseDenied))
			c := condition(got)
			Expect(c.Reason).To(Equal(remediationv1alpha1.ReasonInvalidRevert))
			Expect(c.Message).To(ContainSubstring(why))
			Expect(stub.calls.Load()).To(BeZero(), "nothing was mapped for an invalid revert")
		},
		Entry("a missing request", nil, "does not exist"),
		Entry("a name not derived from the failed UID", func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Name = "revert-chosen-by-hand"
		}, "name is not the one derived"),
		Entry("another UID", func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Reverts.UID = "abababab-abab-4bab-8bab-abababababab"
			r.Name = string(remediationv1alpha1.RevertName(r.Spec.Reverts.UID))
		}, "another UID"),
		Entry("a request that did not fail", func(f, _ *remediationv1alpha1.RemediationRequest) {
			f.Status.Phase = remediationv1alpha1.PhaseInconclusive
			meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionVerified,
				Status: metav1.ConditionUnknown, Reason: "TargetGone", Message: "gone"})
			Expect(k8sClient.Status().Update(ctx, f)).To(Succeed())
		}, "did not end Failed"),
		Entry("another action value", func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse("128Mi")
		}, "not exactly the inverse"),
	)

	It("retries a failed read of the reverted request, never a refusal", func() {
		create(func() { liveLimit("384Mi") })
		failed := asRevertOf(nil)
		r := &RemediationRequestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Source: stub,
			APIReader: failingRequestReader{Reader: k8sClient, name: failed.Name}}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).To(HaveOccurred())
		got := fetch()
		Expect(got.Status.Phase).To(BeEmpty())
		Expect(condition(got)).To(BeNil())
	})

	It("moves an allowed request to PolicyChecked and records the decision", func() {
		create(func() {})
		allowed := testutil.ToFloat64(policyDecisions.WithLabelValues(decisionAllowed))
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.ToFloat64(policyDecisions.WithLabelValues(decisionAllowed)) - allowed).To(Equal(1.0))

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
		allowed := testutil.ToFloat64(policyDecisions.WithLabelValues(decisionAllowed))

		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.ToFloat64(policyDecisions.WithLabelValues(decisionAllowed))).To(Equal(allowed),
			"a decision is counted once")
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
		denials := testutil.ToFloat64(policyDenials.WithLabelValues(ReasonPolicyNotFound))
		deniedCount := testutil.ToFloat64(policyDecisions.WithLabelValues(decisionDenied))
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.ToFloat64(policyDenials.WithLabelValues(ReasonPolicyNotFound)) - denials).To(Equal(1.0))
		Expect(testutil.ToFloat64(policyDecisions.WithLabelValues(decisionDenied)) - deniedCount).To(Equal(1.0))
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

	It("checks again when the target changes while the mapping stops", func() {
		// The mapping compared Git with an object that Flux has replaced since: its stop describes the old one.
		create(func() {})
		stub.result = sourcemap.Result{Outcome: sourcemap.NeedsIntervention, Reason: sourcemap.ReasonValueMismatch,
			Message: "differs"}
		stub.during = func() {
			live := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(deployment), live)).To(Succeed())
			live.Annotations = map[string]string{"example.invalid/touched": "yes"}
			Expect(k8sClient.Update(ctx, live)).To(Succeed())
		}
		_, err := reconcileOnce()
		Expect(err).To(MatchError(ContainSubstring("changed during the policy check")))
		Expect(fetch().Status.Phase).To(BeEmpty(), "a stop on an object that changed is not decided")
	})

	// The real resolver, reading through a Git host that refuses for good: the class survives the mapping, so
	// the policy check decides now instead of retrying without end (a deadline starts only with PolicyChecked).
	DescribeTable("stops in NeedsIntervention when the Git host refuses the source mapping's reads",
		func(class error, reason string) {
			create(func() {
				deployment.Labels = map[string]string{
					sourcemap.LabelKustomizationName:      testAppsPath,
					sourcemap.LabelKustomizationNamespace: "flux-system",
				}
			})
			git := fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: []byte("unused")})
			git.SetFail(func(op string) error {
				if op == opBranchHead {
					return fmt.Errorf("status 403 from https://github.com/example/fleet with token ghs_leaked: %w", class)
				}
				return nil
			})
			resolver := &sourcemap.Resolver{
				Client: fluxReader(deployment, settings.Spec.Repositories[0].URL),
				OpenRepository: func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
					return git, nil
				},
			}
			r := &RemediationRequestReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Source: resolver}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
			Expect(err).NotTo(HaveOccurred(), "a final refusal is not handed back for a retry")
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			c := condition(got)
			Expect(c.Status).To(Equal(metav1.ConditionUnknown))
			Expect(c.Reason).To(Equal(reason))
			Expect(c.Message).NotTo(ContainSubstring("ghs_leaked"))
			Expect(c.Message).NotTo(ContainSubstring("https://"))
			Expect(got.Status.Source).To(BeNil())
		},
		Entry("403", providers.ErrForbidden, ReasonGitForbidden),
		Entry("422", providers.ErrRejected, ReasonGitRejected),
	)

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

	// Duplicates of a merged request (#556): the lab run of 2026-11-08, where two requests for the Incident
	// another request had just fixed ended NeedsIntervention (KustomizationNotReady) instead of Superseded.
	Context("when another request of the same Incident and target was merged", func() {
		// mergeAnother creates a request of the same Incident and target as request, Merged at mergedAt with
		// pull request #51, holding no lock, as the pull request step leaves it.
		mergeAnother := func(mergedAt time.Time) *remediationv1alpha1.RemediationRequest {
			w := fx.MemoryLimitRequest(requestNamespace, request.Name+"-merged")
			w.Spec.PolicyRef = request.Spec.PolicyRef
			w.Spec.TargetRef = request.Spec.TargetRef
			Expect(k8sClient.Create(ctx, w)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, w))).To(Succeed()) })
			at := metav1.NewMicroTime(mergedAt)
			w.Status.Phase = remediationv1alpha1.PhaseMerged
			w.Status.PlanDigest = limitsDigest
			w.Status.LastPhaseTransitionTime = &at
			w.Status.HoldsTarget = new(false)
			w.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 51,
				URL: "https://github.com/o/r/pull/51", HeadSHA: baseCommit}
			Expect(k8sClient.Status().Update(ctx, w)).To(Succeed())
			return w
		}
		// queueBehindLock marks request Queued behind the target lock, as it waited while the other one held it.
		queueBehindLock := func() {
			rr := fetch()
			meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionQueued,
				Status: metav1.ConditionTrue, Reason: ReasonTargetLocked, Message: queuedMessages[ReasonTargetLocked]})
			Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		}
		expectSuperseded := func(w *remediationv1alpha1.RemediationRequest) {
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseSuperseded))
			c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
			Expect(c).NotTo(BeNil())
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal(remediationv1alpha1.ReasonAnotherRequestMerged))
			Expect(c.Message).To(ContainSubstring(w.Name))
			Expect(c.Message).To(ContainSubstring("#51"))
			Expect(meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued)).To(BeNil())
			Expect(got.Status.HoldsTarget).To(HaveValue(BeFalse()))
			Expect(got.Status.PlanDigest).To(BeEmpty())
		}

		It("ends a duplicate waiting behind the lock Superseded, naming the merged request, and reads no Git", func() {
			create(func() {})
			queueBehindLock()
			w := mergeAnother(time.Now().Add(time.Second))
			// Flux is applying the merged revision: what the duplicate found in the lab.
			stub.result = sourcemap.Result{Outcome: sourcemap.NeedsIntervention,
				Reason: sourcemap.ReasonKustomizationReconciling, Message: ksNotReadyMessage}
			_, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			expectSuperseded(w)
			Expect(stub.calls.Load()).To(BeZero())
		})

		It("ends a duplicate whose merge lands while it is decided, before it takes the lock", func() {
			create(func() {})
			var w *remediationv1alpha1.RemediationRequest
			stub.during = func() {
				if w == nil {
					w = mergeAnother(time.Now().Add(time.Second))
				}
			}
			_, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			expectSuperseded(w)
			Expect(stub.calls.Load()).To(BeNumerically("==", 1))
		})

		It("decides a request proposed after the merge as usual", func() {
			create(func() {})
			mergeAnother(time.Now().Add(-time.Hour))
			_, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
		})

		// reconcileAt reconciles request with the reconciler's clock at now.
		reconcileAt := func(now time.Time) (reconcile.Result, error) {
			r := &RemediationRequestReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
				Source: stub, Now: func() time.Time { return now }}
			return r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		}

		// A request in a wait (Flux reconciling) is not Queued: it is checked before the policy check runs again,
		// so it ends Superseded rather than in a wait that times out NeedsIntervention (#556 review).
		It("ends a duplicate that waits for Flux Superseded once another request merges", func() {
			create(func() {})
			stub.result = sourcemap.Result{Outcome: sourcemap.NeedsIntervention,
				Reason: sourcemap.ReasonKustomizationReconciling, Message: ksNotReadyMessage}
			_, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			Expect(fetch().Status.Phase).To(BeEmpty())
			Expect(meta.IsStatusConditionTrue(fetch().Status.Conditions, remediationv1alpha1.ConditionQueued)).To(BeFalse())
			calls := stub.calls.Load()

			w := mergeAnother(time.Now().Add(time.Second))
			_, err = reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			expectSuperseded(w)
			Expect(stub.calls.Load()).To(Equal(calls), "the duplicate read nothing more from Git")
		})

		// A merge whose change no longer holds on the live target (reverted, or never applied long after the
		// merge) supersedes nothing: the duplicate is decided as usual (#556 review).
		It("decides a duplicate as usual when the merged change is not in effect", func() {
			create(func() {})
			mergeAnother(time.Now().Add(time.Second))
			// An hour later the live target still holds the value the merged request replaced (256Mi).
			_, err := reconcileAt(time.Now().Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
		})

		It("ends a duplicate Superseded while the merged change holds on the live target", func() {
			create(func() {
				deployment.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("384Mi")
			})
			w := mergeAnother(time.Now().Add(time.Second))
			_, err := reconcileAt(time.Now().Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			expectSuperseded(w)
		})

		It("leaves a request of another Incident alone", func() {
			create(func() {
				request.Spec.IncidentRef = otherIncident
			})
			mergeAnother(time.Now().Add(time.Second))
			_, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
		})
	})

	// Flux mid-reconcile of a merged revision (#556): the mapping's KustomizationReconciling is a bounded wait,
	// not NeedsIntervention, and a real failure still ends the request at once.
	Context("when Flux is reconciling the Kustomization", func() {
		reconciling := func() sourcemap.Result {
			return sourcemap.Result{Outcome: sourcemap.NeedsIntervention, Reason: sourcemap.ReasonKustomizationReconciling,
				Message: "Kustomization flux-system/apps is not Ready for its current generation"}
		}
		reconcileAt := func(now time.Time) (reconcile.Result, error) {
			r := &RemediationRequestReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
				Source: stub, Now: func() time.Time { return now }}
			return r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		}

		It("waits instead of ending the request NeedsIntervention, then decides once Flux is Ready", func() {
			create(func() {})
			stub.result = reconciling()
			start := time.Now()
			res, err := reconcileAt(start)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			got := fetch()
			Expect(got.Status.Phase).To(BeEmpty())
			c := condition(got)
			Expect(c.Status).To(Equal(metav1.ConditionUnknown))
			Expect(c.Reason).To(Equal(string(sourcemap.ReasonKustomizationReconciling)))
			Expect(got.Status.HoldsTarget).To(BeNil())

			stub.result = resolved()
			_, err = reconcileAt(start.Add(time.Minute))
			Expect(err).NotTo(HaveOccurred())
			Expect(fetch().Status.Phase).To(Equal(remediationv1alpha1.PhasePolicyChecked))
		})

		It("ends NeedsIntervention KustomizationNotReady once the wait is over", func() {
			create(func() {})
			stub.result = reconciling()
			start := time.Now()
			_, err := reconcileAt(start)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileAt(start.Add(DefaultTargetStateTimeout))
			Expect(err).NotTo(HaveOccurred())
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			c := condition(got)
			Expect(c.Reason).To(Equal(string(sourcemap.ReasonKustomizationNotReady)))
			Expect(c.Message).To(ContainSubstring("did not finish reconciling"))
		})

		// A wait, then a pass queued behind the lock for longer than the wait's timeout, then Flux reconciling
		// again (applying the holder's merge): a new wait episode with its own clock, not one that ends at once
		// because the old wait's start was kept while the request was Queued (#556 design review).
		It("starts a new wait after the request was queued behind the lock", func() {
			create(func() {})
			stub.result = reconciling()
			start := time.Now()
			_, err := reconcileAt(start)
			Expect(err).NotTo(HaveOccurred())

			holder := fx.MemoryLimitRequest(requestNamespace, request.Name+"-holder")
			holder.Spec.PolicyRef = request.Spec.PolicyRef
			holder.Spec.TargetRef = request.Spec.TargetRef
			holder.Spec.IncidentRef = otherIncident
			Expect(k8sClient.Create(ctx, holder)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, holder))).To(Succeed()) })
			holder.Status.Phase = remediationv1alpha1.PhasePolicyChecked
			holder.Status.HoldsTarget = new(true)
			Expect(k8sClient.Status().Update(ctx, holder)).To(Succeed())

			stub.result = resolved()
			_, err = reconcileAt(start.Add(time.Minute))
			Expect(err).NotTo(HaveOccurred())
			got := fetch()
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, remediationv1alpha1.ConditionQueued)).To(BeTrue())
			Expect(got.Status.Phase).To(BeEmpty())

			Expect(k8sClient.Delete(ctx, holder)).To(Succeed())
			stub.result = reconciling()
			later := start.Add(DefaultTargetStateTimeout + 5*time.Minute)
			res, err := reconcileAt(later)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			got = fetch()
			Expect(got.Status.Phase).To(BeEmpty())
			c := condition(got)
			Expect(c.Status).To(Equal(metav1.ConditionUnknown))
			Expect(c.Reason).To(Equal(string(sourcemap.ReasonKustomizationReconciling)))
			Expect(c.LastTransitionTime.Time).To(BeTemporally("==", later.Truncate(time.Second)))
		})

		It("still ends a failing Kustomization NeedsIntervention at once", func() {
			create(func() {})
			stub.result = sourcemap.Result{Outcome: sourcemap.NeedsIntervention,
				Reason: sourcemap.ReasonKustomizationNotReady, Message: ksNotReadyMessage}
			_, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			got := fetch()
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseNeedsIntervention))
			Expect(condition(got).Reason).To(Equal(string(sourcemap.ReasonKustomizationNotReady)))
		})
	})
})
