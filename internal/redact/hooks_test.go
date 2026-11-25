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
	"slices"
	"strings"
	"testing"
)

func TestHooksLeaveCorpusDecisionsUnchanged(t *testing.T) {
	type run func(*Redactor) (string, bool)
	checked := func(s string) run {
		return func(r *Redactor) (string, bool) { return r.StringChecked(s) }
	}
	for _, c := range []struct {
		name   string
		run    run
		want   string
		ok     bool
		reason string
	}{
		{"plain text", checked("ordinary text"), "ordinary text", true, ""},
		{"extra pattern", checked("EMP-123456"), "[REDACTED]", true, ""},
		{"literal marker", checked(Withheld), Withheld, true, ""},
		{"work budget", func(r *Redactor) (string, bool) {
			return r.WithWorkBudget(0).StringChecked("ordinary text")
		}, Withheld, false, ReasonWork},
		{"pieces", func(r *Redactor) (string, bool) {
			out, ok := r.PiecesWithin(make([]Piece, FreePieces+1), -1)
			return strings.Join(out, ""), ok
		}, Withheld, false, ReasonHeap},
		{"sequence", func(r *Redactor) (string, bool) {
			out, ok := r.StringsChecked(make([]string, FreePieces*2))
			return strings.Join(out, ""), ok
		}, Withheld, false, ReasonHeap},
		{"short entry", func(r *Redactor) (string, bool) {
			return r.ReplaceCheckedShort(strings.Repeat("a", MaxShortEntry+1), keep, same)
		}, Withheld, false, ReasonSize},
		{"ordinary entry size", checked(strings.Repeat("a", MaxEntry+1)), Withheld, false, ReasonSize},
	} {
		t.Run(c.name, func(t *testing.T) {
			pattern := Pattern{Kind: "employee-id", Expr: "EMP-[0-9]{6}"}
			var reasons []string
			waits := 0
			for _, mode := range []struct {
				name        string
				hooks       Hooks
				wantReasons []string
			}{
				{name: "nil"},
				{"recording", Hooks{Withheld: func(reason string) { reasons = append(reasons, reason) },
					HeapWait: func() { waits++ }}, []string{c.reason}},
			} {
				t.Run(mode.name, func(t *testing.T) {
					r, err := New(pattern, mode.hooks)
					if err != nil {
						t.Fatal(err)
					}
					if got, ok := c.run(r); got != c.want || ok != c.ok {
						t.Errorf("got %.80q, decided %v; want %q, decided %v", got, ok, c.want, c.ok)
					}
					wantReasons := mode.wantReasons
					if c.reason == "" {
						wantReasons = nil
					}
					if !slices.Equal(reasons, wantReasons) || waits != 0 {
						t.Errorf("hooks got reasons %v and %d waits, want %v and no waits", reasons, waits, wantReasons)
					}
				})
			}
		})
	}
}
