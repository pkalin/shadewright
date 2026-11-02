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

package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// A policy created together with its request may not be in the cache yet. getFresh must find it on the API
// server instead of letting the request be denied for good.
func TestGetFreshFallsBackToAPIServer(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "remediation.shadewright.io", Resource: "remediationpolicies"}, "p")
	staleCache := clientfake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return notFound
		},
	}).Build()
	key := types.NamespacedName{Name: "p"}

	apiServer := clientfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&remediationv1alpha1.RemediationPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}).Build()
	r := &RemediationRequestReconciler{Client: staleCache, APIReader: apiServer}
	if err := r.getFresh(context.Background(), key, &remediationv1alpha1.RemediationPolicy{}); err != nil {
		t.Errorf("policy only on the API server: got %v, want it found", err)
	}

	empty := clientfake.NewClientBuilder().WithScheme(scheme).Build()
	r.APIReader = empty
	if err := r.getFresh(context.Background(), key, &remediationv1alpha1.RemediationPolicy{}); !apierrors.IsNotFound(err) {
		t.Errorf("policy nowhere: got %v, want NotFound", err)
	}
}
