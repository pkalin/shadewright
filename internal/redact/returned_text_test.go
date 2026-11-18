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
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// The text decided on is the text the tool returns (#845, sixth decision of 2026-11-18, point 1). The layout put
// field names between the values and the tool then rendered a line without them, so a pattern that exists only
// in the rendered line was missed: an Event whose reason is "Bearer" and whose object has a long name ("Warning
// Bearer Pod/qz12345xk9walphabeta"), which main hid as a bearer token. Here every read tool gets values that form
// a pattern only with a neighbour: a key, a scheme or a flag in one place and a value in another, both in every
// order the places can take, and String over the returned text must hide nothing more of the values: no word
// (a run of letters, digits, "_" and "-") of a value placed, or of the pod's name and namespace, is lost. String over an
// answer also reads its markers, so it may take text the tool writes after a key (the closing quote and the next
// field name of a JSON object after "token="): that changes no value, and the decision leaves that text as it is.

// returnedKeys end a pattern that a value in the next place completes; returnedValues start one.
// eventWarning and flagPassword are inputs several tests share.
const (
	eventWarning = "Warning"
	flagPassword = "--password"
)

var (
	returnedKeys   = []string{"Bearer", "password:", "token=", flagPassword, "X-Api-Key", "Authorization: Bearer", "api_key", "auth"}
	returnedValues = []string{"qz12345xk9walphabeta", "--verbose", "V9qz-1234567890", ": V9qzk", "=Q7wxyz12"}
)

// returnedAllPairs is the number of places up to which a type gets every ordered pair of its places; a larger
// type gets each place with the next one and returnedRandomPairs pairs drawn from a fixed seed.
const (
	returnedAllPairs    = 64
	returnedRandomPairs = 400
)

// returnedPairs returns the ordered pairs of places the test fills for a type.
func returnedPairs(places []fieldPlace, r *rand.Rand) [][2]int {
	var out [][2]int
	if len(places) <= returnedAllPairs {
		for i := range places {
			for j := range places {
				if i != j {
					out = append(out, [2]int{i, j})
				}
			}
		}
		return out
	}
	for i := range len(places) - 1 {
		out = append(out, [2]int{i, i + 1})
	}
	for range returnedRandomPairs {
		i, j := r.IntN(len(places)), r.IntN(len(places))
		if i != j {
			out = append(out, [2]int{i, j})
		}
	}
	return out
}

// fill writes text into the place p of root: the text of a string, the one element of a list, or the value of
// a map entry under the key "k" (in the key when the map's values hold no text).
func (p fieldPlace) fill(root reflect.Value, text string) {
	if p.kind != reflect.Map {
		p.setText(root, text)
		return
	}
	cur := p.at(root)
	m := reflect.MakeMap(cur.Type())
	val := reflect.New(p.elem).Elem()
	key := "k"
	if p.elem.Kind() == reflect.String {
		val.SetString(text)
	} else {
		key = text
	}
	m.SetMapIndex(reflect.ValueOf(key).Convert(cur.Type().Key()), val)
	cur.Set(m)
}

func TestStringOverAToolResultHidesNothingMore(t *testing.T) {
	r := rand.New(rand.NewPCG(845, 6)) // #nosec G404 -- test data from a fixed seed, not a secret
	red := defaultRedactor()
	calls, failed, n := 0, 0, 0
	for _, ft := range fieldTools {
		e := newFieldEnv(t)
		for _, typ := range ft.types {
			places := fieldPlaces(typ)
			for turn, pq := range returnedPairs(places, r) {
				p, q := places[pq[0]], places[pq[1]]
				for c := range 2 {
					k := returnedKeys[(turn+c)%len(returnedKeys)]
					v := returnedValues[(turn+3*c)%len(returnedValues)]
					if n++; !redact.RaceKeeps("returned text", n) {
						continue
					}
					calls++
					ft.call(t, e, typ, "unused-value", func(obj reflect.Value) {
						p.fill(obj, k)
						q.fill(obj, v)
					})
					text := ft.answer(t, e)
					if again := red.String(text); lostWords(text, again, k, v, fieldsPod, fieldsNS) != "" {
						if failed++; failed <= 10 {
							t.Errorf("%s %s = %q, %s = %q: String hides more of the answer\n  answer: %q\n  String: %q",
								ft.name, p.name(typ), k, q.name(typ), v, text, again)
						}
					}
				}
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d answers: String over the answer hides more", failed, calls)
	}
	t.Logf("%d answers", calls)
}

// lostWords returns the first word of values (a run of letters, digits, "_" and "-") that again holds fewer times than
// text, or "".
func lostWords(text, again string, values ...string) string {
	words := func(s string) map[string]int {
		n := map[string]int{}
		for _, w := range strings.FieldsFunc(s, func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' && c != '-' }) {
			n[w]++
		}
		return n
	}
	before, after := words(text), words(again)
	for w := range words(strings.Join(values, " ")) {
		if after[w] < before[w] {
			return w
		}
	}
	return ""
}

// The Event of point 1 of the sixth decision: main hid the name after "Bearer Pod/" as a bearer token.
func TestListEventsDecidesTheRenderedLine(t *testing.T) {
	e := newFieldEnv(t)
	e.reader.events = []eventsv1.Event{{
		ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: fieldsNS},
		Type:       eventWarning, Reason: "Bearer", Note: "request rejected",
		Regarding: corev1.ObjectReference{Kind: toolKind, Name: "qz12345xk9walphabeta", Namespace: fieldsNS},
	}}
	text := callFieldTool(t, e.set, tools.NameListEvents, map[string]any{fieldsArgNS: fieldsNS})
	if strings.Contains(text, "qz12345xk9walphabeta") {
		t.Errorf("the name after Bearer shows:\n%s", text)
	}
	if !strings.Contains(text, "request rejected") {
		t.Errorf("the note is gone:\n%s", text)
	}
}

// The notes of point 3 of the sixth decision, with what main shows of each at list_events (main hides the
// indented line after "password: hunter2" with the value), and the note of that point at get_resource, where
// main's reading of the JSON ran on over "\n" to the next blank.
func TestListEventsNotesOfTheSixthDecision(t *testing.T) {
	e := newFieldEnv(t)
	for note, want := range map[string][]string{
		"token: expired\nretrying in 5s":       {"retrying in 5s"},
		"auth: invalid\nsee runbook":           {"auth: invalid see runbook"},
		"refresh token=Q7wxyz12\nnext attempt": {"next attempt"},
		"password: hunter2\n  host":            nil,
	} {
		e.reader.events = []eventsv1.Event{{
			ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: fieldsNS},
			Type:       "Warning", Reason: "Failed", Note: note,
			Regarding: corev1.ObjectReference{Kind: toolKind, Name: fieldsPod, Namespace: fieldsNS},
		}}
		text := callFieldTool(t, e.set, tools.NameListEvents, map[string]any{fieldsArgNS: fieldsNS})
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("list_events of %q: %q is gone:\n%s", note, w, text)
			}
		}
		for _, secret := range []string{"expired", "Q7wxyz12", "hunter2"} {
			if strings.Contains(note, secret) && strings.Contains(text, secret) {
				t.Errorf("list_events of %q shows %q:\n%s", note, secret, text)
			}
		}
	}
	e.reader.obj = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fieldsPod, Namespace: fieldsNS},
		Status:     corev1.PodStatus{Message: "token: expired\nretrying in 5s"},
	}
	text := callFieldTool(t, e.set, tools.NameGetResource, map[string]any{fieldsArgKind: toolKind, fieldsArgNS: fieldsNS, fieldsArgName: fieldsPod})
	if !strings.Contains(text, " in 5s") || strings.Contains(text, "expired") || strings.Contains(text, "retrying") {
		t.Errorf("get_resource of a note: want expired and retrying hidden, in 5s shown:\n%s", text)
	}
}

// The object gives the key: under a secret map key the whole value is hidden, and nothing beyond it (#845,
// sixth decision of 2026-11-18, point 2). A value that opens a block ("|\n  block") took the next entry of the
// map with it. Each map of strings that a tool shows gets an entry under each secret key with a value in each
// shape, between two entries that must show.
func TestASecretMapValueTakesNoOtherEntry(t *testing.T) {
	shapes := append([]fieldShape{
		{name: "a literal block", shape: func(v string) string { return "|\n  " + v }},
		{name: "a folded block", shape: func(v string) string { return ">-\n  " + v + "\n  more" }},
		{name: "a flow mapping", shape: func(v string) string { return "{" + v }},
		{name: "an open quote", shape: func(v string) string { return "\"" + v }},
	}, fieldShapes...)
	keys := fieldSecretKeys()
	n, maps, lost := 0, 0, 0
	for _, ft := range fieldTools {
		e := newFieldEnv(t)
		for _, typ := range ft.types {
			for _, p := range fieldPlaces(typ) {
				if p.kind != reflect.Map || p.elem.Kind() != reflect.String {
					continue
				}
				if !ft.call(t, e, typ, "Ent0zzkq", func(obj reflect.Value) { p.fill(obj, "Ent0zzkq") }) {
					continue
				}
				maps++
				for i, sh := range shapes {
					n++
					if !redact.RaceKeeps("secret map value", n) {
						continue
					}
					k := keys[(i*7+n)%len(keys)]
					v := fmt.Sprintf("Vm%06dxK9w", n)
					ft.call(t, e, typ, v, func(obj reflect.Value) {
						cur := p.at(obj)
						m := reflect.MakeMap(cur.Type())
						for key, val := range map[string]string{"aaa-before": "keepme1", k: sh.shape(v), "zzz-other": "keepme2"} {
							m.SetMapIndex(reflect.ValueOf(key).Convert(cur.Type().Key()), reflect.ValueOf(val).Convert(cur.Type().Elem()))
						}
						cur.Set(m)
					})
					text := ft.answer(t, e)
					for _, w := range []string{"keepme1", "keepme2"} {
						if !strings.Contains(text, w) {
							if lost++; lost <= 10 {
								t.Errorf("%s %s, key %q, value %s: the entry %s is hidden:\n%s", ft.name, p.name(typ), k, sh.name, w, text)
							}
						}
					}
					if strings.Contains(text, v[:8]) || strings.Contains(text, v[len(v)-8:]) {
						t.Errorf("%s %s, key %q, value %s: the value shows:\n%s", ft.name, p.name(typ), k, sh.name, text)
					}
				}
			}
		}
	}
	if maps == 0 {
		t.Fatal("no tool shows a map of strings")
	}
	if lost > 0 {
		t.Errorf("%d entries next to a secret value are hidden", lost)
	}
}

// Map keys that redact alike are told apart in linear work (#845, sixth decision of 2026-11-18, point 5): one
// counter per redacted key, not a search from " (2)" for each, which made n such keys take n*n/2 probes (1,145 ms
// for 4,096 keys). The allocations of get_resource for a pod with n and 4n such labels must grow about fourfold,
// not sixteenfold.
func TestRenamingAlikeMapKeysIsLinear(t *testing.T) {
	e := newFieldEnv(t)
	allocs := func(n int) float64 {
		labels := make(map[string]string, n)
		for i := range n {
			labels[fmt.Sprintf("AbCdEfGhIjKlMnOpQrStUvWx%08d", i)] = "v"
		}
		e.reader.obj = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fieldsPod, Namespace: fieldsNS, Labels: labels}}
		args := map[string]any{fieldsArgKind: toolKind, fieldsArgNS: fieldsNS, fieldsArgName: fieldsPod}
		return testing.AllocsPerRun(1, func() { callFieldTool(t, e.set, tools.NameGetResource, args) })
	}
	small, large := allocs(512), allocs(2048)
	t.Logf("allocations for 512 and 2,048 alike keys: %.0f and %.0f (%.1f times)", small, large, large/small)
	if large > 8*small {
		t.Errorf("4 times the keys take %.1f times the allocations: the renaming is not linear", large/small)
	}
}

// Renamed keys keep the map's entries, a real key already named like a renamed one pushes it on, and the
// names are the same on every call.
func TestRenamedMapKeysAreStable(t *testing.T) {
	e := newFieldEnv(t)
	labels := map[string]string{}
	for i := range 4 {
		labels["ghp_"+strings.Repeat(string(rune('A'+i)), 36)] = fmt.Sprintf("Ent%dqzk", i)
	}
	marker := redact.Marker(redact.KindToken, 40)
	labels[marker+" (2)"] = "Real2qzk"
	e.reader.obj = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fieldsPod, Namespace: fieldsNS, Labels: labels}}
	args := map[string]any{fieldsArgKind: toolKind, fieldsArgNS: fieldsNS, fieldsArgName: fieldsPod}
	first := callFieldTool(t, e.set, tools.NameGetResource, args)
	var obj struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(first), &obj); err != nil {
		t.Fatalf("the answer is no JSON: %v\n%s", err, first)
	}
	if len(obj.Metadata.Labels) != len(labels) {
		t.Errorf("%d labels, the pod has %d:\n%s", len(obj.Metadata.Labels), len(labels), first)
	}
	if obj.Metadata.Labels[marker+" (2)"] != "Real2qzk" {
		t.Errorf("the real key %q lost its value:\n%s", marker+" (2)", first)
	}
	for range 5 {
		if again := callFieldTool(t, e.set, tools.NameGetResource, args); again != first {
			t.Fatalf("the answer changed between calls:\n%s\n%s", first, again)
		}
	}
}

// A list of autolinks main hid whole as one connection string is hidden whole again (#845, sixth decision of
// 2026-11-18, point 4): property 1 has no exceptions; showing more than main is #860.
func TestAutolinksAreHiddenAsOnMain(t *testing.T) {
	address := "NAME" + "@" + "example.com"
	in := "<https://example.com>,<mailto:" + address + ">"
	want := in[:1] + redact.Marker(redact.KindConnectionString, len(in)-2) + in[len(in)-1:]
	if got := redact.String(in); got != want {
		t.Errorf("String(%q) = %q, want %q", in, got, want)
	}
}
