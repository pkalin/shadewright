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
	"errors"
	"fmt"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
	"sigs.k8s.io/kustomize/api/provider"
)

// kustomizeObjects returns how many objects kustomize reads from data, or false when it refuses it.
func kustomizeObjects(data []byte) (int, bool) {
	nodes, err := provider.NewDefaultDepProvider().GetResourceFactory().RNodesFromBytes(data)
	return len(nodes), err == nil
}

func cm(name string) string {
	return fmt.Sprintf("{apiVersion: v1, kind: ConfigMap, metadata: {name: %s}}", name)
}

func TestDocumentsBoundWhatKustomizeReads(t *testing.T) {
	cases := map[string]struct {
		in   string
		want int // documents' answer; kustomize must read at most this many objects
	}{
		"one":                    {"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n", 1},
		"separators and empties": {"---\n" + cm("a") + "\n--- # c\n" + cm("b") + "\n---\n---\n", 4},
		"CRLF":                   {cm("a") + "\r\n---\r\n" + cm("b") + "\r\n", 2},
		"a volume with items":    {"kind: Deployment\nmetadata: {name: a}\nspec: {items: [1, 2, 3]}\n", 1},
		"List":                   {"apiVersion: v1\nkind: List\nitems: [" + cm("a") + ", " + cm("b") + ", " + cm("c") + "]\n", 3},
		"empty List":             {"apiVersion: v1\nkind: ConfigMapList\nitems: []\n", 0},
		"List among documents":   {cm("x") + "\n---\nkind: ConfigMapList\nitems: [" + cm("a") + ", " + cm("b") + "]\n", 3},
		"nested Lists": {"kind: List\nitems:\n  - kind: List\n    items: [" + cm("a") + ", " + cm("b") + "]\n  - " + cm("c") +
			"\n", 3},
		"aliases count per use": {"kind: List\nx: &a " + cm("a") + "\nitems: [*a, *a, *a, *a]\n", 4},
		"aliased inner List": {"kind: List\nx: &l {kind: List, items: [" + cm("a") + ", " + cm("b") + "]}\n" +
			"items: [*l, *l, *l]\n", 6},
		"merge key":          {"x: &m {kind: List, items: [" + cm("a") + ", " + cm("b") + "]}\n<<: *m\n", 2},
		"escaped kind":       {"\"k\\x69nd\": \"Config\\x4dapList\"\n\"\\x69tems\": [" + cm("a") + ", " + cm("b") + "]\n", 2},
		"items as a mapping": {"kind: List\nitems: {a: " + cm("a") + ", b: " + cm("b") + "}\n", 4},
		"not YAML":           {"kind: List\nitems: [\n", 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := documents([]byte(c.in))
			if err != nil || got != c.want {
				t.Fatalf("documents = %d, %v, want %d", got, err, c.want)
			}
			if n, ok := kustomizeObjects([]byte(c.in)); ok && n > got {
				t.Fatalf("kustomize reads %d objects, more than the bound %d", n, got)
			}
		})
	}
}

// kustomize crashes the process on these (a Go stack overflow in kyaml's deAnchor), so they are not given to it.
func TestDocumentsRefuseAnAnchorThatContainsItself(t *testing.T) {
	for _, in := range []string{
		"kind: List\nitems: &s [*s]\n",
		cm("a") + "\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: b}\ndata: &d {x: *d}\n",
		"a: &a {b: &b [*a]}\n",
		"m: &m {<<: *m}\n",
	} {
		if _, err := documents([]byte(in)); !errors.Is(err, errSelfAlias) {
			t.Fatalf("documents(%q): %v, want errSelfAlias", in, err)
		}
	}
	for _, in := range []string{"a: &a {x: 1}\nb: *a\nc: [*a, *a]\n", "a: &a 1\nb: &b [*a, *a]\nc: [*b, *b]\n"} {
		if _, err := documents([]byte(in)); err != nil {
			t.Fatalf("documents(%q): %v, want no error", in, err)
		}
	}
}

func TestDocumentsStopAtTheBudget(t *testing.T) {
	plain := strings.Repeat("\n---", 3*MaxBuildDocuments)
	if got, _ := documents([]byte(plain)); got != MaxBuildDocuments+1 {
		t.Fatalf("documents = %d, want %d", got, MaxBuildDocuments+1)
	}
	// Each level multiplies the items by 8 through aliases: 8^6 objects, counted without expanding them.
	var b strings.Builder
	b.WriteString("kind: List\nl0: &l0 " + cm("a") + "\n")
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&b, "l%d: &l%d {kind: List, items: [%s]}\n", i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*l%d, ", i-1), 8), ", "))
	}
	b.WriteString("items: [*l6]\n")
	if got, _ := documents([]byte(b.String())); got != MaxBuildDocuments+1 {
		t.Fatalf("documents = %d, want %d", got, MaxBuildDocuments+1)
	}
}

// FuzzDocuments checks that documents never counts fewer objects than kustomize reads from the same bytes.
func FuzzDocuments(f *testing.F) {
	f.Add([]byte("kind: List\nitems: [" + cm("a") + ", " + cm("b") + "]\n"))
	f.Add([]byte(cm("a") + "\n---\n" + cm("b") + "\n"))
	f.Add([]byte("kind: List\nx: &a " + cm("a") + "\nitems: [*a, *a]\n"))
	f.Add([]byte("\"\\x69tems\": [" + cm("a") + "]\nkind: \"\\x4cist\"\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			return
		}
		got, err := documents(data)
		if err != nil || got > MaxBuildDocuments || expansionLarge(data) {
			return
		}
		if n, ok := kustomizeObjects(data); ok && n > got {
			t.Fatalf("kustomize reads %d objects from %q, more than the bound %d", n, data, got)
		}
	})
}

// expansionLarge reports whether replacing the aliases of data by copies, as kustomize does, makes more than
// 100,000 nodes, too many for kustomize to read quickly in a fuzz run.
func expansionLarge(data []byte) bool {
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil {
		return false
	}
	memo := map[*yaml.Node]int{}
	var size func(n *yaml.Node) int
	size = func(n *yaml.Node) int {
		if n == nil {
			return 0
		}
		if c, ok := memo[n]; ok {
			return c
		}
		memo[n] = 1
		c := 1 + size(n.Alias)
		for _, k := range n.Content {
			c = min(c+size(k), 1<<20)
		}
		memo[n] = c
		return c
	}
	return size(&root) > 100000
}
