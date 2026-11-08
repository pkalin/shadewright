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

package github

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// compareStep answers the compare of shaBase (the commit) with shaOld (the ancestor asked about).
func compareStep(raw string) step {
	s := step{method: http.MethodGet, path: "/compare/" + shaBase + "..." + shaOld, status: http.StatusOK, raw: raw}
	s.query = "per_page=1"
	return s
}

func compareStatus(status string) string {
	return `{"status":"` + status + `","commits":[],"files":[]}`
}

// TestIsAncestor asks compare in the direction commit...ancestor, so an ancestor reads as "behind".
func TestIsAncestor(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{"behind", true},
		{"identical", true},
		{"ahead", false},
		{"diverged", false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			h := newHarness(t, compareStep(compareStatus(tc.status)))
			got, err := h.p.IsAncestor(ctx, shaOld, shaBase)
			if err != nil || got != tc.want {
				t.Errorf("IsAncestor = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestIsAncestorSameCommitAsksNothing(t *testing.T) {
	h := newHarness(t)
	if got, err := h.p.IsAncestor(ctx, shaBase, shaBase); !got || err != nil {
		t.Errorf("IsAncestor = %v, %v", got, err)
	}
}

// TestIsAncestorLargeAnswerIsNotAncestry: only a commit that is not behind has commits and files to list.
func TestIsAncestorLargeAnswerIsNotAncestry(t *testing.T) {
	big := `{"status":"behind","files":["` + strings.Repeat("x", maxJSONBody) + `"]}`
	h := newHarness(t, compareStep(big))
	if got, err := h.p.IsAncestor(ctx, shaOld, shaBase); got || err != nil {
		t.Errorf("IsAncestor = %v, %v; want false", got, err)
	}
}

func TestIsAncestorFailures(t *testing.T) {
	notFound := compareStep("")
	notFound.status = http.StatusNotFound
	h := newHarness(t, notFound)
	got, err := h.p.IsAncestor(ctx, shaOld, shaBase)
	if got || !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("not found: IsAncestor = %v, %v", got, err)
	}
	assertSafe(t, err)

	h = newHarness(t, compareStep(compareStatus("sideways")))
	if got, err := h.p.IsAncestor(ctx, shaOld, shaBase); got || err == nil {
		t.Errorf("unknown status: IsAncestor = %v, %v", got, err)
	}
	h = newHarness(t, compareStep("not json"))
	if got, err := h.p.IsAncestor(ctx, shaOld, shaBase); got || err == nil {
		t.Errorf("invalid JSON: IsAncestor = %v, %v", got, err)
	}
	h = newHarness(t)
	for _, c := range []string{"", "main", shaBase[:12], strings.Repeat("A", 40), strings.Repeat("a", 64)} {
		if got, err := h.p.IsAncestor(ctx, c, shaBase); got || err == nil {
			t.Errorf("IsAncestor(%q, ...) = %v, %v; want an error and no request", c, got, err)
		}
		if got, err := h.p.IsAncestor(ctx, shaBase, c); got || err == nil {
			t.Errorf("IsAncestor(..., %q) = %v, %v; want an error and no request", c, got, err)
		}
	}
}

// TestIsAncestorSHA256IsPermanent checks that an ID GitHub cannot compare fails with the permanent
// ErrInvalidCommit, which a caller does not retry.
func TestIsAncestorSHA256IsPermanent(t *testing.T) {
	h := newHarness(t)
	_, err := h.p.IsAncestor(ctx, shaBase, strings.Repeat("ab", 32))
	if !errors.Is(err, providers.ErrInvalidCommit) || providers.IsRetryable(err) {
		t.Errorf("IsAncestor of a SHA-256 ID: %v; want ErrInvalidCommit, not retryable", err)
	}
}
