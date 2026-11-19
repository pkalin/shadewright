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

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The extent rule (#708). After a secret key name or flag, a value that opens a group (a quote, "[", "{",
// "$(" or a backtick) runs to the end of that group, over line ends, with nested groups and quoted strings
// inside it read as such; shell words glue further pieces to it; a plain value under a flag in a YAML flow
// sequence, or after an anchor or tag, runs to the end of its item; a block scalar under a flag item of a
// block sequence takes its indented lines; and "--name=value" at the start of a quoted argument runs to the
// argument's closing quote (argumentEnd). One scanner reads all of these. It never reads more than
// maxValueExtent bytes past the start of the value, nor nests deeper than maxExtentNesting, so each call costs
// constant time and the rules that call it stay linear in the input. A group that does not close within the
// bound gives no extent, and the caller keeps the value it read before (one token, or the text to its
// format's boundary), so a stray quote never takes the rest of a log. After a key name (not a flag), a quoted
// string goes on over a line end only to a line indented deeper than the key, as a YAML scalar does, or to a
// blank line; any other line ends it, the group does not close, and the fields on the next lines stay. A
// shell quote after a flag spans lines as in a shell.
//
// Shell syntax ("$(", backticks, backslash escapes, glued words) applies only where the value is a shell word:
// in free text and after a flag on a command line. Inside a JSON or YAML quoted string, an array, an object, or
// an item of an argument list or sequence, the text is literal: a quoted string ends at its closing quote, and
// a quote opens a string only at the start of a scalar. Every caller takes the union of the value it gave
// before the extent rule and the extent, so a value is never shorter than before #708.
const (
	// maxValueExtent is how far past its start the extent rule reads a value.
	maxValueExtent = 4096
	// maxExtentNesting is how deep groups may nest inside a value.
	maxExtentNesting = 32
)

// substInQuotes marks on the stack of groupEnd a "$(" opened inside a double-quoted string: its ")" closes it
// and the string goes on.
const substInQuotes = 'D'

// backtickInQuotes marks on the stack of groupEnd a backtick opened inside a double-quoted string: the next
// backtick closes it and the string goes on.
const backtickInQuotes = 'B'

// noKey is the keyPos of groupEnd for a value with no key column rule (a flag's value, an array or object).
const noKey = -1

// lineColumn returns the column of pos on its line, reading back at most maxValueExtent bytes; when the line
// starts further back it returns maxValueExtent, so no line is indented deeper.
func lineColumn(s string, pos int) int {
	from := max(0, pos-maxValueExtent)
	i := lastIndexByte(s[from:pos], '\n')
	if i < 0 {
		if from > 0 {
			return maxValueExtent
		}
		return pos
	}
	return pos - (from + i + 1)
}

// lineIndent returns the number of blanks that start the line holding pos, read back at most maxValueExtent
// bytes; when the line starts further back it returns maxValueExtent.
func lineIndent(s string, pos int) int {
	col := lineColumn(s, pos)
	if col >= maxValueExtent {
		return maxValueExtent
	}
	start := pos - col
	t := start
	for t < pos && (s[t] == ' ' || s[t] == '\t') {
		tick(1)
		t++
	}
	return t - start
}

// indentedBeyond reports whether the line that starts at pos, read up to limit, is blank or indented by more
// than col blanks.
func indentedBeyond(s string, pos, limit, col int) bool {
	t := pos
	for t < limit && (s[t] == ' ' || s[t] == '\t') {
		tick(1)
		t++
	}
	return t >= limit || s[t] == '\n' || s[t] == '\r' || t-pos > col
}

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
		tick(1)
		j++
	}
	return j
}

// groupEnd returns the index just past the group that opens at pos: a double or single quote, "[", "{",
// "$(" or a backtick. Inside it, quoted strings are read whole (a double quote escaped n times closes only a
// string opened at the same level, and one escaped fewer times than the context or the open string ends an
// enclosing string, so the group does not close; a single quote has no escapes and a doubled one stays inside), and
// brackets, braces, "$(" (and "(" inside it) and backticks nest, also "$(" and backticks inside double quotes.
// Inside "$(" or backticks, "[", "]", "{" and "}" are plain text, as in a shell. ctx is the escape level of the
// text around the value. keyPos, unless noKey, is the start of the key the value belongs to: a quoted string
// then crosses a line end only to a line that is blank or indented deeper than the key's column. ok is false
// when the group does not close before limit, nests deeper than maxExtentNesting, meets a closer that does not
// match, or a quoted string meets a line it may not cross to.
//
// shell is true when the group is a shell word (free text, a flag's value on a command line). Shell syntax
// applies only there and inside "$(" and backticks: "$(" and a backtick inside a double-quoted string, and a
// backslash before "(", ")", "$" or a backtick. Elsewhere (a JSON or YAML quoted string, an array, an object)
// the text is literal, a quoted string ends at its closing quote, and inside an array or object a quote opens
// a string only at the start of a scalar (after "[", "{", "," or ":"), so "horse'battery" is plain text.
func groupEnd(s string, pos, limit, ctx, keyPos int, shell bool) (int, bool) {
	return groupScanEnd(groupScan{s: s, limit: limit, ctx: ctx, shell: shell}, pos, keyPos)
}

// groupEndKey is groupEnd of a shell word in text of level 0, and also reports whether a quoted string met a line
// end, where the end depends on the key at keyPos (or on there being one).
func groupEndKey(s string, pos, limit, keyPos int) (int, bool, bool) {
	var used bool
	end, ok := groupScanEnd(groupScan{s: s, limit: limit, shell: true, keyUsed: &used}, pos, keyPos)
	return end, ok, used
}

// groupScanEnd is groupEnd with the scanner g, whose props field says whether a YAML node property opens a
// scalar inside an array or object (bracketSpan).
func groupScanEnd(g groupScan, pos, keyPos int) (int, bool) {
	s, limit := g.s, g.limit
	keyCol := -1
	for i := pos; i < limit; i++ {
		tick(1)
		if s[i] == '\n' && g.quote != 0 && g.keyUsed != nil {
			*g.keyUsed = true
		}
		if s[i] == '\n' && g.quote != 0 && keyPos != noKey {
			if keyCol < 0 {
				keyCol = lineColumn(s, keyPos)
			}
			if !indentedBeyond(s, i+1, limit, keyCol) {
				return 0, false
			}
		}
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
	// shell is whether the group itself is a shell word; qShell whether the open quoted string is read as
	// a shell string; prev the last byte read outside quoted strings that is no blank and no backslash.
	shell, qShell bool
	prev          byte
	// props is whether node properties are read at all (bracketSpan's first scan); inProp is whether a YAML
	// node property ("!!str", "&a") that opens a scalar inside an array or object is
	// being read; afterProp whether one ended with a blank and nothing else of the scalar was read yet, so a
	// quote still opens a string ("[!!str \"a]b\"]", #769).
	props, inProp, afterProp bool
	// keyUsed, when set, is set to true when a quoted string meets a line end, where the key decides (groupEndKey).
	keyUsed *bool
}

// shellText reports whether the byte read now is shell text: inside "$(" or backticks, or in a group that
// is a shell word and holds no array or object.
func (g *groupScan) shellText() bool {
	return g.inShell() || g.depth == 0 && g.shell
}

// scalarStart reports whether a quote read now inside an array or object opens a string: it does at the
// start of a scalar only, after "[", "{", "," or ":" and blanks.
func (g *groupScan) scalarStart() bool {
	return g.shellText() || g.afterProp || strings.IndexByte("[{,:", g.prev) >= 0
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

// inShell reports whether the innermost open group is a command substitution ("$(", "(" inside it, or
// backticks), where brackets and braces are plain text.
func (g *groupScan) inShell() bool {
	switch g.top() {
	case ')', substInQuotes, '`', backtickInQuotes:
		return true
	}
	return false
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
	g.prev = '"'
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
	case c == '$' && g.qShell && g.run%2 == 0 && g.qLevel == 0 && i+1 < g.limit && g.s[i+1] == '(':
		// A command substitution inside double quotes: its text is read afresh, its own quotes included,
		// and the string goes on after its ")".
		g.quote, g.run = 0, 0
		return i + 1, g.push(substInQuotes)
	case c == '`' && g.qShell && g.run%2 == 0 && g.qLevel == 0:
		// A backtick substitution inside double quotes, read as "$(" above.
		g.quote, g.run = 0, 0
		return i, g.push(backtickInQuotes)
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

// unquoted reads the byte at i outside any quoted string, and records it as prev when it is no blank or
// backslash and opens no string. It also follows the YAML node properties that open a scalar inside an array
// or object (inProp, afterProp).
func (g *groupScan) unquoted(i int) (int, scanStep) {
	c := g.s[i]
	startsProp := g.props && (c == '!' || c == '&') && g.run == 0 && g.depth > 0 && !g.inShell() && !g.inProp && g.scalarStart()
	j, st := g.unquotedByte(i)
	switch {
	case isSpace(c):
		if g.inProp {
			g.inProp, g.afterProp = false, true
		}
	case startsProp:
		g.inProp, g.afterProp = true, false
	case strings.IndexByte(",[]{}", c) >= 0 || !g.inProp:
		g.inProp, g.afterProp = false, false
	}
	if g.quote == 0 && c != '\\' && !isSpace(c) {
		g.prev = c
	}
	return j, st
}

// unquotedByte is unquoted without the record of prev.
func (g *groupScan) unquotedByte(i int) (int, scanStep) {
	c := g.s[i]
	run := g.run
	g.run = 0
	switch {
	case c == '\\':
		g.run = run + 1
	case run%2 == 1 && g.shellText() && strings.IndexByte("()$`", c) >= 0:
		// Escaped in a shell: plain text ("\)" does not close "$(").
	case c == '"' || c == '\'' && run == 0:
		return i, g.openQuote(c, run)
	case (c == '[' || c == '{' || c == ']' || c == '}') && g.inShell():
		// Plain text in a shell: "${x}", "a[1".
	case c == '[':
		return i, g.push(']')
	case c == '{':
		return i, g.push('}')
	case c == '$' || c == '(' || c == '`' && g.top() != '`' || c == ')' && g.top() == substInQuotes:
		// Shell groups; outside a shell these bytes are plain text.
		if j, st, ok := g.shellByte(i); ok {
			return j, st
		}
	case g.depth > 0 && c == g.top():
		g.depth--
		if g.depth == 0 {
			return i, scanClosed
		}
	case c == ']' || c == '}' || c == ')' && g.shellText():
		// A closer that does not match; outside a shell, "(" and ")" are plain text.
		return i, scanFailed
	}
	return i, scanGoOn
}

// shellByte reads "$(", "(", ")" or a backtick at i that opens or closes a shell group: "$(" and a backtick in
// shell text, "(" inside "$(", and the ")" or backtick that ends a substitution inside a double-quoted
// string, which goes on. ok is false when the byte does none of these.
func (g *groupScan) shellByte(i int) (int, scanStep, bool) {
	switch c := g.s[i]; {
	case c == '$' && i+1 < g.limit && g.s[i+1] == '(' && g.shellText():
		return i + 1, g.push(')'), true
	case c == ')' && g.top() == substInQuotes, c == '`' && g.top() == backtickInQuotes:
		g.depth--
		g.quote, g.qLevel, g.qShell = '"', 0, true
		return i, scanGoOn, true
	case c == '(' && g.inShell():
		return i, g.push(')'), true
	case c == '`' && g.top() != '`' && g.shellText():
		return i, g.push('`'), true
	}
	return i, scanGoOn, false
}

// openQuote reads a quote outside any quoted string, preceded by run backslashes: a double quote escaped
// less than the context ends an enclosing string, so the group fails; inside an array or object a quote
// opens a string only at the start of a scalar; otherwise it opens one.
func (g *groupScan) openQuote(c byte, run int) scanStep {
	j := 0
	if c == '"' {
		j = quoteLevel(run)
		if j < g.ctx {
			return scanFailed
		}
	}
	if g.depth > 0 && !g.scalarStart() {
		return scanGoOn
	}
	g.quote, g.qLevel, g.qShell = c, j, g.shellText()
	return scanGoOn
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
// further groups, escaped blanks ("\\ ") and unquoted characters glued to it ("horse"'battery'staple,
// "$(echo a b)"x, "a"\\ b), up to whitespace, one of ",;|&<>(){}[]", another backslash, or a group that does not
// close; after a flag (flag is true) a comma does not end it ("$(echo),abc" is one argument). A run of
// ")", "]" and "}" belongs to the word when more of the word follows it ("$(cat pw)}x"), and
// ends it otherwise ("[\"abc\"], next"). last is true when the word ends with a quoted group, so its closing
// quote is at end-1. ok is false when the first group does not close. keyPos is as in groupEnd. With wide
// false an escaped punctuation byte and a comma end the word, as on main before #769 (keyValueSpans).
func shellWordEnd(s string, pos, limit, keyPos int, flag, wide bool, w *wordMemo) (end int, last bool, ok bool) {
	end, ok = groupEnd(s, pos, limit, 0, keyPos, true)
	if !ok {
		return 0, false, false
	}
	last = s[pos] == '"' || s[pos] == '\''
	sc := &wordScan{limit: limit, flag: flag, wide: wide, dep: -1}
	defer func() { w.record(sc) }()
	for end < limit {
		tick(1)
		sc.add(end, last)
		if to, toLast, bound, ok := w.resume(s, end, limit, keyPos, flag, wide); ok {
			// The steps taken over keep the bound of the key column they had.
			sc.keyUsed(bound)
			end, last = to, toLast
			continue
		}
		c := s[end]
		if opensGroup(s, end) {
			e, closed, usedKey, indent := w.groupEndKey(s, end, limit, keyPos)
			if !closed {
				break
			}
			if usedKey {
				sc.keyUsed(indent)
			}
			if e >= limit {
				// A group that closes at the limit may close there only because its last quote could not
				// look past it ("'a''" closes for a limit before its fourth quote): a larger limit reads on.
				sc.dependent()
			}
			last = c == '"' || c == '\''
			end = e
			continue
		}
		if c == '\\' && end+1 < limit && (s[end+1] == ' ' || s[end+1] == '\t' || wide && strings.IndexByte(shellEscapedPunct, s[end+1]) >= 0) {
			last = false
			end += 2
			continue
		}
		if c == ')' || c == ']' || c == '}' {
			j := end
			for j < limit && (s[j] == ')' || s[j] == ']' || s[j] == '}') {
				tick(1)
				j++
			}
			if j < limit && !isSpace(s[j]) && strings.IndexByte(wordStops+"\"'`$", s[j]) < 0 {
				last = false
				end = j
				continue
			}
			break
		}
		if wordStopsAt(s, end, limit, flag, wide, sc) {
			break
		}
		last = false
		end++
	}
	if end >= limit {
		sc.add(end, last)
	}
	return end, last, true
}

// wordStopsAt reports whether the plain byte at i ends the shell word: whitespace, or one of wordStops, a comma
// only when no flag comes before the word and, with wide, when it starts the next field (fieldAfterComma). A
// comma decided on bytes up to limit marks the step of sc as dependent on the limit.
func wordStopsAt(s string, i, limit int, flag, wide bool, sc *wordScan) bool {
	c := s[i]
	if isSpace(c) || strings.IndexByte(wordStops, c) >= 0 && c != ',' {
		return true
	}
	if c != ',' || flag {
		return false
	}
	if !wide {
		return true
	}
	field, reached := fieldAfterComma(s, i, limit)
	if reached {
		sc.dependent()
	}
	return field
}

// The shell words of one text (#845, fifteenth decision, P3). Every secret key in a long run of glued words
// ("TOKEN=\"a\",b,TOKEN=\"a\",b,...") reads its value as the shell word to its end, up to maxValueExtent
// bytes, and each later key starts inside the word an earlier key read: without a memory of those reads the
// work is the number of keys times maxValueExtent. A wordMemo keeps, for each word read in one pass of a rule
// over one text, the states its loop went through, and a later read that reaches one of them goes on from the
// first state whose step depended on the earlier read's limit, since every step before it reads the same bytes
// and decides the same with the later, larger limit.
//
// A quoted group that crosses a line end depends on the key's column too (a line indented no deeper than the key
// ends it). Such a step does not end what the memory keeps: the group closed for the earlier read, so every line
// it crossed was indented deeper than that key, and a later read goes on past it when its own key's column is
// less than the least indentation of those lines (the bound of the state). The groups themselves are read once
// per start (groupTrace): the scan up to the furthest limit any read may have, with the line ends it crossed and
// the places where the limit would decide, so that a later read of the same group with another limit or key
// costs a lookup instead of a scan of up to maxValueExtent bytes (#845, P3: a run of TOKEN="a" before a quote, a
// line break and 4,096 blanks read that group once per key). The result is the same as without the memory
// (TestShellWordEndMemoMatches).

// unbounded is the bound of a state no key column limits.
const unbounded = int(^uint(0) >> 1)

// wordMemo is the memory of the shell words read in one pass of a rule over one text; nil keeps none.
type wordMemo struct {
	at map[wordState]wordRef
	// groups are the quoted groups read in the loop of shellWordEnd, by their start.
	groups map[int]*groupTrace
	// blanks is the end of the run of blanks and tabs that starts at a line start, by that start.
	blanks map[int]int
	// colPos and colStart are the last key position whose column was asked and the start of its line, so that
	// the columns of keys asked in increasing order cost the bytes between them (colSet once they hold).
	colPos, colStart int
	colSet           bool
	// resumed counts the reads that went on from a state of an earlier one, for the tests.
	resumed int
}

// wordState is a state of shellWordEnd's loop, with the parameters the steps after it depend on.
type wordState struct {
	pos        int
	flag, wide bool
}

// wordRef is a state of the read scan, by its index in it, with the bound of the key column a later read may
// have to go on from it (unbounded when no step after it depended on the key).
type wordRef struct {
	scan  *wordScan
	i     int
	bound int
}

// wordScan is one read of shellWordEnd: its limit and parameters, the states of its loop in order (the last one
// where it stopped, at a break or at its limit), and the index of the first state whose step went on in a way that
// depended on the limit (dep, -1 while none did). A read with a larger limit goes on from there, or from the last
// state, whose step it takes again. indent is, for each state, the least indentation of the lines the quoted group
// read by its step crossed where the key decides (unbounded when it crossed none).
type wordScan struct {
	limit      int
	flag, wide bool
	pos        []int
	last       []bool
	indent     []int
	dep        int
}

func (sc *wordScan) add(pos int, last bool) {
	sc.pos = append(sc.pos, pos)
	sc.last = append(sc.last, last)
	sc.indent = append(sc.indent, unbounded)
}

// dependent marks the step from the latest state as one that depended on the limit.
func (sc *wordScan) dependent() {
	if sc.dep < 0 {
		sc.dep = len(sc.pos) - 1
	}
}

// keyUsed records that the step from the latest state read a quoted group that closed across line ends, whose
// least indentation was indent: a later read goes on past it only with a key column less than that.
func (sc *wordScan) keyUsed(indent int) {
	k := len(sc.indent) - 1
	sc.indent[k] = min(sc.indent[k], indent)
}

// resume returns the state a read with limit, key, flag and wide at pos may go on from: the first dependent state
// (or the last state) of an earlier read that went through pos with a limit no larger, when that is past pos and
// the key's column is within the bound of the state, and that bound.
func (w *wordMemo) resume(s string, pos, limit, keyPos int, flag, wide bool) (int, bool, int, bool) {
	if w == nil {
		return 0, false, 0, false
	}
	r, ok := w.at[wordState{pos: pos, flag: flag, wide: wide}]
	if !ok || r.scan.limit > limit {
		return 0, false, 0, false
	}
	to := r.scan.pos[r.scan.dep]
	if to <= pos {
		return 0, false, 0, false
	}
	if r.bound != unbounded && w.column(s, keyPos) >= r.bound {
		return 0, false, 0, false
	}
	w.resumed++
	return to, r.scan.last[r.scan.dep], r.bound, true
}

// record keeps the states of sc before its first dependent one, each with the least indentation of the groups
// read from it up to there.
func (w *wordMemo) record(sc *wordScan) {
	if w == nil || len(sc.pos) == 0 {
		return
	}
	if sc.dep < 0 {
		sc.dep = len(sc.pos) - 1
	}
	if w.at == nil {
		w.at = map[wordState]wordRef{}
	}
	bound := unbounded
	for i := sc.dep - 1; i >= 0; i-- {
		tick(1)
		bound = min(bound, sc.indent[i])
		w.at[wordState{pos: sc.pos[i], flag: sc.flag, wide: sc.wide}] = wordRef{scan: sc, i: i, bound: bound}
	}
}

// column returns the column groupScanEnd compares the lines of a quoted group with for the key at keyPos
// (lineColumn), or -1 for noKey, which no line ends.
func (w *wordMemo) column(s string, keyPos int) int {
	switch {
	case keyPos == noKey:
		return -1
	case !w.colSet:
		w.colStart, w.colPos, w.colSet = lastIndexByte(s[:keyPos], '\n')+1, keyPos, true
	case keyPos >= w.colPos:
		if i := lastIndexByte(s[w.colPos:keyPos], '\n'); i >= 0 {
			w.colStart = w.colPos + i + 1
		}
		w.colPos = keyPos
	default:
		return lineColumn(s, keyPos)
	}
	return min(keyPos-w.colStart, maxValueExtent)
}

// blankEnd returns the end of the run of blanks and tabs from the line start at pos, read once per start.
func (w *wordMemo) blankEnd(s string, pos int) int {
	if t, ok := w.blanks[pos]; ok {
		return t
	}
	t := pos
	for t < len(s) && (s[t] == ' ' || s[t] == '\t') {
		tick(1)
		t++
	}
	if w.blanks == nil {
		w.blanks = map[int]int{}
	}
	w.blanks[pos] = t
	return t
}

// groupEndKey is groupEndKey of the quoted group at pos, read through the memory, and also returns the least
// indentation of the lines the group crossed where the key decides (unbounded for none). Without a memory, or
// with a limit past the furthest one the memory reads to, it reads the group itself, and the indentation is -1.
func (w *wordMemo) groupEndKey(s string, pos, limit, keyPos int) (end int, ok, used bool, indent int) {
	if w == nil {
		end, ok, used = groupEndKey(s, pos, limit, keyPos)
		return end, ok, used, -1
	}
	t := w.groups[pos]
	if t == nil {
		t = &groupTrace{g: groupScan{s: s, limit: min(len(s), pos+maxValueExtent), shell: true}, next: pos}
		if w.groups == nil {
			w.groups = map[int]*groupTrace{}
		}
		w.groups[pos] = t
	}
	if limit > t.g.limit {
		end, ok, used = groupEndKey(s, pos, limit, keyPos)
		return end, ok, used, -1
	}
	return t.query(s, limit, keyPos, w)
}

// groupTrace is the read of one quoted group of a shell word (groupEndKey) with the furthest limit a read of it
// may have (g.limit), made only as far as the reads so far needed (next), with what decides the result for a
// smaller limit and any key: the line ends inside quoted strings it crossed, the places where a doubled single
// quote closed the string for a limit just past its first quote, and where it closed or failed.
type groupTrace struct {
	g    groupScan
	next int
	// done is whether the scan closed (closed, the group ending at end) or failed, at the step that started at
	// term.
	done, closed bool
	term, end    int
	lines        []traceLine
	// pairs are the starts of the doubled single quotes read at depth 0, in order: a read whose limit is just past
	// the first one closes the group there.
	pairs []int
}

// traceLine is a line end inside a quoted string of a groupTrace: its index, the end of the blanks of the next
// line, and the least indentation of this and the earlier lines (unbounded for a line that is blank).
type traceLine struct {
	nl, blankEnd, minIndent int
}

// extend reads the group up to to.
func (t *groupTrace) extend(s string, to int, w *wordMemo) {
	g := &t.g
	for !t.done && t.next < to {
		i := t.next
		tick(1)
		if s[i] == '\n' && g.quote != 0 {
			be := w.blankEnd(s, i+1)
			indent := unbounded
			if be < len(s) && s[be] != '\n' && s[be] != '\r' {
				indent = be - (i + 1)
			}
			if k := len(t.lines); k > 0 {
				indent = min(indent, t.lines[k-1].minIndent)
			}
			t.lines = append(t.lines, traceLine{nl: i, blankEnd: be, minIndent: indent})
		}
		start := i
		pair := g.quote == '\'' && s[i] == '\'' && g.depth == 0
		var st scanStep
		switch g.quote {
		case '\'':
			i, st = g.singleQuoted(i)
		case '"':
			i, st = g.doubleQuoted(i)
		default:
			i, st = g.unquoted(i)
		}
		if pair && i == start+1 {
			t.pairs = append(t.pairs, start)
		}
		switch st {
		case scanClosed:
			t.done, t.closed, t.term, t.end = true, true, start, i+1
		case scanFailed:
			t.done, t.term = true, start
		}
		t.next = i + 1
	}
}

// query returns what groupEndKey(s, pos, limit, keyPos) returns, and the least indentation of the lines crossed.
// Up to the step at limit-1 the scan reads what the read with limit reads; that step peeks at limit only for a
// doubled single quote (which closes the group at limit for the read with limit, when it is at depth 0), "$(" and
// a backtick in a string (which open nothing then, and the group does not close either way). A line end inside a
// string ends the group for the key when its next line is neither blank nor indented deeper than the key's column
// and its blanks end before limit (indentedBeyond).
func (t *groupTrace) query(s string, limit, keyPos int, w *wordMemo) (end int, ok, used bool, indent int) {
	t.extend(s, limit, w)
	tick(1)
	n := 0
	for lo, hi := 0, len(t.lines); lo < hi; {
		tick(1)
		if mid := (lo + hi) / 2; t.lines[mid].blankEnd < limit {
			n, lo = mid+1, mid+1
		} else {
			hi = mid
		}
	}
	used = len(t.lines) > 0 && t.lines[0].nl < limit
	indent = unbounded
	if n > 0 {
		indent = t.lines[n-1].minIndent
	}
	if indent != unbounded && w.column(s, keyPos) >= indent {
		return 0, false, used, indent
	}
	if _, found := slices.BinarySearch(t.pairs, limit-1); found {
		return limit, true, used, indent
	}
	if t.done && t.term < limit && t.closed {
		return t.end, true, used, indent
	}
	return 0, false, used, indent
}

// fieldAfterComma reports whether the comma at i, glued to a shell word after a key name, starts the next
// field of a list rather than more of the value (#769): it is followed by whitespace, the end of the input, a
// quote or a separator, by a secret key name the key rules match on their own, with blanks, "=", ":" or ":="
// after it ("password=\"abc\",DB_PASSWORD= \"x y\"", whose value is then redacted as on main), or by a name
// and "=" or ":" ("password=\"abc\",user=bob", ",next: x"). Any other name and "=" followed by whitespace, the
// end or another "=" is base64 padding, not a field ("\"a\",abc="). reached is whether it read up to limit, so
// that a larger limit could decide otherwise.
func fieldAfterComma(s string, i, limit int) (field, reached bool) {
	j := i + 1
	if j >= limit {
		return true, true
	}
	if isSpace(s[j]) || strings.IndexByte("\"'`,;|&<>(){}[]", s[j]) >= 0 {
		return true, false
	}
	for j < limit && (isAlnum(s[j]) || strings.IndexByte("_.-", s[j]) >= 0) {
		tick(1)
		j++
	}
	// commaSecretKey reads a run of key name characters (with the Kelvin sign and the long s, which its
	// case-insensitive match folds to "k" and "s"), the blanks after it and one byte more: it is matched on that
	// much of the text only, so each comma costs what follows it up to its next field, not the rest of the text
	// (#845, fifteenth decision, P3).
	k := j
	for k < limit {
		tick(1)
		if isAlnum(s[k]) || strings.IndexByte("_.-", s[k]) >= 0 {
			k++
			continue
		}
		if r, n := utf8.DecodeRuneInString(s[k:limit]); r == '\u212a' || r == '\u017f' {
			k += n
			continue
		}
		break
	}
	for k < limit && (s[k] == ' ' || s[k] == '\t') {
		tick(1)
		k++
	}
	reached = k+utf8.UTFMax >= limit
	if commaSecretKey.MatchString(counted(s[i+1 : min(k+1, limit)])) {
		return true, reached
	}
	switch {
	case j == i+1 || j >= limit:
		return false, reached
	case s[j] == ':':
		return true, reached
	case s[j] == '=':
		return j+1 < limit && !isSpace(s[j+1]) && s[j+1] != '=', reached
	}
	return false, reached
}

// commaSecretKey is a secret key name at the start of the text after a comma, followed by what the key rules
// read as its separator: "=", ":" or ":=" after optional blanks, or blanks (a compound name).
var commaSecretKey = regexp.MustCompile(`(?i)^(?:` + bareKeyExpr + `)(?:[ \t]*[:=]|[ \t]+)`)

// plainWordEnd returns the end of a shell word that starts with plain text at pos (no quote, "$(" or
// backtick), when a "$(" or backtick group or an escaped blank is glued to that text ("abc$(cat pw)",
// "a,b\\ c"): the word runs on through those groups, plain text and backslash escapes, and ends at
// whitespace, one of ";|&<>()", a quote, or a group that does not close. A quote ends it because an unquoted
// value in free text that meets a quote is often inside a JSON or logfmt string, whose fields after it stay
// (tokenEnd). It returns pos when no group or escaped blank is glued to the text. With wide false any quote
// ends it, as on main before #769 (keyValueSpans).
func plainWordEnd(s string, pos, limit, keyPos int, wide bool) int {
	end, glued := pos, false
	for end < limit {
		tick(1)
		c := s[end]
		switch {
		case c == '$' && end+1 < limit && s[end+1] == '(', c == '`':
			e, ok := groupEnd(s, end, limit, 0, keyPos, true)
			if !ok {
				return wordEnd(pos, end, glued)
			}
			end, glued = e, true
		case c == '\\' && end+1 < limit && (s[end+1] == ' ' || s[end+1] == '\t'):
			end, glued = end+2, true
		case c == '\\' && end+1 < limit && !isSpace(s[end+1]) && s[end+1] != '"' && s[end+1] != '\'':
			end += 2
		case wide && (c == '"' || c == '\'') && end > pos:
			e := gluedQuoteEnd(s, end, limit, keyPos)
			if e < 0 {
				return wordEnd(pos, end, glued)
			}
			end, glued = e, true
		case isSpace(c), c == '"', c == '\'', c == '\\', strings.IndexByte(";|&<>()", c) >= 0:
			return wordEnd(pos, end, glued)
		default:
			end++
		}
	}
	return wordEnd(pos, end, glued)
}

// gluedQuoteEnd returns the end of a quoted piece that opens at q, glued to plain text before it ("ab\"c d\"",
// #769), or -1 when the quote may close an enclosing JSON, YAML or logfmt string instead: the byte after it is
// whitespace, the end of the input, a quote, a backslash or one of ",;:)]}", or the piece does not close on
// its line within the bound.
func gluedQuoteEnd(s string, q, limit, keyPos int) int {
	if q+1 >= limit || isSpace(s[q+1]) || strings.IndexByte(",;:)]}\"'\\", s[q+1]) >= 0 {
		return -1
	}
	e, ok := groupEnd(s, q, limit, 0, keyPos, true)
	if !ok || containsAny(s[q:e], "\r\n") {
		return -1
	}
	return e
}

// wordEnd returns end when glued, else pos.
func wordEnd(pos, end int, glued bool) int {
	if glued {
		return end
	}
	return pos
}

// shellEscapedPunct are the punctuation bytes a backslash escapes inside a shell word of shellWordEnd ("a"\!b,
// #769). A letter or digit after a backslash ("\n", "\t", "\u") ends the word instead, as it reads as a JSON
// escape in a log line, and so do a quote and another backslash, which may close or escape an enclosing string.
const shellEscapedPunct = "!#$%&()*+,-./:;<=>?@[]^_`{|}~"

// wordStops are the bytes that end a shell word of shellWordEnd, besides whitespace.
const wordStops = ",;|&<>(){}[]\\"

// unclosedQuoteEnd returns the end of a value after a flag that opens with a quote at pos that does not close
// within maxValueExtent (#708): the rest of the line, within that bound and with the blanks before its end
// trimmed, so the flag form redacts as much as "password: \"..." does; a blank and a dash on that line start
// the next flag, which stays ("--token 'abc --region x"). A single quote with a double quote after it on the
// line gives 0: it may sit inside a JSON string, whose fields after it stay.
func unclosedQuoteEnd(s string, pos int) int {
	end := lineItemEnd(s, pos)
	if s[pos] == '\'' && indexByte(s[pos:end], '"') >= 0 {
		return 0
	}
	for i := pos + 1; i+1 < end; i++ {
		tick(1)
		if (s[i] == ' ' || s[i] == '\t') && s[i+1] == '-' {
			end = i
			break
		}
	}
	for end > pos && isSpace(s[end-1]) {
		tick(1)
		end--
	}
	return end
}

// quotedWordSpan returns the value of a shell word that opens with a group at pos, for a value after a key
// name: for a word that opens with a quote, the text after that quote up to the closing quote of the last
// quoted piece, or the word's end; for one that opens with "$(" or a backtick, the whole word. ok is false
// when the first group does not close within maxValueExtent. keyPos is as in groupEnd, flag and wide as in
// shellWordEnd.
func quotedWordSpan(s string, pos, keyPos int, flag, wide bool, w *wordMemo) (int, int, bool) {
	end, last, ok := shellWordEnd(s, pos, extentLimit(s, pos), keyPos, flag, wide, w)
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
// does not close within maxValueExtent. With wide false a YAML node property opens no string, as on main
// before #769 (keyValueSpans).
func bracketSpan(s string, pos, ctx int, wide bool) (int, int, bool) {
	if pos >= len(s) || s[pos] != '[' && s[pos] != '{' {
		return 0, 0, false
	}
	limit := extentLimit(s, pos)
	// A quoted scalar behind a YAML node property is read as a string (#769) in a second scan; the value is
	// the longer of the two scans that close, so it is never shorter than without the property rule.
	end, ok := groupEnd(s, pos, limit, ctx, noKey, false)
	if !wide {
		return pos, end, ok
	}
	if pe, pok := groupScanEnd(groupScan{s: s, limit: limit, ctx: ctx, props: true}, pos, noKey); pok {
		end, ok = max(end, pe), true
	}
	return pos, end, ok
}

// commentEnd returns the index after the JSON comments ("// ..." to the line end, "/* ... */") and the
// whitespace after them that start at pos, or pos when none starts there. It reads at most maxValueExtent
// bytes; a comment that does not end by then is not taken, nor are the comments after it, so an unclosed
// "/*" does not extend the value.
func commentEnd(s string, pos int) int {
	limit := extentLimit(s, pos)
	i := pos
	for i+1 < limit && s[i] == '/' && (s[i+1] == '/' || s[i+1] == '*') {
		tick(1)
		if s[i+1] == '/' {
			j := indexByte(s[i:limit], '\n')
			if j < 0 {
				if limit < len(s) {
					return i
				}
				return limit
			}
			i += j
		} else {
			j := index(s[i+2:limit], "*/")
			if j < 0 {
				return i
			}
			i += j + 4
		}
		for i < limit && isSpace(s[i]) {
			tick(1)
			i++
		}
	}
	return i
}

// flowItemEnd returns the end of a plain item of a YAML flow sequence that starts at pos: the next ",", "]" or
// "}", within maxValueExtent bytes, with the blanks before it trimmed. The item goes on over a line end to a
// line indented deeper than the line it starts on, as a YAML flow scalar does; a blank line or any other line
// ends it.
func flowItemEnd(s string, pos int) int {
	limit := extentLimit(s, pos)
	indent := -1
	end := pos
	for {
		tick(1)
		i := indexAny(s[end:limit], ",]}\r\n")
		if i < 0 {
			end = limit
			break
		}
		end += i
		if s[end] != '\r' && s[end] != '\n' {
			break
		}
		next := end + 1
		if s[end] == '\r' && next < limit && s[next] == '\n' {
			next++
		}
		if indent < 0 {
			indent = lineIndent(s, pos)
		}
		t := next
		for t < limit && (s[t] == ' ' || s[t] == '\t') {
			tick(1)
			t++
		}
		if t >= limit || s[t] == '\r' || s[t] == '\n' || t-next <= indent {
			break
		}
		end = t
	}
	for end > pos && isSpace(s[end-1]) {
		tick(1)
		end--
	}
	return end
}

// lineItemEnd returns the end of a plain item of a YAML block sequence that starts at pos: the line end,
// within maxValueExtent bytes, with the blanks before it trimmed.
func lineItemEnd(s string, pos int) int {
	limit := extentLimit(s, pos)
	end := limit
	if i := indexAny(s[pos:limit], "\r\n"); i >= 0 {
		end = pos + i
	}
	for end > pos && isSpace(s[end-1]) {
		tick(1)
		end--
	}
	return end
}

// inFlowSequence reports whether the flag that starts at flagStart is an item of a YAML flow sequence: "["
// or "," right before it, after blanks.
func inFlowSequence(s string, flagStart int) bool {
	i := flagStart
	for i > 0 && flagStart-i < maxValueExtent && (s[i-1] == ' ' || s[i-1] == '\t') {
		tick(1)
		i--
	}
	return i > 0 && (s[i-1] == '[' || s[i-1] == ',')
}

// nodeProperties returns the index after the YAML anchor ("&name") and tag ("!!str", "!x") tokens that start
// at pos, at most two, and the blanks after each, when a value follows them on the same line; otherwise pos.
func nodeProperties(s string, pos int) int {
	i := pos
	for range 2 {
		tick(1)
		if i+1 >= len(s) || s[i] != '&' && s[i] != '!' || isSpace(s[i+1]) {
			break
		}
		limit := extentLimit(s, i)
		j := i + 1
		for j < limit && !isSpace(s[j]) {
			tick(1)
			j++
		}
		k := j
		for k < limit && (s[k] == ' ' || s[k] == '\t') {
			tick(1)
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
func listBlockScalarSpan(s string, pos, col int, tm *tokenMemo) (int, int) {
	limit := extentLimit(s, pos)
	i := pos
	for i < limit && s[i] != '\n' {
		tick(1)
		i++
	}
	start, end := -1, -1
	for i < limit && s[i] == '\n' {
		tick(1)
		ls := i + 1
		t := ls
		for t < limit && (s[t] == ' ' || s[t] == '\t') {
			tick(1)
			t++
		}
		le := t
		for le < limit && s[le] != '\n' {
			tick(1)
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
		return pos, tm.tokenEnd(s, pos)
	}
	return start, end
}
