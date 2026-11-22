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

package incidentcontext

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/redact"
)

// removedControl reports whether r is a code point context text is shown without: Unicode category Cc except
// newline and tab (#852).
func removedControl(r rune) bool { return unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t' }

// everyControl is every code point of category Cc but newline and tab, each after an "x".
func everyControl() string {
	var b strings.Builder
	for r := rune(0); r <= 0x9f; r++ {
		if removedControl(r) {
			b.WriteString("x" + string(r))
		}
	}
	return b.String()
}

// The entry points through which cluster text becomes context remove every control but newline and tab after
// redaction (decided 2026-11-17 on #852), so the text shown, the parts a quote may cite and a model's cleaned
// quote agree. Visible text around the controls survives; a value still never adds lines.
func TestContextTextIsShownWithoutControls(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	r := Twice(red)
	in := "\x1b[31mERROR\x1b[0m db " + everyControl() + "\nnext\tline\n"
	visible := "[31mERROR[0m db " + strings.Repeat("x", strings.Count(everyControl(), "x"))
	ex := providers.LogExcerpt{Text: in, CollectedAt: time.Unix(0, 0), Instance: providers.LogInstance{PodUID: "u\x1bid"}}
	log := RenderLog(red, false, "pod p\x1b container c", ex, nil, 4096, 4096)
	refit := RenderLog(red, true, "pod p\x1b container c", ex, nil, 4096, 4096)
	fit, _, _ := FitLog(refit.Text, refit.HeadLen, 4096)
	for name, tc := range map[string]struct {
		got, want string
	}{
		"Redact":                  {Redact(r, in), visible + "\nnext\tline\n"},
		"Value":                   {Value(r, in), visible + " next line "},
		"RenderLog":               {log.Text, "pod p container c\n"},
		"RenderLog excerpt":       {log.Text[log.HeadLen:], visible + "\nnext\tline\n"},
		"RenderLog refit excerpt": {refit.Text[refit.HeadLen:], visible + "\nnext\tline\n"},
		"FitLog":                  {fit, "pod p container c\n"},
	} {
		if strings.ContainsFunc(tc.got, removedControl) || !strings.Contains(tc.got, tc.want) {
			t.Errorf("%s = %q, want it without controls and holding %q", name, tc.got, tc.want)
		}
	}
}

// Removing a control can join the parts of a secret's key: the decision reads the text as shown too (the
// redactor's controls view), so the value is hidden before the control is removed, and nothing is redacted again.
func TestControlInsideAKeyIsHiddenByTheDecision(t *testing.T) {
	const secret = "Hunter2Secret9x" // #nosec G101 -- not a credential: a made-up value the test looks for
	in := "pass\x1bword=" + secret
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	if got := Redact(Twice(red), in); strings.Contains(got, secret) || strings.ContainsFunc(got, removedControl) {
		t.Errorf("Redact(%q) = %q, leaks %q or keeps a control", in, got, secret)
	}
}

// Every section of a built context that shows text from the cluster (the Incident's summary, the status of
// the subject and its pods, the log excerpts, the metric labels) shows it without controls, in its text and
// in every part a quote may cite, and keeps the visible text: an ANSI-coloured log line is quotable as the
// model would copy it once its quote is cleaned.
func TestBuiltContextHasNoControls(t *testing.T) {
	const line = "\x1b[1;31mERROR\x1b[0m connection refused\r"
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	ref := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	objs := []client.Object{d, rs, crashing(pod(crashPod, rs.Labels, longAgo, ref), "exit\x1b[0m code\x07 1"),
		pod("web-abc-2", rs.Labels, longAgo, ref)}
	s := webSources()
	s.logs.Set(fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true}, "starting\n"+line+"\n")
	s.metrics.Set(metricPlans[0].template, []providers.Series{{
		Labels:  map[string]string{labelPod: crashPod, labelContainer: "si\x1bde"},
		Samples: []providers.Sample{{Time: testNow, Value: 1}},
	}})
	inc := incident(deploymentRef())
	inc.Spec.Symptom = "Back-off\x1b[0m restarting\u009b failed container app"
	inc.Spec.Reason = "Crash\x07Loop"
	c := build(t, newBuilder(t, reader(t, objs...), s, nil), inc)
	seen := map[Section]bool{}
	for _, b := range c.Blocks {
		seen[b.Section] = true
		if strings.ContainsFunc(b.Text, removedControl) {
			t.Errorf("block %s shows a control:\n%q", b.Section, b.Text)
		}
		for _, d := range b.Data() {
			if strings.ContainsFunc(d, removedControl) {
				t.Errorf("block %s has a quotable part with a control: %q", b.Section, d)
			}
		}
	}
	for _, want := range []struct {
		section Section
		text    string
	}{
		{SectionSummary, "Back-off[0m restarting failed container app"},
		{SectionSummary, "CrashLoop"},
		{SectionStatus, "exit[0m code 1"},
		{SectionLogs, "[1;31mERROR[0m connection refused"},
		{SectionMetrics, "container=side"},
	} {
		found := false
		for _, b := range blocks(c, want.section) {
			for _, d := range b.Data() {
				found = found || strings.Contains(d, want.text)
			}
		}
		if !found {
			t.Errorf("no quotable part of section %s holds %q", want.section, want.text)
		}
	}
	if fmt.Sprint(seen[SectionSummary], seen[SectionStatus], seen[SectionLogs], seen[SectionMetrics]) != "true true true true" {
		t.Errorf("sections built %v, want summary, status, logs and metrics", seen)
	}
}

// The context and the model's text are shown without the same code points: redact.Control, which the context
// and the redactor's controls view use, and the set providers.StripControls removes from model text, are one set
// (#852), checked on every code point.
func TestContextAndModelTextLoseTheSameControls(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		s := "a" + string(r) + "b"
		if got, want := redact.Control(r), providers.StripControls(s) == "ab"; got != want || got != removedControl(r) {
			t.Fatalf("%U: redact.Control %v, removed from model text %v, Cc but newline and tab %v", r, got, want, removedControl(r))
		}
	}
}
