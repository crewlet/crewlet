package kv

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// The document families, as buckets.
//
// ONE BUCKET PER FAMILY rather than one for everything or one per record
// class. The file's own rule is one bucket per RETENTION and all three of
// these are ageless, so the split is by what else differs: their value
// profiles (small hot items against large compressed pages), the sweeps that
// read them, and their lifecycles — vectors are derived and can be dropped
// wholesale, which is a thing you must never do to the pages. A company
// running a native wiki against an external tracker opens only one of them.
const (
	workSuffix      = "_work"
	pagesSuffix     = "_pages"
	kbVectorsSuffix = "_kb_vectors"
)

// bucketFor resolves a family to its bucket, refusing one this build does not
// serve rather than answering nil — a nil bucket would panic on the first
// call, and the caller could not tell a missing family from a broken store.
func (f *FleetStore) bucketFor(family coord.Family) (jetstream.KeyValue, error) {
	switch family {
	case coord.FamilyWork:
		return f.work, nil
	case coord.FamilyPages:
		return f.pages, nil
	case coord.FamilyKBVectors:
		return f.kbVectors, nil
	}
	return nil, coord.ErrUnknownFamily(family)
}

// Document reads one document.
func (f *FleetStore) Document(ctx context.Context, family coord.Family, key string) (coord.Record, bool, error) {
	bucket, err := f.bucketFor(family)
	if err != nil {
		return coord.Record{}, false, err
	}
	if key == "" {
		return coord.Record{}, false, errors.New("coord/kv: a document read needs a key")
	}
	entry, err := bucket.Get(ctx, key)
	switch {
	case err == nil:
		return coord.Record{Key: key, Value: entry.Value(), Version: entry.Revision()}, true, nil
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.Record{}, false, nil
	default:
		return coord.Record{}, false, unavailable("read the document", err)
	}
}

// DocumentAt reads one document at an exact revision.
//
// NOT FOUND HERE IS UNKNOWN, and the contract says so: this is answered by
// whichever replica the direct get reached, from that replica's own store, so
// a follower that has not applied the sequence yet reports nothing for a
// document the caller holds a publish acknowledgement for. Answering "absent"
// would tell a boot reconcile to delete a row that exists.
func (f *FleetStore) DocumentAt(ctx context.Context, family coord.Family, key string, revision uint64) (coord.Record, bool, error) {
	bucket, err := f.bucketFor(family)
	if err != nil {
		return coord.Record{}, false, err
	}
	if key == "" || revision == 0 {
		return coord.Record{}, false, errors.New("coord/kv: a revision read needs a key and a revision")
	}
	entry, err := bucket.GetRevision(ctx, key, revision)
	switch {
	case err == nil:
		return coord.Record{Key: key, Value: entry.Value(), Version: entry.Revision()}, true, nil
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.Record{}, false, unavailable(
			fmt.Sprintf("read %s at revision %d — the replica that answered has not "+
				"applied that sequence yet, which is not the same as the document "+
				"being gone", key, revision), err)
	default:
		return coord.Record{}, false, unavailable("read the document revision", err)
	}
}

// Documents lists a family under a prefix.
func (f *FleetStore) Documents(ctx context.Context, family coord.Family, prefix string) ([]coord.Record, error) {
	bucket, err := f.bucketFor(family)
	if err != nil {
		return nil, err
	}
	var out []coord.Record
	watcher, err := bucket.WatchAll(ctx, jetstream.IgnoreDeletes())
	if err != nil {
		return nil, unavailable("list the family", err)
	}
	defer func() { _ = watcher.Stop() }()
	for entry := range watcher.Updates() {
		if entry == nil {
			// The initial pass is complete, which for a listing is the
			// whole answer.
			break
		}
		if prefix != "" && !hasKeyPrefix(entry.Key(), prefix) {
			continue
		}
		out = append(out, coord.Record{
			Key: entry.Key(), Value: entry.Value(), Version: entry.Revision(),
		})
	}
	return out, ctx.Err()
}

// hasKeyPrefix matches on WHOLE SEGMENTS.
//
// A prefix of "c" must select "c.item.ulid" and never "counter.ENG", which a
// byte-wise prefix test would get wrong — the class is a subject token, and a
// listing that leaked one class into another's sweep would purge live records.
func hasKeyPrefix(key, prefix string) bool {
	if key == prefix {
		return true
	}
	return len(key) > len(prefix) &&
		key[:len(prefix)] == prefix &&
		key[len(prefix):len(prefix)+1] == coord.KeySeparator
}

// CreateDocument writes a new document.
func (f *FleetStore) CreateDocument(ctx context.Context, family coord.Family, key string, value []byte) (bool, error) {
	bucket, err := f.bucketFor(family)
	if err != nil {
		return false, err
	}
	if key == "" {
		return false, errors.New("coord/kv: a document create needs a key")
	}
	_, err = bucket.Create(ctx, key, value)
	switch {
	case err == nil:
		return true, nil
	case lostCreateRace(err):
		return false, nil
	case tooLarge(err):
		return false, oversized(key, value, err)
	default:
		return false, unavailable("create the document", err)
	}
}

// tooLarge reports a write the broker refused for its size.
//
// The client refuses it LOCALLY, before the wire, against the MaxPayload it
// read off the server's INFO — which is why this is a client error rather
// than a connection the broker closes underneath us.
func tooLarge(err error) bool { return errors.Is(err, nats.ErrMaxPayload) }

// oversized names the document, its size, and what to do about it.
//
// THE SIZE IS IN THE MESSAGE, because it is the one number an operator needs
// and the one the failure otherwise hides: the content caps in [work] and
// [pages] bound what a person WROTE, and this bounds what the encoded
// document came to. A page of angle brackets re-encodes at six times its
// length, so the two are not the same number and no ratio at the edge makes
// them one.
func oversized(key string, value []byte, err error) error {
	return fmt.Errorf("%w: %s encodes to %d bytes, which this broker will not "+
		"carry. Shorten it, or raise max_payload on the NATS server this fleet "+
		"dials — the embedded broker is already set to the engine's own "+
		"ceiling: %w", coord.ErrDocumentTooLarge, key, len(value), err)
}

// UpdateDocument writes at a version.
func (f *FleetStore) UpdateDocument(ctx context.Context, family coord.Family, key string, value []byte, version uint64) (bool, error) {
	bucket, err := f.bucketFor(family)
	if err != nil {
		return false, err
	}
	_, err = bucket.Update(ctx, key, value, version)
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	case tooLarge(err):
		return false, oversized(key, value, err)
	default:
		return false, unavailable("update the document", err)
	}
}

// PurgeDocument removes a document at a version.
func (f *FleetStore) PurgeDocument(ctx context.Context, family coord.Family, key string, version uint64) (bool, error) {
	bucket, err := f.bucketFor(family)
	if err != nil {
		return false, err
	}
	err = bucket.Purge(ctx, key, jetstream.LastRevision(version))
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	default:
		return false, unavailable("purge the document", err)
	}
}

// WatchDocuments follows a family.
func (f *FleetStore) WatchDocuments(ctx context.Context, family coord.Family, from uint64) (coord.Watcher, error) {
	bucket, err := f.bucketFor(family)
	if err != nil {
		return nil, err
	}
	// DELETES ARE NOT IGNORED. A projection that never saw a removal keeps
	// serving a purged item forever, and unlike the memory changelog there
	// is no lifecycle pass to drop it again.
	opts := []jetstream.WatchOpt{}
	if from > 0 {
		opts = append(opts, jetstream.ResumeFromRevision(from))
	}
	watcher, err := bucket.WatchAll(ctx, opts...)
	if err != nil {
		return nil, unavailable("watch the family", err)
	}
	return &kvWatcher{watcher: watcher, out: relay(ctx, watcher)}, nil
}

// kvWatcher adapts the client's watcher to the contract's.
type kvWatcher struct {
	watcher jetstream.KeyWatcher
	out     <-chan *coord.Change
}

func (w *kvWatcher) Changes() <-chan *coord.Change { return w.out }

func (w *kvWatcher) Stop() error { return w.watcher.Stop() }

// relay translates entries into changes on their own goroutine.
//
// ITS OWN GOROUTINE, AND IT NEVER BLOCKS ON A CONSUMER'S WORK. The client's
// watcher channel is bounded and blocks by design when it fills, and it is
// fed by the connection's read loop — the same loop carrying every seat's
// mailbox on this node. A projector that applied changes inline would
// therefore stall the node's inbox whenever a transaction ran long, so the
// translation is what the read loop sees and the caller does its work
// downstream of a channel it owns.
func relay(ctx context.Context, watcher jetstream.KeyWatcher) <-chan *coord.Change {
	out := make(chan *coord.Change, 256)
	go func() {
		defer close(out)
		for entry := range watcher.Updates() {
			var change *coord.Change
			if entry != nil {
				op := coord.OpPut
				switch entry.Operation() {
				case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
					op = coord.OpPurge
				}
				change = &coord.Change{
					Key:      entry.Key(),
					Value:    entry.Value(),
					Op:       op,
					Revision: entry.Revision(),
					Initial:  entry.Delta() > 0,
				}
			}
			select {
			case out <- change:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// lostCreateRace reports whether a create lost to a first writer.
//
// THREE SHAPES FOR ONE FACT, and every one of them is somebody else's second
// writer. ErrKeyExists is the ordinary case. A revision mismatch is what the
// client reports when the key carried a delete or purge marker it tried to
// step over and lost. And on a REPLICATED stream a share of those losers come
// back as a bare API error the client wraps in neither sentinel — measured at
// three replicas, where a fifth of the losers of a create over a marker
// arrived that way.
//
// Getting this wrong is not loud. A lost race read as an outage makes a claim
// answer "unknown", the caller fails open, and the delivery it was meant to
// deduplicate is processed twice — on a clustered estate only, which is
// exactly where nobody is running the single-server suite that would show it.
func lostCreateRace(err error) bool {
	return errors.Is(err, jetstream.ErrKeyExists) ||
		errors.Is(err, jetstream.ErrKeyRevisionMismatch) ||
		isWrongLastSequence(err)
}

// lostUpdateRace reports whether a conditional write lost its race.
//
// A deleted key lands here too: the record it was conditioned on is gone,
// which is the same answer for the caller — re-read and re-decide.
func lostUpdateRace(err error) bool {
	return errors.Is(err, jetstream.ErrKeyRevisionMismatch) ||
		errors.Is(err, jetstream.ErrKeyNotFound) ||
		isWrongLastSequence(err)
}
