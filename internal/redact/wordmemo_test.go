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
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// wordPieces are the pieces the texts of the tests below are made of: what shellWordEnd and fieldAfterComma
// read (groups, commas before names, keys and separators, escapes, closers, line ends and blanks).
var wordPieces = []string{
	`"a"`, `'b'`, `"`, `'`, "`x`", "$(", ")", "]", "}", "[", "{", ",", ",b", ",b,", ",user=", ",next: ", ",DB_PASSWORD=",
	",password ", ",a=", ",a==", "TOKEN=", "\\ ", "\\,", "\\", " ", "\t", "\n", nlIndent, "x", "abc", "\u02d0", "\u212a",
	"\u017f", "=", ":", "--token=", "\"\\\"a\\\"\"",
	"\"a\nb\"", "\"a\n  b\"", "'a\n c'", "x\n  y",
	// Quoted groups across lines whose end depends on the key's column, and the bytes a group's limit decides on
	// (#845, P3: the group memory, groupTrace).
	"\"\n", "\n        ", "\n\t \t", "\n\n", "''", "'a''b'", "\"$(", "\"`", "\uff1a", "\"\n      \"",
}

// wordText returns a text of n pieces of wordPieces.
func wordText(rng *rand.Rand, n int) string {
	var b strings.Builder
	for range n {
		b.WriteString(wordPieces[rng.IntN(len(wordPieces))])
	}
	return b.String()
}

// The memory of the shell words read (wordMemo) changes no result: on generated texts, every call of
// shellWordEnd with a memory shared by all the calls on one text, in any order, with any limits, keys and flags,
// gives what the same call gives without one.
func TestShellWordEndMemoMatches(t *testing.T) {
	rng := rand.New(rand.NewPCG(845, 15)) // #nosec G404 -- reproducible generated texts
	calls, reused := 0, 0
	for range 40000 {
		s := wordText(rng, 1+rng.IntN(60))
		if rng.IntN(2) == 0 {
			// Glued words, as the keys of one long word read them.
			s = strings.Repeat(`TOKEN="a",b,`+wordPieces[rng.IntN(len(wordPieces))], 1+rng.IntN(12))
		}
		w := &wordMemo{}
		extent := 1 + rng.IntN(len(s))
		for range 40 {
			pos := rng.IntN(len(s))
			if !opensGroup(s, pos) {
				continue
			}
			limit := pos + 1 + rng.IntN(len(s)-pos)
			if rng.IntN(2) == 0 {
				limit = min(len(s), pos+extent) // as extentLimit gives it
			}
			keyPos := noKey
			if rng.IntN(2) == 0 {
				keyPos = rng.IntN(pos + 1)
			}
			flag, wide := rng.IntN(4) == 0, rng.IntN(3) > 0
			wantEnd, wantLast, wantOK := shellWordEnd(s, pos, limit, keyPos, flag, wide, nil)
			before := w.resumed
			end, last, ok := shellWordEnd(s, pos, limit, keyPos, flag, wide, w)
			calls++
			if w.resumed > before {
				reused++
			}
			if end != wantEnd || last != wantLast || ok != wantOK {
				t.Fatalf("shellWordEnd(%q, %d, %d, %d, %v, %v) with the memory = %d %v %v, without = %d %v %v",
					s, pos, limit, keyPos, flag, wide, end, last, ok, wantEnd, wantLast, wantOK)
			}
		}
	}
	if calls < 30000 || reused < 1000 {
		t.Fatalf("%d calls, %d went on from an earlier read: the generator no longer reaches the memory", calls, reused)
	}
}

// oldFieldAfterComma is fieldAfterComma of origin/main, which matched commaSecretKey on the whole rest of the
// text up to limit.
func oldFieldAfterComma(s string, i, limit int) bool {
	j := i + 1
	if j >= limit || isSpace(s[j]) || strings.IndexByte("\"'`,;|&<>(){}[]", s[j]) >= 0 {
		return true
	}
	if oldCommaSecretKey.MatchString(s[j:limit]) {
		return true
	}
	for j < limit && (isAlnum(s[j]) || strings.IndexByte("_.-", s[j]) >= 0) {
		j++
	}
	switch {
	case j == i+1 || j >= limit:
		return false
	case s[j] == ':':
		return true
	case s[j] == '=':
		return j+1 < limit && !isSpace(s[j+1]) && s[j+1] != '='
	}
	return false
}

var oldCommaSecretKey = regexp.MustCompile(`(?i)^(?:` + bareKeyExpr + `)(?:[ \t]*[:=]|[ \t]+)`)

// fieldAfterComma, which matches the secret key name on the name and its separator only (#845, P3), decides
// every comma of the generated texts as origin/main's did, at every limit; and when it says it did not read up
// to the limit, a larger limit decides the same.
func TestFieldAfterCommaMatchesMain(t *testing.T) {
	rng := rand.New(rand.NewPCG(845, 16)) // #nosec G404 -- reproducible generated texts
	names := []string{passwordKey, "PASSWORD", "db_pass", "api-key", userKey, "next", "a", "to\u212aen", "\u017fecret", sasKeyName, "blobSas", "x.y"}
	seps := []string{"=", ":", ":=", " ", "\t=", "  ", "==", "= ", "", ",", "\u212a"}
	commas := 0
	for range 20000 {
		var b strings.Builder
		for range 1 + rng.IntN(6) {
			if rng.IntN(3) == 0 {
				b.WriteString(wordPieces[rng.IntN(len(wordPieces))])
				continue
			}
			b.WriteString("," + names[rng.IntN(len(names))] + seps[rng.IntN(len(seps))])
		}
		s := b.String()
		for i := 0; i < len(s); i++ {
			if s[i] != ',' {
				continue
			}
			for limit := i + 1; limit <= len(s); limit++ {
				commas++
				got, reached := fieldAfterComma(s, i, limit)
				if want := oldFieldAfterComma(s, i, limit); got != want {
					t.Fatalf("fieldAfterComma(%q, %d, %d) = %v, origin/main %v", s, i, limit, got, want)
				}
				if !reached {
					if all, _ := fieldAfterComma(s, i, len(s)); all != got {
						t.Fatalf("fieldAfterComma(%q, %d, %d) = %v without reaching the limit, %v at the end of the text", s, i, limit, got, all)
					}
				}
			}
		}
	}
	if commas < 100000 {
		t.Fatalf("only %d commas checked", commas)
	}
}

// oldViewValueEnd is viewValueEnd of origin/main's #845 branch before the fifteenth decision, which read a word
// to the first blank of the rest of the line before it cut it at the next key.
func oldViewValueEnd(s string, keys []viewKeyAt, i, next int) int {
	k := keys[i]
	end := k.valueEnd
	if k.word {
		end = k.after
		for end < k.valueEnd && indentLen(s, end) == 0 {
			end += charLen(s, end)
		}
	}
	if next >= 0 && keys[next].m[0] < end {
		end = max(keys[next].m[0], k.after)
	}
	switch {
	case k.plain:
		if c := strings.IndexByte(s[k.after:end], ','); c >= 0 {
			end = k.after + c
		}
	case len(k.flow) > 0:
		if c := plainKeyAfterComma.FindStringIndex(s[k.after:end]); c != nil {
			end = k.after + c[0]
		}
	}
	if n := len(k.flow); n > 0 {
		if c := flowCloseAt(s, k.after, end, k.flow[n-1]); c >= 0 {
			end = c
		}
	}
	return valueTail(s, k.after, end, k.flow)
}

// viewValueEnd, which reads a word only up to the next key that takes its value (#845, P3), ends every value of
// generated lines where the old logic did: keys at any places of a line, words or not, in a flow collection or not.
func TestViewValueEndMatchesTheOldLogic(t *testing.T) {
	rng := rand.New(rand.NewPCG(845, 17)) // #nosec G404 -- reproducible generated texts
	pieces := append([]string{"\u3000", "\u00a0", "]", "}", "a,b: c", ", x"}, wordPieces...)
	for range 20000 {
		var b strings.Builder
		for range 1 + rng.IntN(30) {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		s := b.String()
		n := 2 + rng.IntN(6)
		cuts := make([]int, 0, n+2)
		cuts = append(cuts, 0, len(s))
		for range n {
			p := rng.IntN(len(s) + 1)
			for p < len(s) && !utf8.RuneStart(s[p]) {
				p++
			}
			cuts = append(cuts, p)
		}
		slices.Sort(cuts)
		valueEnd := cuts[len(cuts)-1]
		var keys []viewKeyAt
		for j := 0; j+1 < len(cuts)-1; j++ {
			k := viewKeyAt{m: []int{cuts[j], cuts[j]}, after: cuts[j+1], valueEnd: valueEnd, word: rng.IntN(2) == 0}
			switch rng.IntN(4) {
			case 0:
				k.plain, k.flow = true, []byte{'{'}
			case 1:
				k.flow = []byte{'['}
			}
			keys = append(keys, k)
		}
		for i := range keys {
			for _, next := range []int{-1, i + 1 + rng.IntN(len(keys)-i)} {
				if next >= len(keys) {
					continue
				}
				if got, want := viewValueEnd(s, keys, i, next), oldViewValueEnd(s, keys, i, next); got != want {
					t.Fatalf("viewValueEnd(%q, %+v, %d, %d) = %d, the old logic %d", s, keys, i, next, got, want)
				}
			}
		}
	}
}

// The memory of the last value end (tokenMemo) changes no result: on generated texts, every tokenEnd through one
// memory, at positions in any order, gives what tokenEnd gives alone.
func TestTokenMemoMatches(t *testing.T) {
	rng := rand.New(rand.NewPCG(845, 18)) // #nosec G404 -- reproducible generated texts
	pieces := append([]string{"\\\\\"", "\\\\\\\"", "\\\\x", "-password,", "\\'"}, wordPieces...)
	hits := 0
	for range 20000 {
		var b strings.Builder
		for range 1 + rng.IntN(30) {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		s := b.String()
		tm := &tokenMemo{}
		pos := 0
		for range 20 {
			if rng.IntN(3) == 0 {
				pos = rng.IntN(len(s) + 1)
			} else {
				pos = min(len(s), pos+rng.IntN(4))
			}
			if tm.ok && tm.from <= pos && pos <= tm.end {
				hits++
			}
			if got, want := tm.tokenEnd(s, pos), tokenEnd(s, pos); got != want {
				t.Fatalf("tokenEnd(%q, %d) through the memory = %d, alone %d", s, pos, got, want)
			}
		}
	}
	if hits < 10000 {
		t.Fatalf("only %d reads answered from the memory", hits)
	}
}

// A quoted group that closes at the limit of one read, only because its last quote could not look past it,
// does not close at that place for a read with a larger limit: the single-quoted group of the text below closes for
// a limit just after its second quote, while the quote after that makes the two a doubled quote. The memory went on from the first read's state after the
// group, so the second read ended its word there instead of before the group.
func TestShellWordEndMemoAtTheLimit(t *testing.T) {
	s := `"x"'a''b'z`
	w := &wordMemo{}
	for _, limit := range []int{6, 7} {
		wantEnd, wantLast, wantOK := shellWordEnd(s, 0, limit, noKey, false, true, nil)
		end, last, ok := shellWordEnd(s, 0, limit, noKey, false, true, w)
		if end != wantEnd || last != wantLast || ok != wantOK {
			t.Errorf("limit %d: with the memory %d %v %v, without %d %v %v", limit, end, last, ok, wantEnd, wantLast, wantOK)
		}
	}
}

// A quoted group across a line end closes for a read whose key is at a column less than the next line's
// indentation and not for one whose key is at a larger column: a read with such a key does not go on past the
// group from the states an earlier read without a key, or with a key further left, kept.
func TestShellWordEndMemoKeyColumn(t *testing.T) {
	s := "x = \"a\"\"b\n  \"c d"
	w := &wordMemo{}
	for _, keyPos := range []int{noKey, 0, 1, 2, 3} {
		wantEnd, wantLast, wantOK := shellWordEnd(s, 4, len(s), keyPos, false, true, nil)
		end, last, ok := shellWordEnd(s, 4, len(s), keyPos, false, true, w)
		if end != wantEnd || last != wantLast || ok != wantOK {
			t.Errorf("key at %d: with the memory %d %v %v, without %d %v %v", keyPos, end, last, ok, wantEnd, wantLast, wantOK)
		}
	}
}
