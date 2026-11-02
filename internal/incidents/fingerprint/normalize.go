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

	"github.com/pkalin/shadewright/internal/redact"
)

const (
	// MaxSymptomLength is the longest normalized symptom, in bytes (Incident spec.symptom MaxLength). A
	// byte bound also bounds the character count the API server checks.
	MaxSymptomLength = 1024

	// maxInputLength bounds the work done on a single note. Kubernetes caps event notes at 1 KiB, so this
	// only matters for status messages or hostile input.
	maxInputLength = 64 * 1024
)

// Placeholders. Input angle brackets, square brackets and asterisks are replaced before any rule runs, so
// neither these nor the markers of package redact ("[REDACTED:kind]", "[kind]", "***") can come from the
// input.
const (
	placeholderRedacted = "<redacted>"
	placeholderTime     = "<time>"
	placeholderDuration = "<duration>"
	placeholderIP       = "<ip>"
	placeholderUID      = "<uid>"
	placeholderDigest   = "<digest>"
	placeholderPod      = "<pod>"
	placeholderHash     = "<hash>"
)

// rule replaces a part of the text which changes between occurrences of the same problem.
type rule struct {
	re   *regexp.Regexp
	repl string
}

func apply(s string, rules []rule) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// markerRules turn the markers of package redact into placeholders. The marker length depends on the
// length of the redacted text ("[ip]" or "[REDACTED:ip]", "[REDACTED]" or "***"), so without this the same
// problem seen from two addresses or with two secret lengths would get two fingerprints.
var markerRules = []rule{
	{regexp.MustCompile(`\[(?:REDACTED:)?ip\]`), placeholderIP},
	{regexp.MustCompile(`\[[^\]]*\]|\*+`), placeholderRedacted},
}

// identifierRules replace content digests (they change on every image build, while the image name stays)
// and object UIDs. They run after redaction, so a substitution can never hide part of a secret.
var identifierRules = []rule{
	{regexp.MustCompile(`(?i)\b(sha256|sha512):[0-9a-f]{32,128}\b`), "${1}:" + placeholderDigest},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), placeholderUID},
}

var timeRules = []rule{
	// RFC 3339 and similar timestamps, then bare dates and times of day.
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{1,2}:\d{2}(?::\d{2}(?:[.,]\d+)?)?(?:\s?(?:Z|[+-]\d{2}:?\d{2}))?(?:\s(?:UTC|GMT))?\b`), placeholderTime},
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`), placeholderTime},
	{regexp.MustCompile(`\b\d{1,2}:\d{2}:\d{2}(?:[.,]\d+)?\b`), placeholderTime},
	// Unix timestamps in seconds, milliseconds or nanoseconds.
	{regexp.MustCompile(`\b1\d{9}(?:\.\d+|\d{3}|\d{6}|\d{9})?\b`), placeholderTime},
}

// ipRules catch addresses package redact left alone, then fold an address with its brackets and port into
// one placeholder. The compressed IPv6 form is tried first, so "2001:db8:1::1" is one match, not two.
var ipRules = []rule{
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), placeholderIP},
	{regexp.MustCompile(`(?i)(?:(?:\b[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)?::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*\b)?|\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}\b)`), placeholderIP},
	{regexp.MustCompile(`\(?` + placeholderIP + `\)?(?::\d{1,5}\b)?`), placeholderIP},
}

// The alphabet Kubernetes uses for generated name suffixes and pod-template hashes (no vowels, no 0, 1, 3).
const k8sAlphabet = `[bcdfghjklmnpqrstvwxz2456789]`

const (
	// workloadName is the part of a generated name that is kept.
	workloadName = `([a-z0-9](?:[-a-z0-9.]*[a-z0-9])?)`
	// nameEnd captures the character after a generated name, so that it can be put back. Kubelet writes
	// "<pod>_<namespace>(<uid>)".
	nameEnd = `([^A-Za-z0-9]|$)`
	// refSep is what joins a kind to a name: "pod/web-x", "pod web-x", `pod "web-x"`, `pod="shop/web-x"`,
	// "Created pod: web-x".
	refSep = `(?:\s*[:=/]\s*|\s+)["']?(?:[a-z0-9][-a-z0-9]*/)?`
	podRef = `((?i:\bpods?)` + refSep + `)`
	rsRef  = `((?i:\breplica ?sets?|\brs)` + refSep + `)`
)

// generatedNameRules strip pod name suffixes and ReplicaSet hashes, but only from names the text calls a
// Pod or a ReplicaSet. Other names, such as a missing Secret "db-7d4b9", are kept, because they tell two
// problems apart.
var generatedNameRules = []rule{
	{regexp.MustCompile(podRef + workloadName + `-` + k8sAlphabet + `{6,10}-` + k8sAlphabet + `{5}` + nameEnd), "${1}${2}-" + placeholderPod + "${3}"},
	{regexp.MustCompile(podRef + workloadName + `-` + k8sAlphabet + `{5}` + nameEnd), "${1}${2}-" + placeholderPod + "${3}"},
	{regexp.MustCompile(rsRef + workloadName + `-` + k8sAlphabet + `{6,10}` + nameEnd), "${1}${2}-" + placeholderHash + "${3}"},
}

// regardingNames match the name of the object an observation is about, when it is a generated Pod or
// ReplicaSet name. Occurrences of exactly that name are stripped wherever they appear in the note.
var regardingNames = map[string]rule{
	// The name part is lazy, so a pod-template hash is stripped with the suffix when there is one.
	"Pod":        {regexp.MustCompile(`^([a-z0-9](?:[-a-z0-9.]*?[a-z0-9])?)-(?:` + k8sAlphabet + `{6,10}-)?` + k8sAlphabet + `{5}$`), "${1}-" + placeholderPod},
	"ReplicaSet": {regexp.MustCompile(`^` + workloadName + `-` + k8sAlphabet + `{6,10}$`), "${1}-" + placeholderHash},
}

var durationRules = []rule{
	// Go style (1m30s, 250ms) and words (30 seconds).
	{regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h)(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))*\b`), placeholderDuration},
	{regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s?(?:nanoseconds?|microseconds?|milliseconds?|seconds?|secs?|minutes?|mins?|hours?|days?)\b`), placeholderDuration},
}

// Normalize returns the symptom of o. Its note is first redacted with package redact. Then the variable
// parts (UIDs, digests, timestamps, IPs, pod name suffixes, ReplicaSet hashes and durations) are replaced by
// placeholders, whitespace is collapsed, and the result is cut to [MaxSymptomLength] bytes. Image names,
// error codes, the names of missing Secrets or ConfigMaps and dependency kinds are kept. Nothing in the
// result is changed by redacting it again, apart from placeholders.
func Normalize(o Observation) string {
	s := sanitize(o.Note)
	s = redactText(s)
	s = apply(s, identifierRules)
	s = apply(s, timeRules)
	s = apply(s, ipRules)
	s = stripRegarding(s, o.RegardingKind, o.RegardingName)
	s = apply(s, generatedNameRules)
	s = apply(s, durationRules)
	s = strings.Join(strings.Fields(s), " ")
	return finish(s)
}

// redactText redacts s and turns the markers into placeholders.
func redactText(s string) string {
	return apply(redact.String(s), markerRules)
}

// stripRegarding replaces the generated suffix of the regarding object's name wherever the name appears.
func stripRegarding(s, kind, name string) string {
	r, ok := regardingNames[kind]
	if !ok || name == "" || !r.re.MatchString(name) {
		return s
	}
	return strings.ReplaceAll(s, name, r.re.ReplaceAllString(name, r.repl))
}

// sanitize bounds the input, makes it valid UTF-8, turns control and format characters into spaces and
// replaces angle brackets, square brackets and asterisks, so that placeholders and redaction markers cannot
// be forged by the input.
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
		case r == '<' || r == '[':
			return '('
		case r == '>' || r == ']':
			return ')'
		case r == '*':
			return '#'
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.IsSpace(r):
			return ' '
		}
		return r
	}, s)
}

// finish cuts s to MaxSymptomLength and redacts again until nothing changes, because a substitution or a
// cut can create a new match. If that does not settle, the whole symptom is withheld.
func finish(s string) string {
	for range 4 {
		s = truncate(s, MaxSymptomLength)
		r := redactText(s)
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
