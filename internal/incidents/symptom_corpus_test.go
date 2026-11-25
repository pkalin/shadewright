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

package incidents

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shadewright-labs/shadewright/internal/analyzer/collector"
	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
	"github.com/shadewright-labs/shadewright/internal/modules"
	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// The symptom an Incident stores, and the module evidence it is built from, never show a secret a control
// character or a terminal escape sequence splits or hides. The inputs are generated: every secret form of the
// controls gate (redacttest.GateCases) with each control filler at each boundary of its parts, and each form with a
// control or escape sequence around its secret, inside it at every byte, or both. A reader of the stored text, or a
// terminal that renders the note, sees the same text as without them. So the right answer is known by construction
// wherever the path hides the secret of the plain text (a form it supports): no four-byte part of the secret is
// shown (redacttest.VisibleGrams, which also reads the output with every filler removed), and the text before the
// form ("log: ") is kept. A form a path does not hide even without controls (a JSON member, a glued "xBearer") is
// not this test's subject; the controls gate of package redact compares those with origin/main.

// symptomInput is one generated note and the secret it holds.
type symptomInput struct {
	name, text, probe, prefix string
	// plain is text with the controls and escape sequences placed as a terminal prints them: what a reader of the
	// note sees.
	plain string
}

// symptomControls are the controls and escape sequences placed around and inside each secret: C0 controls
// (NUL, backspace, carriage return, escape), DEL, C1 controls (NEL, the CSI U+009B), and ANSI sequences that a
// terminal does not print (colour, reset, bold red, a C1 CSI colour).
var symptomControls = []string{
	"\x00", "\b", "\r", "\x1b", "\x7f", "\u0085", "\u009b",
	"\x1b[31m", "\x1b[0m", "\x1b[1;31m", "\u009b31m",
}

// startsWithControl reports whether s starts with a code point of redact.Control.
func startsWithControl(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return redact.Control(r)
}

// printed is a filler as a terminal prints it: nothing for an escape sequence, the filler without its controls
// otherwise, so a line end or a zero-width character after a carriage return stays.
func printed(fill string) string {
	if strings.HasPrefix(fill, "\x1b[") || strings.HasPrefix(fill, "\u009b") {
		return ""
	}
	return redact.WithoutControls(fill)
}

// symptomCorpus returns the generated inputs.
func symptomCorpus() []symptomInput {
	var out []symptomInput
	bases := map[string]redacttest.GateCase{} // one case of each form, to rebuild the form without its filler
	for _, c := range redacttest.GateCases() {
		if !startsWithControl(c.Filler) {
			continue
		}
		out = append(out, symptomInput{name: c.Name, text: c.Text(), probe: c.Probe, prefix: c.Prefix,
			plain: strings.Replace(c.Text(), c.Filler, printed(c.Filler), 1)})
		if c.Filler == "\x00" && c.Prefix == "" && strings.HasPrefix(c.Text(), "\x00") {
			form, _, _ := strings.Cut(c.Name, " filler ")
			bases[form] = c
		}
	}
	forms := make([]string, 0, len(bases))
	for form := range bases {
		forms = append(forms, form)
	}
	slices.Sort(forms)
	for _, form := range forms {
		c := bases[form]
		plain := strings.TrimPrefix(c.Text(), "\x00")
		if strings.Count(plain, c.Probe) != 1 {
			continue
		}
		p := c.Probe
		for _, f := range symptomControls {
			add := func(where, secret string) {
				out = append(out, symptomInput{name: fmt.Sprintf("%s, %q %s", form, f, where),
					text: strings.Replace(plain, p, secret, 1), probe: p, plain: plain})
			}
			add("around the secret", f+p+f)
			for k := 1; k < len(p); k++ {
				add(fmt.Sprintf("inside the secret at %d", k), p[:k]+f+p[k:])
			}
			add("around and inside the secret", f+p[:len(p)/2]+f+p[len(p)/2:]+f)
		}
	}
	return out
}

// symptomPath is one way text from the cluster reaches an Incident or a person.
type symptomPath struct {
	name string
	text func(note string) string
}

// symptomPaths are the paths of the collector that store or show untrusted text: the symptom of an event or a
// pod-status observation as the Incident spec holds it, a module's evidence value, and the symptom of a module
// observation built from that evidence, as the Incident spec holds it. Condition messages of an Incident are fixed
// texts and hold nothing from the cluster.
var symptomPaths = []symptomPath{
	{"event symptom in spec.symptom", func(note string) string {
		return specOf(collector.Observation{Symptom: fingerprint.Symptom(fingerprint.Observation{
			Note: note, Source: fingerprint.SourceEvent, Reason: "BackOff", Type: "Warning",
			ReportingController: "kubelet", RegardingKind: "Pod", RegardingName: "web-abc-1",
		})}).Symptom
	}},
	{"module evidence value", modules.BoundValue},
	{"module symptom in spec.symptom", func(note string) string {
		return specOf(collector.Observation{Symptom: collector.SymptomText(modules.Symptom{
			Reason: "ScaledObjectNotReady", Summary: "The ScaledObject is not ready",
			Evidence: []modules.Evidence{{Field: "status.conditions.Ready.message", Value: modules.BoundValue(note)}},
		})}).Symptom
	}},
}

// symptomRaceStride is the share of the corpus checked under -race: one input in 16. The race detector makes each
// redaction 10 to 20 times slower, and the whole corpus took about 110 s there; without -race it takes about 8 s.
const symptomRaceStride = 16

// symptomRaceStrideOf is symptomRaceStride under -race, and 1 without it.
func symptomRaceStrideOf() int {
	if redacttest.RaceDetector {
		return symptomRaceStride
	}
	return 1
}

func TestStoredSymptomOmitsSecretsSplitByControls(t *testing.T) {
	corpus := symptomCorpus()
	if len(corpus) < 1000 {
		t.Fatalf("the corpus has %d inputs, too few to cover the gate's forms", len(corpus))
	}
	for _, p := range symptomPaths {
		t.Run(p.name, func(t *testing.T) {
			redacttest.ParallelUnderRace(t)
			leaks, checked := 0, 0
			hides := map[string]bool{} // by plain text: whether the path hides its secret
			for i, in := range corpus {
				if redacttest.RaceDetector && i%symptomRaceStride != 0 {
					continue
				}
				hidden, seen := hides[in.plain]
				if !seen {
					hidden = redacttest.VisibleGrams(p.text(in.plain), in.probe) == 0
					hides[in.plain] = hidden
				}
				if !hidden {
					continue // the path does not hide this form without controls either
				}
				checked++
				out := p.text(in.text)
				if redacttest.VisibleGrams(out, in.probe) != 0 {
					if leaks++; leaks <= 10 {
						t.Errorf("%s (%q): shows the secret in %q", in.name, in.text, out)
					}
				}
				if in.prefix != "" && !strings.Contains(out, strings.TrimSpace(in.prefix)) {
					t.Errorf("%s (%q): lost the text before the secret: %q", in.name, in.text, out)
				}
			}
			if want := 1000 / symptomRaceStrideOf(); checked < want {
				t.Errorf("only %d of %d inputs hold a form the path hides without controls", checked, len(corpus))
			}
			if leaks > 0 {
				t.Errorf("%d of %d inputs show the secret", leaks, checked)
			}
		})
	}
}
