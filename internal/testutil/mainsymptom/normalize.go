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

package mainsymptom

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

const (
	// MaxSymptomLength is the longest normalized symptom, in bytes (Incident spec.symptom MaxLength). A
	// byte bound also bounds the character count the API server checks.
	MaxSymptomLength = 1024

	// maxInputLength bounds the work done on a single note. The exported functions take the long-note marker
	// for any note over redact.MaxShortEntry, so this only holds for a direct caller of normalizeWith. A longer
	// note is withheld whole, never cut before redaction: a cut could drop the closing quote of a secret and
	// leak its tail.
	maxInputLength = 1 << 20
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
	// placeholderLongNote is the normalized symptom of every note longer than redact.MaxShortEntry bytes: its
	// text is not part of the fingerprint (#1087, decision of 2026-11-22).
	placeholderLongNote = "<long note>"
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
	// nameEnd captures the character after a generated name, so that it can be put back. "-" and "."
	// belong to names, so a suffix is stripped only at the end of the complete name. Kubelet writes
	// "<pod>_<namespace>(<uid>)".
	nameEnd = `([^-.A-Za-z0-9]|$)`
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
	// A duration glued to a name ("db-10s", "app.5m") is part of the name and is kept.
	{regexp.MustCompile(`(^|[^-./\w])\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h)(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))*\b`), "${1}" + placeholderDuration},
	{regexp.MustCompile(`(?i)(^|[^-./\w])\d+(?:\.\d+)?\s?(?:nanoseconds?|microseconds?|milliseconds?|seconds?|secs?|minutes?|mins?|hours?|days?)\b`), "${1}" + placeholderDuration},
}

// Normalize returns the normalized symptom of o, the text [Fingerprint] hashes. Its note is first redacted
// with package redact. Then the variable parts (UIDs, digests, timestamps, IPs, pod name suffixes, ReplicaSet
// hashes and durations) are replaced by placeholders, whitespace is collapsed, and the result is cut to
// [MaxSymptomLength] bytes. Image names, error codes, the names of missing Secrets or ConfigMaps and
// dependency kinds are kept. Nothing in the result is changed by redacting it again, apart from placeholders.
//
// A Kubernetes milli quantity ("averageValue=6500m") reads as minutes here and becomes a placeholder: a value
// that changes while the problem lasts must not split its Incident. The text that is shown uses [Symptom].
//
// A note longer than redact.MaxShortEntry bytes is not read: the symptom is the fixed "<long note>", whatever the
// note says and whatever the analyzer is doing. Its redaction would need the full redactor, which can wait for the
// limit on the heap of the redactions running at once, and a fingerprint must not depend on that wait. The note is
// not cut to the short bound instead, because a cut can end inside a secret and leave a part the full reading would
// hide. So two long notes of one subject and reason are one Incident (#1087, decision of 2026-11-22).
func Normalize(o Observation) string {
	if len(o.Note) > redact.MaxShortEntry {
		return placeholderLongNote
	}
	return normalize(o, keepNone)
}

// Symptom returns the symptom of o as an Incident shows it and the model reads it: [Normalize], except that a
// milli quantity that is the value of a key ("averageValue=6500m", "cpu: 250m") is kept, since it is evidence
// and the fingerprint does not depend on it. Other durations become placeholders as in [Normalize], and the
// binary and decimal quantity suffixes (Mi, Gi, k, M) are never read as durations. A bare minute count after
// a key ("timeout=5m") is kept too: it cannot be told from a quantity.
//
// A note longer than redact.MaxShortEntry bytes is not read here either: the symptom is "<long note: N bytes>",
// on a busy and an idle analyzer alike, so the collector never waits for the heap limit (#1087, decision of
// 2026-11-22). The note itself is read through the events tool, with the full redactor and the caller's deadline.
func Symptom(o Observation) string {
	if len(o.Note) > redact.MaxShortEntry {
		return longMarker("note", len(o.Note))
	}
	return normalize(o, keepAfterKey)
}

// Value returns an evidence value as it is shown: [Symptom] of the value alone, where the value as a whole
// counts as the value of its field, so "212500m" is kept. A value longer than redact.MaxShortEntry bytes is not
// read: it is "<long value: N bytes>", as [Symptom] does for a long note.
func Value(s string) string {
	if len(s) > redact.MaxShortEntry {
		return longMarker("value", len(s))
	}
	return normalize(Observation{Note: s}, keepAfterKeyOrWhole)
}

// longMarker is the shown text of a note or value of n bytes, longer than redact.MaxShortEntry, that is not read.
// Its angle brackets cannot come from the input, which sanitize rewrites.
func longMarker(what string, n int) string {
	return "<long " + what + ": " + strconv.Itoa(n) + " bytes>"
}

// quantityMode says which milli quantities the duration rule leaves alone.
type quantityMode int

const (
	// keepNone replaces every match of the duration rules (the fingerprint input).
	keepNone quantityMode = iota
	// keepAfterKey keeps a milli quantity right after "key=" or "key:" (with at most one space).
	keepAfterKey
	// keepAfterKeyOrWhole also keeps a milli quantity that is the whole text.
	keepAfterKeyOrWhole
)

func normalize(o Observation, mode quantityMode) string {
	return normalizeWith(redact.ReplaceCheckedShort, o, mode)
}

// replaceChecked is the signature of redact.ReplaceCheckedShort.
type replaceChecked func(s string, repl func(kind, text string) string, between func(string) string) (string, bool)

// normalizeWith is normalize with the redaction given: redact.ReplaceCheckedShort, which takes no share of the
// limit on the heap of the redactions running at once and never waits for an analysis, or in a test the same
// method of a redactor on a lowered work budget (#962). Every caller gives it a note of at most
// redact.MaxShortEntry bytes; a longer one is withheld by the short entry point and stands as "<redacted>".
func normalizeWith(replace replaceChecked, o Observation, mode quantityMode) string {
	if len(o.Note) > maxInputLength {
		return placeholderRedacted
	}
	// Redact the note as it was read, before any character is replaced: sanitize turns ">" into ")", which
	// would hide a folded block scalar indicator from the rules (#840). Each match becomes its placeholder
	// at once, and only the text between matches is sanitized.
	s, ok := replace(o.Note, rawPlaceholder, sanitize)
	if !ok {
		// Redaction withheld the note whole (its work budget ran out): it stands as a note too long to read. The
		// flag says so, never the text: a note that spells the marker is a note like any other (#873).
		return placeholderRedacted
	}
	s = expandShortIPv6(s)
	s = flatten(s)
	s = apply(s, identifierRules)
	s = apply(s, timeRules)
	s = apply(s, ipRules)
	s = stripRegarding(s, o.RegardingKind, o.RegardingName)
	s = apply(s, generatedNameRules)
	if mode == keepNone {
		s = applyOutsideQuotes(s, func(seg string, _ int) string { return apply(seg, durationRules) })
	} else {
		full := s
		whole := mode == keepAfterKeyOrWhole && milliQuantityRe.MatchString(strings.TrimSpace(full))
		s = applyOutsideQuotes(s, func(seg string, off int) string {
			if whole {
				return seg
			}
			return apply(keepQuantities(seg, full, off), durationRules[1:])
		})
	}
	s = strings.Join(strings.Fields(s), " ")
	return finish(s)
}

// milliQuantityRe matches a whole milli quantity, which the Go style duration rule reads as minutes.
var milliQuantityRe = regexp.MustCompile(`^\d+(?:\.\d+)?m$`)

// keepQuantities applies the Go style duration rule (durationRules[0]) to seg, which starts at byte off of
// full, but keeps a match that is a milli quantity right after a key. Matches are found as ReplaceAllString
// finds them, so every other match is replaced exactly as in [Normalize].
func keepQuantities(seg, full string, off int) string {
	var b strings.Builder
	pos := 0
	for _, m := range durationRules[0].re.FindAllStringSubmatchIndex(seg, -1) {
		value := m[3] // the end of the first group: the character before the duration, when there is one
		if milliQuantityRe.MatchString(seg[value:m[1]]) && afterKey(full, off+value) {
			continue
		}
		b.WriteString(seg[pos:value])
		b.WriteString(placeholderDuration)
		pos = m[1]
	}
	b.WriteString(seg[pos:])
	return b.String()
}

// afterKey reports whether s[:i] ends with a key and "=" or ":", then at most one space. It looks only at the
// last three bytes, so the cost does not grow with the text.
func afterKey(s string, i int) bool {
	if i > 0 && s[i-1] == ' ' {
		i--
	}
	return i >= 2 && (s[i-1] == '=' || s[i-1] == ':') && isKeyChar(s[i-2])
}

// isKeyChar reports whether c can end a key such as "averageValue" or "status.currentMetrics.0.value".
func isKeyChar(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// shortIPv6Re finds IPv6 candidates shorter than the "[ip]" marker, such as "::1".
var shortIPv6Re = regexp.MustCompile(`(?i)(^|[^0-9a-z:.])([0-9a-f]?::[0-9a-f]?)($|[^0-9a-z:.])`)

// expandShortIPv6 writes IPv6 addresses shorter than four bytes in full. Package redact replaces them with
// "***" (no shorter marker names the kind), which would make "::1" and "0:0:0:0:0:0:0:1" differ.
func expandShortIPv6(s string) string {
	return shortIPv6Re.ReplaceAllStringFunc(s, func(m string) string {
		sub := shortIPv6Re.FindStringSubmatch(m)
		addr, err := netip.ParseAddr(sub[2])
		if err != nil || !addr.Is6() || sub[2] == "::" {
			return m
		}
		return sub[1] + addr.StringExpanded() + sub[3]
	})
}

// quotedRe matches a double-quoted span, such as an object or image name in an event note.
var quotedRe = regexp.MustCompile(`"[^"]*"`)

// applyOutsideQuotes applies f to the text outside double-quoted spans, so that names such as secret
// "db-10s" are never rewritten. f gets each span and the byte offset where it starts in s.
func applyOutsideQuotes(s string, f func(seg string, off int) string) string {
	var b strings.Builder
	pos := 0
	for _, m := range quotedRe.FindAllStringIndex(s, -1) {
		b.WriteString(f(s[pos:m[0]], pos))
		b.WriteString(s[m[0]:m[1]])
		pos = m[1]
	}
	b.WriteString(f(s[pos:], pos))
	return b.String()
}

// rawPlaceholder is the placeholder of a match the redactor finds in the note as it was read: the one
// [redactText] gives the same match once the note is sanitized. An address is "<ip>" when its marker names
// the kind ("[ip]" needs four bytes) or when it is a short IPv6 address that [expandShortIPv6] writes in full;
// anything else is "<redacted>".
func rawPlaceholder(kind, text string) string {
	if kind == redact.KindIP && (len(text) >= len("[ip]") || expandShortIPv6(" "+text+" ") != " "+text+" ") {
		return placeholderIP
	}
	return placeholderRedacted
}

// redactText redacts s and turns the markers into placeholders. [finish] cuts the symptom to MaxSymptomLength
// first, so it is a short entry, which never waits for an analysis.
func redactText(s string) string {
	return apply(redact.StringShort(s), markerRules)
}

// stripRegarding replaces the generated suffix of the regarding object's name wherever the whole name
// appears, never inside a longer name such as a ConfigMap "settings-web-q8wzt".
func stripRegarding(s, kind, name string) string {
	r, ok := regardingNames[kind]
	if !ok || name == "" || !r.re.MatchString(name) {
		return s
	}
	repl := r.re.ReplaceAllString(name, r.repl)
	var b strings.Builder
	pos := 0
	for {
		i := strings.Index(s[pos:], name)
		if i < 0 {
			break
		}
		start, end := pos+i, pos+i+len(name)
		b.WriteString(s[pos:start])
		if (start == 0 || !isNameChar(s[start-1])) && (end == len(s) || !isNameChar(s[end])) {
			b.WriteString(repl)
		} else {
			b.WriteString(name)
		}
		pos = end
	}
	b.WriteString(s[pos:])
	return b.String()
}

// isNameChar reports whether c can appear in a Kubernetes object name.
func isNameChar(c byte) bool {
	return c == '-' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z'
}

// sanitize makes the input valid UTF-8, turns control and format characters other than line breaks and
// tabs into spaces, and replaces angle brackets, square brackets and asterisks, so that placeholders and
// redaction markers cannot be forged by the input. Layout is kept for redaction.
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "?")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '<' || r == '[':
			return '('
		case r == '>' || r == ']':
			return ')'
		case r == '*':
			return '#'
		case r == '\n' || r == '\r' || r == '\t':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.IsSpace(r):
			return ' '
		}
		return r
	}, s)
}

// flatten turns all remaining whitespace into spaces, after redaction.
func flatten(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
}

// finish cuts s to MaxSymptomLength and redacts again until nothing changes, because a substitution or a
// cut can create a new match. If that does not settle, the whole symptom is withheld.
//
// The note was redacted as it was read before anything here (#845); this pass works on the flattened symptom,
// not on that note, and it stays because two of the 709 recorded fingerprints (fingerprint-main.jsonl) come
// from it: the notes "credentials:\n  user: s3cr3tValue99" and "password: |\n  s3cr3tValue99" flatten to
// "credentials: user: <redacted>" and "password: | <redacted>", which this pass makes "credentials: <redacted>"
// and "password: <redacted>". Without it both fingerprints change, and so do three symptoms of
// TestNormalizeRedacts and the placeholder test (checked 2026-11-17).
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
