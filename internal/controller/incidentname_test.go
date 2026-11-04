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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The Incident name is shown unredacted only when the executor read that Incident back from the API server:
// same namespace, the UID of incidentRef, and the name the collector derives from its fingerprint (#264).
// Everything else, including a read that fails, gives "" and so a redacted name.
func TestVerifiedIncidentName(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const ns, uid = "shadewright-system", "33333333-3333-4333-8333-333333333333"
	hex := strings.Repeat("9f86d081", 8)
	otherHex := strings.Repeat("3f1c9a7b", 8)
	incident := func(namespace, name, fingerprintHex string) *remediationv1alpha1.Incident {
		return &remediationv1alpha1.Incident{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: uid},
			Spec:       remediationv1alpha1.IncidentSpec{Fingerprint: "sha256:" + fingerprintHex},
		}
	}
	request := func(name string, ref remediationv1alpha1.UID) *remediationv1alpha1.RemediationRequest {
		rr := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rr"}}
		rr.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: name, UID: ref}
		return rr
	}
	name := "incident-" + hex
	for _, tc := range []struct {
		desc    string
		objects []client.Object
		rr      *remediationv1alpha1.RemediationRequest
		want    string
	}{
		{"read back", []client.Object{incident(ns, name, hex)}, request(name, uid), name},
		{"no such Incident", nil, request(name, uid), ""},
		{"another UID", []client.Object{incident(ns, name, hex)}, request(name, "44444444-4444-4444-8444-444444444444"), ""},
		{"no UID", []client.Object{incident(ns, name, hex)}, request(name, ""), ""},
		{"in another namespace", []client.Object{incident("other", name, hex)}, request(name, uid), ""},
		{"name not from its fingerprint", []client.Object{incident(ns, name, otherHex)}, request(name, uid), ""},
		{"not the collector's form", []client.Object{incident(ns, "orders-scaling", hex)}, request("orders-scaling", uid), ""},
		{"40 hex digits", []client.Object{incident(ns, "incident-"+hex[:40], hex[:40])},
			request("incident-"+hex[:40], uid), ""},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
			r := &RemediationRequestReconciler{Client: c, APIReader: c}
			if got := r.verifiedIncidentName(context.Background(), tc.rr); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	// A read that fails (no permission, an outage) keeps the name redacted instead of blocking the body.
	forbidden := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(incident(ns, name, hex)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Group: "remediation.shadewright.io", Resource: "incidents"},
					name, nil)
			},
		}).Build()
	r := &RemediationRequestReconciler{Client: forbidden, APIReader: forbidden}
	if got := r.verifiedIncidentName(context.Background(), request(name, uid)); got != "" {
		t.Errorf("failed read: got %q, want \"\"", got)
	}
}
