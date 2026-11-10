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

package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/tools"
)

// fluxGroups are the API groups of the objects get_delivery_status reads: the delivery units and the
// GitRepositories of Kustomizations.
var fluxGroups = []string{"kustomize.toolkit.fluxcd.io", "helm.toolkit.fluxcd.io", "source.toolkit.fluxcd.io"}

// listReader is the analyzer's API reader as the Flux grant allows it: it lists the recorded objects it
// holds (obj, then others), matching kind, namespace and name by the list's selectors, fails a list with err
// when set, and fails the test on any get.
type listReader struct {
	t      *testing.T
	obj    *unstructured.Unstructured
	others []*unstructured.Unstructured
	err    error
	lists  int
}

func (r *listReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	r.t.Error("Get was called: the analyzer holds no get on Flux objects")
	return errors.New("get")
}

func (r *listReader) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists++
	if r.err != nil {
		return r.err
	}
	o := &client.ListOptions{}
	o.ApplyOptions(opts)
	l := list.(*unstructured.UnstructuredList)
	for _, obj := range append([]*unstructured.Unstructured{r.obj}, r.others...) {
		if o.FieldSelector != nil && o.Namespace == obj.GetNamespace() &&
			o.FieldSelector.String() == "metadata.name="+obj.GetName() &&
			l.GroupVersionKind().Kind == obj.GetKind()+"List" {
			l.Items = []unstructured.Unstructured{*obj.DeepCopy()}
		}
	}
	return nil
}

// recordedUnit reads an object recorded for the Flux module and moves it into ns, a watched namespace.
func recordedUnit(t *testing.T, name, ns string) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "modules", "flux", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	u.SetNamespace(ns)
	return u
}

func deliveryCall(u *unstructured.Unstructured) providers.ToolCall {
	args := `{"kind":"` + u.GetKind() + `","namespace":"` + u.GetNamespace() + `","name":"` + u.GetName() + `"}`
	return providers.ToolCall{ID: "1", Name: tools.NameGetDeliveryStatus, Arguments: []byte(args)}
}

// defines reports whether set defines get_delivery_status.
func defines(set *tools.Set) bool {
	return slices.ContainsFunc(set.Definitions(), func(def providers.ToolDefinition) bool {
		return def.Name == tools.NameGetDeliveryStatus
	})
}

// appliedRevision is the revision the recorded Kustomizations applied.
const appliedRevision = "cluster/kind@sha1:0e2d4c6b8a9f7e5d3c1b0a9f8e7d6c5b4a3f2e1d"

// gitSource is the recorded GitRepository of the Kustomizations, moved into the watched namespace shop, Ready
// for its current generation, tracking the branch cluster/kind and last fetched at revision.
func gitSource(t *testing.T, revision string) *unstructured.Unstructured {
	t.Helper()
	u := recordedUnit(t, "gitrepository-auth-failed.json", "shop")
	ready := []any{map[string]any{"type": "Ready", "status": "True", "reason": "Succeeded",
		"observedGeneration": int64(2), "lastTransitionTime": "2026-11-03T11:00:00Z"}}
	if err := unstructured.SetNestedSlice(u.Object, ready, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(u.Object, revision, "status", "artifact", "revision"); err != nil {
		t.Fatal(err)
	}
	return u
}

// fluxFlags are the analysis flags of an install that rendered the Flux module's grant.
func fluxFlags() analysisFlags {
	f := validFlags()
	f.moduleGrants = []string{collector.GrantFlux}
	return f
}

// deliveryResult calls get_delivery_status on u through the analyzer's read tools, with flags f and r as the
// API reader.
func deliveryResult(t *testing.T, f analysisFlags, r *listReader,
	u *unstructured.Unstructured) (*tools.Set, tools.Result) {
	t.Helper()
	d := testDeps(t)
	d.apiReader = r
	set, err := readTools(f, d)
	if err != nil {
		t.Fatal(err)
	}
	res, err := set.Call(t.Context(), deliveryCall(u))
	if err != nil {
		t.Fatal(err)
	}
	return set, res
}

// Acceptance (#562): get_delivery_status reports applied, pending and failed from the Flux adapter, reading
// with list only, the verb the Flux module's grant holds. A Kustomization that would be applied also reads
// its GitRepository, to compare revisions.
func TestDeliveryToolReadsTheFluxAdapter(t *testing.T) {
	for _, tc := range []struct {
		file, want string
		lists      int
	}{
		{"kustomization-ready.json", "delivery: applied\n", 2},
		{"kustomization-reconciling.json", "delivery: pending\n", 1},
		{"kustomization-build-failed.json", "delivery: failed\n", 1},
		{"helmrelease-upgrade-failed.json", "delivery: failed\n", 1},
	} {
		t.Run(tc.file, func(t *testing.T) {
			u := recordedUnit(t, tc.file, "shop")
			r := &listReader{t: t, obj: u, others: []*unstructured.Unstructured{gitSource(t, appliedRevision)}}
			set, res := deliveryResult(t, fluxFlags(), r, u)
			if res.Status != tools.StatusOK || !strings.Contains(res.Text, tc.want) || r.lists != tc.lists {
				t.Errorf("%q, %d lists:\n%s", res.Status, r.lists, res.Text)
			}
			missing := recordedUnit(t, tc.file, "shop")
			missing.SetName("other")
			if nf, _ := set.Call(t.Context(), deliveryCall(missing)); nf.Status != tools.StatusNotFound {
				t.Errorf("missing unit: %q", nf.Status)
			}
			// The delivery namespaces are the watched ones, where the grant is bound: no other is read.
			other, _ := set.Call(t.Context(), deliveryCall(recordedUnit(t, tc.file, "flux-system")))
			if other.Status != tools.StatusRefused || r.lists != tc.lists+1 {
				t.Errorf("unwatched namespace: %q, %d lists", other.Status, r.lists)
			}
		})
	}
}

// A suspended unit is not applied, and a Ready Kustomization that has not applied the revision its
// GitRepository last fetched is pending: the rules of the executor's source mapping and of `kubectl
// shadewright doctor` (sourcemap, fluxstatus.AppliedFetched).
func TestDeliveryToolStatesFollowTheExecutor(t *testing.T) {
	suspend := func(u *unstructured.Unstructured) *unstructured.Unstructured {
		if err := unstructured.SetNestedField(u.Object, true, "spec", "suspend"); err != nil {
			t.Fatal(err)
		}
		return u
	}
	const newer = "cluster/kind@sha1:1f3e5d7c9b0a8f6e4d2c1b0a9f8e7d6c5b4a3f21"
	for _, tc := range []struct {
		name   string
		unit   *unstructured.Unstructured
		source *unstructured.Unstructured
		want   string
	}{
		{"suspended Kustomization", suspend(recordedUnit(t, "kustomization-ready.json", "shop")),
			gitSource(t, appliedRevision), "delivery: suspended\n"},
		{"suspended HelmRelease", suspend(recordedUnit(t, "helmrelease-upgrade-failed.json", "shop")),
			nil, "delivery: suspended\n"},
		{"behind its source", recordedUnit(t, "kustomization-ready.json", "shop"),
			gitSource(t, newer), "delivery: pending\n"},
		{"at its source", recordedUnit(t, "kustomization-ready.json", "shop"),
			gitSource(t, appliedRevision), "delivery: applied\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &listReader{t: t, obj: tc.unit}
			if tc.source != nil {
				r.others = []*unstructured.Unstructured{tc.source}
			}
			if _, res := deliveryResult(t, fluxFlags(), r, tc.unit); res.Status != tools.StatusOK ||
				!strings.Contains(res.Text, tc.want) {
				t.Errorf("want %q, got %q:\n%s", tc.want, res.Status, res.Text)
			}
		})
	}
	// A GitRepository that cannot be read leaves the state unknown: could not fetch, never applied.
	u := recordedUnit(t, "kustomization-ready.json", "shop")
	if _, res := deliveryResult(t, fluxFlags(), &listReader{t: t, obj: u}, u); res.Status != tools.StatusCouldNotFetch {
		t.Errorf("source missing: %q:\n%s", res.Status, res.Text)
	}
}

// The tool turns on by the collector's rule for the Flux grant (collector.Granted): with --module-grants not
// given every grant counts, as the collector then watches Flux's kinds once it may list them. Where the
// analyzer may not list them after all, the read is "could not fetch", never a state.
func TestDeliveryToolFollowsTheCollectorGrantRule(t *testing.T) {
	u := recordedUnit(t, "kustomization-ready.json", "shop")
	f := validFlags()
	f.moduleGrants = nil
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: fluxGroups[0], Resource: "kustomizations"}, "",
		errors.New("no list"))
	r := &listReader{t: t, obj: u, err: forbidden}
	set, res := deliveryResult(t, f, r, u)
	if !defines(set) {
		t.Fatal("--module-grants not given: get_delivery_status is not defined, but the collector watches Flux")
	}
	if res.Status != tools.StatusCouldNotFetch || strings.Contains(res.Text, "delivery:") || r.lists != 1 {
		t.Errorf("forbidden list: %q, %d lists:\n%s", res.Status, r.lists, res.Text)
	}
	// Flux's CRDs not installed: the kind is not served, which the tool never reports as a missing object.
	r = &listReader{t: t, obj: u, err: &meta.NoKindMatchError{GroupKind: u.GroupVersionKind().GroupKind()}}
	if _, res := deliveryResult(t, f, r, u); res.Status != tools.StatusCouldNotFetch {
		t.Errorf("kind not served: %q:\n%s", res.Status, res.Text)
	}
}

// Without the Flux module's grant (--module-grants given without flux) or watched namespaces the tool has no
// delivery system: it is not available and reads nothing.
func TestDeliveryToolNeedsTheFluxGrant(t *testing.T) {
	u := recordedUnit(t, "kustomization-ready.json", "shop")
	noFlux := validFlags()
	noFlux.moduleGrants = []string{collector.GrantKEDA}
	none := validFlags()
	none.moduleGrants = []string{}
	noWatched := fluxFlags()
	noWatched.watched = nil
	for _, f := range []analysisFlags{noFlux, none, noWatched} {
		r := &listReader{t: t, obj: u}
		d := testDeps(t)
		d.apiReader = r
		set, err := readTools(f, d)
		if err != nil {
			t.Fatal(err)
		}
		if defines(set) {
			t.Errorf("grants %v, watched %v: get_delivery_status is defined", f.moduleGrants, f.watched)
		}
		if res, _ := set.Call(t.Context(), deliveryCall(u)); res.Status == tools.StatusOK || r.lists != 0 {
			t.Errorf("grants %v, watched %v: %q, %d lists", f.moduleGrants, f.watched, res.Status, r.lists)
		}
	}
}

// Acceptance (#562): no new grant. Every analyzer rule on the delivery units' groups holds list and watch
// only, never get: the tool reads with list (analyzer.ListGetter). The chart renders the same rules
// (test/chart TestModuleGrantsFollowTheirValues).
func TestAnalyzerHoldsNoGetOnDeliveryUnits(t *testing.T) {
	root, err := os.OpenRoot(filepath.Join("..", "..", "config", "rbac", "analyzer"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	seen := 0
	err = fs.WalkDir(root.FS(), ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || filepath.Ext(path) != ".yaml" {
			return err
		}
		b, err := fs.ReadFile(root.FS(), path)
		if err != nil {
			return err
		}
		for doc := range bytes.SplitSeq(b, []byte("\n---")) {
			var role struct {
				Kind  string              `json:"kind"`
				Rules []rbacv1.PolicyRule `json:"rules"`
			}
			if err := yaml.Unmarshal(doc, &role); err != nil {
				return err
			}
			for _, rule := range role.Rules {
				if !slices.ContainsFunc(rule.APIGroups, func(g string) bool { return slices.Contains(fluxGroups, g) || g == "*" }) {
					continue
				}
				seen++
				for _, v := range rule.Verbs {
					if v != "list" && v != "watch" {
						t.Errorf("%s: %s grants %q on %v", path, role.Kind, v, rule.Resources)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Error("no analyzer rule on Flux's delivery units: the Flux module's grant moved")
	}
}

// Chart templates write a rule as (dict "group" G "resources" (list ...) "verbs" V), V a list or a variable
// set with {{- $name := list ... }} (analyzer-module-roles.yaml).
var (
	chartRule = regexp.MustCompile(
		`\(dict "group" "([^"]+)" "resources" \(list [^)]*\) "verbs" (\$\w+|\(list [^)]*\))\)`)
	chartListVar = regexp.MustCompile(`\{\{-? (\$\w+) := (\(?list [^)}]*\)?) -?\}\}`)
	chartQuoted  = regexp.MustCompile(`"([^"]*)"`)
)

// The same pin on the chart: every rule of the analyzer's templates on the delivery units' groups holds list
// and watch only. The templates are read as text, so the check needs no helm: each mention of such a group
// must be a rule written as chartRule matches, and every verb of it list or watch. The executor's own roles
// (executor-*.yaml) and the CRDs are not the analyzer's.
func TestChartGivesTheAnalyzerNoGetOnDeliveryUnits(t *testing.T) {
	root, err := os.OpenRoot(filepath.Join("..", "..", "dist", "chart", "templates"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	seen := 0
	err = fs.WalkDir(root.FS(), ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if e.Name() == "crd" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(e.Name(), "executor") {
			return nil
		}
		b, err := fs.ReadFile(root.FS(), path)
		if err != nil {
			return err
		}
		text := string(b)
		mentions := 0
		for _, g := range fluxGroups {
			mentions += strings.Count(text, `"`+g+`"`) + strings.Count(text, "- "+g+"\n")
		}
		if mentions == 0 {
			return nil
		}
		vars := map[string]string{}
		for _, m := range chartListVar.FindAllStringSubmatch(text, -1) {
			vars[m[1]] = m[2]
		}
		rules := 0
		for _, m := range chartRule.FindAllStringSubmatch(text, -1) {
			if !slices.Contains(fluxGroups, m[1]) {
				continue
			}
			rules++
			verbs := m[2]
			if strings.HasPrefix(verbs, "$") {
				v, ok := vars[verbs]
				if !ok {
					t.Errorf("%s: the verbs of a rule on %s are %s, which is not a list variable of the file", path, m[1], verbs)
					continue
				}
				verbs = v
			}
			for _, q := range chartQuoted.FindAllStringSubmatch(verbs, -1) {
				if q[1] != "list" && q[1] != "watch" {
					t.Errorf("%s: a rule on %s grants %q: %s", path, m[1], q[1], m[0])
				}
			}
		}
		if rules != mentions {
			t.Errorf("%s: %d mentions of Flux's delivery groups, %d rules read: write each as (dict \"group\" ...)",
				path, mentions, rules)
		}
		seen += rules
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Error("no chart rule on Flux's delivery units: the Flux module's grant moved")
	}
}
