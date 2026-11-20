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
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// tokenRun is "--token,}" repeated to 36 KiB: the reading of the release before reads each value to the end of
// the run (released.tokenEnd), so its work outgrows the budget of redaction and every entry point withholds it
// (#873, decision of 2026-11-20).
var tokenRun = strings.Repeat("--token,}", 4096)

// withheldLogs is a log source whose redaction withheld the excerpt (providers.ErrWithheld).
type withheldLogs struct{ *sceneSources }

func (withheldLogs) Logs(context.Context, providers.LogQuery) (providers.LogExcerpt, error) {
	return providers.LogExcerpt{}, fmt.Errorf("the log excerpt: %w", providers.ErrWithheld)
}

// checkWithheld checks that r is the result of an answer redaction withheld: could not fetch, labelled, the fixed
// text, no values, and nothing of the value in it.
func checkWithheld(t *testing.T, what string, r Result) {
	t.Helper()
	if r.Status != StatusCouldNotFetch || !slices.Contains(r.Labels, LabelWithheld) || len(r.Values) != 0 {
		t.Errorf("%s: status %q, labels %v, %d values; want could not fetch, withheld, none", what, r.Status, r.Labels,
			len(r.Values))
	}
	if strings.Contains(r.Text, "--token") {
		t.Errorf("%s shows the value it withheld: %q", what, r.Text[:min(len(r.Text), 80)])
	}
}

// An answer redaction withholds is shown as a source that failed, at every read tool: could not fetch, labelled
// withheld by redaction, with no value of it (#873).
func TestToolsShowAWithheldAnswerAsCouldNotFetch(t *testing.T) {
	set, src := diffSet(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNS},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container, Args: []string{tokenRun}}}},
	}
	checkWithheld(t, NameGetResource, src.run(set, diffScene{pod: pod}, NameGetResource,
		map[string]any{argKind: kindPod, argNamespace: testNS, argName: podName}))
	evs := eventsScene(redactInput(tokenRun))
	checkWithheld(t, NameListEvents, src.run(set, diffScene{events: evs}, NameListEvents, map[string]any{argNamespace: testNS}))
	series := []providers.Series{{Labels: map[string]string{"pod": podName, "x": tokenRun},
		Samples: []providers.Sample{{Time: testNow, Value: 1}}}}
	checkWithheld(t, NameQueryMetrics, src.run(set, diffScene{pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName,
		Namespace: testNS}}, series: series}, NameQueryMetrics,
		map[string]any{argTemplate: metricTemplates[2].name, argNamespace: testNS, argPod: podName}))
	checkWithheld(t, NameGetDeliveryStatus, src.run(set, diffScene{delivery: providers.DeliveryStatus{Message: redact.Withheld}},
		NameGetDeliveryStatus, map[string]any{argKind: kustomization, argNamespace: fluxNS, argName: unitName}))
	podLogs := map[string]any{argNamespace: testNS, argPod: podName, argContainer: container, argMaxBytes: MaxLogBytes}
	// Withheld by the redaction of the excerpt here (18 KiB, within twice what was asked), and by the log source's.
	excerpt := providers.LogExcerpt{Text: tokenRun[:len(tokenRun)/2]}
	checkWithheld(t, NameGetPodLogs+" (excerpt)", src.run(set, diffScene{excerpt: excerpt},
		NameGetPodLogs, podLogs))
	logSet, err := New(Config{Pods: src, Events: src, Objects: src, Logs: withheldLogs{src}, Redactor: set.redactor,
		Namespaces: []string{testNS}, Now: set.now})
	if err != nil {
		t.Fatal(err)
	}
	checkWithheld(t, NameGetPodLogs+" (source)", src.run(logSet, diffScene{}, NameGetPodLogs, podLogs))
}

// redactInput is an input of the fuzz grammar whose fields are all s.
func redactInput(s string) redacttest.Input { return redacttest.Input{Fields: []string{s, s}, Knob: 2} }
