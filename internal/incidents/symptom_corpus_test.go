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

	"github.com/shadewright-labs/shadewright/internal/analyzer/collector"
	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
	"github.com/shadewright-labs/shadewright/internal/modules"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// The symptom an Incident stores, and the module evidence it is built from, never show a credential that a
// control, an escape sequence, an invisible character, an invalid byte or whitespace other than one ASCII space
// splits or hides. The inputs are generated: every secret form of redacttest.ShownForms with each filler of the
// controls gate (redacttest.GateCases) at each boundary of its parts, and with each separator of
// symptomSeparators around its secret, inside it at every byte, or both, alone and after "log: ". The oracle does
// not depend on the matchers (redacttest.ShowsProbe): the output is squeezed to the characters a credential is
// made of and must not hold the probe, squeezed the same way, nor any four-byte part of it. No row is skipped for
// what a path does with its plain form today; instead every form of redacttest.ShownForms must be hidden in its
// plain form, and the test fails when one is not. Out of scope by construction: a credential split by plain ASCII
// spaces alone, which the printed text shows as it is.

// withheldInPlainForm are the forms of redacttest.ShownForms whose plain form holds a line end (a YAML block
// scalar under a secret key). The printed text is one line, and a value it holds after a run of whitespace other than one ASCII
// space is withheld, never shown, so these must be hidden but may be withheld whole.
var withheldInPlainForm = []string{"block"}

// symptomSeparators are what the generated rows place around and inside each secret, one of each kind the printed
// text removes, collapses or withholds: C0 and C1 controls, DEL and backspace, complete escape sequences of every
// kind (CSI, OSC with BEL and with ST, DCS, SOS, PM, APC, an escape with an intermediate byte, two-byte escapes, the
// C1 CSI and OSC), escapes that take the next byte or are not terminated, invisible characters (zero-width,
// bidirectional, tag, variation selector, soft hyphen, word joiner, BOM), whitespace other than one ASCII space
// (tab, CR, LF, CRLF, NEL, no-break and wide spaces, the line separator, two ASCII spaces) and invalid bytes.
var symptomSeparators = []string{
	"\x00", "\x01", "\x07", "\b", "\x1b", "\x7f", "\u0080", "\u009c",
	"\x1b[31m", "\x1b[0m", "\x1b[1;31m", "\x1b[m", "\x1b[", "\u009b31m", "\u009b",
	"\x1b]0;title\x07", "\x1b]8;;http://x\x1b\\", "\u009d0;t\u009c", "\x1bPq#0\x1b\\", "\x1bXs\x1b\\",
	"\x1b^p\x1b\\", "\x1b_a\x1b\\", "\x1b]0;open", "\x1b(B", "\x1b7", "\x1b=", "\x1b ",
	"\u200b", "\u200c", "\u200d", "\u2060", "\ufeff", "\u202e", "\u2066", "\U000E0041", "\ufe0f", "\u00ad",
	"\t", "\r", "\n", "\r\n", "\v", "\f", "\u0085", "\u00a0", "\u2003", "\u3000", "\u2028", "  ",
	"\xff", "\xc3", "\x80",
}

// symptomPrefixes are the texts each generated row is placed after.
var symptomPrefixes = []string{"", "log: "}

// symptomInput is one generated note and the secret it holds.
type symptomInput struct {
	name, text, probe string
	// sample is set for the rows checked under -race (symptomRaceSample).
	sample bool
	// filler and prefix are the separator placed and the text before the form.
	filler, prefix string
}

// symptomForm is a form of redacttest.ShownForms in plain form.
type symptomForm struct {
	name, plain, probe string
}

// symptomForms returns the forms of redacttest.ShownForms as the gate builds them without a filler, with " ok"
// after them.
func symptomForms(t *testing.T) []symptomForm {
	byName := map[string]symptomForm{}
	for _, c := range redacttest.GateCases() {
		if c.Filler == "\x00" && c.Prefix == "" && strings.HasPrefix(c.Text(), "\x00") {
			form := c.Form()
			byName[form] = symptomForm{name: form, plain: strings.TrimPrefix(c.Text(), "\x00"), probe: c.Probe}
		}
	}
	out := make([]symptomForm, 0, len(redacttest.ShownForms))
	for _, name := range redacttest.ShownForms {
		f, ok := byName[name]
		if !ok {
			t.Fatalf("the controls gate has no form %q", name)
		}
		out = append(out, f)
	}
	return out
}

// symptomCorpus returns the generated inputs.
func symptomCorpus(t *testing.T) []symptomInput {
	forms := symptomForms(t)
	var out []symptomInput
	gate := 0
	for _, c := range redacttest.GateCases() {
		if !c.ShownInScope() {
			continue
		}
		out = append(out, symptomInput{name: c.Name, text: c.Text(), probe: c.Probe, sample: gate%symptomGateStride == 0,
			filler: c.Filler, prefix: c.Prefix})
		gate++
	}
	for fi, f := range forms {
		if strings.Count(f.plain, f.probe) != 1 {
			t.Fatalf("form %q holds its probe %d times", f.name, strings.Count(f.plain, f.probe))
		}
		p := f.probe
		for si, sep := range symptomSeparators {
			for pi, prefix := range symptomPrefixes {
				add := func(where, secret string, sample bool) {
					out = append(out, symptomInput{name: fmt.Sprintf("%s %q, %q %s", f.name, prefix, sep, where),
						text: prefix + strings.Replace(f.plain, p, secret, 1), probe: p, sample: sample, filler: sep,
						prefix: prefix})
				}
				// Under -race one form in three, turning with the separator and the prefix, gives one row of its
				// group: around the secret (row 0), inside it at byte k (row k) or around and inside it (the last).
				pick := -1
				if (fi+si+pi)%3 == 0 {
					pick = (si*len(symptomPrefixes) + pi + fi) % (len(p) + 1)
				}
				add("around the secret", sep+p+sep, pick == 0)
				for k := 1; k < len(p); k++ {
					add(fmt.Sprintf("inside the secret at %d", k), p[:k]+sep+p[k:], pick == k)
				}
				add("around and inside the secret", sep+p[:len(p)/2]+sep+p[len(p)/2:]+sep, pick == len(p))
			}
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

// Every form of redacttest.ShownForms is hidden by every shown path in its plain form, and the text before it is
// kept, so the form is recognised, not withheld (but those of withheldInPlainForm). A matcher that stops
// recognising one fails here, not by skipping its rows.
func TestShownFormsAreRecognisedInPlainForm(t *testing.T) {
	for _, f := range symptomForms(t) {
		for _, p := range symptomPaths {
			out := p.text("log: " + f.plain)
			if redacttest.ShowsProbe(out, f.probe) {
				t.Errorf("%s: %s shows the secret of the plain form %q: %q", p.name, f.name, f.plain, out)
			}
			if !strings.Contains(out, "log:") && !slices.Contains(withheldInPlainForm, f.name) {
				t.Errorf("%s: %s lost the text before the plain form %q: %q", p.name, f.name, f.plain, out)
			}
		}
	}
}

// Terminal strings are not interpreted: a note that holds one, in any encoding, is withheld whole by the event
// symptom and the module evidence value, so nothing of its payload or of the text around it is stored.
func TestTerminalStringsWithholdTheStoredSymptom(t *testing.T) {
	for _, note := range []string{"\x9dtitle\x07ready", "\x9dtitle", "x\u009done\u009dtwo\u009crest\u009cz"} {
		sym := fingerprint.Symptom(fingerprint.Observation{Note: note, Source: fingerprint.SourceEvent})
		if sym != "<redacted>" {
			t.Errorf("Symptom(%q) = %q, want <redacted>", note, sym)
		}
		if v := modules.BoundValue(note); v != "<redacted>" {
			t.Errorf("BoundValue(%q) = %q, want <redacted>", note, v)
		}
	}
}

// symptomGateStride is the share of the gate's rows checked under -race: one in symptomGateStride.
const symptomGateStride = 7

// symptomRaceSample returns the rows checked under -race, a stratified sample: every separator with every prefix,
// for one form in three (turning with the separator and the prefix, so every form is taken), at one place that
// turns from group to group (around the secret, inside it at a byte, or both), and one in symptomGateStride of the
// gate's rows. The race detector makes each redaction 10 to 20 times slower: the
// whole corpus takes about 30 s without it and several minutes with it. Without -race every row is checked. It
// fails when the sample leaves out a separator or filler of the corpus with a prefix the corpus has it with.
func symptomRaceSample(t *testing.T, corpus []symptomInput) []symptomInput {
	if !redacttest.RaceDetector {
		return corpus
	}
	var out []symptomInput
	seen := map[[2]string]bool{}
	for _, in := range corpus {
		if in.sample {
			out = append(out, in)
			seen[[2]string{in.filler, in.prefix}] = true
		}
	}
	for _, in := range corpus {
		if !seen[[2]string{in.filler, in.prefix}] {
			t.Errorf("the sample under -race leaves out the separator %q after %q", in.filler, in.prefix)
			seen[[2]string{in.filler, in.prefix}] = true
		}
	}
	return out
}

func TestStoredSymptomOmitsSecretsSplitByControls(t *testing.T) {
	corpus := symptomCorpus(t)
	if len(corpus) < 20000 {
		t.Fatalf("the corpus has %d inputs, too few to cover the forms and separators", len(corpus))
	}
	corpus = symptomRaceSample(t, corpus)
	for _, p := range symptomPaths {
		t.Run(p.name, func(t *testing.T) {
			redacttest.ParallelUnderRace(t)
			leaks := 0
			for _, in := range corpus {
				if out := p.text(in.text); redacttest.ShowsProbe(out, in.probe) {
					if leaks++; leaks <= 10 {
						t.Errorf("%s (%q): shows the secret in %q", in.name, in.text, out)
					}
				}
			}
			if leaks > 0 {
				t.Errorf("%d of %d inputs show the secret", leaks, len(corpus))
			}
		})
	}
}
