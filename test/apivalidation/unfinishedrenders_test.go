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

package apivalidation

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// status.unfinishedRenders, the executor's mark of render checks that started and did not finish (#714,
// ADR-0035).
var _ = Describe("RemediationRequest status.unfinishedRenders", func() {
	process := strings.Repeat("e", 32)
	mark := func(digest string, count int32) *v1alpha1.UnfinishedRenders {
		return &v1alpha1.UnfinishedRenders{PlanDigest: testDigest(digest), Count: count, Process: process}
	}
	// marked creates a request and stores the mark of digest in its status, one write per count up to count.
	marked := func(digest string, count int32) *v1alpha1.RemediationRequest {
		rr := createdRequest(false)
		for n := int32(1); n <= count; n++ {
			rr.Status.UnfinishedRenders = mark(digest, n)
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, rr)).To(Succeed())
		}
		return reread(rr)
	}

	It("keeps a request stored without it valid", func() {
		rr := createdRequest(false)
		rr.Status.Phase = v1alpha1.PhasePending
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	It("accepts counts from 1 to MaxUnfinishedRenders, one write at a time", func() {
		rr := marked("a", v1alpha1.MaxUnfinishedRenders)
		Expect(rr.Status.UnfinishedRenders.Count).To(BeEquivalentTo(v1alpha1.MaxUnfinishedRenders))
	})

	It("states the schema's maximum count as MaxUnfinishedRenders", func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases",
			"remediation.shadewright.io_remediationrequests.yaml"))
		Expect(err).NotTo(HaveOccurred())
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Expect(yaml.Unmarshal(raw, crd)).To(Succeed())
		Expect(crd.Spec.Versions).NotTo(BeEmpty())
		for _, v := range crd.Spec.Versions {
			count := v.Schema.OpenAPIV3Schema.Properties["status"].Properties["unfinishedRenders"].Properties["count"]
			Expect(count.Maximum).NotTo(BeNil(), "version %s", v.Name)
			Expect(*count.Maximum).To(BeEquivalentTo(v1alpha1.MaxUnfinishedRenders), "version %s", v.Name)
		}
	})

	DescribeTable("refuses a malformed mark",
		func(m *v1alpha1.UnfinishedRenders, msg string) {
			rr := createdRequest(false)
			rr.Status.UnfinishedRenders = m
			expectStatusRefused(rr, msg)
		},
		Entry("a count of 0", mark("a", 0), "status.unfinishedRenders.count"),
		Entry("a count over the bound", mark("a", v1alpha1.MaxUnfinishedRenders+1), "status.unfinishedRenders.count"),
		Entry("a digest without its algorithm",
			&v1alpha1.UnfinishedRenders{PlanDigest: "not-a-digest", Count: 1, Process: process},
			"status.unfinishedRenders.planDigest"),
		Entry("no process", &v1alpha1.UnfinishedRenders{PlanDigest: testDigest("a"), Count: 1},
			"status.unfinishedRenders.process"),
		Entry("a process that is not 32 hexadecimal characters",
			&v1alpha1.UnfinishedRenders{PlanDigest: testDigest("a"), Count: 1, Process: "executor-7f9c"},
			"status.unfinishedRenders.process"),
		Entry("a new mark above 1", mark("a", 2), "starts at count 1"),
	)

	DescribeTable("refuses a count that does not rise by exactly 1 for the same plan digest",
		func(from, to int32) {
			rr := marked("a", from)
			rr.Status.UnfinishedRenders = mark("a", to)
			expectStatusRefused(rr, "rises by exactly 1 per write for the same planDigest")
		},
		Entry("a jump from 1 to 3", int32(1), int32(3)),
		Entry("a lower count", int32(2), int32(1)),
	)

	It("refuses the same count written by another process", func() {
		rr := marked("a", 1)
		rr.Status.UnfinishedRenders = mark("a", 1)
		rr.Status.UnfinishedRenders.Process = strings.Repeat("f", 32)
		expectStatusRefused(rr, "rises by exactly 1 per write for the same planDigest")
	})

	It("refuses a mark for another plan digest that does not start at 1", func() {
		rr := marked("a", 1)
		rr.Status.UnfinishedRenders = mark("b", 2)
		expectStatusRefused(rr, "starts at count 1")
	})

	It("keeps a mark that a status write leaves as it is", func() {
		rr := marked("a", 2)
		rr.Status.Phase = v1alpha1.PhasePending
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
	})

	It("accepts removing the mark, and count 1 for another plan digest", func() {
		rr := marked("a", 3)
		rr.Status.UnfinishedRenders = mark("b", 1)
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		rr = reread(rr)
		rr.Status.UnfinishedRenders = nil
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed())
		rr = reread(rr)
		Expect(rr.Status.UnfinishedRenders).To(BeNil())
		rr.Status.UnfinishedRenders = mark("b", 1)
		Expect(k8sClient.Status().Update(ctx, rr)).To(Succeed(), "a new mark after the removal")
	})
})
