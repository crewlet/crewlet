// Package natsobj keeps the object store's objects in the fleet's own NATS
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
// holding a copy holds every object. For a fleet of three or five data nodes
// at three copies that is what any placement would have done anyway; a
// company whose files outgrow one member's disk takes the S3 backend
// (internal/objstore/s3obj) instead.
//
// # The bucket's own format, and the library only for the put
//
// The bucket is a NATS object store in that store's own documented layout —
// an object is a run of messages on `$O.<bucket>.C.<nuid>`, closed by one
// message on `$O.<bucket>.M.<name, base64url>` naming them — so every object
// stays readable by NATS's own tools (`nats object get`). The library writes
// it: a put is the library's streaming put, made safe by two rules, each the
// answer to a measured failure:
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
//
// EVERYTHING ELSE IS READ AND REMOVED THROUGH THE STREAM ITSELF, because the
// library's other verbs cannot be made safe:
//
//   - AN OBJECT'S METADATA IS READ FROM THE STREAM'S LEADER (jsapi.Leader).
//     The library's info read is a direct get, which any replica answers —
//     one behind the quorum included — and a replica that has not applied
//     an upload yet answers it "not found": a file uploaded on one node
//     would read as missing on the next, an audit would count a healthy
//     object lost, and a delete would be told the object was already gone
//     and leave it there.
//   - A GET IS AN ORDERED CONSUMER OF THE OBJECT'S OWN MESSAGES, read one
//     message at a time under [objstore.ReadStall] and ended by its
//     context. The library's own reader cannot be stopped once a read
//     blocks — the read holds the lock its close needs — so a download
//     whose pieces stopped arriving held its caller for the whole of its
//     budget; and given a context with no deadline it cuts the WHOLE
//     download off at five seconds. A ranged get walks the object's
//     message HEADERS to the message holding its offset — a few dozen bytes
//     a message rather than the bytes before it — and reads from there.
//   - A DELETE IS A PURGE AND NOTHING ELSE: of the object's metadata, then
//     of its pieces. The library's delete writes a delete MARKER in the
//     metadata's place, which no later put of the name ever replaces — the
//     store never reuses one ([objstore.Key]) — so every object ever deleted
//     would leave a message the hourly listing ships for ever. Metadata
//     first, so an object never names pieces that are gone; a crash
//     between the two leaves pieces nothing names, which are the store's to
//     sweep, never an object half there.
package natsobj

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
)

var log = logging.Get("objstore")

// Bucket is the object store bucket the objects live in, and Stream the
// JetStream stream that backs it — what a backup's stream snapshots carry the
// objects in.
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
	// store is the library's handle on the bucket, which only a put goes
	// through — see the package doc.
	store jetstream.ObjectStore
	// stream is the stream behind the bucket, which a get and a listing
	// consume and a delete purges.
	stream jetstream.Stream
	// leader reads one object's metadata from the stream's leader.
	leader *jsapi.Leader
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
				Bucket: Bucket,
				// THE DESCRIPTION A BUILD THAT KEPT CHUNKS GAVE IT,
				// unchanged: every boot of either build writes its own
				// text through this create-or-update, so two texts would
				// flip the stream's replicated config back and forth for
				// the whole of a rolling upgrade. It is read by nothing.
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
	leader, err := jsapi.NewLeader(js, Stream)
	if err != nil {
		return nil, fmt.Errorf("natsobj: address the %s stream's leader: %w", Stream, err)
	}
	return &Backend{store: store, stream: stream, leader: leader}, nil
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

// metaSubject is the subject the bucket keeps name's metadata on, and
// pieceSubject the subject an object's pieces are on — the object store's own
// documented layout, the name in base64url exactly as the library encodes it.
func metaSubject(name string) string {
	return "$O." + Bucket + ".M." + base64.URLEncoding.EncodeToString([]byte(name))
}

func pieceSubject(nuid string) string { return "$O." + Bucket + ".C." + nuid }

// meta reads name's metadata from the stream's leader: the object, a delete
// marker an earlier build's delete left (Deleted set), or
// [objstore.ErrNotFound] when the leader holds no message for the name. Its
// ModTime is the instant the leader stored the message — the object became
// whole then — rather than whatever the library wrote into it.
func (b *Backend) meta(ctx context.Context, name string) (jetstream.ObjectInfo, error) {
	msg, err := b.leader.Last(ctx, metaSubject(name))
	switch {
	case errors.Is(err, jsapi.ErrNoMessage):
		return jetstream.ObjectInfo{}, objstore.ErrNotFound
	case err != nil:
		return jetstream.ObjectInfo{}, err
	}
	var info jetstream.ObjectInfo
	if err := json.Unmarshal(msg.Data, &info); err != nil {
		return jetstream.ObjectInfo{}, fmt.Errorf("%w: %s's metadata is unreadable: %w",
			errUnreadable, name, err)
	}
	info.ModTime = msg.Time.UTC()
	return info, nil
}

// errUnreadable is metadata that does not decode — no object anything could
// read, and one a delete can still clear.
var errUnreadable = errors.New("natsobj: unreadable metadata")

// live is name's metadata when it names an object, [objstore.ErrNotFound]
// when the name holds nothing or only a delete marker.
func (b *Backend) live(ctx context.Context, name string) (jetstream.ObjectInfo, error) {
	info, err := b.meta(ctx, name)
	if err == nil && info.Deleted {
		return jetstream.ObjectInfo{}, objstore.ErrNotFound
	}
	return info, err
}

// readAhead is how many of an object's messages a get holds in hand before
// they are read: a mebibyte at [MessageBytes].
//
// EIGHT: enough that the next message is usually there when a reader asks for
// it, so a download streams at the wire's pace rather than a round trip per
// message — and what a node serving a handful of downloads at once holds in
// memory for each, where the library's own default of five hundred would be
// sixty-four mebibytes a download.
const readAhead = 8

// Get implements [objstore.Backend].
//
// AN ORDERED CONSUMER OF THE OBJECT'S OWN MESSAGES, never the library's get —
// see the package doc. Each message is waited for under
// [objstore.ReadStall] and under ctx, so a read whose pieces have stopped
// arriving ends rather than waits out its caller's whole budget, and a reader
// that is closed or whose caller gave up stops at once.
//
// A RANGE starts at the message holding its offset, found by walking the
// object's message HEADERS ([Backend.seek]): the library has no ranged read,
// and the object store's pieces cannot be addressed by their place in the
// object, so the cost of an offset is a header per message before it rather
// than every byte.
func (b *Backend) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	if err := objstore.CheckRead(ctx, off, n); err != nil {
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	}
	info, err := b.live(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	}
	size := int64(info.Size)
	if off >= size {
		return io.NopCloser(eof{}), nil
	}
	subject := pieceSubject(info.NUID)
	cfg := jetstream.OrderedConsumerConfig{
		FilterSubjects:   []string{subject},
		DeliverPolicy:    jetstream.DeliverAllPolicy,
		MaxResetAttempts: listResets,
	}
	var skip int64
	if off > 0 {
		start, before, found, serr := b.seek(ctx, subject, off)
		if serr != nil {
			return nil, fmt.Errorf("natsobj: get %s: find offset %d: %w", name, off, serr)
		}
		if !found {
			return b.gone(ctx, name)
		}
		cfg.DeliverPolicy, cfg.OptStartSeq, skip = jetstream.DeliverByStartSequencePolicy, start, before
	}
	cons, err := b.stream.OrderedConsumer(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	}
	if info := cons.CachedInfo(); info == nil || info.NumPending == 0 {
		b.drop(ctx, cons)
		return b.gone(ctx, name)
	}
	msgs, err := cons.Messages(jetstream.PullMaxMessages(readAhead))
	if err != nil {
		b.drop(ctx, cons)
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	}
	return &object{ctx: ctx, b: b, name: name, cons: cons, msgs: msgs, skip: skip, left: n}, nil
}

// gone answers a get whose object's pieces are not there: an object that ends
// before it begins — which the store reads as one holding fewer bytes than its
// row says — unless the object itself went between its metadata's read and
// the pieces', which a second look tells apart.
func (b *Backend) gone(ctx context.Context, name string) (io.ReadCloser, error) {
	if _, err := b.live(ctx, name); err != nil {
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	}
	return io.NopCloser(eof{}), nil
}

// seek finds the message on subject holding byte off of its object: its
// stream sequence, how many of its bytes come before off, and false when no
// message holds it.
//
// FROM THE HEADERS ALONE: a message delivered without its payload carries the
// payload's size in a header (`Nats-Msg-Size`), so the walk costs a few dozen
// bytes a message — never the bytes before off — and assumes nothing about
// how the pieces were cut.
func (b *Backend) seek(ctx context.Context, subject string, off int64) (uint64, int64, bool, error) {
	cons, err := b.stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects:   []string{subject},
		HeadersOnly:      true,
		MaxResetAttempts: listResets,
	})
	if err != nil {
		return 0, 0, false, err
	}
	defer b.drop(ctx, cons)
	if info := cons.CachedInfo(); info == nil || info.NumPending == 0 {
		return 0, 0, false, nil
	}
	msgs, err := cons.Messages()
	if err != nil {
		return 0, 0, false, err
	}
	defer msgs.Stop()
	var at int64
	for {
		msg, err := next(ctx, msgs)
		if err != nil {
			return 0, 0, false, err
		}
		meta, err := msg.Metadata()
		if err != nil {
			return 0, 0, false, err
		}
		size, err := strconv.ParseInt(msg.Headers().Get(nats.MsgSize), 10, 64)
		if err != nil {
			return 0, 0, false, fmt.Errorf("message %d carries no size: %w", meta.Sequence.Stream, err)
		}
		if at+size > off {
			return meta.Sequence.Stream, off - at, true, nil
		}
		at += size
		if meta.NumPending == 0 {
			return 0, 0, false, nil
		}
	}
}

// next is the next message of a consumer, waited for no longer than
// [objstore.ReadStall] and no longer than ctx.
func next(ctx context.Context, msgs jetstream.MessagesContext) (jetstream.Msg, error) {
	wait, cancel := context.WithTimeout(ctx, objstore.ReadStall)
	defer cancel()
	msg, err := msgs.Next(jetstream.NextContext(wait))
	switch {
	case err == nil:
		return msg, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case wait.Err() != nil:
		return nil, fmt.Errorf("no message arrived for %v: %w", objstore.ReadStall, wait.Err())
	}
	return nil, err
}

// eof is an empty stream.
type eof struct{}

func (eof) Read([]byte) (int, error) { return 0, io.EOF }

// object is one get's stream.
type object struct {
	ctx  context.Context
	b    *Backend
	name string
	cons jetstream.Consumer
	msgs jetstream.MessagesContext

	// skip is how many bytes of the first message come before the range,
	// and left how many the range has still to hand over — negative for
	// the rest of the object.
	skip int64
	left int64

	// held is the rest of the current message, and last whether it is the
	// object's final one.
	held []byte
	last bool

	closeOnce sync.Once
}

func (o *object) Read(p []byte) (int, error) {
	// THE CALLER'S CONTEXT FIRST, before a message already in hand is
	// handed over: a reader whose caller gave up stops here, whatever the
	// consumer had read ahead.
	if err := o.ctx.Err(); err != nil {
		return 0, err
	}
	if o.left == 0 {
		return 0, io.EOF
	}
	for len(o.held) == 0 {
		if o.last {
			return 0, io.EOF
		}
		msg, err := next(o.ctx, o.msgs)
		if err != nil {
			return 0, fmt.Errorf("natsobj: read %s: %w", o.name, err)
		}
		meta, err := msg.Metadata()
		if err != nil {
			return 0, fmt.Errorf("natsobj: read %s: %w", o.name, err)
		}
		data := msg.Data()
		data = data[min(o.skip, int64(len(data))):]
		o.skip = 0
		o.held, o.last = data, meta.NumPending == 0
	}
	if o.left >= 0 && int64(len(p)) > o.left {
		p = p[:o.left]
	}
	n := copy(p, o.held)
	o.held = o.held[n:]
	if o.left > 0 {
		o.left -= int64(n)
	}
	return n, nil
}

// Close stops the stream and deletes its consumer, rather than leaving it for
// the broker to reap minutes later.
func (o *object) Close() error {
	o.closeOnce.Do(func() {
		o.msgs.Stop()
		o.b.drop(o.ctx, o.cons)
	})
	return nil
}

// Stat implements [objstore.Backend], from the stream's leader.
func (b *Backend) Stat(ctx context.Context, name string) (objstore.Info, error) {
	info, err := b.live(ctx, name)
	if err != nil {
		return objstore.Info{}, fmt.Errorf("natsobj: stat %s: %w", name, err)
	}
	return infoOf(&info), nil
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

// Delete implements [objstore.Backend]: a purge of the name's metadata, then
// of the object's pieces, and never a marker — see the package doc.
//
// A delete marker an earlier build's delete left is purged the same way, so a
// name the listing visits for it ([Backend.List]) is cleared by the ordinary
// delete the collector makes of it; and so is metadata that cannot be read,
// whose pieces — if it names any — nothing can find any more.
func (b *Backend) Delete(ctx context.Context, name string) error {
	info, err := b.meta(ctx, name)
	switch {
	case errors.Is(err, objstore.ErrNotFound):
		return nil
	case err != nil && !errors.Is(err, errUnreadable):
		return fmt.Errorf("natsobj: delete %s: %w", name, err)
	}
	if err := b.stream.Purge(ctx, jetstream.WithPurgeSubject(metaSubject(name))); err != nil {
		return fmt.Errorf("natsobj: delete %s: purge its metadata: %w", name, err)
	}
	if info.NUID == "" {
		return nil
	}
	if err := b.stream.Purge(ctx, jetstream.WithPurgeSubject(pieceSubject(info.NUID))); err != nil {
		return fmt.Errorf("natsobj: delete %s: purge its pieces: %w", name, err)
	}
	return nil
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
// anything could get and is logged rather than visited. A name holding only
// the delete marker an earlier build's delete left is visited too, as an
// object of no bytes written when the marker was: the marker is a message the
// stream keeps for ever until something purges it, and the collector clears
// it with the same delete it makes of any name ([Backend.Delete]).
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
		default:
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

// drop deletes a consumer once its listing or its get is over, rather than
// leaving it for the broker to reap minutes later — under a context of its
// own, since either ends as often because its caller's context did.
func (b *Backend) drop(ctx context.Context, cons jetstream.Consumer) {
	info := cons.CachedInfo()
	if info == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dropBudget)
	defer cancel()
	_ = b.stream.DeleteConsumer(ctx, info.Name)
}

// dropBudget bounds deleting a consumer: one request to the
// broker's API, which the broker reaps on its own after five idle minutes if
// this is not answered.
const dropBudget = 5 * time.Second
