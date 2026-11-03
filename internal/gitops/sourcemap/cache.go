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
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// Bounds of the read cache the executor shares between the policy check and the freshness rechecks.
const (
	// DefaultCacheEntries is how many file reads the cache keeps, found or not.
	DefaultCacheEntries = 4096
	// DefaultCacheBytes is how many bytes the cache keeps, file contents and keys together.
	DefaultCacheBytes = 16 << 20
	// cacheEntryOverhead is what one entry is counted for besides its content, path and commit, so that
	// entries without content (a file not found) are bounded by bytes too.
	cacheEntryOverhead = 256
)

// commitPattern is a full Git object ID (SHA-1 or SHA-256). Only reads at such a commit are cached: a branch,
// a tag or an abbreviated ID can name other content later.
var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ReadCache keeps the outcome of ReadFile calls at a commit, which never changes: the content of a file, or
// that the file does not exist or is too large. It is bounded by entries and bytes and evicts the least
// recently used entry first. It is safe for concurrent use.
//
// An entry is keyed by the whole RemediationSettings repository entry (URL, provider, target branch and
// credentials), the commit and the path, so a read through one entry is never answered from another's, even
// for the same URL. A file not found is kept only when it was read after a file at the same commit was found
// through the same entry: GitProvider answers ErrNotFound for a missing commit too, which a host that has not
// yet replicated a new commit may return for a while. Branch heads and cluster objects are never cached: the mapping reads them every time.
// Every read, served from the cache or not, counts against [MaxReads], so the cache never changes a result.
type ReadCache struct {
	maxEntries int
	maxBytes   int64

	mu    sync.Mutex
	lru   *list.List // of *cacheEntry, most recently used first
	items map[cacheKey]*list.Element
	bytes int64
	// found counts the kept files per entry and commit: a commit with one is known to exist.
	found map[commitKey]int
}

// NewReadCache returns a cache that keeps at most maxEntries reads and maxBytes bytes. A bound of zero or
// less keeps nothing.
func NewReadCache(maxEntries int, maxBytes int64) *ReadCache {
	return &ReadCache{maxEntries: maxEntries, maxBytes: maxBytes, lru: list.New(), items: map[cacheKey]*list.Element{},
		found: map[commitKey]int{}}
}

// commitKey names one commit as read through one settings entry.
type commitKey struct {
	repo   [sha256.Size]byte
	commit string
}

// cacheKey names one read: the settings entry it went through (a digest of the whole entry), the commit and
// the path.
type cacheKey struct {
	repo   [sha256.Size]byte
	commit string
	path   string
}

type cacheOutcome int

const (
	cachedFile cacheOutcome = iota
	cachedNotFound
	cachedTooLarge
)

type cacheEntry struct {
	key     cacheKey
	outcome cacheOutcome
	file    providers.File
	cost    int64
}

// Fixed errors for cached outcomes. The walker looks only at their class, never at their text.
var (
	errCachedNotFound = fmt.Errorf("file at commit: %w", providers.ErrNotFound)
	errCachedTooLarge = fmt.Errorf("file at commit: %w", providers.ErrTooLarge)
)

// repoKey is the digest of a settings repository entry. ok is false when it cannot be encoded, and then the
// cache must not be used.
func repoKey(repo v1alpha1.GitRepository) (key [sha256.Size]byte, ok bool) {
	b, err := json.Marshal(repo)
	if err != nil {
		return key, false
	}
	return sha256.Sum256(b), true
}

// readFile reads p at commit through git, answering from the cache when it can. A nil cache, or a commit that
// is not a full object ID, always reads through git. Only a file, ErrNotFound and ErrTooLarge are kept; any
// other error is returned and never kept.
func (c *ReadCache) readFile(ctx context.Context, git providers.GitProvider, repo [sha256.Size]byte, commit, p string) (
	providers.File, error) {
	if c == nil || !commitPattern.MatchString(commit) {
		return git.ReadFile(ctx, commit, p)
	}
	k := cacheKey{repo: repo, commit: commit, path: p}
	if e, ok := c.get(k); ok {
		// A cancelled read fails as an uncached one would.
		if err := ctx.Err(); err != nil {
			return providers.File{}, err
		}
		return e.result()
	}
	// Whether the commit was known to exist before the read started, so a not found is about the path.
	exists := c.exists(commitKey{repo: repo, commit: commit})
	f, err := git.ReadFile(ctx, commit, p)
	c.put(k, f, err, exists)
	return f, err
}

func (c *ReadCache) exists(k commitKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.found[k] > 0
}

// result returns the cached outcome, with a copy of the content so a caller cannot change the cache.
func (e *cacheEntry) result() (providers.File, error) {
	switch e.outcome {
	case cachedNotFound:
		return providers.File{}, errCachedNotFound
	case cachedTooLarge:
		return providers.File{}, errCachedTooLarge
	}
	return providers.File{Path: e.file.Path, Commit: e.file.Commit, Data: bytes.Clone(e.file.Data)}, nil
}

func (c *ReadCache) get(k cacheKey) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return cacheEntry{}, false
	}
	c.lru.MoveToFront(el)
	return *el.Value.(*cacheEntry), true
}

// put keeps the outcome of a read, if it is one that never changes, and evicts until the bounds hold. A not
// found is kept only when the commit was known to exist before the read started.
func (c *ReadCache) put(k cacheKey, f providers.File, err error, commitExists bool) {
	e := &cacheEntry{key: k}
	switch {
	case err == nil && len(f.Data) <= MaxFileBytes:
		e.outcome = cachedFile
		e.file = providers.File{Path: f.Path, Commit: f.Commit, Data: bytes.Clone(f.Data)}
	case providers.IsNotFound(err) && commitExists:
		e.outcome = cachedNotFound
	case providers.IsTooLarge(err):
		e.outcome = cachedTooLarge
	default:
		// A transient or refused read, a not found at a commit that may not exist yet, or a file over the
		// limit that the adapter did not refuse.
		return
	}
	e.cost = int64(len(e.file.Data)+len(e.file.Path)+len(e.file.Commit)+len(k.commit)+len(k.path)) + cacheEntryOverhead
	if c.maxEntries <= 0 || e.cost > c.maxBytes {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		// Another reconcile read the same file meanwhile; the outcome is the same.
		c.lru.MoveToFront(el)
		return
	}
	c.items[k] = c.lru.PushFront(e)
	c.bytes += e.cost
	if e.outcome == cachedFile {
		c.found[commitKey{repo: k.repo, commit: k.commit}]++
	}
	for c.lru.Len() > c.maxEntries || c.bytes > c.maxBytes {
		oldest := c.lru.Back()
		old := oldest.Value.(*cacheEntry)
		c.lru.Remove(oldest)
		delete(c.items, old.key)
		c.bytes -= old.cost
		if old.outcome == cachedFile {
			ck := commitKey{repo: old.key.repo, commit: old.key.commit}
			if c.found[ck]--; c.found[ck] <= 0 {
				delete(c.found, ck)
			}
		}
	}
}

// Len returns how many reads the cache keeps.
func (c *ReadCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// Bytes returns how many bytes the cache counts for what it keeps.
func (c *ReadCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}
