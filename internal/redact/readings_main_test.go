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
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/pkalin/shadewright/internal/redact"
)

// What main did with the answers of TestReadingsKeepTheSignalOfAHiddenText (#845, eleventh decision of
// 2026-11-18). A signal and a secret on two lines of one value are decided as main decides them; the table holds
// main's decision for every answer of the corpus. This file and signal_forms_test.go depend on nothing else in
// the package's tests and use only redact.String, so that they compile in a checkout of main, where
// TestWriteReadingsMainTable writes the table.

// readingsMainTable holds, for each answer of the corpus, whether main hid its secret: "row<TAB>hidden or
// shown", the row the first 16 hex digits of the SHA-256 of "place<TAB>form".
const readingsMainTable = "testdata/readings-main.tsv"

// Forms of a piece of an answer, as the layout of the redaction package names them.
const (
	readingsAsIs = iota
	readingsLine
	readingsJSON
)

// readingsPiece is a piece of a generated answer: text the tool writes as it is, or a value (data) the tool
// writes in a form, with the key it stands under and whether it is a map key.
type readingsPiece struct {
	text         string
	data, mapKey bool
	form         int
	key          string
}

// readingsPlace lays a generated form with its secret out as the pieces of an answer. oneValue is set when the
// form and the secret are one value of the answer.
type readingsPlace struct {
	name     string
	oneValue bool
	ps       func(f signalForm, secret string) []readingsPiece
}

// readingsOther is the field name of the neighbouring word.
const readingsOther = "other"

// readingsNeighbour is a plain word in the field after the generated one.
const readingsNeighbour = "plainneighbourword"

// The fields of the event list_events shows with each generated note: its time, type, reason and object.
const (
	readingsEventTime = "2026-11-18T10:00:00Z"
	readingsEventType = "Warning"
	readingsReason    = "Failed"
	readingsKind      = "Pod"
	readingsPod       = "web-abc-1"
	// readingsTemplate is the template of query_metrics.
	readingsTemplate = "container_restarts_total"
	// readingsArgs is the field of the generated list, and readingsEnd the end of the JSON answers.
	readingsArgs = "args"
	readingsEnd  = "\"\n}\n"
)

// readingsEvent returns the pieces of one line of list_events: an event at time at with the note.
func readingsEvent(at, note string) []readingsPiece {
	return []readingsPiece{
		{text: at + " "}, {text: readingsEventType, data: true, form: readingsLine, key: "type"},
		{text: " "}, {text: readingsReason, data: true, form: readingsLine, key: "reason"},
		{text: " "}, {text: readingsKind, data: true, form: readingsLine, key: argKind},
		{text: "/"}, {text: readingsPod, data: true, form: readingsLine, key: argName},
		{text: " count 1: "}, {text: note, data: true, form: readingsLine, key: "note"}, {text: "\n"},
	}
}

// readingsPlaces lay the generated forms out as the read tools write their answers (#845, twelfth decision of
// 2026-11-18: the comparison with main is made on the text the tools return, in the encoding they return it
// in). The first three are the JSON of get_resource, encoded without HTML escapes and with an indent of one
// space, each map's keys sorted: a map key with the secret as its value, a value of a field, and two items of a
// list, one item per line. Then the answer of query_metrics, which writes the labels of a series on one line
// ("name=value, ..."), a line break of a value as a space: the form and the secret in two labels, the
// neighbouring word in a third. The last is the answer of list_events: the form and the secret in the note of
// one event, the neighbouring word in the note of the next. TestReadingsPlacesAreTheToolsAnswers checks each against
// the encoder and the tool.
var readingsPlaces = []readingsPlace{
	{"map key", false, func(f signalForm, secret string) []readingsPiece {
		key := f.prefix
		entry := []readingsPiece{{text: key, data: true, form: readingsJSON, mapKey: true}, {text: signalQuotedColon},
			{text: secret, data: true, form: readingsJSON, key: key}}
		other := []readingsPiece{{text: readingsOther + signalQuotedColon}, {text: readingsNeighbour, data: true, form: readingsJSON, key: readingsOther}}
		first, second := entry, other
		if key > readingsOther {
			first, second = other, entry
		}
		out := append([]readingsPiece{{text: "{\n \""}}, first...)
		out = append(out, readingsPiece{text: "\",\n \""})
		out = append(out, second...)
		return append(out, readingsPiece{text: readingsEnd})
	}},
	{"value", true, func(f signalForm, secret string) []readingsPiece {
		return []readingsPiece{
			{text: "{\n \"message\": \""}, {text: f.prefix + f.sep + secret, data: true, form: readingsJSON, key: "message"},
			{text: "\",\n \"other\": \""}, {text: readingsNeighbour, data: true, form: readingsJSON, key: readingsOther}, {text: readingsEnd},
		}
	}},
	{"list items", false, func(f signalForm, secret string) []readingsPiece {
		return []readingsPiece{
			{text: "{\n \"" + readingsArgs + "\": [\n  \""}, {text: readingsListItems(f, secret)[0], data: true, form: readingsJSON, key: readingsArgs},
			{text: "\",\n  \""}, {text: secret, data: true, form: readingsJSON, key: readingsArgs},
			{text: "\"\n ],\n \"other\": \""}, {text: readingsNeighbour, data: true, form: readingsJSON, key: readingsOther}, {text: readingsEnd},
		}
	}},
	{"labels of a series", false, func(f signalForm, secret string) []readingsPiece {
		items := readingsListItems(f, secret)
		label := func(name, v string) []readingsPiece {
			return []readingsPiece{{text: name, data: true, form: readingsLine}, {text: "="}, {text: v, data: true, form: readingsLine, key: name}}
		}
		out := []readingsPiece{{text: readingsTemplate + ": 1 series\n{"}}
		out = append(out, label("arg0", items[0])...)
		out = append(out, readingsPiece{text: ", "})
		out = append(out, label("arg1", items[1])...)
		out = append(out, readingsPiece{text: ", "})
		out = append(out, label(readingsOther, readingsNeighbour)...)
		return append(out, readingsPiece{text: "} 1\n"})
	}},
	{"free text", true, func(f signalForm, secret string) []readingsPiece {
		out := []readingsPiece{{text: "2 events match; the newest 2 are shown\n"}}
		out = append(out, readingsEvent(readingsEventTime, f.prefix+f.sep+secret)...)
		return append(out, readingsEvent(readingsEventTime, readingsNeighbour)...)
	}},
}

// readingsListItems are the two items of a list for a form: the form with its separator, and the secret.
func readingsListItems(f signalForm, secret string) []string {
	return []string{f.prefix + strings.TrimRight(f.sep, " \r\n"), secret}
}

// readingsSecret is the secret of the form with index i.
func readingsSecret(i int) string { return fmt.Sprintf("Qz9%05dSecretValue", i) }

// readingsRow is the row of the table for a place and a form.
func readingsRow(place, form string) string {
	sum := sha256.Sum256([]byte(place + "\t" + form))
	return hex.EncodeToString(sum[:])[:16]
}

// readingsWrite writes a value in its form as the tools write it: a line with each control character a space,
// or the inside of a JSON string. TestReadingsWriteAsTheLayout checks it against redact.Written.
func readingsWrite(form int, s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case form == readingsLine && (unicode.IsControl(c) || c == ' ' || c == ' '):
			b.WriteByte(' ')
		case form != readingsJSON:
			b.WriteRune(c)
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		case c == ' ' || c == ' ':
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// readingsMainHides reports whether main hid secret in the answer of ps: its first pass redacted each value that
// is no map key alone, and its second pass read the answer written with those values in place. Each pass is
// incidentcontext.Redact of main: String, then String again of the text made valid UTF-8.
func readingsMainHides(ps []readingsPiece, secret string) bool {
	pass := func(s string) string { return redact.String(strings.ToValidUTF8(redact.String(s), "?")) }
	var b strings.Builder
	for _, p := range ps {
		switch {
		case p.data && !p.mapKey:
			v := pass(p.text)
			if strings.Contains(p.text, secret) && !strings.Contains(v, secret) {
				return true
			}
			b.WriteString(readingsWrite(p.form, v))
		case p.data:
			b.WriteString(readingsWrite(p.form, p.text))
		default:
			b.WriteString(p.text)
		}
	}
	return !strings.Contains(pass(b.String()), secret)
}

// The decisions of a row of the tables written from main.
const (
	readingsHidden = "hidden"
	readingsShown  = "shown"
)

// readReadingsMainTable reads a table written from main (readingsMainTable, readingsToolsTable): main's decision
// by row.
func readReadingsMainTable(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		row, v, ok := strings.Cut(line, "\t")
		if !ok || (v != readingsHidden && v != readingsShown) {
			t.Fatalf("%s: malformed line %q", path, line)
		}
		out[row] = v == readingsHidden
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestWriteReadingsMainTable writes readingsMainTable from the redactor it runs with, when READINGS_MAIN_TABLE
// names the output path (and READINGS_MAIN_COMMIT the commit); it is run once in a checkout of main with this
// file and signal_forms_test.go.
func TestWriteReadingsMainTable(t *testing.T) {
	path := os.Getenv("READINGS_MAIN_TABLE")
	if path == "" {
		t.Skip("READINGS_MAIN_TABLE is not set")
	}
	var b strings.Builder
	b.WriteString("# Whether main hid the secret of each answer of TestReadingsKeepTheSignalOfAHiddenText, at main " +
		os.Getenv("READINGS_MAIN_COMMIT") + " (#845).\n# row (the first 16 hex digits of the SHA-256 of \"place<TAB>form\")\thidden or shown\n")
	for i, f := range signalForms() {
		for _, pl := range readingsPlaces {
			v := readingsShown
			if readingsMainHides(pl.ps(f, readingsSecret(i)), readingsSecret(i)) {
				v = readingsHidden
			}
			b.WriteString(readingsRow(pl.name, f.name) + "\t" + v + "\n")
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}
