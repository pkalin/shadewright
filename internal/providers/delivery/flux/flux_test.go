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

package flux_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/delivery/flux"
)

const (
	revOld = "0e2d4c6b8a9f7e5d3c1b0a9f8e7d6c5b4a3f2e1d"
	revNew = "1f3c5e7a9b2d4f6081a3c5e7f9b1d3f5a7c9e1b3"

	kustomization = "Kustomization"
	buildFailed   = "kustomization-build-failed.json"
	readyFile     = "kustomization-ready.json"
	mainBranch    = "main"
	helmRelease   = "HelmRelease"
)

// recorded reads an object recorded for the Flux module, as the API reader decodes it (integers as int64).
func recorded(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "modules", "flux", "testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(b); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return u
}

// reader serves one object, or fails with err. It records the reads.
type reader struct {
	obj   *unstructured.Unstructured
	err   error
	reads int
}

func (r *reader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	r.reads++
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.err != nil {
		return r.err
	}
	u := obj.(*unstructured.Unstructured)
	if r.obj == nil || r.obj.GroupVersionKind() != u.GroupVersionKind() ||
		r.obj.GetNamespace() != key.Namespace || r.obj.GetName() != key.Name {
		return apierrors.NewNotFound(schema.GroupResource{Group: u.GroupVersionKind().Group, Resource: "x"}, key.Name)
	}
	r.obj.DeepCopyInto(u)
	return nil
}

func (r *reader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("not used")
}

func unitOf(u *unstructured.Unstructured) providers.DeliveryUnit {
	return providers.DeliveryUnit{Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName()}
}

func status(t *testing.T, u *unstructured.Unstructured) providers.DeliveryStatus {
	t.Helper()
	v, err := flux.New(&reader{obj: u}, ancestry{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := v.Status(t.Context(), unitOf(u))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return st
}

// kind is the branch the recorded objects other than buildFailed track.
const kind = "cluster/kind"

func outcome(t *testing.T, st providers.DeliveryStatus, commit, branch string) providers.DeliveryOutcome {
	t.Helper()
	got, err := st.Outcome(commit, branch)
	if err != nil {
		t.Fatalf("Outcome(%s): %v", commit, err)
	}
	return got
}

// TestRecordedObjects checks the outcome for both commits of the recorded objects: applied, pending and
// failed.
func TestRecordedObjects(t *testing.T) {
	for _, tc := range []struct {
		file, branch        string
		forOld, forNew      providers.DeliveryOutcome
		ready               bool
		generation, observe int64
	}{
		// Applied: Ready for generation 1, last applied revision is the old commit.
		{readyFile, kind, providers.DeliveryApplied, providers.DeliveryPending, true, 1, 1},
		// Pending: a new generation not observed yet, health checks of the new commit running.
		{"kustomization-reconciling.json", kind, providers.DeliveryPending, providers.DeliveryPending, false, 2, 1},
		// Failed: the build of the new commit failed; the applied revision names it too, but it is not Ready.
		{buildFailed, mainBranch, providers.DeliveryPending, providers.DeliveryFailed, false, 3, 3},
		// Failed: the dry-run of the new commit failed; the old one stays applied, but the unit reports a
		// failure, so it is not Applied either.
		{"kustomization-invalid-manifest.json", kind, providers.DeliveryPending, providers.DeliveryFailed, false, 1, 1},
		// Failed: stalled on the new commit.
		{"kustomization-stalled.json", kind, providers.DeliveryPending, providers.DeliveryFailed, false, 4, 3},
		// Failed, but on the old commit: the dependency is missing.
		{"kustomization-dependency-missing.json", kind, providers.DeliveryFailed, providers.DeliveryPending, false, 1, 1},
		// A HelmRelease's revisions are chart versions: no commit is ever Applied or Failed.
		{"helmrelease-upgrade-failed.json", kind, providers.DeliveryPending, providers.DeliveryPending, false, 5, 5},
	} {
		t.Run(tc.file, func(t *testing.T) {
			st := status(t, recorded(t, tc.file))
			if st.Ready != tc.ready || st.Generation != tc.generation || st.ObservedGeneration != tc.observe {
				t.Errorf("Ready, Generation, ObservedGeneration = %v, %d, %d; want %v, %d, %d",
					st.Ready, st.Generation, st.ObservedGeneration, tc.ready, tc.generation, tc.observe)
			}
			if got := outcome(t, st, revOld, tc.branch); got != tc.forOld {
				t.Errorf("outcome for the old commit = %s, want %s", got, tc.forOld)
			}
			if got := outcome(t, st, revNew, tc.branch); got != tc.forNew {
				t.Errorf("outcome for the new commit = %s, want %s", got, tc.forNew)
			}
		})
	}
}

func TestStatusFields(t *testing.T) {
	st := status(t, recorded(t, "kustomization-invalid-manifest.json"))
	want := providers.DeliveryStatus{
		AppliedRevision: kind + "@sha1:" + revOld, AppliedCommit: revOld, AppliedRef: kind,
		AttemptedCommit: revNew, AttemptedRef: kind,
		Failed: true, Generation: 1, ObservedGeneration: 1,
		Message: "Service/demo/web dry-run failed: failed to create typed patch object (demo/web; /v1, Kind=Service): " +
			".spec.ports[0].port: expected numeric (int or float), got string",
	}
	if st != want {
		t.Errorf("Status = %+v\nwant %+v", st, want)
	}
	hr := status(t, recorded(t, "helmrelease-upgrade-failed.json"))
	if hr.AppliedCommit != "" || hr.AttemptedCommit != "" || !hr.Failed {
		t.Errorf("HelmRelease status = %+v", hr)
	}
}

// TestNeverAppliedOnBadInput changes one field of an applied Kustomization at a time; none may be Applied.
func TestNeverAppliedOnBadInput(t *testing.T) {
	cond := func(u *unstructured.Unstructured) map[string]any {
		return u.Object["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)
	}
	st := func(u *unstructured.Unstructured) map[string]any { return u.Object["status"].(map[string]any) }
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{"spec changed since", func(u *unstructured.Unstructured) { u.SetGeneration(2) }},
		{"no generation", func(u *unstructured.Unstructured) {
			delete(u.Object["metadata"].(map[string]any), "generation")
			st(u)["observedGeneration"] = int64(0)
			cond(u)["observedGeneration"] = int64(0)
		}},
		{"no observed generation", func(u *unstructured.Unstructured) { delete(st(u), "observedGeneration") }},
		{"Ready about an older generation", func(u *unstructured.Unstructured) { cond(u)["observedGeneration"] = int64(0) }},
		{"Ready Unknown", func(u *unstructured.Unstructured) { cond(u)["status"] = "Unknown" }},
		{"stalled too", func(u *unstructured.Unstructured) {
			st(u)["conditions"] = append(st(u)["conditions"].([]any),
				map[string]any{"type": "Stalled", "status": "True", "reason": "InvalidPath"})
		}},
		{"revision in upper case", func(u *unstructured.Unstructured) {
			st(u)["lastAppliedRevision"] = "cluster/kind@sha1:" + strings.ToUpper(revOld)
		}},
		{"abbreviated revision", func(u *unstructured.Unstructured) {
			st(u)["lastAppliedRevision"] = "cluster/kind@sha1:" + revOld[:12]
		}},
		{"revision in the old format", func(u *unstructured.Unstructured) {
			st(u)["lastAppliedRevision"] = "cluster/kind/" + revOld
		}},
		{"revision not a string", func(u *unstructured.Unstructured) { st(u)["lastAppliedRevision"] = int64(1) }},
		{"source not a GitRepository", func(u *unstructured.Unstructured) {
			u.Object["spec"].(map[string]any)["sourceRef"].(map[string]any)["kind"] = "OCIRepository"
		}},
		{"no source", func(u *unstructured.Unstructured) { delete(u.Object["spec"].(map[string]any), "sourceRef") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := recorded(t, readyFile)
			tc.mutate(u)
			if got := outcome(t, status(t, u), revOld, kind); got == providers.DeliveryApplied {
				t.Error("outcome = Applied")
			}
		})
	}
}

// TestHealthCheckCanceledIsNoFailure checks that a cancelled health check of the commit is still pending.
func TestHealthCheckCanceledIsNoFailure(t *testing.T) {
	u := recorded(t, buildFailed)
	u.Object["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["reason"] = "HealthCheckCanceled"
	st := status(t, u)
	if st.Failed {
		t.Error("Failed = true")
	}
	if got := outcome(t, st, revNew, mainBranch); got != providers.DeliveryPending {
		t.Errorf("outcome = %s, want Pending", got)
	}
}

func TestMessageIsBounded(t *testing.T) {
	u := recorded(t, readyFile)
	u.Object["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["message"] =
		strings.Repeat("é", flux.MaxMessageBytes) + "\xff"
	msg := status(t, u).Message
	if len(msg) > flux.MaxMessageBytes || !utf8.ValidString(msg) || msg == "" {
		t.Errorf("message of %d bytes, valid UTF-8 %v", len(msg), utf8.ValidString(msg))
	}
}

func TestInvalidUnitIsNotRead(t *testing.T) {
	r := &reader{obj: recorded(t, readyFile)}
	v, err := flux.New(r, ancestry{})
	if err != nil {
		t.Fatal(err)
	}
	good := unitOf(r.obj)
	for _, u := range []providers.DeliveryUnit{
		{Kind: "GitRepository", Namespace: good.Namespace, Name: good.Name},
		{Kind: "kustomization", Namespace: good.Namespace, Name: good.Name},
		{Kind: kustomization, Namespace: "", Name: good.Name},
		{Kind: kustomization, Namespace: "a/b", Name: good.Name},
		{Kind: kustomization, Namespace: good.Namespace, Name: ""},
		{Kind: kustomization, Namespace: good.Namespace, Name: "../x"},
	} {
		if _, err := v.Status(t.Context(), u); !errors.Is(err, flux.ErrInvalidUnit) {
			t.Errorf("Status(%+v) error = %v, want ErrInvalidUnit", u, err)
		}
	}
	if r.reads != 0 {
		t.Errorf("%d reads, want none", r.reads)
	}
}

func TestReadErrors(t *testing.T) {
	gr := schema.GroupResource{Group: "kustomize.toolkit.fluxcd.io", Resource: "kustomizations"}
	for _, tc := range []struct {
		name      string
		err       error
		want      error
		retryable bool
	}{
		{"not found", apierrors.NewNotFound(gr, "x"), providers.ErrNotFound, false},
		{"kind not served", &meta.NoKindMatchError{GroupKind: gr.WithVersion("v1").GroupVersion().WithKind(kustomization).GroupKind()},
			providers.ErrNotFound, false},
		{"forbidden", apierrors.NewForbidden(gr, "x", errors.New("no")), providers.ErrForbidden, false},
		{"unauthorized", apierrors.NewUnauthorized("no"), providers.ErrForbidden, false},
		{"server error", apierrors.NewInternalError(errors.New("boom")), providers.ErrRetryable, true},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), providers.ErrRetryable, true},
		{"other", errors.New("connection reset"), providers.ErrRetryable, true},
		{"deadline", context.DeadlineExceeded, context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := flux.New(&reader{err: tc.err}, ancestry{})
			if err != nil {
				t.Fatal(err)
			}
			st, err := v.Status(t.Context(), providers.DeliveryUnit{Kind: kustomization, Namespace: "flux-system", Name: "apps"})
			if !errors.Is(err, tc.want) || errors.Is(err, providers.ErrRetryable) != tc.retryable {
				t.Errorf("error = %v, want %v (retryable %v)", err, tc.want, tc.retryable)
			}
			if st != (providers.DeliveryStatus{}) {
				t.Errorf("status = %+v, want none", st)
			}
			if strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "slow down") {
				t.Errorf("error repeats the server's text: %v", err)
			}
		})
	}
}

func TestCancelledContext(t *testing.T) {
	r := &reader{obj: recorded(t, readyFile)}
	v, err := flux.New(r, ancestry{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	st, err := v.Status(ctx, unitOf(r.obj))
	if !errors.Is(err, context.Canceled) || st != (providers.DeliveryStatus{}) || r.reads != 0 {
		t.Errorf("Status = %+v, %v after %d reads; want context.Canceled and no read", st, err, r.reads)
	}
}

// otherObject answers every read with an object of another name.
type otherObject struct{ reader }

func (o *otherObject) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := o.reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	obj.SetName("someone-else")
	return nil
}

func TestAnotherObjectIsNotReported(t *testing.T) {
	u := recorded(t, readyFile)
	v, err := flux.New(&otherObject{reader{obj: u}}, ancestry{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := v.Status(t.Context(), unitOf(u))
	if !errors.Is(err, providers.ErrRetryable) || st != (providers.DeliveryStatus{}) {
		t.Errorf("Status = %+v, %v; want a retryable error and no status", st, err)
	}
}

func TestNewNeedsAReader(t *testing.T) {
	if _, err := flux.New(nil, ancestry{}); !errors.Is(err, providers.ErrInvalidConfig) {
		t.Errorf("New(nil, ...) error = %v, want ErrInvalidConfig", err)
	}
	if _, err := flux.New(&reader{}, nil); !errors.Is(err, providers.ErrInvalidConfig) {
		t.Errorf("New(..., nil) error = %v, want ErrInvalidConfig", err)
	}
}

// ancestry answers from a set of (ancestor, commit) pairs, or fails with err. It records the calls.
type ancestry struct {
	pairs map[[2]string]bool
	err   error
	calls *int
}

func (a ancestry) IsAncestor(_ context.Context, ancestor, commit string) (bool, error) {
	if a.calls != nil {
		*a.calls++
	}
	if a.err != nil {
		return false, a.err
	}
	return ancestor == commit || a.pairs[[2]string{ancestor, commit}], nil
}

// TestVerifyLaterCommit checks a Kustomization that applied the old commit, Ready, against a merge commit
// that is not the applied one: Applied only when the Git host says the applied commit descends from it.
func TestVerifyLaterCommit(t *testing.T) {
	merge := strings.Repeat("9", 40)
	for _, tc := range []struct {
		name  string
		anc   ancestry
		want  providers.DeliveryOutcome
		error bool
	}{
		{"descendant applied", ancestry{pairs: map[[2]string]bool{{merge, revOld}: true}}, providers.DeliveryApplied, false},
		{"unrelated commit applied", ancestry{}, providers.DeliveryPending, false},
		{"ancestry unknown", ancestry{err: &providers.RetryableError{Err: errors.New("compare failed")}},
			providers.DeliveryPending, true},
		{"commit unknown to the host", ancestry{err: providers.ErrNotFound}, providers.DeliveryPending, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			tc.anc.calls = &calls
			u := recorded(t, readyFile)
			v, err := flux.New(&reader{obj: u}, tc.anc)
			if err != nil {
				t.Fatal(err)
			}
			got, st, err := v.Verify(t.Context(), unitOf(u), merge, kind)
			if got != tc.want || (err != nil) != tc.error || calls != 1 {
				t.Errorf("Verify = %s, %v after %d ancestry calls; want %s, error %v, one call", got, err, calls, tc.want, tc.error)
			}
			if st.AppliedCommit != revOld {
				t.Errorf("status = %+v", st)
			}
		})
	}
}

// TestVerifyAsksNoAncestryWhenNotNeeded checks the fast path (the merge commit itself applied) and the units
// that cannot be Applied whatever the history says.
func TestVerifyAsksNoAncestryWhenNotNeeded(t *testing.T) {
	for _, tc := range []struct {
		file, commit, branch string
		want                 providers.DeliveryOutcome
	}{
		{readyFile, revOld, kind, providers.DeliveryApplied},
		{readyFile, revOld, mainBranch, providers.DeliveryPending},
		{readyFile, strings.Repeat("9", 40), mainBranch, providers.DeliveryPending},
		{"kustomization-reconciling.json", revNew, kind, providers.DeliveryPending},
		{buildFailed, revNew, mainBranch, providers.DeliveryFailed},
		{buildFailed, revNew, kind, providers.DeliveryPending},
		{buildFailed, strings.Repeat("9", 40), kind, providers.DeliveryPending},
		{"helmrelease-upgrade-failed.json", revNew, mainBranch, providers.DeliveryPending},
	} {
		calls := 0
		u := recorded(t, tc.file)
		v, err := flux.New(&reader{obj: u}, ancestry{err: errors.New("must not be asked"), calls: &calls})
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := v.Verify(t.Context(), unitOf(u), tc.commit, tc.branch)
		if got != tc.want || err != nil || calls != 0 {
			t.Errorf("%s: Verify = %s, %v after %d ancestry calls; want %s and none", tc.file, got, err, calls, tc.want)
		}
	}
}

func TestVerifyReadError(t *testing.T) {
	v, err := flux.New(&reader{err: apierrors.NewNotFound(schema.GroupResource{}, "x")}, ancestry{})
	if err != nil {
		t.Fatal(err)
	}
	got, st, err := v.Verify(t.Context(), providers.DeliveryUnit{Kind: kustomization, Namespace: "flux-system", Name: "apps"},
		revOld, mainBranch)
	if !errors.Is(err, providers.ErrNotFound) || got != "" || st != (providers.DeliveryStatus{}) {
		t.Errorf("Verify = %q, %+v, %v", got, st, err)
	}
}

func TestHelmReleaseIsRead(t *testing.T) {
	u := recorded(t, "helmrelease-upgrade-failed.json")
	if u.GetKind() != helmRelease {
		t.Fatalf("kind %s", u.GetKind())
	}
	st := status(t, u)
	if st.AppliedRevision != "" || st.Ready {
		t.Errorf("status = %+v", st)
	}
}

// TestVerifyCommitOnlyRevision checks Flux's sha1:<commit> revision, which names no branch: the commit is
// still compared, exactly or through ancestry.
func TestVerifyCommitOnlyRevision(t *testing.T) {
	merge := strings.Repeat("9", 40)
	for _, tc := range []struct {
		name, commit string
		anc          ancestry
		want         providers.DeliveryOutcome
	}{
		{"the merge commit", revOld, ancestry{err: errors.New("must not be asked")}, providers.DeliveryApplied},
		{"a later commit", merge, ancestry{pairs: map[[2]string]bool{{merge, revOld}: true}}, providers.DeliveryApplied},
		{"an unrelated commit", merge, ancestry{}, providers.DeliveryPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := recorded(t, readyFile)
			u.Object["status"].(map[string]any)["lastAppliedRevision"] = "sha1:" + revOld
			v, err := flux.New(&reader{obj: u}, tc.anc)
			if err != nil {
				t.Fatal(err)
			}
			got, st, err := v.Verify(t.Context(), unitOf(u), tc.commit, mainBranch)
			if got != tc.want || err != nil || st.AppliedRef != "" || st.AppliedCommit != revOld {
				t.Errorf("Verify = %s, %+v, %v; want %s", got, st, err, tc.want)
			}
		})
	}
}

// TestVerifyLaterFailure checks that a failing attempt of a later commit holding the merge commit is Failed.
func TestVerifyLaterFailure(t *testing.T) {
	merge := strings.Repeat("9", 40)
	u := recorded(t, buildFailed)
	v, err := flux.New(&reader{obj: u}, ancestry{pairs: map[[2]string]bool{{merge, revNew}: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := v.Verify(t.Context(), unitOf(u), merge, mainBranch); got != providers.DeliveryFailed || err != nil {
		t.Errorf("Verify = %s, %v; want Failed", got, err)
	}
}
