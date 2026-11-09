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
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/gityaml"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/testutil/deadline"
)

// executorMemory is the executor's memory limit in the chart.
const executorMemory = 128 << 20

// anchors is in the message of every refusal of an anchor, alias or merge key.
const anchors = "anchors"

// aliasBomb returns the entries of a mapping, indented by two spaces: a list of pad plain items, then lists of
// aliases that expand to 710,000 more. With 100,000 items (about 200 KB) the share of aliases stays under what
// go.yaml.in/yaml/v2 refuses as excessive aliasing, so before #594 sigs.k8s.io/yaml decoded it and allocated
// over 150 MiB; that many items are now over the node budget (#595), and 50,000 are under it.
func aliasBomb(pad int) string {
	const indent = "  "
	var b strings.Builder
	b.WriteString(indent + "pad: [" + strings.Repeat("x,", pad) + "x]\n")
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
				"  namespace: orders\ndata:\n" + aliasBomb(50000)
		}, soFile, anchors},
		{"an alias bomb in a kustomization file", func(f *fixture) {
			f.files[ksFile] = ksHead + ksResources + "metadata:\n" + aliasBomb(50000)
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
		// The line is the file's, past the empty document between the two separators (scaledObjectYAML has
		// 20 lines and ends with one).
		{"an alias after an empty document of a manifest", func(f *fixture) {
			f.files[soFile] = scaledObjectYAML + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: &m\n" +
				"  name: c\n  namespace: orders\ndata:\n  copy: *m\n"
		}, soFile, soFile + " line 24: YAML anchors"},
		{"an alias after empty documents at the start of a manifest", func(f *fixture) {
			f.files[soFile] = "---\n---\n# c\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n" +
				"  namespace: orders\n  labels: &l {a: b}\n---\n" + scaledObjectYAML
		}, soFile, soFile + " line 10: YAML anchors"},
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
		aliasBomb(50000))
	var got any
	n := allocated(func() { got, _, err = sourcemap.ReadField(data, target, field) })
	// The first anchor is on line 28 of the file (scaledObjectYAML has 20 lines).
	if want := "line 28: YAML anchors"; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("ReadField = %v, %v; want an error starting with %q", got, err, want)
	}
	if n >= executorMemory/2 {
		t.Errorf("ReadField allocated %d MiB, want less than half the executor's %d MiB", n>>20, executorMemory>>20)
	}
}

// peakHeap returns the most heap f held at once, sampled every millisecond, over the heap before it. The
// collector runs at GOGC 10 meanwhile, so the sample stays close to what f keeps alive.
func peakHeap(f func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	var m goruntime.MemStats
	goruntime.GC()
	goruntime.ReadMemStats(&m)
	base := m.HeapAlloc
	var (
		stop atomic.Bool
		peak uint64
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		var s goruntime.MemStats
		for !stop.Load() {
			goruntime.ReadMemStats(&s)
			peak = max(peak, s.HeapAlloc)
			time.Sleep(time.Millisecond)
		}
	}()
	f()
	stop.Store(true)
	<-done
	return peak - min(base, peak)
}

// A file of more than gityaml.MaxNodes YAML nodes is refused with InputTooLarge before any tree of it is built,
// in kustomization files and manifests alike (#595): an alias bomb whose plain items are over the budget, and
// a manifest of 1 MiB packed with flow pairs.
func TestResolveRefusesDenseYAML(t *testing.T) {
	const ksHead = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"
	const ksResources = "resources:\n  - scaledobject.yaml\n  - ./deployment.yaml\n"
	const nodes = "holds more than 131072 YAML nodes"
	tests := []struct {
		name   string
		change func(f *fixture)
		file   string
	}{
		{"an alias bomb in a manifest", func(f *fixture) {
			f.files[soFile] = scaledObjectYAML + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: bomb\n" +
				"  namespace: orders\ndata:\n" + aliasBomb(100000)
		}, soFile},
		{"an alias bomb in a kustomization file", func(f *fixture) {
			f.files[ksFile] = ksHead + ksResources + "metadata:\n" + aliasBomb(100000)
		}, ksFile},
		{"flow pairs in a manifest", func(f *fixture) {
			head := scaledObjectYAML + "---\n["
			f.files[soFile] = head + strings.Repeat("x: ,", (providers.MaxFileBytes-len(head)-5)/4) + "x: ]\n"
		}, soFile},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.change(f)
			var (
				res sourcemap.Result
				err error
			)
			peak := peakHeap(func() { res, _, err = f.run(t) })
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome != sourcemap.NeedsIntervention || res.Reason != sourcemap.ReasonInputTooLarge {
				t.Fatalf("result = %s/%s (%s), want NeedsIntervention/InputTooLarge", res.Outcome, res.Reason, res.Message)
			}
			if res.Message != tc.file+" "+nodes {
				t.Fatalf("message %q, want %q", res.Message, tc.file+" "+nodes)
			}
			if peak > executorMemory/4 {
				t.Errorf("the mapping held %d MiB at its peak, want at most %d MiB", peak>>20, executorMemory>>22)
			}
			sameWithCache(t, tc.change, res)
		})
	}
}

// ReadField refuses a document of 1 MiB packed with nodes before any tree of it is built, and reads the largest
// document the node count accepts within a quarter of the executor's memory (#595). Before, the 1 MiB document
// was decoded.
func TestReadFieldPeakHeap(t *testing.T) {
	target := newFixture().in.Target
	field, err := sourcemap.FieldFor(v1alpha1.Action{Type: v1alpha1.ActionAdjustKedaMaxReplicas}, target)
	if err != nil {
		t.Fatal(err)
	}
	const head = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: big, namespace: orders}\ndata: {x: ["
	const tail = "x]}\n"
	file := func(n int) []byte { return []byte(scaledObjectYAML + "---\n" + head + strings.Repeat("x,", n) + tail) }
	most := (providers.MaxFileBytes - len(file(0))) / 2
	// The most items the node count accepts.
	accepted, over := 0, most
	for accepted+1 < over {
		if mid := (accepted + over) / 2; gityaml.Nodes(file(mid), gityaml.MaxNodes) <= gityaml.MaxNodes {
			accepted = mid
		} else {
			over = mid
		}
	}
	for _, c := range []struct {
		name string
		n    int
		want string
	}{{"1 MiB", most, "holds more than 131072 YAML nodes"}, {"at the budget", accepted, ""}} {
		t.Run(c.name, func(t *testing.T) {
			deadline.Within(t, time.Minute, func(t *testing.T) {
				data := file(c.n)
				if len(data) > providers.MaxFileBytes {
					t.Fatalf("the file has %d bytes, over the limit of %d", len(data), providers.MaxFileBytes)
				}
				var (
					got any
					err error
				)
				peak := peakHeap(func() { got, _, err = sourcemap.ReadField(data, target, field) })
				t.Logf("%d bytes: peak heap %d MiB", len(data), peak>>20)
				switch {
				case c.want == "" && (err != nil || got == nil):
					t.Errorf("ReadField = %v, %v; want the value", got, err)
				case c.want != "" && (err == nil || err.Error() != c.want):
					t.Errorf("ReadField = %v, %v; want %q", got, err, c.want)
				}
				if peak > executorMemory/4 {
					t.Errorf("ReadField held %d MiB at its peak, want at most %d MiB", peak>>20, executorMemory>>22)
				}
			})
		})
	}
}
