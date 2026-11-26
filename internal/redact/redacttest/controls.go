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

package redacttest

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// The controls gate of #852 (decision of 2026-11-22: a new reading may only add to what origin/main hides). Its
// inputs are secret forms (a key, a separator, a flag, a value, a token) with one filler placed at each boundary
// between the form's parts: a control character, a zero-width character, an invalid byte, a blank or a line end,
// or two of them. The probe of each form is the secret it holds, so what an entry point shows of it is known by
// construction ([VisibleGrams]); what origin/main showed of it is a table each gate writes at a commit of main.
// This file, and the gates that read it, compile on origin/main as they are.

// GateCase is one input of the controls gate: the parts of a secret form, joined as they are, with a filler at
// one boundary.
type GateCase struct {
	// Name says which form, filler and place.
	Name string
	// Parts are the form's parts with the filler as a part of its own, in order.
	Parts []string
	// Key, Sep and Value are the text before the form's separator, the separator and the text after it, the filler
	// in whichever it falls (at a boundary of the separator: the key's end or the value's start). Key is empty for
	// a form without one (a token alone).
	Key, Sep, Value string
	// Probe is the secret the form holds.
	Probe string
	// Filler is the filler placed, and Prefix the text before the form ("" or "log: ").
	Filler, Prefix string
	// FormName is the name of the form the case was built from, set when it is built (Form).
	FormName string
}

// Text returns the case as one text.
func (c GateCase) Text() string { return strings.Join(c.Parts, "") }

// gateForm is a secret form: its parts, the index of the separator part (-1 for none) and the probe.
type gateForm struct {
	name  string
	parts []string
	sep   int
	probe string
}

// Probes of the gate. They share no four bytes with a marker of package redact ("[REDACTED:credential]",
// "[token]", ...), with Withheld or with the size note of a value left out, so a four-byte part of a probe in an
// output is the probe's.
const (
	GateProbeValue    = "mysecretvalue"   // #nosec G101 -- a made-up value the gate looks for
	GateProbeHunter   = "Hunter2Secret9x" // #nosec G101 -- a made-up value the gate looks for
	gateProbeBearer   = "Zq7Kp2Vx9Lm4Wt8R"
	gateProbeConnPass = "p4ssw0rdQ7xZ"
	gateProbeAPIKey   = "Wq5Rt7Yu9Io2Pa"
)

// Words of the forms.
const (
	gatePassword = "password"
	gateWord     = "word"
	gateToken    = "token"
	// gateAuthorization names the form of an Authorization header.
	gateAuthorization = "authorization"
)

// GateProbeToken is the GitHub token of Samples (rule 2), made up, the probe of the token forms.
var GateProbeToken = Samples()[2].Secret

// gateForms are the secret forms of the gate.
func gateForms() []gateForm {
	tok := GateProbeToken
	return []gateForm{
		{"key=value", []string{gatePassword, "=", GateProbeValue}, 1, GateProbeValue},
		{"split key=value", []string{"pass", gateWord, "=", GateProbeValue}, 2, GateProbeValue},
		{"split key: value", []string{"pass", gateWord, ":", " ", GateProbeHunter}, 2, GateProbeHunter},
		{"json member", []string{`"password"`, ":", `"`, GateProbeValue, `"`}, 1, GateProbeValue},
		{"split flag", []string{"--pass", gateWord, " ", GateProbeValue}, 2, GateProbeValue},
		{"flag=", []string{"--password", "=", GateProbeHunter}, 1, GateProbeHunter},
		{gateAuthorization, []string{"Authorization", ":", " ", "Bearer", " ", gateProbeBearer}, 1, gateProbeBearer},
		{"glued bearer", []string{"x", "Bearer", " ", gateProbeBearer}, 2, gateProbeBearer},
		{"split token", []string{tok[:20], tok[20:]}, -1, tok},
		{"connection string", []string{"postgres://", "u", ":", gateProbeConnPass, "@", "db.example", "/app"}, 2, gateProbeConnPass},
		{"split env", []string{"DB_PASS", "WORD", "=", GateProbeHunter}, 2, GateProbeHunter},
		{"block", []string{gatePassword, ":", " ", "|", "\n", "  ", GateProbeValue}, 1, GateProbeValue},
		{"split value", []string{gatePassword, "=", GateProbeHunter[:7], GateProbeHunter[7:]}, 1, GateProbeHunter},
		{"api key", []string{"api_key", ":", " ", gateProbeAPIKey}, 1, gateProbeAPIKey},
		{gateToken, []string{gateToken, ":", GateProbeValue}, 1, GateProbeValue},
	}
}

// gateFillers are what the gate places at a boundary: controls of C0, DEL and C1 (U+0085 and the CSI U+009B
// among them), an ANSI colour sequence, zero-width characters, invalid bytes, blanks and line ends, and two of
// them together (a zero-width character and a carriage return: the key of the third count of 2026-11-22).
var gateFillers = []string{
	"\x00", "\x01", "\b", "\v", "\f", "\r", "\x1b", "\x1f", "\x7f", "\u0080", "\u0085", "\u009b", "\u009f",
	"\x1b[31m",
	"\u200b", "\u200c", "\u200d", "\u2060", "\ufeff",
	"\xff", "\xc3", "\x80",
	" ", "\t", "\n", "\r\n", "\u00a0",
	"\u200b\r", "\r\u200b", "\x00\u200b", "\u200b\r ",
}

// GateCases returns the generated inputs of the gate, the same on every run: each form with each filler at each
// boundary of its parts (before the first and after the last included), the form alone and after "log: ", and
// " ok" after it.
func GateCases() []GateCase {
	var out []GateCase
	for _, f := range gateForms() {
		for pos := 0; pos <= len(f.parts); pos++ {
			for fi, fill := range gateFillers {
				for _, prefix := range []string{"", "log: "} {
					if prefix != "" && !gateControl(fill) {
						continue // the prefix only with a control, to keep the corpus small
					}
					out = append(out, gateCase(f, pos, fi, prefix))
				}
			}
		}
	}
	return out
}

// gateControl reports whether fill starts with a control character but newline and tab.
func gateControl(fill string) bool {
	r, _ := utf8.DecodeRuneInString(fill)
	return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f)
}

// gateCase is form f with filler fi at boundary pos.
func gateCase(f gateForm, pos, fi int, prefix string) GateCase {
	fill := gateFillers[fi]
	parts := make([]string, 0, len(f.parts)+3)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	parts = append(parts, f.parts[:pos]...)
	parts = append(parts, fill)
	parts = append(parts, f.parts[pos:]...)
	parts = append(parts, " ok")
	c := GateCase{Name: f.name + " " + strings.TrimSpace(prefix) + " filler " + quoteFill(fill) + " at " + strconv.Itoa(pos),
		Parts: parts, Probe: f.probe, Filler: fill, Prefix: prefix, FormName: f.name}
	if f.sep < 0 {
		c.Value = c.Text()
		return c
	}
	var key, val strings.Builder
	for i, p := range f.parts {
		if i == pos && pos <= f.sep {
			key.WriteString(fill)
		}
		switch {
		case i < f.sep:
			key.WriteString(p)
		case i == f.sep:
			c.Sep = p
		default:
			if i == pos && pos > f.sep {
				val.WriteString(fill)
			}
			val.WriteString(p)
		}
	}
	if pos == len(f.parts) {
		val.WriteString(fill)
	}
	c.Key, c.Value = key.String(), val.String()+" ok"
	return c
}

// quoteFill writes a filler with every byte that is not printable ASCII as \xNN.
func quoteFill(s string) string {
	var b strings.Builder
	for i := range len(s) {
		if c := s[i]; c > 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			const hex = "0123456789abcdef"
			b.WriteString(`\x` + string(hex[c>>4]) + string(hex[c&15]))
		}
	}
	return b.String()
}

// gateGram is the length of the parts of a probe [VisibleGrams] looks for.
const gateGram = 4

// VisibleGrams returns which four-byte parts of probe out shows, bit i for the part that starts at byte i, to a
// reader that skips fillers: out is read as it is and again with every filler removed (a control or blank of any
// kind, a zero-width character, an invalid byte, U+FFFD and "?", which stand for an invalid byte, and a JSON or Go
// escape of one), so a probe a control splits, or a JSON escape of a control, is still read as shown. A gate
// compares the parts an entry point shows with those origin/main's showed: a part main did not show must not be
// shown.
func VisibleGrams(out, probe string) uint64 {
	var bits uint64
	for _, s := range []string{out, withoutFillers(out)} {
		for i := 0; i+gateGram <= len(probe) && i < 64; i++ {
			if strings.Contains(s, probe[i:i+gateGram]) {
				bits |= 1 << i
			}
		}
	}
	return bits
}

// withoutFillers is out with every filler of [VisibleGrams] removed.
func withoutFillers(out string) string {
	var b strings.Builder
	b.Grow(len(out))
	for i := 0; i < len(out); {
		if n := escapeLen(out, i); n > 0 {
			i += n
			continue
		}
		r, n := utf8.DecodeRuneInString(out[i:])
		if !filler(r) {
			b.WriteString(out[i : i+n])
		}
		i += n
	}
	return b.String()
}

// filler reports whether r is a filler: a control, a blank or a separator, a zero-width character, U+FFFD (also
// what an invalid byte decodes to) or "?".
func filler(r rune) bool {
	switch {
	case r < 0x21 || (r >= 0x7f && r <= 0xa0):
		return true
	case r == '?' || r == utf8.RuneError || r == '\ufeff' || r == '\u2028' || r == '\u2029':
		return true
	case r >= '\u200b' && r <= '\u200f', r >= '\u2060' && r <= '\u2064':
		return true
	}
	return false
}

// escapeLen returns the length of a JSON or Go escape of a filler at out[i] (\n, \r, \t, \b, \f, \v, \uXXXX,
// \xNN), or 0.
func escapeLen(out string, i int) int {
	if out[i] != '\\' || i+1 >= len(out) {
		return 0
	}
	switch out[i+1] {
	case 'n', 'r', 't', 'b', 'f', 'v':
		return 2
	case 'x':
		if i+4 <= len(out) {
			return 4
		}
	case 'u':
		if i+6 <= len(out) {
			var r rune
			for _, c := range out[i+2 : i+6] {
				switch {
				case c >= '0' && c <= '9':
					r = r<<4 | (c - '0')
				case c >= 'a' && c <= 'f':
					r = r<<4 | (c - 'a' + 10)
				case c >= 'A' && c <= 'F':
					r = r<<4 | (c - 'A' + 10)
				default:
					return 0
				}
			}
			if filler(r) {
				return 6
			}
		}
	}
	return 0
}

// GateUncovered returns what a sample of the inputs (those built accepts) never takes: each filler, with the
// prefix "log: " for a control, and the prefix itself. A gate that runs a share of the inputs fails on any of them,
// so a stride that lines up with the inputs of one form and boundary cannot leave a filler out.
func GateUncovered(cases []GateCase, built func(i int) bool) []string {
	seen := map[string]bool{}
	for i, c := range cases {
		if built(i) {
			seen[c.Prefix+"\x00"+c.Filler] = true
			seen[c.Prefix] = true
		}
	}
	var out []string
	for _, fill := range gateFillers {
		for _, prefix := range []string{"", "log: "} {
			if prefix != "" && !gateControl(fill) {
				continue
			}
			if !seen[prefix+"\x00"+fill] {
				out = append(out, "the filler "+quoteFill(fill)+" after "+strconv.Quote(prefix))
			}
		}
	}
	if !seen["log: "] {
		out = append(out, `the prefix "log: "`)
	}
	return out
}

// ShownForms are the forms of the gate whose secret the collector's shown paths (the symptom an Incident stores,
// a module's evidence value) hide in plain form, without any filler. The gate's other forms are not hidden there
// even in plain form (a JSON member, a bearer token glued to a word, a token after "token:"), so a filler cannot
// make them worse.
var ShownForms = []string{
	"api key", gateAuthorization, "block", "connection string", "flag=", "key=value", "split env", "split flag",
	"split key: value", "split key=value", "split token", "split value",
}

// Form returns the name of the case's form, as set when the case was built. It is not parsed from Name, which
// holds the prefix after the form ("split key=value log: filler ..."): a parser of the name read the form of every
// case after "log: " as one no list holds, and the cases after "log: " were never checked.
func (c GateCase) Form() string { return c.FormName }

// ShownInScope reports whether a shown path must hide the case's secret: its form is one of ShownForms and its
// filler is not made of ASCII spaces alone (a credential split by plain spaces is shown as it is, out of scope).
func (c GateCase) ShownInScope() bool {
	for _, f := range ShownForms {
		if f == c.Form() {
			return strings.Trim(c.Filler, " ") != ""
		}
	}
	return false
}

// CredentialChars keeps of s the characters a probe of the gate is made of: ASCII letters, digits and "_".
// Everything else, separators, punctuation and the brackets of markers included, is dropped, so a probe split by
// anything an output shows reads as whole.
func CredentialChars(s string) string {
	var b strings.Builder
	for i := range len(s) {
		if c := s[i]; c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ShowsProbe reports whether out shows probe, with no reference to what a matcher does: the probe whole once out
// and probe are squeezed to CredentialChars, or any four-byte part of it (VisibleGrams).
func ShowsProbe(out, probe string) bool {
	return strings.Contains(CredentialChars(out), CredentialChars(probe)) || VisibleGrams(out, probe) != 0
}
