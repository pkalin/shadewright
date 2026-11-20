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

package meter

import (
	"math/rand/v2"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// findExprs are expressions with every kind of assertion and empty match FindAll treats specially: the start of
// the text and of a line, word boundaries, empty matches next to others, groups that do not take part, case
// folding scoped to a group, and the shapes that read past their match.
var findExprs = []string{
	`a`, `a*`, `a*?`, `^a`, `(?m)^a`, `a$`, `(?m)a$`, `\ba`, `\Ba`, `a\b`, `\b`, `\B`, `^`, `$`, `(?m)^`, `x*`,
	`(a)|(b)`, `(a)?b`, `(?i)ab`, `(?i:a)b`, `a.*z|a`, `(?s)a.*?z`, `[^ ]+`, `\w+`, `(?U)a+`, `é`, `\x{FFFD}`,
	`(\w+)=(\S*)`, `\Aa`, `a\z`, `(?m)$\n?`, `.`, `(?s).`, `[[:^alpha:]]`,
}

// findAlphabet makes texts with word and non-word characters, line breaks, a multi-byte character and bytes
// that are not UTF-8, alone and cut in the middle of a character.
var findAlphabet = []string{"a", "b", "z", " ", "\n", "=", "é", "\xe2\x82", "\xff", "_", "x", "A"}

// FindAll returns what FindAllStringSubmatchIndex returns, for every expression on generated texts.
func TestFindAllIsFindAllStringSubmatchIndex(t *testing.T) {
	rng := rand.New(rand.NewPCG(873, 20261120))
	texts := []string{""}
	for range 400 {
		var b strings.Builder
		for range rng.IntN(24) {
			b.WriteString(findAlphabet[rng.IntN(len(findAlphabet))])
		}
		texts = append(texts, b.String())
	}
	spent := 0
	for _, expr := range findExprs {
		re := regexp.MustCompile(expr)
		e := NewExpr(re)
		for _, s := range texts {
			want := re.FindAllStringSubmatchIndex(s, -1)
			got := e.FindAll(s, func(n int) { spent += n })
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%q on %q: got %v, want %v", expr, s, got, want)
			}
		}
	}
	if spent == 0 {
		t.Error("nothing was charged")
	}
}

// Each attempt is charged with the bytes it reads: a.*z|a on a run of n "a" reads the rest of the run for each
// of its n matches, about n*n/2 bytes, while an expression that stops at its match reads a few bytes per match (the character before it and one after).
func TestFindAllChargesEachAttempt(t *testing.T) {
	const n = 2048
	s := strings.Repeat("a", n)
	charge := func(expr string) int {
		spent := 0
		NewExpr(regexp.MustCompile(expr)).FindAll(s, func(k int) { spent += k })
		return spent
	}
	if got := charge(`a.*z|a`); got < n*n/2 {
		t.Errorf("a.*z|a charged %d bytes, want at least %d", got, n*n/2)
	}
	if got := charge(`a`); got > 8*n {
		t.Errorf("a charged %d bytes, want at most %d", got, 8*n)
	}
}

// A meter that runs out stops the search inside the attempt that passes it.
func TestFindAllStopsOnTheMeter(t *testing.T) {
	m := New(100_000)
	over := false
	func() {
		defer Catch(&over)
		NewExpr(regexp.MustCompile(`a.*z|a`)).FindAll(strings.Repeat("a", 32768), m.Spend)
	}()
	if !over || m.Spent() > 100_000+chunk {
		t.Errorf("over %v, spent %d", over, m.Spent())
	}
}
