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
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/redact"
)

// A section redaction withholds whole is could not fetch, labelled withheld by redaction, with a fixed text and
// nothing of the source, and its section is a failed source, so the context escalates (#873, decision of
// 2026-11-20). "--token,}" repeated costs the reading of the release before more than the budget of redaction
// (released.tokenEnd), so every entry point withholds it.
func TestWithheldSectionsAreFailedSources(t *testing.T) {
	run := strings.Repeat("--token,}", 8192)
	d := deployment()
	d.Status.Conditions[0].Message = run
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	p := crashing(pod(crashPod, rs.Labels, longAgo, controllerRef(kindReplicaSet, rs.Name, rs.UID)), "exit")
	src := sources{logs: &fake.Logs{}, metrics: &fake.Metrics{}}
	src.metrics.Set("container_restarts_total", []providers.Series{{
		Labels: map[string]string{labelPod: crashPod, labelContainer: run}, Samples: []providers.Sample{{Value: 4}},
	}})
	c := build(t, newBuilder(t, reader(t, d, rs, p), src, nil), incident(deploymentRef()))
	for _, s := range []Section{SectionStatus, SectionMetrics} {
		blk := one(t, c, s)
		if blk.Availability != CouldNotFetch || !slices.Contains(blk.Labels, LabelWithheld) || len(blk.Values) != 0 ||
			blk.Text != withheldText {
			t.Errorf("%s: availability %q, labels %v, %d values, text %q", s, blk.Availability, blk.Labels,
				len(blk.Values), blk.Text[:min(len(blk.Text), 80)])
		}
		if !slices.Contains(c.FailedSources, s) {
			t.Errorf("%s is not a failed source: %v", s, c.FailedSources)
		}
	}
	if !c.Escalate() {
		t.Error("a context with withheld sections does not escalate")
	}
	for _, blk := range c.Blocks {
		if strings.Contains(blk.Text, "--token,}--token") {
			t.Errorf("section %q shows the withheld text", blk.Section)
		}
	}
}

// A value of the log header that redaction withholds (the container ID or the pod UID of the instance) is a
// withheld excerpt, as a withheld body is: the read is LogReadWithheld, the header says withheld by redaction,
// and nothing of the answer is shown, neither the marker in the header nor the body (#873, decision of
// 2026-11-20). Both modes: the context builder (refit false) and get_pod_logs (refit true).
func TestRenderLogWithheldHeaderValue(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	run := strings.Repeat("--token,}", 8192)
	const body = "starting server on port 8080\n"
	for _, field := range []string{"ContainerID", "PodUID"} {
		in := providers.LogInstance{PodUID: "uid-1", ContainerID: "containerd://abc"}
		if field == "ContainerID" {
			in.ContainerID = run
		} else {
			in.PodUID = run
		}
		for _, refit := range []bool{false, true} {
			ex := providers.LogExcerpt{Text: body, Instance: in, CollectedAt: testNow}
			v := RenderLog(red, refit, "pod p container c, current instance", ex, nil, 4096, 8192)
			if v.Read != LogReadWithheld || strings.Contains(v.Text, redact.Withheld) ||
				strings.Contains(v.Text, "starting server") || strings.Contains(v.Text, "--token") ||
				!strings.Contains(v.Text, "withheld by redaction") {
				t.Errorf("%s withheld, refit %v: read %d, text %q", field, refit, v.Read, v.Text[:min(len(v.Text), 200)])
			}
		}
	}
}

// A log header value or a log body that spells the withheld marker is ordinary text: withheld is told by a flag of
// the redactor, never by the text (#873, decision of 2026-11-20), so a writable value cannot blank the log section
// by spelling the marker. Each field alone, both modes.
func TestRenderLogMarkerTextIsNotWithheld(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"container id", "pod uid", "body"} {
		ex := providers.LogExcerpt{Text: "starting server on port 8080\n", CollectedAt: testNow,
			Instance: providers.LogInstance{PodUID: "uid-1", ContainerID: "containerd://abc"}}
		switch field {
		case "container id":
			ex.Instance.ContainerID = redact.Withheld
		case "pod uid":
			ex.Instance.PodUID = redact.Withheld
		default:
			ex.Text = redact.Withheld
		}
		for _, refit := range []bool{false, true} {
			v := RenderLog(red, refit, "pod p container c, current instance", ex, nil, 4096, 8192)
			if v.Read != LogReadOK || !strings.Contains(v.Text, redact.Withheld) ||
				strings.Contains(v.Text, "withheld by redaction") {
				t.Errorf("%s spells the marker, refit %v: read %d, text %q", field, refit, v.Read, v.Text)
			}
		}
	}
}

// A Redactor has the flag of a withheld text (decision of 2026-11-21 on #873): a redactor with String alone, or a
// wrapper that keeps only String, cannot be a Redactor, so the builder never takes a withheld text for redacted
// text. Before, such a redactor was read with String and a section it withheld was shown as read.
func TestRedactorRequiresTheCheckedForm(t *testing.T) {
	m, ok := reflect.TypeFor[Redactor]().MethodByName("StringChecked")
	if !ok {
		t.Fatal("Redactor has no StringChecked: a redactor without the flag of a withheld text would be accepted")
	}
	if want := reflect.TypeFor[func(string) (string, bool)](); m.Type != want {
		t.Errorf("StringChecked is %v, want %v", m.Type, want)
	}
	type stringOnly interface{ String(s string) string }
	if reflect.TypeFor[RedactFunc]().Implements(reflect.TypeFor[Redactor]()) ||
		!reflect.TypeFor[RedactFunc]().Implements(reflect.TypeFor[stringOnly]()) {
		t.Error("a function of String alone is a Redactor")
	}
}
