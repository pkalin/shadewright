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
package semdiff

import (
	"fmt"
	"strings"
	"testing"
)

// patchFile is the patch file of the fixtures below.
const patchFile = "p.yaml"

// TestGrowthCountsAnUntargetedPatchAsATargetedOne checks #626 at the bound itself: a patch without a target
// counts at least as much as the same patch with a target that selects the one object it names.
func TestGrowthCountsAnUntargetedPatchAsATargetedOne(t *testing.T) {
	var p strings.Builder
	p.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n")
	for i := range 200 {
		fmt.Fprintf(&p, "  k%d: v\n", i)
	}
	bound := func(entry string) int {
		b, refused := buildBound(t, map[string][]byte{
			rootKust:  []byte("resources: [cm.yaml]\npatches:\n  - path: " + patchFile + "\n" + entry),
			"cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n"),
			patchFile: []byte(p.String()),
		})
		if refused {
			t.Fatal("refused")
		}
		return b
	}
	targeted, untargeted := bound("    target: {kind: ConfigMap, name: cm}\n"), bound("")
	if untargeted < targeted || targeted == 0 {
		t.Fatalf("bound without a target %d, with one %d: want at least as much", untargeted, targeted)
	}
}

// TestMergeWidthsPaths counts keys and items per path: the keys that name the object count for none, the items
// of a sequence and the keys of their mappings share one path each, and patches add up path by path.
func TestMergeWidthsPaths(t *testing.T) {
	a, err := patchMergesOf("a", []byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\n  namespace: n\n"+
		"  labels: {x: y}\nspec:\n  containers:\n    - name: a\n      env: [{name: E}]\n    - name: b\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := patchMergesOf("b", []byte("- op: add\n  path: /metadata/labels/z\n  value: v\n---\n"+
		"apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\n  labels: {z: v}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a.json || !b.json || len(a.docs) != 1 || len(b.docs) != 1 || b.docs[0].key != (objectKey{"Pod", "p"}) {
		t.Fatalf("got %+v and %+v, want one document each naming Pod p, and a JSON patch in b", a, b)
	}
	k := newMergedKeys()
	k.apply(&ops{merges: []mergePatch{{merges: a}, {merges: b}}}, false)
	m := k.paths
	counts := k.objects[objectKey{"Pod", "p"}]
	md := m.child(0, "metadata")
	containers := m.child(m.child(0, "spec"), "containers")
	item := m.child(containers, itemKey)
	for name, c := range map[string]struct{ at, want int }{
		"root (spec only)":             {0, 1},
		"metadata (labels, twice)":     {md, 2},
		"metadata/labels":              {m.child(md, "labels"), 2},
		"spec/containers items":        {containers, 2},
		"keys of the containers items": {item, 3},
		"env items":                    {m.child(item, "env"), 1},
	} {
		if got := counts[c.at]; got != c.want {
			t.Errorf("%s: got %d, want %d", name, got, c.want)
		}
	}
	if got := k.widest(); got != 3 {
		t.Errorf("widest: got %d, want 3", got)
	}
}

// TestMergedKeysPerObject sums per object: patches naming distinct objects do not add up, a patch with a target
// counts toward every object of its kind (or of any kind), and after a rename every patch counts toward every
// object.
func TestMergedKeysPerObject(t *testing.T) {
	data := func(kind, name string, n int) *patchMerges {
		var p strings.Builder
		fmt.Fprintf(&p, "kind: %s\nmetadata:\n  name: %s\ndata:\n", kind, name)
		for i := range n {
			fmt.Fprintf(&p, "  k%d: v\n", i)
		}
		pm, err := patchMergesOf("p", []byte(p.String()))
		if err != nil {
			t.Fatal(err)
		}
		return pm
	}
	a, b := mergePatch{merges: data("ConfigMap", "a", 3)}, mergePatch{merges: data("ConfigMap", "b", 5)}
	cms := mergePatch{merges: data("ConfigMap", "x", 7), targeted: true, kind: "ConfigMap"}
	secrets := mergePatch{merges: data("Secret", "x", 11), targeted: true, kind: "Secret"}
	all := mergePatch{merges: data("Pod", "x", 13), targeted: true, kind: anyKind}
	for name, c := range map[string]struct {
		patches []mergePatch
		renamed bool
		want    int
	}{
		"distinct objects":                  {[]mergePatch{a, b, a}, false, 6},
		"one object and its kind":           {[]mergePatch{a, b, cms}, false, 12},
		"another kind":                      {[]mergePatch{a, b, secrets}, false, 11},
		"any kind":                          {[]mergePatch{a, b, cms, secrets, all}, false, 25},
		"renamed: every patch on every one": {[]mergePatch{a, b, a}, true, 11},
	} {
		t.Run(name, func(t *testing.T) {
			k := newMergedKeys()
			k.apply(&ops{merges: c.patches}, c.renamed)
			if got := k.widest(); got != c.want {
				t.Errorf("widest: got %d, want %d", got, c.want)
			}
		})
	}
}
