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
package sourcemap_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

// readsAt is a GitProvider that allows only ReadFile and ListDir at one commit. Every other method is the nil
// embedded interface's, so calling one panics and fails the test.
type readsAt struct {
	providers.GitProvider
	git    *fake.Git
	commit string
	t      *testing.T
}

func (r readsAt) ReadFile(ctx context.Context, commit, path string, limit int) (providers.File, error) {
	if commit != r.commit {
		r.t.Errorf("ReadFile %s at %s, want only commit %s", path, commit, r.commit)
	}
	return r.git.ReadFile(ctx, commit, path, limit)
}

func (r readsAt) ListDir(ctx context.Context, commit, dir string) ([]providers.DirEntry, error) {
	if commit != r.commit {
		r.t.Errorf("ListDir %s at %s, want only commit %s", dir, commit, r.commit)
	}
	return r.git.ListDir(ctx, commit, dir)
}

// gitOf builds the fixture's repository and returns it with the commit at its head.
func gitOf(t *testing.T, f *fixture) (*fake.Git, string) {
	t.Helper()
	files := make(map[string][]byte, len(f.files))
	for p, s := range f.files {
		files[p] = []byte(s)
	}
	git := fake.NewGit(f.gitBranch, files)
	head, err := git.BranchHead(context.Background(), f.gitBranch)
	if err != nil {
		t.Fatal(err)
	}
	return git, head
}

// candidateInput is the lookup's input for the fixture's target, live object and Kustomization.
func candidateInput(t *testing.T, f *fixture) sourcemap.CandidateInput {
	t.Helper()
	gv, err := schema.ParseGroupVersion(f.in.Target.APIVersion)
	if err != nil {
		t.Fatal(err)
	}
	live := f.target
	if f.in.Target.Kind == f.deployment.GetKind() {
		live = f.deployment
	}
	return sourcemap.CandidateInput{
		Target:    gv.WithKind(f.in.Target.Kind),
		Namespace: f.in.Target.Namespace, Name: f.in.Target.Name,
		Labels:        live.GetLabels(),
		Kustomization: f.ks,
	}
}

func applied(head string) string { return "main@sha1:" + head }

//nolint:goconst // literal names keep the table readable
func TestFindManifestCandidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(f *fixture)
		// revision gives the Kustomization's lastAppliedRevision from the commit; nil applies the commit on main.
		revision   func(commit string) string
		path       string
		transforms []string
		reason     sourcemap.Reason
	}{
		{name: "plain ScaledObject", path: soFile},
		{name: "plain Deployment", change: (*fixture).targetDeployment, path: "apps/orders/deployment.yaml"},
		{
			name: "path without a kustomization file is listed as Flux generates it",
			change: func(f *fixture) {
				delete(f.files, ksFile)
			},
			path: soFile,
		},
		{
			name:     "bare commit revision",
			revision: func(c string) string { return "sha1:" + c },
			path:     soFile,
		},
		{
			name: "transforms are searched and named, not refused",
			change: func(f *fixture) {
				set(f.ks, map[string]any{"substitute": map[string]any{"x": "y"}}, "spec", "postBuild")
				set(f.ks, []any{map[string]any{"patch": "[]"}}, "spec", "patches")
				set(f.ks, "", "spec", "namePrefix")
			},
			path:       soFile,
			transforms: []string{"patches", "postBuild"},
		},
		{
			name:   "Helm-managed",
			change: func(f *fixture) { f.target.SetLabels(map[string]string{sourcemap.LabelHelmReleaseName: "orders"}) },
			reason: sourcemap.ReasonHelmManaged,
		},
		{
			name:   "no Flux labels",
			change: func(f *fixture) { f.target.SetLabels(nil) },
			reason: sourcemap.ReasonNoFluxLabels,
		},
		{
			name: "invalid Flux labels",
			change: func(f *fixture) {
				f.target.SetLabels(map[string]string{sourcemap.LabelKustomizationName: "../x", sourcemap.LabelKustomizationNamespace: "flux-system"})
			},
			reason: sourcemap.ReasonInvalidFluxLabels,
		},
		{
			name: "labels name another Kustomization",
			change: func(f *fixture) {
				f.target.SetLabels(map[string]string{sourcemap.LabelKustomizationName: "other", sourcemap.LabelKustomizationNamespace: "flux-system"})
			},
			reason: sourcemap.ReasonInvalidInput,
		},
		{
			name:   "no Kustomization",
			change: func(f *fixture) { f.ks = nil },
			reason: sourcemap.ReasonInvalidInput,
		},
		{
			name:   "invalid workload name",
			change: func(f *fixture) { f.in.Target.Name = "Orders/Worker" },
			reason: sourcemap.ReasonInvalidInput,
		},
		{
			name:   "not in inventory",
			change: func(f *fixture) { set(f.ks, []any{}, "status", "inventory", "entries") },
			reason: sourcemap.ReasonNotInInventory,
		},
		{
			name: "remote cluster",
			change: func(f *fixture) {
				set(f.ks, map[string]any{"secretRef": map[string]any{"name": "k"}}, "spec", "kubeConfig")
			},
			reason: sourcemap.ReasonRemoteCluster,
		},
		{
			name:   "source is not a GitRepository",
			change: func(f *fixture) { set(f.ks, "OCIRepository", "spec", "sourceRef", "kind") },
			reason: sourcemap.ReasonUnsupportedSource,
		},
		{
			name:   "path outside the repository",
			change: func(f *fixture) { set(f.ks, "../outside", "spec", "path") },
			reason: sourcemap.ReasonInvalidPath,
		},
		{
			name:   "path is not a string",
			change: func(f *fixture) { set(f.ks, int64(1), "spec", "path") },
			reason: sourcemap.ReasonInvalidPath,
		},
		{
			name:   "two documents declare the target",
			change: func(f *fixture) { f.files["apps/orders/deployment.yaml"] = deploymentYAML + "---\n" + scaledObjectYAML },
			reason: sourcemap.ReasonAmbiguousDeclaration,
		},
		{
			name: "no document declares the target",
			change: func(f *fixture) {
				f.files[soFile] = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  namespace: orders\n"
			},
			reason: sourcemap.ReasonDeclarationNotFound,
		},
		{
			name:   "a listed source file is missing",
			change: func(f *fixture) { delete(f.files, soFile) },
			reason: sourcemap.ReasonFileNotReadable,
		},
		{name: "no applied revision", revision: func(string) string { return "" }, reason: sourcemap.ReasonRevisionMismatch},
		{name: "branch only", revision: func(string) string { return "main" }, reason: sourcemap.ReasonRevisionMismatch},
		{
			name:     "abbreviated commit",
			revision: func(c string) string { return "main@sha1:" + c[:12] },
			reason:   sourcemap.ReasonRevisionMismatch,
		},
		{
			name:     "upper-case commit",
			revision: func(c string) string { return "main@sha1:" + strings.ToUpper(c) },
			reason:   sourcemap.ReasonRevisionMismatch,
		},
		{
			name:     "older Flux format",
			revision: func(c string) string { return "main/" + c },
			reason:   sourcemap.ReasonRevisionMismatch,
		},
		{
			name:     "unknown digest algorithm",
			revision: func(c string) string { return "main@md5:" + c },
			reason:   sourcemap.ReasonRevisionMismatch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			if tc.change != nil {
				tc.change(f)
			}
			git, head := gitOf(t, f)
			rev := applied(head)
			if tc.revision != nil {
				rev = tc.revision(head)
			}
			if f.ks != nil {
				set(f.ks, rev, "status", "lastAppliedRevision")
			}
			res, err := sourcemap.FindManifestCandidate(context.Background(),
				readsAt{git: git, commit: head, t: t}, candidateInput(t, f))
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if tc.reason != "" {
				if res.Candidate != nil || res.Reason != tc.reason || res.Message == "" {
					t.Fatalf("result = %+v, want reason %s with a message", res, tc.reason)
				}
				return
			}
			c := res.Candidate
			if c == nil || res.Reason != "" || res.Message != "" {
				t.Fatalf("result = %+v (%s), want a candidate", res, res.Message)
			}
			if c.Path != tc.path || c.Commit != head || !slices.Equal(c.Transforms, tc.transforms) {
				t.Errorf("candidate = %+v, want %s at %s with transforms %v", c, tc.path, head, tc.transforms)
			}
		})
	}
}

// TestFindManifestCandidateMatchesResolve checks that on plain layouts the candidate is the file Resolve
// maps the same target to, at the same commit.
//
//nolint:goconst // literal names keep the table readable
func TestFindManifestCandidateMatchesResolve(t *testing.T) {
	tests := []struct {
		name   string
		change func(f *fixture)
	}{
		{"ScaledObject", nil},
		{"Deployment", (*fixture).targetDeployment},
		{"Kustomization at the repository root", func(f *fixture) {
			set(f.ks, "", "spec", "path")
			f.files["kustomization.yaml"] = "resources:\n  - apps/orders\n"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			if tc.change != nil {
				tc.change(f)
			}
			res, _, err := f.run(t)
			if err != nil || res.Outcome != sourcemap.Resolved {
				t.Fatalf("Resolve = %+v, %v", res, err)
			}
			git, head := gitOf(t, f)
			if head != res.Source.BaseCommit {
				t.Fatalf("head %s, Resolve read %s", head, res.Source.BaseCommit)
			}
			set(f.ks, applied(head), "status", "lastAppliedRevision")
			got, err := sourcemap.FindManifestCandidate(context.Background(),
				readsAt{git: git, commit: head, t: t}, candidateInput(t, f))
			if err != nil || got.Candidate == nil {
				t.Fatalf("FindManifestCandidate = %+v, %v", got, err)
			}
			if got.Candidate.Path != string(res.Source.Path) || got.Candidate.Commit != res.Source.BaseCommit {
				t.Errorf("candidate %s at %s, Resolve %s at %s", got.Candidate.Path, got.Candidate.Commit,
					res.Source.Path, res.Source.BaseCommit)
			}
		})
	}
}

// TestFindManifestCandidateReadsTheAppliedCommit checks that the candidate comes from the commit the
// Kustomization applied, not the branch head: the declaration moved to another file after it.
func TestFindManifestCandidateReadsTheAppliedCommit(t *testing.T) {
	f := newFixture()
	git, old := gitOf(t, f)
	head, err := git.PushFiles("main", map[string][]byte{
		ksFile:                   []byte("resources:\n  - scaledobject.yaml\n  - moved.yaml\n"),
		soFile:                   []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  namespace: orders\n"),
		"apps/orders/moved.yaml": []byte(scaledObjectYAML),
	})
	if err != nil {
		t.Fatal(err)
	}
	if head == old {
		t.Fatal("the push did not move the head")
	}
	for _, tc := range []struct{ commit, path string }{{old, soFile}, {head, "apps/orders/moved.yaml"}} {
		set(f.ks, applied(tc.commit), "status", "lastAppliedRevision")
		res, err := sourcemap.FindManifestCandidate(context.Background(),
			readsAt{git: git, commit: tc.commit, t: t}, candidateInput(t, f))
		if err != nil || res.Candidate == nil {
			t.Fatalf("at %s: %+v, %v", tc.commit, res, err)
		}
		if res.Candidate.Path != tc.path || res.Candidate.Commit != tc.commit {
			t.Errorf("applied %s: candidate %s at %s, want %s", tc.commit, res.Candidate.Path, res.Candidate.Commit, tc.path)
		}
	}
}

// TestFindManifestCandidateReturnsGitErrors checks that a failed Git read is an error without provider text,
// never a reason such as "not found".
func TestFindManifestCandidateReturnsGitErrors(t *testing.T) {
	const secret = "ghs_s3cr3tT0ken"
	leaky := errors.New("GET https://x-access-token:" + secret + "@github.com/example/fleet.git: 502 Bad Gateway")
	failOn := func(op string, err error) func(string) error {
		return func(got string) error {
			if got == op {
				return err
			}
			return nil
		}
	}
	tests := []struct {
		name   string
		change func(f *fixture)
		fail   func(string) error
		// unknown applies a commit the host does not have.
		unknown bool
		want    error
	}{
		{name: "file read fails", fail: failOn("ReadFile", &providers.RetryableError{RetryAfter: time.Second, Err: leaky}),
			want: sourcemap.ErrTransient},
		{name: "listing fails", change: func(f *fixture) { delete(f.files, ksFile) },
			fail: failOn("ListDir", leaky), want: sourcemap.ErrTransient},
		{name: "read refused for good", fail: failOn("ReadFile", errors.Join(leaky, providers.ErrForbidden)),
			want: providers.ErrForbidden},
		{name: "applied commit unknown to the host", unknown: true, want: sourcemap.ErrTransient},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			if tc.change != nil {
				tc.change(f)
			}
			git, commit := gitOf(t, f)
			if tc.unknown {
				commit = strings.Repeat("ab", 20)
			}
			set(f.ks, applied(commit), "status", "lastAppliedRevision")
			if tc.fail != nil {
				git.SetFail(tc.fail)
			}
			res, err := sourcemap.FindManifestCandidate(context.Background(),
				readsAt{git: git, commit: commit, t: t}, candidateInput(t, f))
			if !errors.Is(err, tc.want) {
				t.Fatalf("result %+v, error = %v, want %v", res, err, tc.want)
			}
			if errors.Is(err, leaky) || strings.Contains(err.Error(), secret) {
				t.Errorf("error %q carries the provider error", err)
			}
			if res.Candidate != nil || res.Reason != "" {
				t.Errorf("result %+v returned with an error", res)
			}
		})
	}
}
