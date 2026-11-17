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

package providers

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"
)

// removedControl reports whether r is a code point model text is stored without: every code point of
// Unicode category Cc (C0 U+0000 to U+001F, DEL U+007F, C1 U+0080 to U+009F) except newline and tab.
func removedControl(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f)
}

// cleanText reports whether s is valid UTF-8 without a removed control code point, so StripControls
// returns it as it is.
func cleanText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, removedControl)
}

// StripControls returns s without its control characters: every code point of Unicode category Cc except
// newline and tab is removed, and each byte that is not UTF-8 becomes U+FFFD (as encoding/json and the
// Kubernetes API store it), so no byte a terminal reads as a control is left, in its 7-bit or 8-bit form. It
// knows no terminal grammar: the rest of an escape sequence stays as the visible text it is, and letters
// that a backspace, a carriage return or a sequence would hide on a terminal are shown. Text that is valid
// UTF-8 without such a code point comes back unchanged, byte for byte.
func StripControls(s string) string {
	if cleanText(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteRune(utf8.RuneError)
		case !removedControl(r):
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// StripControlsJSON returns the JSON text raw with [StripControls] applied to each decoded string, keys
// included, so a control written as an escape (\u001b, \b, \f, \r) goes as well as a raw one. A string
// literal it changes is written anew by encoding/json; everything else, and a text without controls, is
// left byte for byte. Raw control characters outside string literals are removed too (newline and tab, JSON
// whitespace, stay), and bytes that are not UTF-8 become U+FFFD. It never repairs or refuses anything: text
// that is not JSON stays text that the caller's parser refuses, and nothing it returns decodes to a string
// with a control.
func StripControlsJSON(raw []byte) []byte {
	if !mayHoldControl(raw) {
		return raw
	}
	s := StripControls(string(raw))
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '"' {
			j := strings.IndexByte(s[i:], '"')
			if j < 0 {
				j = len(s) - i
			}
			out.WriteString(s[i : i+j])
			i += j
			continue
		}
		end := literalEnd(s, i)
		out.WriteString(cleanLiteral(s[i:end]))
		i = end
	}
	return []byte(out.String())
}

// mayHoldControl reports whether raw may hold a control character: raw text that is not valid UTF-8 or holds
// a removed code point, or a backslash, which may start an escape of one.
func mayHoldControl(raw []byte) bool {
	return bytes.IndexByte(raw, '\\') >= 0 || !cleanText(string(raw))
}

// literalEnd returns the index after the string literal that starts with the quote at s[start], or len(s)
// when it is not closed.
func literalEnd(s string, start int) int {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(s)
}

// cleanLiteral returns the JSON string literal lit with the controls of its decoded value removed. A literal
// that holds none, or that is not valid JSON (the document's parser refuses it), comes back unchanged.
func cleanLiteral(lit string) string {
	if !strings.Contains(lit, `\`) {
		// Raw controls are gone already, and without an escape the literal holds no other.
		return lit
	}
	var v string
	if json.Unmarshal([]byte(lit), &v) != nil {
		return lit
	}
	c := StripControls(v)
	if c == v {
		return lit
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(c) != nil {
		// A string always encodes; were it not to, an empty string is safe and still valid JSON.
		return `""`
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// WithoutControls returns c with [StripControls] applied to every model-derived string the analysis loop reads:
// the content (as JSON, [StripControlsJSON]: the loop decodes it as the structured result), and each tool
// call's ID, name and arguments (as JSON). It is applied once, where a reply of any provider enters the loop,
// before anything parses, redacts, stores, logs, records or compares the text (issue #852). Model is left as
// it is: every adapter accepts only a name of a fixed ASCII pattern, and removing characters could turn a
// refused name into the configured one. ProviderBlocks is opaque and goes back to its adapter unchanged. c is
// not changed in place; a reply without controls comes back with its slices shared.
func WithoutControls(c Completion) Completion {
	c.Content = string(StripControlsJSON([]byte(c.Content)))
	var calls []ToolCall
	for i, tc := range c.ToolCalls {
		n := ToolCall{ID: StripControls(tc.ID), Name: StripControls(tc.Name),
			Arguments: StripControlsJSON(tc.Arguments)}
		if n.ID == tc.ID && n.Name == tc.Name && bytes.Equal(n.Arguments, tc.Arguments) {
			continue
		}
		if calls == nil {
			calls = slices.Clone(c.ToolCalls)
		}
		calls[i] = n
	}
	if calls != nil {
		c.ToolCalls = calls
	}
	return c
}
