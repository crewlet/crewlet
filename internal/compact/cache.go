package compact

import (
	"container/list"
	"sync"
)

const (
	// CacheEntries bounds how many rewrites one node keeps.
	//
	// What the cache serves is a block re-rendered on every round of a turn
	// — the prior-work ledger's argument values, a conversation's older
	// entries — so it needs to hold one turn's worth of rewrites for every
	// turn running at once. A node runs at most `node.max_concurrent`
	// turns (default 32), and a busy turn's ledger rewrites a few dozen
	// values; 2048 holds that with room, and an evicted entry costs one
	// repeated call, never a wrong answer.
	CacheEntries = 2048

	// CacheBytes bounds the rewrites' total size, which the entry count
	// alone does not: a source compacted for an answer can be tens of
	// kilobytes where an argument is two hundred bytes. 16 MiB is noise on
	// any host that runs model turns.
	CacheBytes = 16 << 20
)

// Cache is a node-local LRU of rewrites keyed by request identity. A nil
// *Cache remembers nothing.
//
// NODE-LOCAL ON PURPOSE: a rewrite is a pure function of its request at
// temperature zero, so a peer holding a different one is a peer that asked
// at a different time — never a disagreement anything has to resolve — and
// a miss costs one call.
type Cache struct {
	mu      sync.Mutex
	order   *list.List // front is most recently used
	entries map[string]*list.Element
	bytes   int
}

type cached struct {
	key  string
	text string
}

// NewCache builds an empty cache.
func NewCache() *Cache {
	return &Cache{order: list.New(), entries: make(map[string]*list.Element)}
}

// Get is the rewrite cached under key.
func (c *Cache) Get(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cached).text, true
}

// Put records a rewrite, evicting the least recently used past either bound.
func (c *Cache) Put(key, text string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.entries[key] = c.order.PushFront(&cached{key: key, text: text})
	c.bytes += len(text)
	for c.order.Len() > CacheEntries || (c.bytes > CacheBytes && c.order.Len() > 1) {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		entry := oldest.Value.(*cached)
		delete(c.entries, entry.key)
		c.bytes -= len(entry.text)
	}
}
