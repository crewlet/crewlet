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
// # One object per chunk, streamed in full messages
//
// Each chunk is an object named by its hash, and the object store carries an
// object as a run of messages on a subject of its own, closed by one message
// naming them — so an object never has to fit the broker's message size, and
// a put of a name already held replaces it and moves its modification time,
// the property [objstore.Backend.Put] asks for. Three rules make the library's
// put and get safe to stream through, each the answer to a measured failure:
//
//   - EVERY MESSAGE BUT THE LAST IS FULL ([MessageBytes]). The library sends
//     one message per Read of the reader it is handed, so a reader that
//     answers a few bytes at a time — a network body — would store an object
//     as thousands of tiny messages; a put reads through a filler that
//     answers only full ones, and ends the object only where its reader
//     answers io.EOF itself ([objstore.Fill]) — never at a body cut short.
//   - A PUT IS ENDED BY ITS READER, never by its context. A put whose
//     context ends purges the pieces it stored under that same, dead
//     context, which fails, and leaves them on the stream with nothing
//     naming them; one whose reader fails purges them with a live one. So
//     the library's put runs under a context that does not end with the
//     caller's, and the caller giving up reaches it as a read error
//     ([objstore.ContextReader]).
//   - A GET CARRIES ITS CALLER'S DEADLINE. The library bounds a get whose
//     context has none by its own five-second default — over the WHOLE
//     download, the stream included — so a get with no deadline is refused
//     ([objstore.CheckRead]) rather than cut five seconds in.
package natsobj

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
)

var log = logging.Get("objstore")

// Bucket is the object store bucket the chunks live in, and Stream the
// JetStream stream that backs it — what a backup's stream snapshots carry the
// chunks in.
const (
	Bucket = "crewlet_files"
	Stream = "OBJ_" + Bucket
)

// MessageBytes is how many bytes of an object one broker message carries —
// every message of a put but its last.
//
// 128 KiB: the library's own default, and so what every object a fleet already
// holds was cut into. Far under the broker's 8 MiB payload ceiling
// (queue.MaxPayloadBytes) on purpose: a node without `data` sends its uploads
// across the same leaf link as its events and its coordination, and a message
// holds that link for as long as it takes to cross it. A gibibyte is 8,192 of
// them.
const MessageBytes = 128 << 10

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

// Backend is the objects in the broker's object store.
type Backend struct {
	store jetstream.ObjectStore
	// stream is the stream behind the bucket, which a listing reads the
	// objects' metadata from directly — see [Backend.List].
	stream jetstream.Stream
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
			// NO BYTE CEILING (MaxBytes): the engine reserves one for
			// the state logs alone — the company's own records, whose
			// growth the trim governs — and every other stream it runs,
			// the mailboxes and every coordination bucket, declares none
			// and is bounded by what writes it. This one is bounded by
			// the collector, which deletes what no row names a day
			// after it was written. A ceiling here would be a
			// reservation carved out of the budget the logs' ceilings
			// draw on, for bytes no log is waiting for.
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
	stream, err := js.Stream(ctx, Stream)
	if err != nil {
		return nil, fmt.Errorf("natsobj: bind the %s stream: %w", Stream, err)
	}
	return &Backend{store: store, stream: stream}, nil
}

// Put implements [objstore.Backend].
//
// UNDER [context.WithoutCancel] and with no deadline, so the library bounds
// each message's acknowledgement by its own default instead, and every way
// the put can fail — the caller's context ending among them — reaches it as a
// failed read, which it cleans up after under a context that still works. See
// the package doc.
func (b *Backend) Put(ctx context.Context, name string, r io.Reader, m objstore.PutMeta) error {
	meta := jetstream.ObjectMeta{Name: name,
		Opts: &jetstream.ObjectMetaOptions{ChunkSize: MessageBytes}}
	if m.ContentType != "" {
		meta.Headers = nats.Header{"Content-Type": []string{m.ContentType}}
	}
	body := filler{r: objstore.ContextReader(ctx, r)}
	if _, err := b.store.Put(context.WithoutCancel(ctx), meta, body); err != nil {
		return fmt.Errorf("natsobj: put %s: %w", name, err)
	}
	return nil
}

// filler hands the library only full messages: every Read answers as many
// bytes as it was asked for, short only at the end of the object.
//
// The library publishes one message per Read and cuts nothing itself, so
// without this a body arriving a few hundred bytes at a time is stored as a
// message per few hundred bytes — a gibibyte as millions of messages.
//
// THROUGH [objstore.Fill], which is what keeps a failure a failure: the
// library compares the error it is handed against io.EOF to find the end and
// purges what it stored on anything else, so the end has to reach it as
// io.EOF itself and every failure — a body cut short's io.ErrUnexpectedEOF
// among them — as the failure it is.
type filler struct {
	r io.Reader
}

func (f filler) Read(p []byte) (int, error) {
	n, end, err := objstore.Fill(f.r, p)
	if end {
		return n, io.EOF
	}
	return n, err
}

// Get implements [objstore.Backend].
//
// THE LIBRARY HAS NO RANGED READ, so a range is the object streamed from its
// start with everything before the offset discarded: a read at offset off
// costs off bytes from the broker.
func (b *Backend) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	if err := objstore.CheckRead(ctx, off, n); err != nil {
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	}
	res, err := b.store.Get(ctx, name)
	if errors.Is(err, jetstream.ErrObjectNotFound) {
		return nil, objstore.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	}
	info, err := res.Info()
	if err != nil {
		_ = res.Close()
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	}
	size := int64(info.Size)
	if off >= size {
		_ = res.Close()
		return io.NopCloser(eof{}), nil
	}
	r := &object{ctx: ctx, name: name, res: res, size: size}
	if off > 0 {
		if _, err := io.CopyN(io.Discard, r, off); err != nil {
			_ = res.Close()
			return nil, fmt.Errorf("natsobj: get %s: skip to offset %d: %w", name, off, err)
		}
	}
	if n < 0 {
		return r, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(r, n), r}, nil
}

// eof is an empty stream.
type eof struct{}

func (eof) Read([]byte) (int, error) { return 0, io.EOF }

// object is one get's stream, its failures told apart.
//
// THE LIBRARY REPORTS TWO DIFFERENT THINGS AS A DIGEST MISMATCH: bytes that
// arrived whole and hash to something else, which is a corrupt object, and a
// stream that stopped short — a deadline, a broken link — which closes its
// pipe and is hashed as if it had ended there. Only the first is
// [objstore.ErrCorrupt]: a stream cut by a slow read raising a corruption
// alarm would send an operator to restore an object nothing is wrong with.
type object struct {
	ctx  context.Context
	name string
	res  jetstream.ObjectResult
	size int64
	// read is how many bytes the library has handed over, the ones
	// skipped to an offset included.
	read int64
}

func (o *object) Read(p []byte) (int, error) {
	n, err := o.res.Read(p)
	o.read += int64(n)
	switch {
	case o.read > o.size:
		return n, fmt.Errorf("%w: %s streamed past the %d bytes it was stored as",
			objstore.ErrCorrupt, o.name, o.size)
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF) && o.read == o.size:
		return n, io.EOF
	case errors.Is(err, io.EOF):
		// A CLEAN END, SHORT: the bytes match the digest and not the
		// size the object was recorded with — the record is wrong, and
		// reading it again ends here again.
		return n, fmt.Errorf("%w: %s ended at %d of the %d bytes it was stored as",
			objstore.ErrCorrupt, o.name, o.read, o.size)
	case errors.Is(err, jetstream.ErrDigestMismatch) && o.read == o.size && o.ctx.Err() == nil:
		return n, fmt.Errorf("%w: %s does not hash to the digest it was stored with",
			objstore.ErrCorrupt, o.name)
	case errors.Is(err, jetstream.ErrDigestMismatch):
		err = io.ErrUnexpectedEOF
	}
	return n, fmt.Errorf("natsobj: read %s: the stream stopped at %d of %d bytes: %w",
		o.name, o.read, o.size, err)
}

func (o *object) Close() error { return o.res.Close() }

// Stat implements [objstore.Backend].
func (b *Backend) Stat(ctx context.Context, name string) (objstore.Info, error) {
	info, err := b.store.GetInfo(ctx, name)
	if errors.Is(err, jetstream.ErrObjectNotFound) {
		return objstore.Info{}, objstore.ErrNotFound
	}
	if err != nil {
		return objstore.Info{}, fmt.Errorf("natsobj: stat %s: %w", name, err)
	}
	return infoOf(info), nil
}

// infoOf is what the library says of an object, in the store's terms.
func infoOf(info *jetstream.ObjectInfo) objstore.Info {
	return objstore.Info{Name: info.Name, Size: int64(info.Size),
		Written: info.ModTime.UTC(), Digest: digestOf(info.Digest)}
}

// digestOf is the library's `SHA-256=<base64>` digest as a [objstore.Hash],
// empty for one it did not write: an object some other tool put in the
// bucket has no digest the engine can compare against, and reads as an
// object from a backend that keeps none.
func digestOf(digest string) objstore.Hash {
	sum, err := jetstream.DecodeObjectDigest(digest)
	if err != nil || len(sum) != 32 {
		return ""
	}
	return objstore.Hash(hex.EncodeToString(sum))
}

// Delete implements [objstore.Backend].
func (b *Backend) Delete(ctx context.Context, name string) error {
	err := b.store.Delete(ctx, name)
	if err == nil || errors.Is(err, jetstream.ErrObjectNotFound) {
		return nil
	}
	return fmt.Errorf("natsobj: delete %s: %w", name, err)
}

// metaSubjects is every subject the bucket keeps an object's metadata on:
// `$O.<bucket>.M.<name, base64url>`, one message each, the object store's own
// documented layout.
const metaSubjects = "$O." + Bucket + ".M.>"

// listResets is how many times a listing re-creates its consumer after losing
// it — the broker's heartbeats missed, a consumer reaped while a slow visitor
// held the listing — before it gives up and fails.
//
// FIVE: the library makes the first at once and waits one, two, four and eight
// seconds before the rest, each try bounded at ten, so a broker that has gone
// away fails a listing in about a minute — the pass is tried again later —
// and one back from a leader election is caught by the first or second.
const listResets = 5

// List implements [objstore.Backend].
//
// A CONSUMER OF ITS OWN OVER THE METADATA SUBJECTS, the last message on each,
// rather than the library's Watch — which knows the listing is complete only
// from a message it could DECODE, so a bucket whose newest metadata it cannot
// read, or an empty one whose emptiness check failed, never says it is done
// and holds its caller for ever. Here every message carries how many follow
// it, whatever its body says, and a listing ends when one says none. Streamed
// rather than gathered, too: the library's own List holds every object's info
// in one slice, which is the company's whole inventory in memory at once.
//
// Every name is visited as it is — what one means is not this backend's to
// judge — except one whose metadata cannot be read, which is no object
// anything could get and is logged rather than visited.
func (b *Backend) List(ctx context.Context, visit func(objstore.Info) error) error {
	cons, err := b.stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects:   []string{metaSubjects},
		DeliverPolicy:    jetstream.DeliverLastPerSubjectPolicy,
		MaxResetAttempts: listResets,
	})
	if err != nil {
		return fmt.Errorf("natsobj: list the bucket: %w", err)
	}
	defer b.drop(ctx, cons)
	if info := cons.CachedInfo(); info == nil || info.NumPending == 0 {
		return nil
	}
	msgs, err := cons.Messages()
	if err != nil {
		return fmt.Errorf("natsobj: list the bucket: %w", err)
	}
	defer msgs.Stop()
	for {
		msg, err := msgs.Next(jetstream.NextContext(ctx))
		if err != nil {
			return fmt.Errorf("natsobj: list the bucket: %w", err)
		}
		meta, err := msg.Metadata()
		if err != nil {
			return fmt.Errorf("natsobj: list the bucket: %w", err)
		}
		var info jetstream.ObjectInfo
		switch err := json.Unmarshal(msg.Data(), &info); {
		case err != nil:
			log.WarnContext(ctx, "object_metadata_unreadable", "subject", msg.Subject(),
				"error", err, "detail", "an object no get could read; it is left where it is")
		case !info.Deleted:
			info.ModTime = meta.Timestamp
			if err := visit(infoOf(&info)); err != nil {
				return err
			}
		}
		if meta.NumPending == 0 {
			return nil
		}
	}
}

// drop deletes a listing's consumer once the listing is over, rather than
// leaving it for the broker to reap minutes later — under a context of its
// own, since a listing ends as often because its caller's context did.
func (b *Backend) drop(ctx context.Context, cons jetstream.Consumer) {
	info := cons.CachedInfo()
	if info == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dropBudget)
	defer cancel()
	_ = b.stream.DeleteConsumer(ctx, info.Name)
}

// dropBudget bounds deleting a listing's consumer: one request to the
// broker's API, which the broker reaps on its own after five idle minutes if
// this is not answered.
const dropBudget = 5 * time.Second
