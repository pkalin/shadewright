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

package main

import (
	"errors"
	"slices"
	"testing"
)

// TestEmptyWatchNamespacesIsRefused: the verifier does not start without a namespace to watch.
func TestEmptyWatchNamespacesIsRefused(t *testing.T) {
	for _, s := range []string{"", " ", ","} {
		if _, err := parseWatched(s); !errors.Is(err, errNoWatchedNamespace) {
			t.Errorf("parseWatched(%q) = %v, want the empty list refused", s, err)
		}
	}
	got, err := parseWatched("demo,team-a")
	if err != nil || !slices.Equal(got, []string{"demo", "team-a"}) {
		t.Errorf("parseWatched = %v, %v", got, err)
	}
}
