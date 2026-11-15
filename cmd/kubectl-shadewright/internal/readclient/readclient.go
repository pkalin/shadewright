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

// Package readclient is the read-only Kubernetes client of kubectl-shadewright. Its [Reader] has get and list
// and no other method, so the plugin cannot write to the cluster through it. It is built on client-go's dynamic
// and metadata clients and converts what they return into the typed objects the plugin reads, for a fixed set
// of kinds ([Scheme] and the resource table below), so the plugin links neither controller-runtime nor the
// typed clientset and its informers.
//
// It answers as controller-runtime's client did, which the plugin used before: the same errors from the API
// server, unwrapped, and a [meta.NoKindMatchError] when the API server does not serve the kind (a 404 on a
// list, or a 404 on a get that names no object).
package readclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/fluxstatus"
)

// ObjectKey names an object: its namespace (empty for a cluster-scoped object) and name.
type ObjectKey = types.NamespacedName

// Object is a Kubernetes object: a typed object of [Scheme], an [unstructured.Unstructured] or a
// [metav1.PartialObjectMetadata], the last two with their kind set.
type Object interface {
	metav1.Object
	runtime.Object
}

// ObjectList is a list of Kubernetes objects: a typed list of [Scheme], an [unstructured.UnstructuredList] or a
// [metav1.PartialObjectMetadataList], the last two with their list kind set.
type ObjectList interface {
	metav1.ListInterface
	runtime.Object
}

// Reader gets and lists objects. It has no method that writes.
type Reader interface {
	// Get reads the object key names into obj.
	Get(ctx context.Context, key ObjectKey, obj Object) error
	// List reads the objects that opts select into list.
	List(ctx context.Context, list ObjectList, opts ...ListOption) error
}

// ListOption narrows a list.
type ListOption interface {
	apply(*listOptions)
}

// listOptions are the namespace and the list options a list sends.
type listOptions struct {
	namespace string
	metav1.ListOptions
}

// InNamespace lists the objects of one namespace; without it a list reads all namespaces.
type InNamespace string

func (n InNamespace) apply(o *listOptions) { o.namespace = string(n) }

// MatchingLabels lists only the objects that carry all of these labels.
type MatchingLabels map[string]string

func (m MatchingLabels) apply(o *listOptions) {
	o.LabelSelector = labels.SelectorFromSet(labels.Set(m)).String()
}

// Limit asks the API server for at most this many objects; the rest come with the continue token.
type Limit int64

func (l Limit) apply(o *listOptions) { o.Limit = int64(l) }

// Continue asks for the page after the one that returned this token.
type Continue string

func (c Continue) apply(o *listOptions) { o.Continue = string(c) }

// resources maps each kind the plugin reads to its resource name. A kind outside it is an error: the plugin
// reads only these, and a guessed plural could name another resource.
var resources = map[schema.GroupKind]string{
	{Group: "", Kind: "ConfigMap"}:                                               "configmaps",
	{Group: appsv1.GroupName, Kind: "Deployment"}:                                "deployments",
	{Group: appsv1.GroupName, Kind: "StatefulSet"}:                               "statefulsets",
	{Group: apiextensionsv1.GroupName, Kind: "CustomResourceDefinition"}:         "customresourcedefinitions",
	{Group: remediationv1alpha1.GroupVersion.Group, Kind: "Incident"}:            "incidents",
	{Group: remediationv1alpha1.GroupVersion.Group, Kind: "RemediationRequest"}:  "remediationrequests",
	{Group: remediationv1alpha1.GroupVersion.Group, Kind: "RemediationSettings"}: "remediationsettings",
	{Group: fluxstatus.SourceGroup, Kind: fluxstatus.KindGitRepository}:          "gitrepositories",
	{Group: fluxstatus.KustomizeGroup, Kind: fluxstatus.KindKustomization}:       "kustomizations",
}

// Resource returns the resource of a kind the plugin reads, or false for any other kind.
func Resource(gvk schema.GroupVersionKind) (schema.GroupVersionResource, bool) {
	r, ok := resources[gvk.GroupKind()]
	return gvk.GroupVersion().WithResource(r), ok
}

// Scheme returns a scheme of only the API groups the plugin reads as typed objects: core and apps (v1),
// apiextensions (v1) and Shadewright's own.
func Scheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme,
		apiextensionsv1.AddToScheme, remediationv1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Client is a [Reader] on client-go's dynamic and metadata clients.
type Client struct {
	scheme   *runtime.Scheme
	dynamic  dynamic.Interface
	metadata metadata.Interface
}

// New returns a Client for cfg. The dynamic and the metadata client share one HTTP client.
func New(cfg *rest.Config) (*Client, error) {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	md, err := metadata.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	return NewForClients(dyn, md)
}

// NewForClients returns a Client on the given dynamic and metadata clients, such as client-go's fakes.
func NewForClients(dyn dynamic.Interface, md metadata.Interface) (*Client, error) {
	s, err := Scheme()
	if err != nil {
		return nil, err
	}
	return &Client{scheme: s, dynamic: dyn, metadata: md}, nil
}

// kind returns the kind of obj: the one set on an unstructured or metadata-only object, else the one the
// scheme knows for its Go type.
func (c *Client) kind(obj runtime.Object) (schema.GroupVersionKind, error) {
	switch obj.(type) {
	case *unstructured.Unstructured, *unstructured.UnstructuredList,
		*metav1.PartialObjectMetadata, *metav1.PartialObjectMetadataList:
		gvk := obj.GetObjectKind().GroupVersionKind()
		if gvk.Kind == "" {
			return gvk, fmt.Errorf("readclient: %T has no kind set", obj)
		}
		return gvk, nil
	}
	gvks, _, err := c.scheme.ObjectKinds(obj)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("readclient: %w", err)
	}
	return gvks[0], nil
}

// resource returns the resource of gvk, a kind or (list true) a list kind.
func resource(gvk schema.GroupVersionKind, list bool) (schema.GroupVersionResource, schema.GroupVersionKind, error) {
	if list {
		if !strings.HasSuffix(gvk.Kind, "List") {
			return schema.GroupVersionResource{}, gvk, fmt.Errorf("readclient: %s is not a list kind", gvk)
		}
		gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
	}
	gvr, ok := Resource(gvk)
	if !ok {
		return gvr, gvk, fmt.Errorf("readclient: the plugin does not read %s", gvk)
	}
	return gvr, gvk, nil
}

// Get reads the object key names into obj. A typed object is converted from what the API server returned and
// keeps no kind, as a typed object decoded by controller-runtime did not.
func (c *Client) Get(ctx context.Context, key ObjectKey, obj Object) error {
	gvk, err := c.kind(obj)
	if err != nil {
		return err
	}
	gvr, gvk, err := resource(gvk, false)
	if err != nil {
		return err
	}
	if m, ok := obj.(*metav1.PartialObjectMetadata); ok {
		got, err := c.metadata.Resource(gvr).Namespace(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
		if err != nil {
			return getError(err, gvk)
		}
		*m = *got
		m.SetGroupVersionKind(gvk)
		return nil
	}
	got, err := c.dynamic.Resource(gvr).Namespace(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		return getError(err, gvk)
	}
	if u, ok := obj.(*unstructured.Unstructured); ok {
		u.Object = got.Object
		return nil
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got.Object, obj); err != nil {
		return fmt.Errorf("readclient: decoding %s %s: %w", gvk.Kind, key, err)
	}
	obj.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
	return nil
}

// List reads the objects opts select into list. A typed list is converted from what the API server returned
// and keeps no kind.
func (c *Client) List(ctx context.Context, list ObjectList, opts ...ListOption) error {
	listGVK, err := c.kind(list)
	if err != nil {
		return err
	}
	gvr, gvk, err := resource(listGVK, true)
	if err != nil {
		return err
	}
	var o listOptions
	for _, opt := range opts {
		opt.apply(&o)
	}
	if m, ok := list.(*metav1.PartialObjectMetadataList); ok {
		got, err := c.metadata.Resource(gvr).Namespace(o.namespace).List(ctx, o.ListOptions)
		if err != nil {
			return listError(err, gvk)
		}
		*m = *got
		m.SetGroupVersionKind(listGVK)
		return nil
	}
	got, err := c.dynamic.Resource(gvr).Namespace(o.namespace).List(ctx, o.ListOptions)
	if err != nil {
		return listError(err, gvk)
	}
	if u, ok := list.(*unstructured.UnstructuredList); ok {
		*u = *got
		return nil
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got.UnstructuredContent(), list); err != nil {
		return fmt.Errorf("readclient: decoding %s: %w", listGVK.Kind, err)
	}
	list.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
	return nil
}

// getError returns err, or a NoKindMatchError when it is a 404 that names no object: the API server answers
// so for a path it does not serve, and names the object when only the object is missing.
func getError(err error, gvk schema.GroupVersionKind) error {
	var status apierrors.APIStatus
	if apierrors.IsNotFound(err) && errors.As(err, &status) {
		if d := status.Status().Details; d == nil || d.Name == "" {
			return noKindMatch(gvk)
		}
	}
	return err
}

// listError returns err, or a NoKindMatchError when it is a 404: a list of a served kind never is one.
func listError(err error, gvk schema.GroupVersionKind) error {
	if apierrors.IsNotFound(err) {
		return noKindMatch(gvk)
	}
	return err
}

func noKindMatch(gvk schema.GroupVersionKind) error {
	return &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
}
