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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errListGetter is returned for a use of [ListGetter] it does not serve.
var errListGetter = errors.New("list getter")

// ListGetter is a [client.Reader] whose Get reads one namespaced object with list, by a field selector on
// its name, from Reader (the analyzer's API reader). It lets the analyzer read one object of a kind it may
// list but not get, such as Flux's Kustomizations and HelmReleases, whose only grant is the Flux module's
// list and watch in the watched namespaces (package modulerbac/flux): no new verb, and no informer. It serves
// only *unstructured.Unstructured objects of a namespace, and its List refuses every call.
type ListGetter struct {
	Reader client.Reader
}

var _ client.Reader = ListGetter{}

// Get lists the objects of obj's kind in key's namespace whose name is key's name, at most two, and copies the
// one found into obj. None is a NotFound error that names the kind as obj gives it. A kind the API server does
// not serve (a no-match error: Flux's CRDs not installed) is an error that is neither a NotFound nor a
// no-match, since no object is known to be missing; the Reader's other errors are returned as they are, so a
// caller can classify them.
func (g ListGetter) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || g.Reader == nil {
		return fmt.Errorf("%w: only unstructured objects, with a reader", errListGetter)
	}
	gvk := u.GroupVersionKind()
	if gvk.Kind == "" || key.Namespace == "" || key.Name == "" {
		return fmt.Errorf("%w: a kind, a namespace and a name are required", errListGetter)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := g.Reader.List(ctx, list, client.InNamespace(key.Namespace),
		client.MatchingFields{"metadata.name": key.Name}, client.Limit(2)); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("%w: the API server does not serve %s", errListGetter, gvk.Kind)
		}
		return err
	}
	switch len(list.Items) {
	case 0:
		return apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, key.Name)
	case 1:
	default:
		return fmt.Errorf("%w: the list by name returned more than one object", errListGetter)
	}
	item := list.Items[0]
	if item.GetNamespace() != key.Namespace || item.GetName() != key.Name {
		return fmt.Errorf("%w: the list by name returned another object", errListGetter)
	}
	if item.GetKind() == "" {
		item.SetGroupVersionKind(gvk)
	}
	item.DeepCopyInto(u)
	return nil
}

// List refuses every call: a ListGetter only gets.
func (ListGetter) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("%w: List is not served", errListGetter)
}
