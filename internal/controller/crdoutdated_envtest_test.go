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
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	fx "github.com/shadewright-labs/shadewright/internal/testfixtures"
)

// A real API server with the CustomResourceDefinition of RemediationRequest of v0.5.2 (testdata/crd-v0.5.2, copied
// from that tag): it prunes status.gitWaits from a status write that succeeds, and the executor's status writer
// reports it (#866). Its own envtest, as the suite's holds the current CRDs.
var _ = Describe("RemediationRequest status writes under the CRD of v0.5.2", func() {
	It("fails a write whose Git wait the API server pruned, with a Warning Event", func(sctx SpecContext) {
		env := &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("testdata", "crd-v0.5.2")},
			ErrorIfCRDPathMissing: true,
		}
		if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
			env.BinaryAssetsDirectory = dir
		}
		oldCfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(env.Stop()).To(Succeed()) })
		c, err := client.New(oldCfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		const ns = "old-crd"
		Expect(c.Create(sctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		rr := fx.MemoryLimitRequest(ns, "rr")
		Expect(c.Create(sctx, rr)).To(Succeed())
		rec := record.NewFakeRecorder(4)
		r := &RemediationRequestReconciler{Client: c, Scheme: scheme.Scheme, Recorder: rec}

		rr.Status.Phase = api.PhaseAwaitingApproval
		rr.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitPaced, Since: metav1.Now(), Operation: "read the pull request"}}
		err = r.Status().Update(sctx, rr)
		Expect(IsCRDOutdated(err)).To(BeTrue(), "error: %v", err)
		Expect(err.Error()).To(ContainSubstring("status.gitWaits"))

		// The write itself went through: the phase is stored, the wait is not.
		stored := &api.RemediationRequest{}
		Expect(c.Get(sctx, client.ObjectKeyFromObject(rr), stored)).To(Succeed())
		Expect(stored.Status.Phase).To(Equal(api.PhaseAwaitingApproval))
		Expect(stored.Status.GitWaits).To(BeEmpty())

		var events []string
		for len(rec.Events) > 0 {
			events = append(events, <-rec.Events)
		}
		Expect(events).To(HaveLen(1))
		Expect(strings.HasPrefix(events[0], "Warning "+EventCustomResourceDefinitionOutdated)).To(BeTrue(),
			"event: %s", events[0])
	})

	// An enum value added since v0.5.2 is refused, not pruned (#895): the CRD of v0.5.2 does not list
	// ExecutorRestarted among the reasons of status.verification.lastReset, so the API server answers 422 Invalid.
	// The executor's status writer classes it like a pruned field: CRD outdated, the Warning Event, and the later
	// writes of the reconcile refused. Another Invalid answer, and a reason the old CRD lists, are left alone.
	It("fails a write whose ExecutorRestarted reset the API server refused, with a Warning Event", func(sctx SpecContext) {
		env := &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("testdata", "crd-v0.5.2")},
			ErrorIfCRDPathMissing: true,
		}
		if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
			env.BinaryAssetsDirectory = dir
		}
		oldCfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(env.Stop()).To(Succeed()) })
		c, err := client.New(oldCfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		const ns = "old-crd-enum"
		Expect(c.Create(sctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		rr := fx.MemoryLimitRequest(ns, "rr")
		Expect(c.Create(sctx, rr)).To(Succeed())
		rec := record.NewFakeRecorder(4)
		r := &RemediationRequestReconciler{Client: c, Scheme: scheme.Scheme, Recorder: rec}
		key := client.ObjectKeyFromObject(rr)
		start := metav1.NewTime(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
		withReset := func(reason api.ResetReason, resets int64) *api.RemediationRequest {
			fresh := &api.RemediationRequest{}
			Expect(c.Get(sctx, key, fresh)).To(Succeed())
			fresh.Status.Verification = &api.VerificationWindow{WindowStart: &start, Resets: resets,
				LastReset: &api.VerificationReset{Reason: reason, Time: start}}
			return fresh
		}
		drain := func() []string {
			var out []string
			for len(rec.Events) > 0 {
				out = append(out, <-rec.Events)
			}
			return out
		}

		// Another Invalid answer (a CEL rule: lastReset without resets) is returned as it is.
		err = r.Status().Update(sctx, withReset(api.ResetNotAvailable, 0))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error: %v", err)
		Expect(IsCRDOutdated(err)).To(BeFalse(), "error: %v", err)
		Expect(drain()).To(BeEmpty())

		err = r.Status().Update(sctx, withReset(api.ResetExecutorRestarted, 1))
		Expect(IsCRDOutdated(err)).To(BeTrue(), "error: %v", err)
		Expect(isClusterError(err)).To(BeTrue())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "the API server's answer is kept")
		Expect(err.Error()).To(ContainSubstring("status.verification.lastReset.reason (value ExecutorRestarted)"))
		events := drain()
		Expect(events).To(HaveLen(1))
		Expect(events[0]).To(HavePrefix("Warning " + EventCustomResourceDefinitionOutdated))
		Expect(events[0]).To(ContainSubstring("ExecutorRestarted"))
		stored := &api.RemediationRequest{}
		Expect(c.Get(sctx, key, stored)).To(Succeed())
		Expect(stored.Status.Verification).To(BeNil(), "the refused write stored nothing")

		// Every later status write of the request in the reconcile is refused with it, a valid one included.
		Expect(IsCRDOutdated(r.Status().Update(sctx, withReset(api.ResetNotAvailable, 1)))).To(BeTrue())
		Expect(r.takeOutdatedCRD(key)).To(HaveOccurred())

		// The next reconcile, with a reason the old CRD lists, writes.
		Expect(r.Status().Update(sctx, withReset(api.ResetNotAvailable, 1))).To(Succeed())
		Expect(drain()).To(BeEmpty())
	})
})
