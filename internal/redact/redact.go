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
// line is removed even when it is harmless. Values that look like references rather than secrets, such as
// "secretName: db-creds", are kept, because the name of a missing Secret is what a diagnosis needs.
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
// empty. keep, when set, receives the submatches and returns true to leave a match alone. span, when set,
// replaces both: it receives the submatches and the end of the previous span it returned for this rule, and
// returns the byte range to redact, or ok false to leave the match alone.
type rule struct {
	kind   string
	re     *regexp.Regexp
	groups []int
	keep   func(s string, m []int) bool
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
			if ru.keep != nil && ru.keep(s, m) {
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

// keepReference keeps key-value matches whose key, in group 1, is a reference to a secret.
func keepReference(s string, m []int) bool {
	key := strings.ToLower(strings.Trim(s[m[2]:m[3]], `"'`))
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

// keepKeyValue keeps key-value matches whose key is a reference, and YAML block scalar headers, whose
// content the block scalar rule redacts.
func keepKeyValue(s string, m []int) bool {
	if keepReference(s, m) {
		return true
	}
	return m[8] >= 0 && blockIndicator.MatchString(s[m[8]:m[9]])
}

// isAlnum reports whether c is an ASCII letter or digit.
func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// ipv6Span confirms an IPv6 candidate with net/netip, so clock times and similar are kept. A single colon
// at either end that belongs to the surrounding text ("addr ::1: refused") is trimmed first. Candidates
// glued to letters or digits are kept, as they are part of a longer token.
func ipv6Span(s string, m []int, _ int) (int, int, bool) {
	start, end := m[0], m[1]
	trimEnd := end-start > 1 && s[end-1] == ':' && s[end-2] != ':'
	trimStart := end-start > 1 && s[start] == ':' && s[start+1] != ':'
	for _, try := range [][2]bool{{false, false}, {trimEnd, false}, {false, trimStart}, {trimEnd, trimStart}} {
		a, b := start, end
		if try[1] {
			a++
		}
		if try[0] {
			b--
		}
		if b <= a || (a > 0 && isAlnum(s[a-1])) || (b < len(s) && isAlnum(s[b])) {
			continue
		}
		if addr, err := netip.ParseAddr(s[a:b]); err == nil && addr.Is6() {
			return a, b, true
		}
	}
	return 0, 0, false
}

// blockScalarSpan returns the content of a YAML literal or folded block scalar whose header line the rule
// matched: the following lines that are blank or indented deeper than the key, up to the last non-blank
// one. With no content, the indicator itself (group 2) is redacted. Headers inside a block already
// redacted are skipped, so each byte is scanned once.
func blockScalarSpan(s string, m []int, prevEnd int) (int, int, bool) {
	if m[0] < prevEnd || keepReference(s, m) {
		return 0, 0, false
	}
	keyCol := m[2] - m[0]
	start, end := -1, -1
	pos := m[1]
	for pos < len(s) && s[pos] == '\n' {
		line := pos + 1
		next := strings.IndexByte(s[line:], '\n')
		lineEnd := len(s)
		if next >= 0 {
			lineEnd = line + next
		}
		text := strings.TrimRight(s[line:lineEnd], "\r")
		trimmed := strings.TrimLeft(text, " \t")
		if trimmed != "" {
			if len(text)-len(trimmed) <= keyCol {
				break
			}
			if start < 0 {
				start = line + len(text) - len(trimmed)
			}
			end = line + len(text)
		}
		pos = lineEnd
	}
	if start < 0 {
		return m[4], m[5], true
	}
	return start, end, true
}

// Secret key names shared by the key-value and block scalar rules.
const secretKeyExpr = `["']?[A-Za-z0-9_.-]*(?:password|passwd|passphrase|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|client[_-]?secret|credentials?|connection[_-]?string|conn[_-]?str)[A-Za-z0-9_.-]*["']?`

// Quoted value forms: double-quoted with backslash escapes, and single-quoted with doubled quotes (YAML).
const (
	doubleQuotedExpr = `"((?:[^"\\\r\n]|\\.)*)"`
	singleQuotedExpr = `'((?:[^'\r\n]|'')*)'`
)

// Value forms for key-value rules: quoted, or bare up to the next whitespace, so punctuation inside a
// value (env files allow "&", ";" and ",") is redacted with it. One of groups 2, 3 and 4 matches.
const valueExpr = `(?:` + doubleQuotedExpr + `|` + singleQuotedExpr + `|(\S+))`

func builtinRules() []rule {
	return []rule{
		// PEM private keys, also when truncated before the END line.
		{kind: KindPrivateKey, re: regexp.MustCompile(
			`(?s)-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY( BLOCK)?-----.*?(?:-----END [A-Z0-9 ]{0,40}PRIVATE KEY( BLOCK)?-----|\z)`)},
		// JSON Web Tokens: base64url header and payload starting with '{"', optional signature.
		{kind: KindJWT, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)},
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
		// URLs with credentials in the userinfo part, such as postgres://user:pass@host/db.
		{kind: KindConnectionString, re: regexp.MustCompile(
			`(?i)\b[a-z][a-z0-9+.-]{1,30}://[^\s/:@'"<>]*:[^\s@'"<>/]+@[^\s'"<>]+`)},
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
			`(?i)\b(?:bearer|basic)[ \t]+([A-Za-z0-9._~+/-]{8,}=*)`)},
		// Credentials in URL query strings, such as Azure SAS signatures and presigned URLs.
		{kind: KindCredential, groups: []int{1}, re: regexp.MustCompile(
			`(?i)[?&](?:sig|signature|token|access_token|refresh_token|id_token|api_key|apikey|key|code|password|secret|client_secret|x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature)=([^&\s#"'<>]+)`)},
		// Key-value pairs whose key names a secret, in YAML, JSON, env and log formats.
		{kind: KindCredential, groups: []int{2, 3, 4}, keep: keepKeyValue, re: regexp.MustCompile(
			`(?i)(` + secretKeyExpr + `)[ \t]*[:=][ \t]*` + valueExpr)},
		// YAML literal and folded block scalars under a secret key, with chomping and indentation indicators.
		{kind: KindCredential, span: blockScalarSpan, re: regexp.MustCompile(
			`(?im)^[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:[ \t]*([|>][1-9+-]{0,2})[ \t]*(?:#[^\r\n]*)?\r?$`)},
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
