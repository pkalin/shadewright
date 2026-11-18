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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
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
})
