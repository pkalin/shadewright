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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/cmd/kubectl-shadewright/internal/readclient"
	"github.com/pkalin/shadewright/internal/fluxstatus"
)

// testKinds are the kinds the fake cluster serves: every kind the plugin reads.
var testKinds = []schema.GroupVersionKind{
	corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	appsv1.SchemeGroupVersion.WithKind("Deployment"),
	appsv1.SchemeGroupVersion.WithKind("StatefulSet"),
	apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"),
	remediationv1alpha1.GroupVersion.WithKind("Incident"),
	remediationv1alpha1.GroupVersion.WithKind("RemediationRequest"),
	remediationv1alpha1.GroupVersion.WithKind(kindSettings),
	{Group: fluxstatus.SourceGroup, Version: fluxstatus.SourceVersion, Kind: fluxstatus.KindGitRepository},
	{Group: fluxstatus.KustomizeGroup, Version: fluxstatus.KustomizeVersion, Kind: fluxstatus.KindKustomization},
}

// reactors stand in for the interceptors of a fake client: each is prepended to client-go's dynamic and
// metadata fakes, and a call goes on to the objects of the fake unless the reactor reports it handled.
// metadataOnly tells a metadata-only read (the metadata client) from a read of whole objects.
type reactors struct {
	get  func(a k8stesting.GetAction, metadataOnly bool) (handled bool, ret runtime.Object, err error)
	list func(a k8stesting.ListAction, metadataOnly bool) (handled bool, ret runtime.Object, err error)
}

// testScheme is the plugin's own scheme, which the tests use to give typed objects their kind.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s, err := readclient.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeReader returns the plugin's client on client-go's dynamic and metadata fakes, which hold objs (whole
// objects in the dynamic fake, their metadata in the metadata fake) and run r before their own reactions.
func fakeReader(t *testing.T, objs []readclient.Object, r *reactors) readclient.Reader {
	t.Helper()
	listKinds := map[schema.GroupVersionResource]string{}
	for _, gvk := range testKinds {
		gvr, ok := readclient.Resource(gvk)
		if !ok {
			t.Fatalf("the plugin does not read %s", gvk)
		}
		listKinds[gvr] = gvk.Kind + "List"
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	md := metadatafake.NewSimpleMetadataClient(metadatafake.NewTestScheme())
	s := testScheme(t)
	for _, o := range objs {
		u := &unstructured.Unstructured{}
		if in, ok := o.(*unstructured.Unstructured); ok {
			u = in.DeepCopy()
		} else {
			content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
			if err != nil {
				t.Fatal(err)
			}
			u.Object = content
			gvks, _, err := s.ObjectKinds(o)
			if err != nil {
				t.Fatal(err)
			}
			u.SetGroupVersionKind(gvks[0])
		}
		gvr, ok := readclient.Resource(u.GroupVersionKind())
		if !ok {
			t.Fatalf("the plugin does not read %s", u.GroupVersionKind())
		}
		if err := dyn.Tracker().Create(gvr, u, u.GetNamespace()); err != nil {
			t.Fatal(err)
		}
		m := &metav1.PartialObjectMetadata{}
		m.SetGroupVersionKind(u.GroupVersionKind())
		m.ObjectMeta = metav1.ObjectMeta{Name: u.GetName(), Namespace: u.GetNamespace(), Labels: u.GetLabels(),
			Annotations: u.GetAnnotations(), UID: u.GetUID(), Generation: u.GetGeneration()}
		if err := md.Tracker().Create(gvr, m, u.GetNamespace()); err != nil {
			t.Fatal(err)
		}
	}
	if r != nil {
		for fake, metadataOnly := range map[*k8stesting.Fake]bool{&dyn.Fake: false, &md.Fake: true} {
			if r.get != nil {
				fake.PrependReactor("get", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
					return r.get(a.(k8stesting.GetAction), metadataOnly)
				})
			}
			if r.list != nil {
				fake.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
					return r.list(a.(k8stesting.ListAction), metadataOnly)
				})
			}
		}
	}
	c, err := readclient.NewForClients(dyn, md)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// actionKind returns the kind whose resource the action reads, or "" for one the fake does not serve.
func actionKind(a k8stesting.Action) string {
	for _, gvk := range testKinds {
		if gvr, _ := readclient.Resource(gvk); gvr == a.GetResource() {
			return gvk.Kind
		}
	}
	return ""
}

// actionKey is the namespace and name a get reads.
func actionKey(a k8stesting.GetAction) readclient.ObjectKey {
	return readclient.ObjectKey{Namespace: a.GetNamespace(), Name: a.GetName()}
}

// listFails returns reactors that fail with err each list of whole objects of kind, counting them in calls when
// it is not nil, and pass every other call on.
func listFails(kind string, calls *int, fail func(call int) error) *reactors {
	return &reactors{list: func(a k8stesting.ListAction, metadataOnly bool) (bool, runtime.Object, error) {
		if metadataOnly || actionKind(a) != kind {
			return false, nil, nil
		}
		*calls++
		if err := fail(*calls); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	}}
}
