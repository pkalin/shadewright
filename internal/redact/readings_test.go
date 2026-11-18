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

package redact_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact"
)

// No reading of an answer takes another reading's output as its only input (#845, ninth decision of 2026-11-18).
// A map key hidden as a piece (a connection string, a line break, then "-authorization") lost the word that hides
// the value after it before the whole answer was read; String over the answer as written hides that value, and
// Pieces hides at least what String hides of the answer (property 2).
func TestPiecesKeepTheSignalOfAHiddenKey(t *testing.T) {
	value := "Qz9Secret" + "Value"
	key := "postgres" + "://u:p@h/db" + "\n-authorization"
	ps := []redact.Piece{
		{Text: `{"`},
		{Text: key, Data: true, Form: redact.JSON, MapKey: true},
		{Text: signalQuotedColon},
		{Text: value, Data: true, Form: redact.JSON, Key: key},
		{Text: `"}`},
	}
	var raw strings.Builder
	for _, p := range ps {
		raw.WriteString(redact.Written(p.Form, p.Text))
	}
	if strings.Contains(redact.String(raw.String()), value) {
		t.Fatalf("String shows the value of the answer as written: %s", redact.String(raw.String()))
	}
	got := strings.Join(redact.Pieces(ps), "")
	if strings.Contains(got, value) {
		t.Errorf("Pieces shows the value that String hides of the answer: %s", got)
	}
	if again := redact.String(got); again != got {
		t.Errorf("String hides more of the decided answer:\n%s\n%s", got, again)
	}
	if !strings.HasPrefix(got, `{"`) || !strings.HasSuffix(got, `"}`) || !strings.Contains(got, signalQuotedColon) {
		t.Errorf("the text the tool writes changed: %s", got)
	}
}

// A key or a value hidden by one rule that carries the signal of another rule (a key word, a flag, a header name)
// before, inside and after a line break, as a map key, a value, a list item and free text (#845, ninth decision of
// 2026-11-18). The rule that hides the carrier must not take away the signal that hides what follows it: Pieces
// hides at least what String hides of the answer as the tool writes it, and of the answer with each value
// redacted alone first (what main's second pass read). Strings hides at least what String hides of the items
// joined. A plain word in a neighbouring field stays wherever String over the answer keeps it.

// signalOther is the field name of the neighbouring word.
const signalOther = "other"

// signalNeighbour is a plain word in the field after the generated one.
const signalNeighbour = "plainneighbourword"

// mainMarked is the answer of ps as main's second pass read it: each value that is no map key redacted alone as
// it was read, then written in its form.
func mainMarked(ps []redact.Piece) string {
	var b strings.Builder
	for _, p := range ps {
		switch {
		case p.Data && !p.MapKey:
			b.WriteString(redact.Written(p.Form, redact.String(p.Text)))
		default:
			b.WriteString(redact.Written(p.Form, p.Text))
		}
	}
	return b.String()
}

// writtenAnswer is the answer of ps as the tool writes it, nothing redacted.
func writtenAnswer(ps []redact.Piece) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(redact.Written(p.Form, p.Text))
	}
	return b.String()
}

// signalPlaces lay a generated form with its secret out as the pieces of an answer: a map key with the secret as
// its value, a value of a field, two items of a list, and free text on a line.
var signalPlaces = []struct {
	name string
	ps   func(f signalForm, secret string) []redact.Piece
}{
	{"map key", func(f signalForm, secret string) []redact.Piece {
		key := f.prefix
		return []redact.Piece{
			{Text: `{"`}, {Text: key, Data: true, Form: redact.JSON, MapKey: true}, {Text: signalQuotedColon},
			{Text: secret, Data: true, Form: redact.JSON, Key: key},
			{Text: `", "other": "`}, {Text: signalNeighbour, Data: true, Form: redact.JSON, Key: signalOther}, {Text: `"}`},
		}
	}},
	{"value", func(f signalForm, secret string) []redact.Piece {
		return []redact.Piece{
			{Text: `{"message": "`}, {Text: f.prefix + f.sep + secret, Data: true, Form: redact.JSON, Key: "message"},
			{Text: `", "other": "`}, {Text: signalNeighbour, Data: true, Form: redact.JSON, Key: signalOther}, {Text: `"}`},
		}
	}},
	{"list items", func(f signalForm, secret string) []redact.Piece {
		return []redact.Piece{
			{Text: `{"args": ["`}, {Text: f.prefix + strings.TrimRight(f.sep, " \r\n"), Data: true, Form: redact.JSON, Key: "args"},
			{Text: `", "`}, {Text: secret, Data: true, Form: redact.JSON, Key: "args"},
			{Text: `"], "other": "`}, {Text: signalNeighbour, Data: true, Form: redact.JSON, Key: signalOther}, {Text: `"}`},
		}
	}},
	{"free text", func(f signalForm, secret string) []redact.Piece {
		return []redact.Piece{
			{Text: "Warning Failed Pod/web: "}, {Text: f.prefix + f.sep + secret, Data: true, Form: redact.Line},
			{Text: "\nNormal Pulled Pod/web: "}, {Text: signalNeighbour, Data: true, Form: redact.Line}, {Text: "\n"},
		}
	}},
}

func TestReadingsKeepTheSignalOfAHiddenText(t *testing.T) {
	forms := signalForms()
	calls, failed, lostNeighbour, n := 0, 0, 0, 0
	byPlace := map[string]int{}
	for i, f := range forms {
		for _, pl := range signalPlaces {
			if n++; !redact.RaceKeeps("readings", n) {
				continue
			}
			calls++
			secret := fmt.Sprintf("Qz9%05dSecretValue", i)
			ps := pl.ps(f, secret)
			got := strings.Join(redact.Pieces(ps), "")
			raw, marked := writtenAnswer(ps), mainMarked(ps)
			want := !strings.Contains(redact.String(raw), secret) || !strings.Contains(redact.String(marked), secret)
			if want && strings.Contains(got, secret) {
				byPlace[pl.name]++
				if failed++; failed <= 10 {
					t.Errorf("%s, %s: the secret shows, String hides it\n  answer:  %q\n  decided: %q", pl.name, f.name, raw, got)
				}
			}
			if strings.Contains(redact.String(raw), signalNeighbour) && !strings.Contains(got, signalNeighbour) {
				if lostNeighbour++; lostNeighbour <= 10 {
					t.Errorf("%s, %s: the neighbouring word is hidden\n  decided: %q", pl.name, f.name, got)
				}
			}
		}
	}
	// Strings hides in the items apart at least what String hides of them joined (property 2), for an item that
	// carries a hidden text and a signal and the secret in the next item.
	for i, f := range forms {
		if !redact.RaceKeeps("readings strings", i) {
			continue
		}
		calls++
		secret := fmt.Sprintf("Qz9%05dSecretValue", i)
		items := []string{f.prefix + strings.TrimRight(f.sep, " \r\n"), secret}
		if !strings.Contains(redact.String(strings.Join(items, "\n")), secret) && strings.Contains(strings.Join(redact.Strings(items), "\n"), secret) {
			byPlace["Strings"]++
			if failed++; failed <= 10 {
				t.Errorf("Strings, %s: the secret shows, String hides it joined: %q", f.name, redact.Strings(items))
			}
		}
	}
	if failed > 0 || lostNeighbour > 0 {
		t.Errorf("%d of %d answers show a secret String hides (by place %v), %d lose the neighbouring word", failed, calls, byPlace, lostNeighbour)
	}
	t.Logf("%d answers of %d forms", calls, len(forms))
}
