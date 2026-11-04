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
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
				return apierrors.NewForbidden(schema.GroupResource{Group: remediationv1alpha1.GroupVersion.Group, Resource: "incidents"},
					name, nil)
			},
		}).Build()
	r := &RemediationRequestReconciler{Client: forbidden, APIReader: forbidden}
	if got := r.verifiedIncidentName(context.Background(), request(name, uid)); got != "" {
		t.Errorf("failed read: got %q, want \"\"", got)
	}

	// logged returns a context whose logger records each line's key-value pairs into lines.
	logged := func(lines *[]string) context.Context {
		var mu sync.Mutex
		return logf.IntoContext(context.Background(), funcr.New(func(_, args string) {
			mu.Lock()
			defer mu.Unlock()
			*lines = append(*lines, args)
		}, funcr.Options{}))
	}

	// A read that hangs ends at its own deadline, long before the step's, and keeps the name redacted (#272).
	stalled := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(incident(ns, name, hex)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}).Build()
	var lines []string
	r = &RemediationRequestReconciler{Client: stalled, APIReader: stalled, IncidentReadTimeout: 50 * time.Millisecond}
	step, cancel := context.WithTimeout(logged(&lines), 5*time.Second)
	defer cancel()
	start := time.Now()
	if got := r.verifiedIncidentName(step, request(name, uid)); got != "" {
		t.Errorf("stalled read: got %q, want \"\"", got)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("stalled read took %v, want about its own deadline", took)
	}
	if step.Err() != nil {
		t.Errorf("the read's deadline ended the step's context: %v", step.Err())
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"reason"="Timeout"`) {
		t.Errorf("stalled read: logged %q, want one line with reason Timeout", lines)
	}

	// An API error that comes as the read's deadline passes keeps its own reason: only a timeout is logged as one.
	late := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(incident(ns, name, hex)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				<-ctx.Done()
				return apierrors.NewForbidden(schema.GroupResource{Group: remediationv1alpha1.GroupVersion.Group, Resource: "incidents"},
					name, nil)
			},
		}).Build()
	lines = nil
	r = &RemediationRequestReconciler{Client: late, APIReader: late, IncidentReadTimeout: 50 * time.Millisecond}
	if got := r.verifiedIncidentName(logged(&lines), request(name, uid)); got != "" {
		t.Errorf("late forbidden read: got %q, want \"\"", got)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"reason"="Forbidden"`) {
		t.Errorf("late forbidden read: logged %q, want one line with reason Forbidden", lines)
	}

	// A short step caps the read at half the time the step has left, whatever IncidentReadTimeout says, so a
	// small WriteTimeout still leaves the open its time.
	r = &RemediationRequestReconciler{Client: stalled, APIReader: stalled, IncidentReadTimeout: time.Minute}
	short, cancelShort := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShort()
	if got := r.verifiedIncidentName(short, request(name, uid)); got != "" {
		t.Errorf("short step: got %q, want \"\"", got)
	}
	if short.Err() != nil {
		t.Errorf("short step: the read used up the step's deadline: %v", short.Err())
	}

	// A cancelled step cancels the read too: the name is redacted at once and the cancellation stays the
	// caller's.
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	r = &RemediationRequestReconciler{Client: stalled, APIReader: stalled}
	start = time.Now()
	if got := r.verifiedIncidentName(cancelled, request(name, uid)); got != "" {
		t.Errorf("cancelled step: got %q, want \"\"", got)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("cancelled step took %v, want an immediate return", took)
	}
}
