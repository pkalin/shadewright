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

package v1alpha1

import (
	"strings"
	"testing"
)

func nameTestSpec() RemediationRequestSpec {
	return RemediationRequestSpec{
		IncidentRef: IncidentRef{Name: "orders", UID: "11111111-1111-4111-8111-111111111111"},
		TargetRef: ObjectRef{
			APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject", Namespace: "shop", Name: "orders",
			UID: "22222222-2222-4222-8222-222222222222",
		},
		PolicyRef: "keda-capacity",
		Action: Action{
			Type:                  ActionAdjustKedaMaxReplicas,
			AdjustKedaMaxReplicas: &AdjustKedaMaxReplicas{PreviousValue: 10, DesiredValue: 15},
		},
	}
}

func requestName(t *testing.T, spec RemediationRequestSpec) Name {
	t.Helper()
	n, err := RequestName(spec)
	if err != nil {
		t.Fatalf("RequestName: %v", err)
	}
	return n
}

// A request without replaces keeps the name it had before replaces existed, so the requests already in a
// cluster are found again by a retried create (invariant 5).
func TestRequestNameUnchangedWithoutReplaces(t *testing.T) {
	if got, want := requestName(t, nameTestSpec()), Name("remediation-af5b88cf21ab82fe1801f345"); got != want {
		t.Fatalf("RequestName = %q, want %q", got, want)
	}
}

// replaces and chainRoot are part of the plan, so they change the name (ADR-0028 section 4.1): the same plan
// answering another hint, or replacing another request, is another request, and the same plan from the same
// hint is the same request.
func TestRequestNameDerivesFromReplaces(t *testing.T) {
	plain := nameTestSpec()
	withHint := func(predecessor Name, commentID int64, digestByte string) RemediationRequestSpec {
		s := nameTestSpec()
		s.Replaces = &RequestReplacement{
			Name: predecessor, UID: "33333333-3333-4333-8333-333333333333",
			Hint: HintRef{Source: HintSourceGitHub, CommentID: commentID, Digest: "sha256:" + strings.Repeat(digestByte, 64)},
		}
		s.ChainRoot = "33333333-3333-4333-8333-333333333333"
		return s
	}
	base := requestName(t, withHint("remediation-a", 1, "a"))
	if base == requestName(t, plain) {
		t.Fatalf("a replacement has the name of the plain plan: %q", base)
	}
	if again := requestName(t, withHint("remediation-a", 1, "a")); again != base {
		t.Fatalf("the same plan from the same hint got another name: %q, then %q", base, again)
	}
	for name, spec := range map[string]RemediationRequestSpec{
		"another predecessor": withHint("remediation-b", 1, "a"),
		"another comment":     withHint("remediation-a", 2, "a"),
		"another digest":      withHint("remediation-a", 1, "b"),
	} {
		if got := requestName(t, spec); got == base {
			t.Errorf("%s: name unchanged (%q)", name, got)
		}
	}
}
