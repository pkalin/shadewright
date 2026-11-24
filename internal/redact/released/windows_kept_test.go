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

package released

import (
	"math/rand/v2"
	"strings"
	"testing"

	v052 "github.com/shadewright-labs/shadewright/internal/redact/released/testdata/v052"
)

// #1087 keeps no part for an empty run between slashes in the reading of base64 windows (windows.go), counting
// the run in the part after it instead, so that a run of slashes holds no heap for each slash. These tests check
// that the spans stay those of the release before on generated texts of slash-split parts with runs of empty
// parts around the window bound (a window holds at most maxBase64Window/4 parts, so runs of 23 to 25 count), "="
// padding, path words and parts that read as base64; and runs and parts around the most a part counts (255).

// slashParts are the parts of the generated slash texts.
var slashParts = []string{"", "", "", "=", "==", "a", "x9", "api", "v1", "usr", "bin", "tmp", "PATH", "ABCD", "abcd",
	"Xk9q", "Lm2Z", "pQ7r", "Zz8wYk", "hunter2", "lorem", "a b", "+", "Q+", "9/"}

// slashText returns a text of parts joined by runs of slashes, some of them long runs of empty parts.
func slashText(r *rand.Rand) string {
	var b strings.Builder
	if r.IntN(3) == 0 {
		b.WriteString(pick(r, []string{"token: ", "key=", "url ", "lorem ", "\"", "path: /"}))
	}
	for range 1 + r.IntN(30) {
		switch r.IntN(8) {
		case 6:
			b.WriteString(strings.Repeat("/", 250+r.IntN(12))) // around the most a part counts (maxPartCount)
		case 7:
			b.WriteString(strings.Repeat(secretLike(r), 11)[:250+r.IntN(12)]) // a part of about that many characters
		case 0:
			b.WriteString(strings.Repeat("/", 22+r.IntN(5))) // around the window bound
		case 1:
			b.WriteString(strings.Repeat("/", 1+r.IntN(4)))
		case 2:
			b.WriteString(secretLike(r)[:4+r.IntN(16)])
		default:
			b.WriteString(pick(r, slashParts))
		}
		b.WriteByte('/')
	}
	if r.IntN(2) == 0 {
		b.WriteString(pick(r, []string{"\n", " lorem", "=", "==", "\"", ""}))
	}
	return b.String()
}

func TestSlashWindowsMatchKeptCopy(t *testing.T) {
	r := rand.New(rand.NewPCG(1087, 1)) // #nosec G404 -- reproducible generated texts
	inputs := make([]string, 0, 6000)
	for range sampled(6000) {
		inputs = append(inputs, slashText(r))
	}
	checkSameAsKept(t, "slash windows", inputs)
	hidden := 0
	for _, s := range inputs {
		if len(v052.Spans(s)) > 0 {
			hidden++
		}
	}
	// The corpus reaches the windows: a good share of its texts has something the release before hides.
	if hidden < len(inputs)/10 {
		t.Errorf("the release before hides something in %d of %d texts only", hidden, len(inputs))
	}
	t.Logf("%d of %d texts have something hidden", hidden, len(inputs))
}

// A base64 key split by slashes between runs of empty parts of every length up to past the window bound: the
// spans are those of the release before, and the words around the runs are never in one. The answer is known by
// construction for the words (no rule hides "lorem" or "ipsum" after a blank), and by the kept copy for the key.
func TestKeyBetweenSlashRunsMatchesKeptCopy(t *testing.T) {
	r := rand.New(rand.NewPCG(1087, 2)) // #nosec G404 -- reproducible generated texts
	for run := range 31 {
		for _, pad := range []string{"", "=", "=="} {
			key := secretLike(r)
			split := key[:8] + "/" + key[8:16] + "/" + key[16:] + pad
			s := "lorem " + strings.Repeat("/", run) + split + strings.Repeat("/", run) + " ipsum"
			if f := sameAsKept(s); f != "" {
				t.Errorf("run of %d: %s", run, f)
			}
			for _, sp := range Spans(s) {
				if sp.Start < len("lorem ") || sp.End > len(s)-len(" ipsum") {
					t.Errorf("run of %d: span %v takes in the words around the key in %q", run, sp, s)
				}
			}
		}
	}
}
