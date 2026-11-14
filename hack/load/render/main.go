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

// Command render runs the executor's pre-commit render check (internal/gitops/semdiff) on the largest
// repository its limits allow, and prints the time and the memory it took. hack/load/measure.sh runs it in a
// container with the executor's memory limit, so the process peak is the cgroup's. Test tooling only.
//
// Shapes (n MiB of ConfigMaps; with n = 4, the default, both sit just under the 4 MiB build budget, counted with
// repetition, and at most at the 1,024 object cap; a larger n is refused):
//   - flat:    n files of about 1 MiB (255 ConfigMaps of 4 KiB each) listed by one kustomization, plus the Deployment.
//   - diamond: one base of about 1 MiB (255 ConfigMaps) reached through n overlays with a name prefix, plus
//     the Deployment.
//   - tiny:    n files of about 1 MiB of ConfigMaps of about 78 bytes each (about 13,400 per file), plus the
//     Deployment: far over the document bound (semdiff.MaxBuildDocuments), so refused before the render.
//   - docs:    n ConfigMaps of about 78 bytes each in files of 1,000, plus the Deployment (n+1 documents); with
//     n = 4095 the build is at the document bound, over the object cap, which is checked
//     after kustomize has built them, so it measures the largest render the document bound lets through.
//   - patches: 1,023 ConfigMaps of about 78 bytes, plus the Deployment, and n strategic merge patches of them
//     (one label each, files of 1,000): with n = 1708 the build is at the transform budget
//     (semdiff.MaxTransformBytes), which counts a patch without a target as one with a target since #626; with
//     n = 3072 (the document bound, every object patched three times) it is refused before the render.
//   - annotations: as flat with 4 MiB, plus a common annotation of one long value sized so that the bound on
//     what patches and transformers add to the render (semdiff.MaxTransformBytes) is at n percent of the budget:
//     with n = 100 the build is at every byte budget at once (#495). The size is n percent of the largest the
//     walk accepts, found by semdiff.Load; CALIBRATE_SIZE=<size> sets it instead (to measure past the budget).
//   - images:  the same with a Pod of 1,000 containers whose image an images entry renames: the costliest
//     place per byte measured (#495).
//   - dense, denseseq, denseannotations: the same with values of many YAML nodes, where memory follows nodes
//     more than bytes: a patch with a target on every ConfigMap that adds one-character annotations (dense) or
//     a list of numbers (denseseq), or a common annotation of one-character keys (denseannotations) (#495).
//   - heavy:   as docs, each ConfigMap padded so that the build is just under the 4 MiB byte budget: both
//     input bounds at once. Files hold as many as fit in 1 MiB, and a ConfigMap is at most about 1 MiB, so
//     with n below 4 the build stays under the byte budget.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/patch"
	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

const (
	webName      = "web"
	shapeDiamond = "diamond"
	shapeTiny    = "tiny"
	shapeDocs    = "docs"
	shapeHeavy   = "heavy"
	shapePatches = "patches"
	shapeAnnot   = "annotations"
	shapeImages  = "images"
	// node-dense: many tiny entries or items (#495).
	shapeDense      = "dense"
	shapeDenseSeq   = "denseseq"
	shapeDenseAnnot = "denseannotations"
)

const deployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: web
spec:
  replicas: 1
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: web
          image: registry.example.com/web:1.0.0
          resources:
            requests:
              memory: 256Mi
            limits:
              memory: 512Mi
`

func main() {
	semdiff.RunRenderProcess() // Check renders in a child process of this binary (ADR-0035)
	shape, n := "flat", 4
	if len(os.Args) > 1 {
		shape = os.Args[1]
	}
	if len(os.Args) > 2 {
		var err error
		if n, err = strconv.Atoi(os.Args[2]); err != nil || n < 1 {
			fmt.Fprintln(os.Stderr,
				"usage: render flat|diamond|tiny|docs|heavy|patches|annotations|images|dense|denseseq|denseannotations "+
					"[MiB, number of ConfigMaps or percent, default 4]")
			os.Exit(2)
		}
	}
	files, path := build(shape, n)
	runtime.GC() // what fit left behind is not part of the check
	var in int
	for _, b := range files {
		in += len(b)
	}
	target := v1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: webName, Name: webName}
	patchTarget := target
	if shape == shapeDiamond {
		target.Name = "o0-web" // the rendered name; the file holds "web"
	}
	act := v1alpha1.Action{
		Type: v1alpha1.ActionSetContainerMemoryLimit,
		SetContainerMemoryLimit: &v1alpha1.SetContainerMemoryLimit{
			Container: webName, Previous: resource.MustParse("512Mi"), Desired: resource.MustParse("768Mi")},
	}
	file := dir(shape) + "deployment.yaml"
	patched, err := patch.SetContainerMemoryLimit(files[file], patchTarget, *act.SetContainerMemoryLimit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "patch:", err)
		os.Exit(1)
	}

	var peakHeap, peakSys atomic.Uint64
	done := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			for old := peakHeap.Load(); m.HeapInuse > old && !peakHeap.CompareAndSwap(old, m.HeapInuse); old = peakHeap.Load() {
			}
			for old := peakSys.Load(); m.Sys > old && !peakSys.CompareAndSwap(old, m.Sys); old = peakSys.Load() {
			}
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	start := time.Now()
	res, err := semdiff.Check(context.Background(), semdiff.Input{
		Files: files, Path: path, File: file, Patched: patched, Target: target, Action: act})
	took := time.Since(start)
	close(done)

	outcome := fmt.Sprintf("objects=%d", res.Objects)
	if se, ok := errors.AsType[*semdiff.Error](err); ok {
		// The diamond changes every copy of the shared base: refused, but only after both renders.
		outcome = "refused:" + string(se.Reason)
	} else if err != nil {
		outcome = "error:" + err.Error()
	}
	fmt.Printf("shape=%s input_mib=%.1f files=%d time_s=%.2f go_heap_inuse_peak_mib=%.0f "+
		"go_sys_peak_mib=%.0f rss_peak_mib=%s %s\n",
		shape, float64(in)/(1<<20), len(files), took.Seconds(), float64(peakHeap.Load())/(1<<20),
		float64(peakSys.Load())/(1<<20), rssPeak(), outcome)
}

// rssPeak is the process's peak resident set (VmHWM in /proc/self/status, Linux only), in MiB.
func rssPeak() string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "n/a"
	}
	for l := range strings.SplitSeq(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "VmHWM:" {
			kb, _ := strconv.Atoi(f[1])
			return fmt.Sprintf("%.0f", float64(kb)/1024)
		}
	}
	return "n/a"
}

func dir(shape string) string {
	if shape == shapeDiamond {
		return "apps/base/"
	}
	return "apps/"
}

// configMaps returns n ConfigMaps of about 4 KiB each.
func configMaps(prefix string, n int) []byte {
	var b strings.Builder
	pad := strings.Repeat("x", 3900)
	for i := range n {
		fmt.Fprintf(&b, "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s-%d\n  namespace: web\n"+
			"data:\n  payload: %s\n", prefix, i, pad)
	}
	return []byte(b.String())
}

// tinyConfigMaps returns ConfigMaps of about 78 bytes each (about 13,400 per MiB), at most size bytes in all.
func tinyConfigMaps(prefix string, size int) []byte {
	var b strings.Builder
	for i := 0; ; i++ {
		d := fmt.Sprintf("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s-%d\n  namespace: web\n", prefix, i)
		if b.Len()+len(d) > size {
			return []byte(b.String())
		}
		b.WriteString(d)
	}
}

// paddedConfigMaps returns n ConfigMaps, numbered from first, of about size bytes each (at least about 78).
func paddedConfigMaps(first, n, size int) []byte {
	var b strings.Builder
	for i := first; i < first+n; i++ {
		d := fmt.Sprintf("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: d-%d\n  namespace: web\n", i)
		b.WriteString(d)
		if extra := size - len(d) - len("data:\n  p: \n"); extra > 0 {
			fmt.Fprintf(&b, "data:\n  p: %s\n", strings.Repeat("x", extra))
		}
	}
	return []byte(b.String())
}

// entries returns the body of a flow mapping of n entries with one-character values.
func entries(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "a%d: x, ", i)
	}
	return b.String() + "z: x"
}

// targetedPatch returns a patches list of one inline patch with a target that selects every ConfigMap.
func targetedPatch(body string) string {
	return "patches:\n  - target:\n      kind: ConfigMap\n    patch: '" + body + "'\n"
}

// fit returns the kustomization file kust makes for the size that is pct percent of the largest the walk
// accepts (semdiff.Load, which checks semdiff.MaxTransformBytes without rendering): with 100 the build is at
// the budget, with 101 just over it.
func fit(files map[string][]byte, kust func(size int) []byte, pct int) []byte {
	accepts := func(size int) bool {
		files["apps/kustomization.yaml"] = kust(size)
		git := fake.NewGit("main", files)
		head, err := git.BranchHead(context.Background(), "main")
		if err != nil {
			panic(err)
		}
		_, err = semdiff.Load(context.Background(), git, head, "apps")
		if err != nil && !errors.Is(err, semdiff.ErrTooLarge) {
			fmt.Fprintln(os.Stderr, "fit:", err)
			os.Exit(1)
		}
		return err == nil
	}
	if v := os.Getenv("CALIBRATE_SIZE"); v != "" {
		n, _ := strconv.Atoi(v)
		return kust(n)
	}
	lo, hi := 0, 1
	for accepts(hi) {
		lo, hi = hi, hi*2
	}
	for hi-lo > 1 {
		if mid := (lo + hi) / 2; accepts(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	size := lo * pct / 100
	if pct > 100 {
		size = max(size, hi)
	}
	fmt.Fprintf(os.Stderr, "fit: largest accepted size %d, using %d\n", lo, size)
	return kust(size)
}

func kust(extra string, resources ...string) []byte {
	var b strings.Builder
	b.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" + extra + "resources:\n")
	for _, r := range resources {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	return []byte(b.String())
}

// build makes the repository: n MiB of ConfigMaps (255 of 4 KiB per MiB, or tiny ones) in the given shape.
// With 4 (the default) flat and diamond sit just under the 4 MiB build budget and the 1,024 object cap.
func build(shape string, n int) (map[string][]byte, string) {
	files := map[string][]byte{}
	switch shape {
	case shapeDiamond:
		files["apps/base/kustomization.yaml"] = kust("", "deployment.yaml", "cm.yaml")
		files["apps/base/deployment.yaml"] = []byte(deployment)
		files["apps/base/cm.yaml"] = configMaps("cm", 255)
		overlays := make([]string, 0, n)
		for i := range n {
			d := fmt.Sprintf("o%d", i)
			overlays = append(overlays, d)
			files["apps/"+d+"/kustomization.yaml"] = kust(fmt.Sprintf("namePrefix: %s-\n", d), "../base")
		}
		files["apps/kustomization.yaml"] = kust("", overlays...)
	case shapeDocs, shapeHeavy:
		files["apps/deployment.yaml"] = []byte(deployment)
		pad, per := 0, 1000
		if shape == shapeHeavy {
			// One padded ConfigMap must fit in a file, so with a small n the build stays under the byte budget.
			pad = min((semdiff.MaxBuildBytes-8192-len(deployment))/n, semdiff.MaxFileBytes-1024)
			per = max(1, min(per, (semdiff.MaxFileBytes-1024)/max(pad, 128)))
		}
		// Files of at most per ConfigMaps each, so that each stays under the 1 MiB file limit.
		res := []string{"deployment.yaml"}
		for i := 0; i*per < n; i++ {
			f := fmt.Sprintf("cm%d.yaml", i)
			res = append(res, f)
			files["apps/"+f] = paddedConfigMaps(i*per, min(n-i*per, per), pad)
		}
		files["apps/kustomization.yaml"] = kust("", res...)
	case shapePatches:
		files["apps/deployment.yaml"] = []byte(deployment)
		files["apps/cm.yaml"] = paddedConfigMaps(0, 1023, 0)
		var k strings.Builder
		k.WriteString("patches:\n")
		for i := 0; i*1000 < n; i++ {
			var p strings.Builder
			for j := i * 1000; j < min(n, (i+1)*1000); j++ {
				fmt.Fprintf(&p, "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: d-%d\n  namespace: web\n"+
					"  labels:\n    p%d: \"x\"\n", j%1023, j)
			}
			f := fmt.Sprintf("p%d.yaml", i)
			files["apps/"+f] = []byte(p.String())
			fmt.Fprintf(&k, "  - path: %s\n", f)
		}
		files["apps/kustomization.yaml"] = kust(k.String(), "deployment.yaml", "cm.yaml")
	case shapeAnnot, shapeImages, shapeDense, shapeDenseSeq, shapeDenseAnnot:
		// The build at the input budgets (4 x 255 ConfigMaps of 4 KiB and the Deployment, 1,021 documents),
		// plus one transformer or targeted patch sized by fit.
		res := append(make([]string, 0, 6), "deployment.yaml")
		files["apps/deployment.yaml"] = []byte(deployment)
		for i := range 4 {
			f := fmt.Sprintf("cm%d.yaml", i)
			res = append(res, f)
			files["apps/"+f] = configMaps(fmt.Sprintf("cm%d", i), 255)
		}
		if shape == shapeImages {
			var p strings.Builder
			p.WriteString("apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\n  namespace: web\nspec:\n  containers:\n")
			for range 1000 {
				p.WriteString("    - image: a\n")
			}
			files["apps/pod.yaml"] = []byte(p.String())
			res = append(res, "pod.yaml")
		}
		extra := func(size int) string {
			switch shape {
			case shapeAnnot:
				return "commonAnnotations:\n  a: " + strings.Repeat("x", size) + "\n"
			case shapeImages:
				return "images:\n  - name: a\n    newName: " + strings.Repeat("x", size) + "\n"
			case shapeDenseAnnot:
				return "commonAnnotations: {" + entries(size) + "}\n"
			case shapeDense:
				return targetedPatch("{apiVersion: v1, kind: ConfigMap, metadata: {name: any, annotations: {" +
					entries(size) + "}}}")
			default:
				return targetedPatch("{apiVersion: v1, kind: ConfigMap, metadata: {name: any}, extra: [" +
					strings.Repeat("1,", size) + "1]}")
			}
		}
		k := fit(files, func(size int) []byte { return kust(extra(size), res...) }, n)
		files["apps/kustomization.yaml"] = k
	case shapeTiny:
		res := append(make([]string, 0, n+1), "deployment.yaml")
		files["apps/deployment.yaml"] = []byte(deployment)
		for i := range n {
			f := fmt.Sprintf("cm%d.yaml", i)
			res = append(res, f)
			files["apps/"+f] = tinyConfigMaps(fmt.Sprintf("c%d", i), 1<<20-1024)
		}
		files["apps/kustomization.yaml"] = kust("", res...)
	default:
		res := append(make([]string, 0, n+1), "deployment.yaml")
		files["apps/deployment.yaml"] = []byte(deployment)
		for i := range n {
			f := fmt.Sprintf("cm%d.yaml", i)
			res = append(res, f)
			files["apps/"+f] = configMaps(fmt.Sprintf("cm%d", i), 255)
		}
		files["apps/kustomization.yaml"] = kust("", res...)
	}
	return files, "apps"
}
