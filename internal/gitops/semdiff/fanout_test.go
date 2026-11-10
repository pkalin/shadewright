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
	"fmt"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

const oneByteOver = "one byte over"

// transformBudget is semdiff.MaxTransformBytes as a literal, so the test pins the bound (#495).
const transformBudget = 4 << 20

func wantTransformBudget(t *testing.T, err error) {
	t.Helper()
	wantReason(t, err, semdiff.ErrTooLarge)
	if !strings.Contains(err.Error(), fmt.Sprintf("add more than %d bytes", transformBudget)) {
		t.Fatalf("got %v, want the transform budget named", err)
	}
}

// annotatedRepo is ordersRepo with 1,022 ConfigMaps and one common annotation whose key and value are v bytes,
// so the build holds 1,024 objects (documents: a Service, a ScaledObject and the ConfigMaps), none of a kind
// with a pod template, so the annotation reaches one place in each: v bytes and 3 nodes (the mapping, the key
// and the value), plus 4 nodes for metadata/annotations, which it may create. Its bound is 1,024 x (v + 7 x 128).
func annotatedRepo(v int) map[string][]byte {
	files := ordersRepo()
	files[soKust] = append([]byte("commonAnnotations:\n  a: "+strings.Repeat("x", v-1)+"\n"),
		kust("scaledobject.yaml", "cms.yaml")...)
	files["apps/orders/cms.yaml"] = configMapsYAML(1022)
	return files
}

// annotationAtBudget is the largest annotation whose bound is within the budget: 1,024 x (3,200 + 896) =
// 4,194,304, exactly 4 MiB.
const annotationAtBudget = 3200

// TestCheckTransformBudget reproduces #495: an annotation of 64 KiB on 1,024 objects adds 64 MiB to the rendered
// objects (on 1,000 objects it measured a Go heap peak of 127 MiB, the executor's memory limit), from about
// 120 KiB of input, within every input budget.
// It is refused before the render; the largest annotation the bound allows is accepted and rendered.
func TestCheckTransformBudget(t *testing.T) {
	for name, c := range map[string]struct {
		v    int
		over bool
	}{
		"far over the budget":   {64 << 10, true},
		oneByteOver:             {annotationAtBudget + 1, true},
		"just under the budget": {annotationAtBudget, false},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := semdiff.Check(context.Background(), soInput(t, annotatedRepo(c.v), "apps/orders", 15))
			if c.over {
				wantTransformBudget(t, err)
				return
			}
			if err != nil || res.Objects != 1024 {
				t.Fatalf("got %+v, %v, want the change accepted over 1,024 objects", res, err)
			}
		})
	}
}

func TestLoadTransformBudget(t *testing.T) {
	for name, c := range map[string]struct {
		v    int
		over bool
	}{atBudget: {annotationAtBudget, false}, oneByteOver: {annotationAtBudget + 1, true}} {
		t.Run(name, func(t *testing.T) {
			git := fake.NewGit("main", annotatedRepo(c.v))
			_, err := semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
			if !c.over {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			wantTransformBudget(t, err)
		})
	}
}

func load(t *testing.T, files map[string][]byte, dir string) (map[string][]byte, error) {
	t.Helper()
	git := fake.NewGit("main", files)
	return semdiff.Load(context.Background(), git, head(t, git), dir)
}

// pods returns one Pod with n containers, each with the image "a", under spec.containers.
func pods(n int) []byte {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\n  namespace: orders\nspec:\n  containers:\n")
	for i := range n {
		fmt.Fprintf(&b, "    - name: c%d\n      image: a\n      env:\n        - name: E\n          valueFrom:\n"+
			"            configMapKeyRef:\n              name: cm0\n              key: k\n", i)
	}
	return []byte(b.String())
}

// TestCheckTransformBudgetPerKind checks every patch and transformer kind the renderer allows: each with a value
// far over the budget for the places it reaches is refused, and each with a small value is accepted. The
// resource files are 1,022 ConfigMaps and one Pod of 1,000 containers (each with an image and a name reference),
// so the build holds 1,025 documents; with the bound of each kind, the large value is over the budget and the
// small one well under it.
func TestCheckTransformBudgetPerKind(t *testing.T) {
	big := strings.Repeat("x", 16<<10)
	targeted := func(v string) string {
		return "patches:\n  - target:\n      kind: ConfigMap\n    patch: |\n      apiVersion: v1\n      kind: ConfigMap\n" +
			"      metadata:\n        name: any\n      data:\n        k: " + v + "\n"
	}
	cases := map[string]func(v string) string{
		"commonLabels": func(v string) string { return "commonLabels:\n  a: " + v + "\n" },
		"labels":       func(v string) string { return "labels:\n  - pairs:\n      a: " + v + "\n" },
		// A field spec of its own may pass any sequence, so it counts a place in every item of every sequence:
		// one through the Pod's 2,000 containers and env items would refuse even the small value.
		"labels with fields": func(v string) string {
			return "labels:\n  - pairs:\n      a: " + v + "\n    fields:\n      - path: metadata/x\n        create: true\n"
		},
		"commonAnnotations":     func(v string) string { return "commonAnnotations:\n  a: " + v + "\n" },
		"namespace":             func(v string) string { return "namespace: n" + v + "\n" },
		"namePrefix":            func(v string) string { return "namePrefix: p" + v + "\n" },
		"nameSuffix":            func(v string) string { return "nameSuffix: s" + v + "\n" },
		"images":                func(v string) string { return "images:\n  - name: a\n    newName: b" + v + "\n" },
		"replicas":              func(v string) string { return "replicas:\n  - name: p" + v + "\n    count: 1\n" },
		"targeted inline patch": targeted,
	}
	for name, extra := range cases {
		for _, over := range []bool{true, false} {
			if name == "replicas" && over {
				// replicas writes its count, never its name, and only in kinds this repository does not hold.
				continue
			}
			t.Run(fmt.Sprintf("%s over=%v", name, over), func(t *testing.T) {
				v := "y"
				if over {
					v = big
				}
				files := ordersRepo()
				files[soKust] = append([]byte(extra(v)), kust("scaledobject.yaml", "cms.yaml", "pod.yaml")...)
				files["apps/orders/cms.yaml"] = configMapsYAML(1022)
				files["apps/orders/pod.yaml"] = pods(1000)
				_, err := load(t, files, "apps/orders")
				if over {
					wantTransformBudget(t, err)
				} else if err != nil {
					t.Fatalf("Load: %v", err)
				}
			})
		}
	}
}

// TestCheckTransformBudgetTargetedPatchFile counts a patch file with a target once per object, and the same
// patch without a target once: kustomize applies that to the one object it names.
func TestCheckTransformBudgetTargetedPatchFile(t *testing.T) {
	patchYAML := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm0\n  namespace: orders\ndata:\n  k: " +
		strings.Repeat("x", 16<<10) + "\n"
	for name, c := range map[string]struct {
		entry string
		over  bool
	}{
		"with a target":    {"patches:\n  - path: p.yaml\n    target:\n      name: cm0\n", true},
		"without a target": {"patches:\n  - path: p.yaml\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			files := ordersRepo()
			files[soKust] = append([]byte(c.entry), kust("scaledobject.yaml", "cms.yaml")...)
			files["apps/orders/cms.yaml"] = configMapsYAML(1022)
			files["apps/orders/p.yaml"] = []byte(patchYAML)
			_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
			if c.over {
				wantTransformBudget(t, err)
			} else if err != nil {
				t.Fatalf("Check: %v", err)
			}
		})
	}
}

// TestCheckTransformBudgetCountsEveryVisit puts a transformer under the budget in a base that four overlays
// reach: each visit applies it again, so the build is over the budget although no one kustomization is.
func TestCheckTransformBudgetCountsEveryVisit(t *testing.T) {
	for name, c := range map[string]struct {
		overlays int
		over     bool
	}{"one path": {1, false}, "four paths": {4, true}} {
		t.Run(name, func(t *testing.T) {
			files := map[string][]byte{
				"apps/base/kustomization.yaml": append([]byte("commonAnnotations:\n  a: "+strings.Repeat("x", 1<<10)+"\n"),
					kust("cms.yaml")...),
				"apps/base/cms.yaml": configMapsYAML(1000),
			}
			res := make([]string, 0, c.overlays)
			for i := range c.overlays {
				d := fmt.Sprintf("o%d", i)
				res = append(res, "../"+d)
				files["apps/"+d+"/kustomization.yaml"] = append([]byte("nameSuffix: -"+d+"\n"), kust("../base")...)
			}
			files["apps/root/kustomization.yaml"] = kust(res...)
			_, err := load(t, files, "apps/root")
			if c.over {
				wantTransformBudget(t, err)
			} else if err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

// TestLoadTransformBudgetComponent puts the transformer in a component: it applies to the objects of the
// kustomization that lists it, 1,022 ConfigMaps here, which its own directory does not hold.
func TestLoadTransformBudgetComponent(t *testing.T) {
	for name, c := range map[string]struct {
		v    int
		over bool
	}{"small": {16, false}, "large": {16 << 10, true}} {
		t.Run(name, func(t *testing.T) {
			files := ordersRepo()
			files[soKust] = []byte("resources: [scaledobject.yaml, cms.yaml]\ncomponents: [../comp]\n")
			files["apps/orders/cms.yaml"] = configMapsYAML(1022)
			files["apps/comp/kustomization.yaml"] = []byte("apiVersion: kustomize.config.k8s.io/v1alpha1\n" +
				"kind: Component\ncommonAnnotations:\n  a: " + strings.Repeat("x", c.v) + "\n")
			_, err := load(t, files, "apps/orders")
			if c.over {
				wantTransformBudget(t, err)
			} else if err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

// denseEntries returns the body of a flow mapping of entries with one-character values ("a0: x, a1: x, ...")
// of about size bytes: two YAML nodes for every 6 to 8 bytes.
func denseEntries(size int) string {
	var b strings.Builder
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "a%d: x, ", i)
	}
	return b.String() + "z: x"
}

// TestLoadTransformBudgetCountsNodes reproduces the node-dense builds of the #495 review: within the byte bound
// alone, each put about a million YAML nodes into the render (821 to 1,188 MiB of Go heap for the patches,
// 418 MiB for the annotation, measured with hack/load/render). The render holds every node, whatever its bytes,
// so they are refused before it.
func TestLoadTransformBudgetCountsNodes(t *testing.T) {
	patch := func(body string) string {
		return "patches:\n  - target:\n      kind: ConfigMap\n    patch: '" + body + "'\n"
	}
	for name, extra := range map[string]string{
		// About 3.5 KiB on each of 1,024 objects, under 4 MiB in all: about 1,000 nodes each, in two mappings
		// so that neither is over gityaml.MaxMappingKeys keys.
		"targeted patch of tiny entries": patch("{apiVersion: v1, kind: ConfigMap, metadata: {name: any, " +
			"annotations: {" + denseEntries(1750) + "}, labels: {" + denseEntries(1750) + "}}}"),
		"targeted patch of a list of numbers": patch("{apiVersion: v1, kind: ConfigMap, metadata: {name: any}, " +
			"extra: [" + strings.Repeat("1,", 1750) + "1]}"),
		// About 1.3 KiB in each of 3 places of 1,024 objects, under 4 MiB in all: about 400 nodes per place.
		"commonAnnotations of one-character keys": "commonAnnotations: {" + denseEntries(1300) + "}\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := ordersRepo()
			files[soKust] = append([]byte(extra), kust("scaledobject.yaml", "cms.yaml")...)
			files["apps/orders/cms.yaml"] = configMapsYAML(1022)
			_, err := load(t, files, "apps/orders")
			wantTransformBudget(t, err)
		})
	}
}
