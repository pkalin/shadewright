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
	"strings"
	"unicode"
	"unicode/utf8"
)

// The canonical view of a text (#845). What to redact under a secret key with a block value is decided once,
// on one reading of the text as it was read: nothing is replaced, escaped, cut or repaired first. The view is
// a set of read-only functions over the original bytes:
//
//   - A line ends at a line feed, a carriage return followed by a line feed, or a carriage return alone.
//   - A line's indentation is the run of indentation characters at its start: every Unicode space and
//     control character (the tab, NUL, the no-break and ideographic spaces, the zero-width space and
//     joiners, the byte order mark, the line and paragraph separators and NEL among them) and every byte
//     that is not valid UTF-8. A line of nothing else is blank.
//   - An indentation or a key's column has two widths: its characters, each counted once, and the same with
//     a tab advancing to the next multiple of eight. A line is deeper than a key when either width is larger,
//     so no choice of tab width can lift a value line out of its block.
//
// The block-value rule (viewBlockSpans) reads the view line by line and returns byte ranges of the original;
// they join the ranges of every other rule and the text is rewritten once ([Redactor.String]). No rule reads
// another's output, so the order of the rules does not matter.

// col is a column, or the width of an indentation, in the view's two widths: n counts characters, w counts
// them with a tab advancing to the next multiple of eight.
type col struct{ n, w int }

// deeper reports whether c lies to the right of k in either width.
func (c col) deeper(k col) bool { return c.n > k.n || c.w > k.w }

// atLeast reports whether c lies at k or to its right in either width.
func (c col) atLeast(k col) bool { return c.n >= k.n || c.w >= k.w }

// step returns c advanced over one character whose first byte is ch.
func (c col) step(ch byte) col {
	if ch == '\t' {
		return col{c.n + 1, (c.w/8 + 1) * 8}
	}
	return col{c.n + 1, c.w + 1}
}

// charLen returns the length of the character at s[i]: a valid UTF-8 sequence, or one byte that is not valid
// UTF-8.
func charLen(s string, i int) int {
	if s[i] < utf8.RuneSelf {
		return 1
	}
	_, n := utf8.DecodeRuneInString(s[i:])
	return n
}

// colOf returns the column of to on the line that starts at from.
func colOf(s string, from, to int) col {
	var c col
	for i := from; i < to; {
		tick(1)
		c = c.step(s[i])
		i += charLen(s, i)
	}
	return c
}

// breakLen returns the length of the line break at s[i]: 2 for CRLF, 1 for LF or a lone CR, 0 otherwise.
func breakLen(s string, i int) int {
	switch s[i] {
	case '\n':
		return 1
	case '\r':
		if i+1 < len(s) && s[i+1] == '\n' {
			return 2
		}
		return 1
	}
	return 0
}

// indentLen returns the length of the indentation character at s[i], or 0 (a line break is none).
func indentLen(s string, i int) int {
	c := s[i]
	if c == '\n' || c == '\r' {
		return 0
	}
	if c < utf8.RuneSelf {
		if c <= ' ' || c == 0x7f {
			return 1
		}
		return 0
	}
	r, n := utf8.DecodeRuneInString(s[i:])
	if r == utf8.RuneError && n == 1 || unicode.IsSpace(r) || unicode.In(r, unicode.Cc, unicode.Cf) {
		return n
	}
	return 0
}

// trimIndent returns t without indentation characters at either end.
func trimIndent(t string) string {
	for len(t) > 0 {
		tick(1)
		n := indentLen(t, 0)
		if n == 0 {
			break
		}
		t = t[n:]
	}
	for len(t) > 0 {
		tick(1)
		n := lastIndentLen(t, len(t))
		if n == 0 {
			break
		}
		t = t[:len(t)-n]
	}
	return t
}

// lastIndentLen returns the length of the indentation character that ends at s[:end], or 0.
func lastIndentLen(s string, end int) int {
	r, n := utf8.DecodeLastRuneInString(s[:end])
	if r == utf8.RuneError && n <= 1 {
		if s[end-1] == '\n' || s[end-1] == '\r' {
			return 0
		}
		return 1
	}
	if indentLen(s, end-n) == n {
		return n
	}
	return 0
}

// viewLine is one line of the view: where it starts, where its text starts after the indentation, where it
// ends before its break, where the next line starts, and the width of its indentation.
type viewLine struct {
	start, text, end, next int
	ind                    col
}

// blank reports whether the line holds only indentation.
func (l viewLine) blank() bool { return l.text == l.end }

// readLine returns the line of the view that starts at pos.
func readLine(s string, pos int) viewLine {
	l := viewLine{start: pos, text: pos}
	for l.text < len(s) {
		tick(1)
		n := indentLen(s, l.text)
		if n == 0 {
			break
		}
		l.ind = l.ind.step(s[l.text])
		l.text += n
	}
	l.end = len(s)
	if i := indexAny(s[l.text:], "\r\n"); i >= 0 {
		l.end = l.text + i
	}
	l.next = l.end
	if l.end < len(s) {
		l.next += breakLen(s, l.end)
	}
	return l
}

// viewLines returns the lines from pos on that pred takes, and blank lines between them: the start of the
// first one's text, the end of the last one, and the start of the line after it; start is -1 when pred takes
// none. A comment line that pred does not take (a "#" at column 0 inside a block, #845) is read like a blank
// line: the block goes on after it when a line pred takes follows, and its text is added to holes, which the
// block keeps.
func viewLines(s string, pos int, pred func(col) bool, holes *[][2]int) (start, end, next int) {
	start, end, next = -1, -1, pos
	var comments [][2]int
	for pos < len(s) {
		tick(1)
		l := readLine(s, pos)
		pos = l.next
		if l.blank() {
			continue
		}
		if !pred(l.ind) {
			if s[l.text] == '#' {
				comments = append(comments, [2]int{l.text, l.end})
				continue
			}
			break
		}
		if start < 0 {
			start = l.text
		} else if holes != nil {
			*holes = append(*holes, comments...)
		}
		comments = comments[:0]
		end, next = l.end, l.next
	}
	return start, end, next
}

// viewBlock returns the block under a header line whose next line starts at pos, of a key at column key: after
// any "---" or "..." lines at column 0, the lines deeper than the key; with none, the lines from the next one
// that is indented less than the key but not at column 0. It returns the start of the block's text, its end,
// and the start of the line after it, or -1, -1, pos; the comment lines inside it that it keeps go to holes
// (viewLines).
func viewBlock(s string, pos int, key col, holes *[][2]int) (start, end, next int) {
	first := pos
	for pos < len(s) {
		tick(1)
		l := readLine(s, pos)
		if l.blank() {
			pos = l.next
			continue
		}
		if t := trimIndent(s[l.text:l.end]); l.ind.n != 0 || t != "---" && t != "..." {
			break
		}
		pos = l.next
	}
	if start, end, next := viewLines(s, pos, func(c col) bool { return c.deeper(key) }, holes); start >= 0 {
		return start, end, next
	}
	for p := pos; p < len(s); {
		tick(1)
		l := readLine(s, p)
		p = l.next
		if l.blank() {
			continue
		}
		if l.ind.n == 0 || l.ind.atLeast(key) && pos == first {
			break
		}
		return viewLines(s, pos, func(c col) bool { return c.atLeast(l.ind) }, holes)
	}
	return -1, -1, first
}

// addHoles calls add for start to end without the holes, sorted ranges inside it.
func addHoles(start, end int, holes [][2]int, add func(start, end int)) {
	for _, h := range holes {
		tick(1)
		add(start, h[0])
		start = h[1]
	}
	add(start, end)
}

// viewLooksLikeSecret reports whether any line of the block s[start:end], or a word of one, looks like a secret
// (wordsLookLikeSecret), each line read without its indentation.
func viewLooksLikeSecret(s string, start, end int) bool {
	for pos := start; pos < end; {
		tick(1)
		l := readLine(s, pos)
		if wordsLookLikeSecret(trimIndent(s[l.text:min(l.end, end)])) {
			return true
		}
		pos = l.next
	}
	return false
}

// viewKey matches a secret key and its colon, with or without blanks around it, and a YAML block scalar
// indicator after it if there is one (group 2): the keys the block-value rule reads on a line.
var viewKey = regexp.MustCompile(`(?i)(` + secretKeyExpr + `)` + keySpaceExpr + `*:(?:` + keySpaceExpr + `*([|>][1-9+-]{0,2}))?`)

// neighbourKey matches a secret key of any form inside a value the block-value rule redacts: a key and ":"
// or "=" (also in quotes, plain or escaped), or a secret-named flag. Its name (group 1, or group 2 for a flag)
// is left out of the value, as every key-value rule shows a key's name, so a neighbouring key stays readable
// while its value goes with the value around it.
var neighbourKey = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])(?:\\*["'])?(` + bareKeyExpr + `)(?:\\*["'])?` +
	keySpaceExpr + `*[:=]|(?:^|[^A-Za-z0-9_.-])(--?` + bareKeyExpr + `)(?:[ \t=]|$)`)

// plainKeyAfterComma matches a comma and a plain key ("name:", "'user':") after it: the end of a value in a
// flow collection at the same boundary the flow rule has, so the fields after it stay.
var plainKeyAfterComma = regexp.MustCompile(`,` + keySpaceExpr + `*(?:"[^"\r\n]*"|'[^'\r\n]*'|[A-Za-z_][A-Za-z0-9_.-]*)` +
	keySpaceExpr + `*:`)

// maxStripClosers bounds the closing brackets after a value that the block-value rule keeps, so a line of many
// brackets costs constant time per key.
const maxStripClosers = 8

// viewKeyAt is one key the block-value rule found on a line.
type viewKeyAt struct {
	m        []int
	strEnd   int
	flow     []byte // the flow collections open where the key stands, innermost last
	after    int
	block    bool // the key has an indicator followed by a blank or the end of its value
	plain    bool // the key has no indicator and stands in a flow collection
	word     bool // the key is quoted, has no indicator and stands in free text: its value is one word
	bare     bool
	takes    bool // the key redacts its own value (not a reference, not refused by its form)
	valueEnd int
}

// viewBlockSpans is the block-value rule (#845): on the canonical view, every secret key with a YAML block
// indicator wherever it stands, and every secret key in a flow collection, as viewLine reads them. The lines
// of a block it redacts are skipped; the lines of a block it refuses are read for keys of their own, and a
// bare short key on one of them refuses its block unread (no line of the outer block looks like a secret),
// so each line is read a fixed number of times.
func viewBlockSpans(s string, add func(start, end int)) {
	refused := -1
	for pos := 0; pos < len(s); {
		tick(1)
		l := readLine(s, pos)
		pos = l.next
		if indexByte(s[l.text:l.end], ':') < 0 {
			continue
		}
		viewEnvLine(s, l, add)
		if e := viewKeyLine(s, l, refused, add, &refused); e > pos {
			pos = e
		}
	}
}

// viewKeyLine reads the keys of line l and returns where the next line to read starts when a block below l
// was redacted, or 0. For each key:
//
//   - The value of a key with an indicator is the text after the indicator, up to the end of the line or of the
//     quoted string the key stands in, and the block below the line. The block is read once for the line: the
//     lines deeper than the first key when it is first on its line, otherwise deeper than the line's text. It
//     goes when one key with an indicator takes it: any key that is no bare short key, or a bare short key
//     when a line of the block looks like a secret.
//   - The value of a key in a flow collection ("[" or "{" opened before it on the line, in quotes or not)
//     without an indicator is the text after its colon.
//   - Such a value ends before the next key on the line that takes its own value, before a comma and a plain
//     key, at the end of the line or of the string; closing brackets that close the collections opened before
//     the key stay. A closing bracket inside the value does not end it: brackets are followed to the line's
//     end, not to the first that could close (#845).
//   - Inside the value, the names of other secret keys stay (neighbourKey); everything else goes.
func viewKeyLine(s string, l viewLine, refused int, add func(start, end int), refusedOut *int) int {
	ms := viewKey.FindAllStringSubmatchIndex(counted(s[l.text:l.end]), -1)
	if len(ms) == 0 {
		return 0
	}
	ctx := newLineContext(s, l.start, l.end)
	br := &bracketCursor{s: s, pos: l.start}
	keys := make([]viewKeyAt, 0, len(ms))
	for _, m := range ms {
		tick(1)
		for i := range m {
			if m[i] >= 0 {
				m[i] += l.text
			}
		}
		keys = append(keys, readViewKey(s, l, ctx, br, m))
	}
	var holes [][2]int
	bStart, bEnd, bNext, accepted := viewLineBlock(s, l, keys, refused, &holes)
	covered := -1
	for i, k := range keys {
		if !k.block && !k.plain && !k.word || k.after > k.valueEnd || k.m[0] < covered {
			continue
		}
		end := viewValueEnd(s, keys, i)
		// The keys inside this value are read with it, so a line of keys is read once.
		covered = max(covered, end)
		if k.after < end && (s[k.after] != '#' || !k.block || bStart < 0 || k.strEnd >= 0) &&
			(!k.bare || wordsLookLikeSecret(s[k.after:end])) {
			addAround(s, k.after, end, false, add)
		}
	}
	if bStart < 0 {
		return 0
	}
	if accepted {
		addHoles(bStart, bEnd, holes, func(start, end int) { addAround(s, start, end, true, add) })
		return bNext
	}
	*refusedOut = max(*refusedOut, bEnd)
	return 0
}

// readViewKey reads the key that the viewKey match m (offsets in s) holds on line l: the end of the quoted
// string it stands in, the flow collections open before it, and whether its value is a block, a plain flow
// value or one word (viewKeyLine).
func readViewKey(s string, l viewLine, ctx *lineContext, br *bracketCursor, m []int) viewKeyAt {
	k := viewKeyAt{m: m, strEnd: -1}
	keyAt := m[2]
	if q := s[keyAt]; (q == '"' || q == '\'') && s[m[3]-1] != q {
		keyAt++
	}
	if k.strEnd = ctx.stringEnd(keyAt); k.strEnd >= 0 && k.strEnd < m[3] {
		// The quote that would close the string is the key's own: quotes inside a quoted string that are
		// not escaped cannot be told apart, so the key is read as standing outside it.
		k.strEnd = -1
	}
	strStart := -1
	if k.strEnd >= 0 {
		strStart = ctx.strs[ctx.si][0]
	}
	k.flow = br.at(keyAt, strStart)
	k.valueEnd = l.end
	if k.strEnd >= 0 {
		k.valueEnd = k.strEnd
	}
	if keepReference(s, m) || sasRefused(s, m, true) {
		return k
	}
	k.bare = bareShortColon(s, m)
	if m[4] >= 0 {
		k.after, k.block = afterViewIndicator(s, m[5], k.valueEnd, k.strEnd >= 0)
		if q := escapedQuoteAt(s, k.after, k.valueEnd); k.strEnd >= 0 && q > 0 && (k.block || k.after == m[5]) {
			// An escaped quote ends an item of JSON escaped inside the string: the value ends there.
			k.block, k.valueEnd = true, k.after
		}
	}
	if !k.block && len(k.flow) > 0 {
		// A plain value in a flow collection; a quoted value, an array or an object is the extent rule's.
		k.after = plainValueStart(s, m, k.valueEnd)
		k.plain = k.after >= k.valueEnd || strings.IndexByte(`"'\[{`, s[k.after]) < 0
	}
	if !k.block && !k.plain && len(k.flow) == 0 && k.strEnd < 0 && (s[m[2]] == '"' || s[m[2]] == '\'') &&
		s[m[3]] == ':' {
		// A quoted key, its colon right after it, and a plain value in free text ("retry 'token': a]b"): the
		// value is one word. With a blank before the colon it is prose ("volume \"secret-x\" : secret ...").
		k.after = plainValueStart(s, m, k.valueEnd)
		k.word = k.after >= k.valueEnd || strings.IndexByte(`"'\[{`, s[k.after]) < 0
	}
	k.takes = (k.block || k.plain || k.word) && !k.bare
	return k
}

// plainValueStart returns where the value of the viewKey match m starts: after its colon, or after the
// indicator in group 2 when there is one, past the indentation characters, at most at end.
func plainValueStart(s string, m []int, end int) int {
	i := m[1]
	if m[4] >= 0 {
		i = m[4]
	}
	for i < end && indentLen(s, i) > 0 {
		tick(1)
		i += indentLen(s, i)
	}
	return i
}

// viewLineBlock reads the block below line l once for the keys with an indicator outside quotes, and returns
// its start, end and the start of the line after it (bStart < 0 when there is none) and whether a key takes
// it: any key that is no bare short key, or a bare one when the block looks like a secret. A line whose keys
// are all bare short keys and that lies inside a block refused before it (up to refused) reads no block.
func viewLineBlock(s string, l viewLine, keys []viewKeyAt, refused int, holes *[][2]int) (bStart, bEnd, bNext int, accepted bool) {
	bStart, bEnd = -1, -1
	claim, allBare := -1, true
	for i, k := range keys {
		if k.block && k.strEnd < 0 {
			if claim < 0 {
				claim = i
			}
			allBare = allBare && k.bare
		}
	}
	if claim < 0 || allBare && l.end < refused {
		return bStart, bEnd, 0, false
	}
	key := l.ind
	if firstKey := l.text + dashes(s[l.text:l.end]); keys[claim].m[2] == firstKey {
		key = colOf(s, l.start, firstKey)
	}
	bStart, bEnd, bNext = viewBlock(s, l.next, key, holes)
	if bStart < 0 {
		return bStart, bEnd, bNext, false
	}
	countScan(bEnd - bStart)
	// The block is read for secrets at most once for the line, however many bare short keys claim it (#845,
	// fourth round: each key read it again, quadratic in a line of many "pass: |").
	looks, read := false, false
	for _, k := range keys {
		if !k.block || k.strEnd >= 0 {
			continue
		}
		if !k.bare {
			return bStart, bEnd, bNext, true
		}
		if !read {
			looks, read = viewLooksLikeSecret(s, bStart, bEnd), true
		}
		if looks {
			return bStart, bEnd, bNext, true
		}
	}
	return bStart, bEnd, bNext, false
}

// viewValueEnd returns where the value of keys[i] on its line ends: before the next key in the same string
// that takes its own value, at a comma for a plain flow value, before a comma and a plain key in a flow
// collection, at the last bracket that closes the innermost collection, without its trailing separators.
func viewValueEnd(s string, keys []viewKeyAt, i int) int {
	k := keys[i]
	end := k.valueEnd
	if k.word {
		end = k.after
		for end < k.valueEnd && indentLen(s, end) == 0 {
			tick(1)
			end += charLen(s, end)
		}
	}
	for _, n := range keys[i+1:] {
		if n.takes && n.strEnd == k.strEnd && n.m[0] < end {
			end = max(n.m[0], k.after)
			break
		}
	}
	switch {
	case k.plain:
		// A plain value in a flow collection ends at a comma, as the flow rule has it.
		if c := indexByte(s[k.after:end], ','); c >= 0 {
			end = k.after + c
		}
	case len(k.flow) > 0:
		if c := plainKeyAfterComma.FindStringIndex(counted(s[k.after:end])); c != nil {
			end = k.after + c[0]
		}
	}
	if n := len(k.flow); n > 0 {
		if c := flowCloseAt(s, k.after, end, k.flow[n-1]); c >= 0 {
			end = c
		}
	}
	return valueTail(s, k.after, end, k.flow)
}

// afterViewIndicator returns where the text after a block indicator that ends at ie starts, past the blanks
// after it, and whether a blank or the end of the value (valEnd) follows the indicator ("|x", "|#" and ">="
// are no indicators). A blank is any indentation character of the view, or inside a quoted string an escaped
// line break or tab.
func afterViewIndicator(s string, ie, valEnd int, inStr bool) (int, bool) {
	if ie == valEnd {
		return ie, true
	}
	i := ie
	for i < valEnd {
		tick(1)
		n := indentLen(s, i)
		if n == 0 && s[i] == '\\' && inStr && i+1 < valEnd && strings.IndexByte("ntr", s[i+1]) >= 0 {
			n = 2
		}
		if n == 0 {
			break
		}
		i += n
	}
	return i, i > ie
}

// escapedQuoteAt returns the length of a run of backslashes and a double quote at s[i] before end, or 0.
func escapedQuoteAt(s string, i, end int) int {
	j := i
	for j < end && s[j] == '\\' {
		tick(1)
		j++
	}
	if j > i && j < end && s[j] == '"' {
		return j + 1 - i
	}
	return 0
}

// valueTail returns the end of a value from start to end without the blanks and separators at its end and,
// for a key inside flow collections (flow, innermost last), the closing brackets of those collections.
func valueTail(s string, start, end int, flow []byte) int {
	trim := func() {
		for end > start {
			tick(1)
			if c := s[end-1]; c == ',' || c == ';' {
				end--
				continue
			}
			n := lastIndentLen(s[:end], end)
			if n == 0 || end-n < start {
				break
			}
			end -= n
		}
	}
	trim()
	for i := len(flow) - 1; i >= 0 && end > start; i-- {
		tick(1)
		if s[end-1] != flow[i]+2 {
			// '[' and '{' are two below ']' and '}'.
			break
		}
		end--
		trim()
	}
	return end
}

// addAround adds the range start to end without the names of the secret keys in it (neighbourKey). A key name
// at start is part of the value unless block is set (a block's first line may be a key of its own).
func addAround(s string, start, end int, block bool, add func(start, end int)) {
	pos := start
	for _, m := range neighbourKey.FindAllStringSubmatchIndex(counted(s[start:end]), -1) {
		tick(1)
		ns, ne := m[2], m[3]
		if ns < 0 {
			ns, ne = m[4], m[5]
		}
		ns, ne = start+ns, start+ne
		if ns == start && !block {
			// The value starts with a key name: it is the value, not a neighbour.
			continue
		}
		add(pos, ns)
		pos = ne
	}
	add(pos, end)
}

// flowCloseAt returns where a value from start to end in a flow collection opened by opener ends: at the last
// closing bracket of that collection on the line, brackets opened inside the value followed, that is followed
// by the end, a blank, a comma, a semicolon or a comment, after any more closing brackets and quotes; or -1.
// A bracket glued to more text ("a]b") is part of the value, however it could pair with an earlier one.
func flowCloseAt(s string, start, end int, opener byte) int {
	var inner []byte
	last := -1
	for i := start; i < end; i++ {
		tick(1)
		switch c := s[i]; c {
		case '[', '{':
			inner = append(inner, c)
		case ']', '}':
			if n := len(inner); n > 0 && inner[n-1] == c-2 {
				inner = inner[:n-1]
				continue
			}
			if c != opener+2 {
				continue
			}
			j := i
			for j < end && strings.IndexByte(`]})"'`, s[j]) >= 0 {
				tick(1)
				j++
			}
			if j == end || indentLen(s, j) > 0 || strings.IndexByte(",;#", s[j]) >= 0 {
				last = i
			}
		}
	}
	return last
}

// bracketCursor follows the flow brackets ("[" and "{") of a line from its start, quotes or not, so that the
// block-value rule can ask, for each key in order, which collections are open where it stands.
type bracketCursor struct {
	s     string
	pos   int
	stack []int
}

// at returns the innermost maxStripClosers brackets open at pos that open after from (-1 for any), innermost
// last. Calls come with pos in increasing order, so the line is read once.
func (b *bracketCursor) at(pos, from int) []byte {
	for ; b.pos < pos; b.pos++ {
		tick(1)
		switch c := b.s[b.pos]; c {
		case '[', '{':
			b.stack = append(b.stack, b.pos)
		case ']', '}':
			if n := len(b.stack); n > 0 && b.s[b.stack[n-1]] == c-2 {
				b.stack = b.stack[:n-1]
			}
		}
	}
	var out []byte
	for _, p := range b.stack[max(0, len(b.stack)-maxStripClosers):] {
		if p > from {
			out = append(out, b.s[p])
		}
	}
	return out
}

// viewEnvName matches the text of a line that names a Kubernetes environment variable holding a secret, in
// block form ("- name: DB_PASSWORD"), up to the line's end; group 1 is the name.
var viewEnvName = regexp.MustCompile(`(?i)^-[ \t]+name[ \t]*:[ \t]*["']?(` + bareKeyExpr + `)["']?$`)

// viewEnvOwnName matches the text of a line, after its indentation, that is an environment entry's own "name"
// key naming a secret (group 1), as it follows the entry's "value" key in block form.
var viewEnvOwnName = regexp.MustCompile(`(?i)^name[ \t]*:[ \t]*["']?(` + bareKeyExpr + `)["']?$`)

// viewEnvLine redacts, on the canonical view, the value of the environment variable that line l names
// (viewEnvName), as envValueSpan does on lines that end at a line feed: the "value" key among the entry's own
// keys on the following lines, at most envEntryLines of them, inline or in a block below it. An entry whose
// "value" comes first ("- value: x" and "name: DB_PASSWORD" below it, #845) is read from its value line: its own
// keys on the following lines, as many, are looked through for the name.
func viewEnvLine(s string, l viewLine, add func(start, end int)) {
	text := trimIndent(s[l.text:l.end])
	at := l.text + dashes(s[l.text:l.end])
	if at == l.text {
		return
	}
	entry := colOf(s, l.start, at)
	if viewEnvValue(s, l, at, l.end, entry, nil) {
		viewEnvValueFirst(s, l, at, entry, add)
		return
	}
	m := viewEnvName.FindStringSubmatchIndex(counted(text))
	if m == nil {
		return
	}
	for i := range m {
		m[i] += l.text
	}
	if keepReference(s, m) || sasRefused(s, m, false) {
		return
	}
	pos := l.next
	for read := 0; pos < len(s) && read < envEntryLines; read++ {
		tick(1)
		e := readLine(s, pos)
		pos = e.next
		if e.blank() {
			continue
		}
		if !e.ind.atLeast(entry) {
			return
		}
		if e.ind == entry && viewEnvValue(s, e, e.text, e.end, entry, add) {
			return
		}
	}
}

// viewEnvValueFirst redacts the value on line l, whose "value" key starts at at, when one of the entry's own
// keys on the following lines (at column entry, at most envEntryLines lines) names a secret (viewEnvOwnName).
func viewEnvValueFirst(s string, l viewLine, at int, entry col, add func(start, end int)) {
	pos := l.next
	for read := 0; pos < len(s) && read < envEntryLines; read++ {
		tick(1)
		e := readLine(s, pos)
		pos = e.next
		if e.blank() {
			continue
		}
		if !e.ind.atLeast(entry) {
			return
		}
		if e.ind != entry {
			continue
		}
		t := trimIndent(s[e.text:e.end])
		m := viewEnvOwnName.FindStringSubmatchIndex(counted(t))
		if m == nil {
			continue
		}
		for i := range m {
			m[i] += e.text
		}
		if !keepReference(s, m) && !sasRefused(s, m, false) {
			viewEnvValue(s, l, at, l.end, entry, add)
		}
		return
	}
}

// viewEnvValue reports whether s[from:end], on line e, is an environment entry's "value" key, and with add set
// redacts its value: inline after the colon, without blanks and quotes around it, or the block below the line
// (viewBlock) under a block indicator or nothing.
func viewEnvValue(s string, e viewLine, from, end int, entry col, add func(start, end int)) bool {
	rest, ok := strings.CutPrefix(s[from:end], "value")
	rest = strings.TrimLeft(rest, " \t")
	if !ok || rest == "" || rest[0] != ':' {
		return false
	}
	if add == nil {
		return true
	}
	v := end - len(rest) + 1
	for v < end && indentLen(s, v) > 0 {
		tick(1)
		v += indentLen(s, v)
	}
	if v >= end || isBlockHeader(s, v) {
		var holes [][2]int
		if start, bEnd, _ := viewBlock(s, e.next, entry, &holes); start >= 0 {
			addHoles(start, bEnd, holes, add)
		}
		return true
	}
	for end > v && lastIndentLen(s[:end], end) > 0 {
		tick(1)
		end -= lastIndentLen(s[:end], end)
	}
	if c := s[v]; (c == '"' || c == '\'') && end-v >= 2 && s[end-1] == c {
		v, end = v+1, end-1
	}
	add(v, end)
	return true
}

// envValueFirst matches a Kubernetes environment variable in flow form whose value comes before its name
// ({value: x, name: DB_PASSWORD}, also in JSON, plainly or escaped): group 1 is the value, quoted or plain,
// group 2 the name.
var envValueFirst = regexp.MustCompile(`(?i)\\*["']?\bvalue\\*["']?` + jsonSpaceExpr + `:` + jsonSpaceExpr +
	`(\\*"(?:[^"\\\r\n]|\\[^"])*\\*"|'[^'\r\n]*'|[^,{}\[\]"'\r\n]*)` + jsonSpaceExpr + `,` + jsonSpaceExpr +
	`\\*["']?name\\*["']?` + jsonSpaceExpr + `:` + jsonSpaceExpr + `\\*["']?(` + bareKeyExpr + `)\\*["']?` +
	jsonSpaceExpr + `[,}]`)

// envValueFirstSpan is the span function of envValueFirst: the value, without its quotes and the blanks at
// its end. A name that is a reference keeps it.
func envValueFirstSpan(s string, m []int, _ int) (int, int, bool) {
	if isReference(s[m[4]:m[5]]) {
		return 0, 0, false
	}
	start, end := m[2], m[3]
	for end > start && isSpace(s[end-1]) {
		tick(1)
		end--
	}
	for start < end && s[start] == '\\' {
		tick(1)
		start++
	}
	if start < end && (s[start] == '"' || s[start] == '\'') {
		start++
		for end > start && (s[end-1] == '"' || s[end-1] == '\'' || s[end-1] == '\\') {
			tick(1)
			end--
		}
	}
	return start, end, end > start
}

// The strings of a sequence (#845, second to fourth rounds): a key in one string and its value in a later one.
// [Redactor.Strings] takes the decision String takes on the strings joined by line breaks, mapped back to each
// string, so what String finds in the strings joined is found in them apart by construction
// (TestStringsDecidesAsTheJoinedText). On top of it, and for a sequence written in a text (seqScan), one function
// (seqValues) reads what a joined text does not show as a key and value: blank strings between the key and its
// value, a separator in a string of its own, a flag after other text in its string, a key's string that ends
// with a line break.

// seqKeyEnd matches the end of a string whose next non-blank string is its value: a secret key, its closing
// quote if it has one, and a colon, with a block indicator (group 2) and a comment after it if there are any, or
// "=" (group 1 is the key).
var seqKeyEnd = regexp.MustCompile(`(?i)(` + bareKeyExpr + `)(?:\\*["'])?(?:` + keySpaceExpr + `*:(?:` + keySpaceExpr +
	`*([|>][1-9+-]{0,2})(?:` + keySpaceExpr + `+#.*)?)?|` + keySpaceExpr + `*=)$`)

// seqFlagEnd matches a secret-named flag at the end of a string, alone or after other text that does not run
// into its dashes (a blank, "=", a quote, a colon: "app --password", "--args=--password", "\"--password");
// group 1 is the name.
var seqFlagEnd = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])-{1,2}(` + bareKeyExpr + `)$`)

// seqWordEnd matches a secret key at the end of a string with no colon after it, alone or after other words
// (group 1): its colon or equals sign may come in a string of its own, or at the start of the next.
var seqWordEnd = regexp.MustCompile(`(?i)(?:^|\s)(` + bareKeyExpr + `)(?:\\*["'])?$`)

// seqBlockHeader matches what may follow a separator in a string of its own: nothing, or a block indicator and
// a comment.
var seqBlockHeader = regexp.MustCompile(`^(?:[|>][1-9+-]{0,2}(?:` + keySpaceExpr + `+#.*)?)?$`)

// seqEnvValue matches the start of the string after an environment entry's name (viewEnvName): its "value" key
// and separator, quoted or not.
var seqEnvValue = regexp.MustCompile(`(?i)^\\*["']?value\\*["']?` + keySpaceExpr + `*:` + keySpaceExpr + `*`)

// seqSeparator reports whether the string t holds only a separator run of the class (sepRunAt: blanks, control
// and invisible characters, colons and their look-alikes, equals signs and arrows, with at least one mark), with
// a block indicator and a comment after it if there are any; and whether it holds a colon, which may open a
// block.
func seqSeparator(t string) (ok, colon bool) {
	r, found := sepRunAt(t, 0)
	if !found || !seqBlockHeader.MatchString(counted(t[r.end:])) {
		return false, false
	}
	return true, strings.Contains(r.marks, ":")
}

// seqTrim returns t without blanks at either end: indentation characters (indentLen), line breaks, and line
// breaks and tabs escaped with backslashes, as a string written inside another holds them.
func seqTrim(t string) string {
	for len(t) > 0 {
		tick(1)
		if n := indentLen(t, 0); n > 0 {
			t = t[n:]
			continue
		}
		if t[0] == '\n' || t[0] == '\r' {
			t = t[1:]
			continue
		}
		if n := escapedBlankAt(t, 0); n > 0 {
			t = t[n:]
			continue
		}
		break
	}
	for len(t) > 0 {
		tick(1)
		if c := t[len(t)-1]; c == '\n' || c == '\r' {
			t = t[:len(t)-1]
			continue
		}
		if c := t[len(t)-1]; (c == 'n' || c == 'r' || c == 't') && len(t) > 1 && t[len(t)-2] == '\\' {
			j := len(t) - 2
			for j > 0 && t[j-1] == '\\' {
				tick(1)
				j--
			}
			t = t[:j]
			continue
		}
		if n := lastIndentLen(t, len(t)); n > 0 {
			t = t[:len(t)-n]
			continue
		}
		break
	}
	return t
}

// escapedBlankAt returns the length of a line break or tab escaped with backslashes at t[i], or 0.
func escapedBlankAt(t string, i int) int {
	j := i
	for j < len(t) && t[j] == '\\' {
		tick(1)
		j++
	}
	if j > i && j < len(t) && (t[j] == 'n' || t[j] == 'r' || t[j] == 't') {
		return j + 1 - i
	}
	return 0
}

// seqValueStart returns where the value in item starts: after its indentation characters and escaped blanks.
func seqValueStart(item string) int {
	i := 0
	for i < len(item) {
		tick(1)
		n := indentLen(item, i)
		if n == 0 {
			n = escapedBlankAt(item, i)
		}
		if n == 0 {
			break
		}
		i += n
	}
	return i
}

// seqValues decides, for the strings of a sequence, which are the value of a secret key in an earlier string
// (#845), and returns where the value starts in each, or -1. A string is a value when the non-blank string
// before it, blank strings skipped, ends with a secret key and ":" or "=" (seqKeyEnd), or with a secret-named
// flag (seqFlagEnd), or when a string that holds only a separator (seqSeparator) stands between it and a string
// that ends with a secret key (seqWordEnd), or between it and a key that already took it ("--password", "=",
// "V"). After a string that ends with a secret key, a string that starts with a separator holds the value after
// it ("? password", ": V"), and after an environment entry's name ("- name: DB_PASSWORD"), a string that starts
// with its "value" key holds the value after that ("  value: V"). After a key and a colon, with a block
// indicator or nothing after it, the strings that start with indentation after its value are the lines of its
// block. A reference key or flag keeps its value, and under a bare short key the value counts only when a word
// of it looks like a secret. Every string is also read as a key of its own, a value included ("--token",
// "--password", "V"). Each string is read in its folded view (foldView), as String reads a text: an invisible
// character inside a key or a flag, or before its separator, is dropped, a colon look-alike reads as ":", and a
// separator of other characters reads as ": ".
func seqValues(items []string) []int {
	const (
		none = iota
		value
		word
		env
	)
	out := make([]int, len(items))
	pending, bare, block, inBlock := none, false, false, false
	for i, it := range items {
		tick(1)
		out[i] = -1
		t := seqTrim(it)
		if f, ok := foldView(t); ok {
			// What a reader sees: an invisible character inside a key or before its separator, a colon
			// look-alike, a separator of other characters.
			t = seqTrim(f.text)
		}
		if t == "" {
			continue
		}
		if inBlock && seqValueStart(it) > 0 {
			out[i] = seqValueStart(it)
			continue
		}
		inBlock = false
		take := func(start int, v string) {
			if !bare || wordsLookLikeSecret(v) {
				out[i] = start
			}
		}
		switch pending {
		case value:
			if ok, colon := seqSeparator(t); ok {
				// "--password", "=", "V": the separator in a string of its own.
				block = block || colon
				continue
			}
			take(seqValueStart(it), t)
			inBlock = block && out[i] >= 0
			pending = none
		case word:
			pending = none
			if ok, colon := seqSeparator(t); ok {
				pending, block = value, colon
				continue
			}
			// "? password", ": V": the separator and the value in the next string.
			start := seqValueStart(it)
			if r, ok := sepRunAt(it, start); ok && r.end < len(it) {
				take(r.end+seqValueStart(it[r.end:]), it[r.end:])
			}
		case env:
			pending = none
			start := seqValueStart(it)
			if m := seqEnvValue.FindStringIndex(counted(it[start:])); m != nil && start+m[1] < len(it) {
				take(start+m[1], it[start+m[1]:])
			}
		}
		if m := viewEnvName.FindStringSubmatchIndex(counted(t)); m != nil {
			if !isReference(t[m[2]:m[3]]) {
				pending, bare = env, false
			}
			continue
		}
		if m := seqKeyEnd.FindStringSubmatchIndex(counted(t)); m != nil {
			if !isReference(t[m[2]:m[3]]) {
				// A colon may have a block below it, with an indicator or with nothing after it.
				pending, bare, block = value, isBareShortKey(t[m[2]:m[3]]), t[len(t)-1] != '='
			}
			continue
		}
		if m := seqFlagEnd.FindStringSubmatchIndex(counted(t)); m != nil {
			if !referenceKey(t[m[2]:m[3]], true) {
				pending, bare, block = value, false, false
			}
			continue
		}
		if m := seqWordEnd.FindStringSubmatchIndex(counted(t)); m != nil && !isReference(t[m[2]:m[3]]) {
			pending, bare = word, isBareShortKey(t[m[2]:m[3]])
		}
	}
	return out
}

// Strings redacts the strings of a sequence, such as a container's arguments, with the one decision String
// takes on them joined by line breaks: each string loses what String redacts of it in the joined text, what
// String redacts of it alone, and, after its indentation, the value seqValues finds in it (the rule for a key,
// a separator or a flag in one string and the value in a later one, which String also applies to a sequence
// written in a text). So what String hides in the strings joined, Strings hides in them apart. Callers that walk
// an object (the read tool get_resource) call it for every sequence of strings instead of redacting the strings
// one by one.
func (r *Redactor) Strings(items []string) []string {
	joined := r.spans(strings.Join(items, "\n"))
	starts := seqValues(items)
	out := make([]string, len(items))
	off, next := 0, 0
	for i, it := range items {
		end := off + len(it)
		var own []span
		for next < len(joined) && joined[next].end <= off {
			tick(1)
			next++
		}
		for k := next; k < len(joined) && joined[k].start < end; k++ {
			tick(1)
			sp := joined[k]
			sp.start, sp.end = max(sp.start, off)-off, min(sp.end, end)-off
			if sp.end > sp.start {
				own = append(own, sp)
			}
		}
		own = append(own, r.spans(it)...)
		if starts[i] >= 0 && starts[i] < len(it) {
			own = append(own, span{start: starts[i], end: len(it), kind: KindCredential, rule: len(r.rules)})
		}
		out[i] = render(it, mergeSpans(own))
		off = end + 1
	}
	return out
}

// Strings is the package's Strings with the built-in rules.
func Strings(items []string) []string { return defaultRedactor.Strings(items) }

// seqBoundary matches the boundary between two double-quoted strings of a sequence written in a text: the
// closing quote, a separator with a comma or a line break in it (plain or escaped, with blanks and a YAML list
// dash), and the opening quote of the next string. Groups: 1 the backslashes before the closing quote, 2 those
// before the opening quote.
var seqBoundary = regexp.MustCompile(`(\\*)"(?:[ \t]|\\+[rt])*(?:,|\r\n?|\n|\\+n)(?:[ \t,]|\r\n?|\n|\\+[nrt]|-[ \t])*(\\*)"`)

// seqScan is the rule for the strings of a sequence written in a text (#845): JSON or a YAML flow or block
// sequence of double-quoted strings, plain or escaped inside another string. It reads each run of strings at
// one escape level, joined by boundaries (seqBoundary), and redacts the strings seqValues takes, after their
// indentation, up to their closing quote. The first string of a run is read back from its closing quote to its
// opening one, no further than the end of the run before; the last one forward to its closing quote, on its
// line and before the next boundary; so each byte is read a fixed number of times.
func seqScan(s string, add func(start, end int)) {
	ms := seqBoundary.FindAllStringSubmatchIndex(counted(s), -1)
	floor := 0
	for i := 0; i < len(ms); {
		tick(1)
		m := ms[i]
		bs := m[3] - m[2]
		if bs != m[5]-m[4] {
			floor = m[1]
			i++
			continue
		}
		level := quoteLevel(bs)
		var items [][2]int
		if open := seqOpenBefore(s, m[2], floor, level); open >= 0 {
			items = append(items, [2]int{open, m[2]})
		}
		j := i
		for {
			tick(1)
			start := ms[j][1]
			limit := len(s)
			if j+1 < len(ms) {
				limit = ms[j+1][2]
			}
			if j+1 < len(ms) && ms[j+1][3]-ms[j+1][2] == bs && ms[j+1][5]-ms[j+1][4] == bs &&
				seqClosesAt(s, start, limit, level) < 0 {
				items = append(items, [2]int{start, limit})
				j++
				continue
			}
			if end := seqClosesAt(s, start, limit, level); end >= 0 && end == seqLineEnd(s, start, end) {
				items = append(items, [2]int{start, end})
			}
			break
		}
		texts := make([]string, len(items))
		for k, it := range items {
			texts[k] = s[it[0]:it[1]]
		}
		for k, start := range seqValues(texts) {
			if start >= 0 {
				add(items[k][0]+start, items[k][1])
			}
		}
		floor = ms[j][1]
		i = j + 1
	}
}

// seqOpenBefore returns where the string whose closing quote's backslashes start at end begins: after the
// opening double quote of the same escape level before it, read back no further than floor; or -1 when a
// quote of a lower level comes first or there is none.
func seqOpenBefore(s string, end, floor, level int) int {
	for i := end - 1; i >= floor; i-- {
		tick(1)
		if s[i] != '"' {
			continue
		}
		run := 0
		for i-run-1 >= floor && s[i-run-1] == '\\' {
			tick(1)
			run++
		}
		switch l := quoteLevel(run); {
		case l == level:
			return i + 1
		case l < level:
			return -1
		}
	}
	return -1
}

// seqClosesAt returns where the first double quote from start to limit that closes a string of the given
// escape level starts (its backslashes), or -1: a quote escaped more deeply stands inside the string.
func seqClosesAt(s string, start, limit, level int) int {
	run := 0
	for i := start; i < limit; i++ {
		tick(1)
		switch c := s[i]; {
		case c == '\\':
			run++
			continue
		case c == '"' && quoteLevel(run) <= level:
			return i - run
		}
		run = 0
	}
	return -1
}

// seqLineEnd returns end, or the first line break from start before it.
func seqLineEnd(s string, start, end int) int {
	if i := indexAny(s[start:end], "\r\n"); i >= 0 {
		return start + i
	}
	return end
}

// connStringSpan is the span function of the rule for a URL with user info: the whole match, except for a URL
// in angle brackets whose closing bracket comes before the "@" (autolinkClosed), which is no URL with user
// info at all.
func connStringSpan(s string, m []int, _ int) (int, int, bool) {
	return m[0], m[1], !autolinkClosed(s, m[0], m[1])
}

// autolinkClosed reports whether the URL match s[start:end] stands right after "<" and a ">" closes it before
// the "@" of its user info: a ">" followed by the end of the text or a blank, or by a comma or a semicolon and
// then the end, a blank or the "<" of the next link, as the bracket that closes a link in a list is. A ">"
// glued to more text ("Hunter>2", "Hunter><2", "Hunter>,2") is part of the user info, as in a password. Such
// brackets are not allowed unencoded in a URL's user info; a raw password holding ">" and then one of these
// closers inside angle brackets is the one form this leaves out (#845). A URL whose text before the bracket
// holds a user and a password (autolinkPassword) is not closed there.
func autolinkClosed(s string, start, end int) bool {
	if start == 0 || s[start-1] != '<' {
		return false
	}
	at := start + indexByte(s[start:end], '@')
	for i := start; i < at; i++ {
		tick(1)
		if s[i] != '>' {
			continue
		}
		j := i + 1
		if j < len(s) && (s[j] == ',' || s[j] == ';') {
			j++
			if j < len(s) && s[j] == '<' {
				return !autolinkPassword(s, start, i)
			}
		}
		if j == len(s) || indentLen(s, j) > 0 {
			return !autolinkPassword(s, start, i)
		}
	}
	return false
}

// autolinkPassword reports whether the URL in angle brackets from start to its closing bracket at gt holds a
// user and a password ("<postgres://alice:mint>"): a colon after its "://" with text after it that is no port
// number. Such a URL is no link of a list: its match is kept whole, as main kept it (#845, fourth round).
func autolinkPassword(s string, start, gt int) bool {
	sep := strings.Index(s[start:gt], "://")
	if sep < 0 {
		return false
	}
	auth := s[start+sep+3 : gt]
	c := strings.IndexByte(auth, ':')
	if c < 0 || strings.HasPrefix(auth, "[") {
		// No password, or an IPv6 address.
		return false
	}
	for i := c + 1; i < len(auth); i++ {
		tick(1)
		if auth[i] < '0' || auth[i] > '9' {
			return true
		}
	}
	return false
}
