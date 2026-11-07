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

package semdiff_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

// readme is a file outside every build.
const readme = "unrelated/README.md"

// plainOrders is apps/orders without a kustomization file: Flux generates one from its manifests (#122).
func plainOrders() map[string][]byte {
	return map[string][]byte{
		soFile:                                []byte(scaledObjectYAML),
		"apps/orders/notes.txt":               []byte("not a manifest"),
		"apps/orders/web/service.yml":         []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n  namespace: orders\n"),
		"apps/orders/base/kustomization.yaml": kust("cm.yaml"),
		"apps/orders/base/cm.yaml":            []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: base\n  namespace: orders\n"),
		// Not listed by the base's kustomization file, so neither Flux nor the check builds it.
		"apps/orders/base/unused.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unused\n  namespace: orders\n"),
		readme:                         []byte("not part of the build"),
	}
}

// A path without a kustomization file is rendered as Flux renders it, from the kustomization Flux generates:
// its manifests at any depth and its nested bases, and nothing else.
func TestLoadGeneratesTheKustomizationFluxWould(t *testing.T) {
	files := plainOrders()
	git := fake.NewGit("main", files)
	got, err := semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- base\n- scaledobject.yaml\n- web/service.yml\n"
	if string(got["apps/orders/kustomization.yaml"]) != want {
		t.Fatalf("generated kustomization =\n%s\nwant\n%s", got["apps/orders/kustomization.yaml"], want)
	}
	if _, ok := got["apps/orders/base/unused.yaml"]; ok {
		t.Error("a file the base does not list was loaded")
	}
	res, err := semdiff.Check(context.Background(), soInput(t, got, "apps/orders", 15))
	if err != nil {
		t.Fatalf("Check of the generated build: %v", err)
	}
	// Service and ScaledObject from scaledobject.yaml, the Service in web/ and the base's ConfigMap.
	if res.Objects != 4 {
		t.Errorf("rendered %d objects, want 4", res.Objects)
	}
}

// Names that YAML would read as another type stay strings in the generated kustomization.
func TestLoadGeneratedKustomizationQuotesNames(t *testing.T) {
	// A base named true is listed as the resource "true", which unquoted YAML reads as a boolean.
	files := map[string][]byte{
		"apps/orders/true/kustomization.yaml": kust("scaledobject.yaml"),
		"apps/orders/true/scaledobject.yaml":  []byte(scaledObjectYAML),
	}
	git := fake.NewGit("main", files)
	got, err := semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(string(got["apps/orders/kustomization.yaml"]), `- "true"`) {
		t.Fatalf("generated kustomization = %s", got["apps/orders/kustomization.yaml"])
	}
	in := soInput(t, map[string][]byte{soFile: []byte(scaledObjectYAML)}, "apps/orders", 15)
	in.Files, in.File = got, "apps/orders/true/scaledobject.yaml"
	if _, err := semdiff.Check(context.Background(), in); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// The repository root is a path like any other.
func TestLoadGeneratesAtTheRoot(t *testing.T) {
	git := fake.NewGit("main", map[string][]byte{soFile: []byte(scaledObjectYAML)})
	got, err := semdiff.Load(context.Background(), git, head(t, git), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(string(got["kustomization.yaml"]), "- apps/orders/scaledobject.yaml") {
		t.Fatalf("generated kustomization = %s", got["kustomization.yaml"])
	}
	if _, err := semdiff.Check(context.Background(), soInput(t, got, "", 15)); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// errLister fails every listing.
type errLister struct {
	providers.GitProvider
	err error
}

func (e errLister) ListDir(context.Context, string, string) ([]providers.DirEntry, error) {
	return nil, e.err
}

func TestLoadGeneratedRefuses(t *testing.T) {
	with := func(extra map[string][]byte) map[string][]byte {
		files := plainOrders()
		maps.Copy(files, extra)
		return files
	}
	many := map[string][]byte{}
	for i := range providers.MaxDirEntries + 1 {
		many[fmt.Sprintf("apps/orders/big/f%d.txt", i)] = nil
	}
	for name, tc := range map[string]struct {
		files map[string][]byte
		want  *semdiff.Error
	}{
		"too deep": {with(map[string][]byte{
			"apps/orders" + strings.Repeat("/d", sourcemap.MaxListDepth+1) + "/x.txt": nil,
		}), semdiff.ErrTooLarge},
		"a directory over MaxDirEntries": {with(many), semdiff.ErrTooLarge},
		"a nested .sourceignore": {with(map[string][]byte{"apps/orders/web/.sourceignore": nil}),
			semdiff.ErrSourceIgnored},
		"a manifest in a hidden directory": {with(map[string][]byte{"apps/orders/.config/x.yaml": nil}),
			semdiff.ErrInvalidReference},
		"a hidden name":     {with(map[string][]byte{"apps/orders/.x.yaml": nil}), semdiff.ErrInvalidReference},
		"no such directory": {map[string][]byte{readme: nil}, semdiff.ErrFileMissing},
	} {
		t.Run(name, func(t *testing.T) {
			git := fake.NewGit("main", tc.files)
			_, err := semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
			wantReason(t, err, tc.want)
		})
	}

	// A host error is transient and carries no provider text.
	git := fake.NewGit("main", plainOrders())
	_, err := semdiff.Load(context.Background(), errLister{git, errors.New("https://token@host failed")}, head(t, git), "apps/orders")
	var te *sourcemap.TransientError
	if !errors.As(err, &te) || strings.Contains(err.Error(), "token") {
		t.Fatalf("got %v, want a TransientError without provider text", err)
	}
	// A plain not found (a 404 on a subtree, a token GitHub cannot give) is transient too, not FileMissing.
	_, err = semdiff.Load(context.Background(), errLister{git, fmt.Errorf("GitHub returned status 404: %w", providers.ErrNotFound)},
		head(t, git), "apps/orders")
	if !errors.As(err, &te) {
		t.Fatalf("plain not found: %v, want a TransientError", err)
	}
	// An entry no tree can hold is a permanent refusal.
	_, err = semdiff.Load(context.Background(), errLister{git, fmt.Errorf("listing: %w", providers.ErrInvalidEntry)},
		head(t, git), "apps/orders")
	wantReason(t, err, semdiff.ErrInvalidReference)
	// A refused listing stops the build too, as a transient error the caller classifies.
	_, err = semdiff.Load(context.Background(), errLister{git, context.Canceled}, head(t, git), "apps/orders")
	if !errors.As(err, &te) {
		t.Fatalf("cancelled listing: %v, want a TransientError", err)
	}
}

// Listings count against MaxReads and the byte budget, so many or wide directories are refused.
func TestLoadGeneratedCountsListings(t *testing.T) {
	files := plainOrders()
	for i := range semdiff.MaxReads {
		files[fmt.Sprintf("apps/orders/w%d/x.txt", i)] = nil
	}
	git := fake.NewGit("main", files)
	_, err := semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
	wantReason(t, err, semdiff.ErrTooLarge)
	if !strings.Contains(err.Error(), "reads") {
		t.Fatalf("got %v, want the read limit", err)
	}

	files = plainOrders()
	dirs := semdiff.MaxBuildBytes/(sourcemap.ListEntryBytes*providers.MaxDirEntries) + 1
	for d := range dirs {
		for i := range providers.MaxDirEntries {
			files[fmt.Sprintf("apps/orders/w%d/f%d.txt", d, i)] = nil
		}
	}
	git = fake.NewGit("main", files)
	_, err = semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
	wantReason(t, err, semdiff.ErrTooLarge)
	if !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("got %v, want the byte budget", err)
	}
}

// What Flux leaves out of its artifact is not rendered: a file there that would make the build differ (here a
// second copy of the target) is skipped, as Flux skips it (#122).
func TestLoadGeneratedSkipsWhatFluxExcludes(t *testing.T) {
	files := plainOrders()
	for _, p := range []string{"cloudbuild.yaml", ".github/ci.yaml", "assets.zip/so.yaml", "logo.png", ".gitkeep"} {
		files["apps/orders/"+p] = []byte(scaledObjectYAML)
	}
	git := fake.NewGit("main", files)
	got, err := semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := semdiff.Check(context.Background(), soInput(t, got, "apps/orders", 15)); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// A base directory whose name ends in .yaml is built as a directory, as kustomize builds it.
func TestLoadGeneratedKeepsDirectoryResources(t *testing.T) {
	files := map[string][]byte{
		"apps/orders/svc.yaml/kustomization.yaml": kust("scaledobject.yaml"),
		"apps/orders/svc.yaml/scaledobject.yaml":  []byte(scaledObjectYAML),
	}
	git := fake.NewGit("main", files)
	got, err := semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	in := soInput(t, map[string][]byte{soFile: []byte(scaledObjectYAML)}, "apps/orders", 15)
	in.Files, in.File = got, "apps/orders/svc.yaml/scaledobject.yaml"
	if _, err := semdiff.Check(context.Background(), in); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// A base found deep below the path counts the directories passed against MaxDepth, as in the source mapping.
func TestLoadGeneratedBoundsNesting(t *testing.T) {
	files := map[string][]byte{soFile: []byte(scaledObjectYAML)}
	chain := "apps/orders" + strings.Repeat("/d", sourcemap.MaxListDepth)
	for range 4 {
		files[chain+"/kustomization.yaml"] = kust("k")
		chain += "/k"
	}
	files[chain+"/kustomization.yaml"] = kust()
	git := fake.NewGit("main", files)
	_, err := semdiff.Load(context.Background(), git, head(t, git), "apps/orders")
	wantReason(t, err, semdiff.ErrTooLarge)
	if !strings.Contains(err.Error(), "nested") {
		t.Fatalf("got %v, want the depth limit", err)
	}
}
