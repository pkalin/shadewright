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

package released

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/redact/meter"
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
func lineColumn(wm *meter.Meter, s string, pos int) int {
	from := max(0, pos-maxValueExtent)
	i := lastIndexByte(wm, s[from:pos], '\n')
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
func lineIndent(wm *meter.Meter, s string, pos int) int {
	col := lineColumn(wm, s, pos)
	if col >= maxValueExtent {
		return maxValueExtent
	}
	start := pos - col
	t := start
	for t < pos && (s[t] == ' ' || s[t] == '\t') {
		tick(wm, 1)
		t++
	}
	return t - start
}

// indentedBeyond reports whether the line that starts at pos, read up to limit, is blank or indented by more
// than col blanks.
func indentedBeyond(wm *meter.Meter, s string, pos, limit, col int) bool {
	t := pos
	for t < limit && (s[t] == ' ' || s[t] == '\t') {
		tick(wm, 1)
		t++
	}
	return t >= limit || s[t] == '\n' || s[t] == '\r' || t-pos > col
}

// extentLimit returns the index the extent rule reads up to from pos.
func extentLimit(_ *meter.Meter, s string, pos int) int {
	return min(len(s), pos+maxValueExtent)
}

// quoteLevel returns the escape level of a double quote preceded by run backslashes: the lowest zero bit of
// run (0 for a plain quote or one after an escaped backslash, 1 for "\"", 2 for "\\\"", as escapedQuotedSpan
// reads it).
func quoteLevel(wm *meter.Meter, run int) int {
	j := 0
	for run&(1<<j) != 0 {
		tick(wm, 1)
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
func groupEnd(wm *meter.Meter, s string, pos, limit, ctx, keyPos int, shell bool) (int, bool) {
	return groupScanEnd(wm, groupScan{s: s, limit: limit, ctx: ctx, shell: shell}, pos, keyPos)
}

// groupScanEnd is groupEnd with the scanner g, whose props field says whether a YAML node property opens a
// scalar inside an array or object (bracketSpan).
func groupScanEnd(wm *meter.Meter, g groupScan, pos, keyPos int) (int, bool) {
	s, limit := g.s, g.limit
	keyCol := -1
	for i := pos; i < limit; i++ {
		tick(wm, 1)
		if s[i] == '\n' && g.quote != 0 && keyPos != noKey {
			if keyCol < 0 {
				keyCol = lineColumn(wm, s, keyPos)
			}
			if !indentedBeyond(wm, s, i+1, limit, keyCol) {
				return 0, false
			}
		}
		var st scanStep
		switch g.quote {
		case '\'':
			i, st = g.singleQuoted(wm, i)
		case '"':
			i, st = g.doubleQuoted(wm, i)
		default:
			i, st = g.unquoted(wm, i)
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
}

// shellText reports whether the byte read now is shell text: inside "$(" or backticks, or in a group that
// is a shell word and holds no array or object.
func (g *groupScan) shellText(wm *meter.Meter) bool {
	return g.inShell(wm) || g.depth == 0 && g.shell
}

// scalarStart reports whether a quote read now inside an array or object opens a string: it does at the
// start of a scalar only, after "[", "{", "," or ":" and blanks.
func (g *groupScan) scalarStart(wm *meter.Meter) bool {
	return g.shellText(wm) || g.afterProp || strings.IndexByte("[{,:", g.prev) >= 0
}

// push opens a nested group whose closer is c, and reports false when that nests too deep.
func (g *groupScan) push(_ *meter.Meter, c byte) scanStep {
	if g.depth == len(g.stack) {
		return scanFailed
	}
	g.stack[g.depth] = c
	g.depth++
	return scanGoOn
}

// inShell reports whether the innermost open group is a command substitution ("$(", "(" inside it, or
// backticks), where brackets and braces are plain text.
func (g *groupScan) inShell(wm *meter.Meter) bool {
	switch g.top(wm) {
	case ')', substInQuotes, '`', backtickInQuotes:
		return true
	}
	return false
}

// top returns the closer of the innermost open group, or 0.
func (g *groupScan) top(_ *meter.Meter) byte {
	if g.depth == 0 {
		return 0
	}
	return g.stack[g.depth-1]
}

// closeQuote ends the open quoted string; the group is closed when no bracket is open.
func (g *groupScan) closeQuote(_ *meter.Meter) scanStep {
	g.quote = 0
	g.prev = '"'
	if g.depth == 0 {
		return scanClosed
	}
	return scanGoOn
}

// singleQuoted reads the byte at i inside a single-quoted string: only a single quote ends it, and a doubled one stays inside.
func (g *groupScan) singleQuoted(wm *meter.Meter, i int) (int, scanStep) {
	if g.s[i] != '\'' {
		return i, scanGoOn
	}
	if i+1 < g.limit && g.s[i+1] == '\'' {
		return i + 1, scanGoOn
	}
	return i, g.closeQuote(wm)
}

// doubleQuoted reads the byte at i inside a double-quoted string.
func (g *groupScan) doubleQuoted(wm *meter.Meter, i int) (int, scanStep) {
	c := g.s[i]
	switch {
	case c == '\\':
		g.run++
		return i, scanGoOn
	case c == '$' && g.qShell && g.run%2 == 0 && g.qLevel == 0 && i+1 < g.limit && g.s[i+1] == '(':
		// A command substitution inside double quotes: its text is read afresh, its own quotes included,
		// and the string goes on after its ")".
		g.quote, g.run = 0, 0
		return i + 1, g.push(wm, substInQuotes)
	case c == '`' && g.qShell && g.run%2 == 0 && g.qLevel == 0:
		// A backtick substitution inside double quotes, read as "$(" above.
		g.quote, g.run = 0, 0
		return i, g.push(wm, backtickInQuotes)
	case c == '"':
		j := quoteLevel(wm, g.run)
		g.run = 0
		switch {
		case j == g.qLevel:
			return i, g.closeQuote(wm)
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
func (g *groupScan) unquoted(wm *meter.Meter, i int) (int, scanStep) {
	c := g.s[i]
	startsProp := g.props && (c == '!' || c == '&') && g.run == 0 && g.depth > 0 && !g.inShell(wm) && !g.inProp && g.scalarStart(wm)
	j, st := g.unquotedByte(wm, i)
	switch {
	case isSpace(wm, c):
		if g.inProp {
			g.inProp, g.afterProp = false, true
		}
	case startsProp:
		g.inProp, g.afterProp = true, false
	case strings.IndexByte(",[]{}", c) >= 0 || !g.inProp:
		g.inProp, g.afterProp = false, false
	}
	if g.quote == 0 && c != '\\' && !isSpace(wm, c) {
		g.prev = c
	}
	return j, st
}

// unquotedByte is unquoted without the record of prev.
func (g *groupScan) unquotedByte(wm *meter.Meter, i int) (int, scanStep) {
	c := g.s[i]
	run := g.run
	g.run = 0
	switch {
	case c == '\\':
		g.run = run + 1
	case run%2 == 1 && g.shellText(wm) && strings.IndexByte("()$`", c) >= 0:
		// Escaped in a shell: plain text ("\)" does not close "$(").
	case c == '"' || c == '\'' && run == 0:
		return i, g.openQuote(wm, c, run)
	case (c == '[' || c == '{' || c == ']' || c == '}') && g.inShell(wm):
		// Plain text in a shell: "${x}", "a[1".
	case c == '[':
		return i, g.push(wm, ']')
	case c == '{':
		return i, g.push(wm, '}')
	case c == '$' || c == '(' || c == '`' && g.top(wm) != '`' || c == ')' && g.top(wm) == substInQuotes:
		// Shell groups; outside a shell these bytes are plain text.
		if j, st, ok := g.shellByte(wm, i); ok {
			return j, st
		}
	case g.depth > 0 && c == g.top(wm):
		g.depth--
		if g.depth == 0 {
			return i, scanClosed
		}
	case c == ']' || c == '}' || c == ')' && g.shellText(wm):
		// A closer that does not match; outside a shell, "(" and ")" are plain text.
		return i, scanFailed
	}
	return i, scanGoOn
}

// shellByte reads "$(", "(", ")" or a backtick at i that opens or closes a shell group: "$(" and a backtick in
// shell text, "(" inside "$(", and the ")" or backtick that ends a substitution inside a double-quoted
// string, which goes on. ok is false when the byte does none of these.
func (g *groupScan) shellByte(wm *meter.Meter, i int) (int, scanStep, bool) {
	switch c := g.s[i]; {
	case c == '$' && i+1 < g.limit && g.s[i+1] == '(' && g.shellText(wm):
		return i + 1, g.push(wm, ')'), true
	case c == ')' && g.top(wm) == substInQuotes, c == '`' && g.top(wm) == backtickInQuotes:
		g.depth--
		g.quote, g.qLevel, g.qShell = '"', 0, true
		return i, scanGoOn, true
	case c == '(' && g.inShell(wm):
		return i, g.push(wm, ')'), true
	case c == '`' && g.top(wm) != '`' && g.shellText(wm):
		return i, g.push(wm, '`'), true
	}
	return i, scanGoOn, false
}

// openQuote reads a quote outside any quoted string, preceded by run backslashes: a double quote escaped
// less than the context ends an enclosing string, so the group fails; inside an array or object a quote
// opens a string only at the start of a scalar; otherwise it opens one.
func (g *groupScan) openQuote(wm *meter.Meter, c byte, run int) scanStep {
	j := 0
	if c == '"' {
		j = quoteLevel(wm, run)
		if j < g.ctx {
			return scanFailed
		}
	}
	if g.depth > 0 && !g.scalarStart(wm) {
		return scanGoOn
	}
	g.quote, g.qLevel, g.qShell = c, j, g.shellText(wm)
	return scanGoOn
}

// opensGroup reports whether a quoted group of groupEnd opens at pos: a plain quote, "$(" or a backtick.
// Arrays and objects are read by bracketSpan.
func opensGroup(_ *meter.Meter, s string, pos int) bool {
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
func shellWordEnd(wm *meter.Meter, s string, pos, limit, keyPos int, flag, wide bool) (end int, last bool, ok bool) {
	end, ok = groupEnd(wm, s, pos, limit, 0, keyPos, true)
	if !ok {
		return 0, false, false
	}
	last = s[pos] == '"' || s[pos] == '\''
	for end < limit {
		tick(wm, 1)
		c := s[end]
		if opensGroup(wm, s, end) {
			e, closed := groupEnd(wm, s, end, limit, 0, keyPos, true)
			if !closed {
				break
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
				tick(wm, 1)
				j++
			}
			if j < limit && !isSpace(wm, s[j]) && strings.IndexByte(wordStops+"\"'`$", s[j]) < 0 {
				last = false
				end = j
				continue
			}
			break
		}
		if isSpace(wm, c) || strings.IndexByte(wordStops, c) >= 0 && (c != ',' || !flag && (!wide || fieldAfterComma(wm, s, end, limit))) {
			break
		}
		last = false
		end++
	}
	return end, last, true
}

// fieldAfterComma reports whether the comma at i, glued to a shell word after a key name, starts the next
// field of a list rather than more of the value (#769): it is followed by whitespace, the end of the input, a
// quote or a separator, by a secret key name the key rules match on their own, with blanks, "=", ":" or ":="
// after it ("password=\"abc\",DB_PASSWORD= \"x y\"", whose value is then redacted as on main), or by a name
// and "=" or ":" ("password=\"abc\",user=bob", ",next: x"). Any other name and "=" followed by whitespace, the
// end or another "=" is base64 padding, not a field ("\"a\",abc=").
func fieldAfterComma(wm *meter.Meter, s string, i, limit int) bool {
	j := i + 1
	if j >= limit || isSpace(wm, s[j]) || strings.IndexByte("\"'`,;|&<>(){}[]", s[j]) >= 0 {
		return true
	}
	if commaSecretKey.MatchString(counted(wm, s[j:commaKeyReach(wm, s, j, limit)])) {
		return true
	}
	for j < limit && (isAlnum(wm, s[j]) || strings.IndexByte("_.-", s[j]) >= 0) {
		tick(wm, 1)
		j++
	}
	switch {
	case j == i+1 || j >= limit:
		return false
	case s[j] == ':':
		return true
	case s[j] == '=':
		return j+1 < limit && !isSpace(wm, s[j+1]) && s[j+1] != '='
	}
	return false
}

// commaSecretKey is a secret key name at the start of the text after a comma, followed by what the key rules
// read as its separator: "=", ":" or ":=" after optional blanks, or blanks (a compound name).
var commaSecretKey = regexp.MustCompile(`(?i)^(?:` + bareKeyExpr + `)(?:[ \t]*[:=]|[ \t]+)`)

// commaKeyReach returns the end of the text from j that commaSecretKey can read, within limit: the run of the
// characters a key name of bareKeyExpr is made of (ASCII letters, digits, "_", "." and "-", and the two
// characters outside ASCII that a case-insensitive match reads as one of them, KELVIN SIGN for "k" and LONG S for
// "s"), the blanks after it, and one byte more for its separator. A match starts at j and cannot read past it, so
// the expression decides the same on that text as on the rest of the value up to limit; handing it the rest made
// the reading of every comma of a long value cost up to maxValueExtent steps (#873).
func commaKeyReach(wm *meter.Meter, s string, j, limit int) int {
	k := j
	for k < limit {
		tick(wm, 1)
		if c := s[k]; isAlnum(wm, c) || c == '_' || c == '.' || c == '-' {
			k++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[k:limit])
		if r != '\u212a' && r != '\u017f' {
			break
		}
		k += size
	}
	for k < limit && (s[k] == ' ' || s[k] == '\t') {
		tick(wm, 1)
		k++
	}
	return min(k+1, limit)
}

// plainWordEnd returns the end of a shell word that starts with plain text at pos (no quote, "$(" or
// backtick), when a "$(" or backtick group or an escaped blank is glued to that text ("abc$(cat pw)",
// "a,b\\ c"): the word runs on through those groups, plain text and backslash escapes, and ends at
// whitespace, one of ";|&<>()", a quote, or a group that does not close. A quote ends it because an unquoted
// value in free text that meets a quote is often inside a JSON or logfmt string, whose fields after it stay
// (tokenEnd). It returns pos when no group or escaped blank is glued to the text. With wide false any quote
// ends it, as on main before #769 (keyValueSpans). pr, unless nil, is the plain run read before in the same
// pass over s: a read that starts inside it goes on from its end (#962).
func plainWordEnd(wm *meter.Meter, s string, pos, limit, keyPos int, wide bool, pr *plainRun) int {
	end, glued := pos, false
	// s[run:end] holds only bytes the loop steps over by its last case whatever pos, limit, keyPos and wide are.
	run := pos
	if pr != nil && pr.from <= pos && pos < pr.to {
		tick(wm, 1)
		end, run = min(pr.to, limit), pr.from
	}
loop:
	for end < limit {
		tick(wm, 1)
		c := s[end]
		switch {
		case c == '$' && end+1 < limit && s[end+1] == '(', c == '`':
			e, ok := groupEnd(wm, s, end, limit, 0, keyPos, true)
			if !ok {
				break loop
			}
			end, glued, run = e, true, e
		case c == '\\' && end+1 < limit && (s[end+1] == ' ' || s[end+1] == '\t'):
			end, glued = end+2, true
			run = end
		case c == '\\' && end+1 < limit && !isSpace(wm, s[end+1]) && s[end+1] != '"' && s[end+1] != '\'':
			end += 2
			run = end
		case wide && (c == '"' || c == '\'') && end > pos:
			e := gluedQuoteEnd(wm, s, end, limit, keyPos)
			if e < 0 {
				break loop
			}
			end, glued, run = e, true, e
		case isSpace(wm, c), c == '"', c == '\'', c == '\\', strings.IndexByte(";|&<>()", c) >= 0:
			break loop
		default:
			end++
			if c == '$' {
				// A "$" is plain here only because "(" does not follow it before limit: a read with a
				// larger limit may not step over it.
				run = end
			}
		}
	}
	pr.extend(run, end)
	return wordEnd(wm, pos, end, glued)
}

// plainRun is the run of plain text that reaches furthest of those plainWordEnd read in one pass of a rule over
// one text: s[from:to] holds only bytes its loop steps over as plain text whatever the start, limit, key and
// width of the read, so a read that starts inside the run would step over the rest of it too and goes on from
// to at once (up to its own limit). Without it a run of flags glued with no blank ("--token,}" repeated, #962)
// read up to maxValueExtent bytes per flag. It is package redact's memory of the same name (#873); the ends found
// are the same as without it (TestPlainWordEndMemoMatches).
type plainRun struct {
	from, to int
}

// extend keeps s[from:to] when it reaches further than the run kept; on a nil plainRun it does nothing.
func (pr *plainRun) extend(from, to int) {
	if pr != nil && to > pr.to && from < to {
		pr.from, pr.to = from, to
	}
}

// gluedQuoteEnd returns the end of a quoted piece that opens at q, glued to plain text before it ("ab\"c d\"",
// #769), or -1 when the quote may close an enclosing JSON, YAML or logfmt string instead: the byte after it is
// whitespace, the end of the input, a quote, a backslash or one of ",;:)]}", or the piece does not close on
// its line within the bound.
func gluedQuoteEnd(wm *meter.Meter, s string, q, limit, keyPos int) int {
	if q+1 >= limit || isSpace(wm, s[q+1]) || strings.IndexByte(",;:)]}\"'\\", s[q+1]) >= 0 {
		return -1
	}
	e, ok := groupEnd(wm, s, q, limit, 0, keyPos, true)
	if !ok || containsAny(wm, s[q:e], "\r\n") {
		return -1
	}
	return e
}

// wordEnd returns end when glued, else pos.
func wordEnd(_ *meter.Meter, pos, end int, glued bool) int {
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
func unclosedQuoteEnd(wm *meter.Meter, s string, pos int) int {
	end := lineItemEnd(wm, s, pos)
	if s[pos] == '\'' && indexByte(wm, s[pos:end], '"') >= 0 {
		return 0
	}
	for i := pos + 1; i+1 < end; i++ {
		tick(wm, 1)
		if (s[i] == ' ' || s[i] == '\t') && s[i+1] == '-' {
			end = i
			break
		}
	}
	for end > pos && isSpace(wm, s[end-1]) {
		tick(wm, 1)
		end--
	}
	return end
}

// quotedWordSpan returns the value of a shell word that opens with a group at pos, for a value after a key
// name: for a word that opens with a quote, the text after that quote up to the closing quote of the last
// quoted piece, or the word's end; for one that opens with "$(" or a backtick, the whole word. ok is false
// when the first group does not close within maxValueExtent. keyPos is as in groupEnd, flag and wide as in
// shellWordEnd.
func quotedWordSpan(wm *meter.Meter, s string, pos, keyPos int, flag, wide bool) (int, int, bool) {
	end, last, ok := shellWordEnd(wm, s, pos, extentLimit(wm, s, pos), keyPos, flag, wide)
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
func bracketSpan(wm *meter.Meter, s string, pos, ctx int, wide bool) (int, int, bool) {
	if pos >= len(s) || s[pos] != '[' && s[pos] != '{' {
		return 0, 0, false
	}
	limit := extentLimit(wm, s, pos)
	// A quoted scalar behind a YAML node property is read as a string (#769) in a second scan; the value is
	// the longer of the two scans that close, so it is never shorter than without the property rule.
	end, ok := groupEnd(wm, s, pos, limit, ctx, noKey, false)
	if !wide {
		return pos, end, ok
	}
	if pe, pok := groupScanEnd(wm, groupScan{s: s, limit: limit, ctx: ctx, props: true}, pos, noKey); pok {
		end, ok = max(end, pe), true
	}
	return pos, end, ok
}

// commentEnd returns the index after the JSON comments ("// ..." to the line end, "/* ... */") and the
// whitespace after them that start at pos, or pos when none starts there. It reads at most maxValueExtent
// bytes; a comment that does not end by then is not taken, nor are the comments after it, so an unclosed
// "/*" does not extend the value.
func commentEnd(wm *meter.Meter, s string, pos int) int {
	limit := extentLimit(wm, s, pos)
	i := pos
	for i+1 < limit && s[i] == '/' && (s[i+1] == '/' || s[i+1] == '*') {
		tick(wm, 1)
		if s[i+1] == '/' {
			j := indexByte(wm, s[i:limit], '\n')
			if j < 0 {
				if limit < len(s) {
					return i
				}
				return limit
			}
			i += j
		} else {
			j := index(wm, s[i+2:limit], "*/")
			if j < 0 {
				return i
			}
			i += j + 4
		}
		for i < limit && isSpace(wm, s[i]) {
			tick(wm, 1)
			i++
		}
	}
	return i
}

// flowItemEnd returns the end of a plain item of a YAML flow sequence that starts at pos: the next ",", "]" or
// "}", within maxValueExtent bytes, with the blanks before it trimmed. The item goes on over a line end to a
// line indented deeper than the line it starts on, as a YAML flow scalar does; a blank line or any other line
// ends it.
func flowItemEnd(wm *meter.Meter, s string, pos int) int {
	limit := extentLimit(wm, s, pos)
	indent := -1
	end := pos
	for {
		tick(wm, 1)
		i := indexAny(wm, s[end:limit], ",]}\r\n")
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
			indent = lineIndent(wm, s, pos)
		}
		t := next
		for t < limit && (s[t] == ' ' || s[t] == '\t') {
			tick(wm, 1)
			t++
		}
		if t >= limit || s[t] == '\r' || s[t] == '\n' || t-next <= indent {
			break
		}
		end = t
	}
	for end > pos && isSpace(wm, s[end-1]) {
		tick(wm, 1)
		end--
	}
	return end
}

// lineItemEnd returns the end of a plain item of a YAML block sequence that starts at pos: the line end,
// within maxValueExtent bytes, with the blanks before it trimmed.
func lineItemEnd(wm *meter.Meter, s string, pos int) int {
	limit := extentLimit(wm, s, pos)
	end := limit
	if i := indexAny(wm, s[pos:limit], "\r\n"); i >= 0 {
		end = pos + i
	}
	for end > pos && isSpace(wm, s[end-1]) {
		tick(wm, 1)
		end--
	}
	return end
}

// inFlowSequence reports whether the flag that starts at flagStart is an item of a YAML flow sequence: "["
// or "," right before it, after blanks.
func inFlowSequence(wm *meter.Meter, s string, flagStart int) bool {
	i := flagStart
	for i > 0 && flagStart-i < maxValueExtent && (s[i-1] == ' ' || s[i-1] == '\t') {
		tick(wm, 1)
		i--
	}
	return i > 0 && (s[i-1] == '[' || s[i-1] == ',')
}

// nodeProperties returns the index after the YAML anchor ("&name") and tag ("!!str", "!x") tokens that start
// at pos, at most two, and the blanks after each, when a value follows them on the same line; otherwise pos.
func nodeProperties(wm *meter.Meter, s string, pos int) int {
	i := pos
	for range 2 {
		tick(wm, 1)
		if i+1 >= len(s) || s[i] != '&' && s[i] != '!' || isSpace(wm, s[i+1]) {
			break
		}
		limit := extentLimit(wm, s, i)
		j := i + 1
		for j < limit && !isSpace(wm, s[j]) {
			tick(wm, 1)
			j++
		}
		k := j
		for k < limit && (s[k] == ' ' || s[k] == '\t') {
			tick(wm, 1)
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
func listBlockScalarSpan(wm *meter.Meter, s string, pos, col int, tm *tokenMemo) (int, int) {
	limit := extentLimit(wm, s, pos)
	i := pos
	for i < limit && s[i] != '\n' {
		tick(wm, 1)
		i++
	}
	start, end := -1, -1
	for i < limit && s[i] == '\n' {
		tick(wm, 1)
		ls := i + 1
		t := ls
		for t < limit && (s[t] == ' ' || s[t] == '\t') {
			tick(wm, 1)
			t++
		}
		le := t
		for le < limit && s[le] != '\n' {
			tick(wm, 1)
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
		return pos, tm.tokenEnd(wm, s, pos)
	}
	return start, end
}
