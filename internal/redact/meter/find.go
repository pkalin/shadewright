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
	"fmt"
	"io"
	"regexp"
	"unicode/utf8"
)

// Expr is a regular expression whose matches are found with every byte the engine reads charged, attempt by
// attempt (#873, decision of 2026-11-20). FindAllStringSubmatchIndex reads the text once per match attempt, from
// where the attempt starts to where the engine can decide it: for most expressions a little past the match, but
// for one shaped like `a.*z|a` the rest of the text, so on a run of "a" every match costs the length of the text
// and the call costs its square. Charging only the length of the text once, as a count of the call does, would
// let such a call run for seconds inside a budget it has long passed.
type Expr struct {
	re *regexp.Regexp
	// cont is re searched from a later start of a text, read from the character before that start: `\A`, any one
	// character, then re's unanchored search as a capture group, so the assertions of re (^, \b, \B) see the
	// character before the start as FindAll sees it, and a match never starts before the start.
	cont *regexp.Regexp
}

// NewExpr returns re as an Expr. It panics where [CompileExpr] fails: on the built-in expressions, which do not,
// and on a pattern [CompileExpr] accepted.
func NewExpr(re *regexp.Regexp) *Expr {
	e, err := CompileExpr(re)
	if err != nil {
		panic(err)
	}
	return e
}

// CompileExpr returns re as an Expr, or an error when the search of re from a later start does not compile.
func CompileExpr(re *regexp.Regexp) (*Expr, error) {
	cont, err := regexp.Compile(`\A(?s:.)(?s:.)*?(` + re.String() + `)`)
	if err != nil {
		return nil, fmt.Errorf("the search of %q from a later start: %w", re.String(), err)
	}
	return &Expr{re: re, cont: cont}, nil
}

// Regexp returns the expression.
func (e *Expr) Regexp() *regexp.Regexp { return e.re }

// chunk is how many bytes the reader reads before it charges them: the overrun past a budget is at most this.
const chunk = 256

// reader reads a text for the engine and charges what it reads to spend.
type reader struct {
	s       string
	i       int
	pending int
	spend   func(n int)
}

// ReadRune reads the next character of the text, as utf8.DecodeRuneInString does (an invalid byte is U+FFFD of
// width 1), and charges its bytes.
func (r *reader) ReadRune() (rune, int, error) {
	if r.i >= len(r.s) {
		return 0, 0, io.EOF
	}
	c, size := utf8.DecodeRuneInString(r.s[r.i:])
	r.i += size
	r.pending += size
	if r.pending >= chunk {
		r.flush()
	}
	return c, size, nil
}

// flush charges the bytes read and not charged yet.
func (r *reader) flush() {
	if r.pending > 0 {
		n := r.pending
		r.pending = 0
		r.spend(n)
	}
}

// FindAll returns what e.Regexp().FindAllStringSubmatchIndex(s, -1) returns, with each byte the engine reads
// for each attempt passed to spend, at most 256 bytes after it was read. spend may stop the search by a panic
// (Meter.Spend): the attempt it stops is not finished.
func (e *Expr) FindAll(s string, spend func(n int)) [][]int {
	var out [][]int
	end := len(s)
	// As regexp.(*Regexp).allMatches: an empty match right after the previous match is not taken, and after an
	// empty match the search goes on one character later.
	for pos, prevEnd := 0, -1; pos <= end; {
		m := e.attempt(s, pos, spend)
		if m == nil {
			break
		}
		accept := true
		if m[1] == pos {
			if m[0] == prevEnd {
				accept = false
			}
			if pos < end {
				_, w := utf8.DecodeRuneInString(s[pos:])
				pos += w
			} else {
				pos = end + 1
			}
		} else {
			pos = m[1]
		}
		prevEnd = m[1]
		if accept {
			out = append(out, m)
		}
	}
	return out
}

// attempt returns the leftmost match of e in s that starts at pos or later, as the search of FindAll from pos
// finds it, with its submatches as indexes into s.
func (e *Expr) attempt(s string, pos int, spend func(n int)) []int {
	if pos == 0 {
		r := &reader{s: s, spend: spend}
		m := e.re.FindReaderSubmatchIndex(r)
		r.flush()
		return m
	}
	_, w := utf8.DecodeLastRuneInString(s[:pos])
	from := pos - w
	r := &reader{s: s[from:], spend: spend}
	m := e.cont.FindReaderSubmatchIndex(r)
	r.flush()
	if m == nil {
		return nil
	}
	// m[2:4] is re's whole match, m[4:] its groups, relative to from.
	out := make([]int, len(m)-2)
	for k, v := range m[2:] {
		out[k] = v
		if v >= 0 {
			out[k] = v + from
		}
	}
	return out
}
