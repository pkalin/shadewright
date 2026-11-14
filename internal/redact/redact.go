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
// runs that mix letters and digits, such as Git commit hashes and container IDs, 32 characters or more of
// mixed-case base64 that slashes cut short, or that has no slash and no digit (also a base64 blob that is not
// a secret, and at most one short part of up to five characters on each side of it in a path), and every
// scalar value under a secret-named YAML key ("credentials:\n  user: admin" loses "admin"), the value after a
// secret-named command-line flag ("--client-secret x", "-password x", "--sas x", also across line ends, in
// argument lists and after a flag that took the flag as its value), however it reads or starts with
// ("--use-token --verbose" loses "--verbose", "--secret db-creds" loses "db-creds"), and the value after a key
// name of two or more words, joined by "_", "-" or in camelCase, and a blank ("aws_secret_access_key x",
// "ClientSecret x"), only when the whole value is an opaque token: 16 or more characters, every one of them
// from "A-Z a-z 0-9 / + = _ -", and at least one digit or one of "/+=" ("client_secret is missing" keeps "is",
// and a password such as "hunter2" after a blank stays, a known leak). After a flag or such a name the value
// is the text between its quotes on one line, or else one token that ends at whitespace, the line end, or a
// quote character (a plain or backslash-escaped quote), and at nothing else, not at ",", "]" or "}"
// (keyNameValue). The separator between a flag and its value is whitespace including real line ends, a
// backslash directly followed by a real line end, a YAML list dash only at the start of a line after
// indentation, a comma only directly after the flag's closing quote, and that quote, plain or
// backslash-escaped; nothing else, so literal "\n" pairs, a bare comma and a bare "-" start the value. A
// match that starts inside a value already taken is skipped, except a secret-named flag, which is judged on
// its own. Values that look like references rather than secrets, such
// as "secretName: db-creds", are kept, because the name of a missing Secret is what a diagnosis needs, and so
// is the hex of an image digest.
//
// Where a rule would also match prose, it asks the value to look like a credential: the word after "Basic"
// or "Bearer", and the value of the short keys "pass", "auth", "sig" and "sas" before a colon, on their own
// ("pass: 12 fail: 0") or after one of a few prose words and a hyphen ("pre-auth: ok", "two-pass: true"), inline or
// in a block on the following lines, count only with a value of at least 8 bytes that mixes ASCII letters
// with digits or symbols (blanks too when the value is quoted, as in a passphrase; 16 bytes of anything after
// Basic or Bearer, or base64 of "user:password" after Basic). So "Bearer abc12" and "auth: hunter2" pass
// through, while any other hyphenated key ("db-pass", "proxy-auth") always counts. Any byte that is not an
// ASCII letter counts as a symbol, so "auth: déjàvuxx" and "auth: unauthorized," are redacted, and so is
// "basic internationalization": over-redaction kept by decision (#194), because these cannot be told apart
// from a password or token.
//
// Known gaps: JSON escaped more than three times, a Kubernetes environment entry whose "value" comes before
// its "name" or more than 16 lines after it, a base64 secret cut short by slashes with no secret key name
// before it whose parts read as words or acronyms (whole or in part, see base64Windows), a secret after a
// one-word key name and a blank without a dash ("set secret x", which only the long-token rule judges,
// because "kubectl get secret foo" has the same shape), after a single-letter flag ("-p x"), after a key or
// flag name written with Unicode escapes, or in a here-document or here-string, the part of a value after a
// key name past its first token or its quotes on one line (blanks inside an argument that holds the flag,
// YAML flow, block, multi-line, anchored and tagged scalars, shell words of glued pieces and command
// substitution, arrays and objects under a secret field, JSON comments), a value after a flag in a flow list
// of plain items ("[--password, x]") or in pretty-printed escaped JSON ("\",\n  \""), the part of an
// unquoted value after a quote inside it, a value after a compound name and a blank that is no opaque token,
// and a secret in a format no rule knows (TestKeyNameKnownLeaks).
package redact

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"iter"
	"math"
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

// StringKeeping is String, except that a match of a rule that lies wholly inside whole occurrences of strings
// in keep stays as it is. keep holds values the caller has verified are not secrets (an object name it read back
// from the API server, for example), never text from a model, a user or the cluster; s may be any text, a
// template the caller renders or a model's answer. An occurrence counts only when the bytes on both sides are not
// token characters (ASCII letters, digits and "+/_-=."), so a kept string inside a longer run is redacted with
// that run. The rules run over the whole of s, so a match that holds a kept string and anything else (a
// password's value that starts with a kept name, for example) is redacted whole, kept string included. Empty
// strings in keep are ignored.
func StringKeeping(s string, keep ...string) string {
	return defaultRedactor.StringKeeping(s, keep...)
}

// Clean is the package's Clean with the built-in rules.
func Clean(s string, keep ...string) bool {
	return defaultRedactor.Clean(s, keep...)
}

// StringKeeping is the package's StringKeeping with r's rules.
func (r *Redactor) StringKeeping(s string, keep ...string) string {
	spans := r.spans(s)
	if len(spans) == 0 {
		return s
	}
	kept := wholeOccurrences(s, keep)
	if len(kept) == 0 {
		return render(s, spans)
	}
	out := spans[:0]
	for _, sp := range spans {
		if !covered(sp, kept) {
			out = append(out, sp)
		}
	}
	return render(s, out)
}

// Clean reports whether s holds nothing the rules would redact, besides matches that lie wholly inside whole
// occurrences of strings in keep (as [StringKeeping] keeps them) or inside markers this redactor writes. It is
// the check of text that was redacted already: redaction is not idempotent, as a marker in the place of a value
// can be taken for a value again ("token=[REDACTED:token]"), so comparing s with its redaction would refuse
// such text.
func (r *Redactor) Clean(s string, keep ...string) bool {
	spans := r.spans(s)
	if len(spans) == 0 {
		return true
	}
	ok := mergeRanges(append(wholeOccurrences(s, keep), r.markers(s)...))
	for _, sp := range spans {
		if !covered(sp, ok) {
			return false
		}
	}
	return true
}

// markerPattern matches the shapes of the markers [marker] writes; markers keeps only the exact ones.
var markerPattern = regexp.MustCompile(`\[REDACTED(?::[a-z][a-z0-9-]{0,31})?\]|\[[a-z][a-z0-9-]{0,31}\]|\*+`)

// markers returns the byte ranges of the markers of r's rules in s, in order and not overlapping: a long or
// short form naming a kind of one of r's rules, "[REDACTED]", or a run of at most three "*" (the longest
// [marker] writes). A longer run of "*" or a marker naming another kind is not one this redactor wrote, so
// Clean does not take it as clean.
func (r *Redactor) markers(s string) []span {
	var out []span
	for _, m := range markerPattern.FindAllStringIndex(s, -1) {
		if r.isMarker(s[m[0]:m[1]]) {
			out = append(out, span{start: m[0], end: m[1]})
		}
	}
	return out
}

// isMarker reports whether text, a match of markerPattern, is a marker [marker] writes for one of r's rules.
func (r *Redactor) isMarker(text string) bool {
	switch {
	case text[0] == '*':
		return len(text) <= len("***")
	case text == "[REDACTED]":
		return true
	case strings.HasPrefix(text, "[REDACTED:"):
		return r.hasKind(text[len("[REDACTED:") : len(text)-1])
	default:
		return r.hasKind(text[1 : len(text)-1])
	}
}

// hasKind reports whether one of r's rules has the kind.
func (r *Redactor) hasKind(kind string) bool {
	for _, ru := range r.rules {
		if ru.kind == kind {
			return true
		}
	}
	return false
}

// mergeRanges sorts ranges by start and joins the ones that overlap or touch.
func mergeRanges(rs []span) []span {
	slices.SortFunc(rs, func(a, b span) int { return a.start - b.start })
	out := rs[:0]
	for _, x := range rs {
		if n := len(out); n > 0 && x.start <= out[n-1].end {
			out[n-1].end = max(out[n-1].end, x.end)
			continue
		}
		out = append(out, x)
	}
	return out
}

// wholeOccurrences returns the byte ranges of the whole occurrences in s of the non-empty strings of keep, in
// order and not overlapping (the longest one where two start at the same offset).
func wholeOccurrences(s string, keep []string) []span {
	var occ []span
	for pos := 0; pos < len(s); {
		at, n := firstWhole(s[pos:], keep)
		if at < 0 {
			break
		}
		occ = append(occ, span{start: pos + at, end: pos + at + n})
		pos += at + n
	}
	return occ
}

// covered reports whether every byte of sp lies inside the sorted, disjoint ranges of occ.
func covered(sp span, occ []span) bool {
	pos := sp.start
	for _, o := range occ {
		if o.end <= pos {
			continue
		}
		if o.start > pos {
			return false
		}
		pos = o.end
		if pos >= sp.end {
			return true
		}
	}
	return false
}

// firstWhole returns the offset and length of the first whole occurrence in s of a non-empty string of keep
// (the longest one on a tie), or -1.
func firstWhole(s string, keep []string) (int, int) {
	best, bestLen := -1, 0
	for _, k := range keep {
		if k == "" {
			continue
		}
		for from := 0; from <= len(s)-len(k); {
			i := strings.Index(s[from:], k)
			if i < 0 {
				break
			}
			i += from
			if (i == 0 || !isTokenByte(s[i-1])) && (i+len(k) == len(s) || !isTokenByte(s[i+len(k)])) {
				if best < 0 || i < best || (i == best && len(k) > bestLen) {
					best, bestLen = i, len(k)
				}
				break
			}
			from = i + 1
		}
	}
	return best, bestLen
}

// isTokenByte reports whether c can be part of a token run or an object name.
func isTokenByte(c byte) bool {
	return isAlnum(c) || strings.IndexByte("+/_-=.", c) >= 0
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
	return render(s, r.spans(s))
}

// spans returns the matches of every rule in s, sorted, with overlapping matches merged into one named after
// the match that starts first (the first rule, on a tie).
func (r *Redactor) spans(s string) []span {
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
		return nil
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

	merged := spans[:0]
	for i := 0; i < len(spans); {
		cur := spans[i]
		for i++; i < len(spans) && spans[i].start < cur.end; i++ {
			cur.end = max(cur.end, spans[i].end)
		}
		merged = append(merged, cur)
	}
	return merged
}

// render returns s with each of the sorted, disjoint spans replaced by its marker.
func render(s string, spans []span) string {
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	pos := 0
	for _, cur := range spans {
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

// flagReferenceSuffixes mark command-line flags, and only flags, that never take the secret itself:
// "--password-stdin" (the secret comes on standard input, the flag takes no value) and "--token-audience" (the
// audience a token is issued for). As a key of any other form ("PASSWORD_STDIN=", "client_secret_for_audience:")
// the name holds a secret like any other (#585).
var flagReferenceSuffixes = []string{"stdin", "audience"}

// keepReference reports whether the key in group 1 of m is a reference to a secret. It judges only that
// key, never a neighbouring one.
func keepReference(s string, m []int) bool {
	return isReference(s[m[2]:m[3]])
}

// keySeparators removes the separators of a key name before its suffix is compared.
var keySeparators = strings.NewReplacer("_", "", "-", "", ".", "")

// isReference reports whether key, quoted or not, names or points to a secret instead of holding one. A key that holds the word "secret"
// never takes the suffix "id": a Vault AppRole secret ID ("--secret-id", "secret_id", "secretId") and
// "client_secret_key_id" are credentials, not the ID of one, while "secret_name", "secretRef", "secret_file"
// and "secret_path" stay references (#585).
func isReference(key string) bool {
	return referenceKey(key, false)
}

// referenceKey is isReference that, with flag, also takes flagReferenceSuffixes: for a flag followed by
// blanks or a line end ("--password-stdin -u bob"), never for one followed by "=" ("--password-stdin=x" holds
// a value like "PASSWORD_STDIN=x").
func referenceKey(key string, flag bool) bool {
	key = strings.ToLower(strings.Trim(key, `"'\`))
	key = keySeparators.Replace(key)
	secret := strings.Contains(key, "secret")
	for _, suffix := range referenceSuffixes {
		if strings.HasSuffix(key, suffix) && (suffix != "id" || !secret) {
			return true
		}
	}
	if flag {
		for _, suffix := range flagReferenceSuffixes {
			if strings.HasSuffix(key, suffix) {
				return true
			}
		}
	}
	return false
}

// secretWordPattern finds a secret word (secretWordExpr) anywhere in a key.
var secretWordPattern = regexp.MustCompile(`(?i)` + secretWordExpr)

// shortKeyNames are the short key names that hold secrets but are also parts of other words (shortKeyExpr):
// the one list that the key patterns, isBareShortKey and isSasKey read.
var shortKeyNames = strings.Split(shortKeyNamesExpr, "|")

// sasKeyName is the short key name of an Azure shared access signature.
const sasKeyName = "sas"

// isSasKey reports whether key, quoted or not, counts as a secret only through its last part "sas" (an Azure
// shared access signature): it ends in "sas" and holds no other secret word.
func isSasKey(key string) bool {
	k := strings.Trim(key, `"'\`)
	return len(k) >= len(sasKeyName) && strings.EqualFold(k[len(k)-len(sasKeyName):], sasKeyName) &&
		!secretWordPattern.MatchString(k)
}

// sasRefused reports whether the key in group 1 of m is a "sas" key (isSasKey) in a place where it does not
// count. "sas" is also a path segment and an image name ("ghcr.io/acme/sas:v1.2.3"), so it never counts
// right after "/" ("/sas: x"); "AZURE_STORAGE_SAS", "storageSas", "azure.storage.sas", "--source-sas" and
// "?sas=" count. With sep it counts only before "=" or before ":" and a blank or a line end ("sas=...",
// "sas: ...", not the tag in "sas:v1"), unless the key is quoted (`"sas":`), whose closing quote ends it
// (#585).
func sasRefused(s string, m []int, sep bool) bool {
	if !isSasKey(s[m[2]:m[3]]) {
		return false
	}
	end := m[3]
	for end > m[2] && strings.IndexByte(`"'\`, s[end-1]) >= 0 {
		end--
	}
	if at := end - len(sasKeyName); at > 0 {
		if s[at-1] == '/' {
			return true
		}
	}
	if !sep || end < m[3] {
		return false
	}
	i := end
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	switch {
	case i < len(s) && s[i] == '=':
		return false
	case i < len(s) && s[i] == ':':
		return i+1 < len(s) && !isSpace(s[i+1])
	}
	return true
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

// isBareShortKey reports whether key is one of shortKeyNames ("pass", "auth", "sig", "sas") on its own, in
// any case, or in lower case after exactly one word of proseShortKeyPrefixes and a hyphen ("pre-auth", but not
// "Pre-Auth", "pre-x-auth" or "db-pass").
func isBareShortKey(key string) bool {
	if slices.Contains(shortKeyNames, strings.ToLower(key)) {
		return true
	}
	prefix, short, ok := strings.Cut(key, "-")
	return ok && slices.Contains(shortKeyNames, short) && slices.Contains(proseShortKeyPrefixes, prefix)
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
	if m[0] < prevEnd || keepReference(s, m) || sasRefused(s, m, true) {
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
	if m[0] < prevEnd || keepReference(s, m) || sasRefused(s, m, true) {
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
	if keepReference(s, hm) || sasRefused(s, hm, true) {
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
// quotes of a quoted scalar that is the whole value, so it keeps its quotes, otherwise all of it. A quoted
// scalar that a backslash continues past the block's end (an escaped line break, #523) is redacted up to its
// closing quote, continuation lines included.
func scalarValue(s string, start, end int) (int, int) {
	if start < end && (s[start] == '"' || s[start] == '\'') {
		qe := quotedEnd(s, start)
		if qe > end {
			return start + 1, qe - 1
		}
		if qe > 0 && strings.TrimRight(s[qe:end], " \t") == "" {
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
	if m[0] < prevEnd || keepReference(s, m) || sasRefused(s, m, false) {
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

// longTokenSpans redacts a run of base64, base64url or hex characters when some part of it between slashes
// looks random: a piece between hyphens of at least minTokenLength that mixes letters and digits, or a
// hyphenated part of at least that length that mixes upper case, lower case and digits (base64url). A run
// that no such part makes random may still hold standard base64 that its slashes cut short: then only the
// slash-separated parts that read as base64 are redacted, each stretch on its own (base64Windows). Paths
// and image names (short parts between slashes), long names made of words, Kubernetes names and UUIDs
// (lower case, short pieces between hyphens) and the hex of an image digest ("sha256:...", which identifies
// a build, not a credential, also with a suffix such as "-amd64") are kept; what follows the digest's hex is
// judged on its own. It returns the end of the run.
func longTokenSpans(s string, m []int, _ int, add func(start, end int)) int {
	start, run := m[0], s[m[0]:m[1]]
	if prefix := strings.ToLower(s[max(0, m[0]-7):m[0]]); prefix == "sha256:" || prefix == "sha512:" {
		if digest, rest, _ := strings.Cut(run, "-"); isHex(digest) {
			start, run = m[1]-len(rest), rest
		}
	}
	if randomRun(run) {
		add(start, m[1])
		return m[1]
	}
	base64Windows(run, func(a, b int) { add(start+a, start+b) })
	return m[1]
}

// randomRun reports whether some part of run between slashes looks random, as described for longTokenSpans.
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

// minBase64Window is the number of characters, slashes and padding aside, from which slash-separated parts
// are judged together as standard base64 (base64Windows): less than the 40 of an AWS secret access key or
// the 44 of an older Entra client secret, and enough for the score of a window to tell words from base64.
const minBase64Window = 32

// maxBase64Score is the highest score per pair of neighbouring characters (wordScore) at which a window
// counts as base64. Random base64 scores about -0.47 and a window of 32 random characters scores above 0.15
// a few times in 10,000; a 40-character key gives several windows, and 5 of 100,000 seeded slash-split keys
// scored above it in all of them (long runs of lower-case letters by chance). Path words score from about 0.2 (a Windows temp path ending in a random name) to 0.9
// (TestSlashSplitRuleKeepsPaths).
const maxBase64Score = 0.15

// Character classes for wordScore.
const (
	classVowel = iota // a lower-case vowel
	classConsonant
	classUpper
	classDigit
	classOther
	numClasses
)

// wordScore holds, for a character of one class followed by one of another, the log of how much likelier
// that pair is in words (paths, CamelCase names) than in random base64, where each of the 64 characters is
// as likely in any place. The rates for words are rough estimates; the threshold maxBase64Score was set by
// measuring random keys and real paths, not from these rates alone.
var wordScore = func() (t [numClasses][numClasses]float64) {
	inWords := [numClasses][numClasses]float64{
		classVowel:     {.12, .75, .07, .05, .01},
		classConsonant: {.45, .38, .08, .07, .02},
		classUpper:     {.35, .30, .30, .04, .01},
		classDigit:     {.07, .08, .15, .65, .05},
		classOther:     {.15, .25, .30, .20, .10},
	}
	inBase64 := [numClasses]float64{5.0 / 64, 21.0 / 64, 26.0 / 64, 10.0 / 64, 2.0 / 64}
	for a := range t {
		for b := range t[a] {
			t[a][b] = math.Log(inWords[a][b] / inBase64[b])
		}
	}
	return t
}()

// wordClass returns the class of c for wordScore.
func wordClass(c byte) int {
	switch {
	case c == 'a' || c == 'e' || c == 'i' || c == 'o' || c == 'u':
		return classVowel
	case c >= 'a' && c <= 'z':
		return classConsonant
	case c >= 'A' && c <= 'Z':
		return classUpper
	case c >= '0' && c <= '9':
		return classDigit
	}
	return classOther
}

// maxBase64Window is the most characters a window of base64Windows holds: more than the 44 of the longest
// slash-split secret it is meant for, and a bound that keeps the number of windows per part constant.
const maxBase64Window = 96

// windowPart is one slash-separated part of a run, with what base64Windows adds up over a window of parts.
type windowPart struct {
	start, end   int     // byte range in the run, padding included
	chars, pairs int     // characters, padding excluded, and pairs of neighbouring ones that count as evidence
	score        float64 // sum of wordScore over the pairs
	upper, lower bool    // holds an upper-case or a lower-case letter, as evidence
	base64       bool    // may be part of standard base64: no "-" or "_", and "=" only as final padding
	empty        bool    // no characters besides padding
	word         bool    // a path word (pathWord)
	lowerWord    bool    // a path word that starts with a lower-case letter
}

// base64Windows finds standard base64 cut short by slashes in a run that randomRun keeps (#525), such as an
// AWS secret access key after a path or in a CLI flag, and calls add with each range of the run to redact.
// A run without a slash is one part and goes through the same windows: a run of 32 or more characters that
// randomRun keeps (letters and "+" without a digit, say) is redacted when it scores as base64.
//
// It judges every window of adjacent parts that holds from minBase64Window to maxBase64Window characters of
// evidence, apart from the parts around it, so path words next to a key cannot dilute it: the window counts
// when it mixes cases, its parts average at least four characters, and its pairs of neighbouring characters
// score at most maxBase64Score on average (wordScore). A window never starts or ends at an empty part, and one
// that starts or ends at a lower-case path word must score at most -maxBase64Score ("/opt/" before library
// names scores as words, "cijt/" before a key as base64). Parts with "-" or "_" (Kubernetes names, UUIDs)
// never join a window, and parts of upper-case letters, maybe with digits (acronyms and library names such
// as "HTTPS" or "CUDA12"), join one without counting as evidence either way.
//
// Of the parts that some window covers, path words (pathWord) are kept and the others are flagged;
// takeInKeyParts then joins the pieces of a key and adds at most one short part on each side of it. Each
// stretch of adjacent redacted parts is one range, so two keys with a path between them give two ranges and
// the path stays.
// Windows are bounded by maxBase64Window, so the cost is linear in the run.
//
// Known gaps when no secret key name stands before the key, kept for safety and simplicity over perfect paths
// (TestSlashSplitBase64KnownLeaks; after a key name the value goes whatever it reads as, #585: after
// "client_secret=" up to the next whitespace, after "--client-secret" one token or a quoted value on one
// line): a key whose windows all score as words stays whole; a short lower-case end of a key that no window
// covers ("/wed7") stays, as a path word would; a key of upper-case parts with one short mixed part
// ("ABCDEF/GHIJKL/.../yz2+ab3+cd4+") has too little evidence and stays; and a lower-case path word at either
// end of a key ("cijt/...") is kept with the path around it.
func base64Windows(run string, add func(start, end int)) {
	var parts []windowPart
	for pos := 0; pos <= len(run); {
		end := pos + strings.IndexByte(run[pos:], '/')
		if end < pos {
			end = len(run)
		}
		parts = append(parts, newWindowPart(run, pos, end))
		pos = end + 1
	}
	covered := coveredParts(parts)
	if covered == nil {
		return
	}
	redact := make([]bool, len(parts))
	for i, p := range parts {
		redact[i] = covered[i] && !p.word
	}
	takeInKeyParts(parts, redact)
	addStretches(parts, redact, add)
}

// coveredParts reports, for each part, whether a window that qualifies as base64 covers it (see
// base64Windows), or returns nil when no window qualifies.
func coveredParts(parts []windowPart) []bool {
	// diff counts, as a difference array, the qualifying windows over each part.
	var diff []int
	for lo := range parts {
		if parts[lo].empty {
			continue
		}
		var w windowPart
		var upper, lower bool
		for j := lo; j < len(parts) && parts[j].base64; j++ {
			p, n := parts[j], j-lo+1
			w.chars, w.pairs, w.score = w.chars+p.chars, w.pairs+p.pairs, w.score+p.score
			upper, lower = upper || p.upper, lower || p.lower
			if w.chars > maxBase64Window || 4*n > maxBase64Window {
				break
			}
			limit := maxBase64Score
			if parts[lo].lowerWord || p.lowerWord {
				limit = -maxBase64Score // a window at a lower-case path word must read clearly as base64
			}
			if !p.empty && w.chars >= minBase64Window && upper && lower && w.chars >= 4*n && w.score <= limit*float64(w.pairs) {
				if diff == nil {
					diff = make([]int, len(parts)+1)
				}
				diff[lo]++
				diff[j+1]--
			}
		}
	}
	if diff == nil {
		return nil
	}
	covered := make([]bool, len(parts))
	for i, n := 0, 0; i < len(parts); i++ {
		n += diff[i]
		covered[i] = n > 0
	}
	return covered
}

// maxTakeInPart is the longest part next to a key that takeInKeyParts takes in with it.
const maxTakeInPart = 5

// maxBase64Key is the most characters, slashes and padding aside, of the slash-split secrets base64Windows
// is meant for: the 44 of an older Entra client secret. Two stretches and the one part between them that
// hold no more than that are one key, whatever the part reads as (takeInKeyParts).
const maxBase64Key = 44

// keyStretch is a stretch of parts that base64Windows redacts: from the first to the last non-empty part,
// with the characters of its parts.
type keyStretch struct{ first, last, chars int }

// takeInKeyParts decides, from the parts that windows flagged (redact on entry: covered and no path word),
// which parts to redact, and writes that back into redact. Flagged parts with only empty parts ("//")
// between them form one stretch. A part with "-" or "_" (a Kubernetes name) is never added to a stretch, a
// lower-case path word ("etc", "api", "keys") or a long part only to join two pieces of one key:
//
//   - One part alone between two stretches joins them when the two stretches and the part hold at most
//     maxBase64Key characters, whatever the part reads as ("ghquje" or "Vpzfixaifdo" inside a key). Beyond
//     that, it joins them only when it is short (shortPart: at most maxTakeInPart characters) and no
//     lower-case path word, or when either stretch is shorter than minBase64Stretch (a piece of the same key
//     that would be kept on its own). A lower-case path word between two stretches of at least that length
//     ("/api/" between two keys) stays, and the keys are redacted each on its own.
//   - Then each stretch takes in at most one more part on each side: the next non-empty part, when it is
//     short, no lower-case path word and not between two stretches, such as "wed7" at the end of ".../wed7"
//     or a short capitalised word. A part taken in takes in nothing further, so the path after a key loses
//     one short part at most.
//
// Each step looks at a part and its neighbours once, so the cost is linear in the number of parts.
func takeInKeyParts(parts []windowPart, redact []bool) {
	var stretches []keyStretch
	for i := range parts {
		if !redact[i] || parts[i].empty {
			continue
		}
		if n := len(stretches); n > 0 && nextNonEmpty(parts, stretches[n-1].last, 1) == i {
			stretches[n-1].last, stretches[n-1].chars = i, stretches[n-1].chars+parts[i].chars
			continue
		}
		stretches = append(stretches, keyStretch{first: i, last: i, chars: parts[i].chars})
	}
	clear(redact)
	if len(stretches) == 0 {
		return
	}
	// Join stretches across one short part.
	merged := []keyStretch{stretches[0]}
	for _, s := range stretches[1:] {
		prev := &merged[len(merged)-1]
		c := nextNonEmpty(parts, prev.last, 1)
		joined := prev.chars + parts[c].chars + s.chars
		if c != nextNonEmpty(parts, s.first, -1) || !parts[c].base64 || joined > maxBase64Key &&
			(!shortPart(parts[c]) || parts[c].lowerWord && prev.chars >= minBase64Stretch && s.chars >= minBase64Stretch) {
			merged = append(merged, s)
			continue
		}
		prev.last, prev.chars = s.last, joined
	}
	for k, s := range merged {
		first, last := s.first, s.last
		// takeIn returns the part next to the stretch edge at i in direction step when it may be taken in.
		takeIn := func(i, step int) int {
			c := nextNonEmpty(parts, i, step)
			if c < 0 || parts[c].lowerWord || !shortPart(parts[c]) {
				return i
			}
			beyond := nextNonEmpty(parts, c, step)
			if beyond >= 0 && (step < 0 && k > 0 && beyond == merged[k-1].last ||
				step > 0 && k+1 < len(merged) && beyond == merged[k+1].first) {
				return i // between two stretches
			}
			return c
		}
		first, last = takeIn(first, -1), takeIn(last, 1)
		for i := first; i <= last; i++ {
			redact[i] = true
		}
	}
}

// nextNonEmpty returns the index of the first non-empty part after i in direction step (1 or -1), or -1.
func nextNonEmpty(parts []windowPart, i, step int) int {
	for i += step; i >= 0 && i < len(parts); i += step {
		if !parts[i].empty {
			return i
		}
	}
	return -1
}

// shortPart reports whether p is short enough to be redacted with a key next to it (takeInKeyParts): at most
// maxTakeInPart characters of standard base64, so no "-" or "_" (and no ".", which ends a run).
func shortPart(p windowPart) bool {
	return p.base64 && !p.empty && p.end-p.start <= maxTakeInPart
}

// addStretches calls add with the range of each stretch of adjacent redacted parts, without empty parts at
// its ends, that holds at least minBase64Stretch characters.
func addStretches(parts []windowPart, redact []bool, add func(start, end int)) {
	for i := 0; i < len(parts); {
		if !redact[i] || parts[i].empty {
			i++
			continue
		}
		j := i
		for j+1 < len(parts) && redact[j+1] {
			j++
		}
		next := j + 1
		for parts[j].empty {
			j--
		}
		chars := 0
		for k := i; k <= j; k++ {
			chars += parts[k].chars
		}
		// A short stretch among path words that a long window reached over is a word, not a key.
		if chars >= minBase64Stretch {
			add(parts[i].start, parts[j].end)
		}
		i = next
	}
}

// minBase64Stretch is the fewest characters a stretch of adjacent parts that base64Windows redacts holds: a
// key gives one long stretch, while a part that is no path word among words a window covers ("orders" in
// "/namespaces/orders/pods/") gives a short one, which is kept.
const minBase64Stretch = minBase64Window / 2

// minPathWordScore is the score per pair (wordScore) from which a part of four or more lower-case letters
// reads as a path word ("keys" 0.91, "download" 0.84, "credentials" 0.86) in pathWord, and
// minShortPathWordScore the same for three letters ("etc", "app" 0.49, "api" 1.29), which have one
// alternation of vowel and consonant at most. Random letters reach them less often, the longer they are.
const (
	minPathWordScore      = 0.8
	minShortPathWordScore = 0.4
)

// pathWord reports whether part, next to or at the end of a base64 window, is a path word rather than a
// piece of the key: at least three lower-case letters that read as a word (minPathWordScore), maybe after
// one capital and maybe followed by digits ("etc", "credentials", "python3", "Documents"), or a version
// ("v1"). A path word that a window covers is not flagged as key; a window that starts or ends at a
// lower-case one must score clearly as base64, and a lower-case one is redacted with a key only when it joins
// two pieces of one (takeInKeyParts).
func pathWord(part string) bool {
	capital := part != "" && part[0] >= 'A' && part[0] <= 'Z'
	if capital {
		part = part[1:]
	}
	letters := 0
	for letters < len(part) && part[letters] >= 'a' && part[letters] <= 'z' {
		letters++
	}
	for i := letters; i < len(part); i++ {
		if part[i] < '0' || part[i] > '9' {
			return false
		}
	}
	if !capital && part[:letters] == "v" && letters < len(part) {
		return true
	}
	if letters < 3 {
		return false
	}
	var score float64
	for i := 1; i < letters; i++ {
		score += wordScore[wordClass(part[i-1])][wordClass(part[i])]
	}
	if letters == 3 {
		return score >= minShortPathWordScore*float64(letters-1)
	}
	return score >= minPathWordScore*float64(letters-1)
}

// newWindowPart returns the part run[start:end] for base64Windows. A part of upper-case letters, maybe with
// digits, is an acronym or a library name ("REST", "CUDA12", "NCCL2") as often as a piece of a key, so it
// counts as no evidence: its characters fill a window, but it adds no pairs, no score and no case.
func newWindowPart(run string, start, end int) windowPart {
	p := windowPart{start: start, end: end, base64: true}
	text := run[start:end]
	if end == len(run) {
		text = strings.TrimRight(text, "=")
	}
	p.empty = text == ""
	acronym := !p.empty
	for i := range len(text) {
		c := text[i]
		switch {
		case c == '-' || c == '_' || c == '=':
			p.base64 = false
		case c >= 'A' && c <= 'Z':
			p.upper = true
		case c >= 'a' && c <= 'z':
			p.lower = true
		}
		acronym = acronym && (c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
		if i > 0 {
			p.score += wordScore[wordClass(text[i-1])][wordClass(c)]
		}
	}
	p.chars = len(text)
	p.word = !p.empty && pathWord(run[start:end])
	p.lowerWord = p.word && run[start] >= 'a' && run[start] <= 'z'
	if acronym && p.upper {
		p.upper, p.score = false, 0
		return p
	}
	p.pairs = max(0, len(text)-1)
	return p
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
// backslash escapes in double quotes and doubled quotes in single quotes, or -1 if the line ends first. A
// backslash before a line break ("\n" or "\r\n") escapes it, as in YAML, so the value goes on past that line.
func quotedEnd(s string, pos int) int {
	q := s[pos]
	for i := pos + 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\n' || c == '\r':
			return -1
		case q == '"' && c == '\\':
			i++
			if i+1 < len(s) && s[i] == '\r' && s[i+1] == '\n' {
				i++
			}
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
	// boundaryFlowWord: an unquoted value under a bare short key (bareShortColon) in a flow mapping. It is
	// the value boundaryFlow gives when that holds no blank; otherwise it is cut just past the first text
	// after a blank, which still holds the blank, so the value is refused all the same (looksLikeSecret). A
	// refused value does not move the rule past it, so reading on to the next flow delimiter would read the
	// same tail again for every key on the line (#526); this reads only the first word and the blanks after it.
	boundaryFlowWord
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
	case boundaryFlowWord:
		for end < len(s) && !isSpace(s[end]) && strings.IndexByte(",}]", s[end]) < 0 {
			end++
		}
		next := end
		for next < len(s) && isSpace(s[next]) && s[next] != '\r' && s[next] != '\n' {
			next++
		}
		if next < len(s) && strings.IndexByte(",}]\r\n", s[next]) < 0 {
			end = next + 1
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
// secret (bareShortColon); a quoted value may hold blanks (looksLikeQuotedSecret), and in flow form an
// unquoted one is read only up to the text after its first blank (boundaryFlowWord). A refused value
// leaves prevEnd where it was.
func keyValueSpan(boundary int, envName bool) func(s string, m []int, prevEnd int) (int, int, bool) {
	return func(s string, m []int, prevEnd int) (int, int, bool) {
		if m[2] < prevEnd || keepReference(s, m) || sasRefused(s, m, !envName) {
			return 0, 0, false
		}
		if isBlockHeader(s, m[1]) || commentOverBlock(s, m) {
			return 0, 0, false
		}
		bare := bareShortColon(s, m)
		b := boundary
		if bare && b == boundaryFlow {
			b = boundaryFlowWord
		}
		start, end := valueSpan(s, m[1], b, m[2]-m[0])
		if bare {
			if v := s[start:end]; start > m[1] && !looksLikeQuotedSecret(v) || start == m[1] && !looksLikeSecret(v) {
				return 0, 0, false
			}
		}
		return start, end, true
	}
}

// flagValueSpan is the span function of the command-line flag rule: a secret-named flag ("--client-secret",
// "--aws-secret-access-key", "--password", "--secret-id", "--sas", or with one dash and two or more letters,
// as Go programs take them: "-password", "-token"), the separator after it (flagSeparatorExpr) and then its
// value (keyNameValue). The key name is the signal: the value is redacted whatever it reads as or starts
// with, so a key that the long-token rule would keep in part (TestSlashSplitBase64KnownLeaks) goes whole, and
// so does "--Hunter2pw" or the next flag ("--use-token --verbose" loses "--verbose", accepted
// over-redaction). Each flag is judged on its own: one taken as the value of an earlier flag still has its
// own value redacted ("--enable-auth --client-secret x"). The flag counts wherever its dash does not follow a
// letter or a digit ("EXTRA_ARGS=--client-secret x" counts, "a--token x" and "my-token x" do not; after
// another "-" the match starts at that dash). A reference flag ("--secret-name", "--token-ttl",
// "--password-file", "--password-stdin") keeps its value. A value that starts inside the previous value is
// skipped, as it is redacted already, so each value is read once and the cost stays linear.
func flagValueSpan(s string, m []int, prevEnd int) (int, int, bool) {
	switch {
	case m[0] > 0 && isAlnum(s[m[0]-1]), m[1] < prevEnd, referenceKey(s[m[2]:m[3]], true):
		return 0, 0, false
	case m[2] == m[0]+1 && !isLetter(s[m[2]]):
		// One dash: the flag needs a letter after it ("-password"), as "-_x" or "-1" is no flag.
		return 0, 0, false
	}
	sep := s[m[3]:m[1]]
	q := strings.TrimLeft(sep, `\`)
	closed := q != "" && (q[0] == '"' || q[0] == '\'')
	start, end := keyNameValue(s, m[1], m[0], closed)
	return start, end, true
}

// compoundValueSpans is the spans function of the rule for a key name of two or more words, a blank and the
// value after it (keyNameValue), as in "aws configure set aws_secret_access_key x" (#585). The words are
// joined by "_" ("client_secret", "STORAGE_SAS", "SECRET_KEY_BASE"), by "-" ("client-secret") or in camelCase
// ("clientSecret", "ClientSecret", "SecretId"). The name rules are those of the other forms: a secret word
// anywhere in the name and no reference suffix (referenceKey; "client_id" and "access_key_id" keep their
// value, "secret_id" and "password_stdin" do not, as "stdin" is a reference only for a flag). The key must
// start a word, and a one-word name ("secret", "token", "password") never counts in this form, so "kubectl
// get secret foo" and "kubectl create token sa" stay readable. A key name before a blank is as often a word
// of a sentence, a type or a column of a table ("db_password not set", "ClientSecretCredential
// AuthenticationFailed", "db-password   kubernetes.io/basic-auth"), so this form exists only for an opaque
// key after the name: the value is redacted only when the whole of it is an opaque token (opaqueToken). A
// shorter or otherwise shaped value ("hunter2", "Password!") stays (TestKeyNameKnownLeaks). A name joined by
// "-" or in camelCase is also how Kubernetes names a Secret ("kubectl describe secret db-password -n prod")
// or a volume ("volumes=[secret-volume kube-api-access-x]"), so after such a name a value that reads as a flag
// ("--from-literal=x=y", flagWord) is kept, and so is the value after a name ending in "volume"
// (blankReferenceSuffixes). It also takes a flag ("-client_secret x", "--client_secret x", which the flag
// rule takes too), and leaves "=" or ":" after the blanks to the key-value rules. A bare short key with a
// prose prefix ("re-auth") does not count.
//
// The value of a counted name is taken whether or not it is redacted, and so is the value after a
// secret-named flag ("--password x", which the flag rule redacts): a match that starts inside a value already
// taken is skipped ("client_secret db_password --region west" does not read "db_password" as a name), except
// a secret-named flag, which is judged on its own. Each value is read once, so the cost stays linear.
func compoundValueSpans(s string, m []int, prevEnd int, add func(start, end int)) int {
	name := s[m[2]:m[3]]
	key := strings.TrimLeft(name, "-")
	dashes := len(name) - len(key)
	flag := dashes > 0 && key != "" && (dashes == 2 || dashes == 1 && isLetter(key[0]))
	switch {
	case m[2] < prevEnd && !flag, m[2] > 0 && isTokenByte(s[m[2]-1]),
		m[1] < len(s) && (s[m[1]] == '=' || s[m[1]] == ':'), referenceKey(name, dashes > 0):
		return prevEnd
	}
	joined := strings.Contains(key, "_")
	spelled := !joined && !isBareShortKey(key) &&
		(strings.Contains(key, "-") && !strings.Contains(key, "--") || isCamelCase(key))
	named := (joined || spelled) && !hasSuffixFold(keySeparators.Replace(key), blankReferenceSuffixes)
	if !named && !flag {
		return prevEnd
	}
	start, end := keyNameValue(s, m[1], m[2], false)
	if v := s[start:end]; named && opaqueToken(v) && !(spelled && flagWord(v)) {
		add(start, end)
	}
	return max(prevEnd, end)
}

// blankReferenceSuffixes mark key names that, before a blank and a value, name a Kubernetes object rather than
// hold a secret: "secret-volume" is the name of a volume (#585). In the other forms such a name holds a secret
// as before.
var blankReferenceSuffixes = []string{"volume"}

// hasSuffixFold reports whether key ends in one of suffixes, in any case.
func hasSuffixFold(key string, suffixes []string) bool {
	key = strings.ToLower(key)
	for _, suffix := range suffixes {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// opaqueToken reports whether v is an opaque key, the one value the blank form redacts: 16 or more
// characters, every one of them from "A-Z a-z 0-9 / + = _ -", and at least one digit or one of "/+=".
func opaqueToken(v string) bool {
	if len(v) < 16 {
		return false
	}
	marked := false
	for i := range len(v) {
		switch c := v[i]; {
		case c >= '0' && c <= '9', c == '/', c == '+', c == '=':
			marked = true
		case !isLetter(c) && c != '_' && c != '-':
			return false
		}
	}
	return marked
}

// isLetter reports whether c is an ASCII letter.
func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isCamelCase reports whether key joins words in camelCase: it starts with an ASCII letter and has a
// lower-case letter or a digit right before an upper-case one ("clientSecret", "dbPassword", "ClientSecret").
func isCamelCase(key string) bool {
	if key == "" || !isLetter(key[0]) {
		return false
	}
	for i := 1; i < len(key); i++ {
		if key[i] >= 'A' && key[i] <= 'Z' && (key[i-1] >= 'a' && key[i-1] <= 'z' || key[i-1] >= '0' && key[i-1] <= '9') {
			return true
		}
	}
	return false
}

// flagWord reports whether v reads as a command-line flag: one or two dashes, then a lower-case letter, then
// only lower-case letters, digits and dashes, and optionally "=" and its value ("-n", "--namespace",
// "--dry-run", "--from-literal=x=y").
func flagWord(v string) bool {
	v, _, _ = strings.Cut(v, "=")
	w := strings.TrimPrefix(v, "-")
	if w == v {
		return false
	}
	w = strings.TrimPrefix(w, "-")
	if w == "" || w[0] < 'a' || w[0] > 'z' {
		return false
	}
	for i := range len(w) {
		if c := w[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// keyNameValue returns the value after a secret flag or a compound key name and a blank (#585), from pos;
// the name starts at keyStart. A value that opens with a quote, plain or escaped as JSON inside a JSON string
// (escapeLevel), runs to its closing quote on the same line, with escapes handled (quotedEnd,
// escapedQuotedSpan); when that quote does not close, the value is the quote and the token after it. Any
// other value is one unquoted token (tokenEnd). A value never starts in the middle of a quoted string it did
// not open: when a quote opens right before the name and the separator did not close it (closed is false),
// the same quote at pos closes that string, and the value is empty. Each call reads forward from pos to the
// end of its line at most, and back from keyStart over at most eight backslashes.
//
// Values with blanks that are not quoted, YAML block and flow scalars, shell words of glued pieces and arrays
// or objects lose only their first token (TestKeyNameKnownLeaks): taking more needs a tokenizer per format.
func keyNameValue(s string, pos, keyStart int, closed bool) (int, int) {
	if pos >= len(s) {
		return pos, pos
	}
	level := escapeLevel(s, pos)
	if !closed {
		if enclosing, ok := enclosingQuote(s, keyStart); ok && enclosing == level && s[pos+1<<level-1] == s[keyStart-1] {
			return pos, pos
		}
	}
	if level > 0 {
		return escapedQuotedSpan(s, pos, level)
	}
	if c := s[pos]; c == '"' || c == '\'' {
		if end := quotedEnd(s, pos); end > 0 {
			return pos + 1, end - 1
		}
		return pos, tokenEnd(s, pos+1)
	}
	if end := tokenEnd(s, pos); end > pos || s[pos] != '\\' {
		return pos, end
	}
	// A quote escaped in a way no JSON level gives ("\'"): the quote and the token after it, as for a
	// quote that does not close.
	q := pos
	for q < len(s) && s[q] == '\\' {
		q++
	}
	return pos, tokenEnd(s, q+1)
}

// tokenEnd returns the end of the unquoted value that starts at pos, by the one extent rule of every
// key-name form: the value ends at whitespace, the line end, or a quote character ("\"", "'", or one escaped
// by backslashes), and at nothing else. So a key inside a JSON or logfmt string cannot take the fields after
// it ("{\"msg\":\"ClientSecretCredential AuthenticationFailed\",\"reason\":\"x\"}" keeps "reason"), while ",",
// "]" and "}" stay in the value ("[--password, abc, --x]" loses "abc,"), as one inside the value cannot be
// told apart from one of the text around it ("--password abc,def").
func tokenEnd(s string, pos int) int {
	for i := pos; i < len(s); i++ {
		switch c := s[i]; {
		case isSpace(c), c == '"', c == '\'':
			return i
		case c == '\\':
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			if j < len(s) && (s[j] == '"' || s[j] == '\'') {
				return i
			}
			i = j - 1
		}
	}
	return len(s)
}

// enclosingQuote reports whether a quote opens right before the key at keyStart, and the level of its
// escaping: 0 for a plain quote, 1 to 3 for a double quote escaped as in escapeLevel.
func enclosingQuote(s string, keyStart int) (int, bool) {
	if keyStart == 0 {
		return 0, false
	}
	q := s[keyStart-1]
	if q != '"' && q != '\'' {
		return 0, false
	}
	r := 0
	for keyStart-1-r > 0 && s[keyStart-2-r] == '\\' && r < 8 {
		r++
	}
	switch {
	case r == 0:
		return 0, true
	case q == '\'':
		return 0, false
	case r == 1:
		return 1, true
	case r == 3:
		return 2, true
	case r == 7:
		return 3, true
	}
	return 0, false
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
	// shortKeyNamesExpr lists the short key names (shortKeyNames), joined by "|".
	shortKeyNamesExpr = `pass|auth|sig|sas`
	// shortKeyExpr names short keys that hold secrets but are also parts of other words ("bypass",
	// "author", "signal", "kansas", "sasl"), so they count only as a whole key or as the last part of a
	// longer one after "_", "." or "-" ("DB_PASS", "basic_auth", "AZURE_STORAGE_SAS"). "sas" (an Azure shared
	// access signature) also counts as the last word of a camelCase key, spelled "Sas" ("storageSas",
	// "StorageSas"; not "kansas" or "KANSAS"), and only where sasRefused lets it count.
	shortKeyExpr  = `(?:(?:[A-Za-z0-9_.-]*[_.-]|\b)(?:` + shortKeyNamesExpr + `)|[A-Za-z0-9_.-]*(?-i:[a-z0-9]Sas))`
	secretKeyExpr = `["']?` + bareKeyExpr + `["']?`
	flowKeyExpr   = `\\*["']` + bareKeyExpr + `\\*["']|` + bareKeyExpr
	// nextLineHeaderExpr is a YAML key that names a secret with nothing after its colon but blanks or a
	// comment, after the start of a line.
	nextLineHeaderExpr = `[ \t]*(?:-[ \t]+)?(` + secretKeyExpr + `)[ \t]*:(?:[ \t]+#[^\r\n]*)?[ \t]*\r?$`
	// jsonSpaceExpr is JSON whitespace, also escaped inside a JSON string ("\n", "\t").
	jsonSpaceExpr = `(?:[ \t\r\n]|\\+[nrt])*`
	// flagSpaceExpr is one piece of the separator between a secret-named flag and its value: whitespace,
	// real line ends included, a YAML list dash and a blank only after a line end and indentation, or a
	// backslash directly before a line end (a shell line continuation).
	flagSpaceExpr = `(?:[ \t\r]|\n(?:[ \t]*-[ \t])?|\\\r?\n)`
	// flagSeparatorExpr is what stands between a secret-named flag and its value: the flag's closing quote,
	// plain or escaped, and a comma directly after it (JSON and YAML flow lists of quoted items), then
	// flagSpaceExpr pieces; or flagSpaceExpr pieces alone. Nothing else: literal "\n", "\r" and "\t" pairs,
	// a bare comma and a bare "-" are the start of the value.
	flagSeparatorExpr = `(?:\\*["'](?:,` + flagSpaceExpr + `*|` + flagSpaceExpr + `+)|` + flagSpaceExpr + `+)`
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
		// Microsoft Entra client secrets of the current format: three characters, a digit and "Q~" (mostly
		// "8Q~"), then 31 to 34 characters, 40 in all, of letters, digits and "_~.-". Matched as a whole here,
		// because "~" and "." cut it into parts the long-token rule may keep (#581). No word boundary before
		// it: the three characters before the marker are taken, whatever is glued in front. After the 31 to
		// 34 characters, the match runs on to the end of the run of those characters, though not onto a last
		// ".", so a secret glued to another is taken with it instead of leaving the other's tail, while a full
		// stop after a secret stays. "~" may be percent-encoded ("%7E"), as a URL encoder writes it. The
		// minimum length keeps Windows short names ("APP8Q~1"), tilde version ranges and
		// dotted names out; a secret cut to 36 characters or fewer by truncation is not matched.
		{kind: KindToken, re: regexp.MustCompile(
			`[A-Za-z0-9_~.-]{3}[0-9]Q(?:~|%7[Ee])(?:[A-Za-z0-9_~.-]|%7[Ee]){31,34}` +
				`(?:(?:[A-Za-z0-9_~.-]|%7[Ee])*(?:[A-Za-z0-9_~-]|%7[Ee]))?`)},
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
		{kind: KindCredential, span: keyValueSpan(boundaryWhitespace, false), re: regexp.MustCompile(
			`(?i)(` + bareKeyExpr + `)[ \t]*[:=][ \t]*`)},
		// Command-line flags whose name holds a secret, with the value after blanks or line ends, in an
		// argument list (JSON, escaped JSON or a YAML list) or after a shell line continuation:
		// "--client-secret <value>", "\"--password\", \"<value>\"", "- --password\n- <value>"
		// (flagValueSpan). "--name=value" is the rule above.
		{kind: KindCredential, span: flagValueSpan, re: regexp.MustCompile(
			`(?i)--?(` + bareKeyExpr + `)` + flagSeparatorExpr)},
		// A key name of two or more words joined by "_", blanks and one token: "aws_secret_access_key <value>"
		// (compoundValueSpans).
		{kind: KindCredential, spans: compoundValueSpans, re: regexp.MustCompile(
			`(?i)(` + bareKeyExpr + `)[ \t]+`)},
		// JSON and YAML flow mappings: a field after "{" or ",", ending at its closing quote or at , } ].
		// JSON whitespace, newlines included and also escaped, may surround the key and the colon.
		{kind: KindCredential, span: keyValueSpan(boundaryFlow, false), re: regexp.MustCompile(
			`(?i)[{,]` + jsonSpaceExpr + `(` + flowKeyExpr + `)` + jsonSpaceExpr + `:` + jsonSpaceExpr)},
		// Kubernetes environment variables in JSON and YAML flow form: the value after a name that holds a
		// secret, {"name":"DB_PASSWORD","value":"..."}.
		{kind: KindCredential, span: keyValueSpan(boundaryFlow, true), re: regexp.MustCompile(
			`(?i)\\*["']?\bname\\*["']?` + jsonSpaceExpr + `:` + jsonSpaceExpr + `\\*["']?(` + bareKeyExpr + `)\\*["']?` +
				jsonSpaceExpr + `,` + jsonSpaceExpr + `\\*["']?value\\*["']?` + jsonSpaceExpr + `:` + jsonSpaceExpr)},
		// YAML block mappings: a key at the start of a line, its value to the line end plus continuation
		// lines indented deeper than the key.
		{kind: KindCredential, span: keyValueSpan(boundaryLine, false), re: regexp.MustCompile(
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
		// longTokenSpans).
		{kind: KindToken, spans: longTokenSpans, re: regexp.MustCompile(`[A-Za-z0-9+/_-]{24,}=*`)},
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
