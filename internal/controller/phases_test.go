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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fluxdelivery "github.com/pkalin/shadewright/internal/providers/delivery/flux"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
	"github.com/pkalin/shadewright/internal/verification/rollout"
)

const (
	// phasesTargetPath is the file in the GitOps repo that holds the target of these specs.
	phasesTargetPath = "apps/cache.yaml"
	// phasesOtherBranch is a target branch that differs from the one in the receipt.
	phasesOtherBranch = "release-next"
)

// The phases after the merge (#681, ADR-0030 sections 2, 4 and 6) on a real API server, so every status write
// passes the CRD's validation rules (the receipt, the set-once applied time and rollout fields). Flux's
// Kustomization and the Git host are fakes; the target is a StatefulSet whose status the specs set, since no
// controller runs.

const phasesNamespace = "after-merge"

// ksStatus is what the fake Kustomization reports: the commits it applied and last attempted, whether it is
// Ready or failing (Ready False, BuildFailed), and whether it is suspended.
type ksStatus struct {
	applied, attempted string
	failed, suspended  bool
}

// kustomization renders s as the Kustomization flux-system/apps of the main branch.
func kustomization(s ksStatus) *unstructured.Unstructured {
	if s.attempted == "" {
		s.attempted = s.applied
	}
	ready := `{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded", "observedGeneration": 1, ` +
		`"lastTransitionTime": "2026-11-08T10:36:00Z"}`
	if s.failed {
		ready = `{"type": "Ready", "status": "False", "reason": "BuildFailed", "message": "build failed", ` +
			`"observedGeneration": 1, "lastTransitionTime": "2026-11-08T10:36:00Z"}`
	}
	u := &unstructured.Unstructured{}
	Expect(u.UnmarshalJSON([]byte(fmt.Sprintf(`{
  "apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
  "metadata": {"name": "apps", "namespace": "flux-system", "generation": 1},
  "spec": {"path": "./apps", "suspend": %t, "sourceRef": {"kind": "GitRepository", "name": "fleet"}},
  "status": {"observedGeneration": 1, "lastAppliedRevision": "main@sha1:%s", "lastAttemptedRevision": "main@sha1:%s",
    "conditions": [%s]}}`, s.suspended, s.applied, s.attempted, ready)))).To(Succeed())
	return u
}

// phasesWorld is one merged request with a merge receipt, its StatefulSet and the fakes around them.
type phasesWorld struct {
	git          *fakegit.Git
	reader       *rolloutReader
	rec          *record.FakeRecorder
	now, merged  time.Time
	base, merge  string
	r            *RemediationRequestReconciler
	key, ssKey   client.ObjectKey
	settingsRepo *remediationv1alpha1.PlanRepository
}

var _ = Describe("RemediationRequest phases after the merge", func() {
	ctx := context.Background()
	seq := 0

	BeforeEach(func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: phasesNamespace}}))).To(Succeed())
		settings := fx.RemediationSettings()
		settings.Spec.Repositories[0].TargetBranch = mainBranch
		Expect(k8sClient.Create(ctx, settings)).To(Succeed())
	})
	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, fx.RemediationSettings()))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName},
				&remediationv1alpha1.RemediationSettings{}))
		}).Should(BeTrue())
	})

	// statefulSet is the target: two replicas whose container worker has the desired limit, 384Mi, rolled out
	// completely. edit changes it before it is created.
	statefulSet := func(name string, edit func(*appsv1.StatefulSet)) *appsv1.StatefulSet {
		labels := map[string]string{appLabel: name}
		ss := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: phasesNamespace, Name: name, Labels: fluxLabels()},
			Spec: appsv1.StatefulSetSpec{
				Replicas: ptr.To[int32](2),
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
					Type: appsv1.RollingUpdateStatefulSetStrategyType},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainer, Image: "example/worker:1",
						Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("384Mi")}}}}},
				},
			},
		}
		if edit != nil {
			edit(ss)
		}
		return ss
	}

	// setTargetStatus writes the StatefulSet's status: observed, with updated pods and revisions.
	setTargetStatus := func(w *phasesWorld, updated, ready int32, current, update string) {
		ss := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, w.ssKey, ss)).To(Succeed())
		ss.Status = appsv1.StatefulSetStatus{ObservedGeneration: ss.Generation, Replicas: *ss.Spec.Replicas,
			ReadyReplicas: ready, CurrentReplicas: *ss.Spec.Replicas, UpdatedReplicas: updated,
			CurrentRevision: current, UpdateRevision: update}
		Expect(k8sClient.Status().Update(ctx, ss)).To(Succeed())
	}

	// newWorld creates the target and a request Merged an hour's fraction ago with its merge receipt.
	newWorld := func(edit func(*appsv1.StatefulSet)) *phasesWorld {
		seq++
		name := fmt.Sprintf("phases-%d", seq)
		git := fakegit.NewGit(mainBranch, map[string][]byte{phasesTargetPath: []byte("old")})
		base, err := git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		merge, err := git.Push(mainBranch, phasesTargetPath, []byte("new"))
		Expect(err).NotTo(HaveOccurred())

		ss := statefulSet(name, edit)
		Expect(k8sClient.Create(ctx, ss)).To(Succeed())
		w := &phasesWorld{git: git, base: base, merge: merge, rec: record.NewFakeRecorder(20),
			reader: &rolloutReader{Reader: k8sClient}, ssKey: client.ObjectKeyFromObject(ss),
			merged: time.Date(2026, 11, 8, 10, 35, 14, 0, time.UTC)}
		w.now = w.merged.Add(time.Minute)
		setTargetStatus(w, 2, 2, revisionNew, revisionNew)

		settings := fx.RemediationSettings()
		settings.Spec.Repositories[0].TargetBranch = mainBranch
		w.settingsRepo = freshness.RepositoryOf(settings.Spec.Repositories[0])
		rr := fx.MemoryLimitRequest(phasesNamespace, name)
		rr.Spec.TargetRef = remediationv1alpha1.ObjectRef{APIVersion: linkAPI, Kind: "StatefulSet",
			Namespace: phasesNamespace, Name: name, UID: remediationv1alpha1.UID(ss.UID)}
		Expect(k8sClient.Create(ctx, rr)).To(Succeed())
		mergedAt := metav1.NewMicroTime(w.merged)
		head := strings.Repeat("c", 40)
		rr.Status = remediationv1alpha1.RemediationRequestStatus{
			Phase:                   remediationv1alpha1.PhaseMerged,
			LastPhaseTransitionTime: &mergedAt,
			PlanDigest:              "sha256:" + strings.Repeat("a", 64),
			HoldsTarget:             new(false),
			BreakerCounts:           new(true),
			Source: &remediationv1alpha1.ResolvedSource{RepositoryRef: fx.RepositoryName, TargetBranch: mainBranch,
				Path: phasesTargetPath, BuildPath: testAppsPath, BaseCommit: strings.Repeat("b", 40)},
			PlanRepository: w.settingsRepo,
			PullRequest: &remediationv1alpha1.PullRequestStatus{Number: 56,
				URL: "https://github.com/example/fleet/pull/56", HeadSHA: head, MergeCommitSHA: merge, MergedAt: &mergedAt},
			MergeReceipt: &remediationv1alpha1.MergeReceipt{Repository: *w.settingsRepo, TargetBranch: mainBranch,
				PullRequest: 56, HeadSHA: head, MergeCommitSHA: merge, MergedAt: mergedAt},
		}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		w.key = client.ObjectKeyFromObject(rr)
		w.r = w.reconciler()
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, rr))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Namespace: w.ssKey.Namespace, Name: w.ssKey.Name}}))).To(Succeed())
		})
		return w
	}

	reconcileAt := func(w *phasesWorld, at time.Time) (reconcile.Result, error) {
		w.now = at
		return w.r.Reconcile(ctx, reconcile.Request{NamespacedName: w.key})
	}
	mustReconcileAt := func(w *phasesWorld, at time.Time) reconcile.Result {
		res, err := reconcileAt(w, at)
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	fetch := func(w *phasesWorld) *remediationv1alpha1.RemediationRequest {
		rr := &remediationv1alpha1.RemediationRequest{}
		Expect(k8sClient.Get(ctx, w.key, rr)).To(Succeed())
		return rr
	}
	condition := func(rr *remediationv1alpha1.RemediationRequest, typ string) metav1.Condition {
		c := meta.FindStatusCondition(rr.Status.Conditions, typ)
		if c == nil {
			return metav1.Condition{Type: typ}
		}
		return *c
	}
	expectEnd := func(w *phasesWorld, phase remediationv1alpha1.RemediationPhase, reason string) *remediationv1alpha1.RemediationRequest {
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(phase), "conditions %+v", rr.Status.Conditions)
		verified := condition(rr, remediationv1alpha1.ConditionVerified)
		want := metav1.ConditionUnknown
		if phase == remediationv1alpha1.PhaseFailed {
			want = metav1.ConditionFalse
		}
		Expect(verified.Status).To(Equal(want))
		Expect(verified.Reason).To(Equal(reason))
		Expect(rr.Status.Deadline).To(BeNil())
		return rr
	}
	windowEnd := func(w *phasesWorld) time.Time { return w.merged.Add(deliveryWindow) }
	later := func(w *phasesWorld, file string) string {
		c, err := w.git.Push(mainBranch, file, []byte("x"))
		Expect(err).NotTo(HaveOccurred())
		return c
	}

	It("moves a merged request to WaitingForDelivery with the delivery window as its deadline", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.base})
		res := mustReconcileAt(w, w.merged.Add(time.Minute))
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		Expect(rr.Status.Deadline.Time).To(BeTemporally("==", windowEnd(w)))
		Expect(condition(rr, remediationv1alpha1.ConditionDelivered).Status).To(Equal(metav1.ConditionUnknown))
		Expect(res.RequeueAfter).To(Equal(afterMergePollInterval))
		Expect(rr.Status.PullRequest.MergedAt.Time).To(BeTemporally("==", w.merged), "mergedAt is kept")
	})

	// Row 1: the merge commit is applied. Delivered True and Verifying in the same write as appliedObservedAt, and
	// the timeout of a memory fix with defaults is 21 minutes from that time.
	It("verifies an applied merge commit with a timeout of 21 minutes for a memory fix", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		applied := w.merged.Add(2 * time.Minute)
		mustReconcileAt(w, applied)
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		Expect(rr.Status.PullRequest.AppliedObservedAt.Time).To(BeTemporally("==", applied))
		Expect(rr.Status.Deadline.Time).To(BeTemporally("==", applied.Add(21*time.Minute)))
		Expect(condition(rr, remediationv1alpha1.ConditionDelivered).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(rr, remediationv1alpha1.ConditionConfigured).Status).To(Equal(metav1.ConditionTrue))
		Expect(rr.Status.Rollout.AppliedRevision).To(Equal(w.merge))
	})

	// Row 2: a later commit of the branch that holds the merge commit, asked of the receipt's repository.
	It("verifies a descendant revision and names it", func() {
		w := newWorld(nil)
		next := later(w, "apps/other.yaml")
		w.reader.ks = kustomization(ksStatus{applied: next})
		mustReconcileAt(w, w.merged.Add(2*time.Minute))
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		Expect(rr.Status.Rollout.AppliedRevision).To(Equal(next))
		Expect(condition(rr, remediationv1alpha1.ConditionDelivered).Message).To(ContainSubstring(next))
	})

	// Row 3: Flux never applied the merge commit itself and went from the commit before it to a later one.
	It("verifies after a skipped revision", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.base})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		later(w, "apps/a.yaml")
		skipped := later(w, "apps/b.yaml")
		w.reader.ks = kustomization(ksStatus{applied: skipped})
		mustReconcileAt(w, w.merged.Add(5*time.Minute))
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		Expect(rr.Status.Rollout.AppliedRevision).To(Equal(skipped))
	})

	// Row 4: delivered as a descendant, but a later commit overwrote the field.
	It("ends Inconclusive with ValueChanged when the live field does not hold the desired value", func() {
		w := newWorld(func(ss *appsv1.StatefulSet) {
			ss.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
		})
		w.reader.ks = kustomization(ksStatus{applied: later(w, phasesTargetPath)})
		mustReconcileAt(w, w.merged.Add(2*time.Minute))
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonValueChanged)
		c := condition(rr, remediationv1alpha1.ConditionConfigured)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(ReasonValueChanged))
		Expect(condition(rr, remediationv1alpha1.ConditionDelivered).Status).To(Equal(metav1.ConditionTrue))
	})

	// Row 5: the applied revision does not hold the merge commit; at the end of the window, DeliveryNotObserved.
	It("ends Inconclusive with DeliveryNotObserved at the end of the window", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.base})
		mustReconcileAt(w, windowEnd(w).Add(-time.Second))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		mustReconcileAt(w, windowEnd(w))
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonDeliveryNotObserved)
		Expect(condition(rr, remediationv1alpha1.ConditionDelivered).Status).To(Equal(metav1.ConditionFalse))
	})

	// Row 6: Flux failed on exactly the merge commit. Inside the window that is not final: the request keeps
	// waiting and the end of the window decides.
	It("decides a failure of the merge commit at the end of the window, Failed with DeliveryFailed", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.base, attempted: w.merge, failed: true})
		res := mustReconcileAt(w, w.merged.Add(10*time.Minute))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		mustReconcileAt(w, windowEnd(w).Add(-time.Second))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		mustReconcileAt(w, windowEnd(w))
		rr := expectEnd(w, remediationv1alpha1.PhaseFailed, ReasonDeliveryFailed)
		Expect(condition(rr, remediationv1alpha1.ConditionDelivered).Reason).To(Equal(ReasonDeliveryFailed))
	})

	// A failure inside the window that Flux retries and gets past is not held against the change.
	It("verifies a merge commit that failed inside the window and was applied later", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.base, attempted: w.merge, failed: true})
		mustReconcileAt(w, w.merged.Add(10*time.Minute))
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		mustReconcileAt(w, w.merged.Add(20*time.Minute))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
	})

	// Row 7: Flux failed on a later commit that holds the merge commit: DeliveryBlocked, not Failed.
	It("ends Inconclusive with DeliveryBlocked when a later commit failed", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.base, attempted: later(w, "apps/broken.yaml"), failed: true})
		mustReconcileAt(w, w.merged.Add(10*time.Minute))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		mustReconcileAt(w, windowEnd(w))
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonDeliveryBlocked)
		Expect(condition(rr, remediationv1alpha1.ConditionDelivered).Status).To(Equal(metav1.ConditionFalse))
	})

	// Row 8: the Kustomization is suspended.
	It("ends Inconclusive with DeliverySuspended at the end of the window", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.base, suspended: true})
		mustReconcileAt(w, w.merged.Add(10*time.Minute))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		mustReconcileAt(w, windowEnd(w))
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonDeliverySuspended)
	})

	// Rows 9 and 10: a recreated target ends TargetReplaced and a deleted one TargetGone, at once, in
	// WaitingForDelivery and in Verifying.
	replaceTarget := func(w *phasesWorld) {
		old := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, w.ssKey, old)).To(Succeed())
		Expect(k8sClient.Delete(ctx, old)).To(Succeed())
		Expect(k8sClient.Create(ctx, statefulSet(w.ssKey.Name, nil))).To(Succeed())
	}
	deleteTarget := func(w *phasesWorld) {
		Expect(k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: w.ssKey.Namespace,
			Name: w.ssKey.Name}})).To(Succeed())
	}
	for _, tc := range []struct {
		name   string
		change func(*phasesWorld)
		reason string
	}{{"recreated", replaceTarget, ReasonTargetReplaced}, {"deleted", deleteTarget, ReasonTargetGone}} {
		It(fmt.Sprintf("ends a request whose target was %s at once while it waits for the delivery", tc.name), func() {
			w := newWorld(nil)
			w.reader.ks = kustomization(ksStatus{applied: w.base})
			mustReconcileAt(w, w.merged.Add(time.Minute))
			Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
			tc.change(w)
			mustReconcileAt(w, w.merged.Add(2*time.Minute))
			expectEnd(w, remediationv1alpha1.PhaseInconclusive, tc.reason)
		})
		It(fmt.Sprintf("ends a request whose target was %s at once while it is verified", tc.name), func() {
			w := newWorld(nil)
			w.reader.ks = kustomization(ksStatus{applied: w.merge})
			mustReconcileAt(w, w.merged.Add(time.Minute))
			Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
			tc.change(w)
			mustReconcileAt(w, w.merged.Add(2*time.Minute))
			expectEnd(w, remediationv1alpha1.PhaseInconclusive, tc.reason)
		})
	}

	// Row 11: the delivery cannot be read to the end. A failed read is retried inside the window and its grace,
	// never a verdict, and past them it ends DeliveryNotChecked.
	It("ends Inconclusive with DeliveryNotChecked when the delivery cannot be read to the end", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.base})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		w.reader.fail = func(obj client.Object) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Kustomization" {
				return errors.New("the API server is unavailable")
			}
			return nil
		}
		_, err := reconcileAt(w, windowEnd(w).Add(finalGrace))
		Expect(err).To(HaveOccurred())
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		mustReconcileAt(w, windowEnd(w).Add(finalGrace+time.Second))
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonDeliveryNotChecked)
		Expect(condition(rr, remediationv1alpha1.ConditionDelivered).Status).To(Equal(metav1.ConditionUnknown))
	})

	// A template change after the rollout was recorded ends verification RolloutSuperseded at once.
	It("ends Inconclusive with RolloutSuperseded when the template changes after the rollout was recorded", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		rr := fetch(w)
		Expect(rr.Status.Rollout.TemplateRevision).To(Equal(revisionNew))
		Expect(meta.IsStatusConditionTrue(rr.Status.Conditions, rollout.ConditionRolledOut)).To(BeTrue())

		ss := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, w.ssKey, ss)).To(Succeed())
		ss.Spec.Template.Spec.Containers[0].Image = "example/worker:2"
		Expect(k8sClient.Update(ctx, ss)).To(Succeed())
		mustReconcileAt(w, w.merged.Add(3*time.Minute))
		rr = expectEnd(w, remediationv1alpha1.PhaseInconclusive, rollout.ReasonRolloutSuperseded)
		Expect(condition(rr, rollout.ConditionRolledOut).Reason).To(Equal(rollout.ReasonRolloutSuperseded))
	})

	// A partitioned StatefulSet: RolledOut True with reason RolloutPartitioned, naming the pods left on the old
	// revision.
	It("reports a partitioned StatefulSet RolloutPartitioned with the old pods named", func() {
		w := newWorld(func(ss *appsv1.StatefulSet) {
			ss.Spec.Replicas = ptr.To[int32](3)
			ss.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](2)}
		})
		setTargetStatus(w, 1, 3, revisionOld, revisionNew)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		c := condition(rr, rollout.ConditionRolledOut)
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(rollout.ReasonRolloutPartitioned))
		Expect(c.Message).To(ContainSubstring(w.ssKey.Name + "-1, " + w.ssKey.Name + "-0"))
	})

	// Until the executor reads the verifier's report (#683), a request that reaches its timeout ends Inconclusive
	// with ClearingNotChecked instead of staying Verifying. Every reconcile before reads the target again.
	It("ends Inconclusive with ClearingNotChecked at the timeout", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		applied := w.merged.Add(time.Minute)
		mustReconcileAt(w, applied)
		deadline := fetch(w).Status.Deadline.Time
		res := mustReconcileAt(w, deadline.Add(-time.Second))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		Expect(res.RequeueAfter).To(Equal(time.Second))
		// A restart forgets nothing: the new executor decides from the recorded deadline.
		w.r = w.reconciler()
		mustReconcileAt(w, deadline)
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonClearingNotChecked)
	})

	// A rollout still stuck at the timeout ends Failed with RolloutStuck.
	It("ends Failed with RolloutStuck when the rollout is stuck at the timeout", func() {
		w := newWorld(nil)
		setTargetStatus(w, 0, 1, revisionOld, revisionNew)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		applied := w.merged.Add(time.Minute)
		mustReconcileAt(w, applied)
		mustReconcileAt(w, applied.Add(rollout.DefaultDeadline+time.Second))
		Expect(condition(fetch(w), rollout.ConditionRolledOut).Reason).To(Equal(rollout.ReasonRolloutStuck))
		mustReconcileAt(w, fetch(w).Status.Deadline.Time)
		expectEnd(w, remediationv1alpha1.PhaseFailed, rollout.ReasonRolloutStuck)
	})

	// The kill switch pauses writes, not reads: while paused the executor still asks Flux and the Git host (the
	// ancestry of a later commit) and moves the request on, and the delivery window is extended by the paused time.
	It("keeps reading while writes are paused, and extends the delivery window by the pause", func() {
		w := newWorld(nil)
		pause := func(on bool) {
			s := &remediationv1alpha1.RemediationSettings{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, s)).To(Succeed())
			s.Spec.Paused = new(on)
			Expect(k8sClient.Update(ctx, s)).To(Succeed())
		}
		w.reader.ks = kustomization(ksStatus{applied: w.base})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		pause(true)
		pausedAt := w.merged.Add(10 * time.Minute)
		mustReconcileAt(w, pausedAt)
		Expect(fetch(w).Status.Rollout.PausedSince).NotTo(BeNil())
		// Past the hour the paused window is still open.
		mustReconcileAt(w, windowEnd(w).Add(30*time.Minute))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		pause(false)
		resumed := windowEnd(w).Add(30 * time.Minute)
		mustReconcileAt(w, resumed)
		rr := fetch(w)
		Expect(rr.Status.Rollout.PausedSeconds).To(BeNumerically("==", int64(resumed.Sub(pausedAt)/time.Second)))
		Expect(rr.Status.Deadline.Time).To(BeTemporally("==", windowEnd(w).Add(resumed.Sub(pausedAt))))
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))

		// Paused again: a later commit that holds the merge commit is still read through the Git host.
		pause(true)
		w.reader.ks = kustomization(ksStatus{applied: later(w, "apps/other.yaml")})
		mustReconcileAt(w, resumed.Add(time.Minute))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		pause(false)
	})

	// A clock set back never stretches the wait: the window cannot be measured, and the request ends
	// Inconclusive at once instead of waiting for as long as the clock went back.
	It("ends Inconclusive with DeliveryNotChecked when the executor's clock went back", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.base})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		mustReconcileAt(w, w.merged.Add(-mergeCommitClockSlack))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		mustReconcileAt(w, w.merged.Add(-2*time.Hour))
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonDeliveryNotChecked)
	})

	// Delivery is asked against the receipt: a settings entry that names another target branch since the merge
	// does not move the question.
	It("asks about the receipt's branch and repository, not the live settings", func() {
		w := newWorld(nil)
		s := &remediationv1alpha1.RemediationSettings{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, s)).To(Succeed())
		s.Spec.Repositories[0].TargetBranch = phasesOtherBranch
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		w.reader.ks = kustomization(ksStatus{applied: later(w, "apps/other.yaml")})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
	})
})

// reconciler is the executor of the world; a new one is a restart.
func (w *phasesWorld) reconciler() *RemediationRequestReconciler {
	return &RemediationRequestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: w.reader,
		Recorder: w.rec, Now: func() time.Time { return w.now },
		OpenRepository: func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return w.git, nil
		},
		Delivery: func(a providers.AncestryChecker) (DeliveryVerifier, error) {
			return fluxdelivery.New(w.reader, a)
		},
	}
}
