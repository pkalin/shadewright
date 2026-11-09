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

package digest

import (
	"errors"
	"reflect"
	"regexp"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/testutil/deadline"
)

// mustCompute returns the digest of in and fails the test when Compute refuses it.
func mustCompute(t *testing.T, in Input) string {
	t.Helper()
	d, err := Compute(in)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return d
}

// planDigestPattern is the schema pattern of status.planDigest.
var planDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Golden digests. They must never change: a changed value means every stored plan digest is superseded.
// If the encoding has to change, bump encodingVersion and update these values in the same change.
const (
	goldenKeda   = "sha256:99c0ff41a8f7ca8c680a06302e665cc77e9d60a20037272294cee2e3031f3570"
	goldenMemory = "sha256:75ba5ff32fb8fc2252992bd5277ab8c27765e1461b01162dd214292914dbfaa2"
)

func source() Source {
	return Source{
		RepositoryRef:  "fleet",
		URL:            "https://github.com/example/fleet",
		InstallationID: 12345678,
		TargetBranch:   "main",
		Path:           "apps/orders/scaledobject.yaml",
		BuildPath:      "apps/orders",
		BaseCommit:     "0123456789abcdef0123456789abcdef01234567",
	}
}

func kedaInput() Input {
	return Input{
		Action: v1alpha1.Action{
			Type: v1alpha1.ActionAdjustKedaMaxReplicas,
			AdjustKedaMaxReplicas: &v1alpha1.AdjustKedaMaxReplicas{
				PreviousValue: 10,
				DesiredValue:  15,
			},
		},
		TargetUID:        "22222222-2222-4222-8222-222222222222",
		Source:           source(),
		PolicyUID:        "44444444-4444-4444-8444-444444444444",
		PolicyGeneration: 3,
	}
}

func memoryInput(previous, desired string) Input {
	in := kedaInput()
	in.Action = v1alpha1.Action{
		Type: v1alpha1.ActionSetContainerMemoryLimit,
		SetContainerMemoryLimit: &v1alpha1.SetContainerMemoryLimit{
			Container: "worker",
			Previous:  resource.MustParse(previous),
			Desired:   resource.MustParse(desired),
		},
	}
	in.Source.Path = "apps/orders/deployment.yaml"
	return in
}

func TestGolden(t *testing.T) {
	cases := map[string]struct {
		in   Input
		want string
	}{
		"AdjustKedaMaxReplicas":   {kedaInput(), goldenKeda},
		"SetContainerMemoryLimit": {memoryInput("256Mi", "512Mi"), goldenMemory},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := mustCompute(t, tc.in)
			if got != tc.want {
				t.Errorf("Compute() = %s, want %s", got, tc.want)
			}
			if again := mustCompute(t, tc.in); again != got {
				t.Errorf("Compute() is not deterministic: %s then %s", got, again)
			}
		})
	}
}

func TestMatchesPlanDigestPattern(t *testing.T) {
	for _, in := range []Input{kedaInput(), memoryInput("1Mi", "1Ti"), {}} {
		if got := mustCompute(t, in); !planDigestPattern.MatchString(got) {
			t.Errorf("Compute() = %q does not match %s", got, planDigestPattern)
		}
	}
}

func TestEveryFieldChangesDigest(t *testing.T) {
	cases := map[string]struct {
		base   func() Input
		mutate func(*Input)
	}{
		"action.type": {kedaInput, func(in *Input) {
			in.Action.Type = v1alpha1.ActionSetContainerMemoryLimit
		}},
		"action.adjustKedaMaxReplicas.previousValue": {kedaInput, func(in *Input) {
			in.Action.AdjustKedaMaxReplicas.PreviousValue = 11
		}},
		"action.adjustKedaMaxReplicas.desiredValue": {kedaInput, func(in *Input) {
			in.Action.AdjustKedaMaxReplicas.DesiredValue = 16
		}},
		"action.adjustKedaMaxReplicas removed": {kedaInput, func(in *Input) {
			in.Action.AdjustKedaMaxReplicas = nil
		}},
		"action.setContainerMemoryLimit added": {kedaInput, func(in *Input) {
			in.Action.SetContainerMemoryLimit = &v1alpha1.SetContainerMemoryLimit{}
		}},
		"action.setContainerMemoryLimit.container": {memoryBase, func(in *Input) {
			in.Action.SetContainerMemoryLimit.Container = "sidecar"
		}},
		"action.setContainerMemoryLimit.previous": {memoryBase, func(in *Input) {
			in.Action.SetContainerMemoryLimit.Previous = resource.MustParse("257Mi")
		}},
		"action.setContainerMemoryLimit.desired": {memoryBase, func(in *Input) {
			in.Action.SetContainerMemoryLimit.Desired = resource.MustParse("513Mi")
		}},
		"targetUID": {kedaInput, func(in *Input) {
			in.TargetUID = "33333333-3333-4333-8333-333333333333"
		}},
		"source.repositoryRef": {kedaInput, func(in *Input) {
			in.Source.RepositoryRef = "fleet-2"
		}},
		"source.url": {kedaInput, func(in *Input) {
			in.Source.URL = "https://github.com/example/fleet-2"
		}},
		"source.installationID": {kedaInput, func(in *Input) {
			in.Source.InstallationID = 87654321
		}},
		"source.targetBranch": {kedaInput, func(in *Input) {
			in.Source.TargetBranch = "release"
		}},
		"source.path": {kedaInput, func(in *Input) {
			in.Source.Path = "apps/orders/other.yaml"
		}},
		"source.buildPath": {kedaInput, func(in *Input) {
			in.Source.BuildPath = "apps"
		}},
		"source.baseCommit": {kedaInput, func(in *Input) {
			in.Source.BaseCommit = "89abcdef0123456789abcdef0123456789abcdef"
		}},
		"policyUID": {kedaInput, func(in *Input) {
			in.PolicyUID = "55555555-5555-4555-8555-555555555555"
		}},
		"policyGeneration": {kedaInput, func(in *Input) {
			in.PolicyGeneration = 4
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			before := mustCompute(t, tc.base())
			in := tc.base()
			tc.mutate(&in)
			if after := mustCompute(t, in); after == before {
				t.Errorf("changing %s left the digest at %s", name, before)
			}
		})
	}
}

func memoryBase() Input { return memoryInput("256Mi", "512Mi") }

// TestFieldsCovered fails when a field is added to an input type, so the encoding and the per-field test above
// are updated with it instead of the new field being left out of the digest silently.
func TestFieldsCovered(t *testing.T) {
	want := map[reflect.Type]int{
		reflect.TypeFor[Input]():                            5,
		reflect.TypeFor[Source]():                           7,
		reflect.TypeFor[v1alpha1.Action]():                  3,
		reflect.TypeFor[v1alpha1.AdjustKedaMaxReplicas]():   2,
		reflect.TypeFor[v1alpha1.SetContainerMemoryLimit](): 3,
	}
	for typ, n := range want {
		if got := typ.NumField(); got != n {
			t.Errorf("%s has %d fields, the digest encodes %d: update encode and its tests", typ, got, n)
		}
	}
}

func TestEqualQuantitiesGiveSameDigest(t *testing.T) {
	groups := [][]string{
		{"256Mi", "268435456", "268435456000m", "0.25Gi", "268435.456k", "2.68435456e8"},
		{"1Gi", "1024Mi", "1073741824"},
		{"1G", "1000M", "1e9", "1000000000"},
	}
	for _, group := range groups {
		want := mustCompute(t, memoryInput(group[0], "1Ti"))
		for _, q := range group[1:] {
			if got := mustCompute(t, memoryInput(q, "1Ti")); got != want {
				t.Errorf("previous %s gives %s, %s gives %s", q, got, group[0], want)
			}
			if got, w := mustCompute(t, memoryInput("1Mi", q)), mustCompute(t, memoryInput("1Mi", group[0])); got != w {
				t.Errorf("desired %s gives %s, %s gives %s", q, got, group[0], w)
			}
		}
	}
}

func TestCanonicalQuantity(t *testing.T) {
	cases := map[string]string{
		"256Mi":   "268435456",
		"1.25":    "1.25",
		"1500m":   "1.5",
		"1e3":     "1000",
		"0":       "0",
		"0.000":   "0",
		"-1Ki":    "-1024",
		"+512":    "512",
		"100.10m": "0.1001",
	}
	for in, want := range cases {
		if got := canonicalQuantity(resource.MustParse(in)); got != want {
			t.Errorf("canonicalQuantity(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestNoFieldBoundaryCollision checks that moving bytes from one field to the next changes the digest, which a
// plain concatenation would not.
func TestNoFieldBoundaryCollision(t *testing.T) {
	a := kedaInput()
	a.Source.TargetBranch = "main"
	a.Source.Path = "apps/x.yaml"
	b := kedaInput()
	b.Source.TargetBranch = "mainapps"
	b.Source.Path = "/x.yaml"
	if mustCompute(t, a) == mustCompute(t, b) {
		t.Error("digests collide when bytes move between adjacent fields")
	}
}

// TestRefusesUnboundedQuantities is part of #593: a request whose memory value has a huge decimal exponent is
// refused, instead of being written as a plain decimal number of billions of digits.
func TestRefusesUnboundedQuantities(t *testing.T) {
	for name, in := range map[string]Input{
		"previous": memoryInput("1e2147483647", "1Ti"),
		"desired":  memoryInput("1Mi", "1e2147483647"),
	} {
		t.Run(name, func(t *testing.T) {
			deadline.Within(t, 5*time.Second, func(t *testing.T) {
				if d, err := Compute(in); !errors.Is(err, ErrUnboundedQuantity) {
					t.Fatalf("Compute = %q, %v, want %v", d, err, ErrUnboundedQuantity)
				}
			})
		})
	}
}
