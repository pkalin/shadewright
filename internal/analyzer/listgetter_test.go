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

package analyzer

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// unitName is the name of the delivery unit read.
const unitName = "apps"

var kustomizationGVK = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}

// listOnly serves List from items, or fails with err; a Get fails the test: the analyzer may not get Flux
// objects. It records the list options of each call.
type listOnly struct {
	t     *testing.T
	items []unstructured.Unstructured
	err   error
	lists []*client.ListOptions
	kinds []string
}

func (r *listOnly) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	r.t.Error("Get was called: the analyzer holds no get on Flux objects")
	return errors.New("get")
}

func (r *listOnly) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	o := &client.ListOptions{}
	o.ApplyOptions(opts)
	r.lists = append(r.lists, o)
	r.kinds = append(r.kinds, list.GetObjectKind().GroupVersionKind().Kind)
	if r.err != nil {
		return r.err
	}
	list.(*unstructured.UnstructuredList).Items = r.items
	return nil
}

func unit(ns, name string, withKind bool) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]any{}}
	if withKind {
		u.SetGroupVersionKind(kustomizationGVK)
	}
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func target() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(kustomizationGVK)
	return u
}

// A Get is one namespaced list by a field selector on the name, never a get.
func TestListGetterGetsWithList(t *testing.T) {
	r := &listOnly{t: t, items: []unstructured.Unstructured{unit(watchedNS, unitName, false)}}
	got := target()
	if err := (ListGetter{Reader: r}).Get(t.Context(), client.ObjectKey{Namespace: watchedNS, Name: unitName}, got); err != nil {
		t.Fatal(err)
	}
	if got.GetName() != unitName || got.GetNamespace() != watchedNS || got.GroupVersionKind() != kustomizationGVK {
		t.Errorf("got %v %s/%s", got.GroupVersionKind(), got.GetNamespace(), got.GetName())
	}
	if len(r.lists) != 1 {
		t.Fatalf("%d lists", len(r.lists))
	}
	o := r.lists[0]
	if o.Namespace != watchedNS || o.FieldSelector == nil || o.FieldSelector.String() != "metadata.name=apps" ||
		o.Limit != 2 || r.kinds[0] != "KustomizationList" {
		t.Errorf("list options: ns %q, fields %v, limit %d, kind %s", o.Namespace, o.FieldSelector, o.Limit, r.kinds[0])
	}
}

func TestListGetterResults(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: kustomizationGVK.Group, Resource: "kustomizations"}, "", errors.New("no"))
	for _, tc := range []struct {
		name  string
		items []unstructured.Unstructured
		err   error
		check func(error) bool
	}{
		{"none is not found, naming the kind as asked", nil, nil, func(err error) bool {
			return apierrors.IsNotFound(err) && strings.Contains(err.Error(), "Kustomization.kustomize.toolkit.fluxcd.io")
		}},
		// Flux's CRDs not installed: the kind is not served, which is not a missing object.
		{"a kind not served is not a not found", nil, &meta.NoKindMatchError{GroupKind: kustomizationGVK.GroupKind()},
			func(err error) bool {
				return errors.Is(err, errListGetter) && !meta.IsNoMatchError(err) && !apierrors.IsNotFound(err)
			}},
		{"two are refused", []unstructured.Unstructured{unit(watchedNS, unitName, true), unit(watchedNS, unitName, true)}, nil,
			func(err error) bool { return errors.Is(err, errListGetter) }},
		{"another name is refused", []unstructured.Unstructured{unit(watchedNS, "other", true)}, nil,
			func(err error) bool { return errors.Is(err, errListGetter) }},
		{"another namespace is refused", []unstructured.Unstructured{unit("flux-system", unitName, true)}, nil,
			func(err error) bool { return errors.Is(err, errListGetter) }},
		{"the reader's error is kept", nil, forbidden, apierrors.IsForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &listOnly{t: t, items: tc.items, err: tc.err}
			err := (ListGetter{Reader: r}).Get(t.Context(), client.ObjectKey{Namespace: watchedNS, Name: unitName}, target())
			if err == nil || !tc.check(err) {
				t.Errorf("error %v", err)
			}
		})
	}
}

// Anything but a namespaced Get of an unstructured object is refused before any read.
func TestListGetterRefusesOtherUses(t *testing.T) {
	r := &listOnly{t: t}
	g := ListGetter{Reader: r}
	if err := g.Get(t.Context(), client.ObjectKey{Name: unitName}, target()); !errors.Is(err, errListGetter) {
		t.Errorf("no namespace: %v", err)
	}
	if err := g.Get(t.Context(), client.ObjectKey{Namespace: watchedNS, Name: unitName}, &corev1.Pod{}); !errors.Is(err, errListGetter) {
		t.Errorf("typed object: %v", err)
	}
	if err := g.Get(t.Context(), client.ObjectKey{Namespace: watchedNS, Name: unitName}, &unstructured.Unstructured{}); !errors.Is(err, errListGetter) {
		t.Errorf("no kind: %v", err)
	}
	if err := g.List(t.Context(), &unstructured.UnstructuredList{}); !errors.Is(err, errListGetter) {
		t.Errorf("List: %v", err)
	}
	if err := (ListGetter{}).Get(t.Context(), client.ObjectKey{Namespace: watchedNS, Name: unitName}, target()); !errors.Is(err, errListGetter) {
		t.Errorf("no reader: %v", err)
	}
	if len(r.lists) != 0 {
		t.Errorf("%d lists", len(r.lists))
	}
}
