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

package gityaml

import (
	"bytes"
	"strings"
)

// MaxNodes is the most YAML nodes (documents, collections and scalars) [Objects] accepts in one input, as
// counted by [Nodes]. A YAML parser builds a tree of every document before anything looks at it, about 170
// bytes of heap per node, and a file of 1 MiB can hold about a million nodes, so the count is the bound on
// that tree (#595). Real manifests, CRDs included, take 37 to 64 bytes per node, so a file of 1 MiB of them
// stays under it.
const MaxNodes = 1 << 17

// bom is the UTF-8 byte order mark, which the parser skips at the start of a line.
const bom = "\ufeff"

// boundary holds the bytes after which a node may start without a space: the flow indicators, the key,
// value and block entry indicators, and the closing quotes.
const boundary = "[]{},?:-\"'"

// Nodes returns an upper bound of the nodes go.yaml.in/yaml/v3 builds for data, documents included, at most
// limit+1 (the count stops there). It reads the bytes alone, without parsing them, so it can be used before
// any tree is built, and it errs high: text inside scalars and comments counts too.
//
// Each node the parser builds comes from a token, an empty value, an implicit document or a collection that
// an indicator opens, and the count gives each of them a byte that starts or causes it:
//   - each document 3 (the document, its empty content, and an empty value the parser makes before it
//     reports an error), once for the input and once per line that starts with "---" or "..." (after blanks
//     or a byte order mark too);
//   - each byte where a token other than an indicator may start 1: the first byte of a line after blanks
//     (and after one byte order mark), a byte after one of boundary, and a byte after blanks whose run of
//     non-blank bytes before qualifies (see below). A plain scalar ends only at an indicator, a comment or a
//     line break, so the next token after one and blanks is an indicator, counted below;
//   - each "[" and "{" 1 (the collection), wherever it is;
//   - each ":" 2 (the mapping its key opens and an empty value), wherever it is; a ":" with no key before it
//     is a parse error in go.yaml.in/yaml/v3, so it makes no empty key;
//   - each "?" 3 (the mapping it opens, an empty key and an empty value), wherever it is;
//   - each "-" followed by a blank or a line break 2 (a sequence and an empty item);
//   - each "," and "}" 1 (the empty value of a flow mapping entry with no ":").
//
// A run of non-blank bytes qualifies when it ends in one of boundary (an indicator or a quoted scalar), when
// a token may start in it with "&", "*", "!", "|" or ">" (an anchor, alias, tag or block scalar header,
// which a blank and a node may follow), or when it is a "---" or "..." at the start of a line.
func Nodes(data []byte, limit int) int {
	n := 3
	var (
		lineStart = true  // only blanks (and one byte order mark) since the start of the line
		inRun     = false // in a run of non-blank bytes
		runOK     = false // the current run qualifies
		lastOK    = false // the last run on this line qualified
	)
	for i := 0; i < len(data) && n <= limit; i++ {
		c := data[i]
		switch {
		case c == '\n':
			lineStart, inRun, lastOK = true, false, false
			continue
		case isBlank(c):
			if inRun {
				lastOK = runOK || strings.IndexByte(boundary, data[i-1]) >= 0
				inRun = false
			}
			continue
		case lineStart && (i == 0 || data[i-1] == '\n') && bytes.HasPrefix(data[i:], []byte(bom)):
			// The parser skips a byte order mark at the start of a line; the line still starts after it.
			i += len(bom) - 1
			continue
		}
		var start bool
		if inRun {
			start = strings.IndexByte(boundary, data[i-1]) >= 0
		} else {
			start, runOK, inRun = lineStart || lastOK, false, true
			// A document marker counts wherever a line starts with it, after blanks or a byte order mark too.
			if lineStart && (bytes.HasPrefix(data[i:], []byte("---")) || bytes.HasPrefix(data[i:], []byte("..."))) {
				n += 3
				runOK = true
			}
		}
		lineStart = false
		blankNext := i+1 == len(data) || isBlank(data[i+1])
		if start && startsNode(c, blankNext) {
			n++
			runOK = runOK || strings.IndexByte("&*!|>", c) >= 0
		}
		n += indicator(c, blankNext)
	}
	return min(n, limit+1)
}

// startsNode reports whether a token that is not an indicator may start with c, followed by a blank or a line
// break when blankNext: not a "," "]" "}" (indicators only), "#" (a comment), "[" "{" (counted by
// [indicator]), nor a "-" "?" ":" before a blank (an indicator then).
func startsNode(c byte, blankNext bool) bool {
	switch {
	case strings.IndexByte(",]}#[{", c) >= 0:
		return false
	case strings.IndexByte("-?:", c) >= 0 && blankNext:
		return false
	}
	return true
}

// indicator returns the nodes c may open or leave empty as an indicator, wherever it is (see [Nodes]).
func indicator(c byte, blankNext bool) int {
	switch c {
	case '[', '{', ',', '}':
		return 1
	case ':':
		return 2
	case '?':
		return 3
	case '-':
		if blankNext {
			return 2
		}
	}
	return 0
}

// isBlank reports whether c ends a run of non-blank bytes: a space, a tab, a line break or another control
// byte.
func isBlank(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c < 0x20
}
