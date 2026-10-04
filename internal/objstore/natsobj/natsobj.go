// Package natsobj keeps the object store's chunks in the fleet's own NATS
// JetStream object store — the default backend, and the one that needs
// nothing the engine does not already run.
//
// # Why the broker
//
// The data nodes ARE the broker's JetStream members: every stream the estate
// is derived from is replicated across them at `stream.replicas`, and a node
// without `data` reaches it across its leaf link. A company's files are one
// more replicated stream on the same members, at the same count — so a
// member that fails is caught up by the broker as every other stream is, a
// backup copies the bucket with the streams it already copies, and there is no
// placement, repair or second membership to run beside it.
//
// What it costs is that one bucket lives on one replica set: every member
// holding a copy holds every chunk. For a fleet of three or five data nodes
// at three copies that is what any placement would have done anyway; a
// company whose files outgrow one member's disk takes the S3 backend
// (internal/objstore/s3obj) instead.
//
// # One object per chunk
//
// Each chunk is an object named by its hash. The object store cuts it into
// its own smaller pieces on the wire, so a chunk never has to fit the
// broker's message size, and a put of a name already held replaces it and
// moves its modification time — the property [objstore.Backend.Put] asks for.
package natsobj

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
)

// Bucket is the object store bucket the chunks live in, and Stream the
// JetStream stream that backs it — what a backup's stream snapshots carry the
// chunks in.
const (
	Bucket = "crewlet_files"
	Stream = "OBJ_" + Bucket
)

// Config is how the bucket is created.
type Config struct {
	// Replicas is the copies the broker keeps: the stream's own
	// `stream.replicas`, so the files survive exactly what the estate
	// survives.
	Replicas int
	// Clustered says the broker is a cluster, which is what the
	// provisioning budget branches on (see [jsprovision]).
	Clustered bool
}

// Backend is the chunks in the broker's object store.
type Backend struct {
	store jetstream.ObjectStore
}

// Open creates the bucket if it is not there and binds to it.
//
// THROUGH [jsprovision.Place], as every replicated create at boot is: a
// forming cluster answers "no suitable peers" until it has its members, and
// drops a metadata request it cannot place rather than refusing it.
func Open(ctx context.Context, js jetstream.JetStream, cfg Config) (*Backend, error) {
	if js == nil {
		return nil, errors.New("natsobj: the nats object store needs the broker's " +
			"JetStream, and this node has none")
	}
	replicas := max(cfg.Replicas, 1)
	ctx, cancel := context.WithTimeout(ctx, jsprovision.Clustered(cfg.Clustered).Budget())
	defer cancel()
	var store jetstream.ObjectStore
	err := jsprovision.Place(ctx, jsprovision.Clustered(cfg.Clustered).AskTerm(),
		func(ctx context.Context) error {
			made, err := js.CreateOrUpdateObjectStore(ctx, jetstream.ObjectStoreConfig{
				Bucket:      Bucket,
				Description: "Crewlet file chunks, named by their SHA-256",
				Storage:     jetstream.FileStorage,
				Replicas:    replicas,
			})
			if err == nil {
				store = made
			}
			return err
		}, nil)
	if err != nil {
		return nil, fmt.Errorf("natsobj: create the %s bucket at %d copies: %w", Bucket, replicas, err)
	}
	return &Backend{store: store}, nil
}

// Put implements [objstore.Backend].
func (b *Backend) Put(ctx context.Context, h objstore.Hash, data []byte) error {
	if _, err := b.store.PutBytes(ctx, string(h), data); err != nil {
		return fmt.Errorf("natsobj: put %s: %w", h, err)
	}
	return nil
}

// Get implements [objstore.Backend].
func (b *Backend) Get(ctx context.Context, h objstore.Hash) ([]byte, error) {
	data, err := b.store.GetBytes(ctx, string(h))
	if errors.Is(err, jetstream.ErrObjectNotFound) {
		return nil, objstore.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("natsobj: get %s: %w", h, err)
	}
	return data, nil
}

// Stat implements [objstore.Backend].
func (b *Backend) Stat(ctx context.Context, h objstore.Hash) (time.Time, error) {
	info, err := b.store.GetInfo(ctx, string(h))
	if errors.Is(err, jetstream.ErrObjectNotFound) {
		return time.Time{}, objstore.ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("natsobj: stat %s: %w", h, err)
	}
	if info.Deleted {
		return time.Time{}, objstore.ErrNotFound
	}
	return info.ModTime.UTC(), nil
}

// Delete implements [objstore.Backend].
func (b *Backend) Delete(ctx context.Context, h objstore.Hash) error {
	err := b.store.Delete(ctx, string(h))
	if err == nil || errors.Is(err, jetstream.ErrObjectNotFound) {
		return nil
	}
	return fmt.Errorf("natsobj: delete %s: %w", h, err)
}

// List implements [objstore.Backend].
//
// A WATCH OVER THE BUCKET'S METADATA, streamed rather than gathered: the
// library's own List holds every object's info in one slice, which is the
// company's whole inventory in memory at once. A name that is not a content
// hash is somebody else's object and is not visited — the bucket is the
// engine's, but the collector must never be handed a name it would delete as
// garbage on the strength of a parse it skipped.
func (b *Backend) List(ctx context.Context, visit func(objstore.Held) error) error {
	watcher, err := b.store.Watch(ctx, jetstream.IgnoreDeletes())
	if err != nil {
		return fmt.Errorf("natsobj: list the bucket: %w", err)
	}
	defer func() { _ = watcher.Stop() }()
	updates := watcher.Updates()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case info, open := <-updates:
			if !open {
				return errors.New("natsobj: the bucket's listing ended before it was complete")
			}
			if info == nil {
				return nil // the initial values are all delivered
			}
			h, perr := objstore.ParseHash(info.Name)
			if perr != nil || info.Deleted {
				continue
			}
			if err := visit(objstore.Held{Hash: h, Written: info.ModTime.UTC()}); err != nil {
				return err
			}
		}
	}
}
