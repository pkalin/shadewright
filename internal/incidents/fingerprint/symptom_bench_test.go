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

package fingerprint

import (
	"strings"
	"testing"
)

// BenchmarkSymptom measures the stored symptom of a short note and of notes of 4 KiB (the longest a symptom reads),
// on one line and on several.
func BenchmarkSymptom(b *testing.B) {
	line := "Back-off restarting failed container app in pod web-7d4b9c8f6d-x2kqp: exit code 137, OOMKilled "
	long := func(sep string) string {
		var s strings.Builder
		for s.Len()+len(line)+len(sep) <= 4096 {
			s.WriteString(line)
			s.WriteString(sep)
		}
		return s.String()
	}
	for _, c := range []struct{ name, note string }{
		{"short", strings.TrimSpace(line)},
		{"plain", long("")},
		{"multiline", long("\n")},
	} {
		o := Observation{Note: c.note, Reason: reasonBackOff, RegardingKind: kindPod, RegardingName: "web-7d4b9c8f6d-x2kqp"}
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Symptom(o)
			}
		})
	}
}
