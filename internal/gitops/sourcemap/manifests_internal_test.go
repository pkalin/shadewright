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
	"context"
	"fmt"
	"slices"
	"testing"
)

// left is how much of the byte budget the walkers in these tests have left before their last read.
const left = 100

// nearEnd returns a walker with only left bytes of the budget left.
func nearEnd(git *countingGit, cache *ReadCache) *walker {
	w := newWalker(git, commitA, cache, repoA)
	w.bytes = MaxTotalBytes - left
	return w
}

// A file over what is left of the budget is refused before it is downloaded: the provider is asked with the
// remaining budget as its limit and serves none of it, for a cold and a warm cache alike (#298).
func TestWalkerAsksForTheRemainingBudget(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cached), func(t *testing.T) {
			git := newCountingGit(left + 1)
			var cache *ReadCache
			if cached {
				cache = NewReadCache(DefaultCacheEntries, DefaultCacheBytes)
			}
			w := nearEnd(git, cache)
			_, stop, err := w.read(context.Background(), "f.yaml")
			if err != nil || stop == nil || stop.Reason != ReasonInputTooLarge {
				t.Fatalf("read = %v, %v; want InputTooLarge", stop, err)
			}
			if got := git.limits["f.yaml"]; !slices.Equal(got, []int{left}) {
				t.Errorf("provider was asked with limits %v, want [%d]", got, left)
			}
			if got := git.served["f.yaml"]; got != 0 {
				t.Errorf("provider served %d bytes of the refused file, want 0", got)
			}
			if w.bytes > MaxTotalBytes {
				t.Errorf("counted %d bytes, over the budget of %d", w.bytes, MaxTotalBytes)
			}
		})
	}
}

// The cache keeps no file the mapping refused for its budget, and does not record it as too large: a later
// mapping with the budget to spare reads it in full (#298).
func TestWalkerDoesNotCacheABudgetRefusal(t *testing.T) {
	git := newCountingGit(left + 1)
	cache := NewReadCache(DefaultCacheEntries, DefaultCacheBytes)
	if _, stop, err := nearEnd(git, cache).read(context.Background(), "f.yaml"); stop == nil ||
		stop.Reason != ReasonInputTooLarge || err != nil {
		t.Fatalf("near the end: read = %v, %v; want InputTooLarge", stop, err)
	}
	if cache.Len() != 0 {
		t.Fatalf("cache keeps %d entries after the refusal, want 0", cache.Len())
	}
	f, stop, err := newWalker(git, commitA, cache, repoA).read(context.Background(), "f.yaml")
	if stop != nil || err != nil || len(f.Data) != left+1 {
		t.Fatalf("with the budget to spare: read = %d bytes, %v, %v; want the file", len(f.Data), stop, err)
	}
}

// A cached file is checked against what is left of the budget as the provider would check it, so a warm
// cache stops where a cold one does, and a file within it is still read (#298).
func TestWalkerChecksCachedFilesAgainstTheBudget(t *testing.T) {
	git := newCountingGit(left + 1)
	cache := NewReadCache(DefaultCacheEntries, DefaultCacheBytes)
	// Warm the cache with a mapping that has the budget to spare.
	if _, stop, err := newWalker(git, commitA, cache, repoA).read(context.Background(), "f.yaml"); stop != nil ||
		err != nil {
		t.Fatalf("warm-up read = %v, %v", stop, err)
	}
	w := nearEnd(git, cache)
	_, stop, err := w.read(context.Background(), "f.yaml")
	if err != nil || stop == nil || stop.Reason != ReasonInputTooLarge {
		t.Fatalf("cached file over the budget: read = %v, %v; want InputTooLarge", stop, err)
	}
	if git.count("f.yaml") != 1 {
		t.Errorf("provider read the file %d times, want 1 (the warm-up)", git.count("f.yaml"))
	}
	if w.bytes != MaxTotalBytes-left {
		t.Errorf("counted %d bytes, want %d", w.bytes, MaxTotalBytes-left)
	}
}

// Past what is left of the budget a file over MaxFileBytes stops the mapping with InputTooLarge, whether the
// cache is cold or already keeps it as too large; with the full budget left it is FileNotReadable (#298).
func TestWalkerReasonOverMaxFileBytes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		warm bool
		w    func(*countingGit, *ReadCache) *walker
		want Reason
	}{
		{"full budget, cold", false, func(g *countingGit, c *ReadCache) *walker {
			return newWalker(g, commitA, c, repoA)
		}, ReasonFileNotReadable},
		{"full budget, warm", true, func(g *countingGit, c *ReadCache) *walker {
			return newWalker(g, commitA, c, repoA)
		}, ReasonFileNotReadable},
		{"near the end, cold", false, nearEnd, ReasonInputTooLarge},
		{"near the end, warm", true, nearEnd, ReasonInputTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := newCountingGit(MaxFileBytes + 1)
			cache := NewReadCache(DefaultCacheEntries, DefaultCacheBytes)
			if tc.warm {
				// A read with the full limit keeps the file as too large.
				if _, err := cache.readFile(ctx, git, repoA, commitA, "big.yaml", MaxFileBytes); err == nil {
					t.Fatal("warm-up read of a file over MaxFileBytes succeeded")
				}
				if cache.Len() != 1 {
					t.Fatalf("cache keeps %d entries after the warm-up, want 1", cache.Len())
				}
			}
			_, stop, err := tc.w(git, cache).read(ctx, "big.yaml")
			if err != nil || stop == nil || stop.Reason != tc.want {
				t.Fatalf("read = %v, %v; want %s", stop, err, tc.want)
			}
		})
	}
}

// A missing file costs nothing, so it is never refused for the budget, even with none of it left, and an
// empty file is still read then (#298).
func TestWalkerMissingFileAtTheEndOfTheBudget(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cached), func(t *testing.T) {
			var cache *ReadCache
			if cached {
				cache = NewReadCache(DefaultCacheEntries, DefaultCacheBytes)
			}
			for range 2 { // the second round reads from the cache, when there is one
				w := newWalker(newCountingGit(0), commitA, cache, repoA)
				w.bytes = MaxTotalBytes
				if _, stop, err := w.read(context.Background(), "missing.yaml"); err != nil || stop == nil ||
					stop.Reason != reasonFileNotFound {
					t.Fatalf("missing file: read = %v, %v; want not found", stop, err)
				}
				if _, stop, err := w.read(context.Background(), "empty.yaml"); err != nil || stop != nil {
					t.Fatalf("empty file: read = %v, %v; want it read", stop, err)
				}
			}
		})
	}
}
