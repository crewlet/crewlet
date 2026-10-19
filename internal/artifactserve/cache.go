package artifactserve

import (
	"container/list"
	"sync"
)

// lru is a byte-bounded least-recently-used cache: what the origin keeps of
// immutable things it has already read and checked (a document's template, a
// file's gzip, a version's manifest), so a popular artifact is not read from
// the object store and the estate by every viewer.
//
// IMMUTABLE VALUES ONLY. Everything kept here is named by a content digest or
// by an immutable version, so a hit can never be stale; what may become stale
// (an authorization) is not kept here, and a purge only makes a hit pointless,
// never wrong, since nothing is served without a decision first.
type lru[K comparable, V any] struct {
	mu     sync.Mutex
	max    int64
	size   int64
	sizeOf func(V) int64
	order  *list.List
	items  map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	key  K
	val  V
	size int64
}

// newLRU builds a cache holding at most max bytes, each value weighed by
// sizeOf. A value heavier than a quarter of the cache is never kept: one large
// file must not evict every small one a page is made of.
func newLRU[K comparable, V any](max int64, sizeOf func(V) int64) *lru[K, V] {
	return &lru[K, V]{max: max, sizeOf: sizeOf, order: list.New(), items: map[K]*list.Element{}}
}

func (c *lru[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*lruEntry[K, V]).val, true
	}
	var zero V
	return zero, false
}

func (c *lru[K, V]) put(key K, val V) {
	size := c.sizeOf(val)
	if size > c.max/4 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		entry := e.Value.(*lruEntry[K, V])
		c.size += size - entry.size
		entry.val, entry.size = val, size
		c.order.MoveToFront(e)
	} else {
		c.items[key] = c.order.PushFront(&lruEntry[K, V]{key: key, val: val, size: size})
		c.size += size
	}
	for c.size > c.max {
		oldest := c.order.Back()
		entry := oldest.Value.(*lruEntry[K, V])
		c.order.Remove(oldest)
		delete(c.items, entry.key)
		c.size -= entry.size
	}
}

// removeIf drops every entry whose key matches.
func (c *lru[K, V]) removeIf(match func(K) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.items {
		if match(key) {
			c.order.Remove(e)
			delete(c.items, key)
			c.size -= e.Value.(*lruEntry[K, V]).size
		}
	}
}
