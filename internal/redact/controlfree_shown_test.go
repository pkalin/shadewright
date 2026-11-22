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

package redact_test

import (
	"testing"

	"github.com/pkalin/shadewright/internal/redact"
)

// The two entry points #852 added decide text without control characters as the ones they stand in for on
// origin/main: StringShownChecked as StringKeepingChecked (the reviewer hint), StringTwiceShownChecked as
// StringTwiceChecked (incidentcontext.Value, Redact and RenderLog), flag included. With
// TestControlFreeTextDecidedAsOnMain, which compares those two with origin/main's table, they decide such text as
// main did (the condition of the decision of 2026-11-22). This file does not compile on origin/main and is not
// copied there.
func TestControlFreeShownEntriesDecideAsTheOld(t *testing.T) {
	redact.ParallelUnderRace(t)
	r := defaultRedactor()
	differ, ran := 0, 0
	for i, c := range redact.ControlFreeCorpus() {
		if !redact.RaceKeeps("shown entries", i) {
			continue
		}
		ran++
		gotS, okS := r.StringShownChecked(c.In, "hunter")
		wantS, wantOkS := r.StringKeepingChecked(c.In, "hunter")
		gotT, okT := r.StringTwiceShownChecked(c.In, nil)
		wantT, wantOkT := r.StringTwiceChecked(c.In, nil)
		if gotS != wantS || okS != wantOkS || gotT != wantT || okT != wantOkT {
			if differ++; differ <= 5 {
				t.Errorf("%s: in %q\n  StringShownChecked %q %v, StringKeepingChecked %q %v\n  StringTwiceShownChecked %q %v, StringTwiceChecked %q %v",
					c.Name, c.In, gotS, okS, wantS, wantOkS, gotT, okT, wantT, wantOkT)
			}
		}
	}
	if differ > 0 {
		t.Errorf("%d of %d control-free inputs decided unlike the entry points they stand in for", differ, ran)
	}
	if ran == 0 {
		t.Fatal("no input ran")
	}
}
