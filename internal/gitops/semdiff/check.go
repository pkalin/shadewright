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
package semdiff

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"reflect"
	"slices"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/repopath"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
)

// Input is one planned change: the build before it, the file it replaces and what it must change.
type Input struct {
	// Files holds the build at the commit the change builds on, by repository path, as [Load] returns it: every kustomization
	// file and every resource or patch file the path builds from. Files outside the build are ignored.
	Files map[string][]byte
	// Path is the Kustomization's spec.path, the directory rendered, relative to the repository root.
	Path string
	// File is the repository path of the manifest the patch changed; it must be a resource of the build.
	File string
	// Patched is the new content of File, as the patcher (internal/gitops/patch) returned it.
	Patched []byte
	// Target is the object the action changes, as the rendered output names it.
	Target v1alpha1.ObjectRef
	// Action is the planned action, with its previous and desired values.
	Action v1alpha1.Action
}

// Result is the evidence of a passed check, for the pull request.
type Result struct {
	// Objects is how many objects the path renders, the same before and after the change.
	Objects int
	// Field is the one field that changed, for example spec.maxReplicaCount.
	Field string
	// Previous and Desired are the rendered values of Field before and after, as JSON.
	Previous, Desired string
}

// Check renders in.Path before and after replacing in.File with in.Patched and returns nil only when the
// one difference between the two renders is in.Action's field on in.Target, changed from its previous to
// its desired value, and the changed target passes the basic schema checks. Every other outcome is an
// [*Error] with a [Reason]: another object added, removed or changed (for example through a shared base),
// another field of the target changed, the field already different before (something else in the build
// sets it), the desired value not reached (an overlay overrides it), a kustomization outside the allow-list,
// a reference kustomize could read as remote, or a build kustomize refuses. Check reads nothing outside
// in.Files and never runs a plugin or fetches a remote base.
//
// The render covers the repository path only, not the Flux Kustomization's own spec fields (patches, images,
// targetNamespace, postBuild, components). The source mapping already refuses a Kustomization that sets any
// of them, and the pull request step relies on that.
//
// Renders are serialised in the process (kustomize keeps global state). ctx bounds the wait for that turn:
// when it ends first, Check returns ctx.Err() without rendering. A render that has started runs to its end,
// since kustomize cannot be cancelled; the limits on the build ([MaxVisits], [MaxBuildBytes], [MaxBuildDocuments],
// [MaxTransformBytes]) bound how long it can take.
func Check(ctx context.Context, in Input) (Result, error) {
	field, previous, desired, err := plan(in)
	if err != nil {
		return Result{}, err
	}
	dir, _ := cleanPath(in.Path)
	file, _ := cleanPath(in.File)

	before, err := tree(ctx, in.Files, dir)
	if err != nil {
		return Result{}, err
	}
	if !before.resources[file] {
		return Result{}, refuse(ReasonFileNotInBuild, file+" is not a resource of the build of "+dir)
	}
	afterFiles := maps.Clone(before.files)
	afterFiles[file] = in.Patched
	// The patched file may be longer than the original, so the tree after the change is counted against the
	// limits on its own, with every reference that reaches the file, before it is rendered.
	if _, err := tree(ctx, afterFiles, dir); err != nil {
		return Result{}, err
	}

	objsBefore, err := render(ctx, before.files, dir)
	if err != nil {
		return Result{}, err
	}
	objsAfter, err := render(ctx, afterFiles, dir)
	if err != nil {
		return Result{}, err
	}
	tb, ta, err := compare(objsBefore, objsAfter, in.Target, field)
	if err != nil {
		return Result{}, err
	}
	got, _ := field.Get(tb)
	if !field.Comparable(got) {
		return Result{}, refuse(ReasonPreviousValueDiffers,
			"the rendered target does not hold the previous value: its value is not a quantity in the bounded form Shadewright reads")
	}
	if !field.Equals(got, previous) {
		return Result{}, refuse(ReasonPreviousValueDiffers, "the rendered target does not hold the previous value")
	}
	now, _ := field.Get(ta)
	if !field.Equals(now, desired) {
		return Result{}, refuse(ReasonDesiredValueMissing, "the rendered target does not hold the desired value")
	}
	if err := validate(ta, in.Target, in.Action); err != nil {
		return Result{}, err
	}
	return Result{Objects: len(objsAfter), Field: field.String(), Previous: jsonText(got), Desired: jsonText(now)}, nil
}

// plan validates the input and returns the field, previous and desired values of the action.
func plan(in Input) (sourcemap.Field, any, any, error) {
	field, err := sourcemap.FieldFor(in.Action, in.Target)
	if err != nil {
		return sourcemap.Field{}, nil, nil, refuse(ReasonInvalidInput, err.Error())
	}
	if in.Target.Namespace == "" || in.Target.Name == "" {
		return sourcemap.Field{}, nil, nil, refuse(ReasonInvalidInput, "the target needs a namespace and a name")
	}
	previous, ok := sourcemap.PreviousValue(in.Action)
	desired, ok2 := sourcemap.DesiredValue(in.Action)
	if !ok || !ok2 || field.Equals(previous, desired) {
		return sourcemap.Field{}, nil, nil, refuse(ReasonInvalidInput, "the action needs a previous and another desired value")
	}
	if _, ok := cleanPath(in.Path); !ok {
		return sourcemap.Field{}, nil, nil, refuse(ReasonInvalidInput, "the path is not a relative path inside the repository")
	}
	file, ok := cleanPath(in.File)
	if !ok || file == "." || !repopath.IsManifest(file) || repopath.IsKustomizationFile(file) {
		return sourcemap.Field{}, nil, nil, refuse(ReasonInvalidInput, "the changed file is not a manifest path inside the repository")
	}
	if len(in.Patched) > MaxFileBytes {
		return sourcemap.Field{}, nil, nil, refuse(ReasonTooLarge, fmt.Sprintf("the patched file is larger than %d bytes", MaxFileBytes))
	}
	return field, previous, desired, nil
}

// tree walks the build of dir in files and returns the files it reads.
func tree(ctx context.Context, files map[string][]byte, dir string) (*walker, error) {
	w := newWalker(func(_ context.Context, p string) ([]byte, bool, error) {
		data, ok := files[p]
		return data, ok, nil
	})
	// A directory resource whose name ends in .yaml, which only a generated kustomization lists (Load), is
	// known by the kustomization file Load read in it.
	w.isDir = func(p string) bool {
		return slices.ContainsFunc(repopath.KustomizationFileNames, func(n string) bool {
			_, ok := files[path.Join(p, n)]
			return ok
		})
	}
	if err := w.walk(ctx, dir, kindKustomization, 0); err != nil {
		return nil, err
	}
	if err := w.checkGrowth(); err != nil {
		return nil, err
	}
	return w, nil
}

// id is the identity of a rendered object.
type id struct {
	apiVersion, kind, namespace, name string
}

func idOf(obj map[string]any) id {
	meta, _ := obj["metadata"].(map[string]any)
	s := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	return id{s(obj, "apiVersion"), s(obj, "kind"), s(meta, "namespace"), s(meta, "name")}
}

// index maps rendered objects by identity and refuses duplicates.
func index(objs []map[string]any) (map[id]map[string]any, error) {
	m := make(map[id]map[string]any, len(objs))
	for _, o := range objs {
		k := idOf(o)
		if _, dup := m[k]; dup {
			return nil, refuse(ReasonAmbiguousTarget, "the build renders two objects with the same identity")
		}
		m[k] = o
	}
	return m, nil
}

// compare requires that before and after hold the same objects, that only the target differs, and only in
// field. It returns the target before and after.
func compare(before, after []map[string]any, target v1alpha1.ObjectRef, field sourcemap.Field) (
	map[string]any, map[string]any, error) {
	ib, err := index(before)
	if err != nil {
		return nil, nil, err
	}
	ia, err := index(after)
	if err != nil {
		return nil, nil, err
	}
	want := id{target.APIVersion, target.Kind, target.Namespace, target.Name}
	if err := findTarget(ib, want); err != nil {
		return nil, nil, err
	}
	if len(ib) != len(ia) {
		return nil, nil, refuse(ReasonObjectsAddedOrRemoved, "the change adds or removes rendered objects")
	}
	for k, b := range ib {
		a, ok := ia[k]
		switch {
		case !ok:
			return nil, nil, refuse(ReasonObjectsAddedOrRemoved, "the change adds or removes rendered objects")
		case k == want:
			continue
		case !reflect.DeepEqual(b, a):
			return nil, nil, refuse(ReasonOtherObjectChanged, "the change also alters another rendered object")
		}
	}
	tb, ta := ib[want], ia[want]
	if _, had := field.Get(tb); !had {
		return nil, nil, refuse(ReasonFieldNotFound, "the rendered target does not set "+field.String()+" before the change")
	}
	now, ok := field.Get(ta)
	if !ok {
		return nil, nil, refuse(ReasonDesiredValueMissing, "the rendered target does not set "+field.String()+" after the change")
	}
	masked, ok := withField(tb, field, now)
	if !ok || !reflect.DeepEqual(masked, ta) {
		return nil, nil, refuse(ReasonOtherFieldChanged, "the change alters more of the target than "+field.String())
	}
	return tb, ta, nil
}

// findTarget requires exactly one rendered object that is the target, and none that could also be it: the
// same group, kind and name with another version or without a namespace.
func findTarget(objs map[id]map[string]any, want id) error {
	what := fmt.Sprintf("%s %s/%s", want.kind, want.namespace, want.name)
	wantGV, _ := schema.ParseGroupVersion(want.apiVersion)
	for k := range objs {
		if k == want || k.kind != want.kind || k.name != want.name {
			continue
		}
		gv, err := schema.ParseGroupVersion(k.apiVersion)
		if err == nil && gv.Group == wantGV.Group && (k.namespace == want.namespace || k.namespace == "") {
			return refuse(ReasonAmbiguousTarget, "the build may render "+what+" more than once")
		}
	}
	if _, ok := objs[want]; !ok {
		return refuse(ReasonTargetNotFound, "the build does not render "+what)
	}
	return nil
}

// withField returns a deep copy of obj with field set to value, or false when the path is not there.
func withField(obj map[string]any, field sourcemap.Field, value any) (map[string]any, bool) {
	cp, ok := deepCopy(obj).(map[string]any)
	if !ok {
		return nil, false
	}
	var cur any = cp
	for i, s := range field.Steps {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if i == len(field.Steps)-1 {
			if _, ok := m[s.Key]; !ok {
				return nil, false
			}
			m[s.Key] = value
			return cp, true
		}
		cur = m[s.Key]
		if s.Name != "" {
			if cur = pick(cur, s.Name); cur == nil {
				return nil, false
			}
		}
	}
	return nil, false
}

// pick returns the only element of a list of maps whose name equals name, or nil.
func pick(list any, name string) any {
	items, _ := list.([]any)
	var found any
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && m["name"] == name {
			if found != nil {
				return nil
			}
			found = m
		}
	}
	return found
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = deepCopy(e)
		}
		return m
	case []any:
		l := make([]any, len(x))
		for i, e := range x {
			l[i] = deepCopy(e)
		}
		return l
	default:
		return v
	}
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
