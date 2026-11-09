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

package fluxstatus_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/fluxstatus"
)

const (
	revOld = "0e2d4c6b8a9f7e5d3c1b0a9f8e7d6c5b4a3f2e1d"
	revNew = "1f3c5e7a9b2d4f6081a3c5e7f9b1d3f5a7c9e1b3"
)

// fixture reads an object recorded for the Flux module, as encoding/json decodes it (numbers as float64).
func fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "modules", "flux", "testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return obj
}

// TestReadyOnRecordedObjects checks Ready on the recorded objects, and that only the one still being reconciled
// reads as Reconciling: a Ready one and every recorded failure do not (#556).
func TestReadyOnRecordedObjects(t *testing.T) {
	for name, want := range map[string]struct{ ready, reconciling bool }{
		"kustomization-ready.json":              {ready: true},
		"kustomization-reconciling.json":        {reconciling: true},
		"kustomization-build-failed.json":       {},
		"kustomization-stalled.json":            {},
		"kustomization-invalid-manifest.json":   {},
		"kustomization-dependency-missing.json": {},
		"helmrelease-upgrade-failed.json":       {},
		"gitrepository-auth-failed.json":        {},
	} {
		if got := fluxstatus.Ready(fixture(t, name)); got != want.ready {
			t.Errorf("Ready(%s) = %v, want %v", name, got, want.ready)
		}
		if got := fluxstatus.Reconciling(fixture(t, name)); got != want.reconciling {
			t.Errorf("Reconciling(%s) = %v, want %v", name, got, want.reconciling)
		}
	}
}

// TestReconcilingCases checks the cases the recorded objects do not show: a canceled health check and a
// status about an older generation are a reconcile in progress, a current Ready=False or Stalled=True is not.
func TestReconcilingCases(t *testing.T) {
	const statusFalse, statusUnknown = "False", "Unknown"
	ready := func(o map[string]any) map[string]any {
		return o["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{"newer generation", func(o map[string]any) { o["metadata"].(map[string]any)["generation"] = float64(2) }, true},
		{"Ready False about an older generation", func(o map[string]any) {
			o["metadata"].(map[string]any)["generation"] = float64(2)
			ready(o)["status"] = statusFalse
			ready(o)["reason"] = "BuildFailed"
		}, true},
		{"health check canceled", func(o map[string]any) {
			ready(o)["status"] = statusFalse
			ready(o)["reason"] = fluxstatus.ReasonHealthCheckCanceled
		}, true},
		{"Ready Unknown", func(o map[string]any) { ready(o)["status"] = statusUnknown }, true},
		{"Ready False", func(o map[string]any) {
			ready(o)["status"] = statusFalse
			ready(o)["reason"] = "ReconciliationFailed"
		}, false},
		{"Ready Unknown and Stalled", func(o map[string]any) {
			ready(o)["status"] = statusUnknown
			st := o["status"].(map[string]any)
			st["conditions"] = append(st["conditions"].([]any), map[string]any{"type": "Stalled", "status": "True"})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := fixture(t, "kustomization-ready.json")
			tc.mutate(o)
			if got := fluxstatus.Reconciling(o); got != tc.want {
				t.Errorf("Reconciling = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReadyFailsClosed changes one field of a Ready object at a time; none of the changes may read as Ready.
func TestReadyFailsClosed(t *testing.T) {
	ready := func(o map[string]any) map[string]any {
		return o["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"newer generation", func(o map[string]any) { o["metadata"].(map[string]any)["generation"] = float64(2) }},
		{"no observed generation", func(o map[string]any) { delete(o["status"].(map[string]any), "observedGeneration") }},
		{"observed generation of another type", func(o map[string]any) { o["status"].(map[string]any)["observedGeneration"] = "1" }},
		{"Ready about another generation", func(o map[string]any) { ready(o)["observedGeneration"] = float64(0) }},
		{"Ready with a string generation", func(o map[string]any) { ready(o)["observedGeneration"] = "1" }},
		{"Ready with a fractional generation", func(o map[string]any) { ready(o)["observedGeneration"] = 1.5 }},
		{"Ready with a null generation", func(o map[string]any) { ready(o)["observedGeneration"] = nil }},
		{"Ready False", func(o map[string]any) { ready(o)["status"] = "False" }},
		{"Ready Unknown", func(o map[string]any) { ready(o)["status"] = "Unknown" }},
		{"Ready true in lower case", func(o map[string]any) { ready(o)["status"] = "true" }},
		{"no Ready condition", func(o map[string]any) { ready(o)["type"] = "Healthy" }},
		{"conditions not a list", func(o map[string]any) { o["status"].(map[string]any)["conditions"] = "Ready" }},
		{"no status", func(o map[string]any) { delete(o, "status") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := fixture(t, "kustomization-ready.json")
			tc.mutate(o)
			if fluxstatus.Ready(o) {
				t.Error("Ready = true, want false")
			}
		})
	}
}

// TestReadyReadsTheFirstReadyCondition checks that a later Ready=True does not override a first Ready=False.
func TestReadyReadsTheFirstReadyCondition(t *testing.T) {
	o := fixture(t, "kustomization-build-failed.json")
	st := o["status"].(map[string]any)
	st["conditions"] = append(st["conditions"].([]any), map[string]any{"type": "Ready", "status": "True"})
	if fluxstatus.Ready(o) {
		t.Error("Ready = true, want false")
	}
}

func TestFailingOnRecordedObjects(t *testing.T) {
	for name, want := range map[string]string{
		"kustomization-ready.json":              "",
		"kustomization-reconciling.json":        "",
		"kustomization-build-failed.json":       fluxstatus.ConditionReady,
		"kustomization-stalled.json":            fluxstatus.ConditionStalled,
		"kustomization-invalid-manifest.json":   fluxstatus.ConditionReady,
		"kustomization-dependency-missing.json": fluxstatus.ConditionReady,
		"helmrelease-upgrade-failed.json":       fluxstatus.ConditionReady,
		"gitrepository-auth-failed.json":        fluxstatus.ConditionReady,
	} {
		o := fixture(t, name)
		gen, has := fluxstatus.Generation(o)
		cond, typ := fluxstatus.Failing(fluxstatus.Status(o), gen, has)
		if typ != want || (cond == nil) != (want == "") {
			t.Errorf("Failing(%s) = %v, %q; want %q", name, cond, typ, want)
		}
	}
}

func TestParseRevision(t *testing.T) {
	sha256 := strings.Repeat("0123456789abcdef", 4)
	for _, tc := range []struct {
		revision, ref, commit string
		ok                    bool
	}{
		{"main@sha1:" + revOld, "main", revOld, true},
		{"cluster/kind@sha256:" + sha256, "cluster/kind", sha256, true},
		{"a@b@sha1:" + revOld, "a@b", revOld, true},
		{"@sha1:" + revOld, "", "", false},
		{"main/" + revOld, "", "", false},
		{"main@" + revOld, "", "", false},
		{"main@sha1:" + revOld[:12], "", "", false},
		{"main@sha1:" + strings.ToUpper(revOld), "", "", false},
		{"main@sha256:" + revOld, "", "", false},
		{"main@sha1:" + sha256, "", "", false},
		{"main@md5:" + revOld, "", "", false},
		{"2.18.1", "", "", false},
		{"", "", "", false},
	} {
		ref, commit, ok := fluxstatus.ParseRevision(tc.revision)
		if ref != tc.ref || commit != tc.commit || ok != tc.ok {
			t.Errorf("ParseRevision(%q) = %q, %q, %v; want %q, %q, %v", tc.revision, ref, commit, ok, tc.ref, tc.commit, tc.ok)
		}
	}
}

func TestAppliedFetched(t *testing.T) {
	ks := fixture(t, "kustomization-ready.json")
	src := fixture(t, "gitrepository-auth-failed.json")
	if !fluxstatus.AppliedFetched(ks, src, "cluster/kind") {
		t.Error("AppliedFetched = false for the revision the source serves")
	}
	if fluxstatus.AppliedFetched(ks, src, "main") {
		t.Error("AppliedFetched = true for another branch")
	}
	behind := fixture(t, "kustomization-invalid-manifest.json")
	src["status"].(map[string]any)["artifact"].(map[string]any)["revision"] = "cluster/kind@sha1:" + revNew
	if fluxstatus.AppliedFetched(behind, src, "cluster/kind") {
		t.Error("AppliedFetched = true for a Kustomization behind its source")
	}
	empty := fixture(t, "kustomization-reconciling.json")
	delete(empty["status"].(map[string]any), "lastAppliedRevision")
	delete(src["status"].(map[string]any), "artifact")
	if fluxstatus.AppliedFetched(empty, src, "") {
		t.Error("AppliedFetched = true with no revision on either side")
	}
}

func TestIntegerReadsBothDecodings(t *testing.T) {
	for _, v := range []any{int64(3), 3, float64(3), json.Number("3")} {
		if n, ok := fluxstatus.Integer(v); !ok || n != 3 {
			t.Errorf("Integer(%#v) = %d, %v", v, n, ok)
		}
	}
	for _, v := range []any{nil, "3", 3.5, json.Number("3.5"), true} {
		if _, ok := fluxstatus.Integer(v); ok {
			t.Errorf("Integer(%#v) ok", v)
		}
	}
}

func TestParseCommitRevision(t *testing.T) {
	sha256 := strings.Repeat("0123456789abcdef", 4)
	for _, tc := range []struct {
		revision, ref, commit string
		ok                    bool
	}{
		{"main@sha1:" + revOld, "main", revOld, true},
		{"sha1:" + revOld, "", revOld, true},
		{"sha256:" + sha256, "", sha256, true},
		{"@sha1:" + revOld, "", "", false},
		{"sha1:" + revOld[:12], "", "", false},
		{"sha1:" + strings.ToUpper(revNew), "", "", false},
		{"sha256:" + revOld, "", "", false},
		{revOld, "", "", false},
		{"main/" + revOld, "", "", false},
		{"2.18.1", "", "", false},
		{"", "", "", false},
	} {
		ref, commit, ok := fluxstatus.ParseCommitRevision(tc.revision)
		if ref != tc.ref || commit != tc.commit || ok != tc.ok {
			t.Errorf("ParseCommitRevision(%q) = %q, %q, %v; want %q, %q, %v", tc.revision, ref, commit, ok, tc.ref, tc.commit, tc.ok)
		}
	}
	// The source mapping still needs a branch in the revision.
	if fluxstatus.RevisionOfBranch("sha1:"+revOld, "") {
		t.Error("RevisionOfBranch accepted a revision without a branch")
	}
}
