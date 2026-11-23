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

import "testing"

// An empty value inside a span the reading of the release before hides takes nothing and breaks nothing: its
// piece has no byte to carry the span to (a list_events line with an empty field inside a secret's value).
func TestReleasedReadingSkipsAnEmptyValue(t *testing.T) {
	for _, form := range []Form{AsIs, Line, JSON} {
		ps := []Piece{{Text: "password=Zq8LmT3v"}, {Text: "", Data: true, Form: form}, {Text: "Xr9KpW2sYd6HnB4c next=x\n"}}
		out := defaultRedactor.Pieces(ps)
		if len(out) != len(ps) || out[1] != "" {
			t.Errorf("form %d: %q", form, out)
		}
	}
}
