package kv

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the placement maps ------------------------------------------------ //
//
// Two records, each the only key of its own bucket and each changed only by
// compare-and-set: the object store's placement map and the estate map. Their
// read, create and update are one implementation below — the two maps differ
// in what they place and in who watches them, never in how a version is
// arbitrated — and only the estate map is watched.

// mapKey is the one key a placement map's bucket holds.
const mapKey = "map"

// readMap reads a placement map's record, reporting false when none was ever
// written. what names the map in an error.
func (f *FleetStore) readMap(ctx context.Context, kv jetstream.KeyValue, what string) ([]byte, uint64, bool, error) {
	entry, err := f.get(ctx, kv, mapKey)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, 0, false, nil
	case err != nil:
		return nil, 0, false, unavailable("read "+what, err)
	}
	return entry.Value(), entry.Revision(), true, nil
}

// createMap writes a placement map's first record, leaving an existing one
// alone.
//
// Create rather than Put, so two duty holders racing on an empty fleet write
// one map between them and the loser updates the winner's.
func (f *FleetStore) createMap(ctx context.Context, kv jetstream.KeyValue, value []byte, what string) (uint64, bool, error) {
	if len(value) == 0 {
		return 0, false, fmt.Errorf("coord/kv: %s needs a value", what)
	}
	revision, err := f.create(ctx, kv, mapKey, value)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		return 0, false, nil
	case err != nil:
		return 0, false, unavailable("create "+what, err)
	}
	return revision, true, nil
}

// updateMap writes a placement map at the version it was read at.
func (f *FleetStore) updateMap(ctx context.Context, kv jetstream.KeyValue, value []byte, version uint64, what string) (uint64, bool, error) {
	if len(value) == 0 {
		return 0, false, fmt.Errorf("coord/kv: %s needs a value", what)
	}
	if version == 0 {
		// NO VERSION IS A LOST RACE, never an unconditional write: the
		// client reads an expected revision of 0 as "must not exist yet".
		return 0, false, nil
	}
	revision, err := kv.Update(ctx, mapKey, value, version)
	switch {
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, unavailable("update "+what, err)
	}
	return revision, true, nil
}

const (
	objectMapWhat = "the object placement map"
	estateMapWhat = "the estate map"
)

// ObjectMap reads the object placement map.
func (f *FleetStore) ObjectMap(ctx context.Context) (coord.ObjectMapRecord, bool, error) {
	value, version, found, err := f.readMap(ctx, f.objects, objectMapWhat)
	if !found || err != nil {
		return coord.ObjectMapRecord{}, false, err
	}
	return coord.ObjectMapRecord{Value: value, Version: version}, true, nil
}

// CreateObjectMap writes the first object placement map.
func (f *FleetStore) CreateObjectMap(ctx context.Context, value []byte) (coord.ObjectMapRecord, bool, error) {
	version, created, err := f.createMap(ctx, f.objects, value, objectMapWhat)
	if !created || err != nil {
		return coord.ObjectMapRecord{}, false, err
	}
	return coord.ObjectMapRecord{Value: value, Version: version}, true, nil
}

// UpdateObjectMap writes the object placement map at the version it was read
// at.
func (f *FleetStore) UpdateObjectMap(ctx context.Context, value []byte, version uint64) (coord.ObjectMapRecord, bool, error) {
	version, won, err := f.updateMap(ctx, f.objects, value, version, objectMapWhat)
	if !won || err != nil {
		return coord.ObjectMapRecord{}, false, err
	}
	return coord.ObjectMapRecord{Value: value, Version: version}, true, nil
}

// EstateMap reads the estate map.
func (f *FleetStore) EstateMap(ctx context.Context) (coord.EstateMapRecord, bool, error) {
	value, version, found, err := f.readMap(ctx, f.estate, estateMapWhat)
	if !found || err != nil {
		return coord.EstateMapRecord{}, false, err
	}
	return coord.EstateMapRecord{Value: value, Version: version}, true, nil
}

// CreateEstateMap writes the first estate map.
func (f *FleetStore) CreateEstateMap(ctx context.Context, value []byte) (coord.EstateMapRecord, bool, error) {
	version, created, err := f.createMap(ctx, f.estate, value, estateMapWhat)
	if !created || err != nil {
		return coord.EstateMapRecord{}, false, err
	}
	return coord.EstateMapRecord{Value: value, Version: version}, true, nil
}

// UpdateEstateMap writes the estate map at the version it was read at.
func (f *FleetStore) UpdateEstateMap(ctx context.Context, value []byte, version uint64) (coord.EstateMapRecord, bool, error) {
	version, won, err := f.updateMap(ctx, f.estate, value, version, estateMapWhat)
	if !won || err != nil {
		return coord.EstateMapRecord{}, false, err
	}
	return coord.EstateMapRecord{Value: value, Version: version}, true, nil
}

// WatchEstateMap delivers every stored version of the estate map from the
// current one on ([coord.EstateMaps]).
//
// THE CURRENT VERSION IS THE LEADER'S, never the watch's. The watch is the
// client's ordered consumer over the one key, and the broker places that
// consumer — one replica, in memory — on a member it picks at random from the
// stream's peers (nats-server's createGroupForConsumer), which may be a
// follower behind the quorum: a node that had just read version V through the
// leader, or written it, would open the watch and be handed V-1 first, and a
// router or a joiner would act on a map the fleet had already replaced. That
// is the same copy behind the same quorum that kv.go's "Every single-key read
// is the leader's" rules out for every read by key, and the watch keeps the
// rule the same way the gates' view does (gate.go): the leader answers where
// the map IS, and the watch only says where it goes next. So the map is read
// from the leader first and handed over, and a version the watch then offers
// is forwarded only if it is NEWER than every version already accounted for
// ([forwardEstateMap]) — a copy that is behind can delay the next version,
// never hand over an older one.
//
// Read BEFORE the watch opens, and nothing is missed between: the consumer
// starts at its replica's last version of the key and delivers every later
// one, so whatever was written after the leader answered is still to come —
// unless a later write replaced it first, the skip the contract allows. The
// bucket keeps one version of the key, so a write that replaced another
// before the consumer was sent it leaves nothing to send, which is the reason
// the contract allows it.
//
// A REMOVAL ENDS THE WATCH once a map has been handed over. Nothing in the
// engine removes the map, so a delete or purge is an operator's hand on the
// bucket, and a watch that went on delivering nothing after it would leave its
// reader routing by a map that no longer exists; closed, the reader re-opens
// and reads, and the read answers that there is none. A removal before any map
// was handed over is simply the absence of a map, and is not delivered.
//
// THE FORWARDER OWNS THE WATCHER: it stops it on every way out, and then
// drains it, because the client's delivery goroutine blocks handing an entry
// to a full 256-entry buffer while it holds the watcher's lock, and only
// reaches the close that ends it once somebody takes that entry.
func (f *FleetStore) WatchEstateMap(ctx context.Context) (<-chan coord.EstateMapRecord, error) {
	read, err := newLeaderReader(f.js, f.estate)
	if err != nil {
		return nil, unavailable("read "+estateMapWhat, err)
	}
	current, err := read.last(ctx, mapKey)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		current = nil
	case err != nil:
		return nil, unavailable("read "+estateMapWhat, err)
	}
	w, err := f.estate.Watch(ctx, mapKey)
	if err != nil {
		return nil, unavailable("watch "+estateMapWhat, err)
	}
	out := make(chan coord.EstateMapRecord)
	go func() {
		defer close(out)
		defer func() {
			_ = w.Stop()
			for range w.Updates() {
			}
		}()
		forwardEstateMap(ctx, current, w.Updates(), out, f.estate.Bucket())
	}()
	return out, nil
}

// forwardEstateMap hands current over — the leader's newest message of the
// key, nil when it has none — and then every version updates offers that is
// newer than every version accounted for, until ctx ends, updates closes or
// the map is removed after one was handed over.
//
// A VERSION NO NEWER THAN ONE ACCOUNTED FOR IS A COPY BEHIND, and is dropped:
// a version handed over, and the leader's message itself even when that is a
// removal, since the leader said the map is past it. Nothing else is judged
// here — the revision is the stream's sequence, so newer is larger.
func forwardEstateMap(ctx context.Context, current jetstream.KeyValueEntry,
	updates <-chan jetstream.KeyValueEntry, out chan<- coord.EstateMapRecord, bucket string) {

	var floor uint64
	handed := false
	send := func(entry jetstream.KeyValueEntry) bool {
		select {
		case out <- coord.EstateMapRecord{Value: entry.Value(), Version: entry.Revision()}:
			handed = true
			return true
		case <-ctx.Done():
			return false
		}
	}
	if current != nil {
		floor = current.Revision()
		if current.Operation() == jetstream.KeyValuePut && !send(current) {
			return
		}
	}
	for {
		var entry jetstream.KeyValueEntry
		select {
		case <-ctx.Done():
			return
		case e, ok := <-updates:
			if !ok {
				return
			}
			entry = e
		}
		// The end-of-initial-values marker is the client's own guess and
		// carries nothing.
		if entry == nil || entry.Revision() <= floor {
			continue
		}
		floor = entry.Revision()
		if entry.Operation() != jetstream.KeyValuePut {
			if !handed {
				continue
			}
			log.WarnContext(ctx, "coord_kv_estate_map_removed",
				"bucket", bucket, "revision", entry.Revision(),
				"detail", "the estate map was removed from its bucket, which nothing in the "+
					"engine does; every watch of it ends, and a node reading it finds none")
			return
		}
		if !send(entry) {
			return
		}
	}
}
