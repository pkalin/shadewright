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
	"bytes"
	"errors"
	"regexp"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// documentSeparator is kustomize's own document separator (kio.splitDocuments, kyaml v0.21): a line that starts
// with "---". Each match starts with "\n---" and matches do not overlap, so a file holds at most one document
// more than it has "\n---" sequences.
var documentSeparator = regexp.MustCompile(`\n---.*\n`)

// errSelfAlias is returned by [documents] for a document with an anchor that contains an alias to itself. The
// YAML parser accepts it, but kustomize, which replaces every alias by a copy of its anchor before anything else,
// then recurses until the Go stack overflows, which ends the process; recover cannot stop it.
var errSelfAlias = errors.New("a YAML anchor contains itself")

// documents returns an upper bound of the objects kustomize reads from one manifest or patch file, or from an
// inline patch, at most MaxBuildDocuments+1 (the count stops there). Without a List it is the number of YAML
// documents, counted from the separators alone: empty documents count too, and nothing is decoded.
//
// kustomize replaces a document whose kind ends in "List" and that has items by its items, recursively, after
// resolving anchors and merge keys, so one document can hold many objects. A document that could be such a
// List is decoded, one at a time, and its items counted with every alias reference ([listObjects]). The text
// filter is safe: such a document names "items" and a kind ending in "List" in its text, unless it spells them
// with an escape in a double-quoted scalar, which needs a backslash.
//
// A document that holds both an anchor and an alias ("&" and "*", which have no escaped form) is decoded too,
// and refused with errSelfAlias when an anchor contains itself.
func documents(data []byte) (int, error) {
	if !mustDecode(data) {
		return min(1+bytes.Count(data, []byte("\n---")), MaxBuildDocuments+1), nil
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	n, prev := 0, 0
	parts := append(documentSeparator.FindAllStringIndex(s, -1), []int{len(s), len(s)})
	for _, loc := range parts {
		c, err := objectsIn(s[prev:loc[0]])
		if err != nil {
			return 0, err
		}
		if n += c; n > MaxBuildDocuments {
			break
		}
		prev = loc[1]
	}
	return min(n, MaxBuildDocuments+1), nil
}

// mustDecode says whether text may hold a List kustomize expands or an anchor that contains itself: see
// [documents].
func mustDecode[T string | []byte](text T) bool {
	s := string(text)
	return strings.Contains(s, `\`) || strings.Contains(s, "items") && strings.Contains(s, "List") ||
		strings.Contains(s, "&") && strings.Contains(s, "*")
}

// objectsIn returns an upper bound of the objects one document holds: 1 unless it may be a List.
func objectsIn(doc string) (int, error) {
	if !mustDecode(doc) {
		return 1, nil
	}
	var root yaml.Node
	if yaml.Unmarshal([]byte(doc), &root) != nil {
		// kustomize, which parses with the same library, refuses the document, so the render fails.
		return 1, nil
	}
	if selfAlias(&root, map[*yaml.Node]bool{}, map[*yaml.Node]bool{}) {
		return 0, errSelfAlias
	}
	return listObjects(&root, map[*yaml.Node]int{}), nil
}

// selfAlias reports whether an alias below n leads back to a node it is inside of. open holds the nodes on the
// current path, done those already searched.
func selfAlias(n *yaml.Node, open, done map[*yaml.Node]bool) bool {
	if n == nil || done[n] {
		return false
	}
	if open[n] {
		return true
	}
	open[n] = true
	if n.Kind == yaml.AliasNode && selfAlias(n.Alias, open, done) {
		return true
	}
	for _, c := range n.Content {
		if selfAlias(c, open, done) {
			return true
		}
	}
	delete(open, n)
	done[n] = true
	return false
}

// listObjects counts the objects kustomize makes of n (kustomize api resource.Factory.inlineAnyEmbeddedLists
// and kio.ByteReader): a mapping whose kind may end in "List" and that has items is replaced by them, each
// counted the same way; every other node is one object. It errs high: a kind that is not a plain string, or
// items given as a mapping (which kio.ByteReader unwraps key by key), count as a List. memo holds the count of
// each mapping already counted, so a node reached through many aliases is counted once and its count added
// per reference, and a node that contains itself counts as one. The result stops at MaxBuildDocuments+1.
func listObjects(n *yaml.Node, memo map[*yaml.Node]int) int {
	n = resolve(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return 1
	}
	if c, ok := memo[n]; ok {
		return c
	}
	memo[n] = 1
	kind := field(n, "kind", map[*yaml.Node]bool{})
	if kind == nil || kind.Kind == yaml.ScalarNode && !strings.HasSuffix(kind.Value, "List") {
		return 1
	}
	items := field(n, "items", map[*yaml.Node]bool{})
	c := 1
	switch {
	case items == nil:
	case items.Kind == yaml.SequenceNode:
		c = 0
		for _, it := range items.Content {
			c = min(c+listObjects(it, memo), MaxBuildDocuments+1)
		}
	case items.Kind == yaml.MappingNode:
		c = 0
		for i := 1; i < len(items.Content); i += 2 {
			c = min(c+1+listObjects(items.Content[i], memo), MaxBuildDocuments+1)
		}
	}
	memo[n] = c
	return c
}

// resolve follows aliases and document nodes to the node they stand for. An anchor cannot be set on an alias,
// so a chain is short; one of more than 100 steps gives nil, as a guard.
func resolve(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && i < 100; i++ {
		switch {
		case n.Kind == yaml.AliasNode:
			n = n.Alias
		case n.Kind == yaml.DocumentNode && len(n.Content) == 1:
			n = n.Content[0]
		default:
			return n
		}
	}
	return nil
}

// field returns the resolved value of key in mapping m, its own or merged in through a "<<" key, or nil.
// seen holds the mappings already searched, so merges that refer to each other end.
func field(m *yaml.Node, key string, seen map[*yaml.Node]bool) *yaml.Node {
	m = resolve(m)
	if m == nil || m.Kind != yaml.MappingNode || seen[m] {
		return nil
	}
	seen[m] = true
	var merges []*yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := resolve(m.Content[i])
		if k == nil || k.Kind != yaml.ScalarNode {
			continue
		}
		if k.Value == key {
			return resolve(m.Content[i+1])
		}
		if k.Value == "<<" {
			merges = append(merges, m.Content[i+1])
		}
	}
	for _, v := range merges {
		v = resolve(v)
		if v == nil {
			continue
		}
		srcs := []*yaml.Node{v}
		if v.Kind == yaml.SequenceNode {
			srcs = v.Content
		}
		for _, s := range srcs {
			if f := field(s, key, seen); f != nil {
				return f
			}
		}
	}
	return nil
}
