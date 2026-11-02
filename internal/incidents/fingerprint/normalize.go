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
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxSymptomLength is the longest normalized symptom, in bytes (Incident spec.symptom MaxLength). A
	// byte bound also bounds the character count the API server checks.
	MaxSymptomLength = 1024

	// maxInputLength bounds the work done on a single note. Kubernetes caps event notes at 1 KiB, so this
	// only matters for status messages or hostile input.
	maxInputLength = 64 * 1024
)

// Placeholders. Input angle brackets are replaced before any rule runs, so these never come from the input.
const (
	placeholderTime     = "<time>"
	placeholderDuration = "<duration>"
	placeholderIP       = "<ip>"
	placeholderUID      = "<uid>"
	placeholderDigest   = "<digest>"
	placeholderPod      = "<pod>"
	placeholderHash     = "<hash>"
)

// The alphabet Kubernetes uses for generated name suffixes and pod-template hashes (no vowels, no 0, 1, 3).
const k8sAlphabet = `[bcdfghjklmnpqrstvwxz2456789]`

// nameEnd captures the character after a generated name, so that it can be put back.
const nameEnd = `([^A-Za-z0-9]|$)`

// variablePart is one rule that replaces a part of the text which changes between occurrences of the same
// problem.
type variablePart struct {
	re   *regexp.Regexp
	repl string
}

// variableParts run in order, after redaction. Pod names run before durations so that a generated suffix
// such as "79s2k" is never mistaken for a duration.
var variableParts = []variablePart{
	// RFC 3339 and similar timestamps, then bare dates and times of day.
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{1,2}:\d{2}(?::\d{2}(?:[.,]\d+)?)?(?:\s?(?:Z|[+-]\d{2}:?\d{2}))?(?:\s(?:UTC|GMT))?\b`), placeholderTime},
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`), placeholderTime},
	{regexp.MustCompile(`\b\d{1,2}:\d{2}:\d{2}(?:[.,]\d+)?\b`), placeholderTime},
	// Unix timestamps in seconds, milliseconds or nanoseconds.
	{regexp.MustCompile(`\b1\d{9}(?:\.\d+|\d{3}|\d{6}|\d{9})?\b`), placeholderTime},
	// IPv4 with an optional port, then IPv6 (full or compressed, optionally bracketed with a port).
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?\b`), placeholderIP},
	{regexp.MustCompile(`(?i)\[?(?:\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}\b|(?:\b[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)?::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*\b)?)(?:\]:\d{1,5}|\])?`), placeholderIP},
	// Pod names: <workload>-<pod-template-hash>-<suffix>, then <workload>-<suffix> (Jobs, DaemonSets,
	// bare ReplicaSets), then ReplicaSet names <workload>-<pod-template-hash>.
	// The name ends at any character that is not a letter or digit; kubelet writes "<pod>_<namespace>(<uid>)".
	{regexp.MustCompile(`\b([a-z0-9](?:[-a-z0-9.]*[a-z0-9])?)-` + k8sAlphabet + `{6,10}-` + k8sAlphabet + `{5}` + nameEnd), "${1}-" + placeholderPod + "${2}"},
	{regexp.MustCompile(`\b([a-z0-9](?:[-a-z0-9.]*[a-z0-9])?)-` + k8sAlphabet + `{5}` + nameEnd), "${1}-" + placeholderPod + "${2}"},
	{regexp.MustCompile(`\b([a-z0-9](?:[-a-z0-9.]*[a-z0-9])?)-` + k8sAlphabet + `{6,10}` + nameEnd), "${1}-" + placeholderHash + "${2}"},
	// Durations: Go style (1m30s, 250ms) and words (30 seconds).
	{regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h)(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))*\b`), placeholderDuration},
	{regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s?(?:nanoseconds?|microseconds?|milliseconds?|seconds?|secs?|minutes?|mins?|hours?|days?)\b`), placeholderDuration},
}

// digestRe matches content digests. They change on every image build, while the image name stays.
var digestRe = regexp.MustCompile(`(?i)\b(sha256|sha512):[0-9a-f]{32,128}\b`)

// uidRe matches object UIDs.
var uidRe = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// Normalize returns the symptom of o: its note with secrets redacted, variable parts (pod name suffixes,
// ReplicaSet hashes, timestamps, durations, IPs, UIDs, digests) replaced by placeholders, whitespace
// collapsed, and the result cut to [MaxSymptomLength] bytes. Image names, error codes, the names of missing
// Secrets or ConfigMaps and dependency kinds are kept. The result never contains text that matches the
// redaction rules.
func Normalize(o Observation) string {
	s := sanitize(o.Note)
	// Digests and UIDs are hex runs that the redaction rules would otherwise treat as tokens.
	s = digestRe.ReplaceAllString(s, "${1}:"+placeholderDigest)
	s = uidRe.ReplaceAllString(s, placeholderUID)
	s = redact(s)
	for _, p := range variableParts {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	s = strings.Join(strings.Fields(s), " ")
	return finish(s)
}

// sanitize bounds the input, makes it valid UTF-8, turns control and format characters into spaces and
// replaces angle brackets, so that placeholders in the output cannot be forged by the input.
func sanitize(s string) string {
	if len(s) > maxInputLength {
		s = s[:maxInputLength]
		// Drop the last, possibly cut, word: a cut secret might no longer match a redaction rule.
		if i := strings.LastIndexFunc(s, unicode.IsSpace); i >= 0 {
			s = s[:i]
		} else {
			s = ""
		}
	}
	s = strings.ToValidUTF8(s, "?")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '<':
			return '('
		case r == '>':
			return ')'
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.IsSpace(r):
			return ' '
		}
		return r
	}, s)
}

// finish cuts s to MaxSymptomLength and redacts again until nothing matches, because a cut can create a
// new match at the end of the text. If that does not settle, the whole symptom is withheld.
func finish(s string) string {
	for range 4 {
		s = truncate(s, MaxSymptomLength)
		r := redact(s)
		if r == s {
			return s
		}
		s = r
	}
	return placeholderRedacted
}

// truncate cuts s to at most n bytes, at the last space when there is one, and never inside a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if i := strings.LastIndexByte(s[:cut], ' '); i > 0 {
		cut = i
	}
	return strings.TrimRight(s[:cut], " ")
}
