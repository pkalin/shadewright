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
	"encoding/json"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
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
//
// A line break inside a value ends a line for every reading (tenth decision), so a signal and a secret on two
// lines of one value are decided as main decided them (eleventh decision, testdata/readings-main.tsv): what main
// hid is hidden, what it showed may show. So are three rows of one value that main and the value read alone show
// (twelfth decision). In every row, nothing main hid shows. Every answer is one a read tool writes, in its
// encoding (readingsPlaces, TestReadingsPlacesAreTheToolsAnswers).

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

// readingsValueAloneHides reports whether String hides secret in the value of ps that holds it, read alone.
func readingsValueAloneHides(ps []redact.Piece, secret string) bool {
	for _, p := range ps {
		if p.Data && strings.Contains(p.Text, secret) {
			return !strings.Contains(redact.String(p.Text), secret)
		}
	}
	return false
}

// readingsForms maps the forms of readings_main_test.go to those of the layout.
var readingsForms = map[int]redact.Form{readingsAsIs: redact.AsIs, readingsLine: redact.Line, readingsJSON: redact.JSON}

// layoutPieces returns the pieces of the layout for the generated pieces ps.
func layoutPieces(ps []readingsPiece) []redact.Piece {
	out := make([]redact.Piece, len(ps))
	for i, p := range ps {
		out[i] = redact.Piece{Text: p.text, Data: p.data, Form: readingsForms[p.form], MapKey: p.mapKey, Key: p.key}
	}
	return out
}

// The writer of readings_main_test.go, which main's table was written with, writes each generated value as the
// layout does.
func TestReadingsWriteAsTheLayout(t *testing.T) {
	for i, f := range signalForms() {
		for _, pl := range readingsPlaces {
			for _, p := range pl.ps(f, readingsSecret(i)) {
				if !p.data {
					continue
				}
				if got, want := readingsWrite(p.form, p.text), redact.Written(readingsForms[p.form], p.text); got != want {
					t.Fatalf("%s, %s: written %q, the layout writes %q", pl.name, f.name, got, want)
				}
			}
		}
	}
}

// readingsCheckAsMainRows checks, for a run without sampling, the rows decided as main: asMain are exactly the
// split forms in the places of one value, and oneLine the three with "_authorization=" after a connection string
// and a line break, on the line of the secret (twelfth decision of 2026-11-18, #868): String over the answer hides
// them, main and the value alone show them.
func readingsCheckAsMainRows(t *testing.T, forms []signalForm, asMain, oneLine int) {
	t.Helper()
	want := 0
	for _, f := range forms {
		for _, pl := range readingsPlaces {
			if f.split && pl.oneValue {
				want++
			}
		}
	}
	if asMain != want || want == 0 {
		t.Errorf("%d rows decided as main, %d split forms in a value", asMain, want)
	}
	if oneLine != 3 {
		t.Errorf("%d rows of one value that main and the value alone show, want 3", oneLine)
	}
}

// readingsStringsKeepTheSignal checks that Strings hides in the items apart at least what String hides of them
// joined (property 2), for an item that carries a hidden text and a signal and the secret in the next item. It
// returns the number of calls and of failures; reported is the number of failures reported before.
func readingsStringsKeepTheSignal(t *testing.T, forms []signalForm, reported int) (calls, failed int) {
	t.Helper()
	for i, f := range forms {
		if !redact.RaceKeeps("readings strings", i) {
			continue
		}
		calls++
		secret := readingsSecret(i)
		items := readingsListItems(f, secret)
		if !strings.Contains(redact.String(strings.Join(items, "\n")), secret) && strings.Contains(strings.Join(redact.Strings(items), "\n"), secret) {
			if failed++; reported+failed <= 10 {
				t.Errorf("Strings, %s: the secret shows, String hides it joined: %q", f.name, redact.Strings(items))
			}
		}
	}
	return calls, failed
}

// readingsEncode is the answer of get_resource for v without the layout: encoding/json without HTML escapes and
// with an indent of one space, as the tool encodes the object.
func readingsEncode(t *testing.T, v any) string {
	t.Helper()
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Every answer of the readings corpus is one a read tool returns (#845, twelfth decision of 2026-11-18): the JSON
// places are encoding/json's output for the object they stand for, and the free text is the answer of
// list_events for two events with those notes (with a neutral text, which no rule hides, in place of the form).
// Before, the list items were one line of JSON, which no tool writes.
func TestReadingsPlacesAreTheToolsAnswers(t *testing.T) {
	answer := func(ps []readingsPiece) string {
		var b strings.Builder
		for _, p := range ps {
			if p.data {
				b.WriteString(readingsWrite(p.form, p.text))
			} else {
				b.WriteString(p.text)
			}
		}
		return b.String()
	}
	byName := map[string]readingsPlace{}
	for _, pl := range readingsPlaces {
		byName[pl.name] = pl
	}
	for i, f := range signalForms() {
		secret := readingsSecret(i)
		items := readingsListItems(f, secret)
		for name, v := range map[string]any{
			"map key":    map[string]string{f.prefix: secret, readingsOther: readingsNeighbour},
			"value":      map[string]string{"message": f.prefix + f.sep + secret, readingsOther: readingsNeighbour},
			"list items": map[string]any{readingsArgs: items, readingsOther: readingsNeighbour},
		} {
			if got, want := answer(byName[name].ps(f, secret)), readingsEncode(t, v); got != want {
				t.Fatalf("%s, %s: the generated answer\n%q\nencoding/json writes\n%q", name, f.name, got, want)
			}
		}
	}
	// list_events writes the free text as generated, for a neutral form with a line break that no rule hides.
	neutral, word := signalForm{name: "neutral", prefix: "alpha\r\nbeta", sep: " "}, "gamma"
	want := answer(byName["free text"].ps(neutral, word))
	ts := newReadingsToolSet(readingsRedactor())
	got := ts.call(tools.NameListEvents, map[string]any{argNamespace: readingsNS, argKind: readingsKind, argName: readingsPod, readingsArgLimit: 2},
		[]string{neutral.prefix + neutral.sep + word, readingsNeighbour}, nil)
	if got != want {
		t.Errorf("list_events answers\n%q\nthe generated free text is\n%q", got, want)
	}
	// query_metrics writes the labels as generated.
	items := readingsListItems(neutral, word)
	want = answer(byName["labels of a series"].ps(neutral, word))
	got = ts.call(tools.NameQueryMetrics, map[string]any{argNamespace: readingsNS, readingsArgPod: readingsPod, readingsArgTempl: readingsTemplate},
		nil, []providers.Series{readingsSeries(map[string]string{"arg0": items[0], "arg1": items[1], readingsOther: readingsNeighbour})})
	if got != want {
		t.Errorf("query_metrics answers\n%q\nthe generated labels are\n%q", got, want)
	}
}

func TestReadingsKeepTheSignalOfAHiddenText(t *testing.T) {
	forms := signalForms()
	mainHid := readReadingsMainTable(t, readingsMainTable)
	if len(mainHid) != len(forms)*len(readingsPlaces) {
		t.Fatalf("%s has %d rows, the corpus %d answers: write it again in a checkout of main", readingsMainTable, len(mainHid), len(forms)*len(readingsPlaces))
	}
	calls, failed, leaks, lostNeighbour, n := 0, 0, 0, 0, 0
	// The rows decided as main: a signal and a secret on two lines of one value (how many, how many main showed,
	// how many of those String over the answer hides, what the ninth decision expected), and the other rows of one
	// value that String over the answer hides and main and the value read alone show (oneLine).
	asMain, asMainShown, asMainShownStringHides, oneLine := 0, 0, 0, 0
	byPlace := map[string]int{}
	for i, f := range forms {
		for _, pl := range readingsPlaces {
			if n++; !redact.RaceKeeps("readings", n) {
				continue
			}
			calls++
			secret := readingsSecret(i)
			ps := layoutPieces(pl.ps(f, secret))
			got := strings.Join(redact.Pieces(ps), "")
			shown := strings.Contains(got, secret)
			raw, marked := writtenAnswer(ps), mainMarked(ps)
			hid, ok := mainHid[readingsRow(pl.name, f.name)]
			if !ok {
				t.Fatalf("%s has no row for %s, %s: write it again in a checkout of main", readingsMainTable, pl.name, f.name)
			}
			if hid && shown {
				if leaks++; leaks <= 10 {
					t.Errorf("%s, %s: the secret shows, main hid it\n  answer:  %q\n  decided: %q", pl.name, f.name, raw, got)
				}
			}
			stringHides := !strings.Contains(redact.String(raw), secret) || !strings.Contains(redact.String(marked), secret)
			switch {
			case pl.oneValue && !f.split && stringHides && !hid && !readingsValueAloneHides(ps, secret):
				oneLine++
			case pl.oneValue && f.split:
				asMain++
				if !hid {
					asMainShown++
					if stringHides {
						asMainShownStringHides++
					}
				}
			case stringHides && shown:
				byPlace[pl.name]++
				if failed++; failed <= 10 {
					t.Errorf("%s, %s: the secret shows, String hides it\n  answer:  %q\n  decided: %q", pl.name, f.name, raw, got)
				}
			}
			if strings.Contains(redact.String(raw), readingsNeighbour) && !strings.Contains(got, readingsNeighbour) {
				if lostNeighbour++; lostNeighbour <= 10 {
					t.Errorf("%s, %s: the neighbouring word is hidden\n  decided: %q", pl.name, f.name, got)
				}
			}
		}
	}
	if calls == len(forms)*len(readingsPlaces) {
		readingsCheckAsMainRows(t, forms, asMain, oneLine)
	}
	sc, sf := readingsStringsKeepTheSignal(t, forms, failed)
	calls, failed = calls+sc, failed+sf
	if sf > 0 {
		byPlace["Strings"] = sf
	}
	if failed > 0 || leaks > 0 || lostNeighbour > 0 {
		t.Errorf("%d of %d answers show a secret String hides (by place %v), %d a secret main hid, %d lose the neighbouring word", failed, calls, byPlace, leaks, lostNeighbour)
	}
	t.Logf("%d answers of %d forms; %d decided as main (a signal and a secret on two lines of one value), main showed %d of them, %d of those String over the answer hides; %d more of one value that String over the answer hides and main and the value alone show",
		calls, len(forms), asMain, asMainShown, asMainShownStringHides, oneLine)
}

// readingsTheTwelve reports whether a form is one of the 12 that main hid only in a list written as one line of
// JSON (#845, twelfth decision of 2026-11-18): a key and value carrier, "authorization" glued with "-", a line
// break before the signal or inside the carrier, and the separator ": " or "=".
func readingsTheTwelve(f signalForm) bool {
	return strings.HasPrefix(f.name, `key and value, key word authorization, glue "-", `) &&
		!strings.Contains(f.name, `separator "\": \""`) &&
		(strings.HasSuffix(f.name, "line break before the signal") || strings.HasSuffix(f.name, "line break inside the carrier"))
}

// Every generated list goes through every read tool that can carry a list (readingsListTools), and each tool
// decides as main's tool decided (testdata/readings-tools-main.tsv, written at main by
// TestWriteReadingsToolsMainTable): no secret main hid shows, and the 12 forms main hid only in a list on one line
// of JSON are decided as main decides them at every tool. None of these tools writes a list on one line with a
// line break of an item kept as a line break, so main shows those 12 everywhere (the twelfth decision).
func TestReadingsListsAtTheToolsAsMain(t *testing.T) {
	main := readReadingsMainTable(t, readingsToolsTable)
	forms := signalForms()
	if len(main) != len(forms)*len(readingsListTools) {
		t.Fatalf("%s has %d rows, the corpus %d: write it again in a checkout of main", readingsToolsTable, len(main), len(forms)*len(readingsListTools))
	}
	twelve := 0
	for _, f := range forms {
		if readingsTheTwelve(f) {
			twelve++
		}
	}
	if twelve != 12 {
		t.Fatalf("%d forms are the twelve", twelve)
	}
	got := readingsToolsHidden(func(tool string, i int) bool { return readingsTheTwelve(forms[i]) || redact.RaceKeeps(tool, i) })
	leaks, differ, moreHidden, mainShownTwelve := 0, 0, 0, 0
	for _, lt := range readingsListTools {
		for _, f := range forms {
			row := readingsRow(lt.name, f.name)
			hid, ran := got[row]
			if !ran {
				continue
			}
			mainHid, ok := main[row]
			if !ok {
				t.Fatalf("%s has no row for %s, %s", readingsToolsTable, lt.name, f.name)
			}
			switch {
			case mainHid && !hid:
				if leaks++; leaks <= 10 {
					t.Errorf("%s, %s: the secret shows, main hid it", lt.name, f.name)
				}
			case !mainHid && hid:
				moreHidden++
			}
			if readingsTheTwelve(f) {
				if !mainHid {
					mainShownTwelve++
				}
				if hid != mainHid {
					if differ++; differ <= 10 {
						t.Errorf("%s, %s: hidden %v, main %v", lt.name, f.name, hid, mainHid)
					}
				}
			}
		}
	}
	if leaks > 0 || differ > 0 {
		t.Errorf("%d secrets main hid show, %d of the twelve forms decided otherwise than main", leaks, differ)
	}
	t.Logf("%d rows over %d tools; the twelve forms: %d of %d rows shown by main; %d rows hidden that main showed",
		len(got), len(readingsListTools), mainShownTwelve, 12*len(readingsListTools), moreHidden)
}
