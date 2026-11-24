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

// gluedFieldsUnit is the unit the label value of the FuzzDiffBuild input bb930588237836fd (package
// incidentcontext, #873) repeats: a key name, a quoted value and fields glued to it by commas. Each "TOKEN=" starts
// a value that the extent rule reads as one shell word up to its bound (maxValueExtent), and the reading of the
// release before matched the text after every comma in that word against the secret key names up to the bound
// again: about 70,700 steps per byte of the label.
const gluedFieldsUnit = "SECRETPROBE0005TOKEN=\"a\",b,﹕"

// gluedFields returns a series line of the context builder whose label value repeats gluedFieldsUnit to at
// least n bytes.
func gluedFields(n int) string {
	return "metric{pod=web,key=" + strings.Repeat(gluedFieldsUnit, n/len(gluedFieldsUnit)+1) + "} 1\n"
}

// The glued fields cost the same per byte at 64 KiB, 256 KiB and 1 MiB, the reading of the release before
// included (checkWork: at most maxWorkPerByte per byte, and linear).
func TestGluedFieldsWorkIsLinear(t *testing.T) {
	samples := make([]workSample, 0, 3)
	for _, n := range []int{64 << 10, 256 << 10, 1 << 20} {
		w := sampleWorkAlone(t, gluedFields(n))
		samples = append(samples, w)
		if w.release > int64(maxWorkPerByte*w.size) {
			t.Fatalf("the release's reading costs %.1f steps per byte at %d bytes, over %d", perByte(w.release, w.size),
				w.size, maxWorkPerByte)
		}
	}
	checkWork(t, fmt.Sprintf("glued fields %q", gluedFieldsUnit), samples)
}
