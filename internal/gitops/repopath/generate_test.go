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

package repopath_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/gitops/repopath"
	"github.com/pkalin/shadewright/internal/providers"
)

// tree is a listing per directory.
type tree map[string][]providers.DirEntry

func (tr tree) list(_ context.Context, dir string) ([]providers.DirEntry, error) {
	entries, ok := tr[dir]
	if !ok {
		return nil, fmt.Errorf("directory %s: %w", dir, providers.ErrPathNotInTree)
	}
	return entries, nil
}

func file(name string) providers.DirEntry {
	return providers.DirEntry{Name: name, Type: providers.DirEntryFile}
}
func dir(name string) providers.DirEntry {
	return providers.DirEntry{Name: name, Type: providers.DirEntryDir}
}

func TestGenerate(t *testing.T) {
	tr := tree{
		// Sorted by name, as a GitProvider lists.
		orders: {file(".gitkeep"), dir(".github"), dir(".hidden"), file("a.yaml"), dir("assets.zip"), dir("base"),
			file("cloudbuild.yaml"), file("logo.png"), file("notes.txt"), file("run.sh"), dir("sub"), file("z.yml")},
		orders + "/.hidden": {file("README.md")},
		// A base is built from its own kustomization file: its other entries are not checked, hidden or not.
		orders + "/base":      {file(".env.yaml"), file("ignored.yaml"), file("kustomization.yaml"), dir("x:y")},
		orders + "/sub":       {file(".sops.yaml"), file("b.yaml"), dir("empty")},
		orders + "/sub/empty": nil,
	}
	got, err := repopath.Generate(context.Background(), orders, tr.list)
	want := []repopath.Resource{
		{Path: orders + "/a.yaml"}, {Path: orders + "/base", Dir: true, Depth: 1}, {Path: orders + "/sub/b.yaml"},
		{Path: orders + "/z.yml"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Generate = %v, %v, want %v", got, err, want)
	}
}

// Flux's default exclusions, as fluxcd/pkg sourceignore defines them: matched names are left out at any
// depth, directory patterns only for directories.
func TestFluxExcludes(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  bool
		want bool
	}{
		{".git", true, true}, {".gitignore", false, true}, {".gitmodules", false, true}, {".gitattributes", false, true},
		{"a.jpg", false, true}, {"a.jpeg", false, true}, {"a.gif", false, true}, {"a.png", false, true},
		{"a.wmv", false, true}, {"a.flv", false, true}, {"a.tar.gz", false, true}, {"a.zip", true, true},
		{".github", true, true}, {".circleci", true, true}, {".travis.yml", false, true}, {".gitlab-ci.yml", false, true},
		{"appveyor.yml", false, true}, {".drone.yml", false, true}, {"cloudbuild.yaml", false, true},
		{"codeship-services.yml", false, true}, {"codeship-steps.yml", false, true}, {".goreleaser.yml", false, true},
		{".sops.yaml", false, true}, {".flux.yaml", false, true},
		{".github", false, false}, {".git", false, false}, {"a.yaml", false, false}, {"a.PNG", false, false},
		{"zip", false, false}, {"github", true, false},
	} {
		if got := repopath.FluxExcludes(tc.name, tc.dir); got != tc.want {
			t.Errorf("FluxExcludes(%q, dir %v) = %v, want %v", tc.name, tc.dir, got, tc.want)
		}
	}
}

func TestGenerateStops(t *testing.T) {
	deep := tree{}
	d := orders
	for range repopath.MaxListDepth + 1 {
		deep[d] = []providers.DirEntry{dir("d")}
		d += "/d"
	}
	deep[d] = []providers.DirEntry{file("x.yaml")}
	atLimit := tree{}
	d = orders
	for range repopath.MaxListDepth {
		atLimit[d] = []providers.DirEntry{dir("d")}
		d += "/d"
	}
	atLimit[d] = []providers.DirEntry{file("x.yaml")}
	if got, err := repopath.Generate(context.Background(), orders, atLimit.list); err != nil || len(got) != 1 {
		t.Fatalf("a manifest MaxListDepth deep = %v, %v, want it listed", got, err)
	}

	for name, tc := range map[string]struct {
		tr   tree
		want repopath.StopKind
	}{
		"too deep":                 {deep, repopath.StopTooDeep},
		"hidden manifest":          {tree{orders: {file(".env.yaml")}}, repopath.StopInvalidName},
		"manifest in hidden dir":   {tree{orders: {dir(".config")}, orders + "/.config": {file("a.yaml")}}, repopath.StopInvalidName},
		"base in hidden dir":       {tree{orders: {dir(".b")}, orders + "/.b": {file("kustomization.yaml")}}, repopath.StopInvalidName},
		"unlistable directory":     {tree{orders: {dir("a\tb")}}, repopath.StopInvalidName},
		"dot dot directory":        {tree{orders: {dir("..")}}, repopath.StopInvalidName},
		"ambiguous":                {tree{orders: {file("a..b.yaml")}}, repopath.StopInvalidName},
		"colon":                    {tree{orders: {file("https:x.yaml")}}, repopath.StopInvalidName},
		"unknown type":             {tree{orders: {{Name: "l.yaml", Type: "Symlink"}}}, repopath.StopInvalidName},
		"sourceignore":             {tree{orders: {file("a.yaml"), dir("s")}, orders + "/s": {file(".sourceignore")}}, repopath.StopSourceIgnore},
		"sourceignore in a base":   {tree{orders: {dir("b")}, orders + "/b": {file(".sourceignore"), file("kustomization.yaml")}}, repopath.StopSourceIgnore},
		"sourceignore in a hidden": {tree{orders: {dir(".h")}, orders + "/.h": {file(".sourceignore")}}, repopath.StopSourceIgnore},
		"too long": {tree{orders: {dir(strings.Repeat("a", 200))}, orders + "/" + strings.Repeat("a", 200): {
			dir(strings.Repeat("b", 200))}, orders + "/" + strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200): {
			file(strings.Repeat("c", 200) + ".yaml")}}, repopath.StopInvalidName},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := repopath.Generate(context.Background(), orders, tc.tr.list)
			s, ok := errors.AsType[*repopath.Stop](err)
			if !ok || s.Kind != tc.want {
				t.Fatalf("Generate = %v, want stop %d", err, tc.want)
			}
			for _, leak := range []string{".env", "https", ".config", ".b", ".h"} {
				if strings.Contains(s.Error(), leak+"/") || strings.Contains(s.Error(), leak+" ") {
					t.Errorf("stop quotes a listed name: %s", s.Error())
				}
			}
		})
	}

	// A lister error is returned as it is.
	boom := errors.New("boom")
	if _, err := repopath.Generate(context.Background(), orders, func(context.Context, string) ([]providers.DirEntry, error) {
		return nil, boom
	}); !errors.Is(err, boom) {
		t.Errorf("lister error = %v, want it returned", err)
	}
}

// A plain not found is not a permanent answer: the host may not have the commit yet, or be failing.
func TestCheckListing(t *testing.T) {
	for _, tc := range []struct {
		err  error
		n    int
		want repopath.ListFault
	}{
		{nil, 3, repopath.ListOK},
		{nil, providers.MaxDirEntries + 1, repopath.ListTooLarge},
		{fmt.Errorf("x: %w", providers.ErrPathNotInTree), 0, repopath.ListNotADirectory},
		{fmt.Errorf("GitHub returned status 404: %w", providers.ErrNotFound), 0, repopath.ListFailed},
		{fmt.Errorf("x: %w", providers.ErrTooLarge), 0, repopath.ListTooLarge},
		{fmt.Errorf("x: %w", providers.ErrInvalidEntry), 0, repopath.ListInvalidEntry},
		{context.Canceled, 0, repopath.ListFailed},
	} {
		if got := repopath.CheckListing(make([]providers.DirEntry, tc.n), tc.err); got != tc.want {
			t.Errorf("CheckListing(%d entries, %v) = %d, want %d", tc.n, tc.err, got, tc.want)
		}
	}
}

func TestNestDepth(t *testing.T) {
	for _, tc := range []struct {
		dir, p string
		want   int
	}{
		{orders, orders + "/base", 1}, {orders, orders + "/a/b/c", 3}, {".", "apps/orders", 2},
		{orders, "apps/web", 1}, {orders, "apps/orders2/x", 1}, {".", "x", 1},
	} {
		if got := repopath.NestDepth(tc.dir, tc.p); got != tc.want {
			t.Errorf("NestDepth(%s, %s) = %d, want %d", tc.dir, tc.p, got, tc.want)
		}
	}
}
