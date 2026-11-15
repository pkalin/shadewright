package eval

import (
	"strings"
	"testing"
)

// The memory-limit fact of oom takes the value from any source the context holds: the pod template line
// ("48Mi") or the limit metric, and a reply without the value fails it.
func TestLimitFactAcceptsAnySourceOfTheValue(t *testing.T) {
	_, s := mustScenario(t, scnOOM)
	var fact *rubricFact
	for i := range s.rubric.Facts {
		if strings.Contains(s.rubric.Facts[i].Fact, "memory working set") {
			fact = &s.rubric.Facts[i]
		}
	}
	if fact == nil {
		t.Fatal("oom has no working set fact")
	}
	one := rubric{Facts: []rubricFact{*fact}}
	subject := subjectText(s.incident)

	for name, tc := range map[string]struct {
		text string
		pass bool
	}{
		"limit from the pod template line": {
			"The working set peaks at 5.0135e+07 bytes against the pod template memory limit 48Mi.", true},
		"limit from the limit metric": {
			"container_memory_working_set_bytes peaks at 5.0135e+07 and container_memory_limit_bytes is 5.03316e+07.", true},
		"limit in bytes only": {
			"The working set peaks at 50135040 bytes, the limit is 50331648 bytes.", true},
		"no limit at all": {
			"The working set peaks at 5.0135e+07 bytes, so the container is killed.", false},
		"limit without the working set": {
			"The pod template memory limit is 48Mi and the container is killed.", false},
		"metric name without its value": {
			"container_memory_working_set_bytes is high.", false},
	} {
		got := scoreDiagnosis(one, tc.text, subject)
		if got.Pass != tc.pass {
			t.Errorf("%s: pass %t, want %t: %+v", name, got.Pass, tc.pass, got.Facts)
		}
	}
}

// keda-at-ceiling's backlog fact needs the current value of the HPA's metric and its target with its value,
// from the metric lines or in words; a reply that names the metric without its values fails it.
func TestKedaBacklogNeedsTheCurrentValueAndTheTarget(t *testing.T) {
	_, s := mustScenario(t, scnKedaCeiling)
	var fact *rubricFact
	for i := range s.rubric.Facts {
		if strings.Contains(s.rubric.Facts[i].Fact, "backlog") {
			fact = &s.rubric.Facts[i]
		}
	}
	if fact == nil || fact.Unscored != "" {
		t.Fatalf("keda-at-ceiling's backlog fact is missing or unscored: %+v", fact)
	}
	one := rubric{Facts: []rubricFact{*fact}}
	subject := subjectText(s.incident)
	for name, tc := range map[string]struct {
		text string
		pass bool
	}{
		"metric lines":          {"hpa_metric_current{...} last 6.5 and hpa_metric_target{...} 5\nmore", true},
		"in words, per replica": {"The metric is at 6.5 per replica against a target of 5.", true},
		"in words, in all":      {"About 13 requests per second in all; the target is 5 per replica.", true},
		"no target":             {"The metric is at 6.5 per replica.", false},
		"no current value":      {"The metric is above its target of 5.", false},
		"metric names only":     {"hpa_metric_current is above hpa_metric_target.", false},
		"16.5 and 50":           {"The metric is at 16.5 per replica against a target of 50 per replica.", false},
		"130 and 50 in a line": {"hpa_metric_current{...} last 16.5 and hpa_metric_target{...} 50\n" +
			"more; about 130 in all", false},
		"target line, no value": {"hpa_metric_target{horizontalpodautoscaler=keda-hpa-worker} is what it scales on, " +
			"at 6.5 per replica", false},
	} {
		got := scoreDiagnosis(one, tc.text, subject)
		if got.Pass != tc.pass {
			t.Errorf("%s: pass %t, want %t: %+v", name, got.Pass, tc.pass, got.Facts)
		}
	}
}

// A subject fact is scored from the Incident: the reply need not repeat the subject, and a rubric whose
// subject terms are not in the Incident fails.
func TestSubjectFactIsScoredFromTheIncident(t *testing.T) {
	_, s := mustScenario(t, "invalid-manifest")
	subject := subjectText(s.incident)
	if subject != "Kustomization flux-system/apps" {
		t.Fatalf("subject %q", subject)
	}
	r := rubric{Facts: []rubricFact{{
		Fact: "the failing Kustomization", Subject: []string{"Kustomization", "flux-system/apps"},
	}}}
	if got := scoreDiagnosis(r, "The dry-run failed.", subject); !got.Pass {
		t.Errorf("a reply without the subject failed a subject fact: %+v", got)
	}
	r.Facts[0].Subject = []string{"Kustomization", "flux-system/other"}
	if got := scoreDiagnosis(r, "Kustomization flux-system/other failed.", subject); got.Pass {
		t.Errorf("a subject the Incident does not hold passed from the reply: %+v", got)
	}
}

// The facts the model must infer stay required: cache-0 healthy on the old template, the pull error.
func TestInferredFactsStayRequired(t *testing.T) {
	_, s := mustScenario(t, "statefulset-oom")
	subject := subjectText(s.incident)
	text := "cache-1 restarts 2 and is OOMKilled; the memory limit is 48Mi and the working set is 5.01187e+07."
	got := scoreDiagnosis(s.rubric, text, subject)
	if got.Pass {
		t.Fatalf("a reply without cache-0 passed: %+v", got)
	}
	for _, f := range got.Facts {
		if strings.Contains(f.Fact, "cache-0") == f.Pass {
			t.Errorf("fact %q pass %t", f.Fact, f.Pass)
		}
	}

	_, s = mustScenario(t, scnBadImage)
	got = scoreDiagnosis(s.rubric, "podinfo:6.17.99 is in ImagePullBackOff.", subjectText(s.incident))
	if got.Pass {
		t.Errorf("bad-image without the not-found cause passed: %+v", got)
	}
}

// A rubric fact with an empty anyOf group, or a subject only fact that is also marked unscored, is refused.
func TestRubricRefusesAnEmptyAnyOfGroup(t *testing.T) {
	_, s := mustScenario(t, scnOOM)
	s.rubric.Facts[2].AnyOf = [][]string{{"48Mi"}, {}}
	if err := s.checkRubric(); err == nil || !strings.Contains(err.Error(), "empty anyOf group") {
		t.Errorf("err %v", err)
	}
	_, s = mustScenario(t, scnOOM)
	s.rubric.Facts[2].Unscored = "why"
	if err := s.checkRubric(); err == nil {
		t.Error("a scored and unscored fact was accepted")
	}
}
