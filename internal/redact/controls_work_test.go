//go:build redactwork

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
	"fmt"
	"strings"
	"testing"
)

// controlsWorkShapes are the shapes the work of the controls view (#852) is measured on: text without a control,
// on which it only scans, control-dense text, a secret form split by controls, and the shape on which the second
// reading of the layout write of the first build of #852 was superlinear ("--password x" and a control).
var controlsWorkShapes = []struct{ name, unit string }{
	{"no control", "lorem ipsum dolor sit amet "},
	{"a ESC", "a\x1b"},
	{"alternating controls", "a\x00b\x7fc\u0085d\x1b"},
	{"split secrets", "pass\x00word=Hunter2Secret9x\n"},
	{"--password x and a control", "--password x\x01"},
}

// controlsWorkSizes are the sizes measured: from 4 KiB to the largest object (about 1.5 MiB).
var controlsWorkSizes = []int{4 << 10, 16 << 10, 32 << 10, 3 << 19}

// The work of the controls view is linear in the length of the text and inside the budget (decision of 2026-11-22
// on #852): on each shape, the work of the added reading alone (addControlsView on an empty list) per byte at 1.5 MiB is at most
// 1.3 times its work per byte at 32 KiB and at most ownWorkPerByte; and String with it is decided, never withheld,
// at every size. It logs the work per byte of the added reading and of String at each size.
func TestControlsViewWorkIsLinear(t *testing.T) {
	if raceDetector {
		t.Skip("it counts work on texts up to 1.5 MiB")
	}
	r := defaultRedactor
	for _, sh := range controlsWorkShapes {
		per := map[int]float64{}
		var line strings.Builder
		for _, n := range controlsWorkSizes {
			s := repeatTo(sh.unit, n)
			added := Work(func() { r.addControlsView(nil, s, r.newMatchList()) })
			var ok bool
			total := Work(func() { _, ok = r.StringChecked(s) })
			per[n] = float64(added) / float64(n)
			fmt.Fprintf(&line, " %d KiB: %.0f (String %.0f, decided %v);", n>>10, per[n], float64(total)/float64(n), ok)
			if !ok {
				t.Errorf("%s: String withheld an entry of %d bytes", sh.name, n)
			}
		}
		t.Logf("%s, steps per byte of the controls view:%s", sh.name, line.String())
		small, large := per[32<<10], per[3<<19]
		if large > 1.3*small+1 {
			t.Errorf("%s: the controls view takes %.0f steps per byte at 1.5 MiB, %.0f at 32 KiB: not linear", sh.name, large, small)
		}
		if large > ownWorkPerByte {
			t.Errorf("%s: the controls view takes %.0f steps per byte, over %d", sh.name, large, ownWorkPerByte)
		}
	}
}
