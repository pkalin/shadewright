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
	"encoding/json"
	"math"
	"testing"
)

func TestJSONInteger(t *testing.T) {
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{int64(3), 3, true},
		{3, 3, true},
		{float64(3), 3, true},
		{json.Number("3"), 3, true},
		{1.5, 0, false},
		{json.Number("1.5"), 0, false},
		{math.Inf(1), 0, false},
		{float64(math.MaxInt64), 0, false},
		{"3", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		if got, ok := JSONInteger(c.in); got != c.want || ok != c.ok {
			t.Errorf("JSONInteger(%#v) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFirstCondition(t *testing.T) {
	const st, conds, isFalse, ready, typ = "status", "conditions", "False", "Ready", "type"
	first := map[string]any{typ: ready, st: isFalse}
	status := map[string]any{conds: []any{
		"not a map", map[string]any{typ: 1}, first, map[string]any{typ: ready, st: conditionTrue},
	}}
	if got := FirstCondition(status, ready); got[st] != isFalse {
		t.Errorf("FirstCondition = %#v, want the first Ready", got)
	}
	for _, s := range []map[string]any{nil, {}, {conds: ready}, {conds: []any{map[string]any{typ: "Active"}}}} {
		if got := FirstCondition(s, ready); got != nil {
			t.Errorf("FirstCondition(%#v) = %#v, want nil", s, got)
		}
	}
}
