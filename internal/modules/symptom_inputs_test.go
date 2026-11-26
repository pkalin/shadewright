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
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
)

// The inputs of the stored-symptom differential (symptom_differential_test.go) and the writer of origin/main's
// outputs on them. This file uses only what origin/main at 24cd00a6 has (fingerprint.Symptom, fingerprint.Value,
// BoundValue, collectorRegardingKind), so that hack/symptom-main-baseline.sh can copy it into an archive of that
// commit and run TestWriteSymptomMainBaseline there. Regenerating the baseline is a reviewed decision: the files it
// writes are what the frozen reading (package mainsymptom) is held to.

// diffSecret is the probe the generator places in every input.
const diffSecret = "Zq8rT2vX9mK4pL7w" // #nosec G101 -- a made-up value the test looks for

// diffTokenBody is the body of the GitHub token of the vocabulary, written apart from its prefix so that no
// line holds a whole token shape.
const diffTokenBody = "aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3z" // #nosec G101 -- a made-up token the test looks for

// diffReason is the reason of the observations of the differential.
const diffReason = "BackOff"

// diffProbes are the secrets an input may hold: diffSecret, the body of the made-up GitHub token and the made-up
// AWS key body of the vocabulary.
var diffProbes = []string{diffSecret, diffTokenBody, "IOSFODNN7EXAMPLE"}

// diffVocab is the vocabulary of the generator.
var diffVocab = []string{
	"password", "--password", "pass", "word", "token", "secret", "DB_PASSWORD", "api_key", "Authorization:", "Bearer",
	"ghp_", "ghp_" + diffTokenBody, "postgres://", "u:", "@db.example/app", "AKIA", "IOSFODNN7EXAMPLE",
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
	"ghp_" + diffTokenBody + "@" + "db.example/app=Bearer\x012vX9mK4pL7w},\xc3\"",
	"=\npassword\u009b1m--passwordmain ,failed" + "@db.example/appZq8rT2vX9mK4pL7w",
	"-api_key\u200bBearer[abcdef0123456789abcdef0123456789abcdef01x>Zq8rT2vX9mK4pL7w",
	"main\x1b[5 qmain[10.0.0.1ok@sha1:--password\u00adZq8rT",
}

// symptomMainRegressions holds origin/main's stored outputs at 24cd00a6 for each of diffRegressions, in order: the
// index, then Symptom, Value and BoundValue, each quoted, tab separated. symptomMainGeneratedHash holds the SHA-256 of
// origin/main's outputs on every input of diffInputsOf (symptomOutputsHash). Both were written by
// TestWriteSymptomMainBaseline in an archive of that commit (hack/symptom-main-baseline.sh).
const (
	symptomMainRegressions   = "testdata/symptom-main-regressions.tsv"
	symptomMainGeneratedHash = "testdata/symptom-main-generated.sha256"
)

// diffObservation is the observation of the differential whose note is s.
func diffObservation(s string) fingerprint.Observation {
	return fingerprint.Observation{Note: s, Reason: diffReason, Type: "Warning", RegardingKind: collectorRegardingKind,
		RegardingName: "web-abc-1"}
}

// storedOutputs is what a reading stores for an input: its Symptom, Value and BoundValue.
type storedOutputs struct{ symptom, value, bound string }

// storedOutputsRow is the line of o in symptomMainRegressions for the input of index i.
func storedOutputsRow(i int, o storedOutputs) string {
	return strconv.Itoa(i) + "\t" + strconv.Quote(o.symptom) + "\t" + strconv.Quote(o.value) + "\t" +
		strconv.Quote(o.bound)
}

// symptomOutputsHash returns the hex SHA-256 over outs, the outputs of the inputs in order, each output quoted
// and ended by a newline.
func symptomOutputsHash(outs []storedOutputs) string {
	h := sha256.New()
	w := bufio.NewWriter(h)
	for _, o := range outs {
		for _, v := range []string{o.symptom, o.value, o.bound} {
			_, _ = w.WriteString(strconv.Quote(v))
			_ = w.WriteByte('\n')
		}
	}
	_ = w.Flush()
	return hex.EncodeToString(h.Sum(nil))
}

// readOutputsHere is the reading of the build the test runs in: origin/main's when run by
// hack/symptom-main-baseline.sh.
func readOutputsHere(s string) storedOutputs {
	return storedOutputs{fingerprint.Symptom(diffObservation(s)), fingerprint.Value(s), BoundValue(s)}
}

// TestWriteSymptomMainBaseline writes symptomMainRegressions and symptomMainGeneratedHash under the directory
// SYMPTOM_MAIN_BASELINE names, from the reading of the build it runs in; without it, it is skipped. Run it only
// through hack/symptom-main-baseline.sh, which runs it in an archive of origin/main at 24cd00a6.
func TestWriteSymptomMainBaseline(t *testing.T) {
	dir := os.Getenv("SYMPTOM_MAIN_BASELINE")
	if dir == "" {
		t.Skip("SYMPTOM_MAIN_BASELINE is not set")
	}
	var b strings.Builder
	for i, s := range diffRegressions {
		b.WriteString(storedOutputsRow(i, readOutputsHere(s)) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(symptomMainRegressions)), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	in := diffInputsOf()
	outs := make([]storedOutputs, len(in))
	for i, s := range in {
		outs[i] = readOutputsHere(s)
	}
	sum := symptomOutputsHash(outs) + "\n"
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(symptomMainGeneratedHash)), []byte(sum), 0o600); err != nil {
		t.Fatal(err)
	}
}
