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

package gityaml_test

import (
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"

	"github.com/pkalin/shadewright/internal/gitops/gityaml"
)

// treeNodes returns the nodes go.yaml.in/yaml/v3 builds for data, documents included, and whether it parses.
func treeNodes(data string) (int, bool) {
	var count func(n *yaml.Node) int
	count = func(n *yaml.Node) int {
		c := 1
		for _, ch := range n.Content {
			c += count(ch)
		}
		return c
	}
	dec := yaml.NewDecoder(strings.NewReader(data))
	c := 0
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			return c, errors.Is(err, io.EOF)
		}
		c += count(&n)
	}
}

// nodeCases are inputs where a node starts without a separating space, comes from an empty value, or follows
// a token that is not a plain scalar: each place where the count could miss a node.
var nodeCases = []string{
	"",
	"a\n",
	"a: b\n",
	"a:\n",
	"a: b c d\n",
	"- \n-\n- a\n",
	"a:\n- b\n-\n",
	"? \n? a\n: b\n",
	"[x: ,x: ,x: ]\n",
	"[a: b, c]\n",
	"[? a, ? b: c]\n",
	"{a,a,a}\n",
	"{a}\n",
	"{[a], {b}}\n",
	"{? , ? a: b}\n",
	`{"a":b,"c":[d]}` + "\n",
	`["a" ,b]` + "\n",
	"[[a],b,{c: d}]\n",
	"[a ,b , c]\n",
	"[a, ?b]\n",
	"a: !!str x\n",
	"a: !t\n",
	"a: !t x\nb: !t [y]\n",
	"[!t x, !t ]\n",
	"a: |\n  x: y\n  [a, b]\nb: >-\n  z\n",
	"--- a\n--- b\n...\n---\n",
	"---\n---\n",
	"a\n...\n---\nb\n",
	"\ufeffa: b\n",
	"a: b\n\ufeffc: d\n",
	"\ufeff--- a\n--- b\n",
	"\ufeff---\n---\n",
	"a:\tb\n",
	"a: 'x y' \n",
	"a: \"x\\\" y\"\n",
	"a: 'it''s'\n",
	"a: \"x\n  y\"\n",
	"a: b\n  c\n",
	"{a: [b, {c: [d, e]}], f: g}\n",
	"- - - a\n  - b\n",
	"a: [b, c]  # [d, e]\n",
	"#a\n#[x: ,x: ]\n",
	"a: -b\nc: ?d\ne: :f\n",
	"[-a, ?b]\n",
	"a: b:c\n",
	"[a:b, c: d]\n",
	"? [a]\n: [b]\n",
	"[{a: b}: c]\n",
	"{[a, b]: c, {d: e}: f}\n",
}

// The count is an upper bound of the nodes the parser builds, for every case and the dense forms (#595).
func TestNodesUpperBound(t *testing.T) {
	dense := []string{
		"[" + strings.Repeat("x: ,", 100) + "x: ]\n",
		"{" + strings.Repeat("a,", 100) + "a}\n",
		"[" + strings.Repeat("x,", 100) + "x]\n",
		strings.Repeat("a:\n", 100),
		strings.Repeat("-\n", 100),
		strings.Repeat("? \n", 100),
		strings.Repeat("[", 50) + strings.Repeat("]", 50) + "\n",
		strings.Repeat("---\n", 100),
	}
	for _, in := range append(nodeCases, dense...) {
		real, ok := treeNodes(in)
		if !ok {
			t.Errorf("%q does not parse", in)
			continue
		}
		if got := gityaml.Nodes([]byte(in), 1<<20); got < real {
			t.Errorf("Nodes(%q) = %d, want at least the %d nodes of the tree", in, got, real)
		}
	}
}

// The count stops at limit+1.
func TestNodesLimit(t *testing.T) {
	in := []byte("[" + strings.Repeat("x,", 1000) + "x]\n")
	if got := gityaml.Nodes(in, 10); got != 11 {
		t.Errorf("Nodes = %d, want 11", got)
	}
}

// A file whose count is over MaxNodes is refused before any parser builds its tree (#595).
func TestObjectsTooManyNodes(t *testing.T) {
	in := []byte("a: [" + strings.Repeat("x,", gityaml.MaxNodes) + "x]\n")
	_, err := gityaml.Objects(in, 2)
	var e *gityaml.Error
	if !errors.As(err, &e) || !errors.Is(err, gityaml.ErrTooManyNodes) || e.Line != 0 {
		t.Fatalf("Objects = %v, want %v for the whole file", err, gityaml.ErrTooManyNodes)
	}
}

// FuzzNodes checks that the count is never under the nodes the parser builds for input it accepts.
func FuzzNodes(f *testing.F) {
	for _, in := range nodeCases {
		f.Add(in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 4096 || !utf8.ValidString(in) {
			return
		}
		real, ok := treeNodes(in)
		if !ok {
			return
		}
		if got := gityaml.Nodes([]byte(in), 1<<20); got < real {
			t.Errorf("Nodes(%q) = %d, want at least the %d nodes of the tree", in, got, real)
		}
	})
}
