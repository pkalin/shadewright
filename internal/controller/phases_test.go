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

	"github.com/go-logr/logr/funcr"
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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fluxdelivery "github.com/pkalin/shadewright/internal/providers/delivery/flux"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
	verification "github.com/pkalin/shadewright/internal/verification/clearing"
	"github.com/pkalin/shadewright/internal/verification/rollout"
)

const (
	// phasesTargetPath is the file in the GitOps repo that holds the target of these specs.
	phasesTargetPath = "apps/cache.yaml"
	// phasesOtherBranch is a target branch that differs from the one in the receipt.
	phasesOtherBranch = "release-next"
	// kindStatefulSet is the kind of the target of these specs.
	kindStatefulSet = "StatefulSet"
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

// oomPodUID is the pod UID of the OOM kills the reports of these specs record.
const oomPodUID = "77777777-7777-4777-8777-777777777777"

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
			ReadyReplicas: ready, AvailableReplicas: ready, CurrentReplicas: *ss.Spec.Replicas, UpdatedReplicas: updated,
			CurrentRevision: current, UpdateRevision: update}
		Expect(k8sClient.Status().Update(ctx, ss)).To(Succeed())
	}

	// newGit is the GitOps repository of a world: a base commit and the merge commit after it.
	newGit := func(w *phasesWorld) {
		w.git = fakegit.NewGit(mainBranch, map[string][]byte{phasesTargetPath: []byte("old")})
		var err error
		w.base, err = w.git.BranchHead(ctx, mainBranch)
		Expect(err).NotTo(HaveOccurred())
		w.merge, err = w.git.Push(mainBranch, phasesTargetPath, []byte("new"))
		Expect(err).NotTo(HaveOccurred())
		w.rec = record.NewFakeRecorder(20)
		w.merged = time.Date(2026, 11, 8, 10, 35, 14, 0, time.UTC)
		w.now = w.merged.Add(time.Minute)
	}

	// mergedRequest creates rr and writes its status Merged at w.merged with its merge receipt.
	mergedRequest := func(w *phasesWorld, rr *remediationv1alpha1.RemediationRequest) {
		settings := fx.RemediationSettings()
		settings.Spec.Repositories[0].TargetBranch = mainBranch
		w.settingsRepo = freshness.RepositoryOf(settings.Spec.Repositories[0])
		Expect(k8sClient.Create(ctx, rr)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, rr))).To(Succeed())
		})
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
				URL: testPullRequest56URL, HeadSHA: head, MergeCommitSHA: w.merge, MergedAt: &mergedAt},
			MergeReceipt: &remediationv1alpha1.MergeReceipt{Repository: *w.settingsRepo, TargetBranch: mainBranch,
				PullRequest: 56, HeadSHA: head, MergeCommitSHA: w.merge, MergedAt: mergedAt},
		}
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		w.key = client.ObjectKeyFromObject(rr)
		w.r = w.reconciler()
	}

	// newWorldWith creates the target and a request Merged an hour's fraction ago with its merge receipt; edit
	// changes the target and editRequest the request before they are created.
	newWorldWith := func(edit func(*appsv1.StatefulSet), editRequest func(*remediationv1alpha1.RemediationRequest)) *phasesWorld {
		seq++
		name := fmt.Sprintf("phases-%d", seq)
		w := &phasesWorld{reader: &rolloutReader{Reader: k8sClient}}
		newGit(w)
		ss := statefulSet(name, edit)
		Expect(k8sClient.Create(ctx, ss)).To(Succeed())
		w.ssKey = client.ObjectKeyFromObject(ss)
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Namespace: w.ssKey.Namespace, Name: w.ssKey.Name}}))).To(Succeed())
		})
		setTargetStatus(w, 2, 2, revisionNew, revisionNew)

		rr := fx.MemoryLimitRequest(phasesNamespace, name)
		rr.Spec.TargetRef = remediationv1alpha1.ObjectRef{APIVersion: linkAPI, Kind: kindStatefulSet,
			Namespace: phasesNamespace, Name: name, UID: remediationv1alpha1.UID(ss.UID)}
		if editRequest != nil {
			editRequest(rr)
		}
		mergedRequest(w, rr)
		return w
	}
	// newWorld is newWorldWith without a change to the request.
	newWorld := func(edit func(*appsv1.StatefulSet)) *phasesWorld { return newWorldWith(edit, nil) }

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

	// Without a verifier no report is written: a request that reaches its timeout ends Inconclusive with
	// StaleReport instead of staying Verifying. Every reconcile before reads the target again.
	It("ends Inconclusive with StaleReport at the timeout without a report", func() {
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
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, string(remediationv1alpha1.ResetStaleReport))
		Expect(condition(rr, remediationv1alpha1.ConditionCleared).Status).To(Equal(metav1.ConditionUnknown))
		Expect(rr.Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetStaleReport))
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

	// --verify-outcomes off: a Merged request with a receipt keeps Merged with its rollout report (#532), as
	// before #681. One found in WaitingForDelivery or Verifying, which nothing moves on with the flag off, ends
	// Inconclusive with VerificationDisabled at the next reconcile, with no requeue, so no request stays in those
	// phases (and a probe in one of them does not hold the circuit breaker half-open for ever).
	It("keeps Merged and ends the later phases VerificationDisabled with --verify-outcomes off", func() {
		for _, tc := range []struct {
			phase   remediationv1alpha1.RemediationPhase
			applied func(*phasesWorld) string
		}{
			{remediationv1alpha1.PhaseMerged, func(w *phasesWorld) string { return w.base }},
			{remediationv1alpha1.PhaseWaitingForDelivery, func(w *phasesWorld) string { return w.base }},
			{remediationv1alpha1.PhaseVerifying, func(w *phasesWorld) string { return w.merge }},
		} {
			By(string(tc.phase))
			w := newWorld(nil)
			w.reader.ks = kustomization(ksStatus{applied: tc.applied(w)})
			if tc.phase != remediationv1alpha1.PhaseMerged {
				mustReconcileAt(w, w.merged.Add(time.Minute))
				Expect(fetch(w).Status.Phase).To(Equal(tc.phase))
			}
			w.r.VerifyOutcomes = false
			res := mustReconcileAt(w, w.merged.Add(2*time.Minute))
			if tc.phase == remediationv1alpha1.PhaseMerged {
				// The rollout report of #532: it waits for the delivery and never moves the phase.
				after := fetch(w)
				Expect(after.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
				Expect(res.RequeueAfter).To(BeNumerically(">", 0))
				Expect(meta.FindStatusCondition(after.Status.Conditions, remediationv1alpha1.ConditionDelivered)).To(BeNil())
				Expect(after.Status.Deadline).To(BeNil())
				continue
			}
			Expect(res).To(Equal(reconcile.Result{}))
			after := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonVerificationDisabled)
			Expect(condition(after, remediationv1alpha1.ConditionVerified).Message).To(ContainSubstring("--verify-outcomes"))
			Expect(after.Status.LastPhaseTransitionTime.Time).To(BeTemporally("==", w.merged.Add(2*time.Minute)))
			Expect(after.Status.HoldsTarget).To(Equal(new(false)))
		}
	})

	// Without a delivery verifier a request after the merge still gets a requeue or an end: Verifying needs no
	// verifier, and WaitingForDelivery retries the read and ends DeliveryNotChecked past the window and its grace,
	// naming the cause.
	It("keeps checking without a delivery verifier", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		w.r.Delivery = nil
		res := mustReconcileAt(w, w.merged.Add(2*time.Minute))
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		mustReconcileAt(w, fetch(w).Status.Deadline.Time)
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, string(remediationv1alpha1.ResetStaleReport))

		w = newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.base})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		w.r.Delivery = nil
		_, err := reconcileAt(w, w.merged.Add(2*time.Minute))
		Expect(err).To(HaveOccurred())
		Expect(condition(fetch(w), remediationv1alpha1.ConditionDelivered).Message).To(
			ContainSubstring("without a delivery verifier"))
		mustReconcileAt(w, windowEnd(w).Add(finalGrace+time.Second))
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonDeliveryNotChecked)
		Expect(condition(rr, remediationv1alpha1.ConditionVerified).Message).To(
			ContainSubstring("without a delivery verifier"))
	})

	// A GitOps controller that alternates between two applied commits on every read never lets the delivery be
	// recorded. Within the window it is checked again at once; past the window and its grace it ends
	// DeliveryNotChecked instead of waiting for ever.
	It("ends a delivery that keeps moving at the end of the window and its grace", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		w.reader.alternate = kustomization(ksStatus{applied: later(w, "apps/other.yaml")})
		res := mustReconcileAt(w, w.merged.Add(time.Minute))
		Expect(res.RequeueAfter).To(Equal(time.Second))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		_, err := reconcileAt(w, windowEnd(w).Add(time.Minute))
		Expect(err).To(HaveOccurred(), "past the window a delivery that moves is a failed read")
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		mustReconcileAt(w, windowEnd(w).Add(finalGrace+time.Second))
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonDeliveryNotChecked)
		Expect(condition(rr, remediationv1alpha1.ConditionVerified).Message).To(ContainSubstring("kept moving"))
	})

	// A cause that does not clear by itself (here: the receipt's repository left the settings) is retried for the
	// whole window, and named in Delivered while it waits and in the final message, not only in the log.
	It("names a lasting cause of a failed delivery read in the conditions", func() {
		w := newWorld(nil)
		s := &remediationv1alpha1.RemediationSettings{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, s)).To(Succeed())
		s.Spec.Repositories[0].URL = "https://github.com/example/elsewhere"
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		w.reader.ks = kustomization(ksStatus{applied: later(w, "apps/other.yaml")})
		_, err := w.r.afterMerge(ctx, fetch(w))
		Expect(err).To(HaveOccurred())
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
		Expect(condition(rr, remediationv1alpha1.ConditionDelivered).Message).To(
			ContainSubstring("not in RemediationSettings"))
		w.now = windowEnd(w).Add(finalGrace + time.Second)
		Expect(w.r.afterMerge(ctx, fetch(w))).Error().NotTo(HaveOccurred())
		rr = expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonDeliveryNotChecked)
		Expect(condition(rr, remediationv1alpha1.ConditionVerified).Message).To(
			ContainSubstring("not in RemediationSettings"))
	})

	// A target that cannot be read from the timeout to its grace keeps a rollout verdict recorded before: Failed
	// with RolloutStuck. With none recorded the end is Inconclusive with TargetNotChecked.
	failTarget := func(w *phasesWorld) {
		w.reader.fail = func(obj client.Object) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				return errors.New("the API server is unavailable")
			}
			return nil
		}
	}
	It("ends Failed with the recorded RolloutStuck when the target cannot be read at the timeout", func() {
		w := newWorld(nil)
		setTargetStatus(w, 0, 1, revisionOld, revisionNew)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		applied := w.merged.Add(time.Minute)
		mustReconcileAt(w, applied)
		mustReconcileAt(w, applied.Add(rollout.DefaultDeadline+time.Second))
		Expect(condition(fetch(w), rollout.ConditionRolledOut).Reason).To(Equal(rollout.ReasonRolloutStuck))
		deadline := fetch(w).Status.Deadline.Time
		failTarget(w)
		_, err := reconcileAt(w, deadline)
		Expect(err).To(HaveOccurred())
		mustReconcileAt(w, deadline.Add(finalGrace))
		expectEnd(w, remediationv1alpha1.PhaseFailed, rollout.ReasonRolloutStuck)
	})
	It("ends Inconclusive with TargetNotChecked when the target cannot be read to the end", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		deadline := fetch(w).Status.Deadline.Time
		failTarget(w)
		_, err := reconcileAt(w, deadline.Add(finalGrace-time.Second))
		Expect(err).To(HaveOccurred())
		mustReconcileAt(w, deadline.Add(finalGrace))
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonTargetNotChecked)
	})

	// A clock a little behind (a leader failover to a node whose clock is less than a minute behind) does not
	// end a live verification; one set back further ends it Inconclusive at once, never Failed.
	It("tolerates a clock a little behind in Verifying and ends at once when it went back further", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		applied := w.merged.Add(time.Minute)
		mustReconcileAt(w, applied)
		mustReconcileAt(w, applied.Add(-mergeCommitClockSlack))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		mustReconcileAt(w, applied.Add(-mergeCommitClockSlack-time.Second))
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonClearingNotChecked)
	})

	// The upgrade path: the applied time was recorded while the request was Merged (the rollout report of #532,
	// --verify-outcomes off), with a stuck rollout; the flag is turned on after the timeout measured from that
	// time. Nothing was verified in time: Inconclusive, never Failed and never a success.
	It("ends Inconclusive when outcome verification starts after the timeout of a recorded applied time", func() {
		w := newWorld(nil)
		setTargetStatus(w, 0, 1, revisionOld, revisionNew)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		w.r.VerifyOutcomes = false
		applied := w.merged.Add(time.Minute)
		mustReconcileAt(w, applied)
		mustReconcileAt(w, applied.Add(rollout.DefaultDeadline+time.Second))
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(rr.Status.PullRequest.AppliedObservedAt).NotTo(BeNil())
		Expect(condition(rr, rollout.ConditionRolledOut).Reason).To(Equal(rollout.ReasonRolloutStuck))
		w.r = w.reconciler()
		mustReconcileAt(w, applied.Add(2*time.Hour))
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonClearingNotChecked)
	})

	// The KEDA path: a ScaledObject target, read as an unstructured object with the get the executor holds on
	// the target kinds. Configured reads spec.maxReplicaCount; RolledOut is not checked; at the timeout the
	// ScaledObject must be ready by the KedaReady rule, else the request ends Failed with KedaNotReady.
	scaledObject := func(name string, uid types.UID, maxReplicas int, ready string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		Expect(u.UnmarshalJSON([]byte(fmt.Sprintf(`{
  "apiVersion": "keda.sh/v1alpha1", "kind": "ScaledObject",
  "metadata": {"name": %q, "namespace": %q, "uid": %q, "generation": 1,
    "labels": {"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}},
  "spec": {"scaleTargetRef": {"name": "worker"}, "maxReplicaCount": %d},
  "status": {"conditions": [{"type": "Ready", "status": %q, "reason": "ScaledObjectReady"}]}}`,
			name, phasesNamespace, uid, maxReplicas, ready)))).To(Succeed())
		return u
	}
	newKedaWorldWith := func(maxReplicas int, ready string, edit func(*remediationv1alpha1.RemediationRequest)) *phasesWorld {
		seq++
		name := fmt.Sprintf("phases-keda-%d", seq)
		w := &phasesWorld{reader: &rolloutReader{Reader: k8sClient}}
		newGit(w)
		uid := types.UID(fmt.Sprintf("44444444-4444-4444-8444-%012d", seq))
		w.reader.objects = []*unstructured.Unstructured{scaledObject(name, uid, maxReplicas, ready)}
		rr := fx.RemediationRequest(phasesNamespace, name)
		rr.Spec.TargetRef.Namespace = phasesNamespace
		rr.Spec.TargetRef.Name = name
		rr.Spec.TargetRef.UID = remediationv1alpha1.UID(uid)
		if edit != nil {
			edit(rr)
		}
		mergedRequest(w, rr)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		return w
	}
	newKedaWorld := func(maxReplicas int, ready string) *phasesWorld {
		return newKedaWorldWith(maxReplicas, ready, nil)
	}
	// stepAt runs the phases after the merge once at at, as the reconcile does after its earlier steps (which
	// read the target from the cache, and the test API server has no ScaledObject kind).
	stepAt := func(w *phasesWorld, at time.Time) reconcile.Result {
		w.now = at
		res, err := w.r.afterMerge(ctx, fetch(w))
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	// A ScaledObject cannot succeed (ADR-0030 section 3): once rolled out (KedaReady) and configured held for
	// stableFor (3 minutes for AdjustKedaMaxReplicas) it ends Inconclusive with ClearingNotChecked, before its
	// timeout.
	It("ends a ready ScaledObject Inconclusive with ClearingNotChecked after stableFor", func() {
		w := newKedaWorld(15, "True")
		applied := w.merged.Add(time.Minute)
		stepAt(w, applied)
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		Expect(rr.Status.Deadline.Time).To(BeTemporally("==", applied.Add(14*time.Minute)))
		Expect(condition(rr, remediationv1alpha1.ConditionConfigured).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(rr, rollout.ConditionRolledOut).Reason).To(Equal(rollout.ReasonKindNotChecked))
		Expect(condition(rr, remediationv1alpha1.ConditionCleared).Reason).To(Equal(ReasonClearingNotChecked))
		stepAt(w, applied.Add(3*time.Minute-time.Second))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		stepAt(w, applied.Add(3*time.Minute))
		rr = expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonClearingNotChecked)
		Expect(condition(rr, remediationv1alpha1.ConditionCleared).Status).To(Equal(metav1.ConditionUnknown))
		Expect(condition(rr, remediationv1alpha1.ConditionCleared).Reason).To(Equal(ReasonClearingNotChecked))
	})
	It("ends a revert of a ScaledObject Inconclusive with RevertApplied after stableFor", func() {
		w := newKedaWorldWith(15, "True", func(rr *remediationv1alpha1.RemediationRequest) {
			rr.Spec.Reverts = &remediationv1alpha1.RequestRef{Name: "remediation-failed",
				UID: "12121212-1212-4212-8212-121212121212"}
		})
		applied := w.merged.Add(time.Minute)
		stepAt(w, applied)
		Expect(condition(fetch(w), remediationv1alpha1.ConditionCleared).Reason).To(
			Equal(remediationv1alpha1.ReasonRevertApplied))
		stepAt(w, applied.Add(3*time.Minute))
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, remediationv1alpha1.ReasonRevertApplied)
		Expect(condition(rr, remediationv1alpha1.ConditionCleared).Status).To(Equal(metav1.ConditionUnknown))
		Expect(condition(rr, remediationv1alpha1.ConditionCleared).Reason).To(Equal(remediationv1alpha1.ReasonRevertApplied))
	})
	// A check that resets the window and whose status write is lost must not let the next check, which reads the
	// window from before it, end the request across the bad interval.
	It("keeps a ScaledObject's reset whose status write was lost, so it does not end across it", func() {
		w := newKedaWorld(15, "True")
		applied := w.merged.Add(time.Minute)
		stepAt(w, applied)
		ref := fetch(w).Spec.TargetRef
		w.reader.objects = []*unstructured.Unstructured{scaledObject(ref.Name, types.UID(ref.UID), 15, "False")}
		w.r.Client = &statusRecorder{Client: k8sClient,
			fail: func(*remediationv1alpha1.RemediationRequest) bool { return true }}
		bad := applied.Add(time.Minute)
		w.now = bad
		_, err := w.r.afterMerge(ctx, fetch(w))
		Expect(err).To(HaveOccurred())
		Expect(fetch(w).Status.Verification.LastReset).To(BeNil(), "the reset was not stored")
		w.r.Client = k8sClient
		w.reader.objects = []*unstructured.Unstructured{scaledObject(ref.Name, types.UID(ref.UID), 15, "True")}
		stepAt(w, applied.Add(3*time.Minute))
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying),
			"the stored window spans stableFor, but a check inside it was bad")
		Expect(rr.Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetNotAvailable))
		Expect(rr.Status.Verification.LastReset.Time.Time).To(BeTemporally("==", bad))
	})
	// A restart loses the resets kept in memory, so no window time from before it counts (ADR-0030 section 4,
	// amendment of 2026-11-13): the ScaledObject needs a full stableFor after the restart, and the stored
	// windowStart is kept.
	It("counts no ScaledObject window time from before an executor restart", func() {
		w := newKedaWorld(15, "True")
		applied := w.merged.Add(time.Minute)
		stepAt(w, applied)
		w.r = w.reconciler()
		restart := applied.Add(2 * time.Minute)
		stepAt(w, restart)
		before := fetch(w)
		stepAt(w, applied.Add(3*time.Minute))
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying),
			"the stored window spans stableFor, but only one minute of it after the restart")
		Expect(rr.Status.Verification.WindowStart.Time).To(BeTemporally("==", applied))
		Expect(rr.ResourceVersion).To(Equal(before.ResourceVersion), "no status write while the window runs")
		stepAt(w, restart.Add(3*time.Minute-time.Second))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		stepAt(w, restart.Add(3*time.Minute))
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonClearingNotChecked)
	})
	It("ends a ScaledObject that names QueueBacklogDecreasing Inconclusive with CheckNotAvailable", func() {
		w := newKedaWorldWith(15, "True", func(rr *remediationv1alpha1.RemediationRequest) {
			rr.Spec.Verification.Checks = []remediationv1alpha1.VerificationCheck{
				remediationv1alpha1.CheckQueueBacklogDecreasing}
		})
		applied := w.merged.Add(time.Minute)
		stepAt(w, applied)
		stepAt(w, applied.Add(3*time.Minute))
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonCheckNotAvailable)
	})
	It("ends Failed with KedaNotReady when the ScaledObject is not ready at the timeout", func() {
		w := newKedaWorld(15, "False")
		applied := w.merged.Add(time.Minute)
		stepAt(w, applied)
		deadline := fetch(w).Status.Deadline.Time
		stepAt(w, deadline.Add(-time.Second))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying), "not ready before the timeout")
		stepAt(w, deadline)
		rr := expectEnd(w, remediationv1alpha1.PhaseFailed, ReasonKedaNotReady)
		Expect(condition(rr, remediationv1alpha1.ConditionVerified).Message).To(ContainSubstring("NotReady"))
	})
	It("ends a ScaledObject that does not hold the desired maxReplicaCount ValueChanged", func() {
		w := newKedaWorld(12, "True")
		stepAt(w, w.merged.Add(time.Minute))
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonValueChanged)
	})

	// Cleared from the verifier's report (#683, ADR-0030 sections 4 and 7). The report is written here as the
	// verifier would; a memory fix has stableFor 10 minutes and the node grace 1 minute, so success needs both
	// spans at least 11 minutes.
	need := verification.MinStableForMemory + DefaultNodeGrace

	// putReport writes the report of w's request as the verifier would: sample n taken at sampledAt, its window
	// open since start, every desired replica covered. edit changes the object before it is created and before
	// its status is written.
	putReport := func(w *phasesWorld, n int64, sampledAt, start time.Time,
		edit func(*remediationv1alpha1.VerificationReport)) {
		rr := fetch(w)
		ss := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, w.ssKey, ss)).To(Succeed())
		key := types.NamespacedName{Namespace: phasesNamespace, Name: verification.ReportName(rr.UID)}
		vr := &remediationv1alpha1.VerificationReport{}
		err := k8sClient.Get(ctx, key, vr)
		if apierrors.IsNotFound(err) {
			vr = &remediationv1alpha1.VerificationReport{
				ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
				Spec: remediationv1alpha1.VerificationReportSpec{RequestUID: remediationv1alpha1.UID(rr.UID),
					MergeCommitSHA: w.merge, TargetUID: remediationv1alpha1.UID(ss.UID),
					TemplateRevision: rr.Status.Rollout.TemplateRevision, Container: workerContainer},
			}
			if edit != nil {
				edit(vr)
			}
			Expect(k8sClient.Create(ctx, vr)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vr))).To(Succeed()) })
		} else {
			Expect(err).NotTo(HaveOccurred())
		}
		at, from := metav1.NewTime(sampledAt), metav1.NewTime(start)
		vr.Status.Sample, vr.Status.SampledAt, vr.Status.WindowStart = n, &at, &from
		vr.Status.DesiredReplicas, vr.Status.PodsCovered = 2, 2
		if edit != nil {
			edit(vr)
		}
		Expect(k8sClient.Status().Update(ctx, vr)).To(Succeed())
	}
	// verifying runs w's request into Verifying at applied, one minute after the merge, with the rollout complete.
	verifying := func(w *phasesWorld) time.Time {
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		applied := w.merged.Add(time.Minute)
		mustReconcileAt(w, applied)
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		Expect(meta.IsStatusConditionTrue(rr.Status.Conditions, rollout.ConditionRolledOut)).To(BeTrue())
		return applied
	}
	// drive checks w's request every step from from while t is before until, writing a new sample before each
	// check with report (nil writes none), and expects it still Verifying after each. It returns the time of the
	// next check and the next sample number.
	drive := func(w *phasesWorld, from, until time.Time, step time.Duration, n int64,
		report func(t time.Time, n int64)) (time.Time, int64) {
		t := from
		for ; t.Before(until); t = t.Add(step) {
			if report != nil {
				report(t, n)
			}
			n++
			mustReconcileAt(w, t)
			Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying), "at %s", t.Sub(from))
		}
		return t, n
	}
	// quiet is a report whose window is open since start and sampled at each check.
	quiet := func(w *phasesWorld, start time.Time) func(time.Time, int64) {
		return func(t time.Time, n int64) { putReport(w, n, t, start, nil) }
	}

	It("ends Succeeded with Cleared True and Verified True on a quiet window with a fresh, matching report", func() {
		w := newWorld(nil)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		t, n := drive(w, start, start.Add(need), verification.PollInterval, 1, quiet(w, start))
		before := fetch(w)
		Expect(condition(before, remediationv1alpha1.ConditionCleared).Reason).To(Equal(ReasonObserving))
		Expect(before.Status.Verification.WindowStart.Time).To(BeTemporally("==", start))
		putReport(w, n, t, start, nil)
		mustReconcileAt(w, t)
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseSucceeded))
		cleared := condition(rr, remediationv1alpha1.ConditionCleared)
		Expect(cleared.Status).To(Equal(metav1.ConditionTrue))
		Expect(cleared.Reason).To(Equal(ReasonNoRecurrence))
		verified := condition(rr, remediationv1alpha1.ConditionVerified)
		Expect(verified.Status).To(Equal(metav1.ConditionTrue))
		Expect(verified.Reason).To(Equal(ReasonNoRecurrence))
		Expect(rr.Status.Deadline).To(BeNil())
		Expect(*rr.Status.BreakerCounts).To(BeTrue())
	})

	// The trust contract: a report that is no evidence never ends Succeeded, however long its window, and the
	// request ends Inconclusive with the executor's reason at the timeout.
	noEvidence := []struct {
		name   string
		reason remediationv1alpha1.ResetReason
		edit   func(w *phasesWorld) func(*remediationv1alpha1.VerificationReport)
	}{
		{"names another request", remediationv1alpha1.ResetReportMismatch, func(*phasesWorld) func(*remediationv1alpha1.VerificationReport) {
			return func(vr *remediationv1alpha1.VerificationReport) {
				vr.Spec.RequestUID = "99999999-9999-4999-8999-999999999999"
			}
		}},
		{"names another merge commit", remediationv1alpha1.ResetReportMismatch, func(w *phasesWorld) func(*remediationv1alpha1.VerificationReport) {
			return func(vr *remediationv1alpha1.VerificationReport) { vr.Spec.MergeCommitSHA = w.base }
		}},
		{"names another target", remediationv1alpha1.ResetReportMismatch, func(*phasesWorld) func(*remediationv1alpha1.VerificationReport) {
			return func(vr *remediationv1alpha1.VerificationReport) {
				vr.Spec.TargetUID = "88888888-8888-4888-8888-888888888888"
			}
		}},
		{"names another template revision", remediationv1alpha1.ResetReportMismatch, func(*phasesWorld) func(*remediationv1alpha1.VerificationReport) {
			return func(vr *remediationv1alpha1.VerificationReport) { vr.Spec.TemplateRevision = revisionOld }
		}},
		{"covers fewer pods than desired", remediationv1alpha1.ResetPartialRollout, func(*phasesWorld) func(*remediationv1alpha1.VerificationReport) {
			return func(vr *remediationv1alpha1.VerificationReport) { vr.Status.PodsCovered = 1 }
		}},
	}
	for _, tc := range noEvidence {
		It(fmt.Sprintf("never ends Succeeded on a report that %s, and ends Inconclusive with %s", tc.name, tc.reason), func() {
			w := newWorld(nil)
			applied := verifying(w)
			deadline := fetch(w).Status.Deadline.Time
			edit := tc.edit(w)
			start := applied.Add(30 * time.Second)
			drive(w, start, deadline, verification.PollInterval, 1, func(t time.Time, n int64) {
				putReport(w, n, t, start, edit)
			})
			mustReconcileAt(w, deadline)
			rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, string(tc.reason))
			Expect(condition(rr, remediationv1alpha1.ConditionCleared).Status).To(Equal(metav1.ConditionUnknown))
		})
	}

	It("never ends Succeeded on a stale report, and ends Inconclusive with StaleReport", func() {
		w := newWorld(nil)
		applied := verifying(w)
		deadline := fetch(w).Status.Deadline.Time
		start := applied.Add(30 * time.Second)
		// One sample, then the counter never rises again: stale after three poll intervals.
		putReport(w, 1, start, start, nil)
		drive(w, start, deadline, verification.PollInterval, 2, nil)
		rr := fetch(w)
		Expect(rr.Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetStaleReport))
		mustReconcileAt(w, deadline)
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, string(remediationv1alpha1.ResetStaleReport))
	})

	It("reads a report that was never sampled as stale", func() {
		w := newWorld(nil)
		applied := verifying(w)
		deadline := fetch(w).Status.Deadline.Time
		start := applied.Add(30 * time.Second)
		// Created, and its first sample never written (ADR-0030 amendment of 2026-11-13 on #682).
		rr := fetch(w)
		ss := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, w.ssKey, ss)).To(Succeed())
		vr := &remediationv1alpha1.VerificationReport{
			ObjectMeta: metav1.ObjectMeta{Namespace: phasesNamespace, Name: verification.ReportName(rr.UID)},
			Spec: remediationv1alpha1.VerificationReportSpec{RequestUID: remediationv1alpha1.UID(rr.UID),
				MergeCommitSHA: w.merge, TargetUID: remediationv1alpha1.UID(ss.UID),
				TemplateRevision: rr.Status.Rollout.TemplateRevision, Container: workerContainer},
		}
		Expect(k8sClient.Create(ctx, vr)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vr))).To(Succeed()) })
		t, _ := drive(w, start, start.Add(time.Second), verification.PollInterval, 1, nil)
		Expect(fetch(w).Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetStaleReport),
			"read as missing at once, not as a report covering no pods")
		drive(w, t, deadline, verification.PollInterval, 1, nil)
		Expect(fetch(w).Status.Verification.WindowStart).To(BeNil(), "no check was good")
		mustReconcileAt(w, deadline)
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, string(remediationv1alpha1.ResetStaleReport))
	})

	// The age of a report is its counter on the executor's clock, never sampledAt: a counter that rose only
	// since a check more than one poll interval earlier decides nothing, even with a sampledAt an hour ahead.
	It("lets no report older than one poll interval by the executor's clock decide success", func() {
		w := newWorld(nil)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		ahead := func(t time.Time, n int64) { putReport(w, n, t.Add(time.Hour), start, nil) }
		t, n := drive(w, start, start.Add(need+5*time.Minute), 45*time.Second, 1, ahead)
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		t = t.Add(-45 * time.Second).Add(verification.FastPollInterval)
		ahead(t, n)
		mustReconcileAt(w, t)
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseSucceeded))
	})

	It("ends Failed with OOMKilled at once when the report records an OOM kill", func() {
		w := newWorld(nil)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		t, n := drive(w, start, start.Add(2*time.Minute), verification.PollInterval, 1, quiet(w, start))
		putReport(w, n, t, start, func(vr *remediationv1alpha1.VerificationReport) {
			vr.Status.OOMKill = &remediationv1alpha1.OOMKill{PodUID: oomPodUID,
				Container: workerContainer}
		})
		mustReconcileAt(w, t)
		rr := expectEnd(w, remediationv1alpha1.PhaseFailed, ReasonOOMKilled)
		cleared := condition(rr, remediationv1alpha1.ConditionCleared)
		Expect(cleared.Status).To(Equal(metav1.ConditionFalse))
		Expect(cleared.Reason).To(Equal(ReasonOOMKilled))
	})

	// markStalePlan writes on w's merged request what a person's merge of a stale plan leaves when the Git host
	// refused its close (#199): PlanFresh False with the stale reason.
	markStalePlan := func(w *phasesWorld) {
		rr := fetch(w)
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionPlanFresh,
			Status: metav1.ConditionFalse, Reason: freshness.ReasonTargetFileChanged, Message: "The target file changed"})
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	}

	It("ends a merged stale plan Inconclusive with StalePlanMerged where it would end Succeeded (#199)", func() {
		w := newWorld(nil)
		markStalePlan(w)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		t, n := drive(w, start, start.Add(need), verification.PollInterval, 1, quiet(w, start))
		putReport(w, n, t, start, nil)
		mustReconcileAt(w, t)
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonStalePlanMerged)
		Expect(condition(rr, remediationv1alpha1.ConditionVerified).Message).To(ContainSubstring("#56"))
		cleared := condition(rr, remediationv1alpha1.ConditionCleared)
		Expect(cleared.Status).To(Equal(metav1.ConditionUnknown), "as any Inconclusive end from Verifying")
		Expect(cleared.Reason).To(Equal(ReasonStalePlanMerged))
		Expect(condition(rr, remediationv1alpha1.ConditionPlanFresh).Reason).To(Equal(freshness.ReasonTargetFileChanged))
	})

	It("still ends a merged stale plan Failed when the change fails (#199)", func() {
		w := newWorld(nil)
		markStalePlan(w)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		t, n := drive(w, start, start.Add(2*time.Minute), verification.PollInterval, 1, quiet(w, start))
		putReport(w, n, t, start, func(vr *remediationv1alpha1.VerificationReport) {
			vr.Status.OOMKill = &remediationv1alpha1.OOMKill{PodUID: oomPodUID,
				Container: workerContainer}
		})
		mustReconcileAt(w, t)
		expectEnd(w, remediationv1alpha1.PhaseFailed, ReasonOOMKilled)
	})

	It("ends a merged stale plan Inconclusive with StalePlanMerged once its rollout is reported with --verify-outcomes off (#199)", func() {
		w := newWorld(nil)
		markStalePlan(w)
		w.r.VerifyOutcomes = false
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonStalePlanMerged)
		Expect(meta.IsStatusConditionTrue(rr.Status.Conditions, rollout.ConditionRolledOut)).To(BeTrue(),
			"the rollout is still reported")
	})

	It("ends a merged stale plan Inconclusive with StalePlanMerged when its rollout fails with --verify-outcomes off (#199)", func() {
		w := newWorld(nil)
		markStalePlan(w)
		w.r.VerifyOutcomes = false
		w.reader.ks = kustomization(ksStatus{})
		mustReconcileAt(w, w.merged.Add(deliveryWindow+time.Second))
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonStalePlanMerged)
		c := condition(rr, rollout.ConditionRolledOut)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(rollout.ReasonDeliveryNotObserved))
	})

	It("keeps Merged for a plan that was fresh with --verify-outcomes off once its rollout is reported", func() {
		w := newWorld(nil)
		w.r.VerifyOutcomes = false
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		mustReconcileAt(w, w.merged.Add(time.Minute))
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
		Expect(meta.IsStatusConditionTrue(rr.Status.Conditions, rollout.ConditionRolledOut)).To(BeTrue())
	})

	// A revert (ADR-0030 section 8, #96): no report is read, the OOM rule and Cleared do not apply, and it ends
	// Inconclusive with RevertApplied once rolled out, configured and available held for stableFor, never
	// Succeeded.
	asRevert := func(rr *remediationv1alpha1.RemediationRequest) {
		rr.Spec.Reverts = &remediationv1alpha1.RequestRef{Name: "remediation-failed",
			UID: "12121212-1212-4212-8212-121212121212"}
	}
	expectRevertCleared := func(rr *remediationv1alpha1.RemediationRequest) {
		cleared := condition(rr, remediationv1alpha1.ConditionCleared)
		ExpectWithOffset(1, cleared.Status).To(Equal(metav1.ConditionUnknown))
		ExpectWithOffset(1, cleared.Reason).To(Equal(remediationv1alpha1.ReasonRevertApplied))
	}

	It("ends a revert Inconclusive with RevertApplied after stableFor, without a report and never Succeeded", func() {
		w := newWorldWith(nil, asRevert)
		applied := verifying(w)
		stableFor := verification.MinStableForMemory
		t, _ := drive(w, applied.Add(verification.PollInterval), applied.Add(stableFor), verification.PollInterval, 1, nil)
		during := fetch(w)
		expectRevertCleared(during)
		Expect(during.Status.Verification.WindowStart.Time).To(BeTemporally("==", applied),
			"the window opened at the first check, with no report")
		Expect(during.Status.Verification.LastReset).To(BeNil(), "a missing report is no reset for a revert")
		mustReconcileAt(w, t)
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, remediationv1alpha1.ReasonRevertApplied)
		expectRevertCleared(rr)
		Expect(rr.Status.HoldsTarget).To(Equal(new(false)))
	})

	It("never fails a revert for an OOM kill a report records", func() {
		w := newWorldWith(nil, asRevert)
		applied := verifying(w)
		putReport(w, 1, applied, applied, func(vr *remediationv1alpha1.VerificationReport) {
			vr.Status.OOMKill = &remediationv1alpha1.OOMKill{PodUID: "78787878-7878-4878-8878-787878787878",
				Container: workerContainer}
		})
		t, _ := drive(w, applied.Add(verification.PollInterval), applied.Add(verification.MinStableForMemory),
			verification.PollInterval, 2, nil)
		mustReconcileAt(w, t)
		expectRevertCleared(expectEnd(w, remediationv1alpha1.PhaseInconclusive, remediationv1alpha1.ReasonRevertApplied))
	})

	It("ends a revert that is not available Inconclusive with NotAvailable at the timeout", func() {
		w := newWorldWith(nil, asRevert)
		applied := verifying(w)
		deadline := fetch(w).Status.Deadline.Time
		setTargetStatus(w, 2, 1, revisionNew, revisionNew)
		drive(w, applied.Add(verification.PollInterval), deadline, verification.PollInterval, 1, nil)
		Expect(fetch(w).Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetNotAvailable))
		mustReconcileAt(w, deadline)
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, string(remediationv1alpha1.ResetNotAvailable))
		expectRevertCleared(rr)
	})

	It("resets a revert's window while it is not rolled out, so an earlier span does not carry over", func() {
		w := newWorldWith(nil, asRevert)
		setTargetStatus(w, 0, 2, revisionOld, revisionNew)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		applied := w.merged.Add(time.Minute)
		mustReconcileAt(w, applied)
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		Expect(meta.IsStatusConditionTrue(rr.Status.Conditions, rollout.ConditionRolledOut)).To(BeFalse())
		// A window recorded earlier and open, a second after the applied time (and after any reset there).
		start := metav1.NewTime(applied.Add(time.Second))
		if rr.Status.Verification == nil {
			rr.Status.Verification = &remediationv1alpha1.VerificationWindow{}
		}
		rr.Status.Verification.WindowStart = &start
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())

		mustReconcileAt(w, applied.Add(verification.PollInterval))
		setTargetStatus(w, 2, 2, revisionNew, revisionNew)
		rolledOut := start.Add(verification.MinStableForMemory)
		mustReconcileAt(w, rolledOut)
		rr = fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying),
			"the window opens when the rollout completes, not at the earlier start")
		Expect(rr.Status.Verification.WindowStart.Time).To(BeTemporally("==", rolledOut))
		Expect(rr.Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetPartialRollout))
	})

	It("never counts a revert of a target scaled to zero as available", func() {
		w := newWorldWith(nil, asRevert)
		applied := verifying(w)
		ss := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, w.ssKey, ss)).To(Succeed())
		ss.Spec.Replicas = ptr.To[int32](0)
		Expect(k8sClient.Update(ctx, ss)).To(Succeed())
		setTargetStatus(w, 0, 0, revisionNew, revisionNew)
		deadline := fetch(w).Status.Deadline.Time
		drive(w, applied.Add(verification.PollInterval), deadline, verification.PollInterval, 1, nil)
		Expect(fetch(w).Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetNotAvailable))
		mustReconcileAt(w, deadline)
		expectRevertCleared(expectEnd(w, remediationv1alpha1.PhaseInconclusive,
			string(remediationv1alpha1.ResetNotAvailable)))
	})

	// stableFor 10m: the window opens at applied; halfway the target is not available and that check's status
	// write is lost; at applied+stableFor the target is available again. The stored window spans stableFor, but
	// the check that reset it is kept in memory, so the revert does not end across the bad interval.
	It("keeps a revert's reset whose status write was lost, so it does not end across it", func() {
		w := newWorldWith(nil, asRevert)
		applied := verifying(w)
		stableFor := verification.MinStableForMemory
		bad := applied.Add(stableFor / 2)
		drive(w, applied.Add(verification.PollInterval), bad, verification.PollInterval, 1, nil)
		setTargetStatus(w, 2, 1, revisionNew, revisionNew)
		w.r.Client = &statusRecorder{Client: k8sClient,
			fail: func(*remediationv1alpha1.RemediationRequest) bool { return true }}
		_, err := reconcileAt(w, bad)
		Expect(err).To(HaveOccurred())
		Expect(fetch(w).Status.Verification.LastReset).To(BeNil(), "the reset was not stored")
		w.r.Client = k8sClient
		setTargetStatus(w, 2, 2, revisionNew, revisionNew)
		mustReconcileAt(w, applied.Add(stableFor))
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying),
			"the stored window spans stableFor, but a check inside it was bad")
		Expect(rr.Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetNotAvailable))
		Expect(rr.Status.Verification.LastReset.Time.Time).To(BeTemporally("==", bad))
	})

	// The scenario a lost reset leaves after a restart: halfway the target is not available, that check's
	// status write is lost and the executor restarts. The new process has no memory of the reset, so it counts
	// the window only from its own start (ADR-0030 section 4, amendment of 2026-11-13) and the revert does not
	// end across the bad interval; it ends a full stableFor after the restart, the stored windowStart kept.
	It("counts no revert window time from before an executor restart that lost a reset", func() {
		w := newWorldWith(nil, asRevert)
		applied := verifying(w)
		stableFor := verification.MinStableForMemory
		bad := applied.Add(stableFor / 2)
		drive(w, applied.Add(verification.PollInterval), bad, verification.PollInterval, 1, nil)
		setTargetStatus(w, 2, 1, revisionNew, revisionNew)
		w.r.Client = &statusRecorder{Client: k8sClient,
			fail: func(*remediationv1alpha1.RemediationRequest) bool { return true }}
		_, err := reconcileAt(w, bad)
		Expect(err).To(HaveOccurred())
		w.r = w.reconciler()
		setTargetStatus(w, 2, 2, revisionNew, revisionNew)
		restart := bad.Add(verification.PollInterval)
		mustReconcileAt(w, restart)
		before := fetch(w)
		t, _ := drive(w, restart.Add(verification.PollInterval), restart.Add(stableFor), verification.PollInterval, 1, nil)
		rr := fetch(w)
		Expect(rr.Status.Verification.WindowStart.Time).To(BeTemporally("==", applied), "windowStart is not rewritten")
		Expect(rr.Status.Verification.LastReset).To(BeNil(), "the lost reset is gone with the old process")
		Expect(rr.ResourceVersion).To(Equal(before.ResourceVersion), "no status write while the window runs")
		mustReconcileAt(w, t)
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, remediationv1alpha1.ReasonRevertApplied)
	})

	It("ends a revert VerificationDisabled with --verify-outcomes off, Cleared still RevertApplied", func() {
		w := newWorldWith(nil, asRevert)
		applied := verifying(w)
		w.r.VerifyOutcomes = false
		mustReconcileAt(w, applied.Add(verification.PollInterval))
		expectRevertCleared(expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonVerificationDisabled))
	})

	// The timeout table: trouble the report recorded fails the request whatever its last reset; a pod that is
	// not Ready without a restart is no evidence and gives PodNotReady.
	It("ends Failed with Restarted at the timeout when the report recorded trouble", func() {
		w := newWorld(nil)
		applied := verifying(w)
		deadline := fetch(w).Status.Deadline.Time
		start := applied.Add(30 * time.Second)
		drive(w, start, deadline, verification.PollInterval, 1, func(t time.Time, n int64) {
			putReport(w, n, t, start, func(vr *remediationv1alpha1.VerificationReport) {
				if n < 4 {
					return
				}
				// A crash loop after the window opened: a recorded Restarted, then PodNotReady in between.
				trouble := metav1.NewTime(start.Add(time.Minute))
				vr.Status.TroubleResets = 1
				vr.Status.LastTroubleReset = &remediationv1alpha1.WindowReset{Reason: remediationv1alpha1.ResetRestarted,
					Time: trouble}
				vr.Status.Resets = 2
				vr.Status.LastReset = &remediationv1alpha1.WindowReset{Reason: remediationv1alpha1.ResetPodNotReady,
					Time: metav1.NewTime(t)}
			})
		})
		mustReconcileAt(w, deadline)
		rr := expectEnd(w, remediationv1alpha1.PhaseFailed, ReasonRestarted)
		Expect(condition(rr, remediationv1alpha1.ConditionCleared).Status).To(Equal(metav1.ConditionFalse))
	})

	It("ends Inconclusive with PodNotReady at the timeout when a pod is not Ready without a restart", func() {
		w := newWorld(nil)
		applied := verifying(w)
		deadline := fetch(w).Status.Deadline.Time
		start := applied.Add(30 * time.Second)
		t, n := drive(w, start, start.Add(2*time.Minute), verification.PollInterval, 1, quiet(w, start))
		setTargetStatus(w, 2, 1, revisionNew, revisionNew)
		drive(w, t, deadline, verification.PollInterval, n, func(t time.Time, n int64) {
			putReport(w, n, t, start, func(vr *remediationv1alpha1.VerificationReport) {
				vr.Status.Resets = 1
				vr.Status.LastReset = &remediationv1alpha1.WindowReset{Reason: remediationv1alpha1.ResetPodNotReady,
					Time: metav1.NewTime(t)}
			})
		})
		Expect(fetch(w).Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetNotAvailable))
		mustReconcileAt(w, deadline)
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, string(remediationv1alpha1.ResetPodNotReady))
	})

	It("ends a Deployment-backed request that names QueueBacklogDecreasing Inconclusive with CheckNotAvailable", func() {
		seq++
		name := fmt.Sprintf("phases-%d", seq)
		w := &phasesWorld{reader: &rolloutReader{Reader: k8sClient}}
		newGit(w)
		ss := statefulSet(name, nil)
		Expect(k8sClient.Create(ctx, ss)).To(Succeed())
		w.ssKey = client.ObjectKeyFromObject(ss)
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ss))).To(Succeed()) })
		setTargetStatus(w, 2, 2, revisionNew, revisionNew)
		rr := fx.MemoryLimitRequest(phasesNamespace, name)
		rr.Spec.TargetRef = remediationv1alpha1.ObjectRef{APIVersion: linkAPI, Kind: kindStatefulSet,
			Namespace: phasesNamespace, Name: name, UID: remediationv1alpha1.UID(ss.UID)}
		rr.Spec.Verification.Checks = []remediationv1alpha1.VerificationCheck{
			remediationv1alpha1.CheckQueueBacklogDecreasing}
		mergedRequest(w, rr)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		t, n := drive(w, start, start.Add(need), verification.PollInterval, 1, quiet(w, start))
		putReport(w, n, t, start, nil)
		mustReconcileAt(w, t)
		expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonCheckNotAvailable)
	})

	// A restart of the executor loses the resets it kept in memory (unwrittenResets), so the new process counts
	// no window time from before it began reconciling (ADR-0030 section 4, amendment of 2026-11-13): the stored
	// window already spans stableFor plus the node grace, yet success needs that much again after the restart.
	// The stored windowStart is kept as it is and nothing is written while the window runs.
	It("counts no window time from before an executor restart and keeps the stored windowStart", func() {
		w := newWorldWith(nil, func(rr *remediationv1alpha1.RemediationRequest) {
			rr.Spec.Verification.Timeout = &metav1.Duration{Duration: time.Hour}
		})
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		t, n := drive(w, start, start.Add(need), verification.PollInterval, 1, quiet(w, start))
		w.r = w.reconciler()
		restart := t
		putReport(w, n, t, start, nil)
		mustReconcileAt(w, t)
		before := fetch(w)
		Expect(before.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		t, n = drive(w, restart.Add(verification.PollInterval), restart.Add(need), verification.PollInterval, n+1,
			quiet(w, start))
		rr := fetch(w)
		Expect(rr.Status.Verification.WindowStart.Time).To(BeTemporally("==", start), "windowStart is not rewritten")
		Expect(rr.ResourceVersion).To(Equal(before.ResourceVersion), "no status write while the window runs")
		putReport(w, n, t, start, nil)
		mustReconcileAt(w, t)
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseSucceeded))
	})

	// The cost of that rule: a restart closer to the timeout than stableFor plus the node grace leaves no time
	// for a full window, so the request ends at its timeout Inconclusive (here with StaleReport, the last reset
	// of its window), never Failed and never Succeeded. Without the rule the stored window spans stableFor plus
	// the node grace at the deadline and the request would succeed.
	It("ends Inconclusive at the timeout when a restart leaves no time for a full window", func() {
		w := newWorld(nil)
		applied := verifying(w)
		deadline := fetch(w).Status.Deadline.Time
		start := deadline.Add(-need)
		Expect(start.After(applied.Add(verification.PollInterval))).To(BeTrue())
		t, n := drive(w, applied.Add(verification.PollInterval), start, verification.PollInterval, 1, nil)
		t, n = drive(w, t, deadline.Add(-need/2), verification.PollInterval, n, quiet(w, t))
		opened := fetch(w).Status.Verification.WindowStart.Time
		w.r = w.reconciler()
		t, n = drive(w, t, deadline, verification.PollInterval, n, quiet(w, opened))
		Expect(t).To(BeTemporally("==", deadline))
		putReport(w, n, t, opened, nil)
		mustReconcileAt(w, t)
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, string(remediationv1alpha1.ResetStaleReport))
		Expect(rr.Status.Verification.WindowStart.Time).To(BeTemporally("==", opened))
	})

	// The executor's own window only moves forward (validation rules on the object).
	It("refuses a status.verification that moves back", func() {
		w := newWorld(nil)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		drive(w, start, start.Add(time.Minute), verification.PollInterval, 1, quiet(w, start))
		rr := fetch(w)
		Expect(rr.Status.Verification.WindowStart).NotTo(BeNil())
		Expect(rr.Status.Verification.LastReset).NotTo(BeNil())
		Expect(rr.Status.Verification.Resets).To(BeNumerically(">", 0))
		refusals := map[string]func(v *remediationv1alpha1.VerificationWindow) *remediationv1alpha1.VerificationWindow{
			"windowStart only moves forward": func(v *remediationv1alpha1.VerificationWindow) *remediationv1alpha1.VerificationWindow {
				back := metav1.NewTime(v.WindowStart.Add(-time.Minute))
				v.WindowStart = &back
				return v
			},
			"resets never decreases": func(v *remediationv1alpha1.VerificationWindow) *remediationv1alpha1.VerificationWindow {
				v.Resets--
				return v
			},
			"lastReset is kept once set and its time never moves back": func(v *remediationv1alpha1.VerificationWindow) *remediationv1alpha1.VerificationWindow {
				v.LastReset.Time = metav1.NewTime(v.LastReset.Time.Add(-time.Minute))
				return v
			},
			"verification is kept once set": func(*remediationv1alpha1.VerificationWindow) *remediationv1alpha1.VerificationWindow {
				return nil
			},
		}
		for message, edit := range refusals {
			changed := fetch(w)
			changed.Status.Verification = edit(changed.Status.Verification.DeepCopy())
			err := k8sClient.Status().Update(ctx, changed)
			Expect(err).To(HaveOccurred(), message)
			Expect(err.Error()).To(ContainSubstring(message))
		}
		forward := fetch(w)
		later := metav1.NewTime(forward.Status.Verification.WindowStart.Add(time.Minute))
		forward.Status.Verification.WindowStart = &later
		forward.Status.Verification.Resets++
		Expect(k8sClient.Status().Update(ctx, forward)).To(Succeed(), "moving forward is allowed")
	})

	// A bad check that repeats the reason of the last reset on a closed window changes nothing: no reset is
	// counted and no status is written, so the write cannot queue the request again at once and spin until the
	// timeout. Another reason, or a reset that closes an open window, is counted and written.
	It("writes no status for a repeated bad check with the same reason and counts no reset", func() {
		w := newWorld(nil)
		applied := verifying(w)
		Expect(fetch(w).Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetStaleReport))
		Expect(fetch(w).Status.Verification.Resets).To(Equal(int64(1)))
		rec := &statusRecorder{Client: k8sClient}
		w.r.Client = rec
		t := applied
		for range 5 {
			t = t.Add(verification.PollInterval)
			res := mustReconcileAt(w, t)
			Expect(res.RequeueAfter).To(Equal(verification.PollInterval))
		}
		Expect(rec.written()).To(BeEmpty(), "no report, the same StaleReport on every check")
		Expect(fetch(w).Status.Verification.Resets).To(Equal(int64(1)))

		// Another reason is counted and written once, then repeats write nothing.
		start := t
		mismatch := func(vr *remediationv1alpha1.VerificationReport) { vr.Spec.MergeCommitSHA = w.base }
		drive(w, t.Add(verification.PollInterval), t.Add(4*verification.PollInterval), verification.PollInterval,
			1, func(t time.Time, n int64) { putReport(w, n, t, start, mismatch) })
		Expect(rec.written()).To(HaveLen(1))
		v := fetch(w).Status.Verification
		Expect(v.LastReset.Reason).To(Equal(remediationv1alpha1.ResetReportMismatch))
		Expect(v.Resets).To(Equal(int64(2)))
	})

	// The timeout verdict decides Cleared, not the reason string: a Restarted reset the report did not record as
	// trouble holds the window closed and ends Inconclusive, with Cleared Unknown.
	It("ends Inconclusive with Restarted and Cleared Unknown when no trouble was recorded", func() {
		w := newWorld(nil)
		applied := verifying(w)
		deadline := fetch(w).Status.Deadline.Time
		start := applied.Add(30 * time.Second)
		drive(w, start, deadline, verification.PollInterval, 1, func(t time.Time, n int64) {
			putReport(w, n, t, start, func(vr *remediationv1alpha1.VerificationReport) {
				if n < 2 {
					return
				}
				// A restart before the window first opened: a reset, never recorded as trouble.
				vr.Status.Resets = 1
				vr.Status.LastReset = &remediationv1alpha1.WindowReset{Reason: remediationv1alpha1.ResetRestarted,
					Time: metav1.NewTime(start.Add(10 * time.Second))}
			})
		})
		mustReconcileAt(w, deadline)
		rr := expectEnd(w, remediationv1alpha1.PhaseInconclusive, ReasonRestarted)
		cleared := condition(rr, remediationv1alpha1.ConditionCleared)
		Expect(cleared.Status).To(Equal(metav1.ConditionUnknown))
		Expect(cleared.Message).NotTo(ContainSubstring("above its baseline"))
		Expect(condition(rr, remediationv1alpha1.ConditionVerified).Message).NotTo(ContainSubstring("above its baseline"))
	})

	failReport := func(w *phasesWorld, err error) {
		w.reader.fail = func(obj client.Object) error {
			if _, ok := obj.(*remediationv1alpha1.VerificationReport); ok {
				return err
			}
			return nil
		}
	}
	drainEvents := func(w *phasesWorld) []string {
		var events []string
		for {
			select {
			case e := <-w.rec.Events:
				events = append(events, e)
			default:
				return events
			}
		}
	}

	It("reports a rollout first seen stuck when the report read of that check fails", func() {
		w := newWorld(nil)
		setTargetStatus(w, 0, 1, revisionOld, revisionNew)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		applied := w.merged.Add(time.Minute)
		mustReconcileAt(w, applied)
		drainEvents(w)
		failReport(w, errors.New("the API server is unavailable"))
		_, err := reconcileAt(w, applied.Add(rollout.DefaultDeadline+time.Second))
		Expect(err).To(HaveOccurred())
		Expect(condition(fetch(w), rollout.ConditionRolledOut).Reason).To(Equal(rollout.ReasonRolloutStuck))
		Expect(drainEvents(w)).To(ContainElement(HavePrefix("Warning " + rollout.ReasonRolloutStuck)))
	})

	// Freshness is judged at the end of the check's reads: a report read that took a minute leaves a report
	// older than one poll interval by the executor's clock, which decides nothing.
	It("judges freshness after a slow report read", func() {
		w := newWorld(nil)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		t, n := drive(w, start, start.Add(need), verification.PollInterval, 1, quiet(w, start))
		w.reader.fail = func(obj client.Object) error {
			if _, ok := obj.(*remediationv1alpha1.VerificationReport); ok {
				w.now = w.now.Add(time.Minute)
			}
			return nil
		}
		putReport(w, n, t, start, nil)
		mustReconcileAt(w, t)
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
	})

	// A counter is remembered at the time taken before its read. After a restart of the executor (no counter
	// memory), a read that starts at t0 returns sample n a minute later; the verifier wrote n+1 at t0+1 s and
	// stopped. The rise seen at t0+75 s is evidence up to 75 s old, more than one poll interval: no success.
	It("judges a rise against the start of the previous slow read", func() {
		w := newWorld(nil)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		t0, n := drive(w, start, start.Add(need), verification.PollInterval, 1, quiet(w, start))
		w.r.reportCounters.forget(w.key)
		putReport(w, n, t0, start, nil)
		w.reader.fail = func(obj client.Object) error {
			if _, ok := obj.(*remediationv1alpha1.VerificationReport); ok {
				w.now = w.now.Add(time.Minute)
			}
			return nil
		}
		mustReconcileAt(w, t0)
		w.reader.fail = nil
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
		putReport(w, n+1, t0.Add(time.Second), start, nil)
		mustReconcileAt(w, t0.Add(75*time.Second))
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying),
			"a report up to 75 s old decided success")
	})

	// The same restart with fast reads every 15 s: the first check proves nothing, the next rise succeeds.
	It("succeeds one fast check after a restart of the executor", func() {
		w := newWorld(nil)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		t0, n := drive(w, start, start.Add(need), verification.PollInterval, 1, quiet(w, start))
		w.r.reportCounters.forget(w.key)
		putReport(w, n, t0, start, nil)
		mustReconcileAt(w, t0)
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying), "a first check proves nothing")
		t1 := t0.Add(verification.FastPollInterval)
		putReport(w, n+1, t1, start, nil)
		mustReconcileAt(w, t1)
		Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseSucceeded))
	})

	// The same lost write on the Cleared path: the report stays quiet (NotAvailable is the executor's own reset),
	// so only the executor's window can hold the request back.
	It("keeps a reset whose status write was lost, so the request does not succeed across it", func() {
		w := newWorld(nil)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		bad, n := drive(w, start, start.Add(need/2), verification.PollInterval, 1, quiet(w, start))
		setTargetStatus(w, 2, 1, revisionNew, revisionNew)
		putReport(w, n, bad, start, nil)
		stored := fetch(w).Status.Verification
		w.r.Client = &statusRecorder{Client: k8sClient,
			fail: func(*remediationv1alpha1.RemediationRequest) bool { return true }}
		_, err := reconcileAt(w, bad)
		Expect(err).To(HaveOccurred())
		Expect(fetch(w).Status.Verification).To(Equal(stored), "the reset was not stored")
		w.r.Client = k8sClient
		setTargetStatus(w, 2, 2, revisionNew, revisionNew)
		t, n := drive(w, bad.Add(verification.PollInterval), start.Add(need), verification.PollInterval, n+1,
			quiet(w, start))
		putReport(w, n, t, start, nil)
		mustReconcileAt(w, t)
		rr := fetch(w)
		Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying),
			"the stored window spans the need, but a check inside it was bad")
		Expect(rr.Status.Verification.LastReset.Reason).To(Equal(remediationv1alpha1.ResetNotAvailable))
		Expect(rr.Status.Verification.LastReset.Time.Time).To(BeTemporally("==", bad))
	})

	It("drops a kept reset when another writer ended the request", func() {
		w := newWorld(nil)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		bad, n := drive(w, start, start.Add(need/2), verification.PollInterval, 1, quiet(w, start))
		setTargetStatus(w, 2, 1, revisionNew, revisionNew)
		putReport(w, n, bad, start, nil)
		w.r.Client = &statusRecorder{Client: k8sClient,
			fail: func(*remediationv1alpha1.RemediationRequest) bool { return true }}
		_, err := reconcileAt(w, bad)
		Expect(err).To(HaveOccurred())
		w.r.Client = k8sClient
		_, kept := w.r.unwrittenResets.get(fetch(w))
		Expect(kept).To(BeTrue(), "the reset of the lost write is kept")

		rr := fetch(w)
		rr.Status.Phase = remediationv1alpha1.PhaseInconclusive
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		mustReconcileAt(w, bad.Add(verification.PollInterval))
		_, kept = w.r.unwrittenResets.get(fetch(w))
		Expect(kept).To(BeFalse(), "an ended request keeps no reset")
	})

	// ADR-0030 section 4: available replicas equal spec.replicas; more than desired is no quiet window.
	It("resets the executor's window when more replicas are available than desired", func() {
		w := newWorld(nil)
		applied := verifying(w)
		start := applied.Add(30 * time.Second)
		t, n := drive(w, start, start.Add(2*time.Minute), verification.PollInterval, 1, quiet(w, start))
		ss := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, w.ssKey, ss)).To(Succeed())
		ss.Status.Replicas, ss.Status.ReadyReplicas, ss.Status.AvailableReplicas = 3, 3, 3
		Expect(k8sClient.Status().Update(ctx, ss)).To(Succeed())
		putReport(w, n, t, start, nil)
		mustReconcileAt(w, t)
		v := fetch(w).Status.Verification
		Expect(v.LastReset.Reason).To(Equal(remediationv1alpha1.ResetNotAvailable))
		Expect(v.LastReset.Time.Time).To(BeTemporally("==", t))
	})

	It("refuses a status.verification with lastReset and no resets", func() {
		w := newWorld(nil)
		rr := fetch(w)
		rr.Status.Verification = &remediationv1alpha1.VerificationWindow{LastReset: &remediationv1alpha1.VerificationReset{
			Reason: remediationv1alpha1.ResetNotAvailable, Time: metav1.NewTime(w.merged)}}
		err := k8sClient.Status().Update(ctx, rr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("lastReset requires resets of at least 1"))
		rr.Status.Verification.Resets = 1
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	// A failed status write in the retry branch keeps the report read's error, and a Forbidden read is logged at
	// error level at once, naming the missing grant.
	It("joins a failed status write with the report read error and logs a Forbidden read", func() {
		w := newWorld(nil)
		w.reader.ks = kustomization(ksStatus{applied: w.merge})
		failReport(w, apierrors.NewForbidden(remediationv1alpha1.GroupVersion.WithResource("verificationreports").GroupResource(),
			"vr", errors.New("no grant")))
		w.r.Client = &statusRecorder{Client: k8sClient, fail: func(rr *remediationv1alpha1.RemediationRequest) bool {
			stored := &remediationv1alpha1.RemediationRequest{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rr), stored)).To(Succeed())
			return stored.Status.Phase == remediationv1alpha1.PhaseVerifying
		}}
		var errorLogs []string
		logger := funcr.New(func(_, args string) {
			if strings.Contains(args, `"error"=`) {
				errorLogs = append(errorLogs, args)
			}
		}, funcr.Options{})
		w.now = w.merged.Add(time.Minute)
		_, err := w.r.Reconcile(logf.IntoContext(ctx, logger), reconcile.Request{NamespacedName: w.key})
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the read error is kept: %v", err)
		Expect(err.Error()).To(ContainSubstring("status write lost"))
		Expect(errorLogs).To(ContainElement(ContainSubstring("needs get on verificationreports")))
	})

	// Resolved before the rollout (ADR-0030 section 4, #684): two readings of the Incident's count decide only
	// the reason of Cleared True.
	Context("with the request's Incident", func() {
		isIncident := func(obj client.Object) bool {
			_, ok := obj.(*remediationv1alpha1.Incident)
			return ok
		}
		failIncident := func(w *phasesWorld, fail bool) {
			w.reader.fail = nil
			if fail {
				w.reader.fail = func(obj client.Object) error {
					if isIncident(obj) {
						return errors.New("incident read failed")
					}
					return nil
				}
			}
		}
		setCount := func(inc *remediationv1alpha1.Incident, n int64) {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inc), inc)).To(Succeed())
			inc.Status.Count = n
			Expect(k8sClient.Status().Update(ctx, inc)).To(Succeed())
		}
		// incidentWorld is newWorld with an Incident counted count times, named by the request.
		incidentWorld := func(count int64) (*phasesWorld, *remediationv1alpha1.Incident) {
			seq++
			name := fmt.Sprintf("phases-%d", seq)
			w := &phasesWorld{reader: &rolloutReader{Reader: k8sClient}}
			newGit(w)
			ss := statefulSet(name, nil)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			w.ssKey = client.ObjectKeyFromObject(ss)
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ss))).To(Succeed()) })
			setTargetStatus(w, 2, 2, revisionNew, revisionNew)
			inc := &remediationv1alpha1.Incident{
				ObjectMeta: metav1.ObjectMeta{Namespace: phasesNamespace, Name: "incident-" + name},
				Spec: remediationv1alpha1.IncidentSpec{
					Fingerprint: "sha256:" + strings.Repeat("ab", 32), Reason: ReasonOOMKilled,
					Subject: remediationv1alpha1.ObjectRef{APIVersion: linkAPI, Kind: kindStatefulSet, Name: name},
				},
			}
			Expect(k8sClient.Create(ctx, inc)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, inc))).To(Succeed()) })
			setCount(inc, count)
			rr := fx.MemoryLimitRequest(phasesNamespace, name)
			rr.Spec.TargetRef = remediationv1alpha1.ObjectRef{APIVersion: linkAPI, Kind: kindStatefulSet,
				Namespace: phasesNamespace, Name: name, UID: remediationv1alpha1.UID(ss.UID)}
			rr.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: inc.Name, UID: remediationv1alpha1.UID(inc.UID)}
			mergedRequest(w, rr)
			return w, inc
		}
		readings := func(w *phasesWorld) *remediationv1alpha1.IncidentCountReadings {
			rr := fetch(w)
			if rr.Status.Verification == nil {
				return nil
			}
			return rr.Status.Verification.IncidentCount
		}
		// span is the shortest span between the readings for a memory fix: stableFor 10m plus two analyzer
		// windows.
		span := resolvedBeforeSpan(verification.MinStableForMemory)
		// toSuccess applies the merge commit at applied, then drives a quiet window to Succeeded and returns
		// the request.
		toSuccess := func(w *phasesWorld, applied time.Time) *remediationv1alpha1.RemediationRequest {
			w.reader.ks = kustomization(ksStatus{applied: w.merge})
			mustReconcileAt(w, applied)
			Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
			start := applied.Add(30 * time.Second)
			t, n := drive(w, start, start.Add(need), verification.PollInterval, 1, quiet(w, start))
			putReport(w, n, t, start, nil)
			mustReconcileAt(w, t)
			rr := fetch(w)
			Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseSucceeded))
			Expect(*rr.Status.BreakerCounts).To(BeTrue(), "the breaker reads Succeeded as before")
			verified := condition(rr, remediationv1alpha1.ConditionVerified)
			Expect(verified.Status).To(Equal(metav1.ConditionTrue))
			Expect(verified.Reason).To(Equal(ReasonNoRecurrence))
			Expect(condition(rr, remediationv1alpha1.ConditionCleared).Status).To(Equal(metav1.ConditionTrue))
			return rr
		}
		// setMerged writes Merged with the receipt through stop at merged plus ten seconds, as the merge does,
		// and returns that time.
		setMerged := func(w *phasesWorld) time.Time {
			at := w.merged.Add(10 * time.Second)
			w.now = at
			Expect(w.r.stop(ctx, fetch(w), remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged, "merged")).
				To(MatchError(errStopped))
			return at
		}
		// waiting sets w's request Merged (setMerged), moves it to WaitingForDelivery at merged plus a minute,
		// its delivery not seen yet, and returns the time of the write that set Merged.
		waiting := func(w *phasesWorld) time.Time {
			read := setMerged(w)
			w.reader.ks = kustomization(ksStatus{applied: w.base})
			mustReconcileAt(w, w.merged.Add(time.Minute))
			Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
			return read
		}

		It("ends Succeeded with Cleared ResolvedBeforeRollout when the count did not move over the span", func() {
			w, inc := incidentWorld(3)
			read := waiting(w)
			m := readings(w).AtMerge
			Expect(m).NotTo(BeNil())
			Expect(m.UID).To(Equal(string(inc.UID)))
			Expect(m.Count).To(Equal(int64(3)))
			Expect(m.ReadAt.Time).To(BeTemporally("==", read))
			rr := toSuccess(w, read.Add(span))
			a := rr.Status.Verification.IncidentCount.AtApplied
			Expect(a).NotTo(BeNil())
			Expect(a.Count).To(Equal(int64(3)))
			Expect(a.ReadAt.Time).To(BeTemporally("==", read.Add(span)))
			Expect(rr.Status.Verification.IncidentCount.AtMerge).To(Equal(m), "atMerge is kept by the window writes")
			cleared := condition(rr, remediationv1alpha1.ConditionCleared)
			Expect(cleared.Reason).To(Equal(ReasonResolvedBeforeRollout))
			Expect(cleared.Message).To(ContainSubstring("not shown to be the cause"))
		})

		It("keeps NoRecurrence when the count rose between the merge and the rollout", func() {
			w, inc := incidentWorld(3)
			read := waiting(w)
			setCount(inc, 4)
			rr := toSuccess(w, read.Add(span))
			Expect(rr.Status.Verification.IncidentCount.AtApplied.Count).To(Equal(int64(4)))
			Expect(condition(rr, remediationv1alpha1.ConditionCleared).Reason).To(Equal(ReasonNoRecurrence))
		})

		It("keeps NoRecurrence when the span between the readings is one second short", func() {
			w, _ := incidentWorld(3)
			read := waiting(w)
			rr := toSuccess(w, read.Add(span-time.Second))
			Expect(rr.Status.Verification.IncidentCount.AtApplied).NotTo(BeNil())
			Expect(condition(rr, remediationv1alpha1.ConditionCleared).Reason).To(Equal(ReasonNoRecurrence))
		})

		It("takes the reading at the merge in the status write that sets Merged", func() {
			w, inc := incidentWorld(5)
			at := setMerged(w)
			m := readings(w).AtMerge
			Expect(m).NotTo(BeNil())
			Expect(m.UID).To(Equal(string(inc.UID)))
			Expect(m.Count).To(Equal(int64(5)))
			Expect(m.ReadAt.Time).To(BeTemporally("==", at))
		})

		It("writes Merged without the reading when the Incident cannot be read there", func() {
			w, _ := incidentWorld(5)
			failIncident(w, true)
			setMerged(w)
			got := fetch(w)
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
			Expect(readings(w)).To(BeNil())
		})

		It("never takes the reading at the merge after the write that set Merged", func() {
			w, _ := incidentWorld(3)
			failIncident(w, true)
			read := setMerged(w)
			failIncident(w, false)
			w.reader.ks = kustomization(ksStatus{applied: w.base})
			mustReconcileAt(w, w.merged.Add(time.Minute))
			Expect(fetch(w).Status.Phase).To(Equal(remediationv1alpha1.PhaseWaitingForDelivery))
			mustReconcileAt(w, w.merged.Add(2*time.Minute))
			Expect(readings(w)).To(BeNil(), "a reading taken after the merge write could follow the change")
			rr := toSuccess(w, read.Add(span))
			Expect(rr.Status.Verification.IncidentCount).To(BeNil())
			Expect(condition(rr, remediationv1alpha1.ConditionCleared).Reason).To(Equal(ReasonNoRecurrence))
		})

		It("keeps NoRecurrence and never fills in the reading at the applied time when that read failed", func() {
			w, _ := incidentWorld(3)
			read := waiting(w)
			applied := read.Add(span)
			w.reader.ks = kustomization(ksStatus{applied: w.merge})
			failIncident(w, true)
			mustReconcileAt(w, applied)
			got := fetch(w)
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseVerifying))
			Expect(got.Status.PullRequest.AppliedObservedAt.Time).To(BeTemporally("==", applied))
			Expect(got.Status.Verification.IncidentCount.AtApplied).To(BeNil())
			failIncident(w, false)
			start := applied.Add(30 * time.Second)
			t, n := drive(w, start, start.Add(need), verification.PollInterval, 1, quiet(w, start))
			putReport(w, n, t, start, nil)
			mustReconcileAt(w, t)
			rr := fetch(w)
			Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseSucceeded))
			Expect(rr.Status.Verification.IncidentCount.AtApplied).To(BeNil())
			Expect(condition(rr, remediationv1alpha1.ConditionCleared).Reason).To(Equal(ReasonNoRecurrence))
		})

		It("never changes a reading, but lets a write without them through", func() {
			w, _ := incidentWorld(3)
			read := waiting(w)
			w.reader.ks = kustomization(ksStatus{applied: w.merge})
			mustReconcileAt(w, read.Add(time.Minute))
			Expect(readings(w).AtApplied).NotTo(BeNil())
			refusals := map[string]func(c *remediationv1alpha1.IncidentCountReadings){
				"atMerge is never changed once set":   func(c *remediationv1alpha1.IncidentCountReadings) { c.AtMerge.Count++ },
				"atApplied is never changed once set": func(c *remediationv1alpha1.IncidentCountReadings) { c.AtApplied.Count++ },
				"atApplied requires atMerge":          func(c *remediationv1alpha1.IncidentCountReadings) { c.AtMerge = nil },
			}
			for message, edit := range refusals {
				changed := fetch(w)
				edit(changed.Status.Verification.IncidentCount)
				err := k8sClient.Status().Update(ctx, changed)
				Expect(err).To(HaveOccurred(), message)
				Expect(err.Error()).To(ContainSubstring(message))
			}
			// An executor that predates the field (a rolled back release) drops it in its typed status write.
			noApplied := fetch(w)
			noApplied.Status.Verification.IncidentCount.AtApplied = nil
			Expect(k8sClient.Status().Update(ctx, noApplied)).To(Succeed())
			gone := fetch(w)
			gone.Status.Verification.IncidentCount = nil
			Expect(k8sClient.Status().Update(ctx, gone)).To(Succeed())
			Expect(readings(w)).To(BeNil())
		})

		It("takes no reading again after an older executor dropped them, and keeps NoRecurrence", func() {
			w, _ := incidentWorld(3)
			read := waiting(w)
			Expect(readings(w).AtMerge).NotTo(BeNil())
			dropped := fetch(w)
			dropped.Status.Verification.IncidentCount = nil
			Expect(k8sClient.Status().Update(ctx, dropped)).To(Succeed())
			rr := toSuccess(w, read.Add(span))
			Expect(rr.Status.Verification.IncidentCount).To(BeNil(), "a dropped reading is never taken again")
			Expect(condition(rr, remediationv1alpha1.ConditionCleared).Reason).To(Equal(ReasonNoRecurrence))
		})

		It("never takes the reading at the applied time again after an older executor dropped it", func() {
			w, _ := incidentWorld(3)
			read := waiting(w)
			applied := read.Add(span)
			w.reader.ks = kustomization(ksStatus{applied: w.merge})
			mustReconcileAt(w, applied)
			Expect(readings(w).AtApplied).NotTo(BeNil())
			dropped := fetch(w)
			dropped.Status.Verification.IncidentCount.AtApplied = nil
			Expect(k8sClient.Status().Update(ctx, dropped)).To(Succeed())
			start := applied.Add(30 * time.Second)
			t, n := drive(w, start, start.Add(need), verification.PollInterval, 1, quiet(w, start))
			putReport(w, n, t, start, nil)
			mustReconcileAt(w, t)
			rr := fetch(w)
			Expect(rr.Status.Phase).To(Equal(remediationv1alpha1.PhaseSucceeded))
			Expect(rr.Status.Verification.IncidentCount.AtMerge).NotTo(BeNil())
			Expect(rr.Status.Verification.IncidentCount.AtApplied).To(BeNil())
			Expect(condition(rr, remediationv1alpha1.ConditionCleared).Reason).To(Equal(ReasonNoRecurrence))
		})

		It("takes no reading at the applied time in a process that did not take the one at the merge", func() {
			w, _ := incidentWorld(3)
			read := waiting(w)
			// A restart or another leader: its clock may disagree with the one that gave atMerge's readAt.
			w.r = w.reconciler()
			rr := toSuccess(w, read.Add(span))
			Expect(rr.Status.Verification.IncidentCount.AtMerge).NotTo(BeNil())
			Expect(rr.Status.Verification.IncidentCount.AtApplied).To(BeNil())
			Expect(condition(rr, remediationv1alpha1.ConditionCleared).Reason).To(Equal(ReasonNoRecurrence))
		})

		It("writes Merged with the receipt and without the reading when the step's deadline is tight", func() {
			w, _ := incidentWorld(5)
			w.now = w.merged.Add(10 * time.Second)
			tight, cancel := context.WithTimeout(ctx, countWriteReserve+DefaultIncidentReadTimeout-time.Second)
			defer cancel()
			Expect(w.r.stop(tight, fetch(w), remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged, "merged")).
				To(MatchError(errStopped))
			got := fetch(w)
			Expect(got.Status.Phase).To(Equal(remediationv1alpha1.PhaseMerged))
			Expect(got.Status.MergeReceipt).NotTo(BeNil())
			Expect(readings(w)).To(BeNil(), "the optional read never takes from the write's allowance")
		})
	})
})

// reconciler is the executor of the world; a new one is a restart.
func (w *phasesWorld) reconciler() *RemediationRequestReconciler {
	return &RemediationRequestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: w.reader,
		Recorder: w.rec, Now: func() time.Time { return w.now }, VerifyOutcomes: true,
		OpenRepository: func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return w.git, nil
		},
		Delivery: func(a providers.AncestryChecker) (DeliveryVerifier, error) {
			return fluxdelivery.New(w.reader, a)
		},
	}
}
