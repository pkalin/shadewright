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
	"slices"
	"sync"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// Bounds of the read cache the executor shares between the policy check and the freshness rechecks.
const (
	// DefaultCacheEntries is how many file reads and directory listings the cache keeps, found or not.
	DefaultCacheEntries = 4096
	// DefaultCacheBytes is how many bytes the cache keeps, file contents and keys together.
	DefaultCacheBytes = 16 << 20
	// cacheEntryOverhead is what one entry is counted for besides its content, commit and path, so that
	// entries without content (a file not in the tree) are bounded by bytes too.
	cacheEntryOverhead = 256
	// dirEntryOverhead is what one entry of a kept listing is counted for besides its name.
	dirEntryOverhead = 16
)

// ReadCache keeps the outcome of ReadFile calls at a full commit ID, which never changes: the content of a
// file, that the path is not a regular file in the commit's tree ([providers.ErrPathNotInTree]), or that the
// file is too large; and of ListDir calls the same way: the listing, a path that is not a directory in the
// tree, or a directory over providers.MaxDirEntries. A tree at a commit ID never changes, so a listing is as
// safe to keep as a file; a listing and a file read of one path have separate entries. It is bounded by
// entries and bytes and evicts the least recently used entry first. It is safe for concurrent use.
//
// An entry is keyed by the whole RemediationSettings repository entry (URL, provider, target branch and
// credentials), the commit and the path, so a read through one entry is never answered from another's, even
// for the same URL. A plain ErrNotFound is never kept: GitProvider also answers it for a commit the host does
// not have yet, and the GitHub adapter for any 404 or a token it cannot get, so it may not hold on the next
// read. Branch heads and cluster objects are never cached: the mapping reads them every time. Every read and
// listing, served from the cache or not, counts against [MaxReads] and [MaxTotalBytes] alike, so the cache
// never changes a result.
type ReadCache struct {
	maxEntries int
	maxBytes   int64

	mu    sync.Mutex
	lru   *list.List // of *cacheEntry, most recently used first
	items map[cacheKey]*list.Element
	bytes int64
}

// NewReadCache returns a cache that keeps at most maxEntries reads and maxBytes bytes. A bound of zero or
// less keeps nothing.
func NewReadCache(maxEntries int, maxBytes int64) *ReadCache {
	return &ReadCache{maxEntries: maxEntries, maxBytes: maxBytes, lru: list.New(), items: map[cacheKey]*list.Element{}}
}

// cacheKey names one read: the settings entry it went through (a digest of the whole entry), the commit and
// the path.
type cacheKey struct {
	repo   [sha256.Size]byte
	commit string
	path   string
	// list is true for a directory listing, so a listing and a file read of one path never share an entry.
	list bool
}

type cacheOutcome int

const (
	cachedFile cacheOutcome = iota
	cachedNotInTree
	cachedTooLarge
	cachedDir
)

// cacheEntry is one kept read. Only the content is stored: the path and commit of the file are the key's.
type cacheEntry struct {
	key     cacheKey
	outcome cacheOutcome
	data    []byte
	entries []providers.DirEntry
	cost    int64
}

// Fixed errors for cached outcomes. The walker looks only at their class, never at their text.
var (
	errCachedNotInTree = fmt.Errorf("file at commit: %w", providers.ErrPathNotInTree)
	errCachedTooLarge  = fmt.Errorf("file at commit: %w", providers.ErrTooLarge)
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

// readFile reads p at commit through git, at most limit bytes (clamped by providers.ReadLimit), answering from
// the cache when it can. A nil cache, or a commit that is not a full object ID, always reads through git. A
// kept file larger than limit gives ErrTooLarge as the provider would, so a warm cache refuses what a cold one
// refuses. Only a file within limit, ErrPathNotInTree and, for a read with the full MaxFileBytes limit,
// ErrTooLarge are kept: a file over a smaller limit is not too large in general, and a later mapping with more
// budget left may read it. Any other error, a plain ErrNotFound included, is returned and never kept.
func (c *ReadCache) readFile(ctx context.Context, git providers.GitProvider, repo [sha256.Size]byte, commit, p string,
	limit int) (providers.File, error) {
	limit = providers.ReadLimit(limit)
	if c == nil || !providers.IsObjectID(commit) {
		return git.ReadFile(ctx, commit, p, limit)
	}
	k := cacheKey{repo: repo, commit: commit, path: p}
	if e, ok := c.get(k); ok {
		// A cancelled read fails as an uncached one would.
		if err := ctx.Err(); err != nil {
			return providers.File{}, err
		}
		return e.result(limit)
	}
	f, err := git.ReadFile(ctx, commit, p, limit)
	c.put(k, f, err, limit)
	return f, err
}

// listDir lists dir at commit through git, answering from the cache when it can, as readFile does: a nil
// cache, or a commit that is not a full object ID, always lists through git. A listing, ErrPathNotInTree and
// ErrTooLarge (a directory over providers.MaxDirEntries, which no budget changes) are kept, since the tree at a
// commit ID never changes; any other error, a plain ErrNotFound included, is returned and never kept.
func (c *ReadCache) listDir(ctx context.Context, git providers.GitProvider, repo [sha256.Size]byte, commit, dir string) (
	[]providers.DirEntry, error) {
	if c == nil || !providers.IsObjectID(commit) {
		return git.ListDir(ctx, commit, dir)
	}
	k := cacheKey{repo: repo, commit: commit, path: dir, list: true}
	if e, ok := c.get(k); ok {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch e.outcome {
		case cachedNotInTree:
			return nil, errCachedNotInTree
		case cachedTooLarge:
			return nil, errCachedTooLarge
		}
		return slices.Clone(e.entries), nil
	}
	entries, err := git.ListDir(ctx, commit, dir)
	e := &cacheEntry{key: k}
	switch {
	case err == nil && len(entries) <= providers.MaxDirEntries:
		e.outcome, e.entries = cachedDir, slices.Clone(entries)
		for _, d := range entries {
			e.cost += int64(len(d.Name)) + dirEntryOverhead
		}
	case providers.IsPathNotInTree(err):
		e.outcome = cachedNotInTree
	case providers.IsTooLarge(err):
		e.outcome = cachedTooLarge
	default:
		// A transient or refused listing, a plain not found, or an adapter that listed more than it may.
		return entries, err
	}
	c.add(e, e.cost+int64(len(k.commit)+len(k.path))+cacheEntryOverhead)
	return entries, err
}

// get returns the entry for k and marks it used. An entry never changes once kept, so it can be read without
// the lock.
func (c *ReadCache) get(k cacheKey) (*cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*cacheEntry), true
}

// result returns the kept outcome for a read of at most limit bytes, with a copy of the content so a caller
// cannot change the cache.
func (e *cacheEntry) result(limit int) (providers.File, error) {
	switch {
	case e.outcome == cachedNotInTree:
		return providers.File{}, errCachedNotInTree
	case e.outcome == cachedTooLarge, len(e.data) > limit:
		return providers.File{}, errCachedTooLarge
	}
	return providers.File{Path: e.key.path, Commit: e.key.commit, Data: bytes.Clone(e.data)}, nil
}

// put keeps the outcome of a read of at most limit bytes, if it is one that never changes, and evicts until the
// bounds hold.
func (c *ReadCache) put(k cacheKey, f providers.File, err error, limit int) {
	e := &cacheEntry{key: k}
	switch {
	case err == nil && len(f.Data) <= limit && f.Path == k.path && f.Commit == k.commit:
		e.outcome = cachedFile
	case providers.IsPathNotInTree(err):
		e.outcome = cachedNotInTree
	case providers.IsTooLarge(err) && limit >= MaxFileBytes:
		e.outcome = cachedTooLarge
	default:
		// A transient or refused read, a plain not found (a missing commit, a failing host), a file over a
		// limit smaller than MaxFileBytes (refused for one mapping's budget only), a file over the limit that
		// the adapter did not refuse, or a file the adapter labelled with another path or commit.
		return
	}
	if e.outcome == cachedFile {
		e.data = bytes.Clone(f.Data)
	}
	c.add(e, int64(len(f.Data)+len(k.commit)+len(k.path))+cacheEntryOverhead)
}

// add keeps e at the given cost, unless it alone is over the bounds, and evicts until the bounds hold.
func (c *ReadCache) add(e *cacheEntry, cost int64) {
	e.cost = cost
	if c.maxEntries <= 0 || e.cost > c.maxBytes {
		return
	}
	k := e.key

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		// Another reconcile read the same file meanwhile; the outcome is the same.
		c.lru.MoveToFront(el)
		return
	}
	c.items[k] = c.lru.PushFront(e)
	c.bytes += e.cost
	for c.lru.Len() > c.maxEntries || c.bytes > c.maxBytes {
		oldest := c.lru.Back()
		old := oldest.Value.(*cacheEntry)
		c.lru.Remove(oldest)
		delete(c.items, old.key)
		c.bytes -= old.cost
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
