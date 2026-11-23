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

package redact_test

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/redacttest"
	"github.com/pkalin/shadewright/internal/tools"
)

// Secrets a control character splits, which origin/main showed to the model (count of 2026-11-22 on #852): a
// key a NUL splits, a GitHub token a NUL splits and a key an escape splits. The controls view (controls.go) reads
// each without its control, so the log source, the context builder's value and log excerpt, and every read tool
// that shows text from the cluster hide the secret. Each output keeps the controls of the text.
func TestSecretsSplitByAControlAreHidden(t *testing.T) {
	tok := redacttest.GateProbeToken
	cases := []struct{ in, secret string }{
		{"pass\x00word=mysecretvalue", "mysecretvalue"},
		{"token " + tok[:20] + "\x00" + tok[20:], tok[20:]},
		{"pass\x1bword=Hunter2Secret9x", "Hunter2Secret9x"},
	}
	names := []string{canonEntryExcerpt, canonEntryValue, tools.NameGetPodLogs, tools.NameListEvents,
		tools.NameGetResource, tools.NameQueryMetrics, tools.NameGetDeliveryStatus}
	for _, c := range cases {
		for _, name := range names {
			for _, out := range entryPoints[name](c.in) {
				if strings.Contains(out, c.secret) {
					t.Errorf("%s shows %q of %q:\n%q", name, c.secret, c.in, out)
				}
			}
		}
		if out := queryMetricsOfLabels([][2]string{{"note", c.in}}); strings.Contains(out, c.secret) {
			t.Errorf("query_metrics shows %q of the label value %q:\n%q", c.secret, c.in, out)
		}
	}
}
