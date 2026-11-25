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

package analysis

import (
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/incidentcontext"
	"github.com/shadewright-labs/shadewright/internal/providers"
	fakeproviders "github.com/shadewright-labs/shadewright/internal/providers/fake"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

// #981: maxContextBytes of the settings is the bound the analysis gives the context builder; unset it is
// 48 KiB, and a value outside the CRD's range is held to it.
func TestLimitsOfReadsMaxContextBytes(t *testing.T) {
	for _, c := range []struct {
		name string
		a    *remediationv1alpha1.AnalysisSettings
		want int
	}{
		{"no analysis settings", nil, 48 << 10},
		{"unset", &remediationv1alpha1.AnalysisSettings{}, 48 << 10},
		{"set", &remediationv1alpha1.AnalysisSettings{MaxContextBytes: ptr.To[int32](8192)}, 8192},
		{"below the range", &remediationv1alpha1.AnalysisSettings{MaxContextBytes: ptr.To[int32](0)}, 4096},
		{"at the maximum", &remediationv1alpha1.AnalysisSettings{MaxContextBytes: ptr.To[int32](49152)}, 48 << 10},
		{"just above the maximum", &remediationv1alpha1.AnalysisSettings{MaxContextBytes: ptr.To[int32](49153)}, 48 << 10},
		{"the old maximum", &remediationv1alpha1.AnalysisSettings{MaxContextBytes: ptr.To[int32](262144)}, 48 << 10},
		{"above the range", &remediationv1alpha1.AnalysisSettings{MaxContextBytes: ptr.To[int32](1 << 20)}, 48 << 10},
	} {
		s := &remediationv1alpha1.RemediationSettings{Spec: remediationv1alpha1.RemediationSettingsSpec{Analysis: c.a}}
		if got := LimitsOf(s).MaxContextBytes; got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// #981: the analysis builds the context within maxContextBytes of the RemediationSettings the API server
// stores: the set value, or the CRD's default of 49152 when the field is unset.
func TestControllerBuildsWithinMaxContextBytes(t *testing.T) {
	for _, c := range []struct {
		name string
		set  *int32
		want int
	}{
		{"unset", nil, 49152},
		{"set", ptr.To[int32](8192), 8192},
	} {
		t.Run(c.name, func(t *testing.T) {
			ns := namespace(t)
			setSettings(t, 10)
			s := &remediationv1alpha1.RemediationSettings{}
			if err := env.admin.Get(t.Context(), types.NamespacedName{Name: remediationv1alpha1.SettingsName}, s); err != nil {
				t.Fatal(err)
			}
			if c.set != nil {
				s.Spec.Analysis.MaxContextBytes = c.set
				if err := env.admin.Update(t.Context(), s); err != nil {
					t.Fatal(err)
				}
			}
			inc := dueIncident(t, ns, nil)
			f := newFixture()
			f.triage.steps = []step{reply(noProposal)}
			ctl, _ := newController(t, ns, f)
			reconcile(t, ctl, inc)
			if !slices.Equal(f.builder.totals, []int{c.want}) {
				t.Errorf("the builder was given %v, want [%d]", f.builder.totals, c.want)
			}
		})
	}
}

// #1054: maxContextBytes bounds the rendered context message sent to the model (escaped text plus tags and
// notices), not the unescaped block text alone. A log full of angle brackets expands about four times when
// escaped, so both the minimum (4096) and the default (49152) must cut the context to keep the sent message
// within maxContextBytes and mark it truncated so the analysis escalates.
func TestMaxContextBytesBoundsRenderedMessage(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, remediationv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	inc := incident()
	depUID := types.UID(subjectUID)
	rsUID := types.UID("22222222-2222-4222-8222-222222222222")
	podUID := types.UID("33333333-3333-4333-8333-333333333333")
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	sel := map[string]string{"svc": inc.Spec.Subject.Name}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: inc.Spec.Subject.Name, Namespace: subjectNS, UID: depUID},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: sel},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: testContainer}}},
			},
		},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-rs", Namespace: subjectNS, UID: rsUID, Labels: sel,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: inc.Spec.Subject.APIVersion, Kind: inc.Spec.Subject.Kind,
				Name: inc.Spec.Subject.Name, UID: depUID, Controller: new(true),
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: sel}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-pod", Namespace: subjectNS, UID: podUID, Labels: sel,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: inc.Spec.Subject.APIVersion, Kind: "ReplicaSet",
				Name: "web-rs", UID: rsUID, Controller: new(true),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: testContainer}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: testContainer, RestartCount: 2,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: inc.Spec.Reason, ExitCode: 137, FinishedAt: metav1.NewTime(now.Add(-time.Minute)),
				}},
			}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, rs, pod).Build()
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		limit int
		lines int
	}{
		// ~3 KiB of '<' lines fits the old 4096 raw-text limit, but escapes to ~12 KiB rendered.
		{"minimum bound with angle brackets", incidentcontext.MinTotalBytes, 60},
		// ~20 KiB of '<' lines fits the 24 KiB log section bound, but escapes to ~80 KiB rendered.
		{"default bound with angle brackets", incidentcontext.DefaultTotalBytes, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &fakeproviders.Logs{}
			line := strings.Repeat("<", 50) + "\n"
			logs.Set(fakeproviders.LogKey{Namespace: subjectNS, Pod: "web-pod", Container: testContainer, Previous: true},
				strings.Repeat(line, tc.lines)+"last-line\n")
			b, err := incidentcontext.New(incidentcontext.Config{
				Pods: cl, Objects: cl, Namespaces: []string{subjectNS}, Logs: logs, Redactor: red,
				Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			f := newFixture()
			f.cfg.Builder = b
			f.escalation.steps = []step{reply(noProposal)}
			lim := testLimits()
			lim.MaxContextBytes = tc.limit
			out := f.analyze(t, lim)
			if !out.Escalated || f.triage.calls() != 0 {
				t.Fatalf("outcome escalated=%t triageCalls=%d reason=%s, want direct escalation on truncated context",
					out.Escalated, f.triage.calls(), out.Reason)
			}
			reqs := f.escalation.reqs()
			if len(reqs) != 1 {
				t.Fatalf("got %d escalation requests, want 1", len(reqs))
			}
			var userMsg string
			for _, m := range reqs[0].Messages {
				if m.Role == providers.RoleUser {
					userMsg = m.Content
					break
				}
			}
			if len(userMsg) > tc.limit {
				t.Errorf("sent context message is %d bytes, over maxContextBytes %d", len(userMsg), tc.limit)
			}
			if !strings.Contains(userMsg, string(incidentcontext.LabelContextCut)) {
				t.Errorf("cut context message is missing label %q", incidentcontext.LabelContextCut)
			}
			if !strings.Contains(userMsg, "last-line\n") {
				t.Errorf("log tail lost the newest line")
			}
		})
	}

	for _, tc := range []struct {
		name     string
		hint     string
		notReady bool
	}{
		{name: "oversized reviewer hint", hint: strings.Repeat("<", 2<<10)},
		{name: "readiness notice", notReady: true},
		{name: "hint and readiness notice", hint: strings.Repeat("<", 2<<10), notReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			inc := incident()
			limit := incidentcontext.MinTotalBytes
			if tc.notReady {
				obj := &unstructured.Unstructured{Object: scaledObject(kedaTriggerError())}
				reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).Build()
				b, err := incidentcontext.New(incidentcontext.Config{
					Pods: reader, Objects: reader, Namespaces: []string{subjectNS}, Redactor: red,
					Now: func() time.Time { return now },
				})
				if err != nil {
					t.Fatal(err)
				}
				inc = kedaIncident()
				before, err := b.BuildWithin(t.Context(), inc, limit)
				if err != nil {
					t.Fatal(err)
				}
				inc.Spec.Symptom += strings.Repeat("<", (limit-before.Size())/4)
				before, err = b.BuildWithin(t.Context(), inc, limit)
				if err != nil {
					t.Fatal(err)
				}
				if before.Escalate() || before.Size()+len(scalerNotReadyNotice) <= limit {
					t.Fatalf("expected context to fit before the readiness notice, got %d bytes, escalation %t",
						before.Size(), before.Escalate())
				}
				f.cfg.Builder = b
			}
			f.triage.steps = []step{reply(noProposal)}
			f.escalation.steps = []step{reply(noProposal)}
			a, err := New(f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			lim := testLimits()
			lim.MaxContextBytes = limit
			out, err := a.AnalyzeWithHint(t.Context(), inc, lim, tc.hint)
			if err != nil {
				t.Fatal(err)
			}
			reqs := append(f.triage.reqs(), f.escalation.reqs()...)
			if len(reqs) != 1 || len(reqs[0].Messages) < 2 || reqs[0].Messages[1].Role != providers.RoleUser {
				t.Fatalf("expected one provider request with a first user message, got %d requests", len(reqs))
			}
			msg := reqs[0].Messages[1].Content
			if len(msg) > limit {
				t.Errorf("sent context message is %d bytes, over maxContextBytes %d", len(msg), limit)
			}
			if !out.Escalated || f.triage.calls() != 0 {
				t.Error("expected direct escalation on context truncated for the appended text")
			}
			if tc.notReady && strings.Count(msg, scalerNotReadyNotice) != 1 {
				t.Error("expected one complete readiness notice")
			}
			if tc.hint != "" {
				_, hint, shown := strings.Cut(msg, hintNotice)
				body, closed := strings.CutSuffix(hint, "\n</context-block>\n")
				if !shown || !closed || body == "" || strings.ReplaceAll(body, "&lt;", "") != "" ||
					!strings.Contains(msg, string(incidentcontext.LabelContextCut)) {
					t.Error("expected a marked, truncated reviewer hint with complete escapes and closing tag")
				}
			}
		})
	}
}
