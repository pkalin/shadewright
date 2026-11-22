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
	"errors"
	"io"
	"slices"
	"strconv"
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
// included, so a control goes in every form JSON writes one: raw (DEL and C1, which JSON allows inside a
// string) or as an escape (\u001b, \b, \f, \r). Only decoded strings are cleaned, never the JSON text: a string
// literal whose value changes is written anew by encoding/json, and every other byte is left as it is. So a
// raw control between two escapes never joins them (the halves of a surrogate pair with a C1 between them
// stay two U+FFFD), and text JSON refuses (a raw C0 control in a string, a control between tokens) stays text
// the caller's parser refuses; nothing is repaired into another value. If raw parses, the output parses to the
// same tokens with each string cleaned; a text whose strings hold no control comes back byte for byte.
func StripControlsJSON(raw []byte) []byte {
	out, _ := cleanJSON(raw)
	return out
}

// cleanJSON is [StripControlsJSON], and reports whether a decoded string of raw held a removed control.
func cleanJSON(raw []byte) ([]byte, bool) {
	if !mayHoldControl(raw) {
		return raw, false
	}
	s := string(raw)
	var out strings.Builder
	held := false
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
		lit, changed := cleanLiteral(s[i:end])
		held = held || changed
		out.WriteString(lit)
		i = end
	}
	if !held {
		return raw, false
	}
	return []byte(out.String()), true
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

// cleanLiteral returns the JSON string literal lit with the controls of its decoded value removed, and
// whether it held one. A literal that holds none, or that is not valid JSON (a raw C0 control in it: the
// document's parser refuses it), comes back unchanged.
func cleanLiteral(lit string) (string, bool) {
	if !strings.Contains(lit, `\`) && cleanText(lit) {
		return lit, false
	}
	var v string
	if json.Unmarshal([]byte(lit), &v) != nil {
		return lit, false
	}
	c := StripControls(v)
	if c == v {
		return lit, false
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(c) != nil {
		// A string always encodes; were it not to, an empty string is safe and still valid JSON.
		return `""`, true
	}
	return strings.TrimSuffix(b.String(), "\n"), true
}

// controlPaths returns where the decoded strings of the JSON text raw hold a removed control, in document
// order. A document that does not parse ends the list with an entry of empty path at the point it stops, so a
// control past that point is never taken for one in a place the caller accepts.
func controlPaths(raw []byte) []ControlPath {
	type frame struct {
		object, wantKey bool
		key             string
		index           int
	}
	var stack []frame
	var out []ControlPath
	path := func() []string {
		p := make([]string, 0, len(stack))
		for _, f := range stack {
			if f.object {
				p = append(p, f.key)
			} else {
				p = append(p, strconv.Itoa(f.index))
			}
		}
		return p
	}
	// valueDone moves the innermost container past the value that just ended.
	valueDone := func() {
		if len(stack) == 0 {
			return
		}
		top := &stack[len(stack)-1]
		if top.object {
			top.wantKey = true
		} else {
			top.index++
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) && len(stack) == 0 {
			return out
		}
		if err != nil {
			return append(out, ControlPath{})
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				stack = append(stack, frame{object: d == '{', wantKey: d == '{'})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		}
		s, isString := tok.(string)
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].wantKey {
			stack[n-1].key, stack[n-1].wantKey = StripControls(s), false
			if strings.ContainsFunc(s, removedControl) {
				out = append(out, ControlPath{Path: path(), Key: true})
			}
			continue
		}
		if isString && strings.ContainsFunc(s, removedControl) {
			out = append(out, ControlPath{Path: path()})
		}
		valueDone()
	}
}

// WithoutControls returns c with [StripControls] applied to every model-derived string the analysis loop reads:
// the content (as JSON, [StripControlsJSON]: the loop decodes it as the structured result), and each tool
// call's ID, name and arguments (as JSON). It is applied once, where a reply of any provider enters the loop,
// before anything parses, logs, records or compares the text (issue #852); what is stored of the content is
// redacted from [Completion.ContentAsRead] (below). A tool call that
// held a control is cleaned too, so what is stored, logged or recorded about it holds none, and is marked
// [ToolCall.HeldControls]: the loop refuses it instead of running a value the model did not write. Model is
// left as it is: every adapter accepts only a name of a fixed ASCII pattern, and removing characters could
// turn a refused name into the configured one. ProviderBlocks is opaque and goes back to its adapter unchanged.
// Where the content held a control is listed in [Completion.ContentControls]: the content's schema is the
// caller's, which refuses a control in a field that is not free text instead of reading the cleaned value. The
// content as it came is kept in [Completion.ContentAsRead]: removing a control can join or split what a
// redaction rule reads ("x" CR "Bearer" plus a token loses the word boundary before "Bearer"), so text that is
// stored is redacted as it was read and cleaned afterwards, never redacted after this cleaning.
// c is not changed in place; a reply without controls comes back with its slices shared.
func WithoutControls(c Completion) Completion {
	if content, held := cleanJSON([]byte(c.Content)); held {
		paths := controlPaths([]byte(c.Content))
		if len(paths) == 0 {
			// cleanJSON found a control the walk did not place: fail closed at the root.
			paths = []ControlPath{{}}
		}
		c.ContentAsRead = c.Content
		c.Content, c.ContentControls = string(content), append(slices.Clip(c.ContentControls), paths...)
	}
	var calls []ToolCall
	for i, tc := range c.ToolCalls {
		args, argsHeld := cleanJSON(tc.Arguments)
		held := argsHeld || strings.ContainsFunc(tc.ID, removedControl) || strings.ContainsFunc(tc.Name, removedControl)
		n := ToolCall{ID: StripControls(tc.ID), Name: StripControls(tc.Name), Arguments: args,
			HeldControls: tc.HeldControls || held}
		if n.ID == tc.ID && n.Name == tc.Name && bytes.Equal(n.Arguments, tc.Arguments) && n.HeldControls == tc.HeldControls {
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
