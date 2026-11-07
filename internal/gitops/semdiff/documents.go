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
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"
)

// documentSeparator is kustomize's own document separator (kio.splitDocuments, kyaml v0.21): a line that starts
// with "---". kustomize splits a file at each match, after replacing "\r\n" by "\n", and decodes each part
// on its own.
var documentSeparator = regexp.MustCompile(`\n---.*\n`)

// yamlLine finds the line number in a YAML parser error.
var yamlLine = regexp.MustCompile(`line (\d+)`)

// documents returns an upper bound of the objects kustomize processes from one manifest or patch file, or from
// an inline patch, at most MaxBuildDocuments+1 (the count stops there). name is the file, or what holds the
// inline patch, for the refusal's detail.
//
// It fails closed instead of mimicking kustomize's YAML handling, so that no input expands past its count
// (#488, #492). It refuses with InvalidYAML:
//   - input that is not UTF-8: kustomize also reads UTF-16 and UTF-32 with a byte order mark, which a check of
//     the raw bytes misses;
//   - every YAML anchor, alias and merge key (a "<<" key or a key with the !!merge tag, however spelled; a
//     "<<" value is plain text): kustomize replaces aliases by copies and resolves merges before it looks for Lists, so a few bytes could expand into
//     thousands of objects, or without end through an anchor that contains itself, which overflowed the Go
//     stack and ended the process;
//   - a part that does not parse, which kustomize could not build either.
//
// Each part between separators is decoded as kustomize decodes it, and every document in it counts: 1 for a
// document that is not a List, empty ones too, and 1 plus its items, each counted the same way, for a List
// ([listObjects]).
// Once the count is over MaxBuildDocuments the rest of the input is not read, nor checked: the caller must
// refuse it.
func documents(name string, data []byte) (int, error) {
	if !utf8.Valid(data) {
		return 0, refuse(ReasonInvalidYAML, name+" is not UTF-8")
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	n, prev, line := 0, 0, 1
	parts := append(documentSeparator.FindAllStringIndex(s, -1), []int{len(s), len(s)})
	for _, loc := range parts {
		c, err := objectsIn(s[prev:loc[0]])
		if err != nil {
			return 0, refuse(ReasonInvalidYAML, fmt.Sprintf("%s line %d: %v", name, line+err.line-1, err.err))
		}
		if n += c; n > MaxBuildDocuments {
			break
		}
		line += strings.Count(s[prev:loc[1]], "\n")
		prev = loc[1]
	}
	return min(n, MaxBuildDocuments+1), nil
}

// keyKind is the key that names an object's kind.
const keyKind = "kind"

// Errors of [objectsIn]; their text goes into a refusal's detail, so it holds no text from the file.
var (
	errAnchor   = errors.New("YAML anchors, aliases and merge keys are not supported")
	errNotParse = errors.New("does not parse as YAML")
)

// partError is an error of [objectsIn] at a line of the part, counted from 1.
type partError struct {
	line int
	err  error
}

// objectsIn returns an upper bound of the objects in one part of a file: every document it holds counts, an
// empty part as 1.
func objectsIn(part string) (int, *partError) {
	dec := yaml.NewDecoder(strings.NewReader(part))
	n := 0
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			l := 1
			if m := yamlLine.FindStringSubmatch(err.Error()); m != nil {
				l, _ = strconv.Atoi(m[1])
			}
			return 0, &partError{line: l, err: errNotParse}
		}
		if a := anchored(&doc); a != nil {
			return 0, &partError{line: a.Line, err: errAnchor}
		}
		n = min(n+listObjects(&doc), MaxBuildDocuments+1)
	}
	return max(n, 1), nil
}

// anchored returns the first node below n that has an anchor, is an alias, or is a merge key: a mapping key
// with the merge tag (a plain "<<" has it too) or a "<<" key however quoted, or nil. A "<<" value is plain
// text: only a key merges.
func anchored(n *yaml.Node) *yaml.Node {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return n
	}
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 && (c.ShortTag() == "!!merge" || c.Kind == yaml.ScalarNode && c.Value == "<<") {
			return c
		}
		if a := anchored(c); a != nil {
			return a
		}
	}
	return nil
}

// listObjects counts the objects kustomize may make of n, which holds no alias (kustomize api
// resource.Factory.inlineAnyEmbeddedLists and kio.ByteReader): 1 for any node, plus, for a mapping that may be
// a List, its items, each counted the same way. It errs high: a List counts itself besides its items, a mapping
// is a List when any "kind" key is not a plain string or ends in "List", all its "items" keys count, and items
// given as a mapping (which kio.ByteReader unwraps key by key) count 2 per entry besides what each value holds.
// The result stops at MaxBuildDocuments+1.
func listObjects(n *yaml.Node) int {
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return 1
	}
	list := false
	var items []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch k, v := n.Content[i], n.Content[i+1]; {
		case k.Kind != yaml.ScalarNode:
		case k.Value == keyKind:
			list = list || v.Kind != yaml.ScalarNode || strings.HasSuffix(v.Value, "List")
		case k.Value == "items":
			items = append(items, v)
		}
	}
	if !list {
		return 1
	}
	c := 1
	for _, it := range items {
		switch it.Kind {
		case yaml.SequenceNode:
			for _, e := range it.Content {
				c = min(c+listObjects(e), MaxBuildDocuments+1)
			}
		case yaml.MappingNode:
			for i := 1; i < len(it.Content); i += 2 {
				c = min(c+1+listObjects(it.Content[i]), MaxBuildDocuments+1)
			}
		default:
			c = min(c+1, MaxBuildDocuments+1)
		}
	}
	return c
}

// errCopy is the error of [refuseCopy]; its text goes into a refusal's detail.
var errCopy = errors.New("JSON patch copy operations are not supported")

// refuseCopy refuses, with UnsupportedKustomization, a patch file or inline patch that holds a JSON patch
// (RFC 6902) copy operation: a mapping, anywhere in the patch, whose "op" key is "copy" however quoted or
// escaped. Each copy can double a value, so a patch of a few hundred bytes renders gigabytes, past every
// bound the build checks before the render (#488). It looks at every mapping rather than only at the items of
// a top-level list, so how kustomize tells a JSON patch from a strategic merge patch does not matter; a
// strategic merge patch that sets a field "op" to "copy" is refused too. It decodes data as [documents] does,
// and is called after it, which has refused input that is not UTF-8, does not parse or holds an alias.
func refuseCopy(name string, data []byte) error {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	prev, line := 0, 1
	for _, loc := range append(documentSeparator.FindAllStringIndex(s, -1), []int{len(s), len(s)}) {
		dec := yaml.NewDecoder(strings.NewReader(s[prev:loc[0]]))
		for {
			var doc yaml.Node
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return refuse(ReasonInvalidYAML, fmt.Sprintf("%s line %d: %v", name, line, errNotParse))
			}
			if c := copyOp(&doc); c != nil {
				return refuse(ReasonUnsupportedKustomization, fmt.Sprintf("%s line %d: %v", name, line+c.Line-1, errCopy))
			}
		}
		line += strings.Count(s[prev:loc[1]], "\n")
		prev = loc[1]
	}
	return nil
}

// copyOp returns the first "op" key below n whose value is "copy", or nil. Aliases are not followed.
func copyOp(n *yaml.Node) *yaml.Node {
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 && i+1 < len(n.Content) && c.Kind == yaml.ScalarNode &&
			c.Value == "op" && n.Content[i+1].Kind == yaml.ScalarNode && n.Content[i+1].Value == "copy" {
			return c
		}
		if o := copyOp(c); o != nil {
			return o
		}
	}
	return nil
}
