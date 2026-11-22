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

package analysis

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/redact"
)

// The model text corpus of #852 (decision of 2026-11-22: no input shows a secret origin/main hides): a secret
// form in a free-text field of a result, with one control character at each place between the form's parts. The
// answer is known by construction (where the secret is and what it is), and origin/main's own answer is a table:
// for each case, whether origin/main's redaction of the text as the model wrote it (run.redact, StringKeeping)
// hid the secret. This file compiles on origin/main as it is; the table was written there with
// MODELTEXT_MAIN_TABLE set to the output path. TestModelTextHidesWhatMainHid reads it.

// modelTextMainTable is the table of origin/main's answers on the model text corpus.
const modelTextMainTable = "testdata/model-text-main.tsv"

// modelTextForm is a secret form: its parts, joined as they are, and the index of the part that is the secret.
type modelTextForm struct {
	parts  []string
	secret int
}

// modelTextForms are the secret forms of the corpus. The values are made up; the token is built from parts.
func modelTextForms() []modelTextForm {
	return []modelTextForm{
		{parts: []string{"Bearer", " ", "Qz7secretValue1"}, secret: 2},
		{parts: []string{"password", "=", "hunter2Xyz9Qq"}, secret: 2},
		{parts: []string{`"password"`, ":", `"`, "Zq7Kp2Vx9Lm4Wt8R", `"`}, secret: 3},
		{parts: []string{"ghp" + "_", strings.Repeat("A1b2C3", 6)}, secret: 1},
		{parts: []string{"postgres://", "u", ":", "p4ssw0rdQ7", "@", "db.example", "/app"}, secret: 3},
		{parts: []string{"--token", " ", "Zx81Lm4Wt8Rq2"}, secret: 2},
		{parts: []string{"Authorization", ":", " ", "Basic", " ", "dXNlcjpwYXNzd29yZA=="}, secret: 5},
		{parts: []string{"api_key", ":", " ", "Wq5Rt7Yu9Io2Pa"}, secret: 3},
	}
}

// modelTextPrefixes are the texts before the form: one glued to it, one with a blank.
var modelTextPrefixes = []string{"x", "see "}

// modelTextControls returns every code point model text is stored without, in order: Unicode category Cc but
// newline and tab (30 of C0, DEL, and the 32 of C1, U+0085 among them).
func modelTextControls() []rune {
	var out []rune
	for r := rune(0); r <= 0x9f; r++ {
		if (r < 0x20 && r != '\n' && r != '\t') || r >= 0x7f {
			out = append(out, r)
		}
	}
	return out
}

// modelTextCase is one input: the text, the secret in it, and where it came from.
type modelTextCase struct {
	prefix, form, pos int
	ctl               rune
	text, secret      string
}

// modelTextCases returns the corpus: for each prefix, form and place between the form's parts (before the first
// and after the last included), the text with each control at that place.
func modelTextCases() []modelTextCase {
	var out []modelTextCase
	for pi, prefix := range modelTextPrefixes {
		for fi, f := range modelTextForms() {
			for pos := 0; pos <= len(f.parts); pos++ {
				for _, c := range modelTextControls() {
					parts := append(append(append([]string{}, f.parts[:pos]...), string(c)), f.parts[pos:]...)
					out = append(out, modelTextCase{prefix: pi, form: fi, pos: pos, ctl: c,
						text: prefix + strings.Join(parts, "") + " ok", secret: f.parts[f.secret]})
				}
			}
		}
	}
	return out
}

// modelTextShown reports whether text shows secret to a reader that ignores control characters: secret is in
// text once every control code point is removed.
func modelTextShown(text, secret string) bool {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, n := utf8.DecodeRuneInString(text[i:])
		if !((r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f)) || r == utf8.RuneError {
			b.WriteString(text[i : i+n])
		}
		i += n
	}
	return strings.Contains(b.String(), secret)
}

// modelTextKey is the row of the table a case is in.
func modelTextKey(c modelTextCase) string { return fmt.Sprintf("%d\t%d\t%d", c.prefix, c.form, c.pos) }

// TestWriteModelTextMainTable writes the table of the corpus with this tree's redaction of model text (on
// origin/main: run.redact, StringKeeping of the text as the model wrote it) when MODELTEXT_MAIN_TABLE names the
// output: one row per prefix, form and place, with one character per control, "h" when the secret was hidden and
// "s" when it was shown.
func TestWriteModelTextMainTable(t *testing.T) {
	path := os.Getenv("MODELTEXT_MAIN_TABLE")
	if path == "" {
		t.Skip("MODELTEXT_MAIN_TABLE is not set")
	}
	rows := map[string]*strings.Builder{}
	var order []string
	for _, c := range modelTextCases() {
		k := modelTextKey(c)
		if rows[k] == nil {
			rows[k] = &strings.Builder{}
			order = append(order, k)
		}
		if modelTextShown(redact.StringKeeping(c.text, incident().Name), c.secret) {
			rows[k].WriteByte('s')
		} else {
			rows[k].WriteByte('h')
		}
	}
	var b strings.Builder
	b.WriteString("# origin/main's redaction of model text on the corpus of controls_main_corpus_test.go (#852): prefix, form,\n")
	b.WriteString("# place, then per control in order h (hidden) or s (shown). Written with MODELTEXT_MAIN_TABLE.\n")
	for _, k := range order {
		b.WriteString(k + "\t" + rows[k].String() + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}
