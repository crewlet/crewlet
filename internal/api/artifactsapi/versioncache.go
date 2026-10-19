package artifactsapi

import (
	"container/list"
	"sync"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// versionCache keeps what a native format read from a version (a deck, a
// board), by version, within a budget of the bytes each was read from. A
// version never changes, so a cached read is never stale; who may read it is
// decided on every request before the cache is asked, so nothing here is
// any one viewer's.
type versionCache[T any] struct {
	mu    sync.Mutex
	order *list.List // of *versionEntry[T], most recent first
	byKey map[versionKey]*list.Element
	bytes int64
	limit int64
}

type versionKey struct {
	artifact artifacts.ID
	version  int
}

type versionEntry[T any] struct {
	key    versionKey
	value  T
	weight int64
}

func newVersionCache[T any](limit int64) *versionCache[T] {
	return &versionCache[T]{order: list.New(), byKey: map[versionKey]*list.Element{}, limit: limit}
}

// read answers the value at key, loading it on a miss. load answers the value
// and the bytes it was read from; a value heavier than the whole budget is
// answered and not kept. Two misses of one key at once both load, and the
// first to finish is kept: a read is idempotent, and a lock held across a
// load would stall every other version behind one slow object read.
func (c *versionCache[T]) read(key versionKey, load func() (T, int64, error)) (T, error) {
	c.mu.Lock()
	if el, ok := c.byKey[key]; ok {
		c.order.MoveToFront(el)
		v := el.Value.(*versionEntry[T]).value
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	v, weight, err := load()
	if err != nil {
		var zero T
		return zero, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byKey[key]; ok || weight > c.limit {
		return v, nil
	}
	c.byKey[key] = c.order.PushFront(&versionEntry[T]{key: key, value: v, weight: weight})
	c.bytes += weight
	for c.bytes > c.limit {
		last := c.order.Back()
		e := last.Value.(*versionEntry[T])
		c.order.Remove(last)
		delete(c.byKey, e.key)
		c.bytes -= e.weight
	}
	return v, nil
}
