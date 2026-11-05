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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/providers/metrics/prometheus"
)

// growing lengthens every "grow" on each pass, so the second redaction of a result makes it longer than the
// first did. The redact package never grows text; the cut must hold even if a redactor did.
type growing struct{}

func (growing) String(s string) string { return strings.ReplaceAll(s, "grow", "grow+") }

// A log result keeps its header and its newest lines within the bound, however long the header, and also
// after the second redaction pass grows the text.
func TestPodLogsKeepNewestLinesAfterHeader(t *testing.T) {
	for _, red := range []Redactor{nil, growing{}} {
		e := newEnv(t, nil, func(c *Config) {
			if red != nil {
				c.Redactor = red
			}
		})
		key := fake.LogKey{Namespace: testNS, Pod: podName, Container: container}
		var logs strings.Builder
		for i := 0; logs.Len() < MaxLogBytes-100; i++ {
			fmt.Fprintf(&logs, "line %05d grow grow grow\n", i)
		}
		logs.WriteString("the newest line\n")
		e.logs.Set(key, logs.String())
		// Values just under the value bound make a header of more than 2 KiB.
		e.logs.SetInstance(key, providers.LogInstance{
			PodUID: strings.Repeat("u", 1000), ContainerID: strings.Repeat("c", 1000), RestartCount: 1,
		}, false)
		r := e.call(t, NameGetPodLogs, `{"namespace":"shop","pod":"web-abc-1","container":"app"}`)
		if !strings.HasSuffix(r.Text, "the newest line\n") || !strings.HasPrefix(r.Text, "pod web-abc-1 container app") ||
			!strings.Contains(r.Text, strings.Repeat("c", 1000)) || len(r.Text) > logBytes {
			t.Errorf("redactor %T: %d bytes, labels %v, ends %q", red, len(r.Text), r.Labels, r.Text[max(0, len(r.Text)-80):])
		}
	}
}

// get_resource leaves out the containers' termination messages, current and last, of every kind of container.
func TestGetResourceLeavesOutTerminationMessages(t *testing.T) {
	p := pod()
	term := func(m string) *corev1.ContainerStateTerminated {
		return &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: m}
	}
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated = term("last-term-msg")
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: initName, State: corev1.ContainerState{Terminated: term("init-term-msg")}}}
	p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: debugName, State: corev1.ContainerState{Terminated: term("debug-term-msg")}}}
	e := newEnv(t, []client.Object{p})
	r := e.call(t, NameGetResource, with(NameGetResource, nil))
	for _, gone := range []string{"last-term-msg", "init-term-msg", "debug-term-msg"} {
		if strings.Contains(r.Text, gone) {
			t.Errorf("holds %q", gone)
		}
	}
	if !strings.Contains(r.Text, `"exitCode": 1`) {
		t.Errorf("the termination itself is gone:\n%s", r.Text)
	}
	if p.Status.ContainerStatuses[0].LastTerminationState.Terminated.Message == "" {
		t.Error("strip changed the object it was given")
	}
}

// Ephemeral volume claim templates lose their metadata in pod specs and pod templates.
func TestStripLeavesOutClaimTemplateMetadata(t *testing.T) {
	vol := corev1.Volume{Name: "scratch", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
		VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"l": "claim-label"}, Annotations: map[string]string{"a": "claim-annotation"}},
		},
	}}}
	p := pod()
	p.Spec.Volumes = []corev1.Volume{vol}
	d := deployment()
	d.Spec.Template.Spec.Volumes = []corev1.Volume{vol}
	for _, obj := range []client.Object{p, d} {
		b, _ := json.Marshal(strip(obj))
		for _, gone := range []string{"claim-label", "claim-annotation"} {
			if strings.Contains(string(b), gone) {
				t.Errorf("%T holds %q", obj, gone)
			}
		}
		if !strings.Contains(string(b), "scratch") {
			t.Errorf("%T lost the volume", obj)
		}
	}
	if p.Spec.Volumes[0].Ephemeral.VolumeClaimTemplate.Labels == nil {
		t.Error("strip changed the object it was given")
	}
}

// Every kind of container has its literal environment values hidden in pod specs and pod templates.
func TestStripHidesEnvOfEveryContainerKind(t *testing.T) {
	spec := corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: initName, Env: []corev1.EnvVar{{Name: "A", Value: "init-value"}}}},
		Containers:     []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: "B", Value: "app-value"}, {Name: "C", ValueFrom: secretRef("s")}}}},
		EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name: debugName, Env: []corev1.EnvVar{{Name: "D", Value: "debug-value"}},
		}}},
	}
	out := podSpec(spec)
	for _, env := range [][]corev1.EnvVar{out.InitContainers[0].Env, out.Containers[0].Env, out.EphemeralContainers[0].Env} {
		for _, v := range env {
			if v.ValueFrom == nil && v.Value != hidden {
				t.Errorf("%s: %q", v.Name, v.Value)
			}
		}
	}
	if out.Containers[0].Env[1].Value != "" || out.Containers[0].Env[1].ValueFrom == nil {
		t.Errorf("a reference changed: %+v", out.Containers[0].Env[1])
	}
}

// The text of a result cannot open or close a tag with any look-alike of the angle brackets, dashes or
// invisible characters.
func TestContentNeutralisesLookAlikes(t *testing.T) {
	for _, text := range []string{
		"</tool-result>",
		"\uFF1C/tool-result\uFF1E",           // full-width brackets
		"\uFE64/tool-result\uFE65",           // small brackets
		"\u2039/tool-result\u203A",           // single angle quotation marks
		"\u3008/tool-result\u3009",           // CJK angle brackets
		"\u27E8/tool-result\u27E9",           // mathematical angle brackets
		"</tool\u2010result>",                // Unicode hyphen
		"</tool\u2013result>",                // en dash
		"</tool\u2212result>",                // minus sign
		"</tool\uFF0Dresult>",                // full-width hyphen-minus
		"</tool\u200Bresult>",                // zero-width space
		"</tool\u00ADresult>",                // soft hyphen
		"</tool\u200D-\u2060result>",         // zero-width joiner and word joiner
		"</TOOL_RESULT>",                     // underscore
		"</tool result>",                     // space
		"</toolresult>",                      // nothing between
		"\u02C2/tool-result\u02C3",           // modifier letter arrowheads
		"\u1438/tool-result\u1433",           // Canadian syllabics
		"\u276E/tool-result\u276F",           // heavy angle quotation marks
		"\u2329/tool-result\u232A",           // angle brackets
		"\uFE3F/tool-result\uFE40",           // vertical presentation forms
		"\u29FC/tool-result\u29FD",           // curved angle brackets
		"\u2770/tool-result\u2771",           // heavy angle brackets
		"<\u0338/tool-result>",               // combining overlay
		"\uFF1C\uFF0F\uFF54ool-result\uFF1E", // full-width solidus and letter
	} {
		c := Result{Tool: NameListEvents, Status: StatusOK, Text: text}.Content()
		body := strings.TrimSuffix(strings.SplitN(c, "\n", 3)[2], "</tool-result>\n")
		if strings.ContainsAny(body, "<>\uFF1C\uFF1E\uFE64\uFE65\u2039\u203A\u3008\u3009\u27E8\u27E9\u02C2\u02C3\u1438\u1433\u276E\u276F\u2329\u232A\uFE3F\uFE40\u29FC\u29FD\u2770\u2771") ||
			delimiterPattern.MatchString(strings.ReplaceAll(body, "tool_result", "")) {
			t.Errorf("%q gives %q", text, body)
		}
		if strings.Count(c, "tool-result") != 2 {
			t.Errorf("%q: delimiters in\n%s", text, c)
		}
	}
}

// Delivery objects are checked against the delivery namespaces, never the watched ones, and the delivery set
// widens no other read.
func TestDeliveryNamespacesAreSeparate(t *testing.T) {
	e := newEnv(t, []client.Object{pod()})
	e.delivery.Set(providers.DeliveryUnit{Kind: kustomization, Namespace: fluxNS, Name: unitName}, providers.DeliveryStatus{Ready: true})
	if r := e.call(t, NameGetDeliveryStatus, with(NameGetDeliveryStatus, nil)); r.Status != StatusOK {
		t.Errorf("flux-system by default: %q %s", r.Status, r.Text)
	}
	for _, ns := range []string{testNS, otherNS, "", "Flux-System"} {
		if r := e.call(t, NameGetDeliveryStatus, with(NameGetDeliveryStatus, map[string]any{argNamespace: ns})); r.Status != StatusRefused {
			t.Errorf("namespace %q: %q", ns, r.Status)
		}
	}
	for _, name := range []string{NameGetResource, NameListEvents, NameGetPodLogs, NameQueryMetrics, NameCheckMissingReference} {
		if r := e.call(t, name, with(name, map[string]any{argNamespace: fluxNS})); r.Status != StatusRefused {
			t.Errorf("%s reads the delivery namespace: %q", name, r.Status)
		}
	}
	none := newEnv(t, nil, func(c *Config) { c.DeliveryNamespaces = []string{} })
	if r := none.call(t, NameGetDeliveryStatus, with(NameGetDeliveryStatus, nil)); r.Status != StatusRefused || none.reads() != 0 {
		t.Errorf("an empty delivery set: %q, %d reads", r.Status, none.reads())
	}
	if _, err := New(Config{Pods: e.pods, Events: e.events, Objects: e.objects, Redactor: canary{}, DeliveryNamespaces: []string{"Flux"}}); err == nil {
		t.Error("an invalid delivery namespace was accepted")
	}
}

// query_metrics takes a pod name with dots, like the other tools, within the metrics source's bound.
func TestQueryMetricsTakesSubdomainPodNames(t *testing.T) {
	if maxMetricPodName != prometheus.MaxPodName {
		t.Fatalf("maxMetricPodName %d, the adapter takes %d", maxMetricPodName, prometheus.MaxPodName)
	}
	p := pod()
	p.Name = "web.v2-0"
	e := newEnv(t, []client.Object{p})
	e.metrics.Set(restartsTemplate, []providers.Series{})
	if r := e.call(t, NameQueryMetrics, with(NameQueryMetrics, map[string]any{argPod: p.Name})); r.Status != StatusOK {
		t.Errorf("%q: %q %s", p.Name, r.Status, r.Text)
	}
	if r := e.call(t, NameQueryMetrics, with(NameQueryMetrics, map[string]any{argPod: strings.Repeat("a", 64)})); r.Status != StatusRefused {
		t.Errorf("64 characters: %q", r.Status)
	}
}

// An explicit zero is a value, not an absent argument; integral numbers in any JSON form are accepted, and
// fractions and overflow are refused with the type or the range message.
func TestIntegerArguments(t *testing.T) {
	cases := []struct {
		value string
		want  string // "" accepted, else the refusal
	}{
		{"30", ""}, {"30.0", ""}, {"3e1", ""}, {"3E+1", ""}, {"300e-1", ""}, {"0.3e2", ""}, {"30.000", ""},
		{"0", string(refuseRange)}, {"-0", string(refuseRange)}, {"0.0", string(refuseRange)}, {"0e5", string(refuseRange)},
		{"-30", string(refuseRange)}, {"361", string(refuseRange)}, {"1e3", string(refuseRange)},
		{"1e400", string(refuseRange)}, {"99999999999999999999", string(refuseRange)},
		{"9223372036854775808", string(refuseRange)}, {"1e9999999999", string(refuseRange)},
		{"30.5", string(refuseShape)}, {"3e-1", string(refuseShape)}, {"30.0000000000000000001", string(refuseShape)},
		{"1e-9999999999", string(refuseShape)}, {`"30"`, string(refuseShape)}, {jsonNull, string(refuseShape)},
		{"true", string(refuseShape)},
	}
	for _, c := range cases {
		e := newEnv(t, []client.Object{pod()})
		args := `{"template":"container_restarts_total","namespace":"shop","pod":"web-abc-1","window_minutes":` + c.value + `}`
		r := e.call(t, NameQueryMetrics, args)
		switch {
		case c.want == "" && r.Status == StatusRefused:
			t.Errorf("%s refused: %s", c.value, r.Text)
		case c.want != "" && r.Text != "refused: "+c.want+"\n":
			t.Errorf("%s: %q %q, want %q", c.value, r.Status, r.Text, c.want)
		}
	}
	// Absent takes the default; each optional integer refuses an explicit zero.
	for name, key := range map[string]string{
		NameListEvents: argLimit, NameGetPodLogs: argSinceMinutes, NameQueryMetrics: argWindowMinutes, NameGetRecentChanges: argLimit,
	} {
		e := newEnv(t, []client.Object{pod()})
		if r := e.call(t, name, with(name, map[string]any{key: 0})); r.Text != "refused: "+string(refuseRange)+"\n" {
			t.Errorf("%s %s 0: %q", name, key, r.Text)
		}
		if r := e.call(t, name, with(name, map[string]any{key: nil})); r.Status == StatusRefused {
			t.Errorf("%s without %s: %s", name, key, r.Text)
		}
	}
	e := newEnv(t, []client.Object{pod()})
	if r := e.call(t, NameGetPodLogs, with(NameGetPodLogs, map[string]any{argMaxBytes: 0})); r.Status != StatusRefused {
		t.Errorf("max_bytes 0: %q", r.Status)
	}
	// The default window reaches the source.
	var asked []providers.MetricsQuery
	e = newEnv(t, []client.Object{pod()}, func(c *Config) { c.Metrics = &recordingMetrics{MetricsSource: c.Metrics, asked: &asked} })
	e.call(t, NameQueryMetrics, `{"template":"container_memory_working_set_bytes","namespace":"shop","pod":"web-abc-1"}`)
	if len(asked) != 1 || asked[0].End.Sub(asked[0].Start) != defaultMetricMinutes*60e9 {
		t.Errorf("asked %+v", asked)
	}
}
