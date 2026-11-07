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
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
)

// ordersDir is the Kustomization path of the fixture.
const ordersDir = "apps/orders"

// configMapX is a manifest that is not the target.
const configMapX = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"

// plain removes the kustomization file, so Flux generates one from every manifest in apps/orders (#122).
func plain(f *fixture) {
	delete(f.files, ksFile)
	f.files["apps/orders/notes.txt"] = "not a manifest\n"
}

// deep is a directory depth levels below apps/orders.
func deep(depth int) string {
	return ordersDir + strings.Repeat("/d", depth)
}

// orderTarget is the ScaledObject document alone, for a file of its own.
const orderTarget = `apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: orders-worker
  namespace: orders
spec:
  maxReplicaCount: 10
`

func TestResolveWithoutKustomizationFile(t *testing.T) {
	tests := []struct {
		name   string
		change func(f *fixture)
		path   string
	}{
		{"plain manifests in the path", plain, soFile},
		{"a manifest in a nested directory", func(f *fixture) {
			plain(f)
			delete(f.files, soFile)
			f.files["apps/orders/workers/so.yaml"] = orderTarget
		}, "apps/orders/workers/so.yaml"},
		{"a manifest MaxListDepth below the path", func(f *fixture) {
			plain(f)
			delete(f.files, soFile)
			f.files[deep(sourcemap.MaxListDepth)+"/so.yaml"] = orderTarget
		}, deep(sourcemap.MaxListDepth) + "/so.yaml"},
		{"a nested directory with a kustomization file is built as a base, not listed", func(f *fixture) {
			plain(f)
			delete(f.files, soFile)
			f.files["apps/orders/base/kustomization.yaml"] = "resources:\n  - so.yaml\n"
			f.files["apps/orders/base/so.yaml"] = orderTarget
			// Not listed in the base's kustomization file, so Flux does not apply it.
			f.files["apps/orders/base/unused.yaml"] = orderTarget
			// Nor are the base's other entries checked, as Flux builds it from its kustomization file.
			f.files["apps/orders/base/.env.yaml"] = orderTarget
		}, "apps/orders/base/so.yaml"},
		// What Flux leaves out of the artifact is skipped, so a second declaration there is not one (#122).
		{"names Flux excludes by default are skipped", func(f *fixture) {
			plain(f)
			for _, p := range []string{"cloudbuild.yaml", ".github/ci.yaml", "assets.zip/x.yaml", ".sops.yaml", "sub/.flux.yaml"} {
				f.files["apps/orders/"+p] = orderTarget
			}
		}, soFile},
		{"hidden files that are not manifests and hidden directories without manifests are skipped", func(f *fixture) {
			plain(f)
			f.files["apps/orders/.gitkeep"] = ""
			f.files["apps/orders/.cache/notes.txt"] = "x\n"
		}, soFile},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.change(f)
			res, head, err := f.run(t)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome != sourcemap.Resolved {
				t.Fatalf("result = %s/%s (%s), want Resolved", res.Outcome, res.Reason, res.Message)
			}
			if string(res.Source.Path) != tc.path || res.Source.BuildPath != ordersDir || res.Source.BaseCommit != head {
				t.Errorf("source = %+v, want %s built from apps/orders", res.Source, tc.path)
			}
			sameWithCache(t, tc.change, res)
		})
	}
}

// dirLister adds synthetic directories to what the fake lists, for breadth that a map of files would make slow.
type dirLister struct {
	providers.GitProvider
	// extra returns the entries to list for dir instead of the fake's, or false to ask the fake.
	extra func(ctx context.Context, commit, dir string) ([]providers.DirEntry, bool)
}

func (d dirLister) ListDir(ctx context.Context, commit, dir string) ([]providers.DirEntry, error) {
	if entries, ok := d.extra(ctx, commit, dir); ok {
		return entries, nil
	}
	return d.GitProvider.ListDir(ctx, commit, dir)
}

// wide lists n directories in apps/orders besides its files, each holding perDir entries of the given type
// (named f0.txt..., or d0... for directories, which are then empty).
func wide(n, perDir int, typ providers.DirEntryType) func(*fixture) {
	return func(f *fixture) {
		plain(f)
		f.gitWrap = func(g providers.GitProvider) providers.GitProvider {
			return dirLister{GitProvider: g, extra: func(ctx context.Context, commit, dir string) ([]providers.DirEntry, bool) {
				var out []providers.DirEntry
				switch {
				case dir == ordersDir:
					real, err := g.ListDir(ctx, commit, dir)
					if err != nil {
						panic(err)
					}
					out = append(out, real...)
					for i := range n {
						out = append(out, providers.DirEntry{Name: fmt.Sprintf("w%04d", i), Type: providers.DirEntryDir})
					}
				case strings.HasPrefix(dir, "apps/orders/w") && strings.Count(dir, "/") == 2:
					for i := range perDir {
						name := fmt.Sprintf("f%d.txt", i)
						if typ == providers.DirEntryDir {
							name = fmt.Sprintf("d%d", i)
						}
						out = append(out, providers.DirEntry{Name: name, Type: typ})
					}
				case strings.HasPrefix(dir, "apps/orders/w"):
					return nil, true
				default:
					return nil, false
				}
				return out, true
			}}
		}
	}
}

func TestResolveWithoutKustomizationFileStops(t *testing.T) {
	ni := sourcemap.NeedsIntervention
	twice := func(f *fixture) {
		plain(f)
		f.files["apps/orders/copy/so.yaml"] = orderTarget
	}
	manyFiles := func(f *fixture) {
		plain(f)
		for i := range sourcemap.MaxFiles {
			f.files[fmt.Sprintf("apps/orders/many/c%d.yaml", i)] = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n"
		}
	}
	tests := []struct {
		name   string
		change func(f *fixture)
		reason sourcemap.Reason
	}{
		{"two files declare the target", twice, sourcemap.ReasonAmbiguousDeclaration},
		{"no file declares the target", func(f *fixture) {
			plain(f)
			delete(f.files, soFile)
		}, sourcemap.ReasonDeclarationNotFound},
		{"a directory deeper than MaxListDepth", func(f *fixture) {
			plain(f)
			f.files[deep(sourcemap.MaxListDepth+1)+"/x.txt"] = "x\n"
		}, sourcemap.ReasonTooManyFiles},
		{"more than MaxFiles manifests", manyFiles, sourcemap.ReasonTooManyFiles},
		{"a directory over MaxDirEntries", func(f *fixture) {
			plain(f)
			for i := range providers.MaxDirEntries + 1 {
				f.files[fmt.Sprintf("apps/orders/big/f%d.txt", i)] = ""
			}
		}, sourcemap.ReasonTooManyFiles},
		{"more listings than MaxReads", wide(sourcemap.MaxReads, 0, providers.DirEntryFile), sourcemap.ReasonTooManyFiles},
		{"listings over the byte budget", wide(sourcemap.MaxTotalBytes/(sourcemap.ListEntryBytes*providers.MaxDirEntries)+1,
			providers.MaxDirEntries, providers.DirEntryFile), sourcemap.ReasonInputTooLarge},
		{"an entry that is neither a file nor a directory", func(f *fixture) {
			wide(1, 1, providers.DirEntryType("Symlink"))(f)
		}, sourcemap.ReasonInvalidPath},
		{"a .sourceignore file in the path", func(f *fixture) {
			plain(f)
			f.files["apps/orders/.sourceignore"] = "*.txt\n"
		}, sourcemap.ReasonSourceOptions},
		{"a .sourceignore file in a nested directory", func(f *fixture) {
			plain(f)
			f.files["apps/orders/sub/.sourceignore"] = "*.txt\n"
			f.files["apps/orders/sub/x.yaml"] = configMapX
		}, sourcemap.ReasonSourceOptions},
		{"a manifest in a hidden directory", func(f *fixture) {
			plain(f)
			f.files["apps/orders/.config/x.yaml"] = configMapX
		}, sourcemap.ReasonInvalidPath},
		{"a base MaxListDepth deep counts its depth against MaxDepth", func(f *fixture) {
			plain(f)
			base := deep(sourcemap.MaxListDepth)
			// The base and a chain below it: 5 + 4 levels, over MaxDepth (8); counted from 1 it would be 5.
			chain := base
			for range 4 {
				f.files[chain+"/kustomization.yaml"] = "resources:\n  - k\n"
				chain += "/k"
			}
			f.files[chain+"/kustomization.yaml"] = "resources: []\n"
		}, sourcemap.ReasonTooManyFiles},
		{"a listing with an entry no tree can hold", func(f *fixture) {
			plain(f)
			f.gitWrap = func(g providers.GitProvider) providers.GitProvider {
				return failList{GitProvider: g, err: fmt.Errorf("listing: %w", providers.ErrInvalidEntry)}
			}
		}, sourcemap.ReasonInvalidPath},
		{"a hidden name", func(f *fixture) {
			plain(f)
			f.files["apps/orders/.hidden.yaml"] = orderTarget
		}, sourcemap.ReasonInvalidPath},
		{"a name kustomize calls ambiguous", func(f *fixture) {
			plain(f)
			f.files["apps/orders/a..b.yaml"] = orderTarget
		}, sourcemap.ReasonInvalidPath},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.change(f)
			res, _, err := f.run(t)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome != ni || res.Reason != tc.reason {
				t.Fatalf("result = %s/%s (%s), want %s/%s", res.Outcome, res.Reason, res.Message, ni, tc.reason)
			}
			if strings.Contains(res.Message, "hidden") || strings.Contains(res.Message, "a..b") {
				t.Errorf("message quotes an unvalidated name: %s", res.Message)
			}
			sameWithCache(t, tc.change, res)
		})
	}
}

// failList fails every listing with err.
type failList struct {
	providers.GitProvider
	err error
}

func (f failList) ListDir(context.Context, string, string) ([]providers.DirEntry, error) {
	return nil, f.err
}

// A plain not found from the host (a 404 on a subtree, a token it cannot get) is transient, not the permanent
// NoKustomizationFile: only ErrPathNotInTree says the host read the tree.
func TestResolveListingNotFoundIsTransient(t *testing.T) {
	f := newFixture()
	plain(f)
	f.gitWrap = func(g providers.GitProvider) providers.GitProvider {
		return failList{GitProvider: g, err: fmt.Errorf("GitHub returned status 404: %w", providers.ErrNotFound)}
	}
	res, _, err := f.run(t)
	if !errors.Is(err, sourcemap.ErrTransient) {
		t.Fatalf("result = %s/%s, error = %v; want ErrTransient", res.Outcome, res.Reason, err)
	}
}

// listCounter counts the ListDir calls that reach the provider.
type listCounter struct {
	providers.GitProvider
	mu    sync.Mutex
	lists int
}

func (c *listCounter) ListDir(ctx context.Context, commit, dir string) ([]providers.DirEntry, error) {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.GitProvider.ListDir(ctx, commit, dir)
}

// A listing at a full commit ID is kept like a file read; a failed one is retried and never kept.
func TestResolveCachesListings(t *testing.T) {
	cache := newCache()
	var counters []*listCounter
	change := func(f *fixture) {
		plain(f)
		f.files["apps/orders/sub/x.yaml"] = configMapX
		f.cache = cache
		f.gitWrap = func(g providers.GitProvider) providers.GitProvider {
			c := &listCounter{GitProvider: g}
			counters = append(counters, c)
			return c
		}
	}
	for i := range 2 {
		f := newFixture()
		change(f)
		res, _, err := f.run(t)
		if err != nil || res.Outcome != sourcemap.Resolved {
			t.Fatalf("run %d = %+v, %v", i+1, res, err)
		}
	}
	if counters[0].lists != 2 || counters[1].lists != 0 {
		t.Errorf("listings = %d then %d, want 2 then 0 (both from the cache)", counters[0].lists, counters[1].lists)
	}

	// A provider error is transient and not kept: the next run lists again.
	failing := newCache()
	for i := range 2 {
		f := newFixture()
		plain(f)
		f.cache = failing
		c := counted(f, func(op string) error {
			if op == "ListDir" {
				return &providers.RetryableError{Err: errors.New("host unavailable")}
			}
			return nil
		})
		_, _, err := f.run(t)
		if !errors.Is(err, sourcemap.ErrTransient) {
			t.Fatalf("run %d: error = %v, want ErrTransient", i+1, err)
		}
		if c.of("ListDir") != 1 {
			t.Errorf("run %d listed %d times, want 1: a failed listing must not be kept", i+1, c.of("ListDir"))
		}
	}
}
