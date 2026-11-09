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
	"os"
	"path/filepath"
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

// blockKeys returns a ConfigMap whose data holds n keys without values, one per line.
func blockKeys(n int) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: big\ndata:\n")
	for i := range n {
		fmt.Fprintf(&b, "  k%d:\n", i)
	}
	return b.String()
}

// flowKeys returns a flow mapping of n keys without values, the same key repeated (a duplicate key is still a
// key of the tree).
func flowKeys(n int) string {
	return "a: [{" + strings.Repeat("k,", n-1) + "k}]\n"
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

// Real manifests are far from the bound: the CRDs of this repository and a ConfigMap of 1,000 entries pass
// the check (#608).
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
		if w, _ := widestMapping(string(data)); w > gityaml.MaxMappingKeys/8 {
			t.Errorf("%s: a mapping of %d keys, closer to the bound than expected", f, w)
		}
	}
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: dashboards\n  namespace: monitoring\ndata:\n")
	for i := range 1000 {
		fmt.Fprintf(&b, "  dashboard-%04d.json: |\n    {\"title\": \"Dashboard %d\", \"panels\": []}\n", i, i)
	}
	if _, err := gityaml.Objects([]byte(b.String()), 10); err != nil {
		t.Errorf("a ConfigMap of 1,000 entries: Objects = %v, want no error", err)
	}
}

// FuzzMappingKeys checks that the check refuses input it accepts parsing only for a mapping of more than
// MaxMappingKeys keys when the tree holds one, and always when it does (#608).
func FuzzMappingKeys(f *testing.F) {
	for _, in := range nodeCases {
		f.Add(in)
	}
	f.Add(flowKeys(gityaml.MaxMappingKeys))
	f.Add(flowKeys(gityaml.MaxMappingKeys + 1))
	f.Add("a: b\n---\n" + flowKeys(gityaml.MaxMappingKeys+1))
	f.Add("{" + strings.Repeat("k: ,", gityaml.MaxMappingKeys) + "k: }\n")
	f.Add(strings.Repeat("? k\n", gityaml.MaxMappingKeys+1))
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 1<<16 || !utf8.ValidString(in) {
			return
		}
		// Per part, as the check decodes: kustomize splits at a separator line even inside a block scalar.
		w, ok := 0, true
		for _, p := range gityaml.Parts([]byte(in)) {
			pw, pok := widestMapping(p.Text)
			w, ok = max(w, pw), ok && pok
		}
		_, err := gityaml.Objects([]byte(in), 1<<20)
		if errors.Is(err, gityaml.ErrTooManyKeys) && w <= gityaml.MaxMappingKeys {
			t.Fatalf("Objects(%q) refused a widest mapping of %d keys", in, w)
		}
		if ok && w > gityaml.MaxMappingKeys && err == nil {
			t.Fatalf("Objects(%q) accepted a mapping of %d keys", in, w)
		}
	})
}
