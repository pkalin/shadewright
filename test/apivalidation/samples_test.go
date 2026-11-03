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
	"bytes"
	"io"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// The samples are documentation; this keeps them valid against the generated CRDs.
var _ = Describe("config/samples", func() {
	It("are all accepted by the API server", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shadewright-system"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		files, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "remediation_*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(HaveLen(4))

		// The lab samples (docs/lab-oom-acceptance.md) carry placeholders that must stay valid against the
		// schema and must never be replaced by real values in Git.
		lab, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "lab", "remediation_*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(lab).To(HaveLen(4))
		for _, f := range lab {
			data, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			if filepath.Base(f) != "remediation_v1alpha1_remediationpolicy.yaml" {
				Expect(string(data)).To(ContainSubstring("REPLACE"), f)
			}
		}
		files = append(files, lab...)

		for _, f := range files {
			data, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
			for {
				obj := &unstructured.Unstructured{}
				if err := dec.Decode(&obj.Object); err == io.EOF {
					break
				} else {
					Expect(err).NotTo(HaveOccurred(), f)
				}
				Expect(k8sClient.Create(ctx, obj)).To(Succeed(), f)
				Expect(k8sClient.Delete(ctx, obj)).To(Succeed(), f)
			}
		}
	})
})
