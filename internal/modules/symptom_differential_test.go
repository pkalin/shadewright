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
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
	"github.com/shadewright-labs/shadewright/internal/testutil/mainsymptom"
)

// The stored symptom never shows what origin/main's stored symptom hides. A random differential compares the
// current fingerprint.Symptom, fingerprint.Value and BoundValue with origin/main's, frozen in package
// mainsymptom, on inputs drawn with a fixed seed from a vocabulary of keys, tokens, punctuation and separators
// (whitespace of every kind, controls, format characters, CSI and other escapes, invalid bytes), joined so that a
// separator often stands between a key and its value or inside a value. An input fails when a shown output holds a
// four-byte part of a probe (redacttest.VisibleGrams) that origin/main's same output does not. The seed and the
// generator are those of the code review of 2026-11-26, which found 61 such inputs at 14435c81
// (diffRegressions). Without -race every input runs; under -race one in diffRaceStride.

// diffSecret is the probe the generator places in every input.
const diffSecret = "Zq8rT2vX9mK4pL7w" // #nosec G101 -- a made-up value the test looks for

// diffReason is the reason of the observations of the differential.
const diffReason = "BackOff"

// diffProbes are the secrets an input may hold: diffSecret, the body of the made-up GitHub token and the made-up
// AWS key body of the vocabulary.
var diffProbes = []string{diffSecret, "aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3z", "IOSFODNN7EXAMPLE"}

// diffVocab is the vocabulary of the generator.
var diffVocab = []string{
	"password", "--password", "pass", "word", "token", "secret", "DB_PASSWORD", "api_key", "Authorization:", "Bearer",
	"ghp_", "ghp_aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3z", "postgres://", "u:", "@db.example/app", "AKIA", "IOSFODNN7EXAMPLE",
	diffSecret, diffSecret[:5], diffSecret[5:], diffSecret[:9], diffSecret[9:],
	"=", ":", " ", "  ", "\n", "\n  ", "\r", "\r\n", "\t", "\u00a0", "\u0085", "\u2028", "\u200b", "\u00ad", "\x00", "\x01",
	"\x1b[31m", "\x1b[0m", "\x1b[5 q", "\u009b1m", "\x1b", "\x1b]0;t\x07", "\b", "\x7f", "\xff", "\xc3", "\x85", "\x9b",
	"|", ">", "\"", "'", "{", "}", ",", "-", "log: ", "x", "value:", "[", "]", "<redacted>", "@sha1:", "main",
	"1.2.3.4", "10.0.0.1", "ok", "failed", "abcdef0123456789abcdef0123456789abcdef01",
}

const (
	// diffSeed and diffInputs are the seed and the number of inputs of the differential.
	diffSeed   = 7
	diffInputs = 60000
	// diffRaceStride is the share of the inputs checked under -race: one in diffRaceStride.
	diffRaceStride = 20
)

// diffInputsOf returns the inputs of the differential, the same on every run.
func diffInputsOf() []string {
	r := rand.New(rand.NewSource(diffSeed)) // #nosec G404 -- a fixed seed for reproducible test inputs
	out := make([]string, 0, diffInputs)
	for range diffInputs {
		var b strings.Builder
		k := 2 + r.Intn(9)
		for range k {
			b.WriteString(diffVocab[r.Intn(len(diffVocab))])
		}
		s := b.String()
		if !strings.Contains(s, diffSecret[:5]) && !strings.Contains(s, diffSecret[5:]) && !strings.Contains(s, diffSecret[9:]) {
			s += diffSecret
		}
		out = append(out, s)
	}
	return out
}

// diffShowsMore returns a description of the first shown output of s that holds a four-byte part of a probe
// origin/main's same output does not, or "".
func diffShowsMore(s string) string {
	o := fingerprint.Observation{Note: s, Reason: diffReason, Type: "Warning", RegardingKind: collectorRegardingKind,
		RegardingName: "web-abc-1"}
	for _, c := range []struct{ name, now, main string }{
		{"Symptom", fingerprint.Symptom(o), mainsymptom.Symptom(o)},
		{"Value", fingerprint.Value(s), mainsymptom.Value(s)},
		{"BoundValue", BoundValue(s), mainsymptom.BoundValue(s)},
	} {
		for _, p := range diffProbes {
			if redacttest.VisibleGrams(c.now, p)&^redacttest.VisibleGrams(c.main, p) != 0 {
				return c.name + " gives " + strconv.Quote(c.now) + ", origin/main " + strconv.Quote(c.main)
			}
		}
	}
	return ""
}

func TestStoredSymptomShowsNothingMainHides(t *testing.T) {
	in := diffInputsOf()
	more, ran := 0, 0
	for i, s := range in {
		if redacttest.RaceDetector && i%diffRaceStride != 0 {
			continue
		}
		ran++
		if d := diffShowsMore(s); d != "" {
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

// diffRegressions are the 61 inputs of the code review of 2026-11-26 that showed more than origin/main at
// 14435c81: a key and its value, or two parts of a value, glued once a control, a format character or a CSI
// between them was removed.
var diffRegressions = []string{
	"\x00word\xc3Authorization:\t\n\u00adZq8rT2vX9mK4pL7w",
	"ok\r\n--password\x00Zq8rT\u0085",
	"--password\u00ad|Zq8rT2vX9x}x|",
	"\u00a0api_key\u200bZq8rT2vX9mK4pL7w",
	"--password\u009b1mIOSFODNN7EXAMPLE10.0.0.1Zq8rT2vX9mK4pL7w",
	"Bearer\x01Zq8rT2vX9mK4pL7w",
	"token \u009b1mapi_keyAKIA}Bearer\x01Zq8rT2vX9mK4pL7w",
	"api_key\u00adZq8rT2vX9mK4pL7w",
	"value:password1.2.3.4AKIA\u00adZq8rT2vX9mK4pL7w",
	"DB_PASSWORD\x01secretpostgres://<redacted>secretDB_PASSWORDxlog: \xc3Zq8rT2vX9mK4pL7w",
	"\u00ad,\"--password\u009b1m}|:@sha1:10.0.0.1Zq8rT2vX9mK4pL7w",
	"api_key\x01Zq8rT2vX9mK4pL7w",
	"\u0085  Zq8rT\u200b2vX9mK4pL7w{\u0085\u00ad10.0.0.1word",
	"\n\u200b[AKIA<redacted>\u2028Bearer\u200bZq8rT2vX9mK4pL7w",
	"value:api_key\x00Zq8rT2vX9mK4pL7w",
	"Bearer\x01\x00mK4pL7wZq8rT",
	"ghp_log: Bearer\u2028word\"ghp_|api_key\x01Zq8rT2vX9mK4pL7w",
	"DB_PASSWORD1.2.3.4\x00Zq8rT2vX9mK4pL7w",
	"log: Bearer\u200bZq8rT2vX9mK4pL7w",
	"--passwordmain\x1b[0m>pass2vX9mK4pL7w<redacted>",
	"|\xffapi_key\u00adZq8rT2vX9mK4pL7w",
	"\x00}<redacted>Bearer\u200bZq8rT2vX9mK4pL7w",
	"{u:--password\x00Bearer@sha1:,Zq8rT2vX9Zq8rTpassword",
	"]--password\x00|wordZq8rT2vX9mK4pL7w",
	"failed\n  AKIAx}\xffapi_key\x01Zq8rT2vX9mK4pL7w",
	"api_key\u200bZq8rT2vX9mK4pL7w",
	"--password\u200b}Zq8rT2vX9mK4pL7w",
	"log: \u009b1m--password\u009b1m{abcdef0123456789abcdef0123456789abcdef01}wordZq8rT2vX9mK4pL7w",
	"Bearer\u00adZq8rT2vX9mK4pL7w",
	"api_key\x00Zq8rT2vX9mK4pL7w",
	"value:\x1b[5 qvalue:Zq8rT2vX9\u00admK4pL7w  AKIA-,",
	"\u00a0IOSFODNN7EXAMPLE api_key\x01Zq8rT2vX9mK4pL7w",
	"api_key\x00Zq8rT2vX9mK4pL7w",
	"]Zq8rT\x002vX9mK4pL7w",
	"\u00ad<redacted>--password\x1b[0m>wordZq8rT2vX9mK4pL7w",
	" \u200b--password\u00adZq8rT\r\n{",
	"abcdef0123456789abcdef0123456789abcdef01 Bearer\x00Zq8rT2vX9",
	"--password\x01-secretAKIA>Zq8rT2vX9mK4pL7w",
	"--password1.2.3.4\u009b1mZq8rT2vX9mK4pL7w",
	"\x00\u00ad\r\n--password\x1b[31m|Zq8rT2vX9mK4pL7w",
	"ok\r--password\u200b\x1b[0m{tokenZq8rT2vX9mK4pL7w",
	"DB_PASSWORD\u00ad\x1b[5 q<redacted>main>Zq8rT\xff2vX9mK4pL7w",
	"--password\u200bmK4pL7wmain\x00",
	":api_key\x00Zq8rT2vX9mK4pL7w",
	"--password\u200b2vX9mK4pL7w  ",
	"api_key\x01Zq8rT2vX9mK4pL7w",
	"\x1b[5 qokZq8rT\xc32vX9mK4pL7w@sha1:",
	"\xc3:\n@sha1:--password\u00adok}Zq8rT2vX9mK4pL7w",
	"- [Bearer\x00Zq8rT2vX9mK4pL7w",
	"\x1b[0m--password\u200b>,:Zq8rT2vX9",
	"1.2.3.4token\t--password\x00|{Zq8rT2vX9mK4pL7w",
	"\xff[<redacted>Bearer\x01Zq8rT2vX9mK4pL7w",
	"Bearer\x01\x00ghp_10.0.0.1Zq8rT2vX9mK4pL7w",
	"\u0085api_key\x00--passwordx\u2028'secret\x1b[5 qZq8rT2vX9mK4pL7w",
	"--password\u009b1mZq8rT@sha1:",
	"u:  \n  \u200b--password\x1b[31m{]Zq8rT2vX9mK4pL7w",
	"\xc3--password\x00ok[Zq8rT2vX9mK4pL7w",
	"ghp_aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3z@db.example/app=Bearer\x012vX9mK4pL7w},\xc3\"",
	"=\npassword\u009b1m--passwordmain ,failed@db.example/appZq8rT2vX9mK4pL7w",
	"-api_key\u200bBearer[abcdef0123456789abcdef0123456789abcdef01x>Zq8rT2vX9mK4pL7w",
	"main\x1b[5 qmain[10.0.0.1ok@sha1:--password\u00adZq8rT",
}

func TestStoredSymptomRegressionsShowNothingMainHides(t *testing.T) {
	if len(diffRegressions) != 61 {
		t.Fatalf("%d regression inputs, want 61", len(diffRegressions))
	}
	for _, s := range diffRegressions {
		if d := diffShowsMore(s); d != "" {
			t.Errorf("%q: %s", s, d)
		}
	}
}
