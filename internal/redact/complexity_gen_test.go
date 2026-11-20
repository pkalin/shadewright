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
)

// Cost is linear on inputs built from the tokens of the rules themselves (#845, third decision of 2026-11-18,
// property 3). A loop that counts its work can still be quadratic: "{password: marigold" followed by n closing
// braces read the rest of the line again at each brace (152,137, 565,577 and 2,178,889 steps for n = 512, 1024
// and 2048 on 38cf1a37). Each shape is an opening (nothing, or a key in one of its forms) followed by a unit of
// one or two tokens (keys, separators, brackets, quotes, block markers, line ends, invisible characters)
// repeated n, 2n and 4n times; the work of String may at most double when the input doubles, with
// complexitySlack for the constant part.

// complexityOpenings are what a shape starts with.
var complexityOpenings = []string{
	"", "{" + passwordKey + ": marigold", passwordKey + ": |\n", "[--" + canonTokenKey + " ", "\"" + passwordKey + "\": \"",
	passwordKey + "=", "{\"a\": \"",
	"? password\n", "- name: DB_PASSWORD\n", "args: [\"--password\", ",
}

// complexityTokens are the tokens a unit is made of; complexityCore are those paired with each other.
var (
	complexityTokens = []string{
		"}", "]", "{", "[", "\"", "'", ":", "=", "|", ">", "-", "#", "\n", "\r", " ", "\t", ",", "?", "\\",
		passwordKey + ": ", "--" + canonTokenKey + " ", passwordKey, passKey + ": |", "：", "\u200b", "->", "x", "\"password\": ",
	}
	complexityCore = []string{"}", "]", "{", "[", "\"", ":", "|", "\n", " ", passwordKey + ": ", "--" + canonTokenKey + " ", "-"}
)

// complexityUnits returns every unit: each token, and each pair of core tokens.
func complexityUnits() []string {
	units := append([]string(nil), complexityTokens...)
	for _, a := range complexityCore {
		for _, b := range complexityCore {
			units = append(units, a+b)
		}
	}
	return units
}

// complexityN is the smallest count of units; complexitySlack is how much more than twice the work a doubling
// may cost, for the part of the work that does not grow with n.
const (
	complexityN     = 256
	complexitySlack = 1.1
)

func TestTokenShapesWorkIsLinear(t *testing.T) {
	shapes, failed := 0, 0
	for _, open := range complexityOpenings {
		for _, unit := range complexityUnits() {
			shapes++
			var work [3]int64
			for i := range work {
				in := open + strings.Repeat(unit, complexityN<<i)
				work[i] = countWork(t, func() { String(in) })
			}
			for i := 1; i < len(work); i++ {
				if float64(work[i]) > 2*complexitySlack*float64(work[i-1]) {
					if failed++; failed <= 10 {
						t.Errorf("%q + %q x %d: work %d, at twice as many units %d", open, unit, complexityN<<(i-1), work[i-1], work[i])
					}
					break
				}
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d shapes cost more than twice as much at twice the size", failed, shapes)
	}
	t.Logf("%d shapes at %d, %d and %d units", shapes, complexityN, 2*complexityN, 4*complexityN)
	for _, n := range []int{512, 1024, 2048} {
		open := "{" + passwordKey + ": marigold"
		in := open + strings.Repeat("}", n)
		t.Logf("%q and %d closing braces: %d steps", open, n, countWork(t, func() { String(in) }))
	}
}
