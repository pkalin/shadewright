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
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// A corpus of generated metric blocks, built whole (#873, second round): whatever makes Build cheaper must not
// change one byte of the context. Each case answers every metric template with one to three series whose labels
// are drawn from four kinds, so the answer to "which values are secrets" is known by construction:
//
//   - a secret: a label whose value is a GitHub token ("ghp_" and 36 letters and digits), which no context may show;
//   - a plain label (app=web), which a series without the other kinds shows as it is;
//   - a long key: a label name of key words glued with separators, look-alike colons and invisible or invalid
//     characters, repeated up to a few KiB (the shape of FuzzDiffBuild input 81e201e983440dcc), its answer
//     unknown, so only the equality holds for it;
//   - a block: a value of lines under a short key, each one column deeper (the staircase of #873).
//
// A case may give the same labels to every template, as that input did. The context of each case is compared
// with the table buildCorpusTable, written from the build before the change.

// buildCorpusTable holds, for each case of the corpus in order, the SHA-256 of the text of every block of its
// context. It was written by TestWriteBuildCorpusTable at 842c2015 (the head of #873 before its second round)
// with BUILD_CORPUS_TABLE set to the output path; regenerate it only from that commit.
const buildCorpusTable = "testdata/build-corpus.sha256"

// buildCorpusCases is the number of generated cases.
const buildCorpusCases = 240

// buildCase is one generated case: the series each template answers, the secrets it holds and the plain values a
// series without a secret, a long key or a block shows.
type buildCase struct {
	series  map[string][]providers.Series
	secrets []string
	plain   []string
}

// corpusToken is a key word and a secret key name of the corpus.
const corpusToken = "token"

// corpusKeyWords and corpusGlue make the long keys: key words, and what may stand between them.
var (
	corpusKeyWords = []string{"aws_secret_access_key", "password", corpusToken, "api-key", "auth", "pass", "secretName", "x"}
	corpusGlue     = []string{"", "", "", ":", "=", " ", "\t", "\"", ",", "{", "|", "\ufe55", "\uff1a", "\ufeff", "\u200b", "\xbf", "\n"}
	corpusSecrets  = []string{"password", corpusToken, "api_key", "client-secret", "aws_secret_access_key"}
	alnum          = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// buildCorpus generates the cases, the same on every run.
func buildCorpus() []buildCase {
	rng := rand.New(rand.NewPCG(873, 2)) // #nosec G404 -- a seeded corpus, reproducible by design
	word := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteByte(alnum[rng.IntN(len(alnum))])
		}
		return b.String()
	}
	longKey := func() string {
		var unit strings.Builder
		for range 1 + rng.IntN(4) {
			unit.WriteString(corpusKeyWords[rng.IntN(len(corpusKeyWords))])
			unit.WriteString(corpusGlue[rng.IntN(len(corpusGlue))])
		}
		size := 64 << rng.IntN(6) // 64 bytes to 2 KiB
		return strings.Repeat(unit.String(), size/unit.Len()+1) + corpusGlue[rng.IntN(len(corpusGlue))]
	}
	staircase := func() string {
		var b strings.Builder
		for i := range 2 + rng.IntN(30) {
			b.WriteString(strings.Repeat(" ", i) + corpusKeyWords[rng.IntN(len(corpusKeyWords))] + ": |\n")
		}
		return b.String()
	}
	cases := make([]buildCase, buildCorpusCases)
	for i := range cases {
		c := buildCase{series: map[string][]providers.Series{}}
		same := rng.IntN(2) == 0
		var shared []map[string]string
		for _, p := range metricPlans {
			if p.hpa {
				continue
			}
			if same && shared != nil {
				ss := make([]providers.Series, 0, len(shared))
				for _, l := range shared {
					ss = append(ss, providers.Series{Labels: l, Samples: []providers.Sample{{Time: testNow, Value: 1}}})
				}
				c.series[p.template] = ss
				continue
			}
			n := 1 + rng.IntN(3)
			labels := make([]map[string]string, 0, n)
			for j := range n {
				l := map[string]string{labelPod: fmt.Sprintf("%s-%d", crashPod, j)}
				clean := true
				for range 1 + rng.IntN(3) {
					switch rng.IntN(4) {
					case 0:
						secret := "ghp_" + word(36)
						l[corpusSecrets[rng.IntN(len(corpusSecrets))]] = secret
						c.secrets = append(c.secrets, secret)
						clean = false
					case 1:
						l["app"] = "web-" + strings.ToLower(word(4))
					case 2:
						l[longKey()] = word(rng.IntN(8))
						clean = false
					default:
						l["note"] = staircase()
						clean = false
					}
				}
				if clean && l["app"] != "" {
					c.plain = append(c.plain, l["app"])
				}
				labels = append(labels, l)
			}
			ss := make([]providers.Series, 0, len(labels))
			for _, l := range labels {
				ss = append(ss, providers.Series{Labels: l, Samples: []providers.Sample{{Time: testNow, Value: 1}}})
			}
			c.series[p.template] = ss
			shared = labels
		}
		cases[i] = c
	}
	return cases
}

// buildCaseText builds the context of one case and returns the text of its blocks.
func buildCaseText(t testing.TB, c buildCase) string {
	s := webSources()
	for tmpl, ss := range c.series {
		s.metrics.Set(tmpl, ss)
	}
	b := newBuilder(t, reader(t, webObjects()...), s, nil)
	return contextText(build(t, b, incident(deploymentRef())))
}

func textHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// TestWriteBuildCorpusTable writes buildCorpusTable from the build it runs with, when BUILD_CORPUS_TABLE names the
// output path; otherwise it is skipped.
func TestWriteBuildCorpusTable(t *testing.T) {
	path := os.Getenv("BUILD_CORPUS_TABLE")
	if path == "" {
		t.Skip("BUILD_CORPUS_TABLE is not set")
	}
	var b strings.Builder
	b.WriteString("# The SHA-256 of the context text of each case of buildCorpus, in order, written at 842c2015 (#873).\n")
	for _, c := range buildCorpus() {
		b.WriteString(textHash(buildCaseText(t, c)) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}

// Every generated metric block comes out byte for byte as the build before the second round of #873 wrote it, no
// context shows a secret of its case, and a plain value of a series without the other kinds is shown. Under -race
// it checks a seeded sample of the cases (racesample_test.go), without it all of them.
func TestBuildCorpusUnchanged(t *testing.T) {
	f, err := os.Open(buildCorpusTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var want []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			want = append(want, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	cases := buildCorpus()
	if len(want) != len(cases) {
		t.Fatalf("the table has %d rows for %d cases", len(want), len(cases))
	}
	for i, c := range cases {
		if !raceKeeps(t.Name(), i) {
			continue
		}
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			text := buildCaseText(t, c)
			if got := textHash(text); got != want[i] {
				t.Errorf("the context changed: SHA-256 %s, the table has %s", got, want[i])
			}
			for _, s := range c.secrets {
				if strings.Contains(text, s) {
					t.Errorf("the context shows the secret %q", s)
				}
			}
			for _, p := range c.plain {
				if !strings.Contains(text, p) {
					t.Errorf("the context does not show the plain value %q", p)
				}
			}
		})
	}
}
