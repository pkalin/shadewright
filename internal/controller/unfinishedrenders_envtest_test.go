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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// renderThenRefusedCommit is a step that renders the change (checkRender, with its mark and its removal) and
// then fails, as a commit the Git host refuses does.
type renderThenRefusedCommit struct {
	r     *RemediationRequestReconciler
	git   *fakegit.Git
	head  string
	key   client.ObjectKey
	calls atomic.Int32
}

func (s *renderThenRefusedCommit) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if req.NamespacedName != s.key {
		return reconcile.Result{}, nil
	}
	s.calls.Add(1)
	rr := &remediationv1alpha1.RemediationRequest{}
	if err := s.r.Get(ctx, req.NamespacedName, rr); err != nil {
		return reconcile.Result{}, err
	}
	if err := s.r.checkRender(ctx, s.git, s.head, []byte("changed"), rr, nil); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, errors.New("commit the change: the Git host refused it")
}

// The writes of the render mark must not wake the request: a step that fails after a render waits for its
// backoff instead of rendering again at once (#714).
var _ = Describe("RemediationRequest render mark events", func() {
	const ns = "render-mark-events"

	It("retries a commit that fails after a passed render with its backoff, not at once", func(sctx SpecContext) {
		Expect(k8sClient.Create(sctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		rr := fx.MemoryLimitRequest(ns, "rr")
		Expect(k8sClient.Create(sctx, rr)).To(Succeed())
		rr.Status.PlanDigest = digestA
		rr.Status.Source = &remediationv1alpha1.ResolvedSource{RepositoryRef: fx.RepositoryName,
			TargetBranch: mainBranch, Path: renderTestFile, BuildPath: renderTestBuild, BaseCommit: baseCommit}
		Expect(k8sClient.Status().Update(sctx, rr)).To(Succeed())

		git := newRenderTestGit()
		head, err := git.BranchHead(sctx, "main")
		Expect(err).NotTo(HaveOccurred())

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:     k8sClient.Scheme(),
			Metrics:    metricsserver.Options{BindAddress: "0"},
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		step := &renderThenRefusedCommit{git: git, head: head, key: client.ObjectKeyFromObject(rr),
			r: &RemediationRequestReconciler{Client: mgr.GetClient(), APIReader: k8sClient,
				renderCheck: func(context.Context, semdiff.Input) (semdiff.Result, error) {
					return semdiff.Result{}, nil
				}}}
		// The backoff after the first failure is an hour, so any second reconcile within the window below was
		// woken by an event, not by the backoff.
		Expect(ctrl.NewControllerManagedBy(mgr).For(&remediationv1alpha1.RemediationRequest{}).
			WithEventFilter(skipOwnMarks).
			WithOptions(controller.Options{RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Hour, time.Hour)}).
			Named("render-mark-events").Complete(step)).To(Succeed())
		mctx, cancel := context.WithCancel(sctx)
		defer cancel()
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mctx)).To(Succeed())
		}()

		Eventually(step.calls.Load).WithTimeout(10 * time.Second).Should(BeNumerically(">=", 1))
		Eventually(func(g Gomega) {
			got := &remediationv1alpha1.RemediationRequest{}
			g.Expect(k8sClient.Get(sctx, step.key, got)).To(Succeed())
			g.Expect(got.Status.UnfinishedRenders).To(BeNil())
		}).WithTimeout(5*time.Second).Should(Succeed(), "the mark is removed after the render")
		Consistently(step.calls.Load).WithTimeout(3*time.Second).Should(Equal(int32(1)),
			"the writes of the mark woke the request before its backoff")
	})
})
