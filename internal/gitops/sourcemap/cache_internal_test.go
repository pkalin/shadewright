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
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// countingGit serves files of a fixed size at any commit and counts ReadFile calls per path.
type countingGit struct {
	providers.GitProvider
	size int

	mu    sync.Mutex
	reads map[string]int
}

func newCountingGit(size int) *countingGit { return &countingGit{size: size, reads: map[string]int{}} }

func (g *countingGit) ReadFile(_ context.Context, commit, p string) (providers.File, error) {
	g.mu.Lock()
	g.reads[p]++
	g.mu.Unlock()
	switch {
	case strings.HasPrefix(p, "missing"):
		return providers.File{}, fmt.Errorf("file: %w", providers.ErrPathNotInTree)
	case strings.HasPrefix(p, "gone"):
		// What a host answers for a missing commit, or a 404 from a host that is failing.
		return providers.File{}, fmt.Errorf("GitHub returned status 404: %w", providers.ErrNotFound)
	}
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
		if _, err := c.readFile(ctx, git, repoA, commitA, fmt.Sprintf("f%d", i)); err != nil {
			t.Fatal(err)
		}
		// f0 is used after every read, so it is never the least recently used.
		if _, err := c.readFile(ctx, git, repoA, commitA, "f0"); err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}
	for p, want := range map[string]int{"f0": 1, "f1": 2, "f3": 1} {
		if _, err := c.readFile(ctx, git, repoA, commitA, p); err != nil {
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
	if _, err := c.readFile(ctx, git, repoA, commitA, "f0"); err != nil {
		t.Fatal(err)
	}
	if c.Bytes() != entry {
		t.Fatalf("Bytes = %d after one read, want %d", c.Bytes(), entry)
	}
	for i := range 5 {
		if _, err := c.readFile(ctx, git, repoA, commitA, fmt.Sprintf("f%d", i)); err != nil {
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
	if _, err := small.readFile(ctx, newCountingGit(1), repoA, commitA, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := small.readFile(ctx, git, repoA, commitA, "f0"); err != nil {
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
			if _, err := c.readFile(ctx, git, repoA, ref, "f"); err != nil {
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
		f, err := c.readFile(ctx, git, repoA, commitA, "f")
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
		_, err := c.readFile(ctx, git, repoA, commitA, "gone")
		if !providers.IsNotFound(err) || providers.IsPathNotInTree(err) {
			t.Fatalf("read gone: %v", err)
		}
		_, err = c.readFile(ctx, git, repoA, commitA, "missing")
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
	if _, err := c.readFile(context.Background(), git, repoA, commitA, "f"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.readFile(ctx, git, repoA, commitA, "f"); err == nil {
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
				f, err := c.readFile(ctx, git, repoA, commitA, p)
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
