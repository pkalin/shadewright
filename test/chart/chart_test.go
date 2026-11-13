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

// Package chart checks that the Helm chart in dist/chart installs exactly what config/default and config/watch
// install: every Role and ClusterRole grants the same verbs on the same resources, every binding binds the same
// subjects, and the NetworkPolicies, Services, CRDs and security contexts agree. Extra or missing permissions
// fail the test (design 2.2, invariant 6). It needs helm and kustomize: it is skipped when they are missing
// unless CHART_TEST_REQUIRED=1 (as in CI and make test-chart).
package chart

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	operatorNS = "shadewright-system"
	release    = "shadewright"

	kindClusterRole        = "ClusterRole"
	kindRole               = "Role"
	kindRoleBinding        = "RoleBinding"
	kindClusterRoleBinding = "ClusterRoleBinding"
	chartName              = "shadewright"
	unknownField           = "additional properties"
	// memoryLimit is the key the schema requires under resources.limits.
	memoryLimit  = "memory"
	analyzerComp = "analyzer"
	executorComp = "executor"
	verifierComp = "verifier"
)

// components are the Deployments of the chart.
var components = []string{executorComp, analyzerComp, verifierComp}

// helperRole matches the admin, editor and viewer ClusterRoles for people; the chart installs them only on request.
var helperRole = regexp.MustCompile(`-(admin|editor|viewer)-role$`)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "PROJECT")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root (PROJECT) not found")
		}
		dir = parent
	}
}

// tool finds a binary from an environment variable, bin/ of the repository or PATH.
func tool(t *testing.T, envVar, name string) string {
	t.Helper()
	if p := os.Getenv(envVar); p != "" {
		return p
	}
	local := filepath.Join(repoRoot(t), "bin", name)
	if _, err := os.Stat(local); err == nil {
		return local
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if os.Getenv("CHART_TEST_REQUIRED") == "1" {
		t.Fatalf("%s not found (set %s or install it)", name, envVar)
	}
	t.Skipf("%s not found; set %s or CHART_TEST_REQUIRED=1 to fail instead", name, envVar)
	return ""
}

func run(t *testing.T, dir string, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return out
}

// objects decodes a multi-document YAML stream.
func objects(t *testing.T, data []byte) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		u := &unstructured.Unstructured{}
		err := dec.Decode(&u.Object)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("decoding YAML: %v", err)
		}
		if len(u.Object) == 0 {
			continue
		}
		out = append(out, u)
	}
}

func key(u *unstructured.Unstructured) string {
	return fmt.Sprintf("%s/%s/%s", u.GetKind(), u.GetNamespace(), u.GetName())
}

func convert[T any](t *testing.T, u *unstructured.Unstructured) *T {
	t.Helper()
	var out T
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &out); err != nil {
		t.Fatalf("converting %s: %v", key(u), err)
	}
	return &out
}

// grants expands rules into one line per group, resource, verb and resource name, so that two roles that
// grant the same things are equal however their rules are grouped.
func grants(rules []rbacv1.PolicyRule) []string {
	var out []string
	for _, r := range rules {
		names := r.ResourceNames
		if len(names) == 0 {
			names = []string{"*all*"}
		}
		for _, v := range r.Verbs {
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					for _, n := range names {
						out = append(out, fmt.Sprintf("%s|%s|%s|%s", g, res, v, n))
					}
				}
			}
			for _, u := range r.NonResourceURLs {
				out = append(out, fmt.Sprintf("nonresource|%s|%s", u, v))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// diffLines returns the lines only in want ("missing") and only in got ("extra").
func diffLines(want, got []string) (missing, extra []string) {
	for _, w := range want {
		if !slices.Contains(got, w) {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			extra = append(extra, g)
		}
	}
	return missing, extra
}

// describe reduces an RBAC object to what it grants or binds.
func describe(t *testing.T, u *unstructured.Unstructured) []string {
	t.Helper()
	switch u.GetKind() {
	case kindRole:
		return grants(convert[rbacv1.Role](t, u).Rules)
	case kindClusterRole:
		return grants(convert[rbacv1.ClusterRole](t, u).Rules)
	case kindRoleBinding:
		b := convert[rbacv1.RoleBinding](t, u)
		return bindingLines(b.RoleRef, b.Subjects)
	case kindClusterRoleBinding:
		b := convert[rbacv1.ClusterRoleBinding](t, u)
		return bindingLines(b.RoleRef, b.Subjects)
	}
	t.Fatalf("not an RBAC object: %s", key(u))
	return nil
}

func bindingLines(ref rbacv1.RoleRef, subjects []rbacv1.Subject) []string {
	out := make([]string, 0, 1+len(subjects))
	out = append(out, fmt.Sprintf("roleRef|%s|%s|%s", ref.APIGroup, ref.Kind, ref.Name))
	for _, s := range subjects {
		out = append(out, fmt.Sprintf("subject|%s|%s|%s|%s", s.APIGroup, s.Kind, s.Namespace, s.Name))
	}
	slices.Sort(out)
	return out
}

func isRBAC(u *unstructured.Unstructured) bool {
	switch u.GetKind() {
	case kindRole, kindClusterRole, kindRoleBinding, kindClusterRoleBinding:
		return true
	}
	return false
}

// compareRBAC returns one message per difference between the expected and the rendered RBAC objects. An object
// or a grant in the chart that kustomize does not have is "extra"; one that the chart lacks is "missing".
func compareRBAC(t *testing.T, want, got map[string]*unstructured.Unstructured) []string {
	t.Helper()
	var problems []string
	for k, g := range got {
		w, ok := want[k]
		if !ok {
			problems = append(problems, "extra object in the chart: "+k)
			continue
		}
		missing, extra := diffLines(describe(t, w), describe(t, g))
		for _, l := range extra {
			problems = append(problems, fmt.Sprintf("%s: extra in the chart: %s", k, l))
		}
		for _, l := range missing {
			problems = append(problems, fmt.Sprintf("%s: missing from the chart: %s", k, l))
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			problems = append(problems, "missing from the chart: "+k)
		}
	}
	slices.Sort(problems)
	return problems
}

type rendered struct {
	kustomize map[string]*unstructured.Unstructured // config/default and config/watch
	// kustomizeDefault is config/default alone: what the chart installs with no watched namespace.
	kustomizeDefault map[string]*unstructured.Unstructured
	// chart holds every object helm template renders except the helm test hooks, which are in tests.
	chart map[string]*unstructured.Unstructured
	tests map[string]*unstructured.Unstructured
}

// testValues are the placeholder values of the render tests (a cluster ID, image digests, an executor identity
// and the namespace demo, as config/ has).
func testValues(t *testing.T) string {
	return filepath.Join(repoRoot(t), "test", "chart", "testdata", "values.yaml")
}

// helmArgs is the helm template command line for the chart with the test values plus extra --set arguments.
func helmArgs(t *testing.T, helperRoles bool, extraSet ...string) []string {
	t.Helper()
	set := append([]string{fmt.Sprintf("rbac.helpers.enabled=%t", helperRoles)}, extraSet...)
	return helmArgsWith(t, []string{"-f", testValues(t)}, set...)
}

// helmArgsWith is the helm template command line with the given value files (as -f arguments) and --set
// arguments.
func helmArgsWith(t *testing.T, files []string, set ...string) []string {
	t.Helper()
	root := repoRoot(t)
	args := make([]string, 0, 5+len(files)+2*len(set))
	args = append(args, "template", release, filepath.Join(root, "dist", "chart"), "--namespace", operatorNS)
	args = append(args, files...)
	for _, s := range set {
		args = append(args, "--set", s)
	}
	return args
}

// isTestHook reports whether u is a helm test hook (templates/tests): it exists only while helm test runs.
func isTestHook(u *unstructured.Unstructured) bool {
	return slices.Contains(strings.Split(u.GetAnnotations()["helm.sh/hook"], ","), "test")
}

// helmFails renders the chart with the given values and returns helm's error output; it fails the test if the
// render succeeds.
func helmFails(t *testing.T, extraSet ...string) string {
	t.Helper()
	cmd := exec.Command(tool(t, "HELM", "helm"), helmArgs(t, false, extraSet...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("helm template succeeded with %v, want an error", extraSet)
	}
	return stderr.String()
}

func render(t *testing.T, helperRoles bool, extraSet ...string) rendered {
	t.Helper()
	return renderArgs(t, helmArgs(t, helperRoles, extraSet...))
}

// renderArgs renders kustomize and the chart with the given helm template command line.
func renderArgs(t *testing.T, args []string) rendered {
	t.Helper()
	root := repoRoot(t)
	kustomize := tool(t, "KUSTOMIZE", "kustomize")
	helm := tool(t, "HELM", "helm")

	r := rendered{
		kustomize:        map[string]*unstructured.Unstructured{},
		kustomizeDefault: map[string]*unstructured.Unstructured{},
		chart:            map[string]*unstructured.Unstructured{},
		tests:            map[string]*unstructured.Unstructured{},
	}
	for _, dir := range []string{"config/default", "config/watch"} {
		for _, u := range objects(t, run(t, root, kustomize, "build", dir)) {
			r.kustomize[key(u)] = u
			if dir == "config/default" {
				r.kustomizeDefault[key(u)] = u
			}
		}
	}
	for _, u := range objects(t, run(t, root, helm, args...)) {
		if isTestHook(u) {
			r.tests[key(u)] = u
			continue
		}
		r.chart[key(u)] = u
	}
	if len(r.chart) == 0 {
		t.Fatal("helm template rendered no objects: the comparison would prove nothing")
	}
	return r
}

// addKustomize builds one more kustomize directory into r.kustomize.
func addKustomize(t *testing.T, r *rendered, dir string) {
	t.Helper()
	root := repoRoot(t)
	for _, u := range objects(t, run(t, root, tool(t, "KUSTOMIZE", "kustomize"), "build", dir)) {
		r.kustomize[key(u)] = u
	}
}

func rbacOnly(in map[string]*unstructured.Unstructured, dropHelpers bool) map[string]*unstructured.Unstructured {
	out := map[string]*unstructured.Unstructured{}
	for k, u := range in {
		if !isRBAC(u) || (dropHelpers && u.GetKind() == kindClusterRole && helperRole.MatchString(u.GetName())) {
			continue
		}
		out[k] = u
	}
	return out
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRBACEqualsKustomize(t *testing.T) {
	for _, helpers := range []bool{false, true} {
		t.Run(fmt.Sprintf("helpers=%t", helpers), func(t *testing.T) {
			r := render(t, helpers)
			want := rbacOnly(r.kustomize, !helpers)
			if len(want) < 15 {
				t.Fatalf("only %d RBAC objects from kustomize: the comparison would prove nothing", len(want))
			}
			for _, p := range compareRBAC(t, want, rbacOnly(r.chart, false)) {
				t.Error(p)
			}
		})
	}
}

// TestComparisonFailsOnAnExtraVerb is the check on the check: it adds a verb, a resource and a role to the
// rendered chart and expects the comparison to name each of them.
func TestComparisonFailsOnAnExtraVerb(t *testing.T) {
	r := render(t, false)
	want := rbacOnly(r.kustomize, true)
	got := rbacOnly(r.chart, false)

	role := got["Role/"+operatorNS+"/shadewright-executor-role"]
	if role == nil {
		t.Fatal("executor Role not rendered")
	}
	mutated := role.DeepCopy()
	rules, _, _ := unstructured.NestedSlice(mutated.Object, "rules")
	rules = append(rules, map[string]any{
		"apiGroups": []any{""}, "resources": []any{"secrets"}, "verbs": []any{"get"},
	})
	if err := unstructured.SetNestedSlice(mutated.Object, rules, "rules"); err != nil {
		t.Fatal(err)
	}
	got["Role/"+operatorNS+"/shadewright-executor-role"] = mutated

	extra := &unstructured.Unstructured{Object: map[string]any{}}
	extra.SetAPIVersion("rbac.authorization.k8s.io/v1")
	extra.SetKind(kindClusterRole)
	extra.SetName("shadewright-extra")
	got[key(extra)] = extra

	problems := strings.Join(compareRBAC(t, want, got), "\n")
	for _, s := range []string{"|secrets|get|", "extra object in the chart: ClusterRole//shadewright-extra"} {
		if !strings.Contains(problems, s) {
			t.Errorf("comparison did not report %q; got:\n%s", s, problems)
		}
	}
}

func TestNetworkPoliciesServicesAndCRDsEqualKustomize(t *testing.T) {
	r := render(t, false)
	check := func(kind string, spec func(*unstructured.Unstructured) any) {
		n := 0
		for k, w := range r.kustomize {
			if w.GetKind() != kind {
				continue
			}
			n++
			g, ok := r.chart[k]
			if !ok {
				t.Errorf("%s: missing from the chart", k)
				continue
			}
			ws, gs := asJSON(t, spec(w)), asJSON(t, spec(g))
			if ws != gs {
				t.Errorf("%s: differs\n kustomize: %s\n chart:     %s", k, ws, gs)
			}
		}
		for k, g := range r.chart {
			if g.GetKind() == kind {
				if _, ok := r.kustomize[k]; !ok {
					t.Errorf("%s: in the chart only", k)
				}
			}
		}
		if n == 0 {
			t.Errorf("no %s from kustomize", kind)
		}
	}
	check("NetworkPolicy", func(u *unstructured.Unstructured) any {
		return convert[networkingv1.NetworkPolicy](t, u).Spec
	})
	check("Service", func(u *unstructured.Unstructured) any { return convert[corev1.Service](t, u).Spec })
	check("CustomResourceDefinition", func(u *unstructured.Unstructured) any { return u.Object["spec"] })
	check("ServiceAccount", func(u *unstructured.Unstructured) any { return u.GetName() })
	check("Lease", func(u *unstructured.Unstructured) any { return u.GetName() })
	check("ConfigMap", func(u *unstructured.Unstructured) any { return u.Object["data"] })
}

// normalizedPod reduces a Deployment to its Pod spec minus what differs on purpose: the image and its pull
// policy (the manifests use a placeholder, the chart a digest), the cluster ID (a value), the module grants (the
// chart knows which module roles it rendered; a kustomize install may add any of config/modules, so the
// manifests leave --module-grants out and the analyzer checks every module kind), the metrics port (the chart
// makes it optional), GOMEMLIMIT (chart only), the order of the args, and the defaulted automountServiceAccountToken.
func normalizedPod(d *appsv1.Deployment) corev1.PodSpec {
	spec := *d.Spec.Template.Spec.DeepCopy()
	if spec.AutomountServiceAccountToken == nil {
		yes := true
		spec.AutomountServiceAccountToken = &yes
	}
	if len(spec.Volumes) == 0 {
		spec.Volumes = nil
	}
	for i := range spec.Containers {
		c := &spec.Containers[i]
		c.Image, c.ImagePullPolicy = "", ""
		for j, a := range c.Args {
			if strings.HasPrefix(a, "--cluster-id=") {
				c.Args[j] = "--cluster-id=x" // a value of the install
			}
		}
		c.Args = slices.DeleteFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "--module-grants=") })
		// The chart sets GOMEMLIMIT from the memory limit (#601); the manifests do not.
		c.Env = slices.DeleteFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == "GOMEMLIMIT" })
		slices.Sort(c.Args)
		c.Ports = slices.DeleteFunc(c.Ports, func(p corev1.ContainerPort) bool { return p.Name == "metrics" })
		if len(c.VolumeMounts) == 0 {
			c.VolumeMounts = nil
		}
	}
	return spec
}

func TestDeploymentsKeepSeparateIdentitiesAndHardening(t *testing.T) {
	r := render(t, false)
	for _, name := range []string{"shadewright-executor", "shadewright-analyzer"} {
		k := "Deployment/" + operatorNS + "/" + name
		w, g := r.kustomize[k], r.chart[k]
		if w == nil || g == nil {
			t.Fatalf("%s: kustomize %v, chart %v", k, w != nil, g != nil)
		}
		wd, gd := convert[appsv1.Deployment](t, w), convert[appsv1.Deployment](t, g)
		wantSA, gotSA := wd.Spec.Template.Spec.ServiceAccountName, gd.Spec.Template.Spec.ServiceAccountName
		if wantSA != gotSA || gotSA != name {
			t.Errorf("%s: service account %q, want %q", k, gotSA, wantSA)
		}
		if len(gd.Spec.Template.Spec.Containers) != 1 || len(wd.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("%s: want one container", k)
		}
		// The whole Pod spec, not a subset: args, env, ports, probes, resources, security contexts and the rest.
		if w, g := asJSON(t, normalizedPod(wd)), asJSON(t, normalizedPod(gd)); w != g {
			t.Errorf("%s: Pod spec differs\n kustomize: %s\n chart:     %s", k, w, g)
		}
		if asJSON(t, wd.Spec.Replicas) != asJSON(t, gd.Spec.Replicas) {
			t.Errorf("%s: replicas %s, want %s", k, asJSON(t, gd.Spec.Replicas), asJSON(t, wd.Spec.Replicas))
		}
		gc := gd.Spec.Template.Spec.Containers[0]
		if !strings.Contains(gc.Image, "@sha256:") {
			t.Errorf("%s: image %q is not pinned by digest", k, gc.Image)
		}
	}
	// Identities stay apart: only the executor carries the workload identity label with the test values.
	ex := convert[appsv1.Deployment](t, r.chart["Deployment/"+operatorNS+"/shadewright-executor"])
	an := convert[appsv1.Deployment](t, r.chart["Deployment/"+operatorNS+"/shadewright-analyzer"])
	if ex.Spec.Template.Labels["azure.workload.identity/use"] != "true" {
		t.Error("executor Pods lack the workload identity label")
	}
	if _, ok := an.Spec.Template.Labels["azure.workload.identity/use"]; ok {
		t.Error("analyzer Pods carry the workload identity label without a client ID")
	}
	sa := r.chart["ServiceAccount/"+operatorNS+"/shadewright-executor"]
	if sa == nil || sa.GetAnnotations()["azure.workload.identity/client-id"] != "00000000-0000-0000-0000-000000000000" {
		t.Error("executor ServiceAccount lacks the client ID annotation")
	}
	if a := r.chart["ServiceAccount/"+operatorNS+"/shadewright-analyzer"]; a == nil || len(a.GetAnnotations()) != 0 {
		t.Error("analyzer ServiceAccount must exist and carry no identity annotation by default")
	}
}

func TestWatchNamespacesGetOneBindingEach(t *testing.T) {
	r := render(t, false, "analyzer.watchNamespaces={team-a,team-b}")
	for _, ns := range []string{"team-a", "team-b"} {
		if _, ok := r.chart["RoleBinding/"+ns+"/shadewright-analyzer-watch"]; !ok {
			t.Errorf("no watch RoleBinding in %s", ns)
		}
	}
	if _, ok := r.chart["RoleBinding/demo/shadewright-analyzer-watch"]; ok {
		t.Error("a binding for a namespace that is not listed")
	}
	dep := convert[appsv1.Deployment](t, r.chart["Deployment/"+operatorNS+"/shadewright-analyzer"])
	if !slices.Contains(dep.Spec.Template.Spec.Containers[0].Args, "--watch-namespaces=team-a,team-b") {
		t.Errorf("args %v do not carry the watched namespaces", dep.Spec.Template.Spec.Containers[0].Args)
	}
}

// allToggles turns on every optional part of the chart: the settings object, both identities (different IDs),
// two watched namespaces and the helper roles.
// kyvernoOn is the value that renders the Kyverno module's namespaced grant.
const kyvernoOn = "analyzer.modules.kyverno.enabled=true"

var allToggles = []string{
	"settings.create=true",
	"settings.spec.paused=false",
	"analyzer.identity.azure.clientId=11111111-1111-1111-1111-111111111111",
	"analyzer.watchNamespaces={demo,team-b}",
	"analyzer.modules.flux.enabled=true",
	kyvernoOn,
	"analyzer.modules.kyverno.clusterWide.enabled=true",
	// The analysis with a policy, for the policy reader role (#493), which config/analysis-policies installs.
	"analyzer.analysis.triageModel=gpt-4.1-mini-2025-04-14",
	labSuffix,
	"analyzer.analysis.actionPolicies.SetContainerMemoryLimit=demo-memory",
}

func TestRBACEqualsKustomizeWithEveryToggleOn(t *testing.T) {
	r := render(t, true, allToggles...)
	if _, ok := r.chart["RemediationSettings//default"]; !ok {
		t.Error("settings.create=true rendered no RemediationSettings object")
	}
	// config/watch binds one namespace (demo). The chart binds each listed namespace the same way: check that
	// team-b has the same binding as demo, then compare the rest.
	// The module grants that are off by default are installed by config/modules.
	for _, m := range moduleRoles {
		if m.dir != "" {
			addKustomize(t, &r, m.dir)
		}
	}
	addKustomize(t, &r, "config/analysis-policies")
	names := []string{"shadewright-analyzer-watch", "shadewright-verifier-watch"}
	for _, m := range moduleRoles {
		if !m.clusterWide {
			names = append(names, m.role)
		}
	}
	for _, name := range names {
		demo := r.chart["RoleBinding/demo/"+name]
		teamB := r.chart["RoleBinding/team-b/"+name]
		if demo == nil || teamB == nil {
			t.Fatalf("RoleBindings %s for demo and team-b not rendered", name)
		}
		if strings.Join(describe(t, demo), "\n") != strings.Join(describe(t, teamB), "\n") {
			t.Errorf("team-b binding differs from demo: %v vs %v", describe(t, teamB), describe(t, demo))
		}
		delete(r.chart, "RoleBinding/team-b/"+name)
	}

	want := rbacOnly(r.kustomize, false)
	if len(want) < 15 {
		t.Fatalf("only %d RBAC objects from kustomize: the comparison would prove nothing", len(want))
	}
	for _, p := range compareRBAC(t, want, rbacOnly(r.chart, false)) {
		t.Error(p)
	}
}

// TestComparisonFailsOnWidenedNamesAndChangedSubject checks the comparison on two more mutations: a
// resourceNames list widened by one name, and a binding whose subject is another ServiceAccount.
func TestComparisonFailsOnWidenedNamesAndChangedSubject(t *testing.T) {
	r := render(t, false)
	want := rbacOnly(r.kustomize, true)

	got := rbacOnly(r.chart, false)
	rk := "Role/" + operatorNS + "/shadewright-executor-leader-election-role"
	role := got[rk]
	if role == nil {
		t.Fatalf("%s not rendered", rk)
	}
	m := role.DeepCopy()
	rules, _, _ := unstructured.NestedSlice(m.Object, "rules")
	widened := false
	for _, rule := range rules {
		rm := rule.(map[string]any)
		if names, ok := rm["resourceNames"].([]any); ok {
			rm["resourceNames"] = append(names, "another-lease")
			widened = true
		}
	}
	if !widened {
		t.Fatal("no rule with resourceNames to widen")
	}
	if err := unstructured.SetNestedSlice(m.Object, rules, "rules"); err != nil {
		t.Fatal(err)
	}
	got[rk] = m
	if p := strings.Join(compareRBAC(t, want, got), "\n"); !strings.Contains(p, "another-lease") {
		t.Errorf("a widened resourceNames was not reported; got:\n%s", p)
	}

	got = rbacOnly(r.chart, false)
	bk := "ClusterRoleBinding//shadewright-executor-rolebinding"
	b := got[bk]
	if b == nil {
		t.Fatalf("%s not rendered", bk)
	}
	m = b.DeepCopy()
	subjects, _, _ := unstructured.NestedSlice(m.Object, "subjects")
	if len(subjects) == 0 {
		t.Fatal("binding has no subjects")
	}
	subjects[0].(map[string]any)["name"] = "shadewright-analyzer"
	if err := unstructured.SetNestedSlice(m.Object, subjects, "subjects"); err != nil {
		t.Fatal(err)
	}
	got[bk] = m
	p := strings.Join(compareRBAC(t, want, got), "\n")
	if !strings.Contains(p, "missing from the chart: subject|") || !strings.Contains(p, "extra in the chart: subject|") {
		t.Errorf("a changed binding subject was not reported; got:\n%s", p)
	}
}

func TestChartRefusesWeakenedValues(t *testing.T) {
	const (
		id       = "00000000-0000-0000-0000-000000000000"
		labels   = "podLabels"
		annots   = "serviceAccount.annotations"
		separate = "separate identities"
		unknown  = unknownField
		saAnn    = `serviceAccount.annotations.azure\.workload\.identity/`
	)
	type refusal struct {
		set  string
		want string
	}
	cases := map[string]refusal{
		"one client ID for both":         {"analyzer.identity.azure.clientId=" + id, separate},
		"same ID in another case":        {"analyzer.identity.azure.clientId=" + strings.ToUpper(id), separate},
		"identity annotation exec":       {"executor." + saAnn + "client-id=x", annots},
		"identity annotation analyzer":   {"analyzer." + saAnn + "tenant-id=x", annots},
		"identity pod label":             {`analyzer.podLabels.azure\.workload\.identity/use=true`, labels},
		"control-plane pod label":        {"executor.podLabels.control-plane=analyzer", labels},
		"chart pod label":                {`executor.podLabels.app\.kubernetes\.io/component=analyzer`, labels},
		"unknown key under executor":     {"executor.resource.limits.cpu=1", unknown},
		"unknown key under analyzer":     {"analyzer.podLabel.a=b", unknown},
		"watchNamespaces under executor": {"executor.watchNamespaces={a}", unknown},
		"limits removed":                 {"executor.resources.limits=null", "limits"},
	}
	// Every flag the chart renders itself, for both components.
	for _, flag := range []string{
		"leader-elect", "health-probe-bind-address", "metrics-bind-address", "metrics-secure", "namespace",
		"watch-namespaces", "cluster-id", "max-incidents", "module-grants",
	} {
		for _, comp := range components {
			cases["extra arg "+flag+" ("+comp+")"] = refusal{fmt.Sprintf("%s.extraArgs={--%s=x}", comp, flag), "--" + flag}
		}
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if out := helmFails(t, c.set); !strings.Contains(out, c.want) {
				t.Errorf("error does not mention %q:\n%s", c.want, out)
			}
		})
	}
	// Flags and labels that are not reserved are fine.
	helm := tool(t, "HELM", "helm")
	ok := helmArgs(t, false, "executor.extraArgs={--zap-log-level=debug}", "analyzer.podLabels.team=a")
	run(t, repoRoot(t), helm, ok...)
}

// The settings object holds the kill switch (spec.paused). The chart creates it only when it does not exist
// (lookup in the template, covered by hack/chart-install-test.sh on a real cluster, since helm template has no
// cluster and lookup finds nothing there) and never lets Helm delete it: a hook-delete-policy would remove the
// live object, and a normal release object would be rewritten by every upgrade.
func TestSettingsObjectIsAHookThatHelmNeverDeletes(t *testing.T) {
	if r := render(t, false); r.chart["RemediationSettings//default"] != nil {
		t.Error("the settings object is rendered although settings.create is false")
	}
	r := render(t, false, "settings.create=true", "settings.spec.paused=true")
	obj := r.chart["RemediationSettings//default"]
	if obj == nil {
		t.Fatal("settings.create=true rendered no RemediationSettings object")
	}
	ann := obj.GetAnnotations()
	if got := ann["helm.sh/hook"]; got != "post-install,post-upgrade" {
		t.Errorf("hook annotation = %q, want post-install,post-upgrade (the CRD comes from the same release)", got)
	}
	if p, ok := ann["helm.sh/hook-delete-policy"]; ok {
		t.Errorf("hook-delete-policy %q would let Helm delete the live settings object", p)
	}
	if paused, _, _ := unstructured.NestedBool(obj.Object, "spec", "paused"); !paused {
		t.Error("spec.paused from the values is not rendered")
	}
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "dist/chart/templates/settings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `lookup "remediation.shadewright.io/v1alpha1" "RemediationSettings"`) {
		t.Error("templates/settings.yaml does not check for an existing object with lookup")
	}
}

func analyzerArgs(t *testing.T, r rendered) []string {
	t.Helper()
	var dep appsv1.Deployment
	u := r.chart["Deployment/"+operatorNS+"/shadewright-analyzer"]
	if u == nil {
		t.Fatal("no analyzer Deployment rendered")
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &dep); err != nil {
		t.Fatal(err)
	}
	return dep.Spec.Template.Spec.Containers[0].Args
}

func watchRoleHasPodLogs(t *testing.T, r rendered) bool {
	t.Helper()
	u := r.chart["ClusterRole//shadewright-analyzer-watch-role"]
	if u == nil {
		t.Fatal("no analyzer watch ClusterRole rendered")
	}
	var cr rbacv1.ClusterRole
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &cr); err != nil {
		t.Fatal(err)
	}
	for _, rule := range cr.Rules {
		if slices.Contains(rule.Resources, "pods/log") {
			return true
		}
	}
	return false
}

// TestAnalyzerContextSourcesFollowValues checks that the pod logs grant and flag go together (on by default,
// off together), and that the Prometheus flags appear only when set.
func TestAnalyzerContextSourcesFollowValues(t *testing.T) {
	on := render(t, false)
	if !watchRoleHasPodLogs(t, on) {
		t.Error("default values: the watch role has no pods/log rule")
	}
	for _, a := range analyzerArgs(t, on) {
		if strings.HasPrefix(a, "--pod-logs") || strings.HasPrefix(a, "--prometheus") {
			t.Errorf("default values render %s, want none", a)
		}
	}

	off := render(t, false, "analyzer.podLogs.enabled=false")
	if watchRoleHasPodLogs(t, off) {
		t.Error("podLogs.enabled=false still renders the pods/log rule")
	}
	if !slices.Contains(analyzerArgs(t, off), "--pod-logs=false") {
		t.Errorf("podLogs.enabled=false renders no --pod-logs=false: %v", analyzerArgs(t, off))
	}

	prom := render(t, false, "analyzer.prometheus.url=http://prometheus-server.prometheus.svc",
		"analyzer.prometheus.cluster=lab")
	args := analyzerArgs(t, prom)
	for _, want := range []string{"--prometheus-url=http://prometheus-server.prometheus.svc", "--prometheus-cluster=lab"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v lack %s", args, want)
		}
	}
	// A cluster without a URL renders neither flag.
	for _, a := range analyzerArgs(t, render(t, false, "analyzer.prometheus.cluster=lab")) {
		if strings.HasPrefix(a, "--prometheus") {
			t.Errorf("cluster without url renders %s", a)
		}
	}
}

func TestChartRefusesBadContextSourceValues(t *testing.T) {
	for name, set := range map[string]string{
		"url without scheme":   "analyzer.prometheus.url=prometheus:9090",
		"url with a space":     "analyzer.prometheus.url=http://a b",
		"url with user info":   "analyzer.prometheus.url=http://user@prometheus:9090",
		"url with a query":     "analyzer.prometheus.url=http://prometheus:9090/?x=1",
		"url with a fragment":  "analyzer.prometheus.url=http://prometheus:9090/#x",
		"url with a bare ?":    "analyzer.prometheus.url=http://prometheus:9090?",
		"url without a host":   "analyzer.prometheus.url=http:///path",
		"ftp url":              "analyzer.prometheus.url=ftp://prometheus:9090",
		"keda not boolean":     "analyzer.modules.keda.enabled=maybe",
		"unknown module":       "analyzer.modules.other.enabled=true",
		"podLogs not boolean":  "analyzer.podLogs.enabled=maybe",
		"unknown prometheus":   "analyzer.prometheus.token=x",
		"cluster with a space": "analyzer.prometheus.cluster=a b",
	} {
		t.Run(name, func(t *testing.T) { helmFails(t, set) })
	}
	for _, flag := range []string{"pod-logs", "prometheus-url", "prometheus-cluster"} {
		if out := helmFails(t, "analyzer.extraArgs={--"+flag+"=x}"); !strings.Contains(out, "--"+flag) {
			t.Errorf("extra arg %s not refused:\n%s", flag, out)
		}
	}
}

const analyzerClientID = "analyzer.identity.azure.clientId=22222222-2222-2222-2222-222222222222"

// labSuffix is the allow-list a triage model needs.
const labSuffix = "analyzer.analysis.endpointSuffixes={.lab.services.ai.azure.com}"

// triageMini turns the analysis on with a triage model.
const triageMini = "analyzer.analysis.triageModel=gpt-4.1-mini"

// TestAnalyzerAnalysisFollowsValues checks that the analysis flags appear only with a triage model (the
// analysis is off by default), and together with it.
func TestAnalyzerAnalysisFollowsValues(t *testing.T) {
	isAnalysisFlag := func(a string) bool {
		for _, f := range []string{"--triage-model", "--escalation-model", "--model-endpoint-suffixes", "--action-policies"} {
			if strings.HasPrefix(a, f) {
				return true
			}
		}
		return false
	}
	if args := analyzerArgs(t, render(t, false)); slices.ContainsFunc(args, isAnalysisFlag) {
		t.Errorf("default values render analysis flags: %v", args)
	}
	// Without a triage model the other values render nothing.
	if args := analyzerArgs(t, render(t, false, "analyzer.analysis.actionPolicies.SetContainerMemoryLimit=memory",
		"analyzer.analysis.endpointSuffixes={.a.example}")); slices.ContainsFunc(args, isAnalysisFlag) {
		t.Errorf("values without a triage model render analysis flags: %v", args)
	}
	args := analyzerArgs(t, render(t, false, analyzerClientID,
		"analyzer.analysis.triageModel=gpt-4.1-mini-2025-04-14", "analyzer.analysis.escalationModel=gpt-4.1-2025-04-14",
		"analyzer.analysis.endpointSuffixes={.services.ai.azure.com,.openai.azure.com}",
		"analyzer.analysis.actionPolicies.SetContainerMemoryLimit=memory-limits",
		"analyzer.analysis.actionPolicies.AdjustKedaMaxReplicas=keda"))
	for _, want := range []string{"--triage-model=gpt-4.1-mini-2025-04-14", "--escalation-model=gpt-4.1-2025-04-14",
		"--model-endpoint-suffixes=.services.ai.azure.com,.openai.azure.com",
		"--action-policies=AdjustKedaMaxReplicas=keda,SetContainerMemoryLimit=memory-limits"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v lack %s", args, want)
		}
	}
	// No record store unless its account is set; with it, both flags, the container defaulting to analyses.
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--analysis-store-") }) {
		t.Errorf("args %v hold a record store without its account", args)
	}
	args = analyzerArgs(t, render(t, false, analyzerClientID, labSuffix,
		"analyzer.analysis.triageModel=gpt-4.1-mini-2025-04-14", "analyzer.analysis.recordStore.azureBlob.account=stswanabc"))
	for _, want := range []string{"--analysis-store-account=stswanabc", "--analysis-store-container=analyses"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v lack %s", args, want)
		}
	}
}

// A record store set to null, which Helm takes as "remove the default", renders with no store, as the
// helper that checks it does (#454).
func TestChartRendersANullRecordStore(t *testing.T) {
	args := analyzerArgs(t, render(t, false, analyzerClientID, labSuffix, triageMini,
		"analyzer.analysis.recordStore=null"))
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--analysis-store-") }) {
		t.Errorf("args %v hold a record store, want none", args)
	}
}

func TestChartRefusesBadAnalysisValues(t *testing.T) {
	for name, set := range map[string][]string{
		"triage model without identity":  {triageMini, labSuffix},
		"triage model without suffixes":  {analyzerClientID, triageMini},
		"escalation without triage":      {analyzerClientID, "analyzer.analysis.escalationModel=gpt-4.1"},
		"model with a space":             {analyzerClientID, labSuffix, "analyzer.analysis.triageModel=gpt 4"},
		"model with a comma":             {analyzerClientID, labSuffix, "analyzer.analysis.triageModel=a\\,b"},
		"suffix without a dot":           {"analyzer.analysis.endpointSuffixes={azure.com}"},
		"suffix of one label":            {"analyzer.analysis.endpointSuffixes={.com}"},
		"suffix in upper case":           {"analyzer.analysis.endpointSuffixes={.Azure.com}"},
		"policy for an unknown action":   {"analyzer.analysis.actionPolicies.Exec=all"},
		"policy name with an underscore": {"analyzer.analysis.actionPolicies.SetContainerMemoryLimit=a_b"},
		"unknown analysis value":         {"analyzer.analysis.apiKey=x"},
		"record store without triage":    {analyzerClientID, "analyzer.analysis.recordStore.azureBlob.account=stswanabc"},
		"record store without container": {analyzerClientID, labSuffix, triageMini,
			"analyzer.analysis.recordStore.azureBlob.account=stswanabc", "analyzer.analysis.recordStore.azureBlob.container="},
		"account with a dot":           {"analyzer.analysis.recordStore.azureBlob.account=st.evil.example"},
		"account in upper case":        {"analyzer.analysis.recordStore.azureBlob.account=StAnalyses"},
		"container with a slash":       {"analyzer.analysis.recordStore.azureBlob.container=a/b"},
		"container with two hyphens":   {"analyzer.analysis.recordStore.azureBlob.container=a--b"},
		"container of two letters":     {"analyzer.analysis.recordStore.azureBlob.container=ab"},
		"record store with a key":      {"analyzer.analysis.recordStore.azureBlob.accountKey=x"},
		"record store of another kind": {"analyzer.analysis.recordStore.s3.bucket=x"},
	} {
		t.Run(name, func(t *testing.T) { helmFails(t, set...) })
	}
	for _, flag := range []string{"triage-model", "escalation-model", "model-endpoint-suffixes", "action-policies",
		"analysis-store-account", "analysis-store-container"} {
		if out := helmFails(t, "analyzer.extraArgs={--"+flag+"=x}"); !strings.Contains(out, "--"+flag) {
			t.Errorf("extra arg %s not refused:\n%s", flag, out)
		}
	}
}

// moduleRoles are the ClusterRoles of the analyzer's reads of an add-on's kinds (#409), by the value that renders
// each, with the rules config/rbac/analyzer/modules generates, and the config/modules directory that installs
// it when it is off by default ("" for KEDA, which config/default and config/watch install).
var moduleRoles = []struct {
	value, role, dir string
	rules            []string
	clusterWide      bool
}{
	{"analyzer.modules.keda.enabled", "shadewright-analyzer-watch-keda", "",
		[]string{"keda.sh/scaledjobs:list,watch", "keda.sh/scaledobjects:get,list,watch"}, false},
	{"analyzer.modules.flux.enabled", "shadewright-analyzer-watch-flux", "config/modules/flux",
		[]string{"helm.toolkit.fluxcd.io/helmreleases:list,watch",
			"kustomize.toolkit.fluxcd.io/kustomizations:list,watch",
			"source.toolkit.fluxcd.io/gitrepositories:list,watch"}, false},
	{"analyzer.modules.kyverno.enabled", "shadewright-analyzer-watch-kyverno", "config/modules/kyverno",
		[]string{"kyverno.io/policies:list,watch", "wgpolicyk8s.io/policyreports:list,watch"}, false},
	{"analyzer.modules.kyverno.clusterWide.enabled", "shadewright-analyzer-kyverno-cluster",
		"config/modules/kyverno-cluster",
		[]string{"kyverno.io/clusterpolicies:list,watch", "wgpolicyk8s.io/clusterpolicyreports:list,watch"}, true},
}

// ruleLines is each rule of a rendered ClusterRole as group/resource:verbs, one line per resource, sorted.
func ruleLines(t *testing.T, u *unstructured.Unstructured) []string {
	t.Helper()
	var cr rbacv1.ClusterRole
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &cr); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, rule := range cr.Rules {
		for _, g := range rule.APIGroups {
			for _, res := range rule.Resources {
				out = append(out, g+"/"+res+":"+strings.Join(rule.Verbs, ","))
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestModuleGrantsFlagFollowsTheValues (#409 review): the analyzer is told which module roles the chart rendered,
// always, so a kind whose role is left out on purpose (Kyverno's cluster-wide one, off by default) is not
// reported as missing.
func TestModuleGrantsFlagFollowsTheValues(t *testing.T) {
	for _, tc := range []struct {
		set  []string
		want string
	}{
		{nil, "--module-grants=keda"},
		{[]string{"analyzer.modules.keda.enabled=false"}, "--module-grants="},
		{[]string{kyvernoOn}, "--module-grants=keda,kyverno"},
		{[]string{"analyzer.modules.flux.enabled=true", kyvernoOn,
			"analyzer.modules.kyverno.clusterWide.enabled=true"}, "--module-grants=keda,flux,kyverno,kyverno-cluster"},
	} {
		args := analyzerArgs(t, render(t, false, tc.set...))
		var got []string
		for _, a := range args {
			if strings.HasPrefix(a, "--module-grants") {
				got = append(got, a)
			}
		}
		if !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%v: args %v, want %s once", tc.set, got, tc.want)
		}
	}
}

// TestModuleGrantsFollowTheirValues checks each module's read grant: rendered with exactly its rules and bound
// in each watched namespace (Kyverno's cluster-scoped kinds by a ClusterRoleBinding) only while its value is
// true, and gone, binding included, while it is false. KEDA is on by default, the others off. The watch role
// itself names no add-on kind.
func TestModuleGrantsFollowTheirValues(t *testing.T) {
	ns := "analyzer.watchNamespaces={demo,team-b}"
	off := render(t, false, ns, "analyzer.modules.keda.enabled=false")
	on := render(t, false, ns, "analyzer.modules.flux.enabled=true", kyvernoOn,
		"analyzer.modules.kyverno.clusterWide.enabled=true")
	defaults := render(t, false, ns)
	for _, m := range moduleRoles {
		t.Run(m.value, func(t *testing.T) {
			roleKey := "ClusterRole//" + m.role
			bindings := []string{"RoleBinding/demo/" + m.role, "RoleBinding/team-b/" + m.role}
			if m.clusterWide {
				bindings = []string{"ClusterRoleBinding//" + m.role}
			}
			if _, ok := defaults.chart[roleKey]; ok != (m.dir == "") {
				t.Errorf("default values render %s: %v, want %v", m.role, ok, m.dir == "")
			}
			for _, k := range append([]string{roleKey}, bindings...) {
				if _, ok := on.chart[k]; !ok {
					t.Errorf("%s=true does not render %s", m.value, k)
				}
				if _, ok := off.chart[k]; ok {
					t.Errorf("%s=false still renders %s", m.value, k)
				}
			}
			if u := on.chart[roleKey]; u != nil && !slices.Equal(ruleLines(t, u), m.rules) {
				t.Errorf("%s rules %v, want %v", m.role, ruleLines(t, u), m.rules)
			}
		})
	}
	for _, line := range ruleLines(t, on.chart["ClusterRole//shadewright-analyzer-watch-role"]) {
		for _, m := range moduleRoles {
			for _, r := range m.rules {
				if res, _, _ := strings.Cut(r, ":"); strings.HasPrefix(line, res+":") {
					t.Errorf("the watch role grants %s, which belongs to %s", line, m.role)
				}
			}
		}
	}
}

// policyReader is the ClusterRole of the analyzer's read of the policies its requests name (#493).
const policyReader = "ClusterRole//shadewright-analyzer-policy-reader"

// TestPolicyReaderFollowsActionPolicies checks that the analyzer may get exactly the policies of
// --action-policies, and that with no name, or no analysis, there is no rule at all.
func TestPolicyReaderFollowsActionPolicies(t *testing.T) {
	for name, set := range map[string][]string{
		"defaults":         nil,
		"policy, no model": {"analyzer.analysis.actionPolicies.SetContainerMemoryLimit=demo-memory"},
		"model, no policy": {analyzerClientID, labSuffix, triageMini},
	} {
		r := render(t, false, set...)
		for k := range r.chart {
			if strings.Contains(k, "analyzer-policy-reader") {
				t.Errorf("%s: %s rendered", name, k)
			}
		}
	}
	r := render(t, false, analyzerClientID, labSuffix, triageMini,
		"analyzer.analysis.actionPolicies.SetContainerMemoryLimit=memory-limits",
		"analyzer.analysis.actionPolicies.AdjustKedaMaxReplicas=keda")
	role := r.chart[policyReader]
	if role == nil || r.chart["ClusterRoleBinding//shadewright-analyzer-policy-reader"] == nil {
		t.Fatal("the policy reader role or its binding is missing")
	}
	got := describe(t, role)
	want := []string{"remediation.shadewright.io|remediationpolicies|get|keda",
		"remediation.shadewright.io|remediationpolicies|get|memory-limits"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules %v, want %v", got, want)
	}
}
