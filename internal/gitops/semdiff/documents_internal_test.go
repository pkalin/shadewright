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
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"

	"sigs.k8s.io/kustomize/api/provider"

	"github.com/pkalin/shadewright/internal/gitops/gityaml"
)

// kustomizeObjects returns how many objects kustomize reads from data, or false when it refuses it.
func kustomizeObjects(data []byte) (int, bool) {
	nodes, err := provider.NewDefaultDepProvider().GetResourceFactory().RNodesFromBytes(data)
	return len(nodes), err == nil
}

func cm(name string) string {
	return fmt.Sprintf("{apiVersion: v1, kind: ConfigMap, metadata: {name: %s}}", name)
}

// cms returns a YAML flow sequence of n ConfigMaps.
func cms(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = cm(fmt.Sprintf("c%d", i))
	}
	return "[" + strings.Join(items, ", ") + "]"
}

func TestDocumentsBoundWhatKustomizeReads(t *testing.T) {
	cases := map[string]struct {
		in   string
		want int // documents' answer; kustomize must read at most this many objects
	}{
		"one": {"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n", 1},
		// kustomize splits at "\n---" up to the end of the line, so "---\n---\n" is one separator and one part.
		"separators and empties": {"---\n" + cm("a") + "\n--- # c\n" + cm("b") + "\n---\n---\n", 3},
		"CRLF":                   {cm("a") + "\r\n---\r\n" + cm("b") + "\r\n", 2},
		"UTF-8 byte order mark":  {"\uFEFF" + cm("a") + "\n", 1},
		"a volume with items":    {"kind: Deployment\nmetadata: {name: a}\nspec: {items: [1, 2, 3]}\n", 1},
		"List":                   {"apiVersion: v1\nkind: List\nitems: [" + cm("a") + ", " + cm("b") + ", " + cm("c") + "]\n", 4},
		"empty List":             {"apiVersion: v1\nkind: ConfigMapList\nitems: []\n", 1},
		"List among documents":   {cm("x") + "\n---\nkind: ConfigMapList\nitems: [" + cm("a") + ", " + cm("b") + "]\n", 4},
		"nested Lists": {"kind: List\nitems:\n  - kind: List\n    items: [" + cm("a") + ", " + cm("b") + "]\n  - " + cm("c") +
			"\n", 5},
		"escaped kind":       {"\"k\\x69nd\": \"Config\\x4dapList\"\n\"\\x69tems\": [" + cm("a") + ", " + cm("b") + "]\n", 3},
		"items as a mapping": {"kind: List\nitems: {a: " + cm("a") + ", b: " + cm("b") + "}\n", 5},
		"a quoted << value":  {"kind: ConfigMap\nmetadata: {name: a}\ndata: {x: \"<<\"}\n", 1},
		// Only a key merges: a plain "<<" value has the merge tag but is plain text to kustomize.
		"a plain << value":     {"kind: ConfigMap\nmetadata: {name: a}\ndata: {operator: <<}\n", 1},
		"a plain << in a list": {"kind: ConfigMap\nmetadata: {name: a}\nx: [<<, a]\n", 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := documents("f.yaml", []byte(c.in))
			if err != nil || got != c.want {
				t.Fatalf("documents = %d, %v, want %d", got, err, c.want)
			}
			if n, ok := kustomizeObjects([]byte(c.in)); ok && n > got {
				t.Fatalf("kustomize reads %d objects, more than the bound %d", n, got)
			}
		})
	}
}

// utf16LE returns s in UTF-16, little endian, with a byte order mark, which kustomize reads.
func utf16LE(s string) []byte {
	b := binary.LittleEndian.AppendUint16(make([]byte, 0, 2+2*len(s)), 0xFEFF)
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

// aliasKeyList returns a List of n objects whose "kind" and "items" keys are aliases to other values: kustomize
// matches an alias key by the anchor's name, so it reads a List, while the value it stands for names none. About
// 1 MiB for n = 25,000.
func aliasKeyList(n int) string {
	var b strings.Builder
	b.WriteString("x: &kind a\ny: &items b\n*kind : List\n*items :\n")
	for i := range n {
		fmt.Fprintf(&b, "- {kind: A, metadata: {name: c%d}}\n", i)
	}
	return b.String()
}

// aliasBomb returns a few hundred bytes that expand into 10^9 nodes when aliases are replaced by copies.
func aliasBomb() string {
	var b strings.Builder
	b.WriteString("kind: List\nl0: &l0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&b, "l%d: &l%d [%s]\n", i, i, strings.TrimSuffix(strings.Repeat(fmt.Sprintf("*l%d, ", i-1), 10), ", "))
	}
	b.WriteString("items: *l9\n")
	return b.String()
}

// Each of these expanded in kustomize into far more objects than an earlier count gave (#488, #492), so they
// are refused before kustomize reads them. The anchors that contain themselves crashed the process (a Go stack
// overflow in kyaml's deAnchor), so none of these is given to kustomize.
func TestDocumentsRefuseWhatKustomizeExpands(t *testing.T) {
	const line1, line2 = "line 1:", "line 2:"
	cases := map[string]struct {
		in   []byte
		line string
	}{
		"alias keys":          {[]byte(aliasKeyList(5000)), line1},
		"UTF-16, 5,000 items": {utf16LE("apiVersion: v1\nkind: List\nitems: " + cms(5000) + "\n"), ""},
		"UTF-16 big endian":   {[]byte{0xFE, 0xFF, 0, 'a', 0, ':', 0, ' ', 0, '1', 0, '\n'}, ""},
		"UTF-32":              {[]byte{0xFF, 0xFE, 0, 0, 'a', 0, 0, 0}, ""},
		"invalid UTF-8":       {[]byte(cm("a") + "\xC3\n"), ""},
		"a !!merge tag, 5,000 items": {[]byte("apiVersion: v1\nkind: List\n!!merge ignored: {items: " + cms(5000) + "}\n"),
			"line 3:"},
		"a merge tag in long form": {[]byte("kind: List\n!<tag:yaml.org,2002:merge> x: {items: []}\n"), line2},
		"a merge tag by handle": {[]byte("%TAG !y! tag:yaml.org,2002:\n---\nkind: List\n!y!m%65rge x: {items: []}\n"),
			""},
		"a << key":           {[]byte("x: {a: 1}\n<<: {kind: List}\n"), line2},
		"a quoted << key":    {[]byte("\"<<\": {kind: List}\n"), line1},
		"an alias bomb":      {[]byte(aliasBomb()), line2},
		"an unused anchor":   {[]byte(cm("a") + "\n---\nkind: ConfigMap\nmetadata: &m {name: b}\n"), "line 4:"},
		"a self alias":       {[]byte("kind: List\nitems: &s [*s]\n"), line2},
		"a self alias deep":  {[]byte("a: &a {b: &b [*a]}\n"), line1},
		"a self merge":       {[]byte("m: &m {<<: *m}\n"), line1},
		"does not parse":     {[]byte(cm("a") + "\n---\nkind: List\nitems: [\n"), ""},
		"control characters": {[]byte("a: \x00\n"), ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := documents("f.yaml", c.in)
			if !errors.Is(err, ErrInvalidYAML) {
				t.Fatalf("documents: %v, want InvalidYAML", err)
			}
			if !strings.Contains(err.Error(), "f.yaml") || !strings.Contains(err.Error(), c.line) {
				t.Fatalf("documents: %v, want the file and %q named", err, c.line)
			}
		})
	}
}

// A file of 1 MiB packed with nodes is refused with TooLarge before any tree of it is built (#595).
func TestDocumentsRefuseTooManyNodes(t *testing.T) {
	in := []byte("[" + strings.Repeat("x: ,", (1<<20-6)/4) + "x: ]\n")
	_, err := documents("f.yaml", in)
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "f.yaml holds more than 131072 YAML nodes") {
		t.Fatalf("documents: %v, want TooLarge naming the file and its nodes", err)
	}
}

// A mapping of more than gityaml.MaxMappingKeys keys is refused with TooLarge before kustomize decodes it, whose
// duplicate key check is quadratic in the keys of one mapping (#608).
func TestDocumentsRefuseWideMapping(t *testing.T) {
	in := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: big\ndata:\n" +
		strings.Repeat("  k:\n", gityaml.MaxMappingKeys+1))
	_, err := documents("f.yaml", in)
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "f.yaml line 6: a mapping holds more than 256 keys") {
		t.Fatalf("documents: %v, want TooLarge naming the file, the line and the keys", err)
	}
}

// A mapping that repeats a key is refused with InvalidYAML before kustomize decodes it, whose duplicate key check
// builds an error holding the key for every pair of equal keys (#608): 2,048 copies of a 480-byte key took 1.5 s
// and 911 MB of allocations in a render.
func TestDocumentsRefuseDuplicateKey(t *testing.T) {
	in := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: big\ndata:\n" +
		strings.Repeat("  "+strings.Repeat("k", 480)+": x\n", gityaml.MaxMappingKeys))
	_, err := documents("f.yaml", in)
	if !errors.Is(err, ErrInvalidYAML) || !strings.Contains(err.Error(), "f.yaml line 7: "+gityaml.ErrDuplicateKey.Error()) {
		t.Fatalf("documents: %v, want InvalidYAML naming the file, the line and the duplicate key", err)
	}
	// Two "kind" keys, which the count of a List would have had to read both of.
	_, err = documents("f.yaml", []byte("kind: ConfigMap\nkind: List\nitems: ["+cm("a")+"]\n"))
	if !errors.Is(err, ErrInvalidYAML) || !strings.Contains(err.Error(), "f.yaml line 2: "+gityaml.ErrDuplicateKey.Error()) {
		t.Fatalf("documents: %v, want InvalidYAML for two kind keys", err)
	}
}

func TestDocumentsStopAtTheBudget(t *testing.T) {
	for name, in := range map[string]string{
		"separators": strings.Repeat("\n---", 3*MaxBuildDocuments),
		// Each costs kustomize a node: counted as none, 20,000 of them slipped past the budget. 5,000 are over
		// it and under the node budget (#595).
		"empty Lists": strings.Repeat("kind: List\nitems: []\n---\n", 5000),
		"a long List": "kind: List\nitems: " + cms(MaxBuildDocuments) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := documents("f.yaml", []byte(in)); err != nil || got != MaxBuildDocuments+1 {
				t.Fatalf("documents = %d, %v, want %d", got, err, MaxBuildDocuments+1)
			}
		})
	}
}

// FuzzDocuments checks that documents never panics, refuses only with InvalidYAML or, for too many YAML nodes
// or keys in one mapping, TooLarge, and never counts fewer
// objects than kustomize reads from bytes it accepts. Every input reaches kustomize: anchors are refused, so
// nothing accepted expands.
func FuzzDocuments(f *testing.F) {
	f.Add([]byte("kind: List\nitems: [" + cm("a") + ", " + cm("b") + "]\n"))
	f.Add([]byte(cm("a") + "\n---\n" + cm("b") + "\n"))
	f.Add([]byte("kind: List\nx: &a " + cm("a") + "\nitems: [*a, *a]\n"))
	f.Add([]byte("\"\\x69tems\": [" + cm("a") + "]\nkind: \"\\x4cist\"\n"))
	f.Add([]byte("kind: List\n!!merge x: {items: [" + cm("a") + "]}\n"))
	f.Add([]byte("kind: List\nitems: []\n---\nkind: List\nitems: []\n"))
	f.Add(utf16LE("kind: List\nitems: [" + cm("a") + "]\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := documents("f.yaml", data)
		if err != nil {
			if !errors.Is(err, ErrInvalidYAML) && (!errors.Is(err, ErrTooLarge) ||
				!strings.Contains(err.Error(), gityaml.ErrTooManyNodes.Error()) &&
					!strings.Contains(err.Error(), gityaml.ErrTooManyKeys.Error())) {
				t.Fatalf("documents(%q): %v, want only InvalidYAML or TooLarge for the nodes or keys", data, err)
			}
			return
		}
		if got < 1 || got > MaxBuildDocuments+1 {
			t.Fatalf("documents(%q) = %d, out of range", data, got)
		}
		if n, ok := kustomizeObjects(data); ok && n > got {
			t.Fatalf("kustomize reads %d objects from %q, more than the bound %d", n, data, got)
		}
	})
}
