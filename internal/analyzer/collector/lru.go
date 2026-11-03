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

package collector

import "container/list"

// lru is a map with a fixed capacity that drops the least recently used entry when it is full. It is not
// safe for concurrent use: its owner holds a lock around every call, which also covers onEvict.
type lru[K comparable, V any] struct {
	max     int
	order   *list.List // front is the most recently used
	items   map[K]*list.Element
	onEvict func(K, V)
}

type lruEntry[K comparable, V any] struct {
	key K
	val V
}

// newLRU returns an empty lru that holds at most capacity entries (at least one). onEvict, when set, is called
// for every entry dropped to make room, not for entries removed with delete.
func newLRU[K comparable, V any](capacity int, onEvict func(K, V)) *lru[K, V] {
	return &lru[K, V]{max: max(capacity, 1), order: list.New(), items: map[K]*list.Element{}, onEvict: onEvict}
}

// get returns the value of k and marks it as recently used.
func (c *lru[K, V]) get(k K) (V, bool) {
	e, ok := c.items[k]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*lruEntry[K, V]).val, true
}

// put sets the value of k, marks it as recently used and drops the least recently used entries beyond the
// capacity.
func (c *lru[K, V]) put(k K, v V) {
	if e, ok := c.items[k]; ok {
		e.Value.(*lruEntry[K, V]).val = v
		c.order.MoveToFront(e)
		return
	}
	c.items[k] = c.order.PushFront(&lruEntry[K, V]{key: k, val: v})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		ent := oldest.Value.(*lruEntry[K, V])
		c.order.Remove(oldest)
		delete(c.items, ent.key)
		if c.onEvict != nil {
			c.onEvict(ent.key, ent.val)
		}
	}
}

// delete removes k, if present.
func (c *lru[K, V]) delete(k K) {
	if e, ok := c.items[k]; ok {
		c.order.Remove(e)
		delete(c.items, k)
	}
}

// len returns the number of entries.
func (c *lru[K, V]) len() int {
	return c.order.Len()
}
