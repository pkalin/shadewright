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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/tools"
)

// A key found inside a text value hides as far as main hid at that tool, not further (#845, sixth decision of
// 2026-11-18, point 3). list_events shows a note on one line: main redacted the note as it was read, where a
// value ends at the line break (or runs on over indented lines), then the line. get_resource is JSON, where main
// read a line break as "\n": it does not end the value, the next blank does. The branch hid the whole rest of a
// note ("token: expired" hid the lines after it). Each note here is a secret key, a separator, a value, a line
// break and two words; the table holds, written at main (TestWriteNotesMainTable, in a checkout of main with this
// file copied: it uses only the read tools' API), whether main showed the value and each word at each of the two
// tools. A value main hid that shows fails (property 1), and so does a word main showed that is hidden (the
// over-redaction of point 3).

// notesMainTable holds main's answers for the notes of notesCases.
const notesMainTable = "testdata/notes-main-entry.tsv"

// noteCase is one note with the value and the two words after its line break.
type noteCase struct {
	note, value, first, second string
}

// notesCases returns the notes: every secret key, separator, value and line break of the lists below.
func notesCases() []noteCase {
	keys := []string{"token", fieldsKey, "auth", "refresh token", "api_key", "secret", "Authorization", "passwd", "client_secret"}
	seps := []string{": ", "=", " = ", ":\t"}
	values := []string{"expired", "invalid", "hunter2", "V9qz-1234567890", "Bearer qz12345xk9walphabeta"}
	breaks := []string{"\n", "\r\n", "\n  ", "\n\t"}
	out := make([]noteCase, 0, len(keys)*len(seps)*len(values)*len(breaks))
	for _, k := range keys {
		for _, sep := range seps {
			for _, v := range values {
				for _, br := range breaks {
					n := len(out) + 1
					first, second := fmt.Sprintf("keepA%04d", n), fmt.Sprintf("keepB%04d", n)
					out = append(out, noteCase{note: k + sep + v + br + first + " " + second + " in 5s", value: v, first: first, second: second})
				}
			}
		}
	}
	return out
}

// notesReader answers with one Event whose note is note and a pod whose status message is note.
type notesReader struct {
	client.Reader
	note string
}

func (r *notesReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("unexpected kind %T", obj)
	}
	*p = corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, Status: corev1.PodStatus{Message: r.note}}
	return nil
}

func (r *notesReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l, ok := list.(*eventsv1.EventList)
	if !ok {
		return fmt.Errorf("unexpected kind %T", list)
	}
	*l = eventsv1.EventList{Items: []eventsv1.Event{{
		ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: toolNS},
		Regarding:  corev1.ObjectReference{Kind: toolKind, Name: "web", Namespace: toolNS},
		Reason:     "Failed", Type: eventWarning, Note: r.note,
	}}}
	return nil
}

// noteShown is what one tool showed of a note: the value (its last eight bytes), the first and the second word.
type noteShown [3]bool

func (s noteShown) String() string {
	b := make([]byte, 3)
	for i, x := range s {
		b[i] = "01"[map[bool]int{false: 0, true: 1}[x]]
	}
	return string(b)
}

// notesAnswers calls list_events and get_resource for each note and returns what each showed.
func notesAnswers(t *testing.T) (events, objects []noteShown) {
	t.Helper()
	rd := &notesReader{}
	set, err := tools.New(tools.Config{Pods: rd, Events: rd, Objects: rd, Redactor: defaultRedactor(), Namespaces: []string{toolNS}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) string {
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		r, err := set.Call(context.Background(), providers.ToolCall{ID: "c1", Name: name, Arguments: raw})
		if err != nil {
			t.Fatal(err)
		}
		return r.Text
	}
	for _, c := range notesCases() {
		rd.note = c.note
		for _, a := range []struct {
			text string
			dst  *[]noteShown
		}{
			{call(tools.NameListEvents, map[string]any{"namespace": toolNS}), &events},
			{call(tools.NameGetResource, map[string]any{"kind": toolKind, "namespace": toolNS, "name": "web"}), &objects},
		} {
			v := c.value[len(c.value)-min(8, len(c.value)):]
			*a.dst = append(*a.dst, noteShown{strings.Contains(a.text, v), strings.Contains(a.text, c.first), strings.Contains(a.text, c.second)})
		}
	}
	return events, objects
}

func TestNotesHideAsFarAsMain(t *testing.T) {
	f, err := os.Open(notesMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var main [][2]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatalf("%s: malformed line %q", notesMainTable, line)
		}
		main = append(main, [2]string{parts[1], parts[2]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	cases := notesCases()
	if len(main) != len(cases) {
		t.Fatalf("%s has %d rows, the cases are %d: write it again from main", notesMainTable, len(main), len(cases))
	}
	events, objects := notesAnswers(t)
	leaks, lost := 0, 0
	for i, c := range cases {
		for j, got := range []noteShown{events[i], objects[i]} {
			tool := []string{tools.NameListEvents, tools.NameGetResource}[j]
			was := main[i][j]
			for k, what := range []string{"the value", "the first word after the line break", "the second word"} {
				switch shown, mainShown := got[k], was[k] == '1'; {
				case k == 0 && shown && !mainShown:
					if leaks++; leaks <= 10 {
						t.Errorf("%s of %q: %s shows, main hid it", tool, c.note, what)
					}
				case k > 0 && !shown && mainShown:
					if lost++; lost <= 10 {
						t.Errorf("%s of %q: %s is hidden, main showed it", tool, c.note, what)
					}
				}
			}
		}
	}
	if leaks+lost > 0 {
		t.Errorf("%d values show that main hid, %d words are hidden that main showed, of %d notes", leaks, lost, len(cases))
	}
}

// TestWriteNotesMainTable writes notesMainTable from the code it runs with, when NOTES_MAIN_TABLE names the
// output path (and NOTES_MAIN_COMMIT the commit, for its first line); it is run once in a checkout of main.
func TestWriteNotesMainTable(t *testing.T) {
	path := os.Getenv("NOTES_MAIN_TABLE")
	if path == "" {
		t.Skip("NOTES_MAIN_TABLE is not set")
	}
	events, objects := notesAnswers(t)
	var b strings.Builder
	b.WriteString("# What list_events and get_resource showed of each note of notesCases at main " + os.Getenv("NOTES_MAIN_COMMIT") +
		" (#845): the value, the first and the second word after the line break, 1 for shown.\n# note (Go-quoted)\tlist_events\tget_resource\n")
	for i, c := range notesCases() {
		b.WriteString(strconv.Quote(c.note) + "\t" + events[i].String() + "\t" + objects[i].String() + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}
