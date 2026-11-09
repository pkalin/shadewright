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

package patch

import (
	"errors"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// executorMemory is the executor's memory limit in the chart.
const executorMemory = 128 << 20

// aliasBomb returns the entries of a mapping, indented by two spaces, of about 200 KB: a list of 100,000 plain
// items, then lists of aliases that expand to 710,000 more. The plain items keep the share of aliases under
// what go.yaml.in/yaml/v3 refuses as excessive aliasing, so before #595 the patcher decoded it.
func aliasBomb() string {
	const indent = "  "
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

// An alias bomb in another document of the file is refused before any parser expands it (#595, invariant 4).
func TestAliasBombRefused(t *testing.T) {
	dep := string(read(t, "deployment.yaml"))
	in := []byte(dep + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: bomb\n  namespace: orders\ndata:\n" +
		aliasBomb())
	var err error
	n := allocated(func() { _, err = SetContainerMemoryLimit(in, deployment, mem("web", "512Mi", "768Mi")) })
	if !errors.Is(err, ErrInvalidYAML) || !strings.Contains(err.Error(), "anchors") {
		t.Errorf("got %v, want %v naming anchors", err, ErrInvalidYAML)
	}
	if n >= executorMemory/2 {
		t.Errorf("the patcher allocated %d MiB, want less than half the executor's %d MiB", n>>20, executorMemory>>20)
	}
}

// denseFile returns the Deployment of testdata and a ConfigMap holding a flow list of n plain items.
func denseFile(t *testing.T, n int) []byte {
	t.Helper()
	return []byte(string(read(t, "deployment.yaml")) +
		"---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: big, namespace: orders}\ndata: {x: [" +
		strings.Repeat("x,", n) + "x]}\n")
}

// nodesOf returns the YAML nodes of data as the patcher counts them.
func nodesOf(data []byte) int {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	c := 0
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			return c
		}
		c += countNodes(&n, MaxNodes*8)
	}
}

// treeHeap returns the peak heap of decoding the last document of data into nodes.
func treeHeap(t *testing.T, data []byte) uint64 {
	t.Helper()
	return peakHeap(func() {
		var n yaml.Node
		if err := yaml.Unmarshal(data[strings.LastIndex(string(data), "---\n"):], &n); err != nil {
			t.Error(err)
		}
	})
}

// The patcher reads a file one document at a time and holds one node tree at a time besides the target's
// (#595): a file of exactly MaxNodes nodes is patched with a peak heap of about one tree of its dense document
// plus a margin. Before, it held the trees of the original twice and of the patched file at once, plus their
// decoded values.
func TestNodeBudgetPeakHeap(t *testing.T) {
	head := nodesOf(denseFile(t, 0))
	in := denseFile(t, MaxNodes-head)
	if n := nodesOf(in); n != MaxNodes {
		t.Fatalf("the file has %d nodes, want %d", n, MaxNodes)
	}
	var (
		out []byte
		err error
	)
	patched := peakHeap(func() { out, err = SetContainerMemoryLimit(in, deployment, mem("web", "512Mi", "768Mi")) })
	if err != nil || len(out) != len(in) {
		t.Fatalf("got %v, want the file patched", err)
	}
	tree := treeHeap(t, in)
	t.Logf("peak heap: the patcher %d MiB, the tree of the dense document %d MiB", patched>>20, tree>>20)
	if patched > tree+executorMemory/8 {
		t.Errorf("the patcher held %d MiB at its peak, want at most one tree's %d MiB plus %d MiB",
			patched>>20, tree>>20, executorMemory>>23)
	}
	_, err = SetContainerMemoryLimit(denseFile(t, MaxNodes+1-head), deployment, mem("web", "512Mi", "768Mi"))
	if !errors.Is(err, ErrInvalidYAML) || !strings.Contains(err.Error(), "YAML nodes") {
		t.Errorf("one node over the budget: got %v, want %v naming the nodes", err, ErrInvalidYAML)
	}
}

// A file of MaxBytes packed with nodes (a flow list of about 520,000 items) is refused for its nodes with a
// peak heap of one tree of it, under the executor's memory limit (#595). Before, the patcher held about 260 MiB.
func TestDenseFilePeakHeap(t *testing.T) {
	in := denseFile(t, 0)
	in = denseFile(t, (MaxBytes-len(in))/2)
	if len(in) > MaxBytes {
		t.Fatalf("the file has %d bytes, over the limit of %d", len(in), MaxBytes)
	}
	var err error
	patched := peakHeap(func() { _, err = SetContainerMemoryLimit(in, deployment, mem("web", "512Mi", "768Mi")) })
	if !errors.Is(err, ErrInvalidYAML) || !strings.Contains(err.Error(), "YAML nodes") {
		t.Errorf("got %v, want %v naming the nodes", err, ErrInvalidYAML)
	}
	tree := treeHeap(t, in)
	t.Logf("peak heap: the patcher %d MiB, the tree of the dense document %d MiB", patched>>20, tree>>20)
	if patched > tree+executorMemory/8 || patched >= executorMemory {
		t.Errorf("the patcher held %d MiB at its peak, want at most one tree's %d MiB plus %d MiB, under the executor's %d MiB",
			patched>>20, tree>>20, executorMemory>>23, executorMemory>>20)
	}
}
