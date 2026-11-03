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

// Package redact removes secrets, tokens, connection strings and personal data from text before it is
// stored or sent to a model (design 10.3, ADR-0015). Callers run every piece of cluster data through one
// [Redactor] at the boundary; the package does not decide where that boundary is.
//
// Each match is replaced by a marker that names its kind, such as "[REDACTED:jwt]", so a reader still knows
// what was there. The output is never longer than the input: when a marker does not fit in the matched
// text, the first shorter one that fits is used: "[jwt]", then "[REDACTED]", then "***", then one "*" per
// byte (only for matches of one or two bytes). Matching uses RE2 (package regexp), so the time
// taken is linear in the input and no input can make the redactor backtrack or allocate without bound.
//
// Redaction is best effort and errs on the side of removing too much: an IP address or an email in a log
// line is removed even when it is harmless, and so are the user name of any URL ("ssh://git@host"), long
// runs that mix letters and digits, such as bare Git commit hashes and container IDs, and every scalar value
// under a secret-named YAML key ("credentials:\n  user: admin" loses "admin"). Values that look like
// references rather than secrets, such as "secretName: db-creds", are kept, because the name of a missing
// Secret is what a diagnosis needs, and so are the hex of an image digest and a commit in the form Flux
// writes revisions ("main@sha1:" and 40 lower-case hex digits).
//
// Where a rule would also match prose, it asks the value to look like a credential: the word after "Basic"
// or "Bearer", and the value of the short keys "pass", "auth" and "sig" before a colon, on their own ("pass:
// 12 fail: 0") or after one of a few prose words and a hyphen ("pre-auth: ok", "two-pass: true"), inline or
// in a block on the following lines, count only with a value of at least 8 bytes that mixes ASCII letters
// with digits or symbols (blanks too when the value is quoted, as in a passphrase; 16 bytes of anything after
// Basic or Bearer, or base64 of "user:password" after Basic). So "Bearer abc12" and "auth: hunter2" pass
// through, while any other hyphenated key ("db-pass", "proxy-auth") always counts. Any byte that is not an
// ASCII letter counts as a symbol, so "auth: déjàvuxx" and "auth: unauthorized," are redacted, and so is
// "basic internationalization": over-redaction kept by decision (#194), because these cannot be told apart
// from a password or token.
//
// Known gaps: JSON escaped more than three times, a Kubernetes environment entry whose "value" comes before
// its "name" or more than 16 lines after it, and a secret in a format no rule knows.
package redact

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"iter"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Kinds of redacted data, used in markers such as "[REDACTED:jwt]".
const (
	KindPrivateKey       = "private-key"
	KindJWT              = "jwt"
	KindToken            = "token"
	KindConnectionString = "connection-string"
	KindCredential       = "credential"
	KindEmail            = "email"
	KindIP               = "ip"
)

// Pattern is an extra rule supplied by the operator, for data the built-in rules do not know about.
type Pattern struct {
	// Kind names the data in the marker. Lower-case letters, digits and hyphens, starting with a letter,
	// at most 32 characters.
	Kind string
	// Expr is an RE2 regular expression. If it has capture groups, only the text of the groups that
	// matched is redacted; otherwise the whole match is.
	Expr string
}

// rule is one compiled pattern. Spans of the listed groups are redacted, or the whole match when groups is
// empty. span, when set, replaces groups: it receives the submatches and the end of the previous span it returned for this rule, and
// returns the byte range to redact, or ok false to leave the match alone.
type rule struct {
	kind   string
	re     *regexp.Regexp
	groups []int
	span   func(s string, m []int, prevEnd int) (start, end int, ok bool)
	// spans, when set, replaces span for a rule that redacts several ranges per match: it calls add for
	// each and returns the end of what it read, which later matches of the rule before it skip.
	spans func(s string, m []int, prevEnd int, add func(start, end int)) int
}

// Redactor replaces sensitive data in text. It is safe for concurrent use.
type Redactor struct {
	rules []rule
}

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// New returns a redactor with the built-in rules plus the given extra patterns. It fails if a pattern has
// an invalid kind or expression.
func New(extra ...Pattern) (*Redactor, error) {
	rules := builtinRules()
	for i, p := range extra {
		if !kindPattern.MatchString(p.Kind) {
			return nil, fmt.Errorf("extra pattern %d: kind %q must match %s", i, p.Kind, kindPattern)
		}
		re, err := regexp.Compile(p.Expr)
		if err != nil {
			return nil, fmt.Errorf("extra pattern %d (%s): %w", i, p.Kind, err)
		}
		r := rule{kind: p.Kind, re: re}
		for g := 1; g <= re.NumSubexp(); g++ {
			r.groups = append(r.groups, g)
		}
		rules = append(rules, r)
	}
	return &Redactor{rules: rules}, nil
}

var defaultRedactor = &Redactor{rules: builtinRules()}

// String redacts s with the built-in rules.
func String(s string) string {
	return defaultRedactor.String(s)
}

// span is a byte range of the input to replace, with the kind of the rule that found it.
type span struct {
	start, end int
	kind       string
	rule       int
}

// String returns s with every match of every rule replaced by a marker. Overlapping matches are merged
// into one, named after the match that starts first (the first rule, on a tie).
func (r *Redactor) String(s string) string {
	var spans []span
	for i, ru := range r.rules {
		prevEnd := 0
		add := func(start, end int) {
			if end > start {
				spans = append(spans, span{start: start, end: end, kind: ru.kind, rule: i})
			}
		}
		for _, m := range ru.re.FindAllStringSubmatchIndex(s, -1) {
			if ru.spans != nil {
				prevEnd = ru.spans(s, m, prevEnd, add)
				continue
			}
			if ru.span != nil {
				if start, end, ok := ru.span(s, m, prevEnd); ok && end > start {
					spans = append(spans, span{start: start, end: end, kind: ru.kind, rule: i})
					prevEnd = end
				}
				continue
			}
			if len(ru.groups) == 0 {
				if m[1] > m[0] {
					spans = append(spans, span{start: m[0], end: m[1], kind: ru.kind, rule: i})
				}
				continue
			}
			for _, g := range ru.groups {
				if 2*g+1 < len(m) && m[2*g] >= 0 && m[2*g+1] > m[2*g] {
					spans = append(spans, span{start: m[2*g], end: m[2*g+1], kind: ru.kind, rule: i})
				}
			}
		}
	}
	if len(spans) == 0 {
		return s
	}
	slices.SortFunc(spans, func(a, b span) int {
		if a.start != b.start {
			return a.start - b.start
		}
		if a.rule != b.rule {
			return a.rule - b.rule
		}
		return b.end - a.end
	})

	var b strings.Builder
	b.Grow(len(s))
	pos := 0
	for i := 0; i < len(spans); {
		cur := spans[i]
		for i++; i < len(spans) && spans[i].start < cur.end; i++ {
			cur.end = max(cur.end, spans[i].end)
		}
		b.WriteString(s[pos:cur.start])
		b.WriteString(marker(cur.kind, cur.end-cur.start))
		pos = cur.end
	}
	b.WriteString(s[pos:])
	return b.String()
}

// marker returns the replacement for n bytes of the given kind. It is never longer than n, so redaction
// never grows its input.
func marker(kind string, n int) string {
	for _, m := range []string{"[REDACTED:" + kind + "]", "[" + kind + "]", "[REDACTED]", "***"} {
		if len(m) <= n {
			return m
		}
	}
	return strings.Repeat("*", n)
}

// referenceSuffixes mark keys whose values name or point to a secret instead of holding one.
var referenceSuffixes = []string{
	"name", "names", "ref", "refs", "keyref", "path", "file", "filename", "dir", "type", "kind", "id",
	"count", "ttl", "expiry", "expires", "expiresat", "expiration", "version", "length", "len", "url", "uri",
	"endpoint", "provider", "mode", "enabled", "required", "source", "namespace", "selector", "store",
}

// keepReference reports whether the key in group 1 of m is a reference to a secret. It judges only that
// key, never a neighbouring one.
func keepReference(s string, m []int) bool {
	return isReference(s[m[2]:m[3]])
}

// keySeparators removes the separators of a key name before its suffix is compared.
var keySeparators = strings.NewReplacer("_", "", "-", "", ".", "")

// isReference reports whether key, quoted or not, names or points to a secret instead of holding one.
func isReference(key string) bool {
	key = strings.ToLower(strings.Trim(key, `"'\`))
	key = keySeparators.Replace(key)
	for _, suffix := range referenceSuffixes {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// bareShortColon reports whether the key in group 1 of m is a bare short key (isBareShortKey), unquoted and
// followed by a colon. In that form it is as often a label in prose ("pass: 12 fail: 0", "auth: failed for
// user bob", "pre-auth: ok") as a key, so its value counts only when it looks like a secret. As "key=value",
// quoted, or as the last part of any other longer key ("DB_PASS", "basic_auth", "db-pass", "proxy-auth") it
// always counts.
func bareShortColon(s string, m []int) bool {
	if !isBareShortKey(s[m[2]:m[3]]) {
		return false
	}
	i := m[3]
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	return i < len(s) && s[i] == ':'
}

// proseShortKeyPrefixes are the words that, before a hyphen, make a short key read as prose ("pre-auth",
// "two-pass", "multi-sig") rather than as a key. The list is closed and short on purpose: any other word
// ("db-pass", "admin-pass", "proxy-auth") names what the credential is for, so such a key always counts.
var proseShortKeyPrefixes = []string{"pre", "post", "re", "two", "multi", "single"}

// isBareShortKey reports whether key is one of the short names "pass", "auth" or "sig" on its own, in any
// case, or in lower case after exactly one word of proseShortKeyPrefixes and a hyphen ("pre-auth", but not
// "Pre-Auth", "pre-x-auth" or "db-pass").
func isBareShortKey(key string) bool {
	switch strings.ToLower(key) {
	case "pass", "auth", "sig":
		return true
	}
	prefix, short, ok := strings.Cut(key, "-")
	if !ok {
		return false
	}
	switch short {
	case "pass", "auth", "sig":
		return slices.Contains(proseShortKeyPrefixes, prefix)
	}
	return false
}

// looksLikeSecret reports whether v has the shape of a credential rather than of prose: at least 8 bytes,
// no whitespace, and a letter together with a digit or a symbol.
func looksLikeSecret(v string) bool {
	return !strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' }) && looksLikeQuotedSecret(v)
}

// looksLikeQuotedSecret is looksLikeSecret for a quoted value, which may hold blanks (a passphrase such as
// "correct horse battery staple"): at least 8 bytes and a letter together with a digit, a symbol or a blank.
func looksLikeQuotedSecret(v string) bool {
	if len(v) < 8 {
		return false
	}
	var letter, other bool
	for i := range len(v) {
		c := v[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			letter = true
		} else {
			other = true
		}
	}
	return letter && other
}

// lineLooksLikeSecret reports whether the text of one line of a block value, after any sequence entry
// indicators, looks like a secret: as a quoted value (looksLikeQuotedSecret) when it is quoted, otherwise as
// a plain one (looksLikeSecret).
func lineLooksLikeSecret(text string) bool {
	text = strings.TrimRight(text[dashes(text):], " \t")
	if len(text) >= 2 && (text[0] == '"' || text[0] == '\'') && text[len(text)-1] == text[0] {
		return looksLikeQuotedSecret(text[1 : len(text)-1])
	}
	return looksLikeSecret(text)
}

// blockLooksLikeSecret reports whether any line of the block value s[start:end] looks like a secret
// (lineLooksLikeSecret).
func blockLooksLikeSecret(s string, start, end int) bool {
	for line := range strings.SplitSeq(s[start:end], "\n") {
		if lineLooksLikeSecret(strings.TrimLeft(line, " \t\r")) {
			return true
		}
	}
	return false
}

// minSchemeWord is the length from which any value after Basic or Bearer counts: longer than the words
// that follow them in prose ("authentication").
const minSchemeWord = 16

// schemeSpan judges the value after "Basic" or "Bearer" (group 2) outside an Authorization header. Prose
// follows these words too ("failed basic health check", "missing Bearer token"), so the value counts when it
// looks like a credential (looksLikeSecret), is at least minSchemeWord bytes long (an opaque token of
// letters only), or, after Basic, is padded base64 of "user:password" in printable text, which catches
// short ones such as "dTpw" ("u:p").
func schemeSpan(s string, m []int, _ int) (int, int, bool) {
	v := s[m[4]:m[5]]
	if looksLikeSecret(v) || len(v) >= minSchemeWord || strings.EqualFold(s[m[2]:m[3]], "basic") && isBasicCredential(v) {
		return m[4], m[5], true
	}
	return 0, 0, false
}

// isBasicCredential reports whether v is standard padded base64 of printable text holding a colon.
func isBasicCredential(v string) bool {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil || bytes.IndexByte(b, ':') < 0 || !utf8.Valid(b) {
		return false
	}
	return !bytes.ContainsFunc(b, func(r rune) bool { return r < ' ' || r == 0x7f })
}

// blockIndicator matches a YAML block scalar header value such as "|", ">-" or "|2+".
var blockIndicator = regexp.MustCompile(`^[|>][1-9+-]{0,2}$`)

// isAlnum reports whether c is an ASCII letter or digit.
func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// ipv6Span confirms an IPv6 candidate with net/netip, so clock times and similar are kept. A single colon
// at the end that belongs to the surrounding text ("addr ::1: refused") is trimmed, and when a label is
// glued to the front ("source:2001:db8::1" gives the candidate "ce:2001:db8::1"), the address after the
// label's colon is tried. A range glued to letters or digits on either side is kept, as it is part of a
// longer token. Candidates are at most about 45 bytes, so the retries cost constant time.
func ipv6Span(s string, m []int, _ int) (int, int, bool) {
	start, end := m[0], m[1]
	ends := []int{end}
	if end-start > 1 && s[end-1] == ':' && s[end-2] != ':' {
		ends = append(ends, end-1)
	}
	for a := start; a < end; a++ {
		if a > start && (s[a-1] != ':' || s[a] == ':') {
			continue
		}
		if a > 0 && isAlnum(s[a-1]) {
			continue
		}
		for _, b := range ends {
			if b <= a || (b < len(s) && isAlnum(s[b])) {
				continue
			}
			if addr, err := netip.ParseAddr(s[a:b]); err == nil && addr.Is6() {
				return a, b, true
			}
		}
	}
	return 0, 0, false
}

// lineEnd returns the index of the end of the line holding pos, before any "\r\n" or "\n".
func lineEnd(s string, pos int) int {
	end := len(s)
	if i := strings.IndexByte(s[pos:], '\n'); i >= 0 {
		end = pos + i
	}
	if end > pos && s[end-1] == '\r' {
		end--
	}
	return end
}

// blockLine is one non-blank line of a YAML block: where the line starts, where its text starts after the
// indentation, and where it ends, before any line break.
type blockLine struct {
	start, text, end int
}

// indent returns the column of the line's text.
func (l blockLine) indent() int { return l.text - l.start }

// Kinds of block read by blockLines.
const (
	// blockScalar: the lines indented deeper than the key, as in a literal or folded block scalar or the
	// continuation lines of a plain scalar. A comment line is content like any other.
	blockScalar = iota
	// blockValue: the value of a key with nothing after its colon. Comment lines at any indentation are
	// skipped, and a compact sequence ("key:\n- item") may start at the key's own indentation.
	blockValue
)

// blockLines returns, in order, the non-blank lines after pos (which is at a line end) that belong to the
// block of a key at column keyCol, and stops at the first line that does not. Comment lines of a blockValue
// are skipped without being returned. Each line is read once.
func blockLines(s string, pos, keyCol, kind int) iter.Seq[blockLine] {
	return func(yield func(blockLine) bool) {
		for {
			l, next, ok := nextBlockLine(s, pos, keyCol, kind)
			if !ok || !yield(l) {
				return
			}
			pos = next
		}
	}
}

// nextBlockLine returns the first line after pos (which is at a line end) that blockLines would return, and
// the end of that line, from which the next one is read. ok is false when the block ends first.
func nextBlockLine(s string, pos, keyCol, kind int) (blockLine, int, bool) {
	for pos < len(s) && (s[pos] == '\n' || s[pos] == '\r') {
		if s[pos] == '\r' {
			pos++
			continue
		}
		l := blockLine{start: pos + 1, end: lineEnd(s, pos+1)}
		pos = l.end
		text := s[l.start:l.end]
		trimmed := strings.TrimLeft(text, " \t")
		if trimmed == "" {
			continue
		}
		l.text = l.start + len(text) - len(trimmed)
		if kind == blockValue && trimmed[0] == '#' {
			continue
		}
		deeper := l.indent() > keyCol
		compactSequence := kind == blockValue && l.indent() == keyCol && dashes(trimmed) > 0
		if !deeper && !compactSequence {
			break
		}
		return l, pos, true
	}
	return blockLine{}, pos, false
}

// dashes returns the length of the sequence entry indicators ("- ", "- - ") at the start of text.
func dashes(text string) int {
	n := 0
	for n < len(text) && text[n] == '-' && (n+1 == len(text) || text[n+1] == ' ' || text[n+1] == '\t') {
		n++
		for n < len(text) && (text[n] == ' ' || text[n] == '\t') {
			n++
		}
	}
	return n
}

// indentedLines scans the lines after pos, which is at a line end, while they are blank or indented
// deeper than keyCol. It returns the start of the first non-blank one and the end of the last, or -1, -1.
func indentedLines(s string, pos, keyCol int) (int, int) {
	start, end := -1, -1
	for l := range blockLines(s, pos, keyCol, blockScalar) {
		if start < 0 {
			start = l.text
		}
		end = l.end
	}
	return start, end
}

// blockScalarSpan returns the content of a YAML literal or folded block scalar whose header line the rule
// matched: the following lines that are blank or indented deeper than the key, up to the last non-blank
// one. With no content, the indicator itself (group 2) is redacted. Headers inside a block already
// redacted are skipped, so each byte is scanned once. Under a bare short key (bareShortColon) the content
// counts only when one of its lines looks like a secret, and an empty block is kept.
func blockScalarSpan(s string, m []int, prevEnd int) (int, int, bool) {
	if m[0] < prevEnd || keepReference(s, m) {
		return 0, 0, false
	}
	bare := bareShortColon(s, m)
	start, end := indentedLines(s, m[1], m[2]-m[0])
	if start < 0 {
		return m[4], m[5], !bare
	}
	return start, end, !bare || blockLooksLikeSecret(s, start, end)
}

// childKey matches a mapping key at the start of a line's text, after any sequence entry indicators. Group
// 1 is the key; the value starts at the end of the match.
var childKey = regexp.MustCompile(`^(?:-[ \t]+)*("[^"\r\n]*"|'[^'\r\n]*'|[^\s"'#:-][^#:\r\n]*?|-[^\s#:][^#:\r\n]*?)[ \t]*:(?:[ \t]+|$)`)

// inlineValue returns the value that starts at pos and ends at the line end le: the text between the
// quotes of a quoted value, otherwise the text up to the line end without trailing blanks.
func inlineValue(s string, pos, le int) (int, int) {
	if pos >= le {
		return pos, pos
	}
	if c := s[pos]; c == '"' || c == '\'' {
		if qe := quotedEnd(s, pos); qe > 0 {
			return pos + 1, qe - 1
		}
	}
	end := le
	for end > pos && isSpace(s[end-1]) {
		end--
	}
	return pos, end
}

// nextLineSpans redacts the value of a YAML key with nothing after its colon (but blanks or a comment),
// written on the following lines: those indented deeper than the key, comment lines at any indentation,
// and the entries of a compact sequence at the key's own indentation. A key with no such lines is truly
// empty and kept.
//
// A scalar value (plain, quoted, a list of scalars or a flow collection) is redacted whole. A mapping
// keeps its keys and loses the scalar values of its children, at every depth, so "credentials:\n  user:
// admin" loses "admin" as well as any secret-named child. Children whose key is a reference
// ("secretRef", "name") keep their values and everything below them, except the value of a secret key
// with nothing after its colon among them, which is redacted in the same way.
//
// Under a bare short key (bareShortColon) a value counts only when it looks like a secret: a scalar when
// one of its lines does, and in a mapping each value on its own.
//
// The block is read once and its end is returned, so headers inside it are skipped and each byte is
// scanned once.
func nextLineSpans(s string, m []int, prevEnd int, add func(start, end int)) int {
	if m[0] < prevEnd || keepReference(s, m) {
		return prevEnd
	}
	return max(prevEnd, nextLineBlock(s, m[1], m[2]-m[0], bareShortColon(s, m), add))
}

// nextLineHeader matches, on one line, the header the next-line rule looks for (see builtinRules). Group 1
// is the key.
var nextLineHeader = regexp.MustCompile(`(?i)^` + nextLineHeaderExpr)

// nestedHeader returns the submatches, as offsets into s, of the next-line rule's header on line l, or nil
// when l holds none or its key is a reference.
func nestedHeader(s string, l blockLine) []int {
	hm := nextLineHeader.FindStringSubmatchIndex(s[l.start:l.end])
	if hm == nil {
		return nil
	}
	for i := range hm {
		if hm[i] >= 0 {
			hm[i] += l.start
		}
	}
	if keepReference(s, hm) {
		return nil
	}
	return hm
}

// nextLineBlock redacts the value of the key at column keyCol whose header line ends at pos, as described
// for nextLineSpans, and returns the end of the last line of the block, or pos when it has none. With bare,
// each value counts only when it looks like a secret.
func nextLineBlock(s string, pos, keyCol int, bare bool, add func(start, end int)) int {
	const (
		undecided = iota
		scalar
		mapping
	)
	addValue := func(start, end int) {
		if !bare || blockLooksLikeSecret(s, start, end) {
			add(start, end)
		}
	}
	mode := undecided
	start, end := -1, pos
	// keepBelow and redactBelow are the key columns of a reference child, whose lines below are kept, and
	// of a child with a block scalar, whose lines below are redacted whole; -1 when there is none.
	keepBelow, redactBelow := -1, -1
	for pos < len(s) {
		l, next, ok := nextBlockLine(s, pos, keyCol, blockValue)
		if !ok {
			break
		}
		pos, end = next, l.end
		text := s[l.text:l.end]
		if mode == undecided {
			start, mode = l.text+dashes(text), scalar
			if childKey.MatchString(text) {
				mode = mapping
			}
		}
		if mode == scalar {
			continue
		}
		kept := keepBelow >= 0 && l.indent() > keepBelow
		if !kept {
			keepBelow = -1
		}
		inBlockScalar := !kept && redactBelow >= 0 && l.indent() > redactBelow
		if !kept && !inBlockScalar {
			redactBelow = -1
		}
		if kept || bare && !inBlockScalar {
			// A secret key with nothing after its colon, below a reference or under a bare short key, is
			// judged on its own, as a match of the rule would be. Its block is read here and skipped, so
			// each line is still read once.
			if hm := nestedHeader(s, l); hm != nil {
				pos = nextLineBlock(s, l.end, hm[2]-hm[0], bareShortColon(s, hm), add)
				end = pos
				continue
			}
		}
		if kept {
			continue
		}
		if inBlockScalar {
			addValue(inlineValue(s, l.text, l.end))
			continue
		}
		cm := childKey.FindStringSubmatchIndex(text)
		if cm == nil {
			// A scalar line: a sequence entry, a flow collection or a continuation.
			addValue(inlineValue(s, l.text+dashes(text), l.end))
			continue
		}
		keyCol := l.indent() + cm[2]
		if isReference(text[cm[2]:cm[3]]) {
			keepBelow = keyCol
			continue
		}
		vpos := l.text + cm[1]
		switch {
		case vpos >= l.end || s[vpos] == '#':
			// An empty value, maybe with a comment: its children follow on deeper lines.
		case isBlockHeader(s, vpos):
			redactBelow = keyCol
		default:
			addValue(inlineValue(s, vpos, l.end))
		}
	}
	if mode == scalar && (!bare || blockLooksLikeSecret(s, start, end)) {
		add(scalarValue(s, start, end))
	}
	return end
}

// scalarValue returns the part of the scalar block value s[start:end] to redact: the text between the
// quotes of a quoted scalar that is the whole value, so it keeps its quotes, otherwise all of it.
func scalarValue(s string, start, end int) (int, int) {
	if start < end && (s[start] == '"' || s[start] == '\'') {
		if qe := quotedEnd(s, start); qe > 0 && strings.TrimRight(s[qe:end], " \t") == "" {
			return start + 1, qe - 1
		}
	}
	return start, end
}

// envEntryLines bounds the lines of an environment entry read to find its "value": Kubernetes writes it
// right after "name", or after "valueFrom".
const envEntryLines = 16

// envValueSpan redacts the value of a Kubernetes environment variable whose name holds a secret, in the
// block form "- name: DB_PASSWORD\n  value: ...": the "value" key among the entry's own keys, inline or on
// the following lines. A name that is a reference ("DB_PASSWORD_FILE") keeps its value. At most
// envEntryLines lines of the entry are read, so nested entries cost constant work per match.
func envValueSpan(s string, m []int, prevEnd int) (int, int, bool) {
	if m[0] < prevEnd || keepReference(s, m) {
		return 0, 0, false
	}
	line := s[m[0]:m[1]]
	trimmed := strings.TrimLeft(line, " \t")
	nameCol := len(line) - len(trimmed) + dashes(trimmed)
	read := 0
	for l := range blockLines(s, m[1], nameCol-1, blockScalar) {
		if read++; read > envEntryLines {
			break
		}
		if l.indent() != nameCol {
			continue
		}
		rest, ok := strings.CutPrefix(s[l.text:l.end], "value")
		rest = strings.TrimLeft(rest, " \t")
		if !ok || rest == "" || rest[0] != ':' {
			continue
		}
		vpos := l.end - len(rest) + 1
		for vpos < l.end && (s[vpos] == ' ' || s[vpos] == '\t') {
			vpos++
		}
		if vpos >= l.end || isBlockHeader(s, vpos) {
			start, end := indentedLines(s, l.end, nameCol)
			return start, end, start >= 0
		}
		start, end := valueSpan(s, vpos, boundaryLine, nameCol)
		return start, end, true
	}
	return 0, 0, false
}

// minTokenLength is the shortest part of a run of base64 or hex characters that is taken for a random token.
const minTokenLength = 24

// longTokenSpan redacts a run of base64, base64url or hex characters when some part of it between slashes
// looks random: a piece between hyphens of at least minTokenLength that mixes letters and digits, or a
// hyphenated part of at least that length that mixes upper case, lower case and digits (base64url).
// Paths and image names (short parts between slashes), long names made of letters only, Kubernetes names
// and UUIDs (lower case, short pieces between hyphens) and the hex of an image digest ("sha256:...", which
// identifies a build, not a credential, also with a suffix such as "-amd64") are kept, and so is a Git commit
// in the form Flux writes revisions ("main@sha1:" and exactly 40 lower-case hex digits), which names a commit
// in a repository, not a credential. What follows the digest's or commit's hex is judged on its own. A bare
// 40-digit hex run, without "sha1:" right before it, is still redacted.
func longTokenSpan(s string, m []int, _ int) (int, int, bool) {
	start, run := m[0], s[m[0]:m[1]]
	if prefix := strings.ToLower(s[max(0, m[0]-7):m[0]]); prefix == "sha256:" || prefix == "sha512:" {
		if digest, rest, _ := strings.Cut(run, "-"); isHex(digest) {
			start, run = m[1]-len(rest), rest
		}
	} else if sha1Prefix(s, m[0]) {
		if commit, rest, _ := strings.Cut(run, "-"); isSHA1(commit) {
			start, run = m[1]-len(rest), rest
		}
	}
	if randomRun(run) {
		return start, m[1], true
	}
	return 0, 0, false
}

// sha1Prefix reports whether "sha1:" (lower case) ends at pos and does not continue a longer word.
func sha1Prefix(s string, pos int) bool {
	const prefix = "sha1:"
	if pos < len(prefix) || s[pos-len(prefix):pos] != prefix {
		return false
	}
	return pos == len(prefix) || !isAlnum(s[pos-len(prefix)-1])
}

// isSHA1 reports whether s is exactly 40 lower-case hex digits, a SHA-1 commit hash.
func isSHA1(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// randomRun reports whether some part of run between slashes looks random, as described for longTokenSpan.
func randomRun(run string) bool {
	for part := range strings.SplitSeq(run, "/") {
		if len(part) < minTokenLength {
			continue
		}
		if strings.Contains(part, "-") && mixesCase(part) && mixesLettersAndDigits(part) {
			return true
		}
		for piece := range strings.SplitSeq(part, "-") {
			if len(piece) >= minTokenLength && mixesLettersAndDigits(piece) {
				return true
			}
		}
	}
	return false
}

// mixesCase reports whether s holds both an upper-case and a lower-case ASCII letter.
func mixesCase(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' }) &&
		strings.ContainsFunc(s, func(r rune) bool { return r >= 'a' && r <= 'z' })
}

// isHex reports whether s holds only hexadecimal digits.
func isHex(s string) bool {
	for i := range len(s) {
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// mixesLettersAndDigits reports whether s holds both an ASCII letter and a digit.
func mixesLettersAndDigits(s string) bool {
	var letter, digit bool
	for i := range len(s) {
		c := s[i]
		letter = letter || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		digit = digit || c >= '0' && c <= '9'
	}
	return letter && digit
}

// quotedEnd returns the index just past the closing quote of the quoted value starting at pos, honouring
// backslash escapes in double quotes and doubled quotes in single quotes, or -1 if the line ends first.
func quotedEnd(s string, pos int) int {
	q := s[pos]
	for i := pos + 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\n' || c == '\r':
			return -1
		case q == '"' && c == '\\':
			i++
		case c == q:
			if q == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			return i + 1
		}
	}
	return -1
}

// escapeLevel returns n when the value at pos opens with a double quote escaped n times (2^n - 1
// backslashes and a quote: JSON inside a JSON string, once, twice or three times), for n from 1 to 3, or 0.
func escapeLevel(s string, pos int) int {
	r := 0
	for pos+r < len(s) && s[pos+r] == '\\' && r < 8 {
		r++
	}
	if pos+r >= len(s) || s[pos+r] != '"' {
		return 0
	}
	switch r {
	case 1:
		return 1
	case 3:
		return 2
	case 7:
		return 3
	}
	return 0
}

// escapedQuotedSpan returns the value of a double-quoted string escaped level times inside other JSON
// strings, such as the value in "{\"clientSecret\":\"a,b\"}" (level 1), whose escaped opening quote starts
// at pos. A quote preceded by a run of r backslashes closes the string of the level j given by the lowest
// zero bit of r (r mod 2^(j+1) == 2^j - 1): j == level closes the value, a lower j ends an enclosing
// string first (the value then runs up to it), and a higher j is an escaped quote inside the value. So a
// comma, or an inner escaped quote, does not end the value. A line break ends it too.
func escapedQuotedSpan(s string, pos, level int) (int, int) {
	k := 1<<level - 1
	start := pos + k + 1
	run := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '\\':
			run++
			continue
		case '"':
			j := 0
			for run%(2<<j) != 1<<j-1 {
				j++
			}
			if j == level {
				return start, i - k
			}
			if j < level {
				return start, i - run
			}
		case '\n', '\r':
			return start, i - run
		}
		run = 0
	}
	return start, len(s)
}

// Value boundaries for the key-value rules, one per format.
const (
	// boundaryWhitespace: env files, logfmt and free text. The value runs to the next whitespace, so
	// punctuation inside it ("&", ";", ",") is redacted with it.
	boundaryWhitespace = iota
	// boundaryFlow: JSON and YAML flow mappings. An unquoted value ends at ",", "}", "]" or the line end.
	boundaryFlow
	// boundaryLine: YAML block mappings. A plain value runs to the line end, plus continuation lines
	// indented deeper than the key.
	boundaryLine
)

// valueSpan returns the value that starts at pos, after a key and its separator, for the given format.
// A quoted value is the text between its quotes; an unterminated quote is redacted to the boundary. A value
// in escaped quotes (JSON inside a JSON string, escaped up to three times) ends at its own closing quote.
func valueSpan(s string, pos, boundary, keyCol int) (int, int) {
	if pos >= len(s) {
		return pos, pos
	}
	if level := escapeLevel(s, pos); level > 0 {
		return escapedQuotedSpan(s, pos, level)
	}
	if c := s[pos]; c == '"' || c == '\'' {
		if end := quotedEnd(s, pos); end > 0 {
			return pos + 1, end - 1
		}
	}
	end := pos
	switch boundary {
	case boundaryWhitespace:
		for end < len(s) && !isSpace(s[end]) {
			end++
		}
	case boundaryFlow:
		end = len(s)
		if i := strings.IndexAny(s[pos:], ",}]\r\n"); i >= 0 {
			end = pos + i
		}
		for end > pos && isSpace(s[end-1]) {
			end--
		}
	case boundaryLine:
		le := lineEnd(s, pos)
		end = le
		for end > pos && isSpace(s[end-1]) {
			end--
		}
		if end == pos {
			break
		}
		if _, cont := indentedLines(s, le, keyCol); cont > 0 {
			end = cont
		}
	}
	return pos, end
}

// isBlockHeader reports whether a YAML block scalar indicator ("|", ">-", "|2+") starts at pos and is
// followed only by blanks or a comment up to the line end. It reads only the indicator and the blanks after
// it, so it costs constant time per key on any input.
func isBlockHeader(s string, pos int) bool {
	end := pos
	for end < len(s) && end-pos < 3 && strings.IndexByte("|>123456789+-", s[end]) >= 0 {
		end++
	}
	if !blockIndicator.MatchString(s[pos:end]) {
		return false
	}
	blank := end
	for blank < len(s) && (s[blank] == ' ' || s[blank] == '\t') {
		blank++
	}
	return blank == len(s) || s[blank] == '\n' || s[blank] == '\r' || (s[blank] == '#' && blank > end)
}

// isSpace reports whether c is ASCII whitespace.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// keyValueSpan returns the span function of a key-value rule whose regex matches the key (group 1) and its
// separator; the value is found by valueSpan with the given boundary. Keys that are references are left
// alone without reading their value, so a reference never hides a neighbouring secret. A key inside a value
// already redacted is skipped, so each byte is scanned once. A bare YAML block scalar indicator is left to
// the block scalar rule, and a YAML comment over a block on the following lines to the next-line rule
// (commentOverBlock). A bare short key followed by a colon counts only with a value that looks like a
// secret (bareShortColon); a quoted value may hold blanks (looksLikeQuotedSecret).
func keyValueSpan(boundary int) func(s string, m []int, prevEnd int) (int, int, bool) {
	return func(s string, m []int, prevEnd int) (int, int, bool) {
		if m[2] < prevEnd || keepReference(s, m) {
			return 0, 0, false
		}
		if isBlockHeader(s, m[1]) || commentOverBlock(s, m) {
			return 0, 0, false
		}
		start, end := valueSpan(s, m[1], boundary, m[2]-m[0])
		if bareShortColon(s, m) {
			if v := s[start:end]; start > m[1] && !looksLikeQuotedSecret(v) || start == m[1] && !looksLikeSecret(v) {
				return 0, 0, false
			}
		}
		return start, end, true
	}
}

// commentOverBlock reports whether the value of the key in group 1 of m, at the end of m, is a YAML comment
// ("credentials: # set by CI") over a block on the following lines, which the next-line rule redacts so
// that reference children keep their names: a "#" after a colon and a blank, a key first on its line, and
// a line of its block after it. A "#" with no such block, or after "=", counts as the value. Only one key
// per line is first on it, so the line is read at most once.
func commentOverBlock(s string, m []int) bool {
	pos := m[1]
	if pos >= len(s) || s[pos] != '#' || pos == 0 || (s[pos-1] != ' ' && s[pos-1] != '\t') {
		return false
	}
	sep := pos - 1
	for sep > m[3] && (s[sep] == ' ' || s[sep] == '\t') {
		sep--
	}
	if s[sep] != ':' {
		return false
	}
	ls := m[2]
	for ls > 0 && (s[ls-1] == ' ' || s[ls-1] == '\t' || s[ls-1] == '-') {
		ls--
	}
	if ls > 0 && s[ls-1] != '\n' {
		return false
	}
	_, _, ok := nextBlockLine(s, lineEnd(s, pos), m[2]-ls, blockValue)
	return ok
}

// Key names that hold secrets, in three forms: bare, optionally quoted (YAML block mappings), and quoted
// or bare after "{" or "," (JSON and YAML flow mappings, also with escaped quotes inside a JSON string).
// The short names in shortKeyExpr count only as a whole key or the last part of one.
const (
	secretWordExpr = `(?:password|passwd|passphrase|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|client[_-]?secret|credentials?|connection[_-]?string|conn[_-]?str)`
	bareKeyExpr    = `(?:[A-Za-z0-9_.-]*` + secretWordExpr + `[A-Za-z0-9_.-]*|` + shortKeyExpr + `)`
	// shortKeyExpr names short keys that hold secrets but are also parts of other words ("bypass",
	// "author", "signal"), so they count only as a whole key or as the last part of a longer one after "_",
	// "." or "-" ("DB_PASS", "basic_auth").
	shortKeyExpr  = `(?:[A-Za-z0-9_.-]*[_.-]|\b)(?:pass|auth|sig)`
	secretKeyExpr = `["']?` + bareKeyExpr + `["']?`
	flowKeyExpr   = `\\*["']` + bareKeyExpr + `\\*["']|` + bareKeyExpr
	// nextLineHeaderExpr is a YAML key that names a secret with nothing after its colon but blanks or a
	// comment, after the start of a line.
	nextLineHeaderExpr = `[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:(?:[ \t]+#[^\r\n]*)?[ \t]*\r?$`
	// jsonSpaceExpr is JSON whitespace, also escaped inside a JSON string ("\n", "\t").
	jsonSpaceExpr = `(?:[ \t\r\n]|\\+[nrt])*`
)

// Quoted value forms for the Authorization rule: double-quoted with backslash escapes, and single-quoted
// with doubled quotes (YAML).
const (
	doubleQuotedExpr = `"((?:[^"\\\r\n]|\\.)*)"`
	singleQuotedExpr = `'((?:[^'\r\n]|'')*)'`
)

func builtinRules() []rule {
	return []rule{
		// PEM private keys, also when truncated before the END line.
		{kind: KindPrivateKey, re: regexp.MustCompile(
			`(?s)-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY( BLOCK)?-----.*?(?:-----END [A-Z0-9 ]{0,40}PRIVATE KEY( BLOCK)?-----|\z)`)},
		// JSON Web Tokens: a base64url header starting with '{"', any payload, optional signature.
		{kind: KindJWT, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)},
		// Well-known token formats with a recognisable prefix.
		{kind: KindToken, re: regexp.MustCompile(`\b(?:` + strings.Join([]string{
			`gh[pousr]_[A-Za-z0-9]{36,255}`,                 // GitHub
			`github_pat_[A-Za-z0-9_]{22,255}`,               // GitHub fine-grained
			`glpat-[A-Za-z0-9_-]{20,}`,                      // GitLab
			`(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}`,           // AWS access key ID
			`xox[abposr]-[A-Za-z0-9-]{10,}`,                 // Slack
			`xapp-[A-Za-z0-9-]{10,}`,                        // Slack app
			`AIza[0-9A-Za-z_-]{35}`,                         // Google API key
			`ya29\.[0-9A-Za-z_-]{20,}`,                      // Google OAuth access token
			`[rs]k_(?:live|test)_[0-9A-Za-z]{16,}`,          // Stripe
			`sk-(?:proj-|ant-|svcacct-)?[A-Za-z0-9_-]{20,}`, // model provider API keys
			`npm_[A-Za-z0-9]{36}`,                           // npm
			`pypi-[A-Za-z0-9_-]{50,}`,                       // PyPI
			`dop_v1_[a-f0-9]{64}`,                           // DigitalOcean
			`hv[sb]\.[A-Za-z0-9_-]{20,}`,                    // HashiCorp Vault
		}, "|") + `)`)},
		// URLs with any userinfo part, such as postgres://user:pass@host/db, https://token:@host/repo or
		// https://token@host/repo: a user name alone may be a token.
		{kind: KindConnectionString, re: regexp.MustCompile(
			`(?i)\b[a-z][a-z0-9+.-]{1,30}://[^\s/?#@'"<>]+@[^\s'"<>]+`)},
		// Secret fields of semicolon-separated connection strings (Azure Storage, Service Bus, SQL Server):
		// quoted with doubled quotes, or bare up to the next ";" or line end, spaces included.
		{kind: KindConnectionString, groups: []int{1, 2, 3}, re: regexp.MustCompile(
			`(?i)\b(?:AccountKey|SharedAccessKey|SharedAccessSignature|SharedSecretValue|Password|Pwd)[ \t]*=[ \t]*` +
				`(?:"((?:[^"\r\n]|"")*)"|'((?:[^'\r\n]|'')*)'|([^;\r\n]+))`)},
		// Authorization headers: the whole value, scheme and every parameter (SigV4, Digest), quoted or up
		// to the line end.
		{kind: KindCredential, groups: []int{1, 2, 3}, re: regexp.MustCompile(
			`(?i)\b(?:proxy-)?authorization["']?[ \t]*[:=][ \t]*(?:` + doubleQuotedExpr + `|` + singleQuotedExpr + `|([^\r\n]+))`)},
		// Bearer and Basic credentials outside an Authorization header, when the value has the shape of one.
		{kind: KindCredential, span: schemeSpan, re: regexp.MustCompile(
			`(?i)\b(bearer|basic)[ \t]+([A-Za-z0-9._~+/-]+=*)`)},
		// Credentials in URL query strings, such as Azure SAS signatures and presigned URLs.
		{kind: KindCredential, groups: []int{1}, re: regexp.MustCompile(
			`(?i)[?&](?:sig|signature|token|access_token|refresh_token|id_token|api_key|apikey|key|code|password|secret|client_secret|x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature)=([^&\s#"'<>]+)`)},
		// Key-value pairs whose key names a secret, one rule per format so each value ends at its own
		// format's boundary. Env files, logfmt and free text: the value runs to the next whitespace.
		{kind: KindCredential, span: keyValueSpan(boundaryWhitespace), re: regexp.MustCompile(
			`(?i)(` + bareKeyExpr + `)[ \t]*[:=][ \t]*`)},
		// JSON and YAML flow mappings: a field after "{" or ",", ending at its closing quote or at , } ].
		// JSON whitespace, newlines included and also escaped, may surround the key and the colon.
		{kind: KindCredential, span: keyValueSpan(boundaryFlow), re: regexp.MustCompile(
			`(?i)[{,]` + jsonSpaceExpr + `(` + flowKeyExpr + `)` + jsonSpaceExpr + `:` + jsonSpaceExpr)},
		// Kubernetes environment variables in JSON and YAML flow form: the value after a name that holds a
		// secret, {"name":"DB_PASSWORD","value":"..."}.
		{kind: KindCredential, span: keyValueSpan(boundaryFlow), re: regexp.MustCompile(
			`(?i)\\*["']?\bname\\*["']?` + jsonSpaceExpr + `:` + jsonSpaceExpr + `\\*["']?(` + bareKeyExpr + `)\\*["']?` +
				jsonSpaceExpr + `,` + jsonSpaceExpr + `\\*["']?value\\*["']?` + jsonSpaceExpr + `:` + jsonSpaceExpr)},
		// YAML block mappings: a key at the start of a line, its value to the line end plus continuation
		// lines indented deeper than the key.
		{kind: KindCredential, span: keyValueSpan(boundaryLine), re: regexp.MustCompile(
			`(?im)^[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:[ \t]+`)},
		// YAML literal and folded block scalars under a secret key, with chomping and indentation indicators.
		{kind: KindCredential, span: blockScalarSpan, re: regexp.MustCompile(
			`(?im)^[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:[ \t]*([|>][1-9+-]{0,2})[ \t]*(?:#[^\r\n]*)?\r?$`)},
		// YAML block mappings: a key with nothing after its colon but blanks or a comment, its value on the
		// following lines indented deeper than the key.
		{kind: KindCredential, spans: nextLineSpans, re: regexp.MustCompile(`(?im)^` + nextLineHeaderExpr)},
		// Kubernetes environment variables in YAML block form: "- name: DB_PASSWORD" and its "value".
		{kind: KindCredential, span: envValueSpan, re: regexp.MustCompile(
			`(?im)^[ \t]*-[ \t]+name[ \t]*:[ \t]*["']?(` + bareKeyExpr + `)["']?[ \t]*\r?$`)},
		// Long random tokens without a known prefix: a run of base64, base64url or hex characters (see
		// longTokenSpan).
		{kind: KindToken, span: longTokenSpan, re: regexp.MustCompile(`[A-Za-z0-9+/_-]{24,}=*`)},
		// Email addresses.
		{kind: KindEmail, re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`)},
		// IPv4 addresses.
		{kind: KindIP, re: regexp.MustCompile(
			`\b(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\b`)},
		// IPv6 addresses: candidates are confirmed with net/netip, so clock times and similar are kept.
		{kind: KindIP, span: ipv6Span, re: regexp.MustCompile(
			`(?:[0-9A-Fa-f]{0,4}:){2,8}(?:[0-9A-Fa-f]{1,4}|(?:[0-9]{1,3}\.){3}[0-9]{1,3})?`)},
	}
}
