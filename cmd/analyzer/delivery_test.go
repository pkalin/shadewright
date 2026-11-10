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
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/tools"
)

// fluxGroups are the API groups of the delivery units get_delivery_status reads.
var fluxGroups = []string{"kustomize.toolkit.fluxcd.io", "helm.toolkit.fluxcd.io"}

// listReader is the analyzer's API reader as the Flux grant allows it: it lists the one recorded object it
// holds, matching namespace and name by the list's selectors, and fails the test on any get.
type listReader struct {
	t     *testing.T
	obj   *unstructured.Unstructured
	lists int
}

func (r *listReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	r.t.Error("Get was called: the analyzer holds no get on Flux objects")
	return errors.New("get")
}

func (r *listReader) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists++
	o := &client.ListOptions{}
	o.ApplyOptions(opts)
	l := list.(*unstructured.UnstructuredList)
	if o.FieldSelector != nil && o.Namespace == r.obj.GetNamespace() &&
		o.FieldSelector.String() == "metadata.name="+r.obj.GetName() &&
		l.GroupVersionKind().Kind == r.obj.GetKind()+"List" {
		l.Items = []unstructured.Unstructured{*r.obj.DeepCopy()}
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

// Acceptance (#562): get_delivery_status reports applied, pending and failed from the Flux adapter, reading
// with list only, the verb the Flux module's grant holds.
func TestDeliveryToolReadsTheFluxAdapter(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"kustomization-ready.json", "delivery: applied\n"},
		{"kustomization-reconciling.json", "delivery: pending\n"},
		{"kustomization-build-failed.json", "delivery: failed\n"},
		{"helmrelease-upgrade-failed.json", "delivery: failed\n"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			u := recordedUnit(t, tc.file, "shop")
			r := &listReader{t: t, obj: u}
			f, d := validFlags(), testDeps(t)
			f.fluxGrant, d.apiReader = true, r
			set, err := readTools(f, d)
			if err != nil {
				t.Fatal(err)
			}
			res, err := set.Call(t.Context(), deliveryCall(u))
			if err != nil || res.Status != tools.StatusOK || !strings.Contains(res.Text, tc.want) || r.lists != 1 {
				t.Errorf("%v %q, %d lists:\n%s", err, res.Status, r.lists, res.Text)
			}
			missing := recordedUnit(t, tc.file, "shop")
			missing.SetName("other")
			if nf, _ := set.Call(t.Context(), deliveryCall(missing)); nf.Status != tools.StatusNotFound {
				t.Errorf("missing unit: %q", nf.Status)
			}
			// The delivery namespaces are the watched ones, where the grant is bound: no other is read.
			other, _ := set.Call(t.Context(), deliveryCall(recordedUnit(t, tc.file, "flux-system")))
			if other.Status != tools.StatusRefused || r.lists != 2 {
				t.Errorf("unwatched namespace: %q, %d lists", other.Status, r.lists)
			}
		})
	}
}

// Without the Flux module's grant (--module-grants without flux, or not given) the tool has no delivery
// system: it is not available and reads nothing.
func TestDeliveryToolNeedsTheFluxGrant(t *testing.T) {
	u := recordedUnit(t, "kustomization-ready.json", "shop")
	noWatched := validFlags()
	noWatched.fluxGrant, noWatched.watched = true, nil
	for _, f := range []analysisFlags{validFlags(), noWatched} {
		r := &listReader{t: t, obj: u}
		d := testDeps(t)
		d.apiReader = r
		set, err := readTools(f, d)
		if err != nil {
			t.Fatal(err)
		}
		if defines(set) {
			t.Errorf("fluxGrant %v, watched %v: get_delivery_status is defined", f.fluxGrant, f.watched)
		}
		if res, _ := set.Call(t.Context(), deliveryCall(u)); res.Status == tools.StatusOK || r.lists != 0 {
			t.Errorf("fluxGrant %v, watched %v: %q, %d lists", f.fluxGrant, f.watched, res.Status, r.lists)
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
