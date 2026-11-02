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

// Command yamlpatch-spike patches one field of testdata/sample.yaml with four approaches and writes each
// result to out/, for ADR-0020. It is a throwaway comparison, not shipped code: its own module keeps the
// candidate libraries out of the operator's go.mod.
//
// The change: in the Deployment, container "web", resources.limits.memory 512Mi -> 768Mi.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	goccy "github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	yamlv3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/kustomize/kyaml/kio"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"
	k8syaml "sigs.k8s.io/yaml"
)

const (
	oldValue = "512Mi"
	newValue = "768Mi"
)

func main() {
	for _, sample := range []string{"sample", "anchor"} {
		run(sample)
	}
}

// run patches testdata/<sample>.yaml with every approach. In "anchor" the target sits inside an anchored
// map that the sidecar reuses through an alias, so a correct patcher must refuse.
func run(sample string) {
	src, err := os.ReadFile(filepath.Join("testdata", sample+".yaml"))
	if err != nil {
		fail(err)
	}
	fmt.Println(sample + ":")
	if err := os.MkdirAll("out", 0o755); err != nil {
		fail(err)
	}
	for name, patch := range map[string]func([]byte) ([]byte, error){
		"typed":  typedRoundTrip,
		"kyaml":  kyamlPatch,
		"goccy":  goccyPatch,
		"splice": splice,
	} {
		out, err := patch(src)
		if err != nil {
			fmt.Printf("  %-6s refused: %v\n", name, err)
			continue
		}
		if err := os.WriteFile(filepath.Join("out", sample+"-"+name+".yaml"), out, 0o644); err != nil {
			fail(err)
		}
		ok, err := onlyTargetChanged(src, out)
		if err != nil {
			fmt.Printf("  %-6s patched, output does not decode: %v\n", name, err)
			continue
		}
		fmt.Printf("  %-6s patched, only the target changed: %v\n", name, ok)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// typedRoundTrip decodes each document into generic data, sets the field and encodes again
// (sigs.k8s.io/yaml, the way most Kubernetes code reads manifests).
func typedRoundTrip(src []byte) ([]byte, error) {
	var out [][]byte
	// Split at "---" lines and keep each document's final newline, so a literal block keeps its content.
	for _, doc := range docSeparator.Split(string(src), -1) {
		var obj map[string]any
		if err := k8syaml.Unmarshal([]byte(doc), &obj); err != nil {
			return nil, err
		}
		if obj["kind"] == "Deployment" {
			c := web(obj)
			if c == nil {
				return nil, errors.New("container web not found")
			}
			limits := c["resources"].(map[string]any)["limits"].(map[string]any)
			if limits["memory"] != oldValue {
				return nil, errors.New("previous value differs")
			}
			limits["memory"] = newValue
		}
		b, err := k8syaml.Marshal(obj)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return bytes.Join(out, []byte("---\n")), nil
}

// docSeparator matches a document separator line.
var docSeparator = regexp.MustCompile(`(?m)^---\n`)

func web(obj map[string]any) map[string]any {
	spec := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	for _, c := range spec["containers"].([]any) {
		if m := c.(map[string]any); m["name"] == "web" {
			return m
		}
	}
	return nil
}

// kyamlPatch uses Kustomize's kyaml: parse into a node tree that keeps comments, look up the field with a
// path filter, set it, write the tree back.
func kyamlPatch(src []byte) ([]byte, error) {
	r := &kio.ByteReader{Reader: bytes.NewReader(src), PreserveSeqIndent: true}
	nodes, err := r.Read()
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.GetKind() != "Deployment" {
			continue
		}
		f, err := n.Pipe(kyaml.Lookup("spec", "template", "spec", "containers", "[name=web]", "resources", "limits", "memory"))
		if err != nil || f == nil {
			return nil, fmt.Errorf("lookup: %v", err)
		}
		if f.YNode().Value != oldValue {
			return nil, errors.New("previous value differs")
		}
		f.YNode().Value = newValue
	}
	var buf bytes.Buffer
	if err := (&kio.ByteWriter{Writer: &buf}).Write(nodes); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// goccyPatch uses goccy/go-yaml's AST, which keeps comments and token positions, and replaces the value
// node at a YAML path. Its paths have no filter by name, so the container index is looked up first.
func goccyPatch(src []byte) ([]byte, error) {
	file, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	for _, doc := range file.Docs {
		var meta struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string `yaml:"name"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := goccy.NodeToValue(doc.Body, &meta); err != nil {
			return nil, err
		}
		if meta.Kind != "Deployment" {
			continue
		}
		idx := -1
		for i, c := range meta.Spec.Template.Spec.Containers {
			if c.Name == "web" {
				idx = i
			}
		}
		p, err := goccy.PathString(fmt.Sprintf("$.spec.template.spec.containers[%d].resources.limits.memory", idx))
		if err != nil {
			return nil, err
		}
		var cur string
		if err := p.Read(strings.NewReader(doc.String()), &cur); err != nil || cur != oldValue {
			return nil, fmt.Errorf("previous value differs: %q %v", cur, err)
		}
		// A File holding only this document; it shares the node, so the replacement lands in file.
		if err := p.ReplaceWithReader(&ast.File{Docs: []*ast.DocumentNode{doc}}, strings.NewReader(`"`+newValue+`"`)); err != nil {
			return nil, err
		}
	}
	// String already ends with a newline.
	return []byte(file.String()), nil
}

// splice finds the scalar with a node parser (go.yaml.in/yaml/v3, the maintained yaml.v3) and replaces
// only its bytes in the original file. Everything else stays byte for byte; the quote style is kept.
func splice(src []byte) ([]byte, error) {
	dec := yamlv3.NewDecoder(bytes.NewReader(src))
	for {
		var doc yamlv3.Node
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			return nil, errors.New("target not found")
		} else if err != nil {
			return nil, err
		}
		root := doc.Content[0]
		if child(root, "kind") == nil || child(root, "kind").Value != "Deployment" {
			continue
		}
		var c *yamlv3.Node
		for _, item := range path(root, "spec", "template", "spec", "containers").Content {
			if n := child(item, "name"); n != nil && n.Value == "web" {
				c = item
			}
		}
		for _, a := range []*yamlv3.Node{root, c, path(c, "resources"), path(c, "resources", "limits")} {
			if a != nil && a.Anchor != "" {
				return nil, errors.New("target is inside an anchored node, so an edit would change its aliases too")
			}
		}
		n := path(c, "resources", "limits", "memory")
		if n == nil || n.Kind != yamlv3.ScalarNode || n.Value != oldValue {
			return nil, errors.New("previous value differs or is not a plain scalar")
		}
		start := offset(src, n.Line, n.Column)
		quote := ""
		switch n.Style {
		case yamlv3.DoubleQuotedStyle:
			quote = `"`
		case yamlv3.SingleQuotedStyle:
			quote = `'`
		case 0:
		default:
			return nil, errors.New("block or flow scalar style not supported")
		}
		token := quote + n.Value + quote
		if !bytes.HasPrefix(src[start:], []byte(token)) {
			return nil, errors.New("token at position does not match the value")
		}
		out := append([]byte{}, src[:start]...)
		out = append(out, quote+newValue+quote...)
		return append(out, src[start+len(token):]...), nil
	}
}

func child(m *yamlv3.Node, key string) *yamlv3.Node {
	if m == nil || m.Kind != yamlv3.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func path(n *yamlv3.Node, keys ...string) *yamlv3.Node {
	for _, k := range keys {
		n = child(n, k)
	}
	return n
}

// offset turns a 1-based line and column into a byte offset.
func offset(src []byte, line, col int) int {
	off := 0
	for l := 1; l < line; l++ {
		off += bytes.IndexByte(src[off:], '\n') + 1
	}
	return off + col - 1
}

// onlyTargetChanged decodes both files and reports whether the data differs only in the target field. A file
// that does not decode completely is an error, never a match of the documents read so far.
func onlyTargetChanged(before, after []byte) (bool, error) {
	a, err := decodeAll(before)
	if err != nil {
		return false, err
	}
	b, err := decodeAll(after)
	if err != nil {
		return false, err
	}
	if len(a) != len(b) {
		return false, nil
	}
	for _, d := range a {
		if d["kind"] == "Deployment" {
			web(d)["resources"].(map[string]any)["limits"].(map[string]any)["memory"] = newValue
		}
	}
	return reflect.DeepEqual(a, b), nil
}

func decodeAll(src []byte) ([]map[string]any, error) {
	var out []map[string]any
	dec := yamlv3.NewDecoder(bytes.NewReader(src))
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
}
