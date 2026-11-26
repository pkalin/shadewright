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

package redacttest

import "testing"

// ShowsProbe applies its four-byte check to the squeezed output too: a part of the probe that separators split
// in the output is still read as shown.
func TestShowsProbeReadsFourBytesOfTheSqueezedOutput(t *testing.T) {
	for _, c := range []struct {
		out, probe string
		want       bool
	}{
		{"a-b-c-d", "abcdefgh", true},
		{"x m.y.s.e y", GateProbeValue, true},
		{"password=<redacted> ok", GateProbeValue, false},
		{"mys ecr etv alu e", GateProbeValue, true},
		{"m-y-s", GateProbeValue, false},
	} {
		if got := ShowsProbe(c.out, c.probe); got != c.want {
			t.Errorf("ShowsProbe(%q, %q) = %t, want %t", c.out, c.probe, got, c.want)
		}
	}
}
