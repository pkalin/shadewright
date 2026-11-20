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
	"regexp"
	"strings"
)

// The block-value rule's reading of a value's end in a flow collection before flowLine (#845, P3), the oracle of
// TestFlowLineMatchesTheOldReading: each key read from its value to the end of the line.

// plainKeyAfterComma matches a comma and a plain key ("name:", "'user':") after it: the end of a value in a
// flow collection at the same boundary the flow rule has, so the fields after it stay.
var plainKeyAfterComma = regexp.MustCompile(`,` + keySpaceExpr + `*(?:"[^"\r\n]*"|'[^'\r\n]*'|[A-Za-z_][A-Za-z0-9_.-]*)` +
	keySpaceExpr + `*:`)

// flowCloseAt returns where a value from start to end in a flow collection opened by opener ends: at the last
// closing bracket of that collection on the line, brackets opened inside the value followed, that is followed
// by the end, a blank, a comma, a semicolon or a comment, after any more closing brackets and quotes; or -1.
// A bracket glued to more text ("a]b") is part of the value, however it could pair with an earlier one.
func flowCloseAt(s string, start, end int, opener byte) int {
	var inner []byte
	// runEnd is the end of the run of closing brackets and quotes read last: a later bracket in the same run
	// ends there too, so the run is read once, not once for each of its brackets (#845, third decision of
	// 2026-11-18: "{password: x" and n closing braces cost n*n/2 steps).
	last, runEnd := -1, -1
	for i := start; i < end; i++ {
		tick(1)
		switch c := s[i]; c {
		case '[', '{':
			inner = append(inner, c)
		case ']', '}':
			if n := len(inner); n > 0 && inner[n-1] == c-2 {
				inner = inner[:n-1]
				continue
			}
			if c != opener+2 {
				continue
			}
			j := max(i, runEnd)
			for j < end && strings.IndexByte(`]})"'`, s[j]) >= 0 {
				tick(1)
				j++
			}
			runEnd = j
			if j == end || indentLen(s, j) > 0 || strings.IndexByte(",;#", s[j]) >= 0 {
				last = i
			}
		}
	}
	return last
}
