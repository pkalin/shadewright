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
	"strings"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// Short password flags (#1075, decision of 2026-11-22). A one-letter flag means a password for some tools and a
// port or something else for others ("mysql -pX" against "psql -p 5432"), so the command word decides, from a
// fixed table (shortFlagTools): the value of the tool's password flag is hidden, and any other short flag keeps
// its value. In arguments with no command word (a container's "args" with the entrypoint in its image, a sequence
// whose first item is a flag) the value after "-p", "-P" or "-a", and the part after the colon of the value of
// "-u", are hidden unless they are a whole number from 1 to 65535. The shape of a value never decides on its own.
//
// The reading is one pass over the text by words (shortFlagRead). A word whose base name (after the last "/", a
// trailing ".exe" dropped) is a tool of the table is the command word of the words after it, up to the end of
// its command: a shell control character (";", "|", "&", "(", ")", "`", "{", "}"), a bracket, or a line end
// outside a sequence and outside a line continuation. So a tool after "sh -c", "env VAR=x", a path or any other
// word counts as well, and a tool word in the middle of another command makes the flags after it count as its
// own (accepted over-redaction: "ssh mongo -p 22" hides "22"). A word that is not in the table never ends a
// command: a host name such as "docker" could do that.
//
// A sequence is a JSON or YAML flow array ("[" to "]", line ends inside it included), a YAML block sequence (the
// "- " items of consecutive lines), or the strings Strings is given. A sequence has no command word when it is
// the value of an "args" key (JSON, YAML block or flow) with no "command" sequence before it in the same object
// (startSequence), or when its first item is a flag; a free line never has that reading, as "-p 5432" in prose
// has no command word and no sequence to say whose arguments these are.

// shortFlagTool is a row of the table: the letter of the tool's password flag, whether the tool reads the password
// only glued to it ("-pX": "-p X" is a prompt and an argument), and whether the password is the part of the
// value after its first colon ("user:pass").
type shortFlagTool struct {
	letter byte
	glued  bool
	colon  bool
}

// shortFlagTools is the table of the decision, by command word.
var shortFlagTools = map[string]shortFlagTool{
	"mysql": {letter: 'p', glued: true}, "mariadb": {letter: 'p', glued: true},
	"mongo": {letter: 'p'}, "mongosh": {letter: 'p'}, "cqlsh": {letter: 'p'}, "rabbitmqadmin": {letter: 'p'},
	"sshpass":   {letter: 'p'},
	"redis-cli": {letter: 'a'},
	"sqlcmd":    {letter: 'P'}, "mosquitto_pub": {letter: 'P'}, "mosquitto_sub": {letter: 'P'},
	"curl": {letter: 'u', colon: true},
}

// shortArgsLetters are the flags whose value is hidden in arguments with no command word; "u" by the part of its
// value after the colon.
const shortArgsLetters = "pPau"

// shortFlagScan is the rule's scan: the text as written.
func shortFlagScan(wm *meter.Meter, s string, add func(start, end int)) {
	shortFlagRead(wm, s, false, add)
}

// shortFlagSequence reads s as the strings of a sequence joined by line breaks (Strings): a line break ends no
// command, and the first string decides whether the sequence has a command word.
func shortFlagSequence(wm *meter.Meter, s string, rule int) []span {
	var out []span
	shortFlagRead(wm, s, true, func(start, end int) {
		if end > start {
			out = append(out, span{start: start, end: end, kind: KindCredential, rule: rule})
		}
	})
	return out
}

// shortReader is the state of one pass of shortFlagRead.
type shortReader struct {
	s    string
	add  func(start, end int)
	tm   *tokenMemo
	tool shortFlagTool
	// hasTool: a command word of the table stands before, in this command.
	hasTool bool
	// args: the words are arguments with no command word.
	args bool
	// first: the next word is the first item of a sequence.
	first bool
	// lines: a line break ends no command (Strings).
	lines bool
	depth int
	// yaml: inside a YAML block sequence.
	yaml bool
	// command: the sequence read is the value of a "command" key. pending: one ended in this object, with the
	// command word of the table it held (pendingTool, pendingHas), for the "args" sequence after it.
	command, pending, pendingHas bool
	pendingTool                  shortFlagTool
	// The last two words read, as byte ranges, to find the key before a sequence.
	prev, prev2 [2]int
}

// end ends the command: no command word, no sequence item pending.
func (r *shortReader) end() { r.hasTool, r.args, r.first = false, false, false }

// keyIs reports whether the last words read are the key name: "name:", or "name" and ":".
func (r *shortReader) keyIs(name string) bool {
	w := r.s[r.prev[0]:r.prev[1]]
	if len(w) == len(name)+1 && w[len(name)] == ':' && strings.EqualFold(w[:len(name)], name) {
		return true
	}
	return w == ":" && strings.EqualFold(r.s[r.prev2[0]:r.prev2[1]], name)
}

// startSequence starts a sequence. The value of an "args" key has no command word, unless a "command" sequence
// came before it in the same object: then the command word of that one, if it held one of the table, is its own
// (a container's command and arguments, as get_resource writes them). Any other sequence starts with its first
// item, which decides.
func (r *shortReader) startSequence() {
	args, command := r.keyIs("args"), r.keyIs("command")
	r.end()
	r.command = command
	switch {
	case args && r.pending:
		r.hasTool, r.tool = r.pendingHas, r.pendingTool
	case args:
		r.args = true
	default:
		r.first = true
	}
	r.pending = false
}

// endSequence ends a sequence, and keeps the command word of a "command" sequence for the "args" after it.
func (r *shortReader) endSequence() {
	if r.command {
		r.pending, r.pendingHas, r.pendingTool = true, r.hasTool, r.tool
	}
	r.command = false
	r.end()
}

// shortFlagRead is the one pass of the rule over s; with lines set s is a sequence of strings joined by line
// breaks. Each byte is read a bounded number of times: once as a word or a separator, once more as the
// indentation of a line looked at for a YAML item, and the values of the flags by flagExtent, after which the
// pass goes on from the end of the value.
func shortFlagRead(wm *meter.Meter, s string, lines bool, add func(start, end int)) {
	r := &shortReader{s: s, add: add, tm: &tokenMemo{}, lines: lines, first: lines}
	for i := 0; i < len(s); {
		tick(wm, 1)
		switch c := s[i]; {
		case c == '\n' || c == '\r':
			r.lineBreak(wm, i)
			i++
		case c == '{' || c == '}':
			r.end()
			r.pending = false
			i++
		case strings.IndexByte(";|&()`", c) >= 0:
			r.end()
			i++
		case c == '[':
			r.depth++
			r.startSequence()
			i++
		case c == ']':
			r.depth = max(r.depth-1, 0)
			r.endSequence()
			i++
		case shortDelimiter(wm, c):
			i++
		default:
			j := i
			for j < len(s) && !shortDelimiter(wm, s[j]) && strings.IndexByte(";|&()`{}[]", s[j]) < 0 {
				tick(wm, 1)
				j++
			}
			i = r.word(wm, i, j)
		}
	}
}

// shortDelimiter reports whether c ends a word without ending a command: a blank, a quote, a comma, a
// backslash or a redirection.
func shortDelimiter(wm *meter.Meter, c byte) bool {
	return isSpace(wm, c) || strings.IndexByte(`"',\<>`, c) >= 0
}

// lineBreak reads the line break at i: inside a sequence, after a line continuation or before the next item of a
// YAML block sequence it ends no command; before the first item of one it starts the sequence (with no command
// word after an "args:" key); any other ends the command.
func (r *shortReader) lineBreak(wm *meter.Meter, i int) {
	s := r.s
	if r.lines || r.depth > 0 {
		return
	}
	if s[i] == '\n' && i > 0 && s[i-1] == '\r' {
		return // the second byte of CRLF, read with the first
	}
	if b := i - 1; b >= 0 && s[b] == '\\' {
		return // a line continuation
	}
	k := i + 1
	if s[i] == '\r' && k < len(s) && s[k] == '\n' {
		k++
	}
	for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
		tick(wm, 1)
		k++
	}
	item := k < len(s) && s[k] == '-' && (k+1 == len(s) || isSpace(wm, s[k+1]))
	switch {
	case item && r.yaml:
	case item:
		r.startSequence()
		r.yaml = true
	case r.yaml:
		r.endSequence()
		r.yaml = false
	default:
		r.end()
	}
}

// word reads the word s[i:j] and returns where the pass goes on: after the word, or after the value of a flag.
func (r *shortReader) word(wm *meter.Meter, i, j int) int {
	w := r.s[i:j]
	r.prev2, r.prev = r.prev, [2]int{i, j}
	if w == "-" {
		return j // the dash of a YAML block sequence item
	}
	if r.first {
		r.first = false
		if len(w) > 1 && w[0] == '-' && (isLetter(wm, w[1]) || w[1] == '-') {
			r.args = true
		}
	}
	if t, ok := shortToolOf(w); ok {
		r.tool, r.hasTool = t, true
		return j
	}
	if len(w) < 2 || w[0] != '-' || !isLetter(wm, w[1]) || !r.hasTool && !r.args {
		return j
	}
	return max(j, r.flag(wm, i, j))
}

// shortToolOf returns the row of the table of the command word w: its base name, a trailing ".exe" dropped.
func shortToolOf(w string) (shortFlagTool, bool) {
	if k := strings.LastIndexByte(w, '/'); k >= 0 {
		w = w[k+1:]
	}
	if len(w) > 4 && strings.EqualFold(w[len(w)-4:], ".exe") {
		w = w[:len(w)-4]
	}
	t, ok := shortFlagTools[w]
	return t, ok
}

// flag reads the flag word s[i:j] (a dash and a letter) and hides the value of a password flag in it; it returns
// the end of the furthest value read, or j.
func (r *shortReader) flag(wm *meter.Meter, i, j int) int {
	s, end := r.s, j
	if r.hasTool {
		// The tool's letter among the letters after the dash: combined flags ("-vpX") before it take no value.
		k := i + 1
		for k < j && isLetter(wm, s[k]) && s[k] != r.tool.letter {
			tick(wm, 1)
			k++
		}
		if k < j && s[k] == r.tool.letter {
			if vs, ve, glued, ok := r.value(wm, i, k+1, j); ok && (glued || !r.tool.glued) {
				if r.tool.colon {
					vs = afterColon(wm, s, vs, ve)
				}
				r.add(vs, ve)
				end = max(end, ve)
			}
		}
	}
	if r.args && strings.IndexByte(shortArgsLetters, s[i+1]) >= 0 && !longFlagName(wm, s[i:j]) {
		if vs, ve, _, ok := r.value(wm, i, i+2, j); ok {
			if s[i+1] == 'u' {
				vs = afterColon(wm, s, vs, ve)
			}
			if !portNumber(wm, s[vs:ve]) {
				r.add(vs, ve)
			}
			end = max(end, ve)
		}
	}
	return end
}

// longFlagName reports whether the flag word w is a flag of a longer name with one dash, as Go programs take
// them ("-password", "-port=8080", "-alsologtostderr"): a dash, then before any "=" two or more bytes of
// lower-case letters, digits, dashes and characters beyond ASCII (an invisible character inside a name, which
// the folded view drops: "-pass\u200bword"). In arguments with no command word such a word is that flag, not "-p"
// with a value glued to it: "-pS3cret" is the flag "-p", "-port" the flag "-port".
func longFlagName(wm *meter.Meter, w string) bool {
	name, _, _ := strings.Cut(w[1:], "=")
	if len(name) < 2 {
		return false
	}
	for k := range len(name) {
		tick(wm, 1)
		if c := name[k]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c < utf8.RuneSelf {
			return false
		}
	}
	return true
}

// value returns the value of the flag that starts at i, whose letters end at p, in the word that ends at j: glued
// to it ("-pX"), after "=" ("-p=X"), or after a separator (shortSeparatorEnd) as a word of its own. The value is
// the flag rule's (flagExtent): a quoted value, a shell word or an item of a sequence. ok is false when there is
// none.
func (r *shortReader) value(wm *meter.Meter, i, p, j int) (start, end int, glued, ok bool) {
	s := r.s
	from := p
	switch {
	case p < j && s[p] == '=':
		from, glued = p+1, true
	case p < j:
		glued = true
	default:
		from = shortSeparatorEnd(wm, s, p)
		if from == p || from >= len(s) {
			return 0, 0, false, false
		}
	}
	q := strings.TrimLeft(s[p:from], `\`)
	closed := q != "" && (q[0] == '"' || q[0] == '\'')
	start, end = flagExtent(wm, s, []int{i, from, i + 1, p}, closed, r.tm)
	return start, end, glued, end > start
}

// shortSeparatorEnd returns the end of the separator between a flag that ends at p and a value of its own, as the
// flag rule reads one (flagSeparatorExpr), or p when there is none: a comma right after the flag or its closing
// quote then blanks, or the closing quote and blanks, or blanks alone; a blank is a space, a tab, a line end
// (with a YAML list dash after its indentation) or a backslash before a line end.
func shortSeparatorEnd(wm *meter.Meter, s string, p int) int {
	k := p
	for k < len(s) && s[k] == '\\' {
		tick(wm, 1)
		k++
	}
	if k < len(s) && (s[k] == '"' || s[k] == '\'') {
		if k+1 < len(s) && s[k+1] == ',' {
			return shortBlanksEnd(wm, s, k+2)
		}
		if e := shortBlanksEnd(wm, s, k+1); e > k+1 {
			return e
		}
		return p
	}
	if p < len(s) && s[p] == ',' {
		return shortBlanksEnd(wm, s, p+1)
	}
	return shortBlanksEnd(wm, s, p)
}

// shortBlanksEnd returns the end of the blanks of flagSpaceExpr from k.
func shortBlanksEnd(wm *meter.Meter, s string, k int) int {
	for k < len(s) {
		tick(wm, 1)
		switch c := s[k]; {
		case c == ' ' || c == '\t' || c == '\v' || c == '\f':
			k++
		case c == '\r' || c == '\n':
			k++
			if c == '\r' && k < len(s) && s[k] == '\n' {
				k++
			}
			d := k
			for d < len(s) && (s[d] == ' ' || s[d] == '\t') {
				tick(wm, 1)
				d++
			}
			if d+1 < len(s) && s[d] == '-' && (s[d+1] == ' ' || s[d+1] == '\t') {
				k = d + 2
			}
		case c == '\\' && k+1 < len(s) && (s[k+1] == '\n' || s[k+1] == '\r'):
			k += 2
			if s[k-1] == '\r' && k < len(s) && s[k] == '\n' {
				k++
			}
		default:
			return k
		}
	}
	return k
}

// afterColon returns where the part of the value from start to end after its first colon starts, or end when
// it has none.
func afterColon(wm *meter.Meter, s string, start, end int) int {
	for k := start; k < end; k++ {
		tick(wm, 1)
		if s[k] == ':' {
			return k + 1
		}
	}
	return end
}

// portNumber reports whether v, without the quotes and backslashes around it, is a whole number from 1 to 65535
// written with no leading zero.
func portNumber(wm *meter.Meter, v string) bool {
	v = strings.Trim(v, `"'\`)
	if v == "" || len(v) > 5 || v[0] == '0' {
		return false
	}
	n := 0
	for k := range len(v) {
		tick(wm, 1)
		if v[k] < '0' || v[k] > '9' {
			return false
		}
		n = n*10 + int(v[k]-'0')
	}
	return n <= 65535
}
