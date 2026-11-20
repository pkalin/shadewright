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
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/analyzer/refcheck"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/logs/kube"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// The differential fuzz targets of the read tools (#845, fifteenth decision of 2026-11-18), one per tool of the
// registry: each puts the fields its input decodes to (redacttest.Decode) into the places of a source's answer,
// calls the tool through Set.Call and computes the text the tools of origin/main gave for the same answer
// (diff_main_test.go, with the redactor of that release). Checked, by name:
//
//	P1: every probe secret the redactor of the release before hid at any of its passes is hidden in the answer.
//	P4: the answer is valid UTF-8 whatever the source gave, within the tool's bound, valid JSON for get_resource
//	    whenever origin/main's answer was, and nothing panics. (Valid UTF-8 is a property of the tool results and
//	    the incident context; redact.String and the other plain string functions keep the invalid bytes they do not
//	    hide, as the release's String does.) TestGetResourceAlwaysFits checks more for get_resource: valid JSON
//	    within its bound at every size.
//
// P2 and P3 are properties of package redact and checked there.

// diffScene is the answer of every source for one fuzz input.
type diffScene struct {
	pod        *corev1.Pod
	deployment *appsv1.Deployment
	events     []eventsv1.Event
	series     []providers.Series
	excerpt    providers.LogExcerpt
	delivery   providers.DeliveryStatus
}

// sceneSources answer reads and source calls from the current scene.
type sceneSources struct {
	client.Reader
	mu    sync.Mutex
	scene diffScene
}

func notFound(name string) error { return apierrors.NewNotFound(k8sschema.GroupResource{}, name) }

func (s *sceneSources) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	switch o := obj.(type) {
	case *corev1.Pod:
		if s.scene.pod == nil || key.Name != s.scene.pod.Name || key.Namespace != s.scene.pod.Namespace {
			return notFound(key.Name)
		}
		s.scene.pod.DeepCopyInto(o)
	case *appsv1.Deployment:
		if s.scene.deployment == nil || key.Name != s.scene.deployment.Name {
			return notFound(key.Name)
		}
		s.scene.deployment.DeepCopyInto(o)
	default:
		return notFound(key.Name)
	}
	return nil
}

func (s *sceneSources) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l, ok := list.(*eventsv1.EventList)
	if !ok {
		return fmt.Errorf("unexpected list %T", list)
	}
	l.Items = make([]eventsv1.Event, len(s.scene.events))
	for i := range s.scene.events {
		s.scene.events[i].DeepCopyInto(&l.Items[i])
	}
	return nil
}

func (s *sceneSources) Logs(context.Context, providers.LogQuery) (providers.LogExcerpt, error) {
	return s.scene.excerpt, nil
}

func (s *sceneSources) Query(context.Context, providers.MetricsQuery) (providers.MetricsResult, error) {
	return providers.MetricsResult{Series: s.scene.series}, nil
}

func (s *sceneSources) Status(context.Context, providers.DeliveryUnit) (providers.DeliveryStatus, error) {
	return s.scene.delivery, nil
}

// diffSet is a tool set over scene sources, with the built-in redactor.
func diffSet(t testing.TB) (*Set, *sceneSources) {
	t.Helper()
	src := &sceneSources{}
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	set, err := New(Config{
		Pods: src, Events: src, Objects: src, Logs: src, Metrics: src, Delivery: src, Redactor: red,
		Namespaces: []string{testNS}, DeliveryNamespaces: []string{fluxNS}, Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return set, src
}

// run calls the tool name with args on scene and returns its result.
func (s *sceneSources) run(set *Set, scene diffScene, name string, args map[string]any) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scene = scene
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	r, err := set.Call(context.Background(), providers.ToolCall{ID: "c1", Name: name, Arguments: raw})
	if err != nil {
		panic(err)
	}
	return r
}

// answerFails returns the P1 and P4 failures of a tool's answer against the text origin/main gave (mainText,
// with what it hid in rec).
func answerFails(name string, r Result, rec *redacttest.Recorder, mainText string) []string {
	fails := redacttest.LeakFails(rec, name, r.Text)
	if !utf8.ValidString(r.Text) {
		fails = append(fails, fmt.Sprintf("P4: %s answered invalid UTF-8: %q", name, redacttest.Clip(r.Text)))
	}
	if t, ok := lookup(name); ok && len(r.Text) > t.maxBytes {
		fails = append(fails, fmt.Sprintf("P4: %s answered %d bytes, over its bound of %d", name, len(r.Text), t.maxBytes))
	}
	if name == NameGetResource && r.Status == StatusOK && json.Valid([]byte(mainText)) && !json.Valid([]byte(r.Text)) {
		fails = append(fails, fmt.Sprintf("P4: get_resource answered %d bytes that are not valid JSON, origin/main's %d were: ...%q",
			len(r.Text), len(mainText), r.Text[max(len(r.Text)-120, 0):]))
	}
	if r.Status == StatusOK && mainText == "" {
		fails = append(fails, "P4: origin/main gave no answer where the tool answered")
	}
	return fails
}

// nonEmpty returns the strings of fs that are not empty.
func nonEmpty(fs ...string) []string {
	var out []string
	for _, f := range fs {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// labelsOf returns the labels of the pairs of fields (name, value), leaving out pairs with an empty name.
func labelsOf(fs ...string) map[string]string {
	var out map[string]string
	for i := 0; i+1 < len(fs); i += 2 {
		if fs[i] == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[fs[i]] = fs[i+1]
	}
	return out
}

// diffToolSeeds are grammar inputs every tool target starts from, besides its corpus in testdata/fuzz.
var diffToolSeeds = [][]byte{
	{1, 0, 14, 0, 1},
	{1, 1, 19, 0, 6, 3},
	{1, 2, 18, 2, 11, 14, 1, 0},
	{1, 3, 15, 0, 0, 0, 3, 11, 0, 9},
	{1, 18, 20, 0, 0, 11, 23, 2, 1, 7, 2},
	redacttest.RawSeed(0, "password: \x01"),
	redacttest.RawSeed(2, "192.168.1.1--password\n\x01\n"),
	redacttest.RawSeed(3, "--password", "\x01", "token", "\x01"),
	{0xf1, 0, 242, 0, 7, 0}, // a run of one invalid byte and an "x", 3,150 times (the first argument with knob 0)
}

// toolBodies are the checks of the fuzz targets of this file over one tool set, by target: the failures of one
// input (P3 is never checked here). TestMinimizeCollected runs them too.
func toolBodies(set *Set, src *sceneSources) map[string]func([]byte) ([]string, bool) {
	return map[string]func([]byte) ([]string, bool){
		"FuzzDiffGetResource":           diffGetResource(set, src),
		"FuzzDiffGetPodLogs":            diffGetPodLogs(set, src),
		"FuzzDiffListEvents":            diffListEvents(set, src),
		"FuzzDiffQueryMetrics":          diffQueryMetrics(set, src),
		"FuzzDiffDeliveryStatus":        diffDeliveryStatus(set, src),
		"FuzzDiffCheckMissingReference": diffCheckMissingReference(set, src),
		"FuzzDiffGitTools":              diffGitTools(set, src),
	}
}

// fuzzTool runs the target of that name over its seeds and the fuzzer's inputs.
func fuzzTool(f *testing.F, target string) {
	for _, s := range diffToolSeeds {
		f.Add(s)
	}
	body := toolBodies(diffSet(f))[target]
	f.Fuzz(func(t *testing.T, data []byte) {
		fails, counted := body(data)
		redacttest.Check(t, knownToolFailures[target], data, counted, fails)
	})
}

// TestMinimizeCollected makes the inputs a collecting run wrote small and prints them by kind
// (redacttest.MinimizeCollected).
func TestMinimizeCollected(t *testing.T) {
	redacttest.MinimizeCollected(t, toolBodies(diffSet(t)), knownToolFailures)
}

// knownToolFailures are the known failing inputs of the targets of this package, by target and input bytes.
var knownToolFailures = map[string]map[string]redacttest.KnownFailure{}

// foundPodLogsAfterAWideColon was found by FuzzDiffGetPodLogs in its first five minute run (#845, fifteenth
// decision), minimised: a log excerpt (knob 0xd0: kube.Excerpt on both sides, end of the last line missing) where
// a probe after a key, format characters and a fullwidth colon was hidden by the release before, at a pass it
// made on its own excerpt after the log source, and shown while that pass read the excerpt this package's rules
// had already replaced.
const foundPodLogsAfterAWideColon = "\xc5\xd0s\xc51\xa75\x86\xbfPB\xb7ib\xaad\x0f8\\\xc3;\x8d\xacj\x0e01"

// seedGetResourceAtTheLimit is input 3 of the fifteenth decision: a pod with the labels 10.0.0.1 and 10.0.0.2
// (decided alike) and one argument that brings the answer to its bound.
var seedGetResourceAtTheLimit = redacttest.RawSeed(3, strings.Repeat("x ", 8048)+"x", "", "10.0.0.1", "a", "10.0.0.2", "b")

// seedGetResourceOverTheLimit is a pod with the labels 0.0.0.1 and 0.0.0.2 (decided alike) and one argument
// long enough that the answer is over its bound with the shortest markers and the second key renamed: origin/main's
// answer, with one marker shorter and no renamed key, was valid JSON at the bound, and this package's lost its
// closing brace to the cut.
var seedGetResourceOverTheLimit = redacttest.RawSeed(3, strings.Repeat("x ", 8050), "", "0.0.0.1", "v", "0.0.0.2", "v")

// seedGetResourceInvalidRuns is a pod with one argument of 3,000 runs of one invalid byte, each before an "x": each
// run was written as the six bytes of the escape of U+FFFD where origin/main wrote "?", so an answer of 6 KiB on
// origin/main grew past its bound and was cut.
var seedGetResourceInvalidRuns = redacttest.RawSeed(0, strings.Repeat("\xffx", 3000))

// getResourceScene puts the fields into a pod (or, with knob bit 0x10, a Deployment) by the knob's low bits:
// 0 the container's arguments, 1 labels, 2 the waiting, terminated and pod status reasons and messages, 3 one or
// two arguments and two labels.
func getResourceScene(in redacttest.Input) (diffScene, client.Object, string) {
	f := in.Fields
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNS},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container}}},
	}
	c := &p.Spec.Containers[0]
	switch in.Knob % 4 {
	case 0:
		c.Args = nonEmpty(f...)
	case 1:
		p.Labels = labelsOf(f...)
	case 2:
		p.Status = corev1.PodStatus{
			Reason: f[4], Message: f[5],
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:                 container,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: f[0], Message: f[1]}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: f[2], Message: f[3], ExitCode: 1}},
			}},
		}
	default:
		c.Args = nonEmpty(f[0], f[1])
		p.Labels = labelsOf(f[2:]...)
	}
	if in.Knob&0x10 == 0 {
		return diffScene{pod: p}, p, kindPod
	}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: webName, Namespace: testNS, Labels: p.Labels},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: p.Labels}, Spec: p.Spec,
		}},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: f[0], Message: f[1],
		}}},
	}
	return diffScene{deployment: d}, d, kindDeployment
}

// FuzzDiffGetResource checks get_resource on a pod or a Deployment that carries the fields (P1, P4).
func FuzzDiffGetResource(f *testing.F) { fuzzTool(f, "FuzzDiffGetResource") }

func diffGetResource(set *Set, src *sceneSources) func([]byte) ([]string, bool) {
	return func(data []byte) ([]string, bool) {
		in := redacttest.Decode(data, 6)
		scene, obj, kind := getResourceScene(in)
		r := src.run(set, scene, NameGetResource, map[string]any{argKind: kind, argNamespace: testNS, argName: obj.GetName()})
		m := mainTools{rec: redacttest.NewRecorder()}
		mainText, err := m.getResource(obj.DeepCopyObject().(client.Object))
		if err != nil {
			return nil, false
		}
		return answerFails(NameGetResource, r, m.rec, mainText), false
	}
}

// FuzzDiffGetPodLogs checks get_pod_logs on a log of the fields joined by line breaks (P1, P4). The knob: bits
// 0 to 3 give max_bytes (1 KiB to 16 KiB), bit 0x10 passes the log through the log source's Excerpt first (each
// side its own: kube.Excerpt with the built-in redactor, origin/main's with the redactor of that release), bit
// 0x20 asks for the previous instance, bit 0x40 marks the end missing.
func FuzzDiffGetPodLogs(f *testing.F) { fuzzTool(f, "FuzzDiffGetPodLogs") }

func diffGetPodLogs(set *Set, src *sceneSources) func([]byte) ([]string, bool) {
	red, err := redact.New()
	if err != nil {
		panic(err)
	}
	return func(data []byte) ([]string, bool) {
		in := redacttest.Decode(data, 3)
		text := in.Joined("\n")
		maxBytes := min(1024*(int(in.Knob&0x0f)+1), MaxLogBytes)
		previous, endMissing := in.Knob&0x20 != 0, in.Knob&0x40 != 0
		m := mainTools{rec: redacttest.NewRecorder()}
		ours := providers.LogExcerpt{Text: text, EndMissing: endMissing}
		theirs := ours
		if in.Knob&0x10 != 0 {
			ours.Text, ours.Truncated = kube.Excerpt(red, []byte(text), endMissing, maxBytes)
			theirs.Text, theirs.Truncated = m.excerpt([]byte(text), endMissing, maxBytes)
		}
		r := src.run(set, diffScene{excerpt: ours}, NameGetPodLogs, map[string]any{
			argNamespace: testNS, argPod: podName, argContainer: container, argPrevious: previous, argMaxBytes: maxBytes,
		})
		mainText := m.podLogs(podName, container, previous, theirs, maxBytes)
		return answerFails(NameGetPodLogs, r, m.rec, mainText), false
	}
}

// eventsScene puts the fields into events by the knob: 0 a reason and a note per event, 1 one event's type,
// reason, object name and note, 2 a note per event.
func eventsScene(in redacttest.Input) []eventsv1.Event {
	f := in.Fields
	ev := func(i int, typ, reason, name, note string) eventsv1.Event {
		return eventsv1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("e%d", i), Namespace: testNS},
			EventTime:  metav1.NewMicroTime(testNow.Add(-time.Duration(i) * time.Minute)),
			Type:       typ, Reason: reason, Note: note,
			Regarding: corev1.ObjectReference{Kind: "Pod", Name: name, Namespace: testNS},
		}
	}
	var evs []eventsv1.Event
	switch in.Knob % 3 {
	case 0:
		for i := 0; i+1 < len(f); i += 2 {
			evs = append(evs, ev(i, "Warning", f[i], podName, f[i+1]))
		}
	case 1:
		evs = append(evs, ev(0, f[0], f[1], f[2], f[3]))
	default:
		for i, note := range f {
			evs = append(evs, ev(i, "Warning", "BackOff", podName, note))
		}
	}
	return evs
}

// FuzzDiffListEvents checks list_events on events that carry the fields (P1, P4).
func FuzzDiffListEvents(f *testing.F) { fuzzTool(f, "FuzzDiffListEvents") }

func diffListEvents(set *Set, src *sceneSources) func([]byte) ([]string, bool) {
	return func(data []byte) ([]string, bool) {
		in := redacttest.Decode(data, 6)
		evs := eventsScene(in)
		r := src.run(set, diffScene{events: evs}, NameListEvents, map[string]any{argNamespace: testNS})
		m := mainTools{rec: redacttest.NewRecorder()}
		mainText := m.listEvents(evs, defaultEvents)
		return answerFails(NameListEvents, r, m.rec, mainText), false
	}
}

// FuzzDiffQueryMetrics checks query_metrics on series whose labels are the fields, pairs of a name and a value
// (P1, P4). The knob: 0 one series with three labels, 1 three series with one label each besides the pod.
func FuzzDiffQueryMetrics(f *testing.F) { fuzzTool(f, "FuzzDiffQueryMetrics") }

func diffQueryMetrics(set *Set, src *sceneSources) func([]byte) ([]string, bool) {
	tmpl := metricTemplates[2]
	return func(data []byte) ([]string, bool) {
		in := redacttest.Decode(data, 6)
		sample := []providers.Sample{{Time: testNow, Value: 1}}
		var series []providers.Series
		if in.Knob%2 == 0 {
			series = []providers.Series{{Labels: labelsOf(in.Fields...), Samples: sample}}
		} else {
			for i := 0; i+1 < len(in.Fields); i += 2 {
				l := labelsOf(in.Fields[i], in.Fields[i+1])
				if l == nil {
					l = map[string]string{}
				}
				l["pod"] = podName
				series = append(series, providers.Series{Labels: l, Samples: sample})
			}
		}
		scene := diffScene{pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNS}}, series: series}
		r := src.run(set, scene, NameQueryMetrics, map[string]any{argTemplate: tmpl.name, argNamespace: testNS, argPod: podName})
		m := mainTools{rec: redacttest.NewRecorder()}
		mainText := m.queryMetrics(tmpl, series)
		return answerFails(NameQueryMetrics, r, m.rec, mainText), false
	}
}

// FuzzDiffDeliveryStatus checks get_delivery_status on a status whose applied revision and message are the first
// two fields (P1, P4).
func FuzzDiffDeliveryStatus(f *testing.F) { fuzzTool(f, "FuzzDiffDeliveryStatus") }

func diffDeliveryStatus(set *Set, src *sceneSources) func([]byte) ([]string, bool) {
	return func(data []byte) ([]string, bool) {
		in := redacttest.Decode(data, 2)
		st := providers.DeliveryStatus{AppliedRevision: in.Fields[0], Message: in.Fields[1], Ready: in.Knob%2 == 0}
		r := src.run(set, diffScene{delivery: st}, NameGetDeliveryStatus, map[string]any{
			argKind: kustomization, argNamespace: fluxNS, argName: unitName,
		})
		m := mainTools{rec: redacttest.NewRecorder()}
		mainText := m.deliveryStatus(st)
		return answerFails(NameGetDeliveryStatus, r, m.rec, mainText), false
	}
}

// FuzzDiffCheckMissingReference checks check_missing_reference on a pod whose container takes three keys of a
// Secret (the first three fields) and waits because the kubelet could not find the key the knob picks, with the
// other fields as the notes of events about the pod (P1, P4).
func FuzzDiffCheckMissingReference(f *testing.F) { fuzzTool(f, "FuzzDiffCheckMissingReference") }

func diffCheckMissingReference(set *Set, src *sceneSources) func([]byte) ([]string, bool) {
	return func(data []byte) ([]string, bool) {
		in := redacttest.Decode(data, 6)
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNS},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container}}},
		}
		for i, k := range in.Fields[:3] {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{
				Name: fmt.Sprintf("E%d", i), ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: k,
				}},
			})
		}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: container, State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  createConfigError,
				Message: fmt.Sprintf("couldn't find key %v in Secret %v/%v", in.Fields[int(in.Knob)%3], testNS, "db"),
			},
		}}}
		evs := make([]eventsv1.Event, 0, len(in.Fields)-3)
		for i, note := range in.Fields[3:] {
			evs = append(evs, eventsv1.Event{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("e%d", i), Namespace: testNS},
				EventTime:  metav1.NewMicroTime(testNow.Add(-time.Minute)), Type: "Warning", Reason: "FailedMount", Note: note,
				Regarding: corev1.ObjectReference{Kind: "Pod", Name: podName, Namespace: testNS, UID: p.UID},
			})
		}
		r := src.run(set, diffScene{pod: p, events: evs}, NameCheckMissingReference, map[string]any{
			argNamespace: testNS, argPod: podName, argKind: secretKind, argName: "db",
		})
		m := mainTools{rec: redacttest.NewRecorder()}
		mainText := m.checkReference(refcheck.Check(p.DeepCopy(), evs, refcheck.KindSecret, "db", testNow))
		return answerFails(NameCheckMissingReference, r, m.rec, mainText), false
	}
}

// createConfigError is the waiting reason of a container whose configuration names something missing.
const createConfigError = "CreateContainerConfigError"

// FuzzDiffGitTools checks read_git_file and get_recent_changes, which show no text from a source (no Git reader
// is wired): with the fields as the path and the ref, each answers that it is not available or refuses, and the
// answer never repeats a probe (P1 in its strongest form: nothing of the input), and P4.
func FuzzDiffGitTools(f *testing.F) { fuzzTool(f, "FuzzDiffGitTools") }

func diffGitTools(set *Set, src *sceneSources) func([]byte) ([]string, bool) {
	return func(data []byte) ([]string, bool) {
		in := redacttest.Decode(data, 2)
		var fails []string
		for _, name := range []string{NameReadGitFile, NameGetRecentChanges} {
			r := src.run(set, diffScene{}, name, map[string]any{argPath: in.Fields[0], argRef: in.Fields[1]})
			if r.Status != StatusNotAvailable && r.Status != StatusRefused {
				fails = append(fails, fmt.Sprintf("P4: %s answered %s: it shows text from a source now, give it a target of its own", name, r.Status))
			}
			if p := redacttest.ProbesIn(r.Text); len(p) > 0 {
				fails = append(fails, fmt.Sprintf("P1: %s repeats %v: %q", name, p, r.Text))
			}
			fails = append(fails, answerFails(name, r, redacttest.NewRecorder(), "x")...)
		}
		return fails, false
	}
}

// Every tool of the registry has a differential fuzz target above, so that a new tool fails here until it has one.
func TestEveryToolHasADiffFuzzTarget(t *testing.T) {
	covered := map[string]bool{
		NameGetResource: true, NameGetPodLogs: true, NameListEvents: true, NameQueryMetrics: true,
		NameGetDeliveryStatus: true, NameCheckMissingReference: true, NameReadGitFile: true, NameGetRecentChanges: true,
	}
	for _, name := range Names() {
		if !covered[name] {
			t.Errorf("tool %s has no differential fuzz target in diff_fuzz_test.go", name)
		}
	}
}

// Input 3 of the fifteenth decision of #845, as a directed test: two labels that are decided alike (each an
// address) and one argument that brings the answer to its bound made get_resource answer JSON cut before its
// closing brace, while the markers were fitted before the second label was renamed " (2)".
func TestGetResourceLabelsDecidedAlikeAtTheLimit(t *testing.T) {
	set, src := diffSet(t)
	in := redacttest.Decode(seedGetResourceAtTheLimit, 6)
	scene, obj, kind := getResourceScene(in)
	r := src.run(set, scene, NameGetResource, map[string]any{argKind: kind, argNamespace: testNS, argName: obj.GetName()})
	m := mainTools{rec: redacttest.NewRecorder()}
	mainText, err := m.getResource(obj.DeepCopyObject().(client.Object))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(mainText)) {
		t.Fatalf("origin/main's answer of %d bytes is not valid JSON either", len(mainText))
	}
	if fails := answerFails(NameGetResource, r, m.rec, mainText); len(fails) > 0 {
		t.Error(strings.Join(fails, "\n"))
	}
}

// get_resource answers valid JSON within its bound whatever the replacements and the renamed keys add: values are
// left out, largest needed first, and each says how many bytes it had (#845, fifteenth decision, P4).
func TestGetResourceIsValidJSONOverTheLimit(t *testing.T) {
	for name, seed := range map[string][]byte{
		"two map keys decided alike": seedGetResourceOverTheLimit,
		"many runs of invalid bytes": seedGetResourceInvalidRuns,
	} {
		t.Run(name, func(t *testing.T) {
			set, src := diffSet(t)
			in := redacttest.Decode(seed, 6)
			scene, obj, kind := getResourceScene(in)
			r := src.run(set, scene, NameGetResource, map[string]any{argKind: kind, argNamespace: testNS, argName: obj.GetName()})
			m := mainTools{rec: redacttest.NewRecorder()}
			mainText, err := m.getResource(obj.DeepCopyObject().(client.Object))
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid([]byte(mainText)) {
				t.Fatalf("origin/main's answer of %d bytes is not valid JSON either", len(mainText))
			}
			if fails := answerFails(NameGetResource, r, m.rec, mainText); len(fails) > 0 {
				t.Error(strings.Join(fails, "\n"))
			}
			if r.Status != StatusOK || len(r.Text) > resourceBytes || !json.Valid([]byte(r.Text)) {
				t.Errorf("get_resource answered %s, %d bytes, valid JSON %v", r.Status, len(r.Text), json.Valid([]byte(r.Text)))
			}
		})
	}
}

// The kind FuzzDiffGetPodLogs found beyond the seeds, as a directed test.
func TestPodLogsValueAfterAWideColon(t *testing.T) {
	if fails, _ := toolBodies(diffSet(t))["FuzzDiffGetPodLogs"]([]byte(foundPodLogsAfterAWideColon)); len(fails) > 0 {
		t.Error(strings.Join(fails, "\n"))
	}
}
