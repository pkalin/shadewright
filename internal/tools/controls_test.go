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

package tools

import (
	"strings"
	"testing"
	"unicode"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

// removedControl reports whether r is a code point a tool result is shown without: Unicode category Cc except
// newline and tab (#852).
func removedControl(r rune) bool { return unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t' }

// Every tool's result is shown without control characters but newline and tab, in its text and in every part a
// quote may cite, after redaction (decided 2026-11-17 on #852), and the visible text around them survives.
// Each tool is listed: with the text from its source that held controls, or with why its text is fixed. A tool
// added later fails until it is listed.
func TestToolResultsHaveNoControls(t *testing.T) {
	cases := map[string]struct {
		visible string // text of the source, as shown without its controls
		fixed   string // why the tool shows no text from a source
	}{
		NameGetResource:           {visible: `db-creds\" not found`},
		NameListEvents:            {visible: "Back-off[0m restarting failed"},
		NameGetPodLogs:            {visible: "[1;31mERROR[0m connection refused"},
		NameQueryMetrics:          {visible: "container=side"},
		NameGetDeliveryStatus:     {visible: "health[0m check failed"},
		NameCheckMissingReference: {fixed: "answers in fixed words about the pod spec"},
		NameReadGitFile:           {fixed: "the analyzer has no Git identity: a fixed text"},
		NameGetRecentChanges:      {fixed: "the analyzer has no Git identity: a fixed text"},
	}
	for name := range validArgs {
		if _, ok := cases[name]; !ok {
			t.Errorf("tool %s is not listed", name)
		}
	}
	p := pod()
	p.Status.ContainerStatuses[0].State.Waiting.Message = "secret \"db-\x1bcreds\" not\u009b found\r"
	e := newEnv(t, []client.Object{p, deployment(),
		event("e1", podName, "Back-off\x1b[0m restarting\x07 failed", testNow)})
	key := fake.LogKey{Namespace: testNS, Pod: podName, Container: container, Previous: true}
	e.logs.Set(key, "starting\n\x1b[1;31mERROR\x1b[0m connection refused\r\n")
	e.logs.SetInstance(key, providers.LogInstance{PodUID: "uid-pod-logs", RestartCount: 3}, false)
	e.metrics.PodTemplates = []string{restartsTemplate}
	e.metrics.Set(restartsTemplate, []providers.Series{{Labels: map[string]string{argPod: podName, argContainer: "si\x1bde"},
		Samples: []providers.Sample{{Value: 4}}}})
	e.delivery.Set(providers.DeliveryUnit{Kind: kustomization, Namespace: fluxNS, Name: unitName},
		providers.DeliveryStatus{AppliedRevision: "main@sha1:def", Message: "health\x1b[0m check\u0085 failed\x7f"})
	for name, tc := range cases {
		r := e.call(t, name, with(name, nil))
		if strings.ContainsFunc(r.Text, removedControl) {
			t.Errorf("%s shows a control:\n%q", name, r.Text)
		}
		found := false
		for _, d := range r.Data() {
			if strings.ContainsFunc(d, removedControl) {
				t.Errorf("%s has a quotable part with a control: %q", name, d)
			}
			found = found || strings.Contains(d, tc.visible)
		}
		if tc.fixed == "" && !found {
			t.Errorf("%s: no quotable part holds %q:\n%q", name, tc.visible, r.Text)
		}
	}
}
