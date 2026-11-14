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
package semdiff_test

import (
	"context"
	"errors"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	"github.com/pkalin/shadewright/internal/testutil/deadline"
)

// chainedRepo is ordersRepo with a base whose common annotation x holds value bytes on one ConfigMap, an overlay
// patch that moves the annotation into the ConfigMap's name, and a Deployment with refs references to that
// ConfigMap, which kustomize then renames: the value lands in every reference (#614). The walk counts the
// annotation once and the move as one value, so its estimate stays small whatever refs is.
func chainedRepo(value, refs int) map[string][]byte {
	files := ordersRepo()
	files["apps/orders/base/kustomization.yaml"] = append(
		[]byte("commonAnnotations:\n  x: "+strings.Repeat("n", value)+"\n"), kust("cm.yaml")...)
	files["apps/orders/base/cm.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm0\n  namespace: orders\n")
	var b strings.Builder
	b.WriteString("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: refs\n  namespace: orders\nspec:\n" +
		"  selector:\n    matchLabels: {a: b}\n  template:\n    metadata:\n      labels: {a: b}\n    spec:\n" +
		"      containers:\n        - name: c\n          image: a\n          envFrom:\n")
	for range refs {
		b.WriteString("            - configMapRef:\n                name: cm0\n")
	}
	files["apps/orders/refs.yaml"] = []byte(b.String())
	files[soKust] = append(kust("base", "scaledobject.yaml", "refs.yaml"), []byte("patches:\n  - target:\n"+
		"      kind: ConfigMap\n      name: cm0\n    patch: |\n      - op: move\n        from: /metadata/annotations/x\n"+
		"        path: /metadata/name\n")...)
	return files
}

// heapPeak returns the most Go heap this process held while f ran, sampled every millisecond.
func heapPeak(f func()) uint64 {
	var (
		peak atomic.Uint64
		stop atomic.Bool
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		var s goruntime.MemStats
		for !stop.Load() {
			goruntime.ReadMemStats(&s)
			peak.Store(max(peak.Load(), s.HeapAlloc))
			time.Sleep(time.Millisecond)
		}
	}()
	f()
	stop.Store(true)
	<-done
	return peak.Load()
}

// TestCheckBoundsAChainedNameReference reproduces #614: a 64 KiB annotation moved into a ConfigMap name that
// 1,000 references copy, about 100 KB of input within every budget the walk checks, rendered 66 MB and peaked
// at 374 MiB of Go heap in the executor's process (5.6 s), and the check passed. The render process stops at
// its memory limit, so the check is refused with TooLarge, and the executor's own heap stays small.
func TestCheckBoundsAChainedNameReference(t *testing.T) {
	files := chainedRepo(64<<10, 1000)
	if _, err := load(t, files, "apps/orders"); err != nil {
		t.Fatalf("Load: %v, want the build within every budget of the walk", err)
	}
	deadline.Within(t, 60*time.Second, func(t *testing.T) {
		var err error
		peak := heapPeak(func() {
			_, err = semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
		})
		if !errors.Is(err, semdiff.ErrTooLarge) || !strings.Contains(err.Error(), "MiB of memory") {
			t.Fatalf("got %v, want TooLarge at the render process's memory limit", err)
		}
		if peak > 64<<20 {
			t.Errorf("this process peaked at %d MiB of Go heap, want the render outside it", peak>>20)
		}
	})
}

// TestCheckRendersAnOrdinaryChain checks the same chain with a short value: a base annotation moved into a name
// that kustomize copies into 1,000 references renders in the render process and passes.
func TestCheckRendersAnOrdinaryChain(t *testing.T) {
	started := semdiff.CountRenderProcesses(t)
	res, err := semdiff.Check(context.Background(), soInput(t, chainedRepo(16, 1000), "apps/orders", 15))
	if err != nil || res.Objects != 4 || res.Desired != "15" {
		t.Fatalf("got %+v, %v, want the change accepted over 4 objects", res, err)
	}
	if started() != 1 {
		t.Fatalf("Check started %d render processes, want 1", started())
	}
}
