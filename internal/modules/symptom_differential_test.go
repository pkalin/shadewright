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

package modules

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
	"github.com/shadewright-labs/shadewright/internal/testutil/mainsymptom"
)

// The stored symptom never shows what origin/main's stored symptom hides. A random differential compares the
// current fingerprint.Symptom, fingerprint.Value and BoundValue with origin/main's on inputs drawn with a fixed
// seed from a vocabulary of keys, tokens, punctuation and separators (whitespace of every kind, controls, format
// characters, CSI and other escapes, invalid bytes), joined so that a separator often stands between a key and its
// value or inside a value (symptom_inputs_test.go). An input fails when a shown output holds a four-byte part of a
// probe (redacttest.VisibleGrams) that origin/main's same output does not. The seed and the generator are those of
// the code review of 2026-11-26, which found 61 such inputs at 14435c81 (diffRegressions). Without -race every input
// runs; under -race one in diffRaceStride.
//
// origin/main's outputs come from two places. For the 61 regression inputs they are the real outputs of
// origin/main at 24cd00a6, committed in symptomMainRegressions. For the generated inputs they come from the frozen
// reading (package mainsymptom), which calls the live redact package; TestFrozenReadingIsOriginMain holds that
// reading to the real outputs of origin/main (the table and symptomMainGeneratedHash), so a change of the
// redaction rules that moves the frozen reading fails there instead of moving the baseline unseen.

// shownMoreThan returns a description of the first of now's outputs that holds a four-byte part of a probe
// main's same output does not, or "".
func shownMoreThan(now, main storedOutputs) string {
	for _, c := range []struct{ name, now, main string }{
		{"Symptom", now.symptom, main.symptom},
		{"Value", now.value, main.value},
		{"BoundValue", now.bound, main.bound},
	} {
		for _, p := range diffProbes {
			if redacttest.VisibleGrams(c.now, p)&^redacttest.VisibleGrams(c.main, p) != 0 {
				return c.name + " gives " + strconv.Quote(c.now) + ", origin/main " + strconv.Quote(c.main)
			}
		}
	}
	return ""
}

// frozenOutputs is the frozen reading of origin/main (package mainsymptom) of s.
func frozenOutputs(s string) storedOutputs {
	return storedOutputs{mainsymptom.Symptom(diffObservation(s)), mainsymptom.Value(s), mainsymptom.BoundValue(s)}
}

// frozenGenerated is frozenOutputs of every input of diffInputsOf, read once for the tests that read them all.
var frozenGenerated = sync.OnceValue(func() []storedOutputs {
	in := diffInputsOf()
	out := make([]storedOutputs, len(in))
	for i, s := range in {
		out[i] = frozenOutputs(s)
	}
	return out
})

// diffShowsMore returns a description of the first shown output of s that holds a four-byte part of a probe
// the frozen reading of origin/main's same output does not, or "".
func diffShowsMore(s string) string {
	return shownMoreThan(readOutputsHere(s), frozenOutputs(s))
}

// diffShowsMoreAt is diffShowsMore of the input of diffInputsOf of index i, s; without -race it takes the frozen
// reading from frozenGenerated.
func diffShowsMoreAt(i int, s string) string {
	if redacttest.RaceDetector {
		return diffShowsMore(s)
	}
	return shownMoreThan(readOutputsHere(s), frozenGenerated()[i])
}

// mainRegressionOutputs reads symptomMainRegressions: origin/main's real outputs on diffRegressions.
func mainRegressionOutputs(t *testing.T) []storedOutputs {
	t.Helper()
	data, err := os.ReadFile(symptomMainRegressions)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != len(diffRegressions) {
		t.Fatalf("%s has %d lines, want %d: regenerate it with hack/symptom-main-baseline.sh", symptomMainRegressions,
			len(lines), len(diffRegressions))
	}
	out := make([]storedOutputs, len(lines))
	for i, line := range lines {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[0] != strconv.Itoa(i) {
			t.Fatalf("%s line %d is malformed", symptomMainRegressions, i+1)
		}
		var v [3]string
		for k := range v {
			if v[k], err = strconv.Unquote(f[k+1]); err != nil {
				t.Fatalf("%s line %d: %v", symptomMainRegressions, i+1, err)
			}
		}
		out[i] = storedOutputs{v[0], v[1], v[2]}
	}
	return out
}

func TestStoredSymptomShowsNothingMainHides(t *testing.T) {
	in := diffInputsOf()
	more, ran := 0, 0
	for i, s := range in {
		if redacttest.RaceDetector && i%diffRaceStride != 0 {
			continue
		}
		ran++
		if d := diffShowsMoreAt(i, s); d != "" {
			if more++; more <= 10 {
				t.Errorf("input %d %q: %s", i, s, d)
			}
		}
	}
	if more > 0 {
		t.Errorf("%d of %d inputs show more of a secret than origin/main", more, ran)
	}
	t.Logf("%d inputs, %d show more than origin/main", ran, more)
}

func TestStoredSymptomRegressionsShowNothingMainHides(t *testing.T) {
	if len(diffRegressions) != 61 {
		t.Fatalf("%d regression inputs, want 61", len(diffRegressions))
	}
	onMain := mainRegressionOutputs(t)
	for i, s := range diffRegressions {
		if d := shownMoreThan(readOutputsHere(s), onMain[i]); d != "" {
			t.Errorf("%q: %s", s, d)
		}
	}
}

// TestFrozenReadingIsOriginMain holds the frozen reading (package mainsymptom) to origin/main's real outputs at
// 24cd00a6: byte for byte on the 61 regression inputs, and by one SHA-256 over every generated input. The frozen
// reading calls the live redact package, so a change of a redaction rule that changes what origin/main stored
// fails here. The hash needs every input, so under -race only the table is checked.
func TestFrozenReadingIsOriginMain(t *testing.T) {
	onMain := mainRegressionOutputs(t)
	for i, s := range diffRegressions {
		if got := frozenOutputs(s); got != onMain[i] {
			t.Errorf("regression %d %q: frozen reading gives %s, origin/main %s", i, s, storedOutputsRow(i, got),
				storedOutputsRow(i, onMain[i]))
		}
	}
	if redacttest.RaceDetector {
		t.Skip("the hash of the generated inputs is checked without -race")
	}
	data, err := os.ReadFile(symptomMainGeneratedHash)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(string(data))
	if got := symptomOutputsHash(frozenGenerated()); got != want {
		t.Errorf("the frozen reading of the %d generated inputs hashes to %s, origin/main's outputs to %s", diffInputs,
			got, want)
	}
}
