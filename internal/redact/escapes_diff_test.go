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

package redact

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// spansBeforeEscapes is spans as origin/main read a text before the escapes view (#1223): the readings of the text
// as it is, then its controls view and its shown view, and nothing else. It is the old logic kept for the
// differential below.
func (r *Redactor) spansBeforeEscapes(wm *meter.Meter, s string) []span {
	l := r.mainMatches(wm, s)
	for _, view := range []func() (controlsView, bool){
		func() (controlsView, bool) { return controlsViewOf(wm, s) },
		func() (controlsView, bool) { return shownViewOf(wm, s) },
	} {
		v, ok := view()
		if !ok {
			continue
		}
		l.merge(wm)
		r.eachControlsMatch(wm, v, func(start, end, rule int) {
			if !l.covers(wm, start, end) {
				l.add(wm, start, end, rule)
			}
		})
	}
	return settle(wm, s, l.spans(wm))
}

// escapesDiffEvery is the share of the block corpus the differential reads: one input in it.
const escapesDiffEvery = 5

// escapesDiffInputs are the inputs of the differential: every sample and every input of the block corpus, each as
// it is and with escape sequences around and inside its key and value, and two secrets side by side with each glue
// and a sequence between them, so that a match of the escapes view that would take the start of its neighbour shows.
func escapesDiffInputs() []string {
	forms := []string{"\x1b[36m", "\x1b[0m", "\x1b", "\x1b]0;t\x07", "\x1b(B", "\u009b1m", "\x1b[3"}
	var out []string
	for _, s := range redacttest.Samples() {
		out = append(out, s.Text)
		k := strings.Index(s.Text, s.Key+s.Secret)
		if k < 0 {
			continue
		}
		vs := k + len(s.Key)
		for _, f := range forms {
			for _, at := range []int{k, vs - 1, vs, vs + len(s.Secret)/2, vs + len(s.Secret)} {
				if at <= 0 || at > len(s.Text) {
					continue
				}
				out = append(out, s.Text[:at]+f+s.Text[at:])
			}
		}
	}
	for i, c := range redacttest.EntryCorpus() {
		if i%escapesDiffEvery == 0 {
			out = append(out, c.In, "\x1b[36m"+c.In+"\x1b[0m", strings.ReplaceAll(c.In, "=", "\x1b[0m="))
		}
	}
	for _, c := range redacttest.GateCases() {
		out = append(out, c.Text())
	}
	for _, glue := range []string{"", " ", ",", "&", ";", `"`, `", "`, "\t", "\n"} {
		for _, f := range forms {
			for _, second := range []string{"api_key=", `"token": "`, "Authorization: Bearer ", "ghp_"} {
				out = append(out,
					"password=hunter2xyzzy"+f+glue+second+"Zq7Kp2Vx9Lm4Wt8RabcdEFGH1234567890ab",
					"password="+f+"hunter2xyzzy"+glue+f+second+"Zq7Kp2Vx9Lm4Wt8RabcdEFGH1234567890ab",
					"pass"+f+"word=hunter2xyzzy"+glue+second+f+"Zq7Kp2Vx9Lm4Wt8RabcdEFGH1234567890ab")
			}
		}
	}
	return out
}

// The escapes view only adds to what is hidden (#1223): on every input of the differential, every byte the readings
// of origin/main (spansBeforeEscapes) hide is hidden, and an input without an ESC or a C1 control is decided exactly
// as before.
func TestEscapesViewHidesAtLeastWhatMainHid(t *testing.T) {
	inputs := escapesDiffInputs()
	if len(inputs) < 2000 {
		t.Fatalf("only %d inputs", len(inputs))
	}
	lost, same, more := 0, 0, 0
	for _, in := range inputs {
		before := defaultRedactor.spansBeforeEscapes(nil, in)
		now := defaultRedactor.spans(nil, in)
		hidden := make([]bool, len(in))
		for _, sp := range now {
			for k := sp.start; k < sp.end; k++ {
				hidden[k] = true
			}
		}
		lostHere := false
		for _, sp := range before {
			for k := sp.start; k < sp.end; k++ {
				if !hidden[k] {
					lostHere = true
				}
			}
		}
		if lostHere {
			if lost++; lost <= 5 {
				t.Errorf("%q: the escapes view hides less than main:\n main %q\n  now %q", in,
					render(nil, in, before), render(nil, in, now))
			}
		}
		if render(nil, in, before) == render(nil, in, now) {
			same++
		} else {
			more++
			if !holdsEscape(nil, in) {
				t.Errorf("%q holds no ESC or C1 control and is decided otherwise than by main", in)
			}
		}
	}
	if lost > 0 {
		t.Errorf("%d of %d inputs lose a hidden byte", lost, len(inputs))
	}
	if more == 0 {
		t.Fatal("no input is decided otherwise: the differential reads no escape")
	}
	t.Logf("%d inputs: %d decided as main, %d hide more", len(inputs), same, more)
}
