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

package analysis

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/tools"
)

// shownBlock is one context block or tool result as the model saw it, by ID: its name (the context section or
// the tool, both fixed text from code), its text as shown, and the parts of it a quote may cite ([run.shown]).
type shownBlock struct {
	name  string
	text  string
	parts []string
}

// QuoteClass says how a quote that the evidence check refused differs from what the model was shown. It is
// fixed text from code. The classes diagnose the prompt and the check (#474); none of them is ever read to
// decide anything, and the check itself stays exact.
type QuoteClass string

// The classes, in the order [classifyQuote] tries them: the first that fits is the one reported.
const (
	// QuoteTooShort: under the shortest quote that counts, spaces trimmed.
	QuoteTooShort QuoteClass = "too-short"
	// QuoteUnknownSource: the ID names no block or tool result the model was shown.
	QuoteUnknownSource QuoteClass = "unknown-source"
	// QuoteUnquotableSource: the cited block has no quotable part (not available, a refused or failed tool
	// call, a tool answer not about the subject).
	QuoteUnquotableSource QuoteClass = "unquotable-source"
	// QuoteWrongBlock: found verbatim within one quotable part of another block.
	QuoteWrongBlock QuoteClass = "wrong-block"
	// QuoteOutsideData: found verbatim in the cited block as shown, but not within one of its data values: it
	// takes in a label or a line written by code, or spans two values.
	QuoteOutsideData QuoteClass = "outside-data"
	// QuoteUnescaped: found once its angle brackets are escaped as the block shows them.
	QuoteUnescaped QuoteClass = "unescaped"
	// QuoteJoinedLines: found once the line breaks of the block (and the indentation after them) are replaced
	// by one space or by nothing.
	QuoteJoinedLines QuoteClass = "joined-lines"
	// QuoteWhitespace: found once every run of spaces, tabs and line breaks, in the quote and in the block, is
	// collapsed to one space.
	QuoteWhitespace QuoteClass = "whitespace"
	// QuoteCase: found with whitespace collapsed and case ignored.
	QuoteCase QuoteClass = "case"
	// QuotePartial: a prefix of the quote of at least the shortest quote's length and half the quote is found in
	// the cited block, and the rest is not: a line cut and then changed or ended with an ellipsis.
	QuotePartial QuoteClass = "partial"
	// QuoteNotFound: none of the above, a paraphrase.
	QuoteNotFound QuoteClass = "not-found"
)

// QuoteFailure is one quote of a result the evidence check refused, as a class and lengths only: no text of
// the quote or of the block, which may hold model text or cluster data.
type QuoteFailure struct {
	// Source is the cited ID as the schema bounds it (c1, t2...).
	Source string
	// Block is the cited block's context section or tool name, fixed text from code; empty for an unknown ID.
	Block string
	Class QuoteClass
	// QuoteBytes is the quote's length, spaces trimmed; BlockBytes is the cited block's length as shown.
	QuoteBytes, BlockBytes int
	// MatchedBytes is the length of the longest prefix found, for [QuotePartial]; 0 otherwise.
	MatchedBytes int
}

// quoteFailures classifies every quote of ev, for an outcome the evidence check refused.
func (r *run) quoteFailures(ev []Evidence) []QuoteFailure {
	out := make([]QuoteFailure, 0, len(ev))
	for _, e := range ev {
		out = append(out, classifyQuote(r.shown, e))
	}
	return out
}

// classifyQuote says how the quote e differs from the blocks shown. It never passes a quote: it only runs
// once the check refused all of them.
func classifyQuote(shown map[string]shownBlock, e Evidence) QuoteFailure {
	q := strings.TrimSpace(e.Quote)
	f := QuoteFailure{Source: e.Source, QuoteBytes: len(q)}
	blk, ok := shown[e.Source]
	if ok {
		f.Block, f.BlockBytes = blk.name, len(blk.text)
	}
	switch {
	case len(q) < minQuoteBytes:
		f.Class = QuoteTooShort
	case !ok:
		f.Class = QuoteUnknownSource
	case len(blk.parts) == 0:
		f.Class = QuoteUnquotableSource
	case inOtherBlock(shown, e.Source, q):
		f.Class = QuoteWrongBlock
	case strings.Contains(blk.text, q):
		f.Class = QuoteOutsideData
	case strings.Contains(blk.text, tools.EscapeAngles(q)):
		f.Class = QuoteUnescaped
	case joinedLines(blk.text, q):
		f.Class = QuoteJoinedLines
	case strings.Contains(collapse(blk.text), collapse(q)):
		f.Class = QuoteWhitespace
	case strings.Contains(strings.ToLower(collapse(blk.text)), strings.ToLower(collapse(q))):
		f.Class = QuoteCase
	default:
		if n := longestPrefix(blk.text, q); n >= minQuoteBytes && 2*n >= len(q) {
			f.Class, f.MatchedBytes = QuotePartial, n
		} else {
			f.Class = QuoteNotFound
		}
	}
	return f
}

// inOtherBlock reports whether q lies within a quotable part of a block other than id.
func inOtherBlock(shown map[string]shownBlock, id, q string) bool {
	for other, blk := range shown {
		if other == id {
			continue
		}
		for _, p := range blk.parts {
			if strings.Contains(p, q) {
				return true
			}
		}
	}
	return false
}

// joinedLines reports whether a quote of one line is found in text once its line breaks, with the spaces and
// tabs around them, become one space or nothing.
func joinedLines(text, q string) bool {
	if strings.Contains(q, "\n") {
		return false
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
		if i > 0 {
			lines[i] = strings.TrimLeft(lines[i], " \t")
		}
	}
	return strings.Contains(strings.Join(lines, " "), q) || strings.Contains(strings.Join(lines, ""), q)
}

// collapse replaces every run of white space with one space.
func collapse(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// longestPrefix returns the length of the longest prefix of q, cut at a rune boundary, found in text. A prefix
// of a found prefix is found too, so a binary search over the length finds it.
func longestPrefix(text, q string) int {
	lo, hi := 0, len(q)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if strings.Contains(text, q[:mid]) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	for lo > 0 && lo < len(q) && !utf8.RuneStart(q[lo]) {
		lo--
	}
	return lo
}
