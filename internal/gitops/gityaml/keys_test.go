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
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"

	"github.com/pkalin/shadewright/internal/gitops/gityaml"
)

// widestMapping returns the most keys of one mapping in the documents go.yaml.in/yaml/v3 builds for data, and
// whether it parses.
func widestMapping(data string) (int, bool) {
	var widest func(n *yaml.Node) int
	widest = func(n *yaml.Node) int {
		w := 0
		if n.Kind == yaml.MappingNode {
			w = len(n.Content) / 2
		}
		for _, c := range n.Content {
			w = max(w, widest(c))
		}
		return w
	}
	dec := yaml.NewDecoder(strings.NewReader(data))
	w := 0
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			return w, errors.Is(err, io.EOF)
		}
		w = max(w, widest(&n))
	}
}

// duplicateKeys reports whether a mapping in the documents go.yaml.in/yaml/v3 builds for data holds two scalar
// keys that are surely one key (exact: the same text, or values that decode equal) or may be (loose: also the
// same as JSON object keys, numbers compared at 32-bit precision), and whether data parses.
func duplicateKeys(data string) (exact, loose, ok bool) {
	looseKey := func(v any) string {
		switch v := v.(type) {
		case nil:
			return "null"
		case string:
			return v
		case int:
			return strconv.FormatFloat(float64(float32(v)), 'g', -1, 32)
		case float64:
			if math.IsNaN(v) {
				return ".nan"
			}
			return strconv.FormatFloat(float64(float32(v)), 'g', -1, 32)
		}
		return fmt.Sprint(v)
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			var keys []*yaml.Node
			for i := 0; i < len(n.Content); i += 2 {
				if n.Content[i].Kind == yaml.ScalarNode {
					keys = append(keys, n.Content[i])
				}
			}
			for i, a := range keys {
				for _, b := range keys[i+1:] {
					var va, vb any
					ea, eb := a.Decode(&va), b.Decode(&vb)
					same := a.Value == b.Value || ea == nil && eb == nil && reflect.DeepEqual(va, vb)
					exact = exact || same
					loose = loose || same || ea == nil && eb == nil && looseKey(va) == looseKey(vb)
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	dec := yaml.NewDecoder(strings.NewReader(data))
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			return exact, loose, errors.Is(err, io.EOF)
		}
		walk(&n)
	}
}

// blockKeys returns a ConfigMap whose data holds n keys without values, one per line.
func blockKeys(n int) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: big\ndata:\n")
	for i := range n {
		fmt.Fprintf(&b, "  k%d:\n", i)
	}
	return b.String()
}

// flowKeys returns a flow mapping of n distinct keys without values.
func flowKeys(n int) string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%d", i)
	}
	return "a: [{" + strings.Join(keys, ",") + "}]\n"
}

// A mapping of more than MaxMappingKeys keys is refused at the line of its first key, in a block or a flow mapping, at any depth
// and in any document; one of MaxMappingKeys is accepted (#608).
func TestObjectsMappingKeys(t *testing.T) {
	cases := []struct {
		name string
		in   string
		line int
	}{
		{"block mapping", blockKeys(gityaml.MaxMappingKeys + 1), 6},
		{"flow mapping", flowKeys(gityaml.MaxMappingKeys + 1), 1},
		{"second document", "a: b\n---\n" + blockKeys(gityaml.MaxMappingKeys+1), 8},
		{"top level", strings.Repeat("k:\n", gityaml.MaxMappingKeys+1), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := gityaml.Objects([]byte(c.in), 10)
			var e *gityaml.Error
			if !errors.As(err, &e) || !errors.Is(err, gityaml.ErrTooManyKeys) || e.Line != c.line {
				t.Fatalf("Objects = %v, want %v at line %d", err, gityaml.ErrTooManyKeys, c.line)
			}
		})
	}
	for name, in := range map[string]string{
		"block mapping": blockKeys(gityaml.MaxMappingKeys),
		"flow mapping":  flowKeys(gityaml.MaxMappingKeys),
	} {
		t.Run(name+" at the bound", func(t *testing.T) {
			if _, err := gityaml.Objects([]byte(in), 10); err != nil {
				t.Fatalf("Objects = %v, want no error", err)
			}
		})
	}
}

// A scalar key a mapping already holds is refused at its line, however it is quoted or tagged and at any depth,
// before any decoder compares the keys (#608): 2,048 copies of one long key took seconds and gigabytes in the
// decoders' duplicate key check. A mapping wider than MaxMappingKeys is refused for its width first. The same
// key in two mappings, complex keys and distinct numbers pass. Keys are compared as decoders resolve them, not
// as text only: a hexadecimal 0x1 and 1, or ~ and null, are one key, and the last value would silently win.
func TestObjectsDuplicateKeys(t *testing.T) {
	cases := []struct {
		name string
		in   string
		line int
		want error
	}{
		{"repeated in a block mapping", "a: 1\nb: 2\na: 3\n", 3, gityaml.ErrDuplicateKey},
		{"quoted", "a: 1\n'a': 2\n", 2, gityaml.ErrDuplicateKey},
		{"tagged", "1: x\n!!str 1: y\n", 2, gityaml.ErrDuplicateKey},
		{"hexadecimal and decimal", "0x1: a\n1: b\n", 2, gityaml.ErrDuplicateKey},
		{"two spellings of null", "~: a\nnull: b\n", 2, gityaml.ErrDuplicateKey},
		{"empty and null", "? \n: a\nNULL: b\n", 3, gityaml.ErrDuplicateKey},
		{"two spellings of true", "true: a\nTrue: b\n", 2, gityaml.ErrDuplicateKey},
		{"float and integer", "1.0: a\n1: b\n", 2, gityaml.ErrDuplicateKey},
		{"integer and quoted text", "0x1: a\n'1': b\n", 2, gityaml.ErrDuplicateKey},
		{"octal and decimal", "0o10: a\n8: b\n", 2, gityaml.ErrDuplicateKey},
		{"null and its text", "~: a\n'~': b\n", 2, gityaml.ErrDuplicateKey},
		{"repeated in a flow mapping", "x: [{k, k}]\n", 1, gityaml.ErrDuplicateKey},
		{"nested", "metadata:\n  labels:\n    a: b\n    a: c\n", 4, gityaml.ErrDuplicateKey},
		{"second document", "a: b\n---\nc: d\nc: e\n", 4, gityaml.ErrDuplicateKey},
		{"too wide first", strings.Repeat("k:\n", gityaml.MaxMappingKeys+1), 1, gityaml.ErrTooManyKeys},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := gityaml.Objects([]byte(c.in), 10)
			var e *gityaml.Error
			if !errors.As(err, &e) || !errors.Is(err, c.want) || e.Line != c.line {
				t.Fatalf("Objects = %v, want %v at line %d", err, c.want, c.line)
			}
		})
	}
	for name, in := range map[string]string{
		"two mappings":     "a: {k: 1}\nb: {k: 2}\n",
		"complex keys":     "? [a]\n: 1\n? [b]\n: 2\n",
		"values":           "a: x\nb: x\n",
		"distinct numbers": "1: a\n2: b\n0x10: c\n",
		"YAML 1.1 words":   "on: a\ntrue: b\nyes: c\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := gityaml.Objects([]byte(in), 10); err != nil {
				t.Fatalf("Objects = %v, want no error", err)
			}
		})
	}
}

// Real manifests pass: the CRDs of this repository, well under the bound, and a ConfigMap of MaxMappingKeys
// entries (#608). A CRD may hold up to half the bound in one mapping, so that normal API growth does not fail
// the test; the widest held 21 keys in November 2026.
func TestObjectsRealManifests(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "config", "crd", "bases", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs found: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f) // #nosec G304 -- a CRD of this repository.
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gityaml.Objects(data, 10); err != nil {
			t.Errorf("%s: Objects = %v, want no error", f, err)
		}
		if w, _ := widestMapping(string(data)); w > gityaml.MaxMappingKeys/2 {
			t.Errorf("%s: a mapping of %d keys, more than half of MaxMappingKeys", f, w)
		}
	}
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: dashboards\n  namespace: monitoring\ndata:\n")
	for i := range gityaml.MaxMappingKeys {
		fmt.Fprintf(&b, "  dashboard-%04d.json: |\n    {\"title\": \"Dashboard %d\", \"panels\": []}\n", i, i)
	}
	if _, err := gityaml.Objects([]byte(b.String()), 10); err != nil {
		t.Errorf("a ConfigMap of %d entries: Objects = %v, want no error", gityaml.MaxMappingKeys, err)
	}
}

// FuzzMappingKeys checks that the check refuses input it accepts parsing for a mapping of more than
// MaxMappingKeys keys only when the tree holds one, and always when it does; and for a duplicate key only when a
// mapping may hold one key twice, and always when one surely does (#608).
func FuzzMappingKeys(f *testing.F) {
	for _, in := range nodeCases {
		f.Add(in)
	}
	f.Add(flowKeys(gityaml.MaxMappingKeys))
	f.Add(flowKeys(gityaml.MaxMappingKeys + 1))
	f.Add("a: b\n---\n" + flowKeys(gityaml.MaxMappingKeys+1))
	f.Add("{" + strings.Repeat("k: ,", gityaml.MaxMappingKeys) + "k: }\n")
	f.Add(strings.Repeat("? k\n", gityaml.MaxMappingKeys+1))
	f.Add("0x1: a\n1: b\n")
	f.Add("~: a\nnull: b\n")
	f.Add("1.0: a\n'1': b\n")
	f.Add("a: {k: 1, 'k': 2}\n")
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 1<<16 || !utf8.ValidString(in) {
			return
		}
		// Per part, as the check decodes: kustomize splits at a separator line even inside a block scalar.
		w, ok, exact, loose := 0, true, false, false
		for _, p := range gityaml.Parts([]byte(in)) {
			pw, pok := widestMapping(p.Text)
			pe, pl, _ := duplicateKeys(p.Text)
			w, ok, exact, loose = max(w, pw), ok && pok, exact || pe, loose || pl
		}
		_, err := gityaml.Objects([]byte(in), 1<<20)
		if errors.Is(err, gityaml.ErrTooManyKeys) && w <= gityaml.MaxMappingKeys {
			t.Fatalf("Objects(%q) refused a widest mapping of %d keys", in, w)
		}
		if ok && w > gityaml.MaxMappingKeys && err == nil {
			t.Fatalf("Objects(%q) accepted a mapping of %d keys", in, w)
		}
		if errors.Is(err, gityaml.ErrDuplicateKey) && !loose {
			t.Fatalf("Objects(%q) refused a duplicate key in mappings without one", in)
		}
		if ok && exact && err == nil {
			t.Fatalf("Objects(%q) accepted a mapping that holds one key twice", in)
		}
	})
}
