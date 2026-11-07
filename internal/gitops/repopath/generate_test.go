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
		// Listed out of order: the walk sorts.
		orders:                {file("z.yml"), dir("base"), file("a.yaml"), file("notes.txt"), dir("sub"), file("run.sh")},
		orders + "/base":      {file("kustomization.yaml"), file("ignored.yaml"), dir("deeper")},
		orders + "/sub":       {file("b.yaml"), dir("empty")},
		orders + "/sub/empty": nil,
	}
	got, err := repopath.Generate(context.Background(), orders, tr.list)
	want := []repopath.Resource{
		{Path: orders + "/a.yaml"}, {Path: orders + "/base", Dir: true}, {Path: orders + "/sub/b.yaml"},
		{Path: orders + "/z.yml"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Generate = %v, %v, want %v", got, err, want)
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
		"too deep":            {deep, repopath.StopTooDeep},
		"hidden name":         {tree{orders: {file(".env.yaml")}}, repopath.StopInvalidName},
		"hidden directory":    {tree{orders: {dir(".config")}}, repopath.StopInvalidName},
		"dot dot":             {tree{orders: {dir("..")}}, repopath.StopInvalidName},
		"ambiguous":           {tree{orders: {file("a..b.yaml")}}, repopath.StopInvalidName},
		"slash":               {tree{orders: {file("a/b.yaml")}}, repopath.StopInvalidName},
		"colon":               {tree{orders: {file("https:x.yaml")}}, repopath.StopInvalidName},
		"empty":               {tree{orders: {file("")}}, repopath.StopInvalidName},
		"unknown type":        {tree{orders: {{Name: "l.yaml", Type: "Symlink"}}}, repopath.StopInvalidName},
		"sourceignore":        {tree{orders: {file("a.yaml"), dir("s")}, orders + "/s": {file(".sourceignore")}}, repopath.StopSourceIgnore},
		"flux default ignore": {tree{orders: {file("cloudbuild.yaml")}}, repopath.StopFluxIgnored},
		"in a nested base":    {tree{orders: {dir("b")}, orders + "/b": {file("kustomization.yaml"), dir(".github")}}, repopath.StopFluxIgnored},
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
			if strings.Contains(s.Error(), ".env") || strings.Contains(s.Error(), "https") {
				t.Errorf("stop quotes a listed name: %s", s.Error())
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
