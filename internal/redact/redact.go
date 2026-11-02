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
// empty. keep, when set, receives the submatches and returns true to leave a match alone.
type rule struct {
	kind   string
	re     *regexp.Regexp
	groups []int
	keep   func(s string, m []int) bool
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
// into one, named after the match that starts first (the longest, then the first rule, on a tie).
func (r *Redactor) String(s string) string {
	var spans []span
	for i, ru := range r.rules {
		for _, m := range ru.re.FindAllStringSubmatchIndex(s, -1) {
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
		if a.end != b.end {
			return b.end - a.end
		}
		return a.rule - b.rule
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

// keepNonIPv6 keeps candidates that do not parse as an IPv6 address, such as clock times.
func keepNonIPv6(s string, m []int) bool {
	addr, err := netip.ParseAddr(s[m[0]:m[1]])
	return err != nil || !addr.Is6()
}

// Value forms for key-value rules: double-quoted, single-quoted or bare. One of groups 2, 3 and 4 matches.
const valueExpr = `(?:"([^"\r\n]*)"|'([^'\r\n]*)'|([^\s"',;&}<>\]\[]+))`

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
		// Secret fields of semicolon-separated connection strings (Azure Storage, Service Bus, SQL Server).
		{kind: KindConnectionString, groups: []int{1}, re: regexp.MustCompile(
			`(?i)\b(?:AccountKey|SharedAccessKey|SharedAccessSignature|SharedSecretValue|Password|Pwd)[ \t]*=[ \t]*([^;'"\s]+)`)},
		// Authorization headers: the scheme word is redacted with the credential.
		{kind: KindCredential, groups: []int{1}, re: regexp.MustCompile(
			`(?i)\b(?:proxy-)?authorization["']?[ \t]*[:=][ \t]*["']?((?:[A-Za-z]+[ \t]+)?[^\s"',]+)`)},
		// Bearer and Basic credentials outside an Authorization header.
		{kind: KindCredential, groups: []int{1}, re: regexp.MustCompile(
			`(?i)\b(?:bearer|basic)[ \t]+([A-Za-z0-9._~+/-]{8,}=*)`)},
		// Credentials in URL query strings, such as Azure SAS signatures and presigned URLs.
		{kind: KindCredential, groups: []int{1}, re: regexp.MustCompile(
			`(?i)[?&](?:sig|signature|token|access_token|refresh_token|id_token|api_key|apikey|key|code|password|secret|client_secret|x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature)=([^&\s#"'<>]+)`)},
		// Key-value pairs whose key names a secret, in YAML, JSON, env and log formats.
		{kind: KindCredential, groups: []int{2, 3, 4}, keep: keepReference, re: regexp.MustCompile(
			`(?i)(["']?[A-Za-z0-9_.-]*(?:password|passwd|passphrase|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|client[_-]?secret|credentials?|connection[_-]?string|conn[_-]?str)[A-Za-z0-9_.-]*["']?)[ \t]*[:=][ \t]*` + valueExpr)},
		// Email addresses.
		{kind: KindEmail, re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`)},
		// IPv4 addresses.
		{kind: KindIP, re: regexp.MustCompile(
			`\b(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\b`)},
		// IPv6 addresses: candidates are confirmed with net/netip, so clock times and similar are kept.
		{kind: KindIP, keep: keepNonIPv6, re: regexp.MustCompile(
			`(?:[0-9A-Fa-f]{0,4}:){2,7}(?:[0-9A-Fa-f]{1,4}|(?:[0-9]{1,3}\.){3}[0-9]{1,3})?`)},
	}
}
