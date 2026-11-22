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

import (
	"bytes"
	"testing"
)

// The probes of one input are unique, also past the count four digits can number: an input of five fields full
// of fresh probes holds each probe text once, so the recorder never takes two probes for one (#873, review of
// 2026-11-21).
func TestDecodeProbesAreUnique(t *testing.T) {
	field := bytes.Repeat([]byte{ProbeByte}, MaxFieldBytes/len(Probe(0))+1)
	data := []byte{0, 0}
	for i := range 5 {
		if i > 0 {
			data = append(data, FieldSep)
		}
		data = append(data, field...)
	}
	in := Decode(data, 5)
	seen := map[string]int{}
	for i, f := range in.Fields {
		for p := range ProbesIn(f) {
			if j, ok := seen[p]; ok {
				t.Fatalf("probe %s is in fields %d and %d", p, j, i)
			}
			seen[p] = i
		}
	}
	if len(seen) < 10000/2 {
		t.Fatalf("only %d probes in the input", len(seen))
	}
}
