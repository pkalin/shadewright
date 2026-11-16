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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// The grouping cases (ADR-0039, Consequences; #795) need no model to decide: they run a scenario's cluster
// objects through the analyzer's own pipeline on a real API server (envtest), with the collector, the Incident
// controller and its analysis trigger, and the analysis controller with the context builder, the read tools
// and the scenario's recorded replies, as cmd/analyzer wires them. They count what the lab run counts: the
// Incidents of the subject, the analyses (units taken from the daily budget, and model calls) and the
// RemediationRequests.

// groupingFile holds the kubelet events of a scenario's fault that the grouping case adds to objects.yaml.
const groupingFile = "grouping.yaml"

// Times of the pipeline under test. The debounce is short, so an Incident is analyzed seconds after its last
// observation. No case relies on it to order two steps of the pipeline: a case that needs an order waits for
// the first step, or holds back the analyses (startGrouping's gate).
const (
	groupingDebounce = 8 * time.Second
	groupingWait     = 60 * time.Second
	// groupingSettle is how long a case watches after its last expected change, to see nothing more happen:
	// several trigger passes and write windows.
	groupingSettle = 5 * time.Second
)

// logOnce silences controller-runtime's logger, which otherwise prints a stack when it is first used unset.
var logOnce sync.Once

// groupingEnv is one API server with the analyzer's pipeline running against it.
type groupingEnv struct {
	admin client.Client
	model *fake.Model
	// uids maps a fixture's placeholder UIDs to the UIDs the API server gave the created objects.
	uids map[string]string
}

// gatedClient is the analysis controller's client while a case holds back the analyses: every Get waits until
// the gate is closed. A reconcile of the analysis controller starts with a Get of its Incident, so no analysis
// claims an Incident before the case opens the gate, and each one then reads the Incident as it is by then.
type gatedClient struct {
	client.Client
	gate <-chan struct{}
}

func (c gatedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	select {
	case <-c.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// startGrouping starts an API server and the pipeline. model replays the scenario's replies; every analysis
// is one model call, so its requests count the analyses too. A non-nil gate holds back every analysis until
// the case closes it.
func startGrouping(t *testing.T, s *scenario, gate <-chan struct{}) *groupingEnv {
	t.Helper()
	logOnce.Do(func() { ctrllog.SetLogger(logr.Discard()) })
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, remediationv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	te := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: envtestBinaryDir(),
	}
	cfg, err := te.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = te.Stop() })
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Stop the manager before the API server (cleanups run last in, first out).
	var running sync.WaitGroup
	t.Cleanup(func() { cancel(); running.Wait() })
	for _, ns := range []string{nsDemo, evalRequestNamespace} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	// The cluster the analyzer runs in: its settings with a triage model and a daily limit, the budget
	// ConfigMap, and the scenario's policies.
	settings := evalSettings()
	settings.Spec.Analysis = &remediationv1alpha1.AnalysisSettings{
		MaxAnalysesPerDay: ptr.To[int32](10), MaxToolSteps: ptr.To[int32](evalMaxToolSteps),
		MaxOutputTokens: ptr.To[int32](evalMaxOutputTokens),
		Models: &remediationv1alpha1.AnalysisModels{
			Triage: deployment("https://eval.invalid/", "eval-triage", "v1")},
	}
	objs := make([]client.Object, 0, 2+len(s.policies))
	objs = append(objs, settings,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: evalRequestNamespace, Name: incidents.BudgetName}})
	for _, p := range s.policies {
		p = p.DeepCopy()
		p.ResourceVersion = ""
		objs = append(objs, p)
	}
	for _, o := range objs {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatalf("creating %T %s: %v", o, o.GetName(), err)
		}
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  analyzer.CacheOptions(evalRequestNamespace, watchedNamespaces),
		// Each case runs its own manager in this process, with the analyzer's controller names.
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	inc, err := incidents.New(incidents.Config{
		Namespace: evalRequestNamespace, MaxIncidents: 20, Window: 200 * time.Millisecond,
		TriggerInterval: 500 * time.Millisecond, Debounce: groupingDebounce,
	})
	if err != nil {
		t.Fatal(err)
	}
	col, err := collector.New(collector.Config{
		ClusterID: "eval-grouping", Handled: inc.Handled, Namespaces: watchedNamespaces,
		// The collector takes the time of its first reconcile as its start, and an object created before it
		// as one an earlier process may have counted. The lab's analyzer runs before the fault; this clock,
		// a minute behind, puts the fixture's objects after the start, as there. Every time the collector
		// measures on its own clock moves with it.
		Now: func() time.Time { return time.Now().Add(-time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := col.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if err := inc.SetupWithManager(mgr, col.Observations()); err != nil {
		t.Fatal(err)
	}

	model := &fake.Model{}
	for _, r := range s.recording.Replies {
		model.Script(r.completion())
	}
	redactor, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	reader := mgr.GetAPIReader()
	builder, err := incidentcontext.New(incidentcontext.Config{Pods: reader, Objects: reader,
		Namespaces: watchedNamespaces, Redactor: redactor, Policies: evalPolicies, PolicyReader: reader,
		RequestNamespace: evalRequestNamespace})
	if err != nil {
		t.Fatal(err)
	}
	set, err := tools.New(tools.Config{Pods: reader, Events: reader, Objects: reader, Redactor: redactor,
		Namespaces: watchedNamespaces})
	if err != nil {
		t.Fatal(err)
	}
	a, err := analysis.New(analysis.Config{
		Builder: builder, Tools: set, Policies: evalPolicies, Redactor: redactor,
		ExpectedModels: map[analysis.Role]string{analysis.RoleTriage: fakeModelName},
		Models: func(analysis.Role, remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
			return model, nil
		},
		Reserve: func(ctx context.Context, now time.Time) error {
			return incidents.ReserveAnalysis(ctx, reader, admin, evalRequestNamespace, now)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	acClient := mgr.GetClient()
	if gate != nil {
		acClient = gatedClient{Client: acClient, gate: gate}
	}
	ac := &analysis.Controller{Client: acClient, Namespace: evalRequestNamespace, Analyzer: a}
	if err := ac.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	running.Go(func() {
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("the manager stopped: %v", err)
		}
	})
	syncCtx, done := context.WithTimeout(ctx, 30*time.Second)
	defer done()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatal("the cache did not sync")
	}
	return &groupingEnv{admin: admin, model: model, uids: map[string]string{}}
}

// envtestBinaryDir is the envtest binaries make test installs, for a run outside make.
func envtestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

// readFixture decodes the objects of a scenario file, skipping documents that hold only comments.
func readFixture(t *testing.T, path string) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("%s: %v", path, err)
		}
		if len(u.Object) > 0 {
			out = append(out, u)
		}
	}
}

// fixtureLayouts are the time formats a fixture uses: MicroTime (events) and Time.
var fixtureLayouts = []string{"2006-01-02T15:04:05.000000Z07:00", time.RFC3339}

// shiftTimes moves every time of v by d, in the layout it was written in, so that a fixture's times keep
// their order and their distances to each other.
func shiftTimes(v any, d time.Duration) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = shiftTimes(e, d)
		}
	case []any:
		for i, e := range x {
			x[i] = shiftTimes(e, d)
		}
	case string:
		for _, layout := range fixtureLayouts {
			if ts, err := time.Parse(layout, x); err == nil {
				return ts.Add(d).UTC().Format(layout)
			}
		}
	}
	return v
}

// create creates the fixture's objects in order, with their times moved by d, then writes their status. The
// API server gives each object its UID and creation time: owner references and an event's regarding UID are
// mapped from the fixture's placeholder UIDs to the created objects' ones, so objects come after their owners.
func (g *groupingEnv) create(t *testing.T, objs []*unstructured.Unstructured, d time.Duration) {
	t.Helper()
	ctx := t.Context()
	for _, fixture := range objs {
		u := fixture.DeepCopy()
		u.Object = shiftTimes(u.Object, d).(map[string]any)
		placeholder := string(u.GetUID())
		for _, f := range []string{"uid", "creationTimestamp", "resourceVersion", "generation"} {
			unstructured.RemoveNestedField(u.Object, "metadata", f)
		}
		refs := u.GetOwnerReferences()
		for i := range refs {
			refs[i].UID = types.UID(g.mapped(t, string(refs[i].UID)))
		}
		u.SetOwnerReferences(refs)
		if uid, ok, _ := unstructured.NestedString(u.Object, "regarding", "uid"); ok {
			if err := unstructured.SetNestedField(u.Object, g.mapped(t, uid), "regarding", "uid"); err != nil {
				t.Fatal(err)
			}
		}
		if err := completeReplicaSet(u, objs); err != nil {
			t.Fatal(err)
		}
		status, hasStatus, _ := unstructured.NestedMap(u.Object, "status")
		if err := g.admin.Create(ctx, u); err != nil {
			t.Fatalf("creating %s %s: %v", u.GetKind(), u.GetName(), err)
		}
		if placeholder != "" {
			g.uids[placeholder] = string(u.GetUID())
		}
		if hasStatus {
			if err := unstructured.SetNestedMap(u.Object, status, "status"); err != nil {
				t.Fatal(err)
			}
			if err := g.admin.Status().Update(ctx, u); err != nil {
				t.Fatalf("writing the status of %s %s: %v", u.GetKind(), u.GetName(), err)
			}
		}
	}
}

// completeReplicaSet gives a ReplicaSet of the fixture without a pod template, which the API server requires,
// the template of the Deployment that owns it with the ReplicaSet's selector labels (pod-template-hash), as
// the Deployment controller writes it. Neither the collector nor the analysis reads it.
func completeReplicaSet(u *unstructured.Unstructured, objs []*unstructured.Unstructured) error {
	if u.GetKind() != "ReplicaSet" {
		return nil
	}
	if _, ok, _ := unstructured.NestedMap(u.Object, "spec", "template"); ok {
		return nil
	}
	for _, ref := range u.GetOwnerReferences() {
		for _, o := range objs {
			if o.GetKind() != ref.Kind || o.GetName() != ref.Name || o.GetNamespace() != u.GetNamespace() {
				continue
			}
			tmpl, ok, err := unstructured.NestedMap(o.Object, "spec", "template")
			if err != nil || !ok {
				return fmt.Errorf("the owner %s of ReplicaSet %s has no pod template", ref.Name, u.GetName())
			}
			labels, _, err := unstructured.NestedStringMap(u.Object, "spec", "selector", "matchLabels")
			if err != nil {
				return err
			}
			if err := unstructured.SetNestedStringMap(tmpl, labels, "metadata", "labels"); err != nil {
				return err
			}
			return unstructured.SetNestedMap(u.Object, tmpl, "spec", "template")
		}
	}
	return fmt.Errorf("ReplicaSet %s has no pod template and no owner in the fixture", u.GetName())
}

func (g *groupingEnv) mapped(t *testing.T, placeholder string) string {
	t.Helper()
	uid, ok := g.uids[placeholder]
	if !ok {
		t.Fatalf("the fixture names UID %s before the object that has it", placeholder)
	}
	return uid
}

// counts is what a case checks: the subject's Incidents, the analyses and the requests.
type counts struct {
	incidents []remediationv1alpha1.Incident
	requests  []remediationv1alpha1.RemediationRequest
	// analyses is the number of units taken from the daily budget: one per analysis that called the model.
	analyses int
	calls    int
}

func (g *groupingEnv) counts(t *testing.T, subject string) counts {
	t.Helper()
	ctx := t.Context()
	var c counts
	var incs remediationv1alpha1.IncidentList
	if err := g.admin.List(ctx, &incs, client.InNamespace(evalRequestNamespace)); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, inc := range incs.Items {
		if inc.Spec.Subject.Namespace+"/"+inc.Spec.Subject.Name == subject {
			c.incidents = append(c.incidents, inc)
			names[inc.Name] = true
		}
	}
	var rrs remediationv1alpha1.RemediationRequestList
	if err := g.admin.List(ctx, &rrs, client.InNamespace(evalRequestNamespace)); err != nil {
		t.Fatal(err)
	}
	for _, rr := range rrs.Items {
		if names[rr.Spec.IncidentRef.Name] {
			c.requests = append(c.requests, rr)
		}
	}
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: evalRequestNamespace, Name: incidents.BudgetName}
	if err := g.admin.Get(ctx, key, cm); err != nil {
		t.Fatal(err)
	}
	if used := cm.Data[incidents.BudgetUsedKey]; used != "" {
		n, err := strconv.Atoi(used)
		if err != nil {
			t.Fatalf("the budget's count %q: %v", used, err)
		}
		c.analyses = n
	}
	c.calls = len(g.model.Requests())
	return c
}

func (c counts) String() string {
	var b strings.Builder
	for _, inc := range c.incidents {
		fmt.Fprintf(&b, "  Incident %s: reason %s, phase %s, count %d, cause %t, absorbed %v, foldedInto %v\n",
			inc.Name, inc.Spec.Reason, inc.Status.Phase, inc.Status.Count, inc.Status.Cause, inc.Status.Absorbed,
			inc.Status.FoldedInto)
	}
	fmt.Fprintf(&b, "  %d requests, %d analyses, %d model calls", len(c.requests), c.analyses, c.calls)
	return b.String()
}

// open returns the Incidents that were not folded into a cause.
func (c counts) open() []remediationv1alpha1.Incident {
	var out []remediationv1alpha1.Incident
	for _, inc := range c.incidents {
		if inc.Status.FoldedInto == nil {
			out = append(out, inc)
		}
	}
	return out
}

// waitFor polls the counts of the oom subject until done holds, or fails the test with the last counts.
func (g *groupingEnv) waitFor(t *testing.T, what string, done func(counts) bool) {
	t.Helper()
	deadline := time.Now().Add(groupingWait)
	for {
		c := g.counts(t, oomSubject)
		if done(c) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached in %s, the subject has:\n%s", what, groupingWait, c)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// absorbedOf returns the counts of an Incident's absorbed tally by reason.
func absorbedOf(inc remediationv1alpha1.Incident) map[remediationv1alpha1.AbsorbedReason]int64 {
	out := map[remediationv1alpha1.AbsorbedReason]int64{}
	for _, a := range inc.Status.Absorbed {
		out[a.Reason] = a.Count
	}
	return out
}

// oomFixture is the oom scenario's cluster (objects.yaml: the Deployment, its ReplicaSet and the pod whose
// status shows the kill) and the kubelet events of its fault (grouping.yaml), with the shift that moves their
// times so that the kill (finishedAt 11:56:00 in the fixture) happened half a minute ago.
func oomFixture(t *testing.T) (s *scenario, cluster, events []*unstructured.Unstructured, shift time.Duration) {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	s, err = loadScenario(scheme, "oom")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(scenariosDir, "oom")
	kill := time.Date(2026, 11, 4, 11, 56, 0, 0, time.UTC)
	return s, readFixture(t, filepath.Join(dir, "objects.yaml")), readFixture(t, filepath.Join(dir, groupingFile)),
		time.Now().Add(-30 * time.Second).Sub(kill).Truncate(time.Second)
}

// oomWorkload is the Deployment of the oom scenario, and the name of its container.
const oomWorkload = "web"

const oomSubject = nsDemo + "/" + oomWorkload

// checkOneAnalysis checks that the subject's Incidents leave one open, the cause, analyzed once, with at
// most one request.
func checkOneAnalysis(t *testing.T, c counts) remediationv1alpha1.Incident {
	t.Helper()
	open := c.open()
	if len(open) != 1 {
		t.Fatalf("the oom fault left %d Incidents open for %s, want 1:\n%s", len(open), oomSubject, c)
	}
	cause := open[0]
	if cause.Spec.Reason != collector.ReasonOOMKilled || !cause.Status.Cause {
		t.Fatalf("the open Incident is not the OOMKilled cause:\n%s", c)
	}
	if c.analyses != 1 || c.calls != 1 {
		t.Fatalf("the oom fault took %d analyses and %d model calls, want 1 and 1:\n%s", c.analyses, c.calls, c)
	}
	if len(c.requests) > 1 {
		t.Fatalf("the oom fault gave %d requests, want at most 1:\n%s", len(c.requests), c)
	}
	for _, rr := range c.requests {
		if rr.Spec.IncidentRef.Name != cause.Name {
			t.Fatalf("request %s answers Incident %s, not the cause %s", rr.Name, rr.Spec.IncidentRef.Name, cause.Name)
		}
	}
	return cause
}

// TestGroupingOOMOpensOneIncident is the oom case of ADR-0039 when the cause comes first, as in the lab: the
// pod status shows the OOM kill, and the kubelet's BackOff and readiness probe events of the web container
// follow it. One Incident opens, the cause's, and counts both consequences in its absorbed tally; it is
// analyzed once and gives exactly one request. Before the rule the lab opened three Incidents (OOMKilled,
// BackOff, Unhealthy), each analyzed (#432).
//
// The case pins that order: the events are created only once the cause's Incident exists. The collector's pod
// and event reconcilers deliver concurrently, and a consequence seen once that reaches the Incident controller
// just before its cause is held as a first sighting there, never counted on the cause unless it is seen again,
// so with both created at once the tally stayed empty in some runs.
func TestGroupingOOMOpensOneIncident(t *testing.T) {
	s, cluster, events, shift := oomFixture(t)
	g := startGrouping(t, s, nil)
	g.create(t, cluster, shift)
	g.waitFor(t, "the cause's Incident opened", func(c counts) bool {
		for _, inc := range c.incidents {
			if inc.Status.Cause {
				return true
			}
		}
		return false
	})
	g.create(t, events, shift)

	g.waitFor(t, "the cause analyzed, with both consequences counted on it", func(c counts) bool {
		for _, inc := range c.incidents {
			abs := absorbedOf(inc)
			if inc.Status.Cause && abs[remediationv1alpha1.AbsorbedBackOff] > 0 &&
				abs[remediationv1alpha1.AbsorbedUnhealthy] > 0 && c.analyses > 0 {
				return true
			}
		}
		return false
	})
	time.Sleep(groupingSettle)
	c := g.counts(t, oomSubject)
	if len(c.incidents) != 1 {
		t.Fatalf("the oom fault opened %d Incidents for %s, want 1:\n%s", len(c.incidents), oomSubject, c)
	}
	checkOneAnalysis(t, c)
	if len(c.requests) != 1 {
		t.Fatalf("the oom fault gave %d requests, want the one of the recorded reply:\n%s", len(c.requests), c)
	}
}

// TestGroupingOOMFoldsALateCause is the oom case when a consequence Incident opened before its cause
// (ADR-0039 points 12 to 14): an Unhealthy Incident of the web container, opened from the probe failures
// before the kill and not analyzed yet, is folded into the OOMKilled Incident when that opens. One Incident
// stays open and is analyzed once, and the folded one has no analysis and no request.
//
// The analyses wait until the case has seen the fold (the gate of startGrouping): the Unhealthy Incident is
// due a debounce after its last seen, and a run slow to open the cause analyzed it first, with the one
// recorded reply.
func TestGroupingOOMFoldsALateCause(t *testing.T) {
	s, cluster, events, shift := oomFixture(t)
	gate := make(chan struct{})
	var opened sync.Once
	open := func() { opened.Do(func() { close(gate) }) }
	t.Cleanup(open)
	g := startGrouping(t, s, gate)
	// The owners first: the consequence Incident names the Deployment's UID.
	g.create(t, cluster[:2], shift)
	ctx := t.Context()
	fp := fmt.Sprintf("sha256:%064x", 0x795)
	late := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: evalRequestNamespace, Name: incidents.Name(fp)},
		Spec: remediationv1alpha1.IncidentSpec{
			Fingerprint: fp, Reason: collector.ReasonUnhealthy, Type: corev1.EventTypeWarning,
			ReportingController: "kubelet",
			Symptom:             "Readiness probe failed: Get <url>: dial tcp <addr>: connect: connection refused",
			Subject: remediationv1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: nsDemo,
				Name: oomWorkload, UID: remediationv1alpha1.UID(g.mapped(t, "00000000-0000-4000-8000-0000000003c1"))},
		},
	}
	if err := g.admin.Create(ctx, late); err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	began := metav1.NewTime(now.Add(-3 * time.Minute))
	late.Status = remediationv1alpha1.IncidentStatus{
		Phase: remediationv1alpha1.IncidentOpen, FirstSeen: &began, LastSeen: &now, Count: 3,
		Containers: []remediationv1alpha1.Name{oomWorkload},
		Occurrences: []remediationv1alpha1.OccurrenceRecord{
			{ID: "event:0a1b2c3d-4e5f-4a6b-8c7d-000000000795", Total: 3, Began: &began},
		},
	}
	if err := g.admin.Status().Update(ctx, late); err != nil {
		t.Fatal(err)
	}
	// Then the kill: the pod and the kubelet's events.
	g.create(t, append(cluster[2:], events...), shift)

	g.waitFor(t, "the Unhealthy Incident folded", func(c counts) bool {
		for _, inc := range c.incidents {
			if inc.Name == late.Name {
				return inc.Status.FoldedInto != nil
			}
		}
		return false
	})
	if c := g.counts(t, oomSubject); c.analyses != 0 || c.calls != 0 {
		t.Fatalf("an analysis ran while the gate was closed:\n%s", c)
	}
	open()
	g.waitFor(t, "the cause analyzed", func(c counts) bool { return c.analyses > 0 })
	time.Sleep(groupingSettle)
	c := g.counts(t, oomSubject)
	cause := checkOneAnalysis(t, c)
	for _, inc := range c.incidents {
		if inc.Name != late.Name {
			continue
		}
		if inc.Status.Phase != remediationv1alpha1.IncidentResolved || inc.Status.FoldedInto.Name != cause.Name {
			t.Fatalf("the Unhealthy Incident is not folded into the cause %s:\n%s", cause.Name, c)
		}
		if inc.Status.LastAnalysisTime != nil {
			t.Fatalf("the folded Incident was analyzed:\n%s", c)
		}
	}
}

// TestGroupingStartupProbeOpensNothing is the startup-probe case of ADR-0039 (#726): a readiness probe that
// failed once while a new pod of demo/worker started, the pod Ready seconds later and never restarted. The
// collector decides the failure was part of the start, so no observation, no Incident and no analysis follow.
// The lab run of 2026-11-14 opened an Incident and proposed a memory increase from it.
func TestGroupingStartupProbeOpensNothing(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadScenario(scheme, "startup-probe")
	if err != nil {
		t.Fatal(err)
	}
	objs := readFixture(t, filepath.Join(scenariosDir, "startup-probe", "objects.yaml"))
	failure := time.Date(2026, 11, 14, 15, 29, 44, 0, time.UTC)
	shift := time.Now().Add(-10 * time.Second).Sub(failure).Truncate(time.Second)
	g := startGrouping(t, s, nil)
	skipped := settledReadinessSkipped(t)
	g.create(t, objs, shift)

	// The collector's decision on the event: skipped as part of the pod's start.
	deadline := time.Now().Add(groupingWait)
	for settledReadinessSkipped(t) == skipped {
		if time.Now().After(deadline) {
			t.Fatalf("the collector did not skip the probe failure of the starting pod in %s", groupingWait)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Longer than the debounce, so an Incident that had opened would have been analyzed too.
	time.Sleep(groupingDebounce + groupingSettle)
	var incs remediationv1alpha1.IncidentList
	if err := g.admin.List(t.Context(), &incs, client.InNamespace(evalRequestNamespace)); err != nil {
		t.Fatal(err)
	}
	c := g.counts(t, nsDemo+"/worker")
	if len(incs.Items) != 0 || c.analyses != 0 || c.calls != 0 {
		t.Fatalf("the startup-probe case opened %d Incidents, want none:\n%s", len(incs.Items), c)
	}
}

// settledReadinessSkipped reads the collector's count of readiness probe failures it skipped as part of a pod's
// start or shutdown, from the registry the analyzer's metrics endpoint serves.
func settledReadinessSkipped(t *testing.T) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "shadewright_analyzer_settled_probe_failures_skipped_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "probe" && l.GetValue() == "Readiness" {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
