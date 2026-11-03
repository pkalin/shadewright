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
// line is removed even when it is harmless, and so are the user name of any URL ("ssh://git@host"), the word
// after "Basic" or "Bearer", and long runs that mix letters and digits, such as Git commit hashes and
// container IDs. Values that look like references rather than secrets, such as "secretName: db-creds", are
// kept, because the name of a missing Secret is what a diagnosis needs, and so is the hex of an image digest.
package redact

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
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
		for _, m := range ru.re.FindAllStringSubmatchIndex(s, -1) {
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
	key := strings.ToLower(strings.Trim(s[m[2]:m[3]], `"'\`))
	key = strings.NewReplacer("_", "", "-", "", ".", "").Replace(key)
	for _, suffix := range referenceSuffixes {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
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

// indentedLines scans the lines after pos, which is at a line end, while they are blank or indented
// deeper than keyCol. It returns the start of the first non-blank one and the end of the last, or -1, -1.
func indentedLines(s string, pos, keyCol int) (int, int) {
	start, end := -1, -1
	for pos < len(s) && (s[pos] == '\n' || s[pos] == '\r') {
		if s[pos] == '\r' {
			pos++
			continue
		}
		line := pos + 1
		le := lineEnd(s, line)
		text := s[line:le]
		trimmed := strings.TrimLeft(text, " \t")
		if trimmed != "" {
			if len(text)-len(trimmed) <= keyCol {
				break
			}
			if start < 0 {
				start = line + len(text) - len(trimmed)
			}
			end = le
		}
		pos = le
	}
	return start, end
}

// blockScalarSpan returns the content of a YAML literal or folded block scalar whose header line the rule
// matched: the following lines that are blank or indented deeper than the key, up to the last non-blank
// one. With no content, the indicator itself (group 2) is redacted. Headers inside a block already
// redacted are skipped, so each byte is scanned once.
func blockScalarSpan(s string, m []int, prevEnd int) (int, int, bool) {
	if m[0] < prevEnd || keepReference(s, m) {
		return 0, 0, false
	}
	start, end := indentedLines(s, m[1], m[2]-m[0])
	if start < 0 {
		return m[4], m[5], true
	}
	return start, end, true
}

// nestedStart matches the start of a line that opens a nested mapping: a key followed by a colon and a blank
// or the line end, also as a list item, or a flow mapping.
var nestedStart = regexp.MustCompile(`^(?:-[ \t]+)*(?:\{|(?:"[^"]*"|'[^']*'|[^\s"'#:][^#\r\n]*?)[ \t]*:(?:[ \t]|\r?$))`)

// nextLineSpan returns the value of a YAML key with nothing after its colon, written on the following lines
// indented deeper than the key: a plain or quoted scalar, or a list of them. A key with no such lines is
// truly empty and kept, and so is a nested mapping, whose own keys the other rules judge. Only the first
// content line is read before deciding, so a chain of nested mappings costs one line per key. Headers inside
// a value already redacted are skipped, so each byte is scanned once.
func nextLineSpan(s string, m []int, prevEnd int) (int, int, bool) {
	if m[0] < prevEnd || keepReference(s, m) {
		return 0, 0, false
	}
	keyCol := m[2] - m[0]
	pos := m[1]
	for pos < len(s) && (s[pos] == '\n' || s[pos] == '\r') {
		if s[pos] == '\r' {
			pos++
			continue
		}
		line := pos + 1
		le := lineEnd(s, line)
		text := s[line:le]
		trimmed := strings.TrimLeft(text, " \t")
		if trimmed == "" {
			pos = le
			continue
		}
		if len(text)-len(trimmed) <= keyCol || nestedStart.MatchString(trimmed) {
			return 0, 0, false
		}
		start, end := indentedLines(s, m[1], keyCol)
		if c := s[start]; c == '"' || c == '\'' {
			// A quoted scalar that is the whole value keeps its quotes.
			if qe := quotedEnd(s, start); qe > 0 && strings.TrimRight(s[qe:end], " \t") == "" {
				return start + 1, qe - 1, true
			}
		}
		return start, end, true
	}
	return 0, 0, false
}

// minTokenLength is the shortest part of a run of base64 or hex characters that is taken for a random token.
const minTokenLength = 24

// longTokenSpan redacts a run of base64 or hex characters when some part of it between slashes is at least
// minTokenLength long and mixes letters and digits. Paths and image names (short parts between slashes),
// long names made of letters only and the hex of an image digest ("sha256:...", which identifies a build,
// not a credential) are kept.
func longTokenSpan(s string, m []int, _ int) (int, int, bool) {
	run := s[m[0]:m[1]]
	if prefix := strings.ToLower(s[max(0, m[0]-7):m[0]]); (prefix == "sha256:" || prefix == "sha512:") && isHex(run) {
		return 0, 0, false
	}
	for part := range strings.SplitSeq(run, "/") {
		if len(part) >= minTokenLength && mixesLettersAndDigits(part) {
			return m[0], m[1], true
		}
	}
	return 0, 0, false
}

// isHex reports whether s holds only hexadecimal digits.
func isHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
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

// escapedQuotedSpan returns the value of a double-quoted string escaped once inside another JSON string,
// such as the value in "{\"clientSecret\":\"a,b\"}", whose escaped opening quote starts at pos. The text is
// read at the outer level first (every backslash escapes the next byte) and then at the inner one, so the
// value ends at its own escaped closing quote: a comma, or an inner escaped quote (\\\"), does not end it.
// If the outer string or the line ends first, the value runs up to there.
func escapedQuotedSpan(s string, pos int) (int, int) {
	start := pos + 2
	innerEscape := false
	for i := start; i < len(s); {
		c := s[i]
		if c == '"' || c == '\n' || c == '\r' {
			return start, i
		}
		if c != '\\' {
			innerEscape = false
			i++
			continue
		}
		if i+1 >= len(s) {
			return start, len(s)
		}
		switch decoded := s[i+1]; {
		case innerEscape:
			innerEscape = false
		case decoded == '\\':
			innerEscape = true
		case decoded == '"':
			return start, i
		}
		i += 2
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
// in escaped quotes (JSON inside a JSON string) ends at its escaped closing quote.
func valueSpan(s string, pos, boundary, keyCol int) (int, int) {
	if pos >= len(s) {
		return pos, pos
	}
	if s[pos] == '\\' && pos+1 < len(s) && s[pos+1] == '"' {
		return escapedQuotedSpan(s, pos)
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
// the block scalar rule.
func keyValueSpan(boundary int) func(s string, m []int, prevEnd int) (int, int, bool) {
	return func(s string, m []int, prevEnd int) (int, int, bool) {
		if m[2] < prevEnd || keepReference(s, m) {
			return 0, 0, false
		}
		if isBlockHeader(s, m[1]) {
			return 0, 0, false
		}
		start, end := valueSpan(s, m[1], boundary, m[2]-m[0])
		return start, end, true
	}
}

// Key names that hold secrets, in three forms: bare, optionally quoted (YAML block mappings), and quoted
// or bare after "{" or "," (JSON and YAML flow mappings, also with escaped quotes inside a JSON string).
// The short names in shortKeyExpr count only as a whole key.
const (
	secretWordExpr = `(?:password|passwd|passphrase|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|client[_-]?secret|credentials?|connection[_-]?string|conn[_-]?str)`
	bareKeyExpr    = `(?:[A-Za-z0-9_.-]*` + secretWordExpr + `[A-Za-z0-9_.-]*|` + shortKeyExpr + `)`
	// shortKeyExpr names short keys that hold secrets but are also parts of other words ("bypass",
	// "author", "signal"), so they count only as a whole key.
	shortKeyExpr  = `\b(?:pass|auth|sig)`
	secretKeyExpr = `["']?` + bareKeyExpr + `["']?`
	flowKeyExpr   = `\\?["']` + bareKeyExpr + `\\?["']|` + bareKeyExpr
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
		// Bearer and Basic credentials outside an Authorization header.
		{kind: KindCredential, groups: []int{1}, re: regexp.MustCompile(
			`(?i)\b(?:bearer|basic)[ \t]+([A-Za-z0-9._~+/-]+=*)`)},
		// Credentials in URL query strings, such as Azure SAS signatures and presigned URLs.
		{kind: KindCredential, groups: []int{1}, re: regexp.MustCompile(
			`(?i)[?&](?:sig|signature|token|access_token|refresh_token|id_token|api_key|apikey|key|code|password|secret|client_secret|x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature)=([^&\s#"'<>]+)`)},
		// Key-value pairs whose key names a secret, one rule per format so each value ends at its own
		// format's boundary. Env files, logfmt and free text: the value runs to the next whitespace.
		{kind: KindCredential, span: keyValueSpan(boundaryWhitespace), re: regexp.MustCompile(
			`(?i)(` + bareKeyExpr + `)[ \t]*[:=][ \t]*`)},
		// JSON and YAML flow mappings: a field after "{" or ",", ending at its closing quote or at , } ].
		// JSON whitespace, newlines included, may surround the colon.
		{kind: KindCredential, span: keyValueSpan(boundaryFlow), re: regexp.MustCompile(
			`(?i)[{,][ \t\r\n]*(` + flowKeyExpr + `)[ \t\r\n]*:[ \t\r\n]*`)},
		// YAML block mappings: a key at the start of a line, its value to the line end plus continuation
		// lines indented deeper than the key.
		{kind: KindCredential, span: keyValueSpan(boundaryLine), re: regexp.MustCompile(
			`(?im)^[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:[ \t]+`)},
		// YAML literal and folded block scalars under a secret key, with chomping and indentation indicators.
		{kind: KindCredential, span: blockScalarSpan, re: regexp.MustCompile(
			`(?im)^[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:[ \t]*([|>][1-9+-]{0,2})[ \t]*(?:#[^\r\n]*)?\r?$`)},
		// YAML block mappings: a key with nothing after its colon but blanks or a comment, its value on the
		// following lines indented deeper than the key.
		{kind: KindCredential, span: nextLineSpan, re: regexp.MustCompile(
			`(?im)^[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:(?:[ \t]+#[^\r\n]*)?[ \t]*\r?$`)},
		// Long random tokens without a known prefix: a run of base64 or hex characters with a part between
		// slashes of at least 24 characters that mixes letters and digits.
		{kind: KindToken, span: longTokenSpan, re: regexp.MustCompile(`[A-Za-z0-9+/_]{24,}=*`)},
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
