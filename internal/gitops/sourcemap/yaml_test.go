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

package sourcemap_test

import (
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
)

// executorMemory is the executor's memory limit in the chart.
const executorMemory = 128 << 20

// anchors is in the message of every refusal of an anchor, alias or merge key.
const anchors = "anchors"

// aliasBomb returns the entries of a mapping, indented by indent, of about 200 KB: a list of 100,000 plain
// items, then lists of aliases that expand to 710,000 more. The plain items keep the share of aliases under
// what go.yaml.in/yaml/v2 refuses as excessive aliasing, so before #594 sigs.k8s.io/yaml decoded it and
// allocated over 150 MiB.
func aliasBomb(indent string) string {
	var b strings.Builder
	b.WriteString(indent + "pad: [" + strings.Repeat("x,", 100000) + "x]\n")
	b.WriteString(indent + "a0: &a0 [" + strings.Repeat("x,", 999) + "x]\n")
	b.WriteString(indent + "a1: &a1 [" + strings.Repeat("*a0,", 9) + "*a0]\n")
	b.WriteString(indent + "a2: &a2 [" + strings.Repeat("*a1,", 9) + "*a1]\n")
	b.WriteString(indent + "a3: [" + strings.Repeat("*a2,", 5) + "*a2]\n")
	return b.String()
}

// allocated returns how many bytes f allocated.
func allocated(f func()) uint64 {
	var m goruntime.MemStats
	goruntime.GC()
	goruntime.ReadMemStats(&m)
	before := m.TotalAlloc
	f()
	goruntime.ReadMemStats(&m)
	return m.TotalAlloc - before
}

// Every YAML file the mapping reads from Git is checked before sigs.k8s.io/yaml decodes it, which replaces
// aliases by copies: anchors, aliases, merge keys and a separator line with more than a comment are refused,
// in kustomization files and manifests alike (#594, invariant 4).
func TestResolveRefusesYAMLAliases(t *testing.T) {
	const ksHead = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"
	const ksResources = "resources:\n  - scaledobject.yaml\n  - ./deployment.yaml\n"
	tests := []struct {
		name   string
		change func(f *fixture)
		file   string
		want   string
	}{
		{"an alias bomb in a manifest", func(f *fixture) {
			f.files[soFile] = scaledObjectYAML + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: bomb\n" +
				"  namespace: orders\ndata:\n" + aliasBomb("  ")
		}, soFile, anchors},
		{"an alias bomb in a kustomization file", func(f *fixture) {
			f.files[ksFile] = ksHead + ksResources + "metadata:\n" + aliasBomb("  ")
		}, ksFile, anchors},
		{"an anchor in a kustomization file", func(f *fixture) {
			f.files[ksFile] = ksHead + "resources: &r\n  - scaledobject.yaml\n  - ./deployment.yaml\n"
		}, ksFile, "line 3: YAML anchors"},
		{"an anchor on a separator of a kustomization file", func(f *fixture) {
			f.files[ksFile] = "# The orders app.\n--- &k\n" + ksHead + ksResources
		}, ksFile, "line 2: a document separator"},
		{"a merge key in a manifest", func(f *fixture) {
			f.files[soFile] = strings.Replace(scaledObjectYAML, "  name: orders-worker\n",
				"  name: orders-worker\n  <<: {labels: {app: orders}}\n", 1)
		}, soFile, anchors},
		{"an alias in the second document of a manifest", func(f *fixture) {
			f.files[soFile] = scaledObjectYAML + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: &m\n" +
				"  name: c\n  namespace: orders\ndata:\n  copy: *m\n"
		}, soFile, "document 3 line 4: YAML anchors"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.change(f)
			var (
				res sourcemap.Result
				err error
			)
			n := allocated(func() { res, _, err = f.run(t) })
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome != sourcemap.NeedsIntervention || res.Reason != sourcemap.ReasonFileNotReadable {
				t.Fatalf("result = %s/%s (%s), want NeedsIntervention/FileNotReadable", res.Outcome, res.Reason, res.Message)
			}
			if !strings.Contains(res.Message, tc.file) || !strings.Contains(res.Message, tc.want) {
				t.Fatalf("message %q, want %s and %q named", res.Message, tc.file, tc.want)
			}
			if n >= executorMemory/2 {
				t.Errorf("the mapping allocated %d MiB, want less than half the executor's %d MiB", n>>20, executorMemory>>20)
			}
			sameWithCache(t, tc.change, res)
		})
	}
}

// ReadField, which reads the target file again after the patch, checks it the same way (#594).
func TestReadFieldRefusesYAMLAliases(t *testing.T) {
	target := newFixture().in.Target
	field, err := sourcemap.FieldFor(v1alpha1.Action{Type: v1alpha1.ActionAdjustKedaMaxReplicas}, target)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(scaledObjectYAML + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: bomb\ndata:\n" +
		aliasBomb("  "))
	var got any
	n := allocated(func() { got, _, err = sourcemap.ReadField(data, target, field) })
	if err == nil || !strings.Contains(err.Error(), anchors) {
		t.Fatalf("ReadField = %v, %v; want an error about anchors", got, err)
	}
	if n >= executorMemory/2 {
		t.Errorf("ReadField allocated %d MiB, want less than half the executor's %d MiB", n>>20, executorMemory>>20)
	}
}
