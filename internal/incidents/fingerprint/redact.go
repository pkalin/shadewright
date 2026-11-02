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

package fingerprint

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const placeholderRedacted = "<redacted>"

// secretRule replaces one kind of secret. Every replacement starts its secret part with "<", and no rule
// matches a value that starts with "<", so redacting twice changes nothing.
type secretRule struct {
	re   *regexp.Regexp
	repl string
}

// secretValue is the value after a secret-looking key: an optional scheme word such as Bearer, an optional
// quote, then everything up to a separator. A value that starts with "<" is a placeholder.
const secretValue = `(?:(?:bearer|basic|token)\s+)?["']?[^\s<;,&'"]+`

var secretRules = []secretRule{
	// PEM private keys, up to the end marker or the end of the text.
	{regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----(?s:.*?)(?:-----END[A-Z ]*PRIVATE KEY-----|$)`), placeholderRedacted},
	// Credentials in URLs: scheme://user:password@host keeps the scheme and host.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s@<]+@`), "${1}" + placeholderRedacted + "@"},
	// key=value and key: value pairs, including connection strings (AccountKey=...;) and headers.
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|pass|secret|token|access[_-]?token|refresh[_-]?token|id[_-]?token|auth|authorization|api[_-]?key|apikey|access[_-]?key|secret[_-]?key|account[_-]?key|shared[_-]?access[_-]?key|sharedaccesssignature|client[_-]?secret|private[_-]?key|credentials?|sig|x-api-key)(["']?\s*[=:]\s*)` + secretValue), "${1}${2}" + placeholderRedacted},
	// Authorization schemes outside a key: value pair.
	{regexp.MustCompile(`(?i)\b(bearer)\s+[^\s<]+`), "${1} " + placeholderRedacted},
	// JSON Web Tokens.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`), placeholderRedacted},
	// Well-known token formats: GitHub, AWS, Slack, OpenAI-style, Google API keys.
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|(?:AKIA|ASIA)[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{35})`), placeholderRedacted},
}

// minTokenLength is the shortest run of base64 or hex characters treated as a random token.
const minTokenLength = 24

// longTokenRe finds long runs of base64 or hex characters; [isLongToken] decides whether one is a secret.
var longTokenRe = regexp.MustCompile(`[A-Za-z0-9+/_=]{24,}`)

// isLongToken reports whether a run looks random: some part between slashes is at least minTokenLength
// long and mixes letters and digits. Paths and image names (short parts between slashes) and long names
// made of words only are kept.
func isLongToken(s string) bool {
	for part := range strings.SplitSeq(s, "/") {
		if len(part) >= minTokenLength && mixesLettersAndDigits(part) {
			return true
		}
	}
	return false
}

func mixesLettersAndDigits(s string) bool {
	var letter, digit bool
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			digit = true
		case unicode.IsLetter(r):
			letter = true
		}
	}
	return letter && digit
}

// redact replaces every secret in s by a placeholder.
func redact(s string) string {
	for _, r := range secretRules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return longTokenRe.ReplaceAllStringFunc(s, func(m string) string {
		if isLongToken(m) {
			return placeholderRedacted
		}
		return m
	})
}

// containsSecret reports whether any redaction rule matches s.
func containsSecret(s string) bool {
	for _, r := range secretRules {
		if r.re.MatchString(s) {
			return true
		}
	}
	return slices.ContainsFunc(longTokenRe.FindAllString(s, -1), isLongToken)
}
