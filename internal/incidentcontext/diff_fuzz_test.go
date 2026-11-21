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
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/logs/kube"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
	"github.com/pkalin/shadewright/internal/redact/released"
)

// The differential fuzz target of the context builder (#845, fifteenth decision of 2026-11-18): Build with every
// section (summary, object status, log excerpt, metrics, git, policy bounds) on a Deployment whose objects,
// incident, logs and metrics carry the fields the input decodes to (redacttest.Decode), against Build of
// origin/main on the same answers. The builder's code outside excerpt.go, builder.go lines 281 to 285 and logs.go
// line 140 is unchanged since origin/main 87e96974, and origin/main reached its redactor only through
// Redact (redacted, made valid UTF-8, redacted again), Value and RenderLog, each with the redactor's String. So
// origin/main's Build is Build now with a redactor that has only String (no StringTwice, so Twice and RenderLog
// take the paths of origin/main) whose String is that Redact with the redactor of the release before (package
// released): Redact now is that String made valid UTF-8, which it already is. Checked, by name:
//
//	P1: every probe secret the redactor of the release before hid at any of its passes is hidden in the context.
//	P4: every block is valid UTF-8, the context is within its total bound, and nothing panics.

// labelContainer is the label of a series that names its container.
const (
	labelContainer = "container"
	labelPod       = "pod"
)

// mainBuildRedactor is the redactor of origin/main's Build: String is Redact of origin/main excerpt.go with the
// redactor of the release before, each pass recorded.
type mainBuildRedactor struct{ rec *redacttest.Recorder }

func (m mainBuildRedactor) String(s string) string {
	once := m.rec.Wrap(released.String)
	return once(strings.ToValidUTF8(once(s), "?"))
}

// StringChecked never withholds: main had no work budget.
func (m mainBuildRedactor) StringChecked(s string) (string, bool) { return m.String(s), true }

// buildScene is what the sources answer for one input.
type buildScene struct {
	objects []client.Object
	logText string
	series  []providers.Series
	// excerpt, when set, makes the log source cut and redact the log as the log source does (kube.Excerpt), with
	// the redactor of its side.
	excerpt func(data []byte, keep int) (string, bool, error)
}

// sceneReader answers gets and pod lists from the scene's objects.
type sceneReader struct {
	client.Reader
	scene *buildScene
}

func (r sceneReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	for _, o := range r.scene.objects {
		if fmt.Sprintf("%T", o) == fmt.Sprintf("%T", obj) && o.GetName() == key.Name && o.GetNamespace() == key.Namespace {
			switch d := obj.(type) {
			case *appsv1.Deployment:
				o.(*appsv1.Deployment).DeepCopyInto(d)
			case *appsv1.ReplicaSet:
				o.(*appsv1.ReplicaSet).DeepCopyInto(d)
			case *corev1.Pod:
				o.(*corev1.Pod).DeepCopyInto(d)
			default:
				continue
			}
			return nil
		}
	}
	return apierrors.NewNotFound(k8sschema.GroupResource{}, key.Name)
}

func (r sceneReader) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	l, ok := list.(*corev1.PodList)
	if !ok {
		return fmt.Errorf("unexpected list %T", list)
	}
	var o client.ListOptions
	o.ApplyOptions(opts)
	for _, obj := range r.scene.objects {
		p, ok := obj.(*corev1.Pod)
		if !ok || (o.Namespace != "" && p.Namespace != o.Namespace) {
			continue
		}
		if o.LabelSelector != nil && !o.LabelSelector.Matches(labels.Set(p.Labels)) {
			continue
		}
		l.Items = append(l.Items, *p.DeepCopy())
	}
	return nil
}

// sceneLogs and sceneMetrics answer the log and metrics queries from the scene.
type sceneLogs struct{ scene *buildScene }

func (s sceneLogs) Logs(_ context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
	ex := providers.LogExcerpt{Text: s.scene.logText, Instance: providers.LogInstance{PodUID: "uid-" + crashPod, RestartCount: 4}}
	if s.scene.excerpt != nil {
		var err error
		if ex.Text, ex.Truncated, err = s.scene.excerpt([]byte(s.scene.logText), int(q.MaxBytes)); err != nil {
			return providers.LogExcerpt{}, err
		}
	}
	return ex, nil
}

type sceneMetrics struct{ scene *buildScene }

func (s sceneMetrics) Query(context.Context, providers.MetricsQuery) (providers.MetricsResult, error) {
	return providers.MetricsResult{Series: s.scene.series}, nil
}

// buildFields returns the objects, incident, log and series of an input. The knob's low bits put the fields into:
// 0 the crashing container's waiting and terminated reasons and messages, the pod's status reason and message and
// the Deployment's condition; 1 the incident's symptom, reason and reporting controller, a series' labels and the
// log; 2 the log, one field per line; 3 one series' labels, pairs of a name and a value.
func buildFields(in redacttest.Input) ([]client.Object, *remediationv1alpha1.Incident, string, []providers.Series) {
	f := in.Fields
	objs := webObjects()
	d, p := objs[0].(*appsv1.Deployment), objs[2].(*corev1.Pod)
	inc := incident(deploymentRef())
	logText := "starting\npanic: boom\n"
	sample := []providers.Sample{{Time: testNow, Value: 1}}
	series := []providers.Series{{Labels: map[string]string{labelPod: crashPod, labelContainer: appName}, Samples: sample}}
	labelsOf := func(fs ...string) map[string]string {
		l := map[string]string{labelPod: crashPod}
		for i := 0; i+1 < len(fs); i += 2 {
			if fs[i] != "" {
				l[fs[i]] = fs[i+1]
			}
		}
		return l
	}
	switch in.Knob % 4 {
	case 0:
		cs := &p.Status.ContainerStatuses[0]
		cs.State.Waiting.Reason, cs.State.Waiting.Message = f[0], f[1]
		cs.LastTerminationState.Terminated.Reason, cs.LastTerminationState.Terminated.Message = f[2], f[3]
		p.Status.Reason, p.Status.Message = f[4], f[5]
		d.Status.Conditions[0].Reason, d.Status.Conditions[0].Message = f[6], f[7]
	case 1:
		inc.Spec.Symptom, inc.Spec.Reason, inc.Spec.ReportingController = f[0], f[1], f[2]
		series = []providers.Series{{Labels: labelsOf(f[3:7]...), Samples: sample}}
		logText = f[7]
	case 2:
		logText = in.Joined("\n")
	default:
		series = []providers.Series{{Labels: labelsOf(f...), Samples: sample}}
	}
	return objs, inc, logText, series
}

// diffBuild builds the context of inc over scene with red.
func diffBuild(t testing.TB, scene *buildScene, red Redactor, inc *remediationv1alpha1.Incident) *Context {
	t.Helper()
	r := sceneReader{scene: scene}
	b, err := New(Config{
		Pods: r, Objects: r, Namespaces: []string{testNS}, Redactor: red, Now: func() time.Time { return testNow },
		Logs: sceneLogs{scene}, Metrics: sceneMetrics{scene},
		Policies:     map[remediationv1alpha1.ActionType]remediationv1alpha1.Name{remediationv1alpha1.ActionSetContainerMemoryLimit: "web-memory"},
		PolicyReader: r, RequestNamespace: "shadewright-system",
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Build(context.Background(), inc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// contextText is the text of every block of c.
func contextText(c *Context) string {
	var b strings.Builder
	for _, blk := range c.Blocks {
		b.WriteString(blk.Text)
	}
	return b.String()
}

// buildDiff builds the context of an input on both sides and returns the failures. The knob's bit 0x10 passes the
// log through the log source's Excerpt first.
func buildDiff(t testing.TB, in redacttest.Input) []string {
	t.Helper()
	objs, inc, logText, series := buildFields(in)
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	rec := redacttest.NewRecorder()
	ours := &buildScene{objects: objs, logText: logText, series: series}
	theirs := &buildScene{objects: objs, logText: logText, series: series}
	if in.Knob&0x10 != 0 {
		ours.excerpt = func(data []byte, keep int) (string, bool, error) { return kube.Excerpt(red, data, false, keep) }
		theirs.excerpt = func(data []byte, keep int) (string, bool, error) {
			text, truncated := mainExcerpt(rec, data, keep)
			return text, truncated, nil
		}
	}
	c := diffBuild(t, ours, red, inc.DeepCopy())
	diffBuild(t, theirs, mainBuildRedactor{rec}, inc.DeepCopy())
	text := contextText(c)
	fails := redacttest.LeakFails(rec, "the context", text)
	for _, blk := range c.Blocks {
		if !utf8.ValidString(blk.Text) {
			fails = append(fails, fmt.Sprintf("P4: block %s is not valid UTF-8: %q", blk.Section, redacttest.Clip(blk.Text)))
		}
	}
	if c.Size() > DefaultTotalBytes {
		fails = append(fails, fmt.Sprintf("P4: the context is %d bytes, over its bound of %d", c.Size(), DefaultTotalBytes))
	}
	return fails
}

// mainExcerpt is Excerpt of origin/main internal/providers/logs/kube/kube.go (87e96974) with the redactor of the
// release before: the whole log redacted with the part before the cut dropped, made valid UTF-8 and redacted
// again, each pass recorded on the part kept.
func mainExcerpt(rec *redacttest.Recorder, data []byte, keep int) (string, bool) {
	from, truncated := 0, false
	if len(data) > keep {
		truncated, from = true, len(data)-keep
		if data[from-1] != '\n' {
			i := bytes.IndexByte(data[from:], '\n')
			if i < 0 {
				from = len(data)
			} else {
				from += i + 1
			}
		}
	}
	pos := 0
	part := func(n int, f func(start int) string) string {
		start := pos
		pos += n
		if pos <= from {
			return ""
		}
		return f(max(from-start, 0))
	}
	text := released.Replace(string(data),
		func(kind, s string) string {
			return part(len(s), func(skip int) string { return released.Marker(kind, len(s)-skip) })
		},
		func(seg string) string { return part(len(seg), func(skip int) string { return seg[skip:] }) })
	rec.Saw(string(data[from:]), text)
	return rec.Wrap(released.String)(strings.ToValidUTF8(text, "?")), truncated
}

// knownBuildFailures are the known failing inputs of FuzzDiffBuild: none since each section is decided once on
// the values as they were read (#845, fifteenth decision).
var knownBuildFailures = map[string]redacttest.KnownFailure{}

// foundBuildExplicitKey was found by FuzzDiffBuild in its first five minute run (#845, fifteenth decision),
// minimised: the incident symptom "{aws_secret_access_key: <probe>}? password" and its next line ": <probe>". The release
// before ended the first value at "}" and read "? password" as the key of the next line; the rule hides
// "? password" as part of the first value, and the release's reading of the summary, run on the text the rule
// had already replaced, no longer saw the key.
const foundBuildExplicitKey = "19X\xd8Y\xb4A"

// FuzzDiffBuild checks Build against origin/main's Build on the same answers (P1, P4).
func FuzzDiffBuild(f *testing.F) {
	for _, s := range [][]byte{
		{1, 0, 14, 0, 1}, {1, 1, 19, 0, 6, 3}, {1, 2, 15, 0, 0, 0, 3}, {1, 3, 19, 1, 6, 1},
		{1, 0x12, 20, 0, 0, 12, 3, 5}, redacttest.RawSeed(2, "192.168.1.1--password", "\x01"),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fails, counted := buildBodies(t)["FuzzDiffBuild"](data)
		redacttest.Check(t, knownBuildFailures, data, counted, fails)
	})
}

// maxBuildField caps each field of an input of FuzzDiffBuild, in bytes: both builds of an input of 160 KiB (the
// seed found-873-22905eb52eae1385, a label of 40 KiB repeated) took 4 s, and four times that with the fuzzer's
// instrumentation, so a worker on the runner went past its 60 s limit (#873). The fields of eight at this cap still
// fill every section; the work on long inputs is bounded by the redactor's budget and checked by the work tests
// (TestBuildWorkOfARepeatedLabelOfShortWindows, package redact TestBudgetBoundsTheFourShapes), not here.
const maxBuildField = 2 << 10

// capFields cuts each field of in to maxBuildField bytes.
func capFields(in redacttest.Input) redacttest.Input {
	for i, f := range in.Fields {
		if len(f) > maxBuildField {
			in.Fields[i] = f[:maxBuildField]
		}
	}
	return in
}

// buildBodies is the check of FuzzDiffBuild, for TestMinimizeCollected too.
func buildBodies(t testing.TB) map[string]func([]byte) ([]string, bool) {
	return map[string]func([]byte) ([]string, bool){
		"FuzzDiffBuild": func(data []byte) ([]string, bool) {
			return buildDiff(t, capFields(redacttest.Decode(data, 8))), false
		},
	}
}

// TestMinimizeCollected makes the inputs a collecting run wrote small and prints them by kind
// (redacttest.MinimizeCollected).
func TestMinimizeCollected(t *testing.T) {
	redacttest.MinimizeCollected(t, buildBodies(t), map[string]map[string]redacttest.KnownFailure{"FuzzDiffBuild": knownBuildFailures})
}

// Input 1 of the fifteenth decision of #845, as a directed test: a crashing container whose waiting and
// terminated reason is "password\uff1aAuthorization" and whose message is a secret. origin/main writes
// "status reason password\uff1aAuthorization: [credential]"; the context showed the message while finish read
// the lines that already held the rule's own output for each value.
func TestBuildReasonBehindAFullwidthColon(t *testing.T) {
	const secret = "SECRETVALUE99"
	objs, inc, logText, series := buildFields(redacttest.Decode(redacttest.RawSeed(0, "password\uff1aAuthorization", secret,
		"password\uff1aAuthorization", secret), 8))
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	rec := redacttest.NewRecorder()
	c := diffBuild(t, &buildScene{objects: objs, logText: logText, series: series}, red, inc.DeepCopy())
	m := diffBuild(t, &buildScene{objects: objs, logText: logText, series: series}, mainBuildRedactor{rec}, inc.DeepCopy())
	if strings.Contains(contextText(m), secret) {
		t.Fatalf("origin/main shows the secret too:\n%s", contextText(m))
	}
	if strings.Contains(contextText(c), secret) {
		t.Error("P1: the context shows the message the release hid: " + redacttest.Clip(contextText(c)))
	}
}

// The kind FuzzDiffBuild found beyond the seed, as a directed test.
func TestBuildValueAfterAnExplicitKey(t *testing.T) {
	if fails, _ := buildBodies(t)["FuzzDiffBuild"]([]byte(foundBuildExplicitKey)); len(fails) > 0 {
		t.Error(strings.Join(fails, "\n"))
	}
}
