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
	"math/rand/v2"
	"strings"
	"testing"
)

// plainRunBytes are the bytes the generated texts of TestPlainWordEndMemoMatches are made of: plain text, the
// bytes that end a word, open a group or escape, and "$" with and without "(" after it.
var plainRunBytes = []string{"a", "-", ",", "}", "$", "$(", ")", "(", "`", "\\", "\\ ", " ", "\"", "'", "\n", ";", "--token,"}

// The plain run plainWordEnd keeps for one pass over a text (#873) does not change the end it returns: on
// generated texts, reads at any start, limit, key and width, in any order, find the end found without it. A
// "$" just before a read's limit (plain to that read, a group to one with a larger limit) is among them.
func TestPlainWordEndMemoMatches(t *testing.T) {
	rng := rand.New(rand.NewPCG(873, 2)) // #nosec G404 -- reproducible generated texts
	calls, jumped := 0, 0
	for range 20000 {
		var b strings.Builder
		for range 1 + rng.IntN(40) {
			b.WriteString(plainRunBytes[rng.IntN(len(plainRunBytes))])
		}
		s := b.String()
		pr := &plainRun{}
		extent := 1 + rng.IntN(len(s))
		pos := 0
		for range 30 {
			if rng.IntN(3) == 0 {
				pos = rng.IntN(len(s))
			} else {
				pos = min(len(s)-1, pos+rng.IntN(4))
			}
			limit := pos + 1 + rng.IntN(len(s)-pos)
			if rng.IntN(2) == 0 {
				limit = min(len(s), pos+extent)
			}
			keyPos := noKey
			if rng.IntN(2) == 0 {
				keyPos = rng.IntN(pos + 1)
			}
			wide := rng.IntN(3) > 0
			want := plainWordEnd(nil, s, pos, limit, keyPos, wide, nil)
			if pr.from <= pos && pos < pr.to {
				jumped++
			}
			got := plainWordEnd(nil, s, pos, limit, keyPos, wide, pr)
			calls++
			if got != want {
				t.Fatalf("plainWordEnd(%q, %d, %d, %d, %v) with the plain run = %d, without = %d", s, pos, limit,
					keyPos, wide, got, want)
			}
		}
	}
	if jumped < 10000 {
		t.Fatalf("%d of %d reads started inside the plain run: the generator no longer reaches it", jumped, calls)
	}
	t.Logf("%d reads, %d started inside the plain run", calls, jumped)
}
