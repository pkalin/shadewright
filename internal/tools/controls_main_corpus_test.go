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

package tools

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// The entries corpus of #852 (decision of 2026-11-22: no input shows a secret origin/main hides): a secret key and
// its value in two entries of a read tool's answer (two label values, a label's name and value, two arguments,
// two messages, two log lines, two event notes, a revision and a message, a key and a note), with one control
// character in the key, after it, between the entries, before the value or after it. The six read tools that show
// text from a source each take the layouts that fit them. The answer is known by construction (where the secret
// is), and origin/main's own answer is a table: for each case, whether its answer hid the secret. This file
// compiles on origin/main as it is; the table was written there with ENTRIES_MAIN_TABLE set to the output path.
// TestEntriesHideWhatMainHid reads it.

// entriesMainTable is the table of origin/main's answers on the entries corpus.
const entriesMainTable = "testdata/entries-main.tsv"

// entriesSecret is the value of every form: made up, shaped like a credential.
const entriesSecret = "Qz900339SecretValue" // #nosec G101 -- not a credential: a made-up value the corpus looks for

// entriesForm is a secret key in the first entry and its value in the second: the first entry is key[0], the key
// word key[1] and key[2] joined; the control goes inside the key word at the middle of key[1].
type entriesForm struct{ key [3]string }

// Words of the forms, and the type of the events.
const (
	entriesPassword = "password"
	entriesToken    = "token"
	eventWarning    = "Warning"
)

// entriesForms are the forms of the corpus.
var entriesForms = []entriesForm{
	{key: [3]string{"postgres://u:p@h/db---", entriesPassword, ""}},
	{key: [3]string{"--", entriesPassword, ""}},
	{key: [3]string{"", entriesPassword, ""}},
	{key: [3]string{"", entriesToken, ":"}},
	{key: [3]string{"Authorization: ", "Bearer", ""}},
	{key: [3]string{"db_", "pass", "="}},
	{key: [3]string{"postgres://u:p@h/db", "---password", ""}},
}

// entriesPlaces are where the control goes: 0 inside the key word, 1 at the end of the first entry, 2 at the start
// of the second, 3 at its end, 4 at the start of the first, 5 before the key word, 6 after it.
const entriesPlaces = 7

// entriesControls returns the controls a layout is tried with: every code point of Cc but newline and tab for
// all (63), or a sample of 13 of C0, DEL and C1.
func entriesControls(all bool) []rune {
	if !all {
		return []rune{0x00, 0x01, '\b', '\v', '\f', '\r', 0x1b, 0x1f, 0x7f, 0x80, 0x85, 0x9b, 0x9f}
	}
	var out []rune
	for r := rune(0); r <= 0x9f; r++ {
		if entriesControl(r) {
			out = append(out, r)
		}
	}
	return out
}

// entriesLayout puts the two entries into a tool's sources and calls it.
type entriesLayout struct {
	name string
	all  bool // tried with every control
	run  func(set *Set, src *sceneSources, a, b string) Result
}

// entriesLayouts are the layouts of the corpus, by tool.
func entriesLayouts() []entriesLayout {
	sample := []providers.Sample{{Time: testNow, Value: 1}}
	pod := func() *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNS},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: container}}}}
	}
	metrics := func(series []providers.Series) func(set *Set, src *sceneSources) Result {
		return func(set *Set, src *sceneSources) Result {
			tmpl := metricTemplates[2]
			return src.run(set, diffScene{pod: pod(), series: series}, NameQueryMetrics,
				map[string]any{argTemplate: tmpl.name, argNamespace: testNS, argPod: podName})
		}
	}
	resource := func(in redacttest.Input) func(set *Set, src *sceneSources) Result {
		return func(set *Set, src *sceneSources) Result {
			scene, obj, kind := getResourceScene(in)
			return src.run(set, scene, NameGetResource, map[string]any{argKind: kind, argNamespace: testNS, argName: obj.GetName()})
		}
	}
	fields := func(fs ...string) []string { return append(fs, make([]string, 6-len(fs))...) }
	return []entriesLayout{
		{"query_metrics label values", true, func(set *Set, src *sceneSources, a, b string) Result {
			return metrics([]providers.Series{{Labels: map[string]string{"arg0": a, "arg1": b}, Samples: sample}})(set, src)
		}},
		{"query_metrics label name and value", true, func(set *Set, src *sceneSources, a, b string) Result {
			return metrics([]providers.Series{{Labels: map[string]string{a: b}, Samples: sample}})(set, src)
		}},
		{"query_metrics one label value", true, func(set *Set, src *sceneSources, a, b string) Result {
			return metrics([]providers.Series{{Labels: map[string]string{"arg0": a + " " + b}, Samples: sample}})(set, src)
		}},
		{"get_resource arguments", false, func(set *Set, src *sceneSources, a, b string) Result {
			return resource(redacttest.Input{Knob: 0, Fields: fields(a, b)})(set, src)
		}},
		{"get_resource label name and value", false, func(set *Set, src *sceneSources, a, b string) Result {
			return resource(redacttest.Input{Knob: 1, Fields: fields(a, b)})(set, src)
		}},
		{"get_resource label values", false, func(set *Set, src *sceneSources, a, b string) Result {
			return resource(redacttest.Input{Knob: 1, Fields: fields("a0", a, "a1", b)})(set, src)
		}},
		{"get_resource messages of a Deployment", false, func(set *Set, src *sceneSources, a, b string) Result {
			return resource(redacttest.Input{Knob: 0x12, Fields: fields(a, b)})(set, src)
		}},
		{"get_pod_logs lines", false, func(set *Set, src *sceneSources, a, b string) Result {
			return src.run(set, diffScene{excerpt: providers.LogExcerpt{Text: a + "\n" + b}}, NameGetPodLogs, map[string]any{
				argNamespace: testNS, argPod: podName, argContainer: container, argMaxBytes: 4096,
			})
		}},
		{"list_events reason and note", false, func(set *Set, src *sceneSources, a, b string) Result {
			return src.run(set, diffScene{events: eventsScene(redacttest.Input{Knob: 0, Fields: []string{a, b}})},
				NameListEvents, map[string]any{argNamespace: testNS})
		}},
		{"get_delivery_status revision and message", false, func(set *Set, src *sceneSources, a, b string) Result {
			return src.run(set, diffScene{delivery: providers.DeliveryStatus{AppliedRevision: a, Message: b}},
				NameGetDeliveryStatus, map[string]any{argKind: kustomization, argNamespace: fluxNS, argName: unitName})
		}},
		{"check_missing_reference notes", false, func(set *Set, src *sceneSources, a, b string) Result {
			p := pod()
			p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "E0", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "k"},
			}}}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: container, State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: createConfigError,
					Message: fmt.Sprintf("couldn't find key k in Secret %v/db", testNS)},
			}}}
			evs := make([]eventsv1.Event, 0, 2)
			for i, note := range []string{a, b} {
				evs = append(evs, eventsv1.Event{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("e%d", i), Namespace: testNS},
					EventTime:  metav1.NewMicroTime(testNow.Add(-time.Minute)), Type: eventWarning, Reason: "FailedMount", Note: note,
					Regarding: corev1.ObjectReference{Kind: kindPod, Name: podName, Namespace: testNS},
				})
			}
			return src.run(set, diffScene{pod: p, events: evs}, NameCheckMissingReference, map[string]any{
				argNamespace: testNS, argPod: podName, argKind: secretKind, argName: "db",
			})
		}},
	}
}

// entriesCase is one input of the corpus.
type entriesCase struct {
	layout, form, place int
	ctl                 rune
	a, b                string
}

// entriesCases returns the corpus, layout by layout, form by form, place by place, control by control.
func entriesCases() []entriesCase {
	var out []entriesCase
	for li, l := range entriesLayouts() {
		for fi, f := range entriesForms {
			for place := range entriesPlaces {
				for _, c := range entriesControls(l.all) {
					k := f.key
					ctl := string(c)
					a, b := k[0]+k[1]+k[2], entriesSecret
					switch place {
					case 0:
						mid := len(k[1]) / 2
						a = k[0] + k[1][:mid] + ctl + k[1][mid:] + k[2]
					case 1:
						a += ctl
					case 2:
						b = ctl + b
					case 3:
						b += ctl
					case 4:
						a = ctl + a
					case 5:
						a = k[0] + ctl + k[1] + k[2]
					default:
						a = k[0] + k[1] + ctl + k[2]
					}
					out = append(out, entriesCase{layout: li, form: fi, place: place, ctl: c, a: a, b: b})
				}
			}
		}
	}
	return out
}

// entriesControl reports whether c is Cc but newline and tab.
func entriesControl(c rune) bool {
	return (c < 0x20 && c != '\n' && c != '\t') || (c >= 0x7f && c <= 0x9f)
}

// entriesShown reports whether r shows the secret to a reader that ignores control characters. An answer
// withheld whole shows nothing.
func entriesShown(r Result) bool {
	if slices.Contains(r.Labels, LabelWithheld) {
		return false
	}
	var b strings.Builder
	for i := 0; i < len(r.Text); {
		c, n := utf8.DecodeRuneInString(r.Text[i:])
		if c == utf8.RuneError || !entriesControl(c) {
			b.WriteString(r.Text[i : i+n])
		}
		i += n
	}
	return strings.Contains(b.String(), entriesSecret)
}

// entriesKey is the row of the table a case is in.
func entriesKey(c entriesCase) string { return fmt.Sprintf("%d\t%d\t%d", c.layout, c.form, c.place) }

// TestWriteEntriesMainTable writes the table of the corpus with this tree's read tools when ENTRIES_MAIN_TABLE
// names the output: one row per layout, form and place, one character per control, "h" when the answer hid the
// secret and "s" when it showed it.
func TestWriteEntriesMainTable(t *testing.T) {
	path := os.Getenv("ENTRIES_MAIN_TABLE")
	if path == "" {
		t.Skip("ENTRIES_MAIN_TABLE is not set")
	}
	set, src := diffSet(t)
	layouts := entriesLayouts()
	rows := map[string]*strings.Builder{}
	var order []string
	for _, c := range entriesCases() {
		k := entriesKey(c)
		if rows[k] == nil {
			rows[k] = &strings.Builder{}
			order = append(order, k)
		}
		if entriesShown(layouts[c.layout].run(set, src, c.a, c.b)) {
			_ = rows[k].WriteByte('s')
		} else {
			_ = rows[k].WriteByte('h')
		}
	}
	var b strings.Builder
	b.WriteString("# origin/main's read tools on the entries corpus of controls_main_corpus_test.go (#852): layout, form, place,\n")
	b.WriteString("# then per control in order h (hidden) or s (shown). Written with ENTRIES_MAIN_TABLE.\n")
	for _, k := range order {
		b.WriteString(k + "\t" + rows[k].String() + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- the path is the test runner's own setting
		t.Fatal(err)
	}
}
