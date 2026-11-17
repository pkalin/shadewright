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
	"sync"
	"testing"

	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/providers/logs/kube"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// entryPoints are the functions that turn text read from the cluster into text for the model, a status, an
// Event or a log line. Each rewrites the text (characters replaced, lines joined, values or lines cut), so
// each must redact the text as it was read first (#840, decision of 2026-11-17, second).
var entryPoints = map[string]func(in string) []string{
	"fingerprint.Normalize": func(in string) []string {
		return []string{fingerprint.Normalize(fingerprint.Observation{Reason: toolReason, Note: in})}
	},
	"fingerprint.Symptom": func(in string) []string {
		return []string{fingerprint.Symptom(fingerprint.Observation{Reason: toolReason, Note: in})}
	},
	"fingerprint.Value":  func(in string) []string { return []string{fingerprint.Value(in)} },
	"modules.BoundValue": func(in string) []string { return []string{modules.BoundValue(in)} },
	"incidentcontext.Value": func(in string) []string {
		return []string{incidentcontext.Value(redact.String, in)}
	},
	// The context builder's summary shows the symptom an Incident stores through Value, then cuts it.
	"incidentcontext.Value of the symptom": func(in string) []string {
		return []string{incidentcontext.Value(redact.String, fingerprint.Symptom(fingerprint.Observation{Note: in}))}
	},
	// The read tools of the model: the answer of get_resource is JSON, whose escapes must not hide an
	// indicator (an HTML-escaped ">" did, #840).
	tools.NameGetResource: toolText(tools.NameGetResource,
		map[string]any{argKind: toolKind, argNamespace: toolNS, argName: toolPod}),
	tools.NameListEvents: toolText(tools.NameListEvents,
		map[string]any{argNamespace: toolNS, argKind: toolKind, argName: toolPod, "limit": 5}),
	tools.NameGetPodLogs: toolText(tools.NameGetPodLogs,
		map[string]any{argNamespace: toolNS, argPod: toolPod, argContainer: toolContainer}),
	// The text as the value of a metric label, as a delivery object's message, and as the note of an Event the
	// reference check reads (#845: every tool that shows text from a source, TestEveryToolIsAnEntryPoint).
	tools.NameQueryMetrics: toolText(tools.NameQueryMetrics,
		map[string]any{argNamespace: toolNS, argPod: toolPod, argTemplate: toolTemplate}),
	tools.NameGetDeliveryStatus: toolText(tools.NameGetDeliveryStatus,
		map[string]any{argKind: "Kustomization", argNamespace: toolNS, argName: toolDelivery}),
	tools.NameCheckMissingReference: toolText(tools.NameCheckMissingReference,
		map[string]any{argNamespace: toolNS, argPod: toolPod, argKind: "Secret", argName: "db-creds"}),
	// The log excerpt, whole and cut after each of its lines: a cut that drops the key's line must not
	// leave its block bare.
	"kube.Excerpt": func(in string) []string {
		out := []string{}
		text, _ := kube.Excerpt(defaultRedactor(), []byte(in), false, len(in))
		out = append(out, text)
		for i := range len(in) {
			if in[i] == '\n' && i+1 < len(in) {
				text, _ := kube.Excerpt(defaultRedactor(), []byte(in), false, len(in)-i-1)
				out = append(out, text)
			}
		}
		return out
	},
}

// Every input of both block corpora goes through every entry point, and no secret survives in any output.
// A form closed in the redactor and open in a caller fails here. At 6ed30275 (the second build of #840) the
// folded indicator ">" was replaced before redaction in the fingerprint's normalization, and the log excerpt
// was cut before it was redacted; see the counts in the pull request of #840. Under -race each entry point
// gets a seeded sample of the corpus (redact.RaceKeeps); make test runs the whole corpus without -race. The
// entry points run in parallel. No linearity test counts work meanwhile: a top-level test ends only after its
// parallel subtests, and no top-level test of the package is parallel.
func TestBlockCorporaThroughEveryEntryPoint(t *testing.T) {
	cases := redact.EntryCorpus()
	for name, f := range entryPoints {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			leaked, ran := 0, 0
			for i, c := range cases {
				if !redact.RaceKeeps(name, i) {
					continue
				}
				ran++
				for _, out := range f(c.In) {
					if s := leakedSecret(out, c.Secrets); s != "" {
						leaked++
						if leaked <= 5 {
							t.Errorf("%s:\n  in: %q\n out: %q\n leaks %q", c.Name, c.In, out, s)
						}
						break
					}
				}
			}
			if leaked > 0 {
				t.Errorf("%s: %d of %d inputs leak a secret", name, leaked, ran)
			}
			if ran < len(cases)/(2*redact.RaceSampleEvery) {
				t.Fatalf("%s: only %d of %d inputs ran", name, ran, len(cases))
			}
		})
	}
}

// leakedSecret returns the first of secrets found in out, or "".
func leakedSecret(out string, secrets []string) string {
	for _, s := range secrets {
		if strings.Contains(out, s) {
			return s
		}
	}
	return ""
}

// defaultRedactor returns a redactor with the built-in rules, as the log source uses without extra patterns.
// It is built once: New compiles every rule, which cost more than the redaction of a corpus input when each
// call of an entry point built its own.
var defaultRedactor = sync.OnceValue(func() *redact.Redactor {
	r, err := redact.New()
	if err != nil {
		panic(err)
	}
	return r
})
