package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
)

// The document families in memory.
//
// A TWIN, not a lesser implementation: it is certified by the same suite the
// KV backend runs, because a twin that agrees only with itself proves
// nothing. The three properties it has to reproduce faithfully are the ones
// every caller reasons about — a monotonic revision per family, purge markers
// that reach a watcher, and a watch that opens with what is already there and
// then follows what arrives.
type documents struct {
	mu sync.Mutex

	// records is family -> key -> record.
	records map[coord.Family]map[string]coord.Record

	// revision is the family's own monotonic sequence. PER FAMILY, matching
	// the broker: a family's revisions are the sequence of its bucket's
	// stream, and a projection cursor is a position in exactly one of them.
	revision map[coord.Family]uint64

	// watchers are the live views, by family.
	watchers map[coord.Family][]*memWatcher

	// log is a bounded history per family, so a RESUMED watch replays what
	// it missed rather than nothing.
	//
	// The twin would otherwise hold only current values and a resume would
	// deliver silence — which is not what the broker does and not what a
	// node catching up after a restart needs. Bounded because this is an
	// in-process store with no disk behind it, and a resume that falls
	// before the retained window degrades exactly as a compacted stream
	// does: the caller is handed current state instead, which is a
	// complete answer about WHAT IS and an incomplete one about what
	// happened. A projector reconciles per key on top of it either way.
	log map[coord.Family][]coord.Change

	// feeds are the durable consumer groups, keyed by family/class/name.
	// Held here rather than per-handle so a second opener of one group
	// joins the fleet's position instead of starting a new one.
	feeds map[string]*memFeed
}

// logRetained is how many changes per family the twin keeps for resumes.
//
// Sized to be far past any test's needs and small enough that a long-running
// in-process fleet cannot grow without bound. A resume older than this is not
// an error — see [documents.log].
const logRetained = 4096

func newDocuments() *documents {
	return &documents{
		records:  map[coord.Family]map[string]coord.Record{},
		revision: map[coord.Family]uint64{},
		watchers: map[coord.Family][]*memWatcher{},
		log:      map[coord.Family][]coord.Change{},
	}
}

// family returns the store for one family, refusing an unknown one.
func (d *documents) family(f coord.Family) (map[string]coord.Record, error) {
	if !f.Valid() {
		return nil, coord.ErrUnknownFamily(f)
	}
	if d.records[f] == nil {
		d.records[f] = map[string]coord.Record{}
	}
	return d.records[f], nil
}

// next advances a family's revision and returns it.
func (d *documents) next(f coord.Family) uint64 {
	d.revision[f]++
	return d.revision[f]
}

// publish hands a change to every live watcher of a family.
//
// Under the store's lock, so a watcher's stream is in revision order: two
// writers whose changes interleaved would otherwise deliver out of order, and
// a projection applying them by revision would drop the older one — correct
// for state, wrong for a change log where every entry is its own record.
func (d *documents) publish(f coord.Family, change coord.Change) {
	entries := append(d.log[f], change)
	if len(entries) > logRetained {
		entries = entries[len(entries)-logRetained:]
	}
	d.log[f] = entries
	for _, w := range d.watchers[f] {
		w.send(&change)
	}
	for _, feed := range d.feeds {
		feed.offer(f, change)
	}
}

// replay returns the retained changes at or after a revision, and whether the
// window reached back far enough to be complete.
func (d *documents) replay(f coord.Family, from uint64) ([]coord.Change, bool) {
	entries := d.log[f]
	complete := len(entries) == 0 || entries[0].Revision <= from
	var out []coord.Change
	for _, change := range entries {
		if change.Revision >= from {
			out = append(out, change)
		}
	}
	return out, complete
}

func (f *Fleet) docs() *documents {
	if f.documents == nil {
		f.documents = newDocuments()
	}
	return f.documents
}

// Document reads one document.
func (f *Fleet) Document(_ context.Context, family coord.Family, key string) (coord.Record, bool, error) {
	if key == "" {
		return coord.Record{}, false, errors.New("coord/memory: a document read needs a key")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	store, err := f.docs().family(family)
	if err != nil {
		return coord.Record{}, false, err
	}
	record, ok := store[key]
	if !ok {
		return coord.Record{}, false, nil
	}
	return clone(record), true, nil
}

// DocumentAt reads one document at an exact revision.
//
// The twin holds one revision per key, so this answers only when the version
// asked for is the one it holds. A caller asking for an older revision gets
// the same "not visible here" the KV backend gives for a replica that has not
// caught up — which is the answer that keeps both implementations honest
// about what an exact read can promise.
func (f *Fleet) DocumentAt(_ context.Context, family coord.Family, key string, revision uint64) (coord.Record, bool, error) {
	if key == "" || revision == 0 {
		return coord.Record{}, false, errors.New("coord/memory: a revision read needs a key and a revision")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	store, err := f.docs().family(family)
	if err != nil {
		return coord.Record{}, false, err
	}
	record, ok := store[key]
	if !ok || record.Version != revision {
		return coord.Record{}, false, nil
	}
	return clone(record), true, nil
}

// Documents lists a family under a prefix.
func (f *Fleet) Documents(_ context.Context, family coord.Family, prefix string) ([]coord.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	store, err := f.docs().family(family)
	if err != nil {
		return nil, err
	}
	out := make([]coord.Record, 0, len(store))
	for key, record := range store {
		if prefix != "" && !hasKeyPrefix(key, prefix) {
			continue
		}
		out = append(out, clone(record))
	}
	// Sorted so a listing is stable between calls and between backends: an
	// unordered map range would make two captures of one estate un-diffable.
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// hasKeyPrefix matches on whole segments — see the KV backend's copy for why
// a byte-wise prefix would let one class sweep another's records.
func hasKeyPrefix(key, prefix string) bool {
	if key == prefix {
		return true
	}
	return strings.HasPrefix(key, prefix+coord.KeySeparator)
}

// CreateDocument writes a new document.
func (f *Fleet) CreateDocument(_ context.Context, family coord.Family, key string, value []byte) (bool, error) {
	if key == "" {
		return false, errors.New("coord/memory: a document create needs a key")
	}
	if err := checkSize(key, value); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.docs()
	store, err := d.family(family)
	if err != nil {
		return false, err
	}
	if _, exists := store[key]; exists {
		return false, nil
	}
	record := coord.Record{Key: key, Value: slices.Clone(value), Version: d.next(family)}
	store[key] = record
	d.publish(family, coord.Change{
		Key: key, Value: slices.Clone(value), Op: coord.OpPut, Revision: record.Version,
	})
	return true, nil
}

// UpdateDocument writes at a version.
func (f *Fleet) UpdateDocument(_ context.Context, family coord.Family, key string, value []byte, version uint64) (bool, error) {
	if err := checkSize(key, value); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.docs()
	store, err := d.family(family)
	if err != nil {
		return false, err
	}
	record, ok := store[key]
	if !ok || record.Version != version {
		return false, nil
	}
	record.Value = slices.Clone(value)
	record.Version = d.next(family)
	store[key] = record
	d.publish(family, coord.Change{
		Key: key, Value: slices.Clone(value), Op: coord.OpPut, Revision: record.Version,
	})
	return true, nil
}

// PurgeDocument removes a document at a version.
func (f *Fleet) PurgeDocument(_ context.Context, family coord.Family, key string, version uint64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.docs()
	store, err := d.family(family)
	if err != nil {
		return false, err
	}
	record, ok := store[key]
	if !ok || record.Version != version {
		return false, nil
	}
	delete(store, key)
	// The purge takes a revision of its own: a watcher resuming from the
	// last revision it saw must be handed the removal, and a marker sharing
	// the record's revision would be indistinguishable from the write that
	// preceded it.
	d.publish(family, coord.Change{Key: key, Op: coord.OpPurge, Revision: d.next(family)})
	return true, nil
}

// WatchDocuments follows a family.
func (f *Fleet) WatchDocuments(ctx context.Context, family coord.Family, from uint64) (coord.Watcher, error) {
	f.mu.Lock()
	d := f.docs()
	store, err := d.family(family)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}

	w := newMemWatcher(family, f)

	// The opening pass is taken UNDER THE LOCK together with the
	// registration, so a write that lands between them is delivered live
	// rather than lost: a watcher that registered after its snapshot would
	// miss exactly the writes that happened in the gap, and a projection
	// built from it would be short a document with no way to notice.
	var initial []coord.Change
	snapshot := from == 0
	if !snapshot {
		replayed, complete := d.replay(family, from)
		if complete {
			initial = replayed
		} else {
			// The resume point has aged out of the retained window. A
			// full pass is the honest fallback and what a compacted
			// stream gives too: complete about what IS, silent about
			// what happened in between.
			snapshot = true
		}
	}
	if snapshot {
		keys := make([]string, 0, len(store))
		for key := range store {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			record := store[key]
			initial = append(initial, coord.Change{
				Key: key, Value: slices.Clone(record.Value), Op: coord.OpPut,
				Revision: record.Version, Initial: true,
			})
		}
	}
	d.watchers[family] = append(d.watchers[family], w)
	f.mu.Unlock()

	go func() {
		for i := range initial {
			if !w.send(&initial[i]) {
				return
			}
		}
		if snapshot {
			// The caught-up marker, on an opening pass only. A caller
			// that ignores it cannot tell an empty family from one it
			// has not finished reading.
			w.send(nil)
		}
	}()

	go func() {
		select {
		case <-ctx.Done():
			_ = w.Stop()
		case <-w.done:
		}
	}()
	return w, nil
}

// memWatcher is one live view.
//
// # Why a queue and a relay rather than a plain channel
//
// The contract says a watcher's channel CLOSES when the watch ends, and a
// consumer ranging over it depends on that. But a publisher blocked in a send
// while Stop closes the same channel is a data race — and a close-while-
// sending panic in the twin would surface as a flaky projector test rather
// than as the twin bug it is.
//
// So exactly one goroutine ever touches the channel: the relay. Publishers
// hand changes to a bounded queue, and the relay is the only writer and the
// only closer. The queue is CAP-BOUNDED at the same 256 the real watcher's
// channel carries, so a slow consumer blocks its own watch here exactly as it
// does against the broker — dropping instead would put a hole in a projection
// that nothing detects, which is the failure the twin most needs to be honest
// about.
type memWatcher struct {
	out    chan *coord.Change
	done   chan struct{}
	once   sync.Once
	family coord.Family
	fleet  *Fleet

	mu     sync.Mutex
	room   *sync.Cond
	queued []*coord.Change
	closed bool
}

// memWatchQueue is how many changes a stopped consumer may fall behind by
// before its publisher blocks. The real watcher's channel is 256 deep.
const memWatchQueue = 256

func newMemWatcher(family coord.Family, f *Fleet) *memWatcher {
	w := &memWatcher{
		out:    make(chan *coord.Change),
		done:   make(chan struct{}),
		family: family,
		fleet:  f,
	}
	w.room = sync.NewCond(&w.mu)
	go w.relay()
	return w
}

func (w *memWatcher) Changes() <-chan *coord.Change { return w.out }

// relay is the ONLY writer and the only closer of out.
func (w *memWatcher) relay() {
	defer close(w.out)
	for {
		w.mu.Lock()
		for len(w.queued) == 0 && !w.closed {
			w.room.Wait()
		}
		if len(w.queued) == 0 {
			w.mu.Unlock()
			return
		}
		change := w.queued[0]
		w.queued = w.queued[1:]
		// A publisher may be blocked on a full queue.
		w.room.Broadcast()
		w.mu.Unlock()

		select {
		case w.out <- change:
		case <-w.done:
			return
		}
	}
}

// send delivers a change, reporting whether the watcher is still live.
//
// A SLOW CONSUMER BLOCKS ITS OWN WATCH and nothing else, which is the
// broker's behaviour too. Dropping instead would put a hole in a projection
// that nothing detects.
func (w *memWatcher) send(change *coord.Change) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.queued) >= memWatchQueue && !w.closed {
		w.room.Wait()
	}
	if w.closed {
		return false
	}
	w.queued = append(w.queued, change)
	w.room.Broadcast()
	return true
}

func (w *memWatcher) Stop() error {
	w.once.Do(func() {
		close(w.done)
		w.fleet.mu.Lock()
		d := w.fleet.docs()
		live := d.watchers[w.family]
		for i, other := range live {
			if other == w {
				d.watchers[w.family] = append(live[:i:i], live[i+1:]...)
				break
			}
		}
		w.fleet.mu.Unlock()

		// The relay closes out; it is woken by the flag, and a publisher
		// blocked on a full queue is woken by the same broadcast.
		w.mu.Lock()
		w.closed = true
		w.queued = nil
		w.room.Broadcast()
		w.mu.Unlock()
	})
	return nil
}

func clone(r coord.Record) coord.Record {
	r.Value = slices.Clone(r.Value)
	return r
}

// checkSize enforces the transport ceiling the real backend has.
//
// THE TWIN HAS TO REFUSE WHAT THE BROKER REFUSES, which is the whole reason
// both run one contract suite: a document a test accepts must be one
// production accepts. Without it, the one failure that is NOT worth retrying
// would be reachable only against a real broker — and the code path that
// tells it from an outage would be exercised nowhere.
//
// The ceiling is the QUEUE's, not a second number: both estates ride the same
// broker on the default topology, and a document limit that differed from the
// event limit would be two numbers to keep equal for no reader's benefit.
func checkSize(key string, value []byte) error {
	if len(value) <= queue.MaxPayloadBytes {
		return nil
	}
	return fmt.Errorf("%w: %s encodes to %d bytes, over the %d-byte ceiling",
		coord.ErrDocumentTooLarge, key, len(value), queue.MaxPayloadBytes)
}
