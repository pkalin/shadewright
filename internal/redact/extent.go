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

package redact

import "strings"

// The extent rule (#708). After a secret key name or flag, a value that opens a group (a quote, "[", "{",
// "$(" or a backtick) runs to the end of that group, over line ends, with nested groups and quoted strings
// inside it read as such; shell words glue further pieces to it; a plain value under a flag in a YAML flow
// sequence, or after an anchor or tag, runs to the end of its item; a block scalar under a flag item of a
// block sequence takes its indented lines; and "--name=value" at the start of a quoted argument runs to the
// argument's closing quote (argumentEnd). One scanner reads all of these. It never reads more than
// maxValueExtent bytes past the start of the value, nor nests deeper than maxExtentNesting, so each call costs
// constant time and the rules that call it stay linear in the input. A group that does not close within the
// bound gives no extent, and the caller keeps the value it read before (one token, or the text to its
// format's boundary), so a stray quote never takes the rest of a log.
const (
	// maxValueExtent is how far past its start the extent rule reads a value.
	maxValueExtent = 4096
	// maxExtentNesting is how deep groups may nest inside a value.
	maxExtentNesting = 32
)

// substInQuotes marks on the stack of groupEnd a "$(" opened inside a double-quoted string: its ")" closes it
// and the string goes on.
const substInQuotes = 'D'

// extentLimit returns the index the extent rule reads up to from pos.
func extentLimit(s string, pos int) int {
	return min(len(s), pos+maxValueExtent)
}

// quoteLevel returns the escape level of a double quote preceded by run backslashes: the lowest zero bit of
// run (0 for a plain quote or one after an escaped backslash, 1 for "\"", 2 for "\\\"", as escapedQuotedSpan
// reads it).
func quoteLevel(run int) int {
	j := 0
	for run&(1<<j) != 0 {
		j++
	}
	return j
}

// groupEnd returns the index just past the group that opens at pos: a double or single quote, "[", "{",
// "$(" or a backtick. Inside it, quoted strings are read whole (a double quote escaped n times closes only a
// string opened at the same level, and one escaped fewer times than the context or the open string ends an
// enclosing string, so the group does not close; a single quote has no escapes and a doubled one stays inside), and
// brackets, braces, "$(" (and "(" inside it) and backticks nest. ctx is the escape level of the text around
// the value. ok is false when the group does not close before limit, nests deeper than maxExtentNesting, or
// meets a closer that does not match.
func groupEnd(s string, pos, limit, ctx int) (int, bool) {
	g := groupScan{s: s, limit: limit, ctx: ctx}
	for i := pos; i < limit; i++ {
		var st scanStep
		switch g.quote {
		case '\'':
			i, st = g.singleQuoted(i)
		case '"':
			i, st = g.doubleQuoted(i)
		default:
			i, st = g.unquoted(i)
		}
		switch st {
		case scanClosed:
			return i + 1, true
		case scanFailed:
			return 0, false
		}
	}
	return 0, false
}

// scanStep is the result of one byte read by groupScan.
type scanStep int

const (
	scanGoOn scanStep = iota
	scanClosed
	scanFailed
)

// groupScan is the state of groupEnd: the stack of expected closers, the open quote and its escape level,
// and the run of backslashes before the current byte.
type groupScan struct {
	s           string
	limit, ctx  int
	stack       [maxExtentNesting]byte
	depth       int
	quote       byte
	qLevel, run int
}

// push opens a nested group whose closer is c, and reports false when that nests too deep.
func (g *groupScan) push(c byte) scanStep {
	if g.depth == len(g.stack) {
		return scanFailed
	}
	g.stack[g.depth] = c
	g.depth++
	return scanGoOn
}

// top returns the closer of the innermost open group, or 0.
func (g *groupScan) top() byte {
	if g.depth == 0 {
		return 0
	}
	return g.stack[g.depth-1]
}

// closeQuote ends the open quoted string; the group is closed when no bracket is open.
func (g *groupScan) closeQuote() scanStep {
	g.quote = 0
	if g.depth == 0 {
		return scanClosed
	}
	return scanGoOn
}

// singleQuoted reads the byte at i inside a single-quoted string: only a single quote ends it, and a doubled one stays inside.
func (g *groupScan) singleQuoted(i int) (int, scanStep) {
	if g.s[i] != '\'' {
		return i, scanGoOn
	}
	if i+1 < g.limit && g.s[i+1] == '\'' {
		return i + 1, scanGoOn
	}
	return i, g.closeQuote()
}

// doubleQuoted reads the byte at i inside a double-quoted string.
func (g *groupScan) doubleQuoted(i int) (int, scanStep) {
	c := g.s[i]
	switch {
	case c == '\\':
		g.run++
		return i, scanGoOn
	case c == '$' && g.run == 0 && g.qLevel == 0 && i+1 < g.limit && g.s[i+1] == '(':
		// A command substitution inside double quotes: its text is read afresh, its own quotes included,
		// and the string goes on after its ")".
		g.quote = 0
		return i + 1, g.push(substInQuotes)
	case c == '"':
		j := quoteLevel(g.run)
		g.run = 0
		switch {
		case j == g.qLevel:
			return i, g.closeQuote()
		case j < g.qLevel:
			return i, scanFailed
		}
	}
	g.run = 0
	return i, scanGoOn
}

// unquoted reads the byte at i outside any quoted string.
func (g *groupScan) unquoted(i int) (int, scanStep) {
	c := g.s[i]
	run := g.run
	g.run = 0
	switch {
	case c == '\\':
		g.run = run + 1
	case c == '"':
		j := quoteLevel(run)
		if j < g.ctx {
			return i, scanFailed
		}
		g.quote, g.qLevel = '"', j
	case c == '\'' && run == 0:
		g.quote = '\''
	case c == '[':
		return i, g.push(']')
	case c == '{':
		return i, g.push('}')
	case c == '$' && i+1 < g.limit && g.s[i+1] == '(':
		return i + 1, g.push(')')
	case c == ')' && g.top() == substInQuotes:
		g.depth--
		g.quote, g.qLevel = '"', 0
	case c == '(' && (g.top() == ')' || g.top() == substInQuotes):
		return i, g.push(')')
	case c == '`' && g.top() != '`':
		return i, g.push('`')
	case g.depth > 0 && c == g.top():
		g.depth--
		if g.depth == 0 {
			return i, scanClosed
		}
	case c == ']' || c == '}' || c == ')':
		return i, scanFailed
	}
	return i, scanGoOn
}

// opensGroup reports whether a quoted group of groupEnd opens at pos: a plain quote, "$(" or a backtick.
// Arrays and objects are read by bracketSpan.
func opensGroup(s string, pos int) bool {
	if pos >= len(s) {
		return false
	}
	switch c := s[pos]; c {
	case '"', '\'', '`':
		return true
	case '$':
		return pos+1 < len(s) && s[pos+1] == '('
	}
	return false
}

// shellWordEnd returns the end of the shell word that starts with the group at pos: the group, then any
// further groups and unquoted characters glued to it ("horse"'battery'staple, "$(echo a b)"x), up to
// whitespace, one of ",;|&<>(){}[]", a backslash, or a group that does not close. last is true when the word
// ends with a quoted group, so its closing quote is at end-1. ok is false when the first group does not close.
func shellWordEnd(s string, pos, limit int) (end int, last bool, ok bool) {
	end, ok = groupEnd(s, pos, limit, 0)
	if !ok {
		return 0, false, false
	}
	last = s[pos] == '"' || s[pos] == '\''
	for end < limit {
		c := s[end]
		if opensGroup(s, end) {
			e, closed := groupEnd(s, end, limit, 0)
			if !closed {
				break
			}
			last = c == '"' || c == '\''
			end = e
			continue
		}
		if isSpace(c) || strings.IndexByte(",;|&<>(){}[]\\", c) >= 0 {
			break
		}
		last = false
		end++
	}
	return end, last, true
}

// quotedWordSpan returns the value of a shell word that opens with a group at pos, for a value after a key
// name: for a word that opens with a quote, the text after that quote up to the closing quote of the last
// quoted piece, or the word's end; for one that opens with "$(" or a backtick, the whole word. ok is false
// when the first group does not close within maxValueExtent.
func quotedWordSpan(s string, pos int) (int, int, bool) {
	end, last, ok := shellWordEnd(s, pos, extentLimit(s, pos))
	if !ok {
		return 0, 0, false
	}
	start := pos
	if c := s[pos]; c == '"' || c == '\'' {
		start++
	}
	if last {
		end--
	}
	return start, max(start, end), true
}

// bracketSpan returns the value that opens with "[" or "{" at pos (an array or object under a secret key, plain
// or escaped as JSON inside a string at escape level ctx), through its closing bracket. ok is false when it
// does not close within maxValueExtent.
func bracketSpan(s string, pos, ctx int) (int, int, bool) {
	if pos >= len(s) || s[pos] != '[' && s[pos] != '{' {
		return 0, 0, false
	}
	end, ok := groupEnd(s, pos, extentLimit(s, pos), ctx)
	return pos, end, ok
}

// commentEnd returns the index after the JSON comments ("// ..." to the line end, "/* ... */") and the
// whitespace after them that start at pos, or pos when none starts there. It reads at most maxValueExtent
// bytes; a block comment that does not close by then ends there.
func commentEnd(s string, pos int) int {
	limit := extentLimit(s, pos)
	i := pos
	for i+1 < limit && s[i] == '/' && (s[i+1] == '/' || s[i+1] == '*') {
		if s[i+1] == '/' {
			j := strings.IndexByte(s[i:limit], '\n')
			if j < 0 {
				return limit
			}
			i += j
		} else {
			j := strings.Index(s[i+2:limit], "*/")
			if j < 0 {
				return limit
			}
			i += j + 4
		}
		for i < limit && isSpace(s[i]) {
			i++
		}
	}
	return i
}

// flowItemEnd returns the end of a plain item of a YAML flow sequence that starts at pos: the next ",", "]",
// "}" or line end, within maxValueExtent bytes, with the blanks before it trimmed.
func flowItemEnd(s string, pos int) int {
	limit := extentLimit(s, pos)
	end := limit
	if i := strings.IndexAny(s[pos:limit], ",]}\r\n"); i >= 0 {
		end = pos + i
	}
	for end > pos && isSpace(s[end-1]) {
		end--
	}
	return end
}

// lineItemEnd returns the end of a plain item of a YAML block sequence that starts at pos: the line end,
// within maxValueExtent bytes, with the blanks before it trimmed.
func lineItemEnd(s string, pos int) int {
	limit := extentLimit(s, pos)
	end := limit
	if i := strings.IndexAny(s[pos:limit], "\r\n"); i >= 0 {
		end = pos + i
	}
	for end > pos && isSpace(s[end-1]) {
		end--
	}
	return end
}

// inFlowSequence reports whether the flag that starts at flagStart is an item of a YAML flow sequence: "["
// or "," right before it, after blanks.
func inFlowSequence(s string, flagStart int) bool {
	i := flagStart
	for i > 0 && flagStart-i < maxValueExtent && (s[i-1] == ' ' || s[i-1] == '\t') {
		i--
	}
	return i > 0 && (s[i-1] == '[' || s[i-1] == ',')
}

// nodeProperties returns the index after the YAML anchor ("&name") and tag ("!!str", "!x") tokens that start
// at pos, at most two, and the blanks after each, when a value follows them on the same line; otherwise pos.
func nodeProperties(s string, pos int) int {
	i := pos
	for range 2 {
		if i+1 >= len(s) || s[i] != '&' && s[i] != '!' || isSpace(s[i+1]) {
			break
		}
		limit := extentLimit(s, i)
		j := i + 1
		for j < limit && !isSpace(s[j]) {
			j++
		}
		k := j
		for k < limit && (s[k] == ' ' || s[k] == '\t') {
			k++
		}
		if k == j || k >= limit || s[k] == '\n' || s[k] == '\r' {
			break
		}
		i = k
	}
	return i
}

// listBlockScalarSpan returns the content of a YAML block scalar ("|", ">-") that is the item at pos of a block
// sequence: the following lines indented deeper than the item's dash, the first of which starts at col of
// its line, up to the last non-blank one. With no content it is the indicator. It reads at most
// maxValueExtent bytes; a longer block ends at that bound.
func listBlockScalarSpan(s string, pos, col int) (int, int) {
	limit := extentLimit(s, pos)
	i := pos
	for i < limit && s[i] != '\n' {
		i++
	}
	start, end := -1, -1
	for i < limit && s[i] == '\n' {
		ls := i + 1
		t := ls
		for t < limit && (s[t] == ' ' || s[t] == '\t') {
			t++
		}
		le := t
		for le < limit && s[le] != '\n' {
			le++
		}
		text := le
		if text > t && s[text-1] == '\r' {
			text--
		}
		i = le
		if t == text {
			continue
		}
		if t-ls <= col {
			break
		}
		if start < 0 {
			start = t
		}
		end = text
	}
	if start < 0 {
		return pos, tokenEnd(s, pos)
	}
	return start, end
}
