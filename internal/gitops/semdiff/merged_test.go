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
)

// mergedKeysBound is semdiff.MaxMergedKeys as a literal, so the test pins the bound (#626).
const mergedKeysBound = 4096

func wantMergedKeysBound(t *testing.T, err error) {
	t.Helper()
	wantReason(t, err, semdiff.ErrTooLarge)
	if !strings.Contains(err.Error(), fmt.Sprintf("merge more than %d keys", mergedKeysBound)) {
		t.Fatalf("got %v, want the merged keys bound named", err)
	}
}

// mergedPatches is ordersRepo with one ConfigMap and one strategic merge patch of it per entry of keys, each
// adding that many distinct data keys, with a target naming the ConfigMap or without one. As in the probe of
// #626, the ConfigMap has a one-letter name and no namespace, so the bound of a name change ([MaxTransformBytes])
// stays low.
func mergedPatches(keys []int, targeted bool) map[string][]byte {
	files := ordersRepo()
	var k strings.Builder
	k.WriteString("patches:\n")
	for i, n := range keys {
		var p strings.Builder
		p.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\ndata:\n")
		for j := range n {
			fmt.Fprintf(&p, "  k%03d-%04d: v\n", i, j)
		}
		name := fmt.Sprintf("p%d.yaml", i)
		files["apps/orders/"+name] = []byte(p.String())
		fmt.Fprintf(&k, "  - path: %s\n", name)
		if targeted {
			k.WriteString("    target: {kind: ConfigMap, name: c}\n")
		}
	}
	files[soKust] = append([]byte(k.String()), kust("scaledobject.yaml", "cm.yaml")...)
	files["apps/orders/cm.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n")
	return files
}

// repeat returns n copies of v.
func repeat(v, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// TestCheckMergedKeysBound reproduces #626: 60 patches without a target, each of 255 data keys of one
// ConfigMap, passed every bound before the render (the transform budget counted 2.7 of 4 MiB) and rendered one
// mapping of 15,300 keys in 16 s, allocating 4 GB. Patches with and without a target are refused before the
// render once they merge more than the bound into one mapping; at the bound the build renders.
func TestCheckMergedKeysBound(t *testing.T) {
	for name, c := range map[string]struct {
		keys     []int
		targeted bool
		over     bool
	}{
		"60 patches of 255 keys without a target (#626)": {repeat(255, 60), false, true},
		"at the bound without a target":                  {repeat(256, 16), false, false},
		"one key over without a target":                  {append(repeat(256, 16), 1), false, true},
		"at the bound with a target":                     {repeat(256, 16), true, false},
		"one key over with a target":                     {append(repeat(256, 16), 1), true, true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := semdiff.Check(context.Background(), soInput(t, mergedPatches(c.keys, c.targeted), "apps/orders", 15))
			if c.over {
				wantMergedKeysBound(t, err)
			} else if err != nil {
				t.Fatalf("Check: %v", err)
			}
		})
	}
}

// TestLoadTransformBudgetUntargetedPatch counts a patch without a target as it counts one with a target that
// selects one object (#626): one of about 51,000 YAML nodes (160 mappings of 160 keys, so no path merges more
// than MaxMergedKeys) is over the budget whether it has a target or not. Before, only the target was counted.
func TestLoadTransformBudgetUntargetedPatch(t *testing.T) {
	var p strings.Builder
	p.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm0\n  namespace: orders\nx:\n")
	for i := range 160 {
		fmt.Fprintf(&p, "  m%d:\n", i)
		for j := range 160 {
			fmt.Fprintf(&p, "    k%d: 1\n", j)
		}
	}
	for name, entry := range map[string]string{
		"with a target":    "patches:\n  - path: p.yaml\n    target: {kind: ConfigMap, name: cm0}\n",
		"without a target": "patches:\n  - path: p.yaml\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := ordersRepo()
			files[soKust] = append([]byte(entry), kust("scaledobject.yaml", "cms.yaml")...)
			files["apps/orders/cms.yaml"] = configMapsYAML(1)
			files["apps/orders/p.yaml"] = []byte(p.String())
			_, err := load(t, files, "apps/orders")
			wantTransformBudget(t, err)
		})
	}
}

// configMapsPatched is ordersRepo with n ConfigMaps and one patch file without a target holding, for each, a
// strategic merge patch that sets six data keys.
func configMapsPatched(n int) map[string][]byte {
	files := ordersRepo()
	var p strings.Builder
	for i := range n {
		fmt.Fprintf(&p, "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm%d\n  namespace: orders\n"+
			"data: {a: '1', b: '2', c: '3', d: '4', e: '5', f: '6'}\n", i)
	}
	files["apps/orders/cms.yaml"] = configMapsYAML(n)
	files["apps/orders/data.yaml"] = []byte(p.String())
	files[soKust] = append([]byte("patches:\n  - path: data.yaml\n"), kust("scaledobject.yaml", "cms.yaml")...)
	return files
}

// TestCheckMergedKeysPerObject checks that the bound sums per object, as design 8.2.2 says: 800 ConfigMaps
// with one small patch each render, though the patches together merge 4,800 keys into the data mappings,
// which the first sum, across objects, refused.
func TestCheckMergedKeysPerObject(t *testing.T) {
	if _, err := semdiff.Check(context.Background(), soInput(t, configMapsPatched(800), "apps/orders", 15)); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// TestCheckMergedKeysAfterRename keeps the bound an upper bound where a patch may reach an object under another
// name: below a name prefix, patches naming the ConfigMap by its old and its new name count together.
func TestCheckMergedKeysAfterRename(t *testing.T) {
	for name, c := range map[string]struct {
		prefix string
		over   bool
	}{
		"no prefix: two objects":           {"", false},
		"a prefix below: one object, over": {"namePrefix: x-\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			files := mergedPatches(repeat(256, 16), false)
			files["apps/orders/base/kustomization.yaml"] = append([]byte(c.prefix), kust("cm.yaml")...)
			files["apps/orders/base/cm.yaml"] = files["apps/orders/cm.yaml"]
			delete(files, "apps/orders/cm.yaml")
			files["apps/orders/other.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x-c\n")
			files["apps/orders/p16.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x-c\ndata:\n  z: v\n")
			k := strings.Replace(string(files[soKust]), "patches:\n", "patches:\n  - path: p16.yaml\n", 1)
			k = strings.Replace(k, "  - cm.yaml\n", "  - base\n", 1)
			if c.prefix == "" {
				k += "  - other.yaml\n"
			}
			files[soKust] = []byte(k)
			_, err := semdiff.Check(context.Background(), soInput(t, files, "apps/orders", 15))
			if c.over {
				wantMergedKeysBound(t, err)
			} else if err != nil {
				t.Fatalf("Check: %v", err)
			}
		})
	}
}
