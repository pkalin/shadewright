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
// "- " items of consecutive lines, a block scalar item's content lines and comment lines among them included), or
// the strings Strings is given. A sequence has no command word when it is the value of an "args" key (JSON, YAML
// block or flow, or one string) whose object has no "command" key, before or after it (startSequence,
// shortObjects), or when its first item is a flag; a free line never has that reading, as "-p 5432" in prose has
// no command word and no sequence to say whose arguments these are.

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
	"sqlcmd":    {letter: 'P'}, "mosquitto_pub": {letter: 'P'}, "mosquitto_sub": {letter: 'P'}, //nolint:misspell // the MQTT clients' names
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
	// command: the sequence read is the value of a "command" key.
	command bool
	// itemCol: the column of the dash of the last YAML block sequence item. scalar: the lines read are the
	// content of a block scalar item ("- >-" or "- |", then lines further right than its dash).
	itemCol int
	scalar  bool
	// header: the line read is an item of a YAML block sequence whose value is a block scalar ("- >-").
	header bool
	// saved: the state before the comment line being read, taken up again at its end (resume).
	saved *shortReader
	// objects: what decides each "args" key, by the start of the key (shortObjects), read once, at the first
	// "args" sequence (objectsRead), so a text with none pays nothing for it. commands: the command word of the
	// table each "command" key held, by the start of the key, read once (commandRow).
	objects     map[int]argsRow
	objectsRead bool
	commands    map[int]commandWord
	// probe: the pass reads the value of one "command" key only (commandRow): it adds nothing, and stops (done)
	// at the end of that value with the command word it held (found).
	probe, done bool
	found       commandWord
	wm          *meter.Meter
	// skip: the comment lines a flag looked over for its value, shared by the saved states (shortBlanksEnd).
	skip *commentSkip
	keyWords
}

// commentSkip is the last run of comment lines shortBlanksEnd looked over: the line starts from from to to were all
// read by it, and the reading from any of them goes on at the line start to, so a later flag whose separator
// reaches one of them goes on at to without reading the lines again. Without it, a flag with no value on each of
// n comment lines would look over the lines after it n times.
type commentSkip struct{ from, to int }

// commandWord is the command word of the table a "command" key held: has is false when it held none.
type commandWord struct {
	tool shortFlagTool
	has  bool
}

// keyWords holds the last two words read, as byte ranges, to find the key before a value or a sequence.
type keyWords struct {
	s           string
	prev, prev2 [2]int
}

// push records the word s[i:j] as the last read.
func (k *keyWords) push(i, j int) { k.prev2, k.prev = k.prev, [2]int{i, j} }

// keyAt reports whether the last words read are the key name ("name:", or "name" and ":"), and where the name
// starts.
func (k *keyWords) keyAt(name string) (int, bool) {
	w := k.s[k.prev[0]:k.prev[1]]
	if len(w) == len(name)+1 && w[len(name)] == ':' && strings.EqualFold(w[:len(name)], name) {
		return k.prev[0], true
	}
	if w == ":" && strings.EqualFold(k.s[k.prev2[0]:k.prev2[1]], name) {
		return k.prev2[0], true
	}
	return 0, false
}

// end ends the command: no command word, no sequence item pending.
func (r *shortReader) end() { r.hasTool, r.args, r.first = false, false, false }

// startSequence starts a sequence. The value of an "args" key has no command word, unless its object has a
// "command" key, before or after it (shortObjects): then the command word of that one, if it held one of the
// table, is its own (a container's command and arguments, in any key order). With no command, the base name of
// the object's "image" is its command word when it names a row of the table, and the arguments keep the reading
// of arguments with no command word as well. Any other sequence starts with its first item, which decides.
func (r *shortReader) startSequence() {
	at, args := r.keyAt("args")
	_, command := r.keyAt("command")
	if r.probe && !command {
		r.stop()
		return
	}
	r.end()
	r.command = command
	if !args {
		r.first = true
		return
	}
	r.argsOf(at)
}

// argsOf sets the reading of the value of the "args" key at at: the command word of its object's "command", or
// its image of the table with the reading of arguments with no command word, or that reading alone.
func (r *shortReader) argsOf(at int) {
	if !r.objectsRead {
		r.objects, r.objectsRead = shortObjects(r.wm, r.s), true
	}
	row, ok := r.objects[at]
	switch {
	case ok && row.command >= 0:
		w := r.commandRow(row.command)
		r.hasTool, r.tool = w.has, w.tool
	case ok && row.image:
		r.args, r.hasTool, r.tool = true, true, row.tool
	default:
		r.args = true
	}
}

// endSequence ends a sequence; it ends the pass of a probe.
func (r *shortReader) endSequence() {
	if r.probe {
		r.stop()
		return
	}
	r.command = false
	r.end()
}

// resume takes up again, at the end of a comment line, the state from before it; what the line found out about
// the text (the objects, the command words, the memory of values) is kept.
func (r *shortReader) resume() {
	if r.saved == nil {
		return
	}
	cur := *r
	*r = *r.saved
	r.saved = nil
	r.objects, r.objectsRead, r.commands, r.tm, r.done, r.found = cur.objects, cur.objectsRead, cur.commands, cur.tm,
		cur.done, cur.found
}

// stop ends the pass of a probe with the command word read so far.
func (r *shortReader) stop() {
	r.found, r.done = commandWord{tool: r.tool, has: r.hasTool}, true
}

// commandRow returns the command word of the table that the "command" key at at holds: a pass of its own from
// the key to the end of its value (a sequence, or a scalar up to the end of its line or object), read once and
// kept by the key. The values of the keys of a text are apart, so each byte is read at most once more for them.
func (r *shortReader) commandRow(at int) commandWord {
	if w, ok := r.commands[at]; ok {
		return w
	}
	p := &shortReader{s: r.s, add: func(int, int) {}, tm: &tokenMemo{}, probe: true, wm: r.wm, skip: r.skip,
		keyWords: keyWords{s: r.s}}
	p.run(at)
	if !p.done {
		p.stop()
	}
	if r.commands == nil {
		r.commands = map[int]commandWord{}
	}
	r.commands[at] = p.found
	return p.found
}

// shortFlagRead is the one pass of the rule over s; with lines set s is a sequence of strings joined by line
// breaks. Each byte is read a bounded number of times: once as a word or a separator, once more as the
// indentation of a line looked at for a YAML item, and the values of the flags by flagExtent, after which the
// pass goes on from the end of the value.
func shortFlagRead(wm *meter.Meter, s string, lines bool, add func(start, end int)) {
	r := &shortReader{s: s, add: add, tm: &tokenMemo{}, lines: lines, first: lines, wm: wm, skip: &commentSkip{-1, -1},
		keyWords: keyWords{s: s}}
	r.run(0)
}

// run reads s from i to its end, or, for a probe, to the end of the value it reads.
func (r *shortReader) run(i int) {
	s, wm := r.s, r.wm
	for i < len(s) && !r.done {
		tick(wm, 1)
		if c := s[i]; c == '&' && r.yaml && r.depth == 0 && afterItemDash(wm, s, i) {
			// The anchor of a YAML item ("- &pw"), no shell control character.
			for i < len(s) && !isSpace(wm, s[i]) {
				tick(wm, 1)
				i++
			}
			continue
		}
		if e := r.itemHeaderEnd(wm, i); e > i {
			// The header of a block scalar item ends no command: its content is the item.
			r.header = true
			i = e
			continue
		}
		switch c := s[i]; {
		case c == '\n' || c == '\r':
			i = r.lineBreak(wm, i)
		case c == '{' || c == '}':
			if r.probe {
				r.stop()
			}
			r.end()
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
// word after an "args:" key), and so does an item after an "args:" or "command:" key inside an item of the
// sequence being read; any other ends the command.
func (r *shortReader) lineBreak(wm *meter.Meter, i int) int {
	s := r.s
	r.resume()
	if r.lines || r.depth > 0 {
		return i + 1
	}
	if s[i] == '\n' && i > 0 && s[i-1] == '\r' {
		return i + 1 // the second byte of CRLF, read with the first
	}
	if b := i - 1; b >= 0 && s[b] == '\\' {
		return i + 1 // a line continuation
	}
	k := i + 1
	if s[i] == '\r' && k < len(s) && s[k] == '\n' {
		k++
	}
	line := k
	for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
		tick(wm, 1)
		k++
	}
	item := k < len(s) && s[k] == '-' && (k+1 == len(s) || isSpace(wm, s[k+1]))
	_, args := r.keyAt("args")
	_, command := r.keyAt("command")
	if r.carriesOn(k, k-line, item, args || command) {
		return i + 1
	}
	if item {
		r.itemCol = k - line
	}
	switch {
	case item && r.yaml && (args || command):
		// A sequence inside an item of the one being read ("- args:" and its items below it, as YAML writes
		// a container): it starts here and is read as its own.
		r.endSequence()
		if !r.done {
			r.startSequence()
		}
	case item && r.yaml:
	case item:
		r.startSequence()
		r.yaml = true
	case r.yaml:
		r.endSequence()
		r.yaml = false
	case r.probe:
		r.stop()
	default:
		r.end()
	}
	return i + 1
}

// carriesOn reports, for the line whose first byte that is no blank is at k, in column col, whether the line
// break before it leaves the reading as it is: the content of a block scalar item ("- >-" and the lines right of
// its dash, or blank), where the item goes on; or a comment line in a YAML block sequence, or between an "args"
// or "command" key (key) and its first item, which is read as a line of its own with no command word, the
// sequence going on after it as before it (resume).
func (r *shortReader) carriesOn(k, col int, item, key bool) bool {
	s := r.s
	blank := k == len(s) || s[k] == '\n' || s[k] == '\r'
	header := r.header
	r.header = false
	if r.yaml && !item && (r.scalar || header) && (blank || col > r.itemCol) {
		r.scalar = true
		return true
	}
	r.scalar = false
	if k < len(s) && s[k] == '#' && (r.yaml || key) {
		saved := *r
		r.saved = &saved
		r.end()
		r.yaml, r.command, r.depth = false, false, 0
		return true
	}
	return false
}

// afterItemDash reports whether i follows the dash of a YAML block sequence item and a blank, with at most an
// anchor and a tag between them ("- &pw >-", "- !!str |"); it reads back over two words at most.
func afterItemDash(wm *meter.Meter, s string, i int) bool {
	x := i
	for range 3 {
		tick(wm, 1)
		b := x
		for b > 0 && (s[b-1] == ' ' || s[b-1] == '\t') {
			tick(wm, 1)
			b--
		}
		if b == x || b == 0 {
			return false
		}
		if s[b-1] == '-' {
			return b < 2 || s[b-2] == ' ' || s[b-2] == '\t' || s[b-2] == '\n' || s[b-2] == '\r'
		}
		w := b
		for w > 0 && s[w-1] != ' ' && s[w-1] != '\t' && s[w-1] != '\n' && s[w-1] != '\r' {
			tick(wm, 1)
			w--
		}
		if s[w] != '&' && s[w] != '!' {
			return false
		}
		x = w
	}
	return false
}

// itemHeaderEnd returns, inside a YAML block sequence, the end of the header of a block scalar item at i ("|" or
// ">" right after the item's "- " and any anchor or tag (afterItemDash), then at most a chomping indicator and an indentation digit, and blanks up to
// the line end), or i when there is none.
func (r *shortReader) itemHeaderEnd(wm *meter.Meter, i int) int {
	s := r.s
	if !r.yaml || r.depth > 0 || s[i] != '|' && s[i] != '>' || !afterItemDash(wm, s, i) {
		return i
	}
	k := i + 1
	for k < len(s) && k-i < 3 && (s[k] == '-' || s[k] == '+' || s[k] >= '1' && s[k] <= '9') {
		tick(wm, 1)
		k++
	}
	for k < len(s) && (s[k] == ' ' || s[k] == '\t') {
		tick(wm, 1)
		k++
	}
	if k < len(s) && s[k] != '\n' && s[k] != '\r' {
		return i
	}
	return k
}

// word reads the word s[i:j] and returns where the pass goes on: after the word, or after the value of a flag.
func (r *shortReader) word(wm *meter.Meter, i, j int) int {
	w := r.s[i:j]
	if at, ok := r.keyAt("args"); ok && w != "-" && !r.probe {
		// The value of an "args" key written as one string ("args: -pX --verbose"): read as its items are.
		r.end()
		r.argsOf(at)
	}
	r.push(i, j)
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
			// The value as the flag rule reads it may take in the comma or bracket after a flow item ("[-p, 8080,").
			if !portNumber(wm, strings.TrimRight(scalarContent(wm, s[vs:ve]), ",]} \t")) {
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
	case gluedQuote(wm, s, i, p):
		glued = true
	default:
		from = shortSeparatorEnd(wm, s, p, r.skip)
		if from == p || from >= len(s) || !r.nextItem(wm, p, from) {
			return 0, 0, false, false
		}
	}
	q := strings.TrimLeft(s[p:from], `\`)
	closed := q != "" && (q[0] == '"' || q[0] == '\'')
	start, end = flagExtent(wm, s, []int{i, from, i + 1, p}, closed, r.tm)
	if glued {
		end = escapedQuotedEnd(wm, s, i, itemEnd(wm, s, i, start, end))
	}
	if r.depth > 0 && (end == len(s) || strings.IndexByte(`"'\`, s[end]) < 0) {
		// A plain item of a flow sequence ends at its comma or closing bracket, which the flag rule's value takes
		// in ("[-pX]", "[-p, X]"): they are kept. A value that ends at a quote is a string's, whose brackets are
		// its own.
		for end > start && strings.IndexByte(",]}", s[end-1]) >= 0 {
			tick(wm, 1)
			end--
		}
	}
	return start, end, glued, end > start
}

// nextItem reports whether the separator from p to from of a flag in a YAML block sequence leads to a value of the
// flag: one on the flag's line, the next item of the sequence (a dash in the column of the flag's item), or a line
// of the block scalar item the flag is in, further right than its dash. A line break to any other line (a key of
// the object, an item of a sequence further left) ends the flag's item with no value. Outside a YAML block
// sequence a line break between a flag and its value is a blank.
func (r *shortReader) nextItem(wm *meter.Meter, p, from int) bool {
	if !r.yaml || r.depth > 0 || r.lines {
		return true
	}
	s := r.s
	line := from
	for line > p && (s[line-1] == ' ' || s[line-1] == '\t') {
		tick(wm, 1)
		line--
	}
	dash := line > p && s[line-1] == '-' && line < from
	col := from
	if dash {
		col = line - 1
		for line = col; line > p && (s[line-1] == ' ' || s[line-1] == '\t'); line-- {
			tick(wm, 1)
		}
	}
	switch {
	case line == p || s[line-1] != '\n' && s[line-1] != '\r':
		return true // no line break right before the value's line
	case dash:
		return col-line == r.itemCol
	default:
		return r.scalar && col-line > r.itemCol
	}
}

// stringQuote returns, for a flag that starts at i right after a double quote, the number of backslashes before
// that quote (0 in a JSON string, 1 in a JSON string inside another); -1 when no double quote is before it.
func stringQuote(wm *meter.Meter, s string, i int) int {
	if i == 0 || s[i-1] != '"' {
		return -1
	}
	b := 0
	for x := i - 2; x >= 0 && s[x] == '\\' && b < maxValueExtent; x-- {
		tick(wm, 1)
		b++
	}
	return b
}

// itemEnd returns the end of the value from start to end of a flag that starts at i right after a double quote
// with b backslashes before it, the opening quote of the string the flag word stands in: the value ends at the
// quote that closes that string (the same quote with as many backslashes), whatever a shell would read on.
func itemEnd(wm *meter.Meter, s string, i, start, end int) int {
	b := stringQuote(wm, s, i)
	if b < 0 {
		return end
	}
	run := 0
	for k := start; k < end; k++ {
		tick(wm, 1)
		switch {
		case s[k] == '\\':
			run++
		case s[k] == '"' && run == b:
			return k - b
		default:
			run = 0
		}
	}
	return end
}

// escapedQuotedEnd returns the end of the glued value of the flag at i that ends at end, taking in a piece
// quoted with escaped double quotes glued to it, as a value inside a JSON string holds one ("-pab\"cd\"" for the
// shell's -pab"cd"): from backslashes and a quote at end, more backslashes than the quote of the string the flag
// stands in has (stringQuote), to the same backslashes and quote that close it, and the bytes of a word after
// them. A piece with no closing quote before the next quote or line end is not taken.
func escapedQuotedEnd(wm *meter.Meter, s string, i, end int) int {
	n := 0
	for end+n < len(s) && s[end+n] == '\\' {
		tick(wm, 1)
		n++
	}
	if n == 0 || n <= stringQuote(wm, s, i) || end+n >= len(s) || s[end+n] != '"' {
		return end
	}
	m := end + n + 1
	for m < len(s) && s[m] != '"' && s[m] != '\\' && s[m] != '\n' && s[m] != '\r' {
		tick(wm, 1)
		m++
	}
	b := 0
	for m+b < len(s) && s[m+b] == '\\' {
		tick(wm, 1)
		b++
	}
	if b != n || m+b >= len(s) || s[m+b] != '"' {
		return end
	}
	end = m + b + 1
	for end < len(s) && !shortDelimiter(wm, s[end]) && strings.IndexByte(";|&()`{}[]", s[end]) < 0 {
		tick(wm, 1)
		end++
	}
	return end
}

// gluedQuote reports whether the flag that starts at i and ends at p has a quoted value glued to it ("-p'X'",
// "-p\"X\"" inside a JSON string): a quote at p, after any backslashes, that opens a value, as a letter, a digit or
// any other byte of a value follows it, and that is not the closing quote of the string the flag stands in (the
// same quote, with as many backslashes, right before the flag: "\"-p\"").
func gluedQuote(wm *meter.Meter, s string, i, p int) bool {
	k := p
	for k < len(s) && s[k] == '\\' {
		tick(wm, 1)
		k++
	}
	if k+1 >= len(s) || s[k] != '"' && s[k] != '\'' {
		return false
	}
	if c := s[k+1]; isSpace(wm, c) || strings.IndexByte("\"',;|&()`{}[]<>\\:", c) >= 0 {
		return false
	}
	if i == 0 || s[i-1] != s[k] {
		return true
	}
	b := 0
	for x := i - 2; x >= 0 && s[x] == '\\'; x-- {
		tick(wm, 1)
		b++
	}
	return b != k-p
}

// shortSeparatorEnd returns the end of the separator between a flag that ends at p and a value of its own, as the
// flag rule reads one (flagSeparatorExpr), or p when there is none: a comma right after the flag or its closing
// quote then blanks, or the closing quote and blanks, or blanks alone; a blank is a space, a tab, a line end
// (with a YAML list dash after its indentation) or a backslash before a line end.
func shortSeparatorEnd(wm *meter.Meter, s string, p int, skip *commentSkip) int {
	k := p
	for k < len(s) && s[k] == '\\' {
		tick(wm, 1)
		k++
	}
	if k < len(s) && (s[k] == '"' || s[k] == '\'') {
		if k+1 < len(s) && s[k+1] == ',' {
			return shortBlanksEnd(wm, s, k+2, skip)
		}
		if e := shortBlanksEnd(wm, s, k+1, skip); e > k+1 {
			return e
		}
		return p
	}
	if p < len(s) && s[p] == ',' {
		return shortBlanksEnd(wm, s, p+1, skip)
	}
	return shortBlanksEnd(wm, s, p, skip)
}

// shortBlanksEnd returns the end of the blanks of flagSpaceExpr from k. Comment lines after a line break are blanks
// too; the run of them it looks over is kept in skip, and a line start inside the last run kept goes on at its end.
func shortBlanksEnd(wm *meter.Meter, s string, k int, skip *commentSkip) int {
	from := -1
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
			if skip.from <= k && k < skip.to {
				k = skip.to // read by an earlier flag: the same lines lead to the same line start
			}
			if from >= 0 {
				skip.from, skip.to = from, k
			}
			d := k
			for d < len(s) && (s[d] == ' ' || s[d] == '\t') {
				tick(wm, 1)
				d++
			}
			if d < len(s) && s[d] == '#' {
				// A comment line between a flag and its value: the value is after it.
				if from < 0 {
					from = k
				}
				for d < len(s) && s[d] != '\n' && s[d] != '\r' {
					tick(wm, 1)
					d++
				}
				k = d
				continue
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

// scalarContent returns the content of the value v when it is a YAML block scalar, with or without an anchor
// or a tag before its header (the header line dropped and the blanks around the rest), else v.
func scalarContent(wm *meter.Meter, v string) string {
	h := v
	for h != "" && (h[0] == '&' || h[0] == '!') {
		tick(wm, 1)
		k := strings.IndexAny(h, " \t")
		if k < 0 {
			return v
		}
		h = strings.TrimLeft(h[k:], " \t")
	}
	if h == "" || h[0] != '|' && h[0] != '>' {
		return v
	}
	v = h
	k := strings.IndexAny(v, "\r\n")
	if k < 0 {
		return v
	}
	tick(wm, len(v))
	return strings.TrimSpace(v[k:])
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
