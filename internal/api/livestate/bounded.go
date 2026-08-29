package livestate

// boundedSet is an insertion-ordered set with a cap, evicting oldest-first.
//
// Used for the event-id dedupes and, with a value, for the finished-call guard.
// One pruning implementation either way.
//
// It exists because the Python this replaces used a plain dict — which is
// insertion-ordered there — seeded with thirty days of ids and never pruned. In
// an API process that stays up for weeks that is a slow leak. The cap only ever
// has to cover the hydration overlap plus any redelivery: a window of minutes,
// not the process lifetime.
type boundedSet[V any] struct {
	limit int
	items map[string]V
	order []string
}

func newBoundedSet[V any](limit int) *boundedSet[V] {
	return &boundedSet[V]{limit: limit, items: make(map[string]V, limit/8+1)}
}

// put records a key, evicting the oldest once past the cap.
//
// Re-putting an existing key REPLACES its value without moving it in the order.
// That is deliberate: the order is an eviction order, not a recency ranking,
// and promoting a key on every write would let a hot key hold the map open
// while the cap silently stopped bounding anything.
func (b *boundedSet[V]) put(key string, value V) {
	if _, seen := b.items[key]; !seen {
		b.order = append(b.order, key)
	}
	b.items[key] = value
	// Re-sliced forward rather than compacted, and that is sufficient: a
	// forward re-slice REDUCES the remaining capacity, so the next append
	// past it reallocates and the old backing array is released. The array
	// therefore oscillates around the limit rather than accumulating every
	// key ever put — which an added compaction step was written to prevent
	// and, measured, prevented nothing.
	for len(b.order) > b.limit {
		oldest := b.order[0]
		b.order = b.order[1:]
		delete(b.items, oldest)
	}
}

func (b *boundedSet[V]) get(key string) (V, bool) {
	v, ok := b.items[key]
	return v, ok
}

func (b *boundedSet[V]) has(key string) bool {
	_, ok := b.items[key]
	return ok
}

func (b *boundedSet[V]) len() int { return len(b.items) }
