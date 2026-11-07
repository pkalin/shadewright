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
//   - tiny:    n files of about 1 MiB of ConfigMaps of about 100 bytes each, plus the Deployment: far over the
//     object cap, which is checked after kustomize has built them, so it measures the render the byte budget
//     still lets through.
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
)

const (
	webName      = "web"
	shapeDiamond = "diamond"
	shapeTiny    = "tiny"
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
	shape, n := "flat", 4
	if len(os.Args) > 1 {
		shape = os.Args[1]
	}
	if len(os.Args) > 2 {
		var err error
		if n, err = strconv.Atoi(os.Args[2]); err != nil || n < 1 {
			fmt.Fprintln(os.Stderr, "usage: render flat|diamond|tiny [MiB of ConfigMaps, default 4]")
			os.Exit(2)
		}
	}
	files, path := build(shape, n)
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

// tinyConfigMaps returns ConfigMaps of about 100 bytes each, at most size bytes in all.
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
