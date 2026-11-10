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
	"fmt"
	"reflect"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/gityaml"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/testutil/deadline"
)

// executorMemory is the executor's memory limit in the chart.
const executorMemory = 128 << 20

// nodesCause is in the message of every refusal of a file of too many YAML nodes.
const nodesCause = "YAML nodes"

// budget is the most heap the patcher may hold at its peak for any input: a quarter of the executor's memory
// limit, which leaves room under its GOMEMLIMIT (90 percent of the limit, about 115 MiB, #602) for the rest
// of the process.
const budget = executorMemory / 4

// aliasBomb returns the entries of a mapping, indented by two spaces: a list of pad plain items, then lists of
// aliases that expand to 710,000 more. With 100,000 items (about 200 KB) the share of aliases stays under what
// go.yaml.in/yaml/v3 refuses as excessive aliasing, so before #595 the patcher decoded it; that many items are
// now over the node budget, and 50,000 are under it.
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

// An alias bomb in another document of the file is refused before any parser expands it: for its anchors, or
// for its nodes when they are over the budget (#595, invariant 4).
func TestAliasBombRefused(t *testing.T) {
	for pad, want := range map[int]string{50000: "anchors", 100000: nodesCause} {
		dep := string(read(t, "deployment.yaml"))
		in := []byte(dep + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: bomb\n  namespace: orders\ndata:\n" +
			aliasBomb(pad))
		var err error
		n := allocated(func() { _, err = SetContainerMemoryLimit(in, deployment, mem("web", "512Mi", "768Mi")) })
		if !errors.Is(err, ErrInvalidYAML) || !strings.Contains(err.Error(), want) {
			t.Errorf("%d items: got %v, want %v naming %q", pad, err, ErrInvalidYAML, want)
		}
		if n >= executorMemory/2 {
			t.Errorf("%d items: the patcher allocated %d MiB, want less than half the executor's %d MiB", pad, n>>20,
				executorMemory>>20)
		}
	}
}

// peakInChild runs the patcher on the file build returns, in a child process that is killed after a minute,
// and fails when its peak heap is over budget, or when the file is not patched (refused empty) or not refused
// with InvalidYAML naming refused.
func peakInChild(t *testing.T, build func(t *testing.T) []byte, refused string) {
	t.Helper()
	deadline.Within(t, time.Minute, func(t *testing.T) {
		in := build(t)
		if len(in) > MaxBytes {
			t.Fatalf("the file has %d bytes, over the limit of %d", len(in), MaxBytes)
		}
		var (
			out []byte
			err error
		)
		peak := peakHeap(func() { out, err = SetContainerMemoryLimit(in, deployment, mem("web", "512Mi", "768Mi")) })
		t.Logf("%d bytes: peak heap %d MiB", len(in), peak>>20)
		switch {
		case refused == "" && (err != nil || len(out) != len(in)):
			t.Errorf("got %v, want the file patched", err)
		case refused != "" && (!errors.Is(err, ErrInvalidYAML) || !strings.Contains(err.Error(), refused)):
			t.Errorf("got %v, want %v naming %q", err, ErrInvalidYAML, refused)
		}
		if peak > budget {
			t.Errorf("the patcher held %d MiB at its peak, want at most %d MiB", peak>>20, budget>>20)
		}
	})
}

// withDense returns the Deployment of testdata, then a document of open, item repeated and last, of MaxBytes in
// all.
func withDense(t *testing.T, open, item, last string) []byte {
	t.Helper()
	head := string(read(t, "deployment.yaml")) + "---\n" + open
	return []byte(head + strings.Repeat(item, (MaxBytes-len(head)-len(last))/len(item)) + last)
}

// A file of MaxBytes packed with nodes is refused before any tree of it is built, whatever form the nodes take
// (#595). Before, the node budget was checked after the tree of a document was built: flow pairs peaked at
// 135 MiB and a flow mapping of keys without values at 176 MiB.
func TestDenseFilePeakHeap(t *testing.T) {
	cases := map[string]struct {
		open, item, last string
		refused          string
	}{
		"flow pairs":         {"[", "x: ,", "x: ]\n", nodesCause},
		"flow mapping":       {"{", "a,", "a}\n", nodesCause},
		"flow list":          {"[", "x,", "x]\n", nodesCause},
		"block mapping":      {"", "a:\n", "", nodesCause},
		"block list":         {"", "-\n", "", nodesCause},
		"block list of list": {"", "- -\n", "", nodesCause},
		"one long scalar":    {"a: ", "x", "\n", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			peakInChild(t, func(t *testing.T) []byte { return withDense(t, c.open, c.item, c.last) }, c.refused)
		})
	}
}

// listFile returns the Deployment of testdata and a ConfigMap holding a flow list of n+1 plain items.
func listFile(t *testing.T, n int) []byte {
	t.Helper()
	return []byte(string(read(t, "deployment.yaml")) +
		"---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: big, namespace: orders}\ndata: {x: [" +
		strings.Repeat("x,", n) + "x]}\n")
}

// keysFile returns the Deployment of testdata and a ConfigMap whose data holds n+1 flow mappings of eight keys
// without values: each key and its empty value are two nodes for the two bytes the count reads, the most nodes
// it accepts. The mappings are small because the decode compares every pair of keys of a mapping.
func keysFile(t *testing.T, n int) []byte {
	t.Helper()
	return []byte(string(read(t, "deployment.yaml")) +
		"---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: big, namespace: orders}\ndata: {x: [" +
		strings.Repeat("{a,b,c,d,e,f,g,h},", n) + "{a}]}\n")
}

// A file whose mapping repeats one long key is refused by the YAML check, before the decode's duplicate key
// check, in well under a second (#608). Before, that check built an error holding the key for every pair of
// equal keys: 2,048 copies of a 480-byte key took 4.7 s and 2.4 GB of allocations.
func TestDuplicateKeysRefusedInTime(t *testing.T) {
	var b strings.Builder
	b.Write(read(t, "deployment.yaml"))
	b.WriteString("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: big\n  namespace: orders\ndata:\n")
	b.WriteString(strings.Repeat("  "+strings.Repeat("k", 480)+": x\n", gityaml.MaxMappingKeys))
	deadline.Within(t, 30*time.Second, func(t *testing.T) {
		start := time.Now()
		_, err := SetContainerMemoryLimit([]byte(b.String()), deployment, mem("web", "512Mi", "768Mi"))
		took := time.Since(start)
		if !errors.Is(err, ErrInvalidYAML) || !strings.Contains(err.Error(), gityaml.ErrDuplicateKey.Error()) {
			t.Fatalf("got %v, want %v naming the duplicate key", err, ErrInvalidYAML)
		}
		if took > time.Second {
			t.Errorf("the refusal took %s, want well under a second", took)
		}
	})
}

// atBudget returns the most items of file that the node count accepts.
func atBudget(t *testing.T, file func(*testing.T, int) []byte) int {
	t.Helper()
	lo, hi := 0, MaxNodes
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if gityaml.Nodes(file(t, mid), MaxNodes) <= MaxNodes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// The largest files the node count accepts are patched within the budget, and one item more is refused (#595).
func TestNodeBudgetPeakHeap(t *testing.T) {
	for name, file := range map[string]func(*testing.T, int) []byte{"flow list": listFile, "flow mapping": keysFile} {
		t.Run(name+" at the budget", func(t *testing.T) {
			peakInChild(t, func(t *testing.T) []byte { return file(t, atBudget(t, file)) }, "")
		})
		t.Run(name+" over the budget", func(t *testing.T) {
			_, err := SetContainerMemoryLimit(file(t, atBudget(t, file)+1), deployment, mem("web", "512Mi", "768Mi"))
			if !errors.Is(err, ErrInvalidYAML) || !strings.Contains(err.Error(), "the file holds more than 131072 YAML nodes") {
				t.Errorf("got %v, want %v naming the nodes", err, ErrInvalidYAML)
			}
		})
	}
}

// verify compares the patched file with the values findDocument kept and leaves them as they were.
func TestVerifyKeepsTheOriginal(t *testing.T) {
	in := read(t, "deployment.yaml")
	p := mem("web", "512Mi", "768Mi")
	field, err := sourcemap.FieldFor(v1alpha1.Action{
		Type: v1alpha1.ActionSetContainerMemoryLimit, SetContainerMemoryLimit: &p,
	}, deployment)
	if err != nil {
		t.Fatal(err)
	}
	found, err := findDocument(in, deployment)
	if err != nil {
		t.Fatal(err)
	}
	out, err := SetContainerMemoryLimit(in, deployment, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(found, out, field, p.Desired.String()); err != nil {
		t.Fatal(err)
	}
	fresh, err := findDocument(in, deployment)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(found.values, fresh.values) {
		t.Error("verify changed the values of the original it compares with")
	}
}

// wideFile returns the Deployment of testdata and a ConfigMap whose data is one block mapping of n distinct
// keys without values, three counted nodes for each, so the most keys one mapping can hold within MaxNodes.
func wideFile(t *testing.T, n int) []byte {
	t.Helper()
	var b strings.Builder
	b.Write(read(t, "deployment.yaml"))
	b.WriteString("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: big\n  namespace: orders\ndata:\n")
	for i := range n {
		fmt.Fprintf(&b, "  k%d:\n", i)
	}
	return []byte(b.String())
}

// A file within MaxNodes whose one mapping holds as many keys as the node count lets it is refused in well under
// a second (#608). Before, the duplicate key check of the decode compared every pair of keys: about 43,000 keys
// took 4 s of CPU, and about ten times that under the race detector. A mapping of gityaml.MaxMappingKeys keys
// is still patched.
func TestWideMappingRefusedInTime(t *testing.T) {
	p := mem("web", "512Mi", "768Mi")
	t.Run("at the node budget", func(t *testing.T) {
		deadline.Within(t, 30*time.Second, func(t *testing.T) {
			in := wideFile(t, atBudget(t, wideFile))
			start := time.Now()
			_, err := SetContainerMemoryLimit(in, deployment, p)
			took := time.Since(start)
			if !errors.Is(err, ErrInvalidYAML) || !strings.Contains(err.Error(), gityaml.ErrTooManyKeys.Error()) {
				t.Fatalf("got %v, want %v naming the keys", err, ErrInvalidYAML)
			}
			// The race detector slows this about tenfold (3.9 s on CI), so the time is checked only without it;
			// make test runs this test once more without -race.
			if !raceDetector && took > time.Second {
				t.Errorf("the refusal took %s, want well under a second", took)
			}
		})
	})
	t.Run("at the key bound", func(t *testing.T) {
		deadline.Within(t, 30*time.Second, func(t *testing.T) {
			if _, err := SetContainerMemoryLimit(wideFile(t, gityaml.MaxMappingKeys), deployment, p); err != nil {
				t.Fatalf("got %v, want the file patched", err)
			}
		})
	})
}
