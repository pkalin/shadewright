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

package readclient

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

const (
	testNS  = "shadewright-system"
	testWeb = "web"
)

var (
	configMaps  = corev1.SchemeGroupVersion.WithResource("configmaps")
	deployments = appsv1.SchemeGroupVersion.WithResource("deployments")
	incidents   = remediationv1alpha1.GroupVersion.WithResource("incidents")
	settings    = remediationv1alpha1.GroupVersion.WithResource("remediationsettings")
)

// fakes returns a Client on client-go's fakes, the dynamic one holding objs, with their dynamic Fake for
// reactors.
func fakes(t *testing.T, objs ...*unstructured.Unstructured) (*Client, *dynamicfake.FakeDynamicClient,
	*metadatafake.FakeMetadataClient) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{configMaps: "ConfigMapList", deployments: "DeploymentList",
			incidents: "IncidentList", settings: "RemediationSettingsList"})
	for _, o := range objs {
		gvr, ok := Resource(o.GroupVersionKind())
		if !ok {
			t.Fatalf("no resource for %s", o.GroupVersionKind())
		}
		if err := dyn.Tracker().Create(gvr, o, o.GetNamespace()); err != nil {
			t.Fatal(err)
		}
	}
	md := metadatafake.NewSimpleMetadataClient(metadatafake.NewTestScheme())
	c, err := NewForClients(dyn, md)
	if err != nil {
		t.Fatal(err)
	}
	return c, dyn, md
}

func obj(gvk schema.GroupVersionKind, ns, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: fields}
	if u.Object == nil {
		u.Object = map[string]any{}
	}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

// TestGetConvertsTypedObjects reads a cluster-scoped and a namespaced object into typed objects: the fields
// arrive and the kind is left empty, as a typed object decoded by controller-runtime was.
func TestGetConvertsTypedObjects(t *testing.T) {
	c, _, _ := fakes(t,
		obj(remediationv1alpha1.GroupVersion.WithKind("RemediationSettings"), "", remediationv1alpha1.SettingsName,
			map[string]any{"spec": map[string]any{"paused": true}}),
		obj(corev1.SchemeGroupVersion.WithKind("ConfigMap"), testNS, "budget",
			map[string]any{"data": map[string]any{"day": "2026-11-06"}}))
	s := &remediationv1alpha1.RemediationSettings{}
	if err := c.Get(context.Background(), ObjectKey{Name: remediationv1alpha1.SettingsName}, s); err != nil {
		t.Fatal(err)
	}
	if s.Spec.Paused == nil || !*s.Spec.Paused || s.Kind != "" || s.APIVersion != "" {
		t.Errorf("settings %+v", s)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), ObjectKey{Namespace: testNS, Name: "budget"}, cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["day"] != "2026-11-06" || cm.Namespace != testNS {
		t.Errorf("ConfigMap %+v", cm)
	}
}

// TestListSendsOptions checks that the namespace, labels, limit and continue token reach the API server and
// that a typed list keeps the continue token the server returned.
func TestListSendsOptions(t *testing.T) {
	c, dyn, _ := fakes(t)
	var got k8stesting.ListActionImpl
	dyn.PrependReactor("list", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		got = a.(k8stesting.ListActionImpl)
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("DeploymentList"))
		list.SetContinue("next")
		dep := obj(appsv1.SchemeGroupVersion.WithKind("Deployment"), testNS, "shadewright-analyzer", nil)
		dep.SetLabels(map[string]string{"control-plane": "analyzer"})
		list.Items = []unstructured.Unstructured{*dep}
		return true, list, nil
	})
	list := &appsv1.DeploymentList{}
	err := c.List(context.Background(), list, InNamespace(testNS), MatchingLabels{"control-plane": "analyzer"},
		Limit(500), Continue("tok"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace != testNS || got.ListOptions.LabelSelector != "control-plane=analyzer" ||
		got.ListOptions.Limit != 500 || got.ListOptions.Continue != "tok" {
		t.Errorf("sent %+v", got)
	}
	if list.Continue != "next" || len(list.Items) != 1 || list.Items[0].Name != "shadewright-analyzer" {
		t.Errorf("list %+v", list)
	}
}

// TestErrors checks the errors a read returns: a missing object is NotFound, a kind the API server does not
// serve is a NoKindMatchError on get and list, and any other error comes back unchanged.
func TestErrors(t *testing.T) {
	ctx := context.Background()
	unserved := apierrors.NewNotFound(schema.GroupResource{}, "")
	forbidden := apierrors.NewForbidden(incidents.GroupResource(), "", errors.New("no"))
	for name, tc := range map[string]struct {
		react func(*dynamicfake.FakeDynamicClient)
		read  func(*Client) error
		check func(error) bool
	}{
		"missing object": {
			read:  func(c *Client) error { return c.Get(ctx, ObjectKey{Namespace: testNS, Name: "x"}, &corev1.ConfigMap{}) },
			check: func(err error) bool { return apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) },
		},
		"get of an unserved kind": {
			react: func(d *dynamicfake.FakeDynamicClient) {
				d.PrependReactor("get", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, unserved
				})
			},
			read: func(c *Client) error {
				return c.Get(ctx, ObjectKey{Namespace: testNS, Name: "x"}, &remediationv1alpha1.Incident{})
			},
			check: meta.IsNoMatchError,
		},
		"list of an unserved kind": {
			react: func(d *dynamicfake.FakeDynamicClient) {
				d.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewNotFound(incidents.GroupResource(), "")
				})
			},
			read:  func(c *Client) error { return c.List(ctx, &remediationv1alpha1.IncidentList{}) },
			check: meta.IsNoMatchError,
		},
		"forbidden list": {
			react: func(d *dynamicfake.FakeDynamicClient) {
				d.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, forbidden
				})
			},
			read:  func(c *Client) error { return c.List(ctx, &remediationv1alpha1.IncidentList{}) },
			check: func(err error) bool { return errors.Is(err, forbidden) },
		},
		"cancelled read": {
			react: func(d *dynamicfake.FakeDynamicClient) {
				d.PrependReactor("get", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, context.Canceled
				})
			},
			read: func(c *Client) error {
				return c.Get(ctx, ObjectKey{Name: remediationv1alpha1.SettingsName}, &remediationv1alpha1.RemediationSettings{})
			},
			check: func(err error) bool { return errors.Is(err, context.Canceled) },
		},
		"kind the plugin does not read": {
			read:  func(c *Client) error { return c.List(ctx, &corev1.SecretList{}) },
			check: func(err error) bool { return err != nil && !apierrors.IsNotFound(err) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, dyn, _ := fakes(t)
			if tc.react != nil {
				tc.react(dyn)
			}
			if err := tc.read(c); !tc.check(err) {
				t.Errorf("err = %v (%T)", err, err)
			}
		})
	}
}

// TestMetadataOnly reads a metadata-only object and list through the metadata client, never the dynamic one.
func TestMetadataOnly(t *testing.T) {
	c, dyn, md := fakes(t)
	m := &metav1.PartialObjectMetadata{}
	m.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
	m.SetName(testWeb)
	m.SetNamespace(testNS)
	m.SetLabels(map[string]string{"app": testWeb})
	if err := md.Tracker().Create(deployments, m, testNS); err != nil {
		t.Fatal(err)
	}
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("DeploymentList"))
	if err := c.List(context.Background(), list, InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Labels["app"] != testWeb {
		t.Errorf("list %+v", list)
	}
	got := &metav1.PartialObjectMetadata{}
	got.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
	if err := c.Get(context.Background(), ObjectKey{Namespace: testNS, Name: testWeb}, got); err != nil {
		t.Fatal(err)
	}
	if got.Name != testWeb || got.GroupVersionKind().Kind != "Deployment" {
		t.Errorf("get %+v", got)
	}
	if n := len(dyn.Actions()); n != 0 {
		t.Errorf("%d calls to the dynamic client", n)
	}
}
