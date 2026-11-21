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

package causereason

import (
	"slices"
	"testing"
)

// A reader that changes the list it was given changes no other reader's list.
func TestListsAreCopies(t *testing.T) {
	for name, list := range map[string]func() []string{"Waiting": Waiting, "Tallied": Tallied} {
		want := list()
		got := list()
		got[0] = "Changed"
		if !slices.Equal(list(), want) {
			t.Errorf("%s: a change to one copy reached the next: %v", name, list())
		}
	}
}

// The predicates agree with the lists, and the tally's list is the OOM kill and the waits, nothing else.
func TestPredicatesMatchTheLists(t *testing.T) {
	if want := append([]string{OOMKilled}, Waiting()...); !slices.Equal(Tallied(), want) {
		t.Fatalf("Tallied is %v, want %v", Tallied(), want)
	}
	for _, r := range append(Tallied(), "Error", "CrashLoopBackOff", "", "oomkilled") {
		if IsWaiting(r) != slices.Contains(Waiting(), r) || ShowsTally(r) != slices.Contains(Tallied(), r) {
			t.Errorf("%q: IsWaiting %v, ShowsTally %v disagree with the lists", r, IsWaiting(r), ShowsTally(r))
		}
	}
}
