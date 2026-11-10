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

// Package gityaml is the fail-closed check every YAML file read from Git passes before a parser that expands
// aliases decodes it (invariant 4: Git content is untrusted input). Both readers of a build use it: the
// semantic diff (internal/gitops/semdiff) before kustomize renders, and the source mapping
// (internal/gitops/sourcemap) before sigs.k8s.io/yaml decodes. Those parsers replace each alias by a copy, so
// a file of a few kilobytes could expand into hundreds of megabytes, or without end through an anchor that
// contains itself (#488, #492, #594).
package gityaml

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
var documentSeparator = regexp.MustCompile(`\n---(.*)\n`)

// yamlLine finds the line number in a YAML parser error.
var yamlLine = regexp.MustCompile(`line (\d+)`)

// Causes of an [Error]. Their text follows the name of the file in a refusal's detail, so it holds no text
// from the file.
var (
	// ErrNotUTF8 is input that is not UTF-8.
	ErrNotUTF8 = errors.New("is not UTF-8")
	// ErrAnchor is a YAML anchor, alias or merge key.
	ErrAnchor = errors.New("YAML anchors, aliases and merge keys are not supported")
	// ErrSeparator is a document separator line that holds more than a comment after "---".
	ErrSeparator = errors.New("a document separator holds more than a comment")
	// ErrNotParse is a part that does not parse as YAML.
	ErrNotParse = errors.New("does not parse as YAML")
	// ErrLineBreak is a line break other than LF or CRLF: a bare CR, NEL, LS or PS.
	ErrLineBreak = errors.New("a line ends in a line break other than LF or CRLF")
	// ErrDirective is a YAML directive, a line that starts with "%".
	ErrDirective = errors.New("YAML directives (lines starting with %) are not supported")
	// ErrTooManyNodes is input of more than MaxNodes YAML nodes, as [Nodes] counts them.
	ErrTooManyNodes = fmt.Errorf("holds more than %d YAML nodes", MaxNodes)
	// ErrTooManyKeys is a mapping of more than MaxMappingKeys keys.
	ErrTooManyKeys = fmt.Errorf("a mapping holds more than %d keys", MaxMappingKeys)
	// ErrDuplicateKey is a scalar key that a mapping already holds.
	ErrDuplicateKey = errors.New("a mapping holds a key twice")
)

// MaxMappingKeys is the most keys [Objects] accepts in one YAML mapping. The parsers that decode a file after
// the check into Go values, go.yaml.in/yaml/v3 in the patcher and kustomize's fork of it in the render, look
// for a duplicate key by comparing each key with every key after it in the same mapping, so the time is
// quadratic in the keys of one mapping: a mapping of 43,000 keys, within MaxNodes, took 4.3 s in the patcher's
// decode and 6.3 s in a render, about ten times that under the race detector (#608). The widest mapping in
// about 500 real manifests over 20 KB, CRDs included, held 76 keys, so 256 leaves room for real files and keeps
// the comparisons of one mapping under 33,000. Duplicate keys are refused before any decode too
// ([ErrDuplicateKey]): the decoders build an error text, holding the key, for every pair of equal keys, so 2,048
// copies of one long key took seconds and gigabytes of allocations.
const MaxMappingKeys = 256

// Error is a refusal of [Objects]: its cause and the line of the file it is at, counted from 1, or 0 when it
// is about the whole file.
type Error struct {
	Line int
	Err  error
}

// Error returns the cause, after its line when it has one, written to follow the name of the file.
func (e *Error) Error() string {
	if e.Line == 0 {
		return e.Err.Error()
	}
	return fmt.Sprintf("line %d: %v", e.Line, e.Err)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// Part is the text between two document separators, as kustomize splits a file.
type Part struct {
	// Text is the part, with "\r\n" replaced by "\n".
	Text string
	// Line is the line of the file the part starts at, counted from 1.
	Line int
	// sep is what follows "---" on the separator line that ends the part; empty for the last part.
	sep string
}

// Parts splits data as kustomize does (see documentSeparator). The last part is empty when data ends with a
// separator.
func Parts(data []byte) []Part {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	var parts []Part
	prev, line := 0, 1
	for _, loc := range documentSeparator.FindAllStringSubmatchIndex(s, -1) {
		parts = append(parts, Part{Text: s[prev:loc[0]], Line: line, sep: s[loc[2]:loc[3]]})
		line += strings.Count(s[prev:loc[1]], "\n")
		prev = loc[1]
	}
	return append(parts, Part{Text: s[prev:], Line: line})
}

// Objects returns an upper bound of the objects kustomize processes from data, a whole file or one document,
// at most limit+1 (the count stops there). Every YAML file read from Git goes through it before a parser that
// expands aliases decodes it.
//
// It fails closed instead of mimicking a parser's YAML handling, so that no input expands past what was
// checked (#488, #492, #594). It refuses, with an [*Error]:
//   - input that is not UTF-8: kustomize and sigs.k8s.io/yaml also read UTF-16 and UTF-32 with a byte order
//     mark, which a check of the raw bytes misses;
//   - every YAML anchor, alias and merge key (a "<<" key or a key with the !!merge tag, however spelled; a
//     "<<" value is plain text): parsers replace aliases by copies and resolve merges before anything looks at
//     the result, so a few bytes could expand into thousands of objects or hundreds of megabytes, or without
//     end through an anchor that contains itself, which overflowed the Go stack and ended the process;
//   - a separator line with more than a comment after "---": kustomize drops that text, but a parser given
//     the whole file reads it as the start of the next document, so an anchor there would not be checked; a
//     comment needs a space or a tab before its "#", since "---#x: ..." is a key holding what follows;
//   - a line break other than LF or CRLF (a bare CR, NEL, LS or PS): kustomize splits at LF only, while the
//     parsers end a line at each of them, so the text after one on a separator line would be read unchecked;
//   - a YAML directive (a line that starts with "%"): kustomize decodes the part before the first separator
//     on its own, where a directive does not parse, while a parser of the whole file accepts it;
//   - input of more than [MaxNodes] YAML nodes as [Nodes] counts them, before any part is decoded: a parser
//     builds the whole tree of a document before anything looks at it, so a dense document of 1 MiB would
//     take more heap than the executor has (#595);
//   - a mapping of more than [MaxMappingKeys] keys, or one that holds a scalar key twice, found in the node
//     tree of each part before anything decodes it into Go values: the duplicate key check of those decoders
//     is quadratic in the keys of one mapping and builds an error for every pair of equal keys, so a mapping
//     of tens of thousands of keys, or of a few thousand copies of one long key, held the CPU for seconds
//     (#608); the decoders refuse a duplicate key anyway;
//   - a part that does not parse, which kustomize could not build either.
//
// Each part between separators is decoded as kustomize decodes it, and every document in it counts: 1 for a
// document that is not a List, empty ones too, and 1 plus its items, each counted the same way, for a List
// ([listObjects]).
// Once the count is over limit the rest of the input is not read, nor checked: the caller must refuse it.
func Objects(data []byte, limit int) (int, error) {
	if !utf8.Valid(data) {
		return 0, &Error{Err: ErrNotUTF8}
	}
	if err := checkLines(string(data)); err != nil {
		return 0, err
	}
	// Before any part is decoded, so no tree of more than MaxNodes nodes is ever built (#595).
	if Nodes(data, MaxNodes) > MaxNodes {
		return 0, &Error{Err: ErrTooManyNodes}
	}
	n := 0
	parts := Parts(data)
	for i, p := range parts {
		c, err := objectsIn(p.Text, limit)
		if err != nil {
			return 0, &Error{Line: p.Line + err.Line - 1, Err: err.Err}
		}
		if n += c; n > limit {
			break
		}
		if !commentOnly(p.sep) {
			return 0, &Error{Line: parts[i+1].Line - 1, Err: ErrSeparator}
		}
	}
	return min(n, limit+1), nil
}

// checkLines refuses a line break other than LF or CRLF, and a line that starts with "%" (a directive), in s,
// which is UTF-8. It reads the whole of s.
func checkLines(s string) *Error {
	line := 1
	for i, r := range s {
		switch r {
		case '\n':
			line++
		case '\r':
			if !strings.HasPrefix(s[i+1:], "\n") {
				return &Error{Line: line, Err: ErrLineBreak}
			}
		case '\u0085', ' ', ' ':
			return &Error{Line: line, Err: ErrLineBreak}
		case '%':
			if i == 0 || s[i-1] == '\n' {
				return &Error{Line: line, Err: ErrDirective}
			}
		}
	}
	return nil
}

// commentOnly reports whether sep, what follows "---" on a separator line, is empty, blank, or a comment
// after a space or a tab. Only a space or a tab separates "---" from a comment for the YAML parsers.
func commentOnly(sep string) bool {
	if sep == "" {
		return true
	}
	if sep[0] != ' ' && sep[0] != '\t' {
		return false
	}
	s := strings.TrimLeft(sep, " \t")
	return s == "" || s[0] == '#'
}

// keyKind is the key that names an object's kind.
const keyKind = "kind"

// objectsIn returns an upper bound of the objects in one part of a file, at most limit+1: every document it
// holds counts, an empty part as 1. The line of an error is counted from the start of the part.
func objectsIn(part string, limit int) (int, *Error) {
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
			return 0, &Error{Line: l, Err: ErrNotParse}
		}
		if a, cause := refused(&doc); a != nil {
			return 0, &Error{Line: a.Line, Err: cause}
		}
		n = min(n+listObjects(&doc, limit), limit+1)
	}
	return max(n, 1), nil
}

// refused returns the first node below n that the check refuses, in document order, and the cause, or nil:
// a node that has an anchor, is an alias, or is a merge key (a mapping key with the merge tag, which a plain
// "<<" has too, or a "<<" key however quoted), with [ErrAnchor]; a mapping of more than [MaxMappingKeys] keys
// with [ErrTooManyKeys]; a scalar key whose text an earlier key of the same mapping has, with
// [ErrDuplicateKey]. Like the decoders, it compares the text of the key, not its tag or quoting. A "<<" value is
// plain text: only a key merges.
func refused(n *yaml.Node) (*yaml.Node, error) {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return n, ErrAnchor
	}
	var keys map[string]struct{}
	if n.Kind == yaml.MappingNode {
		if len(n.Content) > 2*MaxMappingKeys {
			return n, ErrTooManyKeys
		}
		keys = make(map[string]struct{}, len(n.Content)/2)
	}
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			if c.ShortTag() == "!!merge" || c.Kind == yaml.ScalarNode && c.Value == "<<" {
				return c, ErrAnchor
			}
			if c.Kind == yaml.ScalarNode {
				if _, dup := keys[c.Value]; dup {
					return c, ErrDuplicateKey
				}
				keys[c.Value] = struct{}{}
			}
		}
		if a, cause := refused(c); a != nil {
			return a, cause
		}
	}
	return nil, nil
}

// listObjects counts the objects kustomize may make of n, which holds no alias (kustomize api
// resource.Factory.inlineAnyEmbeddedLists and kio.ByteReader): 1 for any node, plus, for a mapping that may be
// a List, its items, each counted the same way. It errs high: a List counts itself besides its items, a mapping
// is a List when any "kind" key is not a plain string or ends in "List", all its "items" keys count, and items
// given as a mapping (which kio.ByteReader unwraps key by key) count 2 per entry besides what each value holds.
// The result stops at limit+1.
func listObjects(n *yaml.Node, limit int) int {
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
				c = min(c+listObjects(e, limit), limit+1)
			}
		case yaml.MappingNode:
			for i := 1; i < len(it.Content); i += 2 {
				c = min(c+1+listObjects(it.Content[i], limit), limit+1)
			}
		default:
			c = min(c+1, limit+1)
		}
	}
	return c
}
