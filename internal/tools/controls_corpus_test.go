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
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/redact"
)

// The control corpus (#852): text from the cluster with control characters in it, through every place that
// shows such text to the model, with the answer known by construction. It calls only what origin/main also has
// (New, Set.Call, incidentcontext.Value and Redact, redact.New), so it was run there first to show the symptom:
// of 764 cases, 1,345 outputs showed a secret once their controls are dropped and 1,611 kept a control.

// corpusControl reports whether r is a code point shown text never holds: Unicode category Cc (C0, DEL, C1)
// except newline and tab.
func corpusControl(r rune) bool { return unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t' }

// corpusControls returns every code point of corpusControl, as strings: 30 of C0, DEL and 32 of C1.
func corpusControls() []string {
	var out []string
	for r := rune(0); r <= 0x9f; r++ {
		if corpusControl(r) {
			out = append(out, string(r))
		}
	}
	return out
}

// corpusStrip is the text a reader that ignores control characters sees: each code point of corpusControl
// removed, every other byte kept.
func corpusStrip(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if n == 1 && r == utf8.RuneError || !corpusControl(r) {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// Parts of the corpus. The secret is a value only the cases put after a secret key; the token is shaped like a
// GitHub token, built from parts so the source holds none.
var (
	corpusSecret = "Zq7Kp2Vx9Lm4Wt8R" // #nosec G101 -- not a credential: a made-up value the corpus looks for
	corpusToken  = strings.Repeat("Hd4kQ", 7) + "w"
)

// controlCase is one input of the corpus and its answer: text every shown output keeps (visible) and secrets no
// shown output holds, whole or by its first or last eight bytes, even once its controls are removed.
type controlCase struct {
	name    string
	in      string
	visible []string
	secrets []string
}

// controlCases builds the corpus: for each control, plain text with it at the start, at the end, inside a word,
// as a run, between the bytes of a broken character and between two whole ones; and secrets that read whole
// only once it is removed (a key split by it, a value, a flag, a connection string, a token split at its prefix
// and in its body). Then escape sequences and overwrites, each with its text as a reader without a terminal sees
// it, and a secret a carriage return or backspaces would hide on a terminal.
func controlCases() []controlCase {
	var cases []controlCase
	s := corpusSecret
	for _, c := range corpusControls() {
		n := fmt.Sprintf("%U", []rune(c)[0])
		cases = append(cases,
			controlCase{n + " at the start", c + "alpha bravo", []string{"alpha bravo"}, nil},
			controlCase{n + " at the end", "alpha bravo" + c, []string{"alpha bravo"}, nil},
			controlCase{n + " inside a word", "alpha" + c + "bravo", []string{"alphabravo"}, nil},
			controlCase{n + " as a run", "alpha" + c + c + c + "bravo", []string{"alphabravo"}, nil},
			// Invalid bytes on both sides never join into a character ("é" is C3 A9): each becomes "?".
			controlCase{n + " inside a broken character", "alpha\xc3" + c + "\xa9bravo", []string{"alpha??bravo"}, nil},
			controlCase{n + " between characters", "alphaé" + c + "ébravo", []string{"alphaéébravo"}, nil},
			controlCase{n + " inside a secret key", "pass" + c + "word=" + s, nil, []string{s}},
			controlCase{n + " inside a secret value", "password=" + s[:7] + c + s[7:], nil, []string{s}},
			controlCase{n + " inside a secret flag", "--pass" + c + "word " + s, nil, []string{s}},
			controlCase{n + " inside a connection string", "postgres://admin:" + s[:5] + c + s[5:] + "@db:5432/app", nil, []string{s}},
			controlCase{n + " after a token prefix", "ghp_" + c + corpusToken, nil, []string{corpusToken}},
			controlCase{n + " inside a token", "ghp_" + corpusToken[:17] + c + corpusToken[17:], nil, []string{corpusToken}},
		)
	}
	return append(cases,
		controlCase{"SGR colours", "\x1b[1;31mERROR\x1b[0m connection refused", []string{"[1;31mERROR[0m connection refused"}, nil},
		controlCase{"OSC title", "\x1b]0;window title\x07done", []string{"]0;window titledone"}, nil},
		controlCase{"8-bit CSI", "\u009b31mred text", []string{"31mred text"}, nil},
		controlCase{"backspace overwrite", "public\b\b\b\b\b\bhidden", []string{"publichidden"}, nil},
		controlCase{"carriage return overwrite", "progress 10%\rprogress 99%", []string{"progress 10%progress 99%"}, nil},
		controlCase{"secret under a carriage return", "password=" + s + "\rxxxxxxxxxxxxxxxxxxxxxxxxx", nil, []string{s}},
		controlCase{"secret under backspaces", "password=" + s + strings.Repeat("\b", len(s)) + "xxxxxxxxxxxxxxxx", nil, []string{s}},
		// No terminal grammar is read: the rest of a sequence stays visible text, so "pass[0mword" is no secret key
		// as shown, and the release before showed its value too. Known limit, stated in ADR-0023.
		controlCase{"key split by an escape sequence", "pass\x1b[0mword=x", []string{"pass[0mword=x"}, nil},
	)
}

// corpusReader answers every read with objects that carry one text: a pod whose container argument it is, and
// one Event whose note it is.
type corpusReader struct {
	client.Reader
	text string
}

func (r *corpusReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("unexpected kind %T", obj)
	}
	*p = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container, Args: []string{r.text}}}},
	}
	return nil
}

func (r *corpusReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l, ok := list.(*eventsv1.EventList)
	if !ok {
		return fmt.Errorf("unexpected kind %T", list)
	}
	*l = eventsv1.EventList{Items: []eventsv1.Event{{
		ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: testNS},
		Regarding:  corev1.ObjectReference{Kind: kindPod, Name: podName, Namespace: testNS},
		Reason:     "BackOff", Type: "Warning", Note: r.text,
	}}}
	return nil
}

// corpusRef is the Secret check_missing_reference is asked about.
const corpusRef = "db-creds"

// controlEntry is one place that shows cluster text to the model, and whether its output holds the text of
// the input (a tool that only answers about it in fixed words does not).
type controlEntry struct {
	name  string
	shows bool
	call  func(t *testing.T, in string) string
}

// controlEntries returns the places of the corpus: the context's value and redacted text, and every read tool
// that shows text from a source, over one tool set with the built-in rules.
func controlEntries(t *testing.T) []controlEntry {
	t.Helper()
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	reader, logs, metrics, delivery := &corpusReader{}, &fake.Logs{}, &fake.Metrics{}, &fake.Delivery{}
	set, err := New(Config{Pods: reader, Events: reader, Objects: reader, Logs: logs, Metrics: metrics,
		Delivery: delivery, Redactor: red, Namespaces: []string{testNS}, DeliveryNamespaces: []string{testNS}})
	if err != nil {
		t.Fatal(err)
	}
	tool := func(name string, args map[string]any) func(t *testing.T, in string) string {
		return func(t *testing.T, in string) string {
			reader.text = in
			logs.Set(fake.LogKey{Namespace: testNS, Pod: podName, Container: container}, in)
			metrics.Set(restartsTemplate, []providers.Series{{Labels: map[string]string{argContainer: container,
				"message": in}, Samples: []providers.Sample{{Value: 1}}}})
			delivery.Set(providers.DeliveryUnit{Kind: kustomization, Namespace: testNS, Name: unitName},
				providers.DeliveryStatus{AppliedRevision: "main@sha1:0123456789abcdef0123456789abcdef01234567", Message: in})
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			r, err := set.Call(context.Background(), providers.ToolCall{ID: "c1", Name: name, Arguments: raw})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if r.Status != StatusOK {
				t.Fatalf("%s answered %s: %s", name, r.Status, r.Text)
			}
			return r.Text
		}
	}
	return []controlEntry{
		{"incidentcontext.Value", true, func(_ *testing.T, in string) string { return incidentcontext.Value(incidentcontext.Twice(red), in) }},
		{"incidentcontext.Redact", true, func(_ *testing.T, in string) string { return incidentcontext.Redact(incidentcontext.Twice(red), in) }},
		{NameGetResource, true, tool(NameGetResource, map[string]any{argKind: kindPod, argNamespace: testNS, argName: podName})},
		{NameListEvents, true, tool(NameListEvents, map[string]any{argNamespace: testNS, argKind: kindPod, argName: podName, argLimit: 5})},
		{NameGetPodLogs, true, tool(NameGetPodLogs, map[string]any{argNamespace: testNS, argPod: podName, argContainer: container})},
		{NameQueryMetrics, true, tool(NameQueryMetrics, map[string]any{argNamespace: testNS, argPod: podName, argTemplate: restartsTemplate})},
		{NameGetDeliveryStatus, true, tool(NameGetDeliveryStatus, map[string]any{argKind: kustomization, argNamespace: testNS, argName: unitName})},
		{NameCheckMissingReference, false, tool(NameCheckMissingReference, map[string]any{argNamespace: testNS, argPod: podName, argKind: secretKind, argName: corpusRef})},
	}
}

// secretShown reports whether out shows the secret s, or its first or last eight bytes.
func secretShown(out, s string) bool {
	n := min(len(s), 8)
	return strings.Contains(out, s[:n]) || strings.Contains(out, s[len(s)-n:])
}

// Every input of the control corpus, through every place that shows cluster text to the model: the output holds
// no control character but newline and tab, is valid UTF-8, keeps the visible text of the input, and shows no
// secret, not even to a reader that drops what control characters are left (#852). Under -race a seeded sample
// of the cases runs.
func TestControlCorpusThroughEveryEntryPoint(t *testing.T) {
	cases := controlCases()
	if raceDetector {
		rng := rand.New(rand.NewPCG(852, 1)) // #nosec G404 -- a reproducible sample, not a secret
		rng.Shuffle(len(cases), func(i, j int) { cases[i], cases[j] = cases[j], cases[i] })
		cases = cases[:len(cases)/8]
	}
	t.Logf("%d cases, %d controls", len(cases), len(corpusControls()))
	failed := map[string]int{}
	for _, e := range controlEntries(t) {
		for _, c := range cases {
			out := e.call(t, c.in)
			var why []string
			if strings.ContainsFunc(out, corpusControl) {
				why = append(why, "holds a control")
			}
			if !utf8.ValidString(out) {
				why = append(why, "is not valid UTF-8")
			}
			for _, v := range c.visible {
				if e.shows && !strings.Contains(out, v) {
					why = append(why, fmt.Sprintf("lost %q", v))
				}
			}
			for _, s := range c.secrets {
				if secretShown(corpusStrip(out), s) {
					why = append(why, fmt.Sprintf("shows %q", s))
				}
			}
			if len(why) > 0 {
				failed[e.name]++
				if failed[e.name] <= 5 {
					t.Errorf("%s, %s: %s\n  in: %q\n out: %q", e.name, c.name, strings.Join(why, ", "), c.in, out)
				}
			}
		}
	}
	for name, n := range failed {
		t.Errorf("%s: %d of %d cases failed", name, n, len(cases))
	}
}
