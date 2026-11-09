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
	"testing"

	"github.com/pkalin/shadewright/internal/gitops/gityaml"
)

// The count, the refusals and their lines; semdiff's tests cover the count against kustomize in depth.
func TestObjects(t *testing.T) {
	cases := map[string]struct {
		in   string
		n    int
		err  error
		line int
	}{
		"one document":                   {"a: 1\n", 1, nil, 0},
		"an empty file":                  {"", 1, nil, 0},
		"two parts":                      {"a: 1\n---\nb: 2\n", 2, nil, 0},
		"a separator with a comment":     {"a: 1\n--- # next\nb: 2\n", 2, nil, 0},
		"CRLF":                           {"a: 1\r\n---\r\nb: 2\r\n", 2, nil, 0},
		"a List":                         {"kind: List\nitems: [{a: 1}, {b: 2}]\n", 3, nil, 0},
		"over the limit":                 {"a: 1\n---\nb: 2\n---\nc: 3\n---\nd: &x 4\n", 3, nil, 0},
		"not UTF-8":                      {"a: \xC3\n", 0, gityaml.ErrNotUTF8, 0},
		"an anchor":                      {"a: 1\n---\nb: &b 2\n", 0, gityaml.ErrAnchor, 3},
		"an alias to an earlier part":    {"a: &a 1\n---\nb: *a\n", 0, gityaml.ErrAnchor, 1},
		"a merge key":                    {"a: 1\n<<: {b: 2}\n", 0, gityaml.ErrAnchor, 2},
		"an anchor on a separator":       {"# c\n--- &a {b: 1}\n", 0, gityaml.ErrSeparator, 2},
		"text on a separator":            {"a: 1\n---x\nb: 2\n", 0, gityaml.ErrSeparator, 2},
		"an anchor on the first line":    {"--- &a {b: 1}\n", 0, gityaml.ErrAnchor, 1},
		"a part that does not parse":     {"a: 1\n---\nb: [\n", 0, gityaml.ErrNotParse, 3},
		"a separator in a block scalar":  {"a: |\n  x\n--- &b\n  y\n", 0, gityaml.ErrSeparator, 3},
		"a document end marker in parts": {"a: 1\n...\n--- # c\nb: 2\n", 2, nil, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			n, err := gityaml.Objects([]byte(c.in), 2)
			if c.err == nil {
				if err != nil || n != c.n {
					t.Fatalf("Objects = %d, %v; want %d, nil", n, err, c.n)
				}
				return
			}
			var e *gityaml.Error
			if !errors.As(err, &e) || !errors.Is(err, c.err) || e.Line != c.line {
				t.Fatalf("Objects = %d, %v; want %v at line %d", n, err, c.err, c.line)
			}
		})
	}
}

func TestPartsLines(t *testing.T) {
	parts := gityaml.Parts([]byte("a: 1\nb: 2\n---\nc: 3\n--- # x\n"))
	want := []struct {
		text string
		line int
	}{{"a: 1\nb: 2", 1}, {"c: 3", 4}, {"", 6}}
	if len(parts) != len(want) {
		t.Fatalf("Parts = %+v, want %d parts", parts, len(want))
	}
	for i, w := range want {
		if parts[i].Text != w.text || parts[i].Line != w.line {
			t.Errorf("part %d = %q at line %d, want %q at line %d", i, parts[i].Text, parts[i].Line, w.text, w.line)
		}
	}
}
