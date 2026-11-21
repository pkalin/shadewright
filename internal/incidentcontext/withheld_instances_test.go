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

package incidentcontext

import (
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// A log read redaction withheld, the first or the second of a container (#943), makes the logs a failed source
// labelled withheld by redaction, with nothing of the withheld answer, and counts once under the reason withheld
// (#873, decision of 2026-11-20: every caller of the log source treats withheld as a failed source).
func TestWithheldLogReadOfEitherInstanceIsFailedSource(t *testing.T) {
	cases := []struct {
		name    string
		logs    *instanceLogs
		reads   int
		excerpt bool
	}{
		{name: "first read", logs: &instanceLogs{currentErr: providers.ErrWithheld}, reads: 1},
		{name: "second read", logs: &instanceLogs{current: servingLine, previousErr: providers.ErrWithheld},
			reads: 2, excerpt: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := reasons()
			c := buildWith(t, exited, tc.logs)
			logs := one(t, c, SectionLogs)
			if len(tc.logs.asked) != tc.reads {
				t.Fatalf("%d reads, want %d", len(tc.logs.asked), tc.reads)
			}
			if !slices.Contains(c.FailedSources, SectionLogs) || !slices.Contains(logs.Labels, LabelWithheld) {
				t.Errorf("failed sources %v, labels %v: want logs failed and labelled withheld", c.FailedSources, logs.Labels)
			}
			if !strings.Contains(logs.Text, "could not fetch: withheld by redaction\n") ||
				strings.Contains(logs.Text, servingLine) != tc.excerpt {
				t.Errorf("block %q", logs.Text)
			}
			checkCounted(t, before, map[int]float64{reasonWithheld: 1})
		})
	}
}
