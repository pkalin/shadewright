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

// Package meter holds the work budget of one redaction (#873, decision of 2026-11-20): the steps the redactor
// may take on one entry, counted where it counts its work, and the stop when they run out.
package meter

// Meter is the steps left to one redaction. It belongs to one call and one goroutine. A nil Meter counts
// nothing and never stops: the tests that read a rule alone, and the reads that are not an entry of their own.
type Meter struct {
	left  int64
	spent int64
}

// New returns a meter of steps steps.
func New(steps int64) *Meter { return &Meter{left: steps} }

// exceeded is the value Spend panics with when the steps run out; [Catch] recovers it and nothing else.
type exceeded struct{}

// Spend takes n steps from m and stops the redaction, by a panic that [Catch] recovers at the entry point,
// when none are left. The step that runs out is not done: the counting sites spend before they work.
func (m *Meter) Spend(n int) {
	if m == nil {
		return
	}
	m.spent += int64(n)
	m.left -= int64(n)
	if m.left < 0 {
		panic(exceeded{})
	}
}

// Spent returns the steps m counted, those of the step that ran out included.
func (m *Meter) Spent() int64 {
	if m == nil {
		return 0
	}
	return m.spent
}

// Catch is deferred by an entry point: it recovers the stop of [Meter.Spend] and sets *over, and lets every
// other panic go on.
func Catch(over *bool) {
	if r := recover(); r != nil {
		if _, ok := r.(exceeded); !ok {
			panic(r)
		}
		*over = true
	}
}
