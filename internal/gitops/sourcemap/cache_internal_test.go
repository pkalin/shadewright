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

package sourcemap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// countingGit serves files of a fixed size at any commit, refuses one over the read's limit as a provider
// would (before downloading it), and counts ReadFile calls, their limits and the bytes served per path.
type countingGit struct {
	providers.GitProvider
	size int

	mu     sync.Mutex
	reads  map[string]int
	limits map[string][]int
	served map[string]int
}

func newCountingGit(size int) *countingGit {
	return &countingGit{size: size, reads: map[string]int{}, limits: map[string][]int{}, served: map[string]int{}}
}

func (g *countingGit) ReadFile(_ context.Context, commit, p string, limit int) (providers.File, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads[p]++
	g.limits[p] = append(g.limits[p], limit)
	switch {
	case strings.HasPrefix(p, "missing"):
		return providers.File{}, fmt.Errorf("file: %w", providers.ErrPathNotInTree)
	case strings.HasPrefix(p, "gone"):
		// What a host answers for a missing commit, or a 404 from a host that is failing.
		return providers.File{}, fmt.Errorf("GitHub returned status 404: %w", providers.ErrNotFound)
	case g.size > providers.ReadLimit(limit):
		return providers.File{}, fmt.Errorf("file: %w", providers.ErrTooLarge)
	}
	g.served[p] += g.size
	return providers.File{Path: p, Commit: commit, Data: bytes.Repeat([]byte{'x'}, g.size)}, nil
}

func (g *countingGit) count(p string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads[p]
}

var (
	commitA = strings.Repeat("a", 40)
	repoA   = sha256.Sum256([]byte("a"))
)

func TestReadCacheStaysWithinItsEntries(t *testing.T) {
	c := NewReadCache(3, 1<<20)
	git := newCountingGit(10)
	ctx := context.Background()
	for i := range 4 {
		if _, err := c.readFile(ctx, git, repoA, commitA, fmt.Sprintf("f%d", i), MaxFileBytes); err != nil {
			t.Fatal(err)
		}
		// f0 is used after every read, so it is never the least recently used.
		if _, err := c.readFile(ctx, git, repoA, commitA, "f0", MaxFileBytes); err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	for p, want := range map[string]int{"f0": 1, "f1": 2, "f3": 1} {
		if _, err := c.readFile(ctx, git, repoA, commitA, p, MaxFileBytes); err != nil {
			t.Fatal(err)
		}
		if got := git.count(p); got != want {
			t.Errorf("%s was read %d times, want %d", p, got, want)
		}
	}
}

func TestReadCacheStaysWithinItsBytes(t *testing.T) {
	const size = 1000
	// The commit and the path are counted once, as part of the key; the file is stored without them.
	entry := int64(size + len(commitA) + len("f0") + cacheEntryOverhead)
	limit := 2*entry + 10
	c := NewReadCache(100, limit)
	git := newCountingGit(size)
	ctx := context.Background()
	if _, err := c.readFile(ctx, git, repoA, commitA, "f0", MaxFileBytes); err != nil {
		t.Fatal(err)
	}
	if c.Bytes() != entry {
		t.Fatalf("Bytes = %d after one read, want %d", c.Bytes(), entry)
	}
	for i := range 5 {
		if _, err := c.readFile(ctx, git, repoA, commitA, fmt.Sprintf("f%d", i), MaxFileBytes); err != nil {
			t.Fatal(err)
		}
		if c.Bytes() > limit {
			t.Fatalf("Bytes = %d after %d reads, over the limit %d", c.Bytes(), i+1, limit)
		}
	}
	if c.Len() != 2 {
		t.Errorf("Len = %d, want 2", c.Len())
	}
	// A file that alone is larger than the cache is never kept, and does not push out what is kept.
	small := NewReadCache(100, 1000)
	if _, err := small.readFile(ctx, newCountingGit(1), repoA, commitA, "s", MaxFileBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := small.readFile(ctx, git, repoA, commitA, "f0", MaxFileBytes); err != nil {
		t.Fatal(err)
	}
	if small.Len() != 1 || small.Bytes() > 1000 {
		t.Errorf("after a file over the byte bound: Len %d, Bytes %d; want the one small file kept", small.Len(), small.Bytes())
	}
}

// Only a full commit ID addresses content that never changes; a branch name or a short ID is read every time.
func TestReadCacheKeepsOnlyReadsAtAFullCommitID(t *testing.T) {
	ctx := context.Background()
	for _, ref := range []string{defaultTargetBranch, commitA[:12], strings.ToUpper(commitA), commitA + "0"} {
		c := NewReadCache(10, 1<<20)
		git := newCountingGit(1)
		for range 2 {
			if _, err := c.readFile(ctx, git, repoA, ref, "f", MaxFileBytes); err != nil {
				t.Fatal(err)
			}
		}
		if git.count("f") != 2 || c.Len() != 0 {
			t.Errorf("ref %q: %d reads and %d entries, want 2 reads and none kept", ref, git.count("f"), c.Len())
		}
	}
}

// A caller that changes the content it got cannot change what the cache gives the next caller.
func TestReadCacheReturnsCopies(t *testing.T) {
	c := NewReadCache(10, 1<<20)
	git := newCountingGit(4)
	ctx := context.Background()
	for range 3 {
		f, err := c.readFile(ctx, git, repoA, commitA, "f", MaxFileBytes)
		if err != nil {
			t.Fatal(err)
		}
		if string(f.Data) != "xxxx" || f.Path != "f" || f.Commit != commitA {
			t.Fatalf("file = %+v, want xxxx at f and %s", f, commitA)
		}
		f.Data[0] = 'y'
	}
}

// A path the adapter found absent from the commit's tree is kept; a plain not found, which a missing commit or
// a failing host also gives, is read again every time.
func TestReadCacheKeepsOnlyAPathNotInTheTree(t *testing.T) {
	c := NewReadCache(10, 1<<20)
	git := newCountingGit(1)
	ctx := context.Background()
	for range 2 {
		_, err := c.readFile(ctx, git, repoA, commitA, "gone", MaxFileBytes)
		if !providers.IsNotFound(err) || providers.IsPathNotInTree(err) {
			t.Fatalf("read gone: %v", err)
		}
		_, err = c.readFile(ctx, git, repoA, commitA, "missing", MaxFileBytes)
		if !providers.IsPathNotInTree(err) || !providers.IsNotFound(err) {
			t.Fatalf("read missing: %v", err)
		}
	}
	if git.count("gone") != 2 {
		t.Errorf("a plain not found was read %d times, want 2: it was kept", git.count("gone"))
	}
	if git.count("missing") != 1 {
		t.Errorf("a path not in the tree was read %d times, want 1", git.count("missing"))
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d, want 1", c.Len())
	}
}

// A cancelled context fails a read the cache could answer, as it would fail a read from the host.
func TestReadCacheHonoursCancellation(t *testing.T) {
	c := NewReadCache(10, 1<<20)
	git := newCountingGit(1)
	if _, err := c.readFile(context.Background(), git, repoA, commitA, "f", MaxFileBytes); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.readFile(ctx, git, repoA, commitA, "f", MaxFileBytes); err == nil {
		t.Error("a cancelled read was answered")
	}
}

// Many reconciles read through one cache at once; run with -race.
func TestReadCacheIsSafeForConcurrentUse(t *testing.T) {
	c := NewReadCache(16, 1<<20)
	git := newCountingGit(8)
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				p := fmt.Sprintf("f%d", (g+i)%32)
				f, err := c.readFile(ctx, git, repoA, commitA, p, MaxFileBytes)
				if err != nil || f.Path != p || len(f.Data) != 8 {
					t.Errorf("read %s = %+v, %v", p, f, err)
					return
				}
			}
		})
	}
	wg.Wait()
	if c.Len() > 16 {
		t.Errorf("Len = %d, over the bound 16", c.Len())
	}
}

// ListDir lists two entries in any directory, and answers by prefix as ReadFile does: missing* is not in the
// tree, gone* a plain not found, wide* over MaxDirEntries and fail* a transient failure.
func (g *countingGit) ListDir(_ context.Context, _, dir string) ([]providers.DirEntry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads["list "+dir]++
	switch {
	case strings.HasPrefix(dir, "missing"):
		return nil, fmt.Errorf("directory: %w", providers.ErrPathNotInTree)
	case strings.HasPrefix(dir, "gone"):
		return nil, fmt.Errorf("GitHub returned status 404: %w", providers.ErrNotFound)
	case strings.HasPrefix(dir, "wide"):
		return nil, fmt.Errorf("directory: %w", providers.ErrTooLarge)
	case strings.HasPrefix(dir, "fail"):
		return nil, &providers.RetryableError{Err: errors.New("host unavailable")}
	}
	return []providers.DirEntry{{Name: "a.yaml", Type: providers.DirEntryFile}, {Name: "b", Type: providers.DirEntryDir}}, nil
}

// A listing, a path not in the tree and a directory over MaxDirEntries are kept; a plain not found and a failed
// listing are asked again every time. A listing and a file read of one path never answer each other, and a
// caller cannot change what the cache gives the next one.
func TestReadCacheKeepsListings(t *testing.T) {
	c := NewReadCache(10, 1<<20)
	src := newCountingGit(1)
	ctx := context.Background()
	for range 2 {
		entries, err := c.listDir(ctx, src, repoA, commitA, "d")
		if err != nil || len(entries) != 2 || entries[0].Name != "a.yaml" {
			t.Fatalf("list d = %v, %v", entries, err)
		}
		entries[0].Name = "changed"
		if _, err := c.listDir(ctx, src, repoA, commitA, "missing"); !providers.IsPathNotInTree(err) {
			t.Fatalf("list missing: %v", err)
		}
		if _, err := c.listDir(ctx, src, repoA, commitA, "wide"); !providers.IsTooLarge(err) {
			t.Fatalf("list wide: %v", err)
		}
		if _, err := c.listDir(ctx, src, repoA, commitA, "gone"); !providers.IsNotFound(err) || providers.IsPathNotInTree(err) {
			t.Fatalf("list gone: %v", err)
		}
		if _, err := c.listDir(ctx, src, repoA, commitA, "fail"); !providers.IsRetryable(err) {
			t.Fatalf("list fail: %v", err)
		}
	}
	for p, want := range map[string]int{"list d": 1, "list missing": 1, "list wide": 1, "list gone": 2, "list fail": 2} {
		if got := src.count(p); got != want {
			t.Errorf("%s reached the provider %d times, want %d", p, got, want)
		}
	}
	if _, err := c.readFile(ctx, src, repoA, commitA, "d", MaxFileBytes); err != nil || src.count("d") != 1 {
		t.Errorf("a file read of a listed path was answered from the listing: %v, %d reads", err, src.count("d"))
	}
	// A branch name is listed every time.
	for range 2 {
		if _, err := c.listDir(ctx, src, repoA, defaultTargetBranch, "e"); err != nil {
			t.Fatal(err)
		}
	}
	if src.count("list e") != 2 {
		t.Errorf("a listing at a branch name reached the provider %d times, want 2", src.count("list e"))
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.listDir(cancelled, src, repoA, commitA, "d"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled listing the cache could answer = %v, want context.Canceled", err)
	}
}

// A kept listing is counted for its names and the string headers of each entry, so many short names cannot
// hold more memory than the byte bound says.
func TestReadCacheCountsListingCost(t *testing.T) {
	c := NewReadCache(10, 1<<20)
	if _, err := c.listDir(context.Background(), newCountingGit(1), repoA, commitA, "d"); err != nil {
		t.Fatal(err)
	}
	// countingGit lists a.yaml and b; a DirEntry holds two string headers of 16 bytes each.
	want := int64(len("a.yaml")+32+len("b")+32+len(commitA)+len("d")) + cacheEntryOverhead
	if c.Bytes() != want {
		t.Errorf("Bytes = %d, want %d", c.Bytes(), want)
	}
}

// A file the cache cannot keep is not copied first.
func TestReadCacheDoesNotCopyWhatItCannotKeep(t *testing.T) {
	c := NewReadCache(10, 100)
	f := providers.File{Path: "f", Commit: commitA, Data: make([]byte, 4096)}
	k := cacheKey{repo: repoA, commit: commitA, path: "f"}
	if allocs := testing.AllocsPerRun(10, func() { c.put(k, f, nil, MaxFileBytes) }); allocs > 1 {
		t.Errorf("put of a file over the byte bound made %.0f allocations, want at most 1 (no copy)", allocs)
	}
	if c.Len() != 0 {
		t.Errorf("Len = %d, want 0", c.Len())
	}
}
