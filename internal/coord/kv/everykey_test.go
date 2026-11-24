package kv

import (
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// keyWrites records every value the KV backend writes to the buckets on one
// server, from the write itself as the broker carries it: a put's subject
// ($KV.<bucket>.<key>) and its bytes, never a removal. It is the gate that
// holds every key the package writes to a class declared for it
// ([keyWrites.check]); the stores a suite opens are registered with it
// ([keyWrites.observe]), so a bucket added to them is held unlisted.
type keyWrites struct {
	nc *nats.Conn

	mu sync.Mutex
	// largest is the largest value written under each key of each bucket.
	largest map[string]map[string]int
	// opened is the holding of every bucket a registered store settled, and
	// the bucket's name with its store's prefix taken off.
	opened map[string]openedBucket
}

// openedBucket is a bucket a registered store settled: its holding and its
// suffix, the name every store's copy of it shares.
type openedBucket struct {
	holding holding
	suffix  string
}

// recordKeyWrites starts a recorder of every bucket on srv, from before the
// first write of any store a case opens there.
func recordKeyWrites(t *testing.T, srv *server.Server) *keyWrites {
	t.Helper()
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("connect the recorder: %v", err)
	}
	t.Cleanup(nc.Close)
	w := &keyWrites{nc: nc, largest: map[string]map[string]int{}, opened: map[string]openedBucket{}}
	if _, err := nc.Subscribe("$KV.>", w.put); err != nil {
		t.Fatalf("subscribe the recorder: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return w
}

// put records one message on a bucket's subject that carries a value.
func (w *keyWrites) put(m *nats.Msg) {
	if m.Header.Get("KV-Operation") != "" {
		return
	}
	bucket, key, ok := strings.Cut(strings.TrimPrefix(m.Subject, "$KV."), ".")
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.largest[bucket] == nil {
		w.largest[bucket] = map[string]int{}
	}
	w.largest[bucket][key] = max(w.largest[bucket][key], len(m.Data))
}

// observe registers every bucket handle a store opened under prefix holds,
// a fleet store's or a lease store's, by its holding.
func (w *keyWrites) observe(t testing.TB, prefix string, store any) {
	t.Helper()
	var handles []jetstream.KeyValue
	switch s := store.(type) {
	case *FleetStore:
		fields := reflect.ValueOf(s).Elem()
		for i := range fields.NumField() {
			if f := fields.Field(i); f.Type() == reflect.TypeFor[jetstream.KeyValue]() {
				// The field is the package's own, so it is read as itself:
				// reflection hands out no unexported field's value otherwise.
				handle := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface()
				handles = append(handles, handle.(jetstream.KeyValue))
			}
		}
	case *Store:
		handles = append(handles, s.leases.kv, s.duties.kv, s.counter.kv)
	default:
		t.Fatalf("observe a %T, which is no store of this package", store)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, h := range handles {
		b := h.(settledBucket)
		name := b.Bucket()
		w.opened[name] = openedBucket{holding: b.holding, suffix: strings.TrimPrefix(name, prefix)}
	}
}

// writes is every key recorded, by bucket: after a round trip every write the
// broker took is queued to the recorder, and after a barrier queued behind
// its pending messages every one of them has been handled.
func (w *keyWrites) writes(t *testing.T) map[string]map[string]int {
	t.Helper()
	if err := w.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{})
	if err := w.nc.Barrier(func() { close(handled) }); err != nil {
		t.Fatal(err)
	}
	<-handled
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.largest
}

// check fails t for every key written to a bucket that declares classes and
// matching none of them, for every value past its key's class bound, and for
// a write to a bucket no registered store opened. Premise: every bucket whose
// suffix owns names saw a write, and every class its holding declares was
// written.
func (w *keyWrites) check(t *testing.T, owns func(suffix string) bool) {
	t.Helper()
	written := w.writes(t)
	hit := map[string]map[string]bool{}
	for bucket, keys := range written {
		opened, ok := w.opened[bucket]
		if !ok {
			t.Errorf("the store wrote to %s, which no store the suite opened holds", bucket)
			continue
		}
		h := opened.holding
		if hit[opened.suffix] == nil {
			hit[opened.suffix] = map[string]bool{}
		}
		for _, key := range slices.Sorted(func(yield func(string) bool) {
			for k := range keys {
				if !yield(k) {
					return
				}
			}
		}) {
			c := h.rest
			i := slices.IndexFunc(h.classes, func(k keyClass) bool { return server.SubjectMatchesFilter(key, k.keys) })
			switch {
			case i >= 0:
				c = h.classes[i].class
				hit[opened.suffix][h.classes[i].keys] = true
			// A family the positions register declares no class for yet falls
			// to its rest, the one rest still undeclared.
			case len(h.classes) > 0 && h.rest != undeclared:
				t.Errorf("the store wrote %q to %s, which declares classes and falls to its rest", key, bucket)
			}
			if keys[key] > c.bound {
				t.Errorf("the store wrote %d bytes under %s's %q, past its class's %d", keys[key], bucket, key,
					c.bound)
			}
		}
		hit[opened.suffix][""] = true
	}
	owned := map[string]holding{}
	for _, opened := range w.opened {
		if owns(opened.suffix) {
			owned[opened.suffix] = opened.holding
		}
	}
	for suffix, h := range owned {
		if !hit[suffix][""] {
			t.Errorf("premise: the suite wrote nothing to any %s bucket", suffix)
		}
		for _, k := range h.classes {
			if !hit[suffix][k.keys] {
				t.Errorf("premise: the suite wrote no key %s declares %q for", suffix, k.keys)
			}
		}
	}
	if len(owned) == 0 {
		t.Fatal("premise: no bucket the suite owns was opened, so the recorder read nothing")
	}
}
