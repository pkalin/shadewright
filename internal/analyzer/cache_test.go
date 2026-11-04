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
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

const (
	demo  = "demo"
	teamB = "team-b"
)

func TestParseNamespaces(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{in: demo, want: []string{demo}},
		{in: " team-b, demo ,team-b", want: []string{demo, teamB}},
		{in: "demo,,", want: []string{demo}},
		// No namespace is an empty list, never "all namespaces".
		{in: ""},
		{in: " , "},
		{in: "Demo", wantErr: true},
		{in: "demo,team_a", wantErr: true},
		{in: "*", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseNamespaces(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseNamespaces(%q) error = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("ParseNamespaces(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCacheOptions(t *testing.T) {
	opts := CacheOptions("shadewright-system", []string{demo, teamB})
	if len(opts.DefaultNamespaces) != 2 {
		t.Fatalf("DefaultNamespaces = %v, want demo and team-b", opts.DefaultNamespaces)
	}
	for _, ns := range []string{demo, teamB} {
		if _, ok := opts.DefaultNamespaces[ns]; !ok {
			t.Errorf("DefaultNamespaces lacks %s", ns)
		}
	}
	found := 0
	for obj, by := range opts.ByObject {
		switch obj.(type) {
		case *remediationv1alpha1.Incident, *remediationv1alpha1.RemediationRequest:
			found++
			if _, ok := by.Namespaces["shadewright-system"]; !ok || len(by.Namespaces) != 1 {
				t.Errorf("%T is cached in %v, want the operator namespace only", obj, by.Namespaces)
			}
		}
	}
	if found != 2 {
		t.Errorf("ByObject restricts %d of Incident and RemediationRequest, want 2", found)
	}
}

// TestCacheOptionsWithNoNamespace checks that an empty list never becomes an empty DefaultNamespaces, which
// controller-runtime reads as every namespace: the default is the operator namespace alone.
func TestCacheOptionsWithNoNamespace(t *testing.T) {
	for _, watched := range [][]string{nil, {}} {
		opts := CacheOptions("shadewright-system", watched)
		if len(opts.DefaultNamespaces) != 1 {
			t.Fatalf("DefaultNamespaces = %v for %#v, want the operator namespace only", opts.DefaultNamespaces, watched)
		}
		if _, ok := opts.DefaultNamespaces["shadewright-system"]; !ok {
			t.Errorf("DefaultNamespaces = %v, want the operator namespace", opts.DefaultNamespaces)
		}
	}
}

func TestCacheOptionsStripManagedFields(t *testing.T) {
	opts := CacheOptions("shadewright-system", []string{demo})
	if opts.DefaultTransform == nil {
		t.Fatal("no DefaultTransform: managed fields of every event and pod would be cached")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:          "web",
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubelet"}},
	}}
	out, err := opts.DefaultTransform(pod)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(*corev1.Pod); len(got.ManagedFields) != 0 {
		t.Errorf("managed fields kept: %v", got.ManagedFields)
	}
}
