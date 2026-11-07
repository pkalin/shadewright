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

package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// scenariosDir holds one directory per lab scenario, and source.yaml.
const scenariosDir = "testdata/scenarios"

// fixtureNow is the time the recorded contexts are built at: every timestamp in the fixtures is shortly
// before it, so the context builder's window covers them.
var fixtureNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// watchedNamespaces is the analyzer's --watch-namespaces in the lab (the demo workloads).
var watchedNamespaces = []string{"demo"}

// source says where the vendored expected.yaml files come from: the rubric field of the manifest.
type source struct {
	// FleetRepository is the repository of the scenarios.
	FleetRepository string `json:"fleetRepository"`
	// FleetCommit is the commit the expected.yaml files were copied from, unchanged.
	FleetCommit string `json:"fleetCommit"`
}

// expected is the part of a scenario's expected.yaml (schema scenarios.shadewright.io/v1alpha1, owned by
// the fleet repository) the harness scores against.
type expected struct {
	APIVersion string `json:"apiVersion"`
	Name       string `json:"name"`
	Incident   struct {
		Reason string `json:"reason"`
		Object struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Namespace  string `json:"namespace"`
			Name       string `json:"name"`
		} `json:"object"`
	} `json:"incident"`
	Diagnosis struct {
		Evidence []struct {
			Fact   string `json:"fact"`
			Source string `json:"source"`
		} `json:"evidence"`
	} `json:"diagnosis"`
	Outcome struct {
		// Type is refusal or pull-request.
		Type   string `json:"type"`
		Reason string `json:"reason"`
		Phase  string `json:"phase"`
		Action string `json:"action"`
		// Container and Previous are optional: the container the plan must name and the limit it replaces.
		Container string `json:"container"`
		Previous  string `json:"previous"`
		Bounds    *struct {
			Min intstr.IntOrString `json:"min"`
			Max intstr.IntOrString `json:"max"`
		} `json:"bounds"`
		Paths []string `json:"paths"`
	} `json:"outcome"`
}

// Outcome types of expected.yaml.
const (
	outcomeRefusal     = "refusal"
	outcomePullRequest = "pull-request"
)

// rubric says how each fact of expected.yaml is scored: by terms that must appear in the diagnosis, or not
// at all, with the reason.
type rubric struct {
	Facts []rubricFact `json:"facts"`
}

// rubricFact scores one fact. It passes when every term of All, and at least one of Any (when Any is set),
// appears in the diagnosis text, compared without case.
type rubricFact struct {
	// Fact is the fact of expected.yaml, verbatim.
	Fact string   `json:"fact"`
	All  []string `json:"all,omitempty"`
	Any  []string `json:"any,omitempty"`
	// Unscored says why the fact is not scored: the recorded context cannot carry it yet.
	Unscored string `json:"unscored,omitempty"`
}

// recording is the model replies recorded for a scenario, replayed in order by the fake model.
type recording struct {
	// Source says how the replies were made: "synthetic" (written by hand to the schema) or the run that
	// recorded them.
	Source  string          `json:"source"`
	Replies []recordedReply `json:"replies"`
}

// recordedReply is one completion.
type recordedReply struct {
	Model     string `json:"model"`
	Content   string `json:"content,omitempty"`
	ToolCalls []struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"toolCalls,omitempty"`
	Usage struct {
		InputTokens       int64 `json:"inputTokens"`
		CachedInputTokens int64 `json:"cachedInputTokens,omitempty"`
		OutputTokens      int64 `json:"outputTokens"`
	} `json:"usage"`
}

func (r recordedReply) completion() providers.Completion {
	c := providers.Completion{Model: r.Model, Content: r.Content, Usage: providers.Usage{
		InputTokens: r.Usage.InputTokens, CachedInputTokens: r.Usage.CachedInputTokens, OutputTokens: r.Usage.OutputTokens,
	}}
	for _, tc := range r.ToolCalls {
		c.ToolCalls = append(c.ToolCalls, providers.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	return c
}

// scenario is one recorded scenario.
type scenario struct {
	name      string
	expected  expected
	rubric    rubric
	incident  *remediationv1alpha1.Incident
	objects   []client.Object
	recording recording
}

func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, eventsv1.AddToScheme, remediationv1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func loadSource() (source, error) {
	var s source
	if err := readStrict(filepath.Join(scenariosDir, "source.yaml"), &s); err != nil {
		return s, err
	}
	if s.FleetRepository == "" || len(s.FleetCommit) != 40 {
		return s, errors.New("source.yaml: fleetRepository and a 40-character fleetCommit are required")
	}
	return s, nil
}

// loadScenarios reads every scenario directory, sorted by name.
func loadScenarios(scheme *runtime.Scheme) ([]*scenario, error) {
	entries, err := os.ReadDir(scenariosDir)
	if err != nil {
		return nil, fmt.Errorf("reading the scenarios: %w", err)
	}
	var out []*scenario
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := loadScenario(scheme, e.Name())
		if err != nil {
			return nil, fmt.Errorf("scenario %s: %w", e.Name(), err)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("no scenarios found")
	}
	return out, nil
}

func loadScenario(scheme *runtime.Scheme, name string) (*scenario, error) {
	return loadScenarioDir(scheme, filepath.Join(scenariosDir, name), name)
}

// loadScenarioDir loads the scenario called name from dir.
func loadScenarioDir(scheme *runtime.Scheme, dir, name string) (*scenario, error) {
	s := &scenario{name: name}
	raw, err := os.ReadFile(filepath.Join(dir, "expected.yaml"))
	if err != nil {
		return nil, err
	}
	// Not strict: the fleet repository owns the schema, and the harness reads only what it scores.
	if err := yaml.Unmarshal(raw, &s.expected); err != nil {
		return nil, fmt.Errorf("expected.yaml: %w", err)
	}
	if s.expected.APIVersion != "scenarios.shadewright.io/v1alpha1" || s.expected.Name != name {
		return nil, fmt.Errorf("expected.yaml: apiVersion %q and name %q do not match the scenario",
			s.expected.APIVersion, s.expected.Name)
	}
	if err := readStrict(filepath.Join(dir, "rubric.yaml"), &s.rubric); err != nil {
		return nil, err
	}
	if err := s.checkRubric(); err != nil {
		return nil, err
	}
	if err := readStrict(filepath.Join(dir, "replies.yaml"), &s.recording); err != nil {
		return nil, err
	}
	decoder := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	raw, err = os.ReadFile(filepath.Join(dir, "incident.yaml"))
	if err != nil {
		return nil, err
	}
	obj, _, err := decoder.Decode(raw, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("incident.yaml: %w", err)
	}
	inc, ok := obj.(*remediationv1alpha1.Incident)
	if !ok {
		return nil, fmt.Errorf("incident.yaml holds a %T, not an Incident", obj)
	}
	s.incident = inc
	if err := s.checkIncident(); err != nil {
		return nil, err
	}
	raw, err = os.ReadFile(filepath.Join(dir, "objects.yaml"))
	if err != nil {
		return nil, err
	}
	// The same splitter as the source mapper (internal/gitops/sourcemap): any valid document separator.
	docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for i := 0; ; i++ {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("objects.yaml document %d: %w", i, err)
		}
		if len(bytes.TrimSpace(doc)) == 0 || isComment(doc) {
			continue
		}
		obj, _, err := decoder.Decode(doc, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("objects.yaml document %d: %w", i, err)
		}
		co, ok := obj.(client.Object)
		if !ok {
			return nil, fmt.Errorf("objects.yaml document %d is a %T", i, obj)
		}
		s.objects = append(s.objects, co)
	}
	return s, nil
}

// isComment reports whether a YAML document holds only comments.
func isComment(doc []byte) bool {
	for line := range strings.SplitSeq(string(doc), "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			return false
		}
	}
	return true
}

// checkRubric checks that the rubric names every fact of expected.yaml once, verbatim, and nothing else, so
// a change of the expected facts in the fleet repository cannot pass unscored.
func (s *scenario) checkRubric() error {
	want := make([]string, 0, len(s.expected.Diagnosis.Evidence))
	got := make([]string, 0, len(s.rubric.Facts))
	for _, e := range s.expected.Diagnosis.Evidence {
		want = append(want, e.Fact)
	}
	for _, f := range s.rubric.Facts {
		if slices.Contains(got, f.Fact) {
			return fmt.Errorf("rubric.yaml: fact %q is listed twice", f.Fact)
		}
		got = append(got, f.Fact)
		scored := len(f.All) > 0 || len(f.Any) > 0
		if scored == (f.Unscored != "") {
			return fmt.Errorf("rubric.yaml: fact %q needs terms (all or any) or a reason it is unscored, not both", f.Fact)
		}
		if slices.ContainsFunc(slices.Concat(f.All, f.Any), func(t string) bool { return strings.TrimSpace(t) == "" }) {
			return fmt.Errorf("rubric.yaml: fact %q has an empty term", f.Fact)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		return fmt.Errorf("rubric.yaml lists the facts %q, expected.yaml has %q", got, want)
	}
	return nil
}

// checkIncident checks that the recorded Incident is the one expected.yaml describes.
func (s *scenario) checkIncident() error {
	e, sub := s.expected.Incident, s.incident.Spec.Subject
	if s.incident.Spec.Reason != e.Reason || sub.Kind != e.Object.Kind || sub.Name != e.Object.Name ||
		sub.Namespace != e.Object.Namespace || (e.Object.APIVersion != "" && sub.APIVersion != e.Object.APIVersion) {
		return fmt.Errorf("incident.yaml (reason %s, subject %s %s/%s) is not the Incident of expected.yaml "+
			"(%s, %s %s/%s)", s.incident.Spec.Reason, sub.Kind, sub.Namespace, sub.Name,
			e.Reason, e.Object.Kind, e.Object.Namespace, e.Object.Name)
	}
	return nil
}

// readStrict reads a YAML file of the harness's own, refusing unknown fields.
func readStrict(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.UnmarshalStrict(raw, into); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}
