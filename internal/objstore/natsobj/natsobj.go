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
//     Every read, a get, a walk or a listing, LOOKS AGAIN after a quiet
//     second ([quietWait]) rather than wait out its heartbeats, which must
//     be long enough for a link at the floor ([pullHeartbeat]): at its first
//     message it asks its consumer, and makes it again if it never had the
//     pull — which a cluster drops, routinely, at a consumer's creation —
//     and at every later one it waits again, so the client sends a pull it
//     skipped.
//   - A DELETE IS A PURGE AND NOTHING ELSE: of the object's metadata, then
//     of its pieces. The library's delete writes a delete MARKER in the
//     metadata's place, which no later put of the name ever replaces — the
//     store never reuses one ([objstore.Key]) — so every object ever deleted
//     would leave a message the hourly listing ships for ever. Metadata
//     first, so an object never names pieces that are gone; a crash
//     between the two leaves pieces nothing names — never an object half
//     there — which are an unfinished upload like any other.
//   - AN UNFINISHED UPLOAD IS FOUND THROUGH THE STREAM TOO ([Backend.Pending]):
//     pieces no metadata names, which a put killed before its last message
//     and a delete interrupted between its purges both leave, and which no
//     listing of the names shows. The library has no word for them at all.
//
// # What it costs, measured
//
// `make bench` (BenchmarkBackend, through internal/objstore/objstoretest's
// Bench) at `stream.sync: always`, production's default — on four vCPUs of a
// KVM Intel Xeon at 2.8 GHz without SHA instructions, Go 1.27.2, nats-server
// v2.15.0, nats.go v1.54.0, 2026-10-10, two runs on a VM shared with other
// work. Every read figure is of an object written moments before, read warm,
// and was taken with a hand ([readAhead]) of eight messages; four, alternated
// with eight on one machine twice over, read within that machine's noise in
// every arm below:
//
//   - ON A MEMBER'S OWN CLIENT, as a data node of a solo fleet: a 64 KiB put
//     took 1.9–2.8 ms — every message is fsynced — and larger ones ran at
//     74–117 MB/s; a whole read ran at 450–740 MB/s from 1 MiB up, and
//     200–250 MB/s verified ([objstore.Store.Open]), which is this CPU's
//     SHA-256 rate; eight verified reads at once at 560–590 MB/s together. A
//     Stat, which is a leader read, ran 10,700–10,800 a second from one
//     goroutine over one object, 4,800–8,600 across ten thousand, and
//     15,000–19,000 across them at 32 in flight; a listing named
//     75,000–94,000 objects a second, at a hundred thousand as at ten
//     thousand.
//   - A PAGE IS PAID FOR BY ITS OFFSET: 64 KiB at a random offset of a
//     128 MiB object took 32–36 ms, the walk of the headers of the 512
//     messages before it on average, where the S3 backend's one ranged request
//     took half a millisecond on the node's side. A page deep in a large file
//     costs that walk again every time it is read.
//   - THROUGH A LEAF, as a node without `data`: a 64 KiB put took 2.6–2.7 ms
//     and larger ones ran at 82–105 MB/s; a whole read at 250–360 MB/s, about
//     half a member's, and 144–226 MB/s verified; eight verified reads at
//     once at 370–470 MB/s; a page 38–43 ms. A Stat from one goroutine pays
//     the link's round trip — 2,900–4,200 a second — and 32 in flight recover
//     it, at 15,000–25,000; a listing named 63,000–92,000 objects a second.
//   - AT THREE REPLICAS (internal/objstore/natsobj/replicated: three members
//     on that one machine, each fsyncing its own copy to the one disk, driven
//     from a follower's client, as two of a three-member fleet's data nodes
//     reach the bucket): every 128 KiB message is a quorum append, and a
//     64 KiB put took 4.2–4.9 ms and larger ones ran at 39–80 MB/s, a third
//     to three quarters of one member's. A whole read from 16 MiB up ran at
//     218–476 MB/s, and 156–217 verified; eight verified reads at once at
//     144–180 MB/s together. A small read pays for making its consumer, a
//     decision of the cluster's, and for the first pulls a cluster drops at
//     that moment ([quietWait]): of 3,000 64 KiB gets through a follower's
//     client, from 2 to 124 lost theirs from one run to the next, with the
//     machine's load — each costing a second, where a one-second heartbeat
//     made it two — and gets averaged 7.2–7.3 ms in runs that lost 8, where
//     one that kept its pull took about 4. A Stat is a leader read
//     across a route — 2,100–2,800 a second from one goroutine, 5,600–23,400
//     at 32 in flight — and a listing named 80,000–92,000 objects a second.
package natsobj

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
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

// Open binds to the bucket, creating it first if it is not there.
//
// # A LOOKUP, THEN A CREATE, AND NEVER AN UPDATE
//
// Every data node opens the bucket at boot, and a fleet booting together does
// it at the same moment. The library's create-or-update sends an UPDATE first,
// and an update of a stream another node has just created — its group not yet
// through its first election — is applied by members none of which leads that
// group yet, while only a leader answers an update
// (server/jetstream_cluster.go, processClusterUpdateStream): its reply is never
// sent. Measured on a three-member fleet booting together: all three nodes'
// requests went unanswered for the whole fifteen-second ask term, on each boot
// instrumented, and nothing said so. It was also the node that booted last deciding
// the bucket's configuration for everybody.
//
// So Open asks whether the bucket exists — a READ, asked again at the read
// term while nobody answers ([jsprovision.Timing.Read]) — and creates it only
// when told it is absent or when nobody answered, through
// [jsprovision.Timing.Place] as every replicated create at boot is. A create
// that is not refused outright may have landed, or found the name taken by a
// peer that won the race, so the bucket is then read back
// ([jsprovision.Timing.Settle]) rather than written again. Nothing here applies
// a configuration to a bucket that exists.
//
// A BUCKET REPLICATED BELOW THIS NODE IS REFUSED, as the queue refuses such a
// stream: an upload acknowledged there would prove fewer copies than
// `stream.replicas` promises. Above it is fine, so a node configured for one
// copy starts against a fleet's three.
func Open(ctx context.Context, js jetstream.JetStream, cfg Config) (*Backend, error) {
	return open(ctx, js, cfg, jsprovision.Clustered(cfg.Clustered).Timing())
}

// open is [Open] at timing, which only this package's own tests set to
// anything but the production one.
func open(ctx context.Context, js jetstream.JetStream, cfg Config, timing jsprovision.Timing) (*Backend, error) {
	if js == nil {
		return nil, errors.New("natsobj: the nats object store needs the broker's " +
			"JetStream, and this node has none")
	}
	replicas := max(cfg.Replicas, 1)
	// ONE BREADCRUMB OVER THE WHOLE OF IT, because either half can be the
	// one that stalls and the operator's question is the same: which object
	// is this node still waiting on.
	stop := jsprovision.WhenSlow(ctx, func(after time.Duration) {
		log.WarnContext(ctx, "natsobj_bucket_slow", "bucket", Bucket,
			"replicas", replicas, "waited", after,
			"detail", "the object store's bucket is still being looked up, "+
				"and created if it was absent; on a fleet that is a metadata "+
				"group that has not settled")
	})
	defer stop()

	store, err := lookupBucket(ctx, js, timing)
	switch {
	case err == nil:
	case errors.Is(err, jetstream.ErrBucketNotFound), jsprovision.Unanswered(ctx, err):
		// TOLD it is absent, or told nothing — and the create decides
		// both: absent and it is made, present and it is read back.
		if !errors.Is(err, jetstream.ErrBucketNotFound) {
			// NAMED, as every other provisioning lookup nobody answered
			// is: the boot carries on, and this is the one line that
			// says the bucket's existence was decided by the create
			// because the broker never said.
			log.WarnContext(ctx, "natsobj_bucket_lookup_unanswered",
				"bucket", Bucket, "error", err.Error(),
				"detail", "the broker did not say whether this bucket exists, "+
					"so the create below decides it: absent and it is made, "+
					"present and it comes back as a peer's win and is read back")
		}
		if store, err = createBucket(ctx, js, timing, replicas); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("natsobj: look up the %s bucket: %w", Bucket, err)
	}

	// THE STREAM BEHIND IT, which a get and a listing consume — read back
	// rather than read once, because a bucket a peer has just made can still
	// be not there on the member that answers.
	var stream jetstream.Stream
	if err = timing.Settle(ctx, func(ctx context.Context) error {
		var e error
		stream, e = js.Stream(ctx, Stream)
		return e
	}); err != nil {
		return nil, fmt.Errorf("natsobj: bind the %s stream: %w", Stream, err)
	}
	if got := stream.CachedInfo().Config.Replicas; got < replicas {
		return nil, fmt.Errorf("natsobj: the %s bucket is replicated %dx and this "+
			"node is configured for %dx (stream.replicas): an upload acknowledged "+
			"there would prove fewer copies than this node promises — raise the "+
			"bucket's replicas or align stream.replicas on every node",
			Bucket, got, replicas)
	}
	leader, err := jsapi.NewLeader(js, Stream)
	if err != nil {
		return nil, fmt.Errorf("natsobj: address the %s stream's leader: %w", Stream, err)
	}
	return &Backend{store: store, stream: stream, leader: leader}, nil
}

// lookupBucket asks whether the bucket exists, under the lookup ceiling and at
// the read term.
func lookupBucket(ctx context.Context, js jetstream.JetStream, timing jsprovision.Timing) (jetstream.ObjectStore, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, timing.Lookup)
	defer cancel()
	var store jetstream.ObjectStore
	err := timing.Read(lookupCtx, func(ctx context.Context) error {
		var e error
		store, e = js.ObjectStore(ctx, Bucket)
		return e
	}, nil)
	return store, err
}

// createBucket creates the bucket at replicas copies, or reads back the one a
// peer made.
//
// ctx IS OPEN'S, and the create's own deadline is derived here, because the
// read-back runs precisely when that deadline may have expired — see
// [jsprovision.Settle].
func createBucket(ctx context.Context, js jetstream.JetStream, timing jsprovision.Timing,
	replicas int) (jetstream.ObjectStore, error) {

	createCtx, cancel := context.WithTimeout(ctx, timing.Budget)
	defer cancel()
	var store jetstream.ObjectStore
	err := timing.Place(createCtx, func(ctx context.Context) error {
		var e error
		// CREATE, NOT CreateOrUpdate — see [Open].
		//
		// NO BYTE CEILING (MaxBytes): the engine reserves one for the
		// state logs alone — the company's own records, whose growth the
		// trim governs — and every other stream it runs, the mailboxes
		// and every coordination bucket, declares none and is bounded by
		// what writes it. This one is bounded by the collector, which
		// deletes what no row names a day after it was written. A ceiling
		// here would be a reservation carved out of the budget the logs'
		// ceilings draw on, for bytes no log is waiting for.
		store, e = js.CreateObjectStore(ctx, jetstream.ObjectStoreConfig{
			Bucket: Bucket,
			// WRITTEN ONCE, by whichever node creates the bucket: nothing
			// here updates a bucket that exists, so a later build's text
			// reaches only a fleet's first boot. It is read by nothing.
			Description: "Crewlet files, one object per upload",
			Storage:     jetstream.FileStorage,
			Replicas:    replicas,
		})
		return e
	}, nil)
	if err == nil {
		return store, nil
	}
	if jsprovision.NoApplicableLimit(err) {
		// NO LIMIT APPLIES TO THIS BUCKET AT ALL, which is not the bucket
		// failing to fit: the account's limits are tiered and carry none
		// for the class `stream.replicas` puts this node in, so the broker
		// refused before it compared a byte — and a bucket reserves no
		// ceiling, so there is nothing here to make smaller either. Asked
		// BEFORE the general refusal below, which it is one of, because it
		// is the one worded differently: bare, it is the broker's own text
		// and names neither the class nor the field that picks it.
		return nil, fmt.Errorf("natsobj: create the %s bucket at %d copies: %w%s",
			Bucket, replicas, err, jsprovision.NoApplicableLimitDetail(replicas))
	}
	if jsprovision.Refused(err) {
		// REFUSED OUTRIGHT, so nothing was made and there is nothing to
		// read back — the refusal is the answer.
		return nil, fmt.Errorf("natsobj: create the %s bucket at %d copies: %w", Bucket, replicas, err)
	}
	// THE NAME WAS TAKEN — a peer won the race — or nobody said whether the
	// create landed: read back what is there, ON ctx, NOT createCtx.
	readErr := timing.Settle(ctx, func(ctx context.Context) error {
		var e error
		store, e = js.ObjectStore(ctx, Bucket)
		return e
	})
	if readErr != nil {
		// BOTH WRAPPED, as at every other create site: the create's error
		// says what went wrong and the read-back's whether the bucket is
		// really absent, and a caller asks either with [errors.Is].
		return nil, fmt.Errorf("natsobj: create the %s bucket at %d copies: %w "+
			"(and reading it back: %w)", Bucket, replicas, err, readErr)
	}
	return store, nil
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

// nameOfMeta is the name a metadata subject spells, and whether it spells one
// — [metaSubject] read back.
func nameOfMeta(subject string) (string, bool) {
	token, ok := strings.CutPrefix(subject, metaSubject(""))
	if !ok || token == "" {
		return "", false
	}
	name, err := base64.URLEncoding.DecodeString(token)
	if err != nil || len(name) == 0 {
		return "", false
	}
	return string(name), true
}

// meta reads name's metadata from the stream's leader: the object, a delete
// marker the library's own delete left — `nats object rm`, or any other client
// of the bucket's documented format — (Deleted set), or
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
		// [objstore.ErrCorrupt], A DEFINITE ANSWER: the leader holds this
		// name's metadata and it names no object anything could read — and
		// reading it again reads the same bytes. A delete still clears it.
		return jetstream.ObjectInfo{}, fmt.Errorf("%w: %s's metadata does not decode: %w",
			objstore.ErrCorrupt, name, err)
	}
	info.ModTime = msg.Time.UTC()
	return info, nil
}

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
// they are read: half a mebibyte at [MessageBytes].
//
// ENOUGH that the next message is usually there when a reader asks for it, so
// a download streams at the wire's pace rather than a round trip per message:
// the client asks for more once half the hand is read, so two to four messages
// are always asked for ahead of the reader. NO MORE, because a hand is what a read on a slow
// link waits on WHOLE ([pullHeartbeat]) — and what a node serving a handful of
// downloads at once holds in memory for each, where the library's own default
// of five hundred would be sixty-four mebibytes a download.
//
// FOUR, not the eight it was: a hand of a mebibyte takes thirty seconds at the
// floor, and no heartbeat could both outlast that and leave a replaced
// consumer's first hand time to arrive inside [objstore.ReadStall]'s minute.
const readAhead = 4

// walkAhead is how many messages a WALK holds in hand — a listing over the
// bucket's metadata, or a ranged get's walk over an object's message headers.
//
// FIVE HUNDRED, the client's own default, stated rather than inherited: what a
// walk holds is a name's metadata or a message's headers, a few hundred bytes
// each, so a full hand is a hundred-odd kibibytes however large the objects —
// where [readAhead] has to be small because each of ITS messages is a whole
// [MessageBytes]. Stated because it is also where a walk's consumer can be
// lost unnoticed: everything already in hand is read without asking the broker
// again, so a consumer reaped while a slow visitor held the listing is found
// only at the next pull, and the replacement it takes is what
// [TestAListingWhoseConsumerIsLostFinishesOnItsReplacement] stages past this
// many names.
const walkAhead = 500

// pullHeartbeat is how often the broker tells a get or a walk that is waiting
// on a pull that the pull is still being served. Two going missing while a
// reader waits and the client replaces the consumer, from the last message
// read — the one way a read notices that the broker took its consumer away
// under it: reaped while a slow reader held it, or dropped in a leader change.
//
// # The longest a healthy read hears nothing
//
// The client hears of a heartbeat, or of a message, only once the whole of it
// has arrived, and the broker sends a heartbeat only while a pull waits with
// nothing to deliver — never while what it delivered is still crossing the
// link. And a compressed link, which a leaf's is by default, carries what the
// broker flushed at once as ONE BLOCK that the far end decodes only whole: a
// get's first pull puts its whole hand ([readAhead]) on the wire in one flush,
// so a read hears nothing until the last byte of it has arrived. At the floor
// every leg of the store is held to ([objstore.MiBPace]) half a mebibyte takes
// fifteen seconds — on a link many reads share too, since each one's hand
// waits behind the others' and each has its share. A read replaced in that
// silence is replaced again before its replacement's hand arrives, behind what
// its predecessor was sent, and never completes another message. Measured
// across a leaf link a quarter above the floor with the hand at eight, a
// heartbeat of one second and one of two each ended their read at its
// forty-second deadline having handed over nothing — two being the shortest
// that outlasts one MESSAGE at the floor, which is not what arrives.
//
// # A hand at the floor, so two are twice it
//
// DERIVED rather than stated, so it moves with the hand and the floor: two
// heartbeats are thirty seconds, twice the longest a healthy read hears
// nothing at the floor — the room [objstore.ReadStall] gives a mebibyte — and
// a consumer lost under a read costs its quiet second ([quietWait]), those
// thirty seconds and then its replacement's first hand, forty-six in all,
// inside the stall's minute (TestAReadsHeartbeatOutlastsItsHandAtTheFloor
// holds both).
//
// WHAT IT NO LONGER COSTS is a read that hears nothing for a second for any
// other reason — a first pull a cluster dropped, a pull the client skipped:
// that read looks again ([quietWait]) rather than wait out the heartbeats.
const pullHeartbeat = time.Duration(readAhead*MessageBytes) * objstore.MiBPace / objstore.MiB

// pullExpiry is how long one pull a get or a walk sends waits for messages
// before the broker answers it as expired and the client asks again.
//
// TWICE [pullHeartbeat], the shortest the client accepts beside it: it refuses
// a heartbeat over half the expiry. It bounds nothing a reader waits for — a
// pull the stream can answer is answered at once, and a pull nobody serves is
// noticed by its missing heartbeats — so what it sets is how often a reader
// waiting on a slow broker asks again: twice a minute.
const pullExpiry = 2 * pullHeartbeat

// quietWait is how long a read waits for a message before it looks again: at
// its first message by asking its consumer whether the pull for it ever
// arrived, and at every later one by waiting again, which is when the client
// decides whether to send a pull.
//
// # A CLUSTER DROPS A NEW CONSUMER'S FIRST PULL, routinely
//
// The member that makes a consumer answers the request for it as soon as the
// consumer exists, and that answer can reach the reader's own member ahead of
// the news that the consumer's member serves its pulls; a pull the reader
// sends in that gap is dropped without a word. Measured on a three-member
// fleet (internal/objstore/natsobj/replicated), from one get in twenty-five to
// one in fifteen hundred lost its first pull that way as the machine's load
// rose and fell, through a follower's client and through the stream leader's
// alike, on nats-server v2.15.1 as on v2.15.0. A solo member never showed it.
//
// The heartbeats cannot tell that read from one whose first hand is still
// crossing a slow link — neither hears anything — so a read that waited on
// them for its first message waited as long as the slowest link must be
// allowed: ten seconds at the client's own default, thirty at a heartbeat
// that honours the floor. Its CONSUMER can tell them apart. One that holds no
// pull and has delivered nothing never got the pull, and is made again; one
// that holds the pull or has delivered is being read, however slowly, and is
// waited on. Asked, never guessed, because a read remade while its hand is on
// the wire waits behind the hand it threw away.
//
// # THE CLIENT SKIPS A PULL, now and then
//
// The client decides whether to ask for more only as a wait begins, and asks
// for nothing while the pull it sent last is still being sent — which the
// goroutine sending it marks done only after the broker may already have
// answered it. A reader on a loaded machine can take the whole answer and
// begin its next wait inside that moment, and then the next pull is never
// sent: the consumer, having delivered its hand, holds no pull and has nothing
// to say, and the reader waited for two heartbeats. Measured on one member,
// one 1 MiB get in ten to twenty thousand stalled that way, its consumer
// showing its first hand delivered, no pull waiting and the rest pending.
// nats.go v1.54.0 decides so, and its main branch still does. A wait begun
// again is a decision made again, and the pull the client skipped goes out.
//
// ONE SECOND: about twenty times the worst p99 `make bench` measured the
// broker at three replicas on one shared machine (55 ms, for sixteen writes at
// once), so a broker that is merely busy is seldom asked, asking costs one
// small request, and waiting again costs nothing. A dropped first pull or a
// skipped one costs that second.
const quietWait = time.Second

// firstPullRemakes is how many times a read remakes a consumer that never got
// its first pull, before it waits for the pull like any other.
//
// THREE: a cluster dropped a first pull one read in twenty-five at the worst
// measured, and a pull made again was dropped as often as a first one, so
// after two remakes one read in fifteen thousand would still wait out its
// heartbeats, and after three one in four hundred thousand. A consumer still
// without its pull after three is behind a link slow enough to hold the pull
// itself back, where remaking it again only adds a consumer for the cluster
// to create while the pull is on its way — and its heartbeats are still over
// it.
const firstPullRemakes = 3

// pulls is what every get and walk asks its consumer for: up to batch
// messages in hand, under the pull expiry and heartbeat above.
func pulls(batch int) []jetstream.PullMessagesOpt {
	return []jetstream.PullMessagesOpt{jetstream.PullMaxMessages(batch),
		jetstream.PullExpiry(pullExpiry), jetstream.PullHeartbeat(pullHeartbeat)}
}

// errQuiet is a wait for a message that ended with nothing arrived, before
// the read's own stall or its caller's context ended.
var errQuiet = errors.New("natsobj: no message yet")

// reading is one ordered consumer being read — a get's, a seek's or a
// listing's — each message waited for no longer than stall, or for as long as
// the caller's context allows where stall is zero.
type reading struct {
	b     *Backend
	cons  jetstream.Consumer
	msgs  jetstream.MessagesContext
	stall time.Duration
}

// consume makes an ordered consumer over cfg and reads it, batch messages in
// hand, answering it with its first message — or no reading at all where the
// consumer has nothing to deliver.
//
// THE FIRST MESSAGE IS THE ONE A CLUSTER CAN DROP THE PULL FOR ([quietWait]):
// one that has not come in a second is asked about, and a consumer that never
// got the pull is dropped and made again, up to [firstPullRemakes] times —
// each with a whole stall of its own, since nothing the read made had begun.
func (b *Backend) consume(ctx context.Context, cfg jetstream.OrderedConsumerConfig, batch int,
	stall time.Duration) (*reading, jetstream.Msg, error) {

	for remade := 0; ; remade++ {
		cons, err := b.stream.OrderedConsumer(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		if info := cons.CachedInfo(); info == nil || info.NumPending == 0 {
			b.drop(ctx, cons)
			return nil, nil, nil
		}
		msgs, err := cons.Messages(pulls(batch)...)
		if err != nil {
			b.drop(ctx, cons)
			return nil, nil, err
		}
		r := &reading{b: b, cons: cons, msgs: msgs, stall: stall}
		wait, cancel := r.waiting(ctx)
		msg, err := r.quietly(ctx, wait)
		if errors.Is(err, errQuiet) {
			if remade < firstPullRemakes && r.unpulled(wait) {
				cancel()
				r.stop(ctx)
				continue
			}
			msg, err = r.from(ctx, wait, wait)
		}
		cancel()
		if err != nil {
			r.stop(ctx)
			return nil, nil, err
		}
		return r, msg, nil
	}
}

// next is r's next message: waited for [quietWait], then once more for the
// rest of r's stall, so the client decides again whether to send a pull.
func (r *reading) next(ctx context.Context) (jetstream.Msg, error) {
	wait, cancel := r.waiting(ctx)
	defer cancel()
	msg, err := r.quietly(ctx, wait)
	if errors.Is(err, errQuiet) {
		msg, err = r.from(ctx, wait, wait)
	}
	return msg, err
}

// quietly is r's next message, waited for no longer than [quietWait].
func (r *reading) quietly(ctx, wait context.Context) (jetstream.Msg, error) {
	soon, cancel := context.WithTimeout(wait, quietWait)
	defer cancel()
	return r.from(ctx, wait, soon)
}

// waiting is ctx bounded by r's stall, where it has one.
func (r *reading) waiting(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.stall == 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, r.stall)
}

// from is the next message r's consumer hands over before until ends — until
// being wait or shorter — and says which ended a wait that got none: ctx, the
// caller's own; wait, r's stall; or until alone, errQuiet.
func (r *reading) from(ctx, wait, until context.Context) (jetstream.Msg, error) {
	msg, err := r.msgs.Next(jetstream.NextContext(until))
	switch {
	case err == nil:
		return msg, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case wait.Err() != nil:
		return nil, fmt.Errorf("no message arrived for %v: %w", r.stall, wait.Err())
	case until.Err() != nil:
		return nil, errQuiet
	}
	return nil, err
}

// unpulled reports whether r's consumer says it never had a pull: none waiting
// on it and nothing delivered. A consumer the broker no longer has is as good
// as one that never had a pull; one that cannot be asked before wait ends is
// not known to lack one, and is waited on.
func (r *reading) unpulled(wait context.Context) bool {
	info, err := r.cons.Info(wait)
	switch {
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		return true
	case err != nil:
		return false
	}
	return info.NumWaiting == 0 && info.Delivered.Consumer == 0
}

// stop ends r and deletes its consumer, rather than leaving it for the broker
// to reap minutes later.
func (r *reading) stop(ctx context.Context) {
	r.msgs.Stop()
	r.b.drop(ctx, r.cons)
}

// Get implements [objstore.Backend].
//
// AN ORDERED CONSUMER OF THE OBJECT'S OWN MESSAGES, never the library's get —
// see the package doc. Each message is waited for under
// [objstore.ReadStall] and under ctx, so a read whose pieces have stopped
// arriving ends rather than waits out its caller's whole budget, and a reader
// that is closed or whose caller gave up stops at once. The FIRST is waited
// for here, before the reader is handed back, so a consumer a cluster dropped
// the first pull for is made again before anybody reads ([quietWait]).
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
	r, first, err := b.consume(ctx, cfg, readAhead, objstore.ReadStall)
	switch {
	case err != nil:
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	case r == nil:
		return b.gone(ctx, name)
	}
	o := &object{ctx: ctx, name: name, r: r, skip: skip, left: n}
	if err := o.hold(first); err != nil {
		r.stop(ctx)
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	}
	return o, nil
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
	r, msg, err := b.consume(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects:   []string{subject},
		HeadersOnly:      true,
		MaxResetAttempts: listResets,
	}, walkAhead, objstore.ReadStall)
	if err != nil || r == nil {
		return 0, 0, false, err
	}
	defer r.stop(ctx)
	var at int64
	for {
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
		if msg, err = r.next(ctx); err != nil {
			return 0, 0, false, err
		}
	}
}

// eof is an empty stream.
type eof struct{}

func (eof) Read([]byte) (int, error) { return 0, io.EOF }

// object is one get's stream.
type object struct {
	ctx  context.Context
	name string
	r    *reading

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

// hold makes msg the message o hands over next, past what of it comes
// before the range.
func (o *object) hold(msg jetstream.Msg) error {
	meta, err := msg.Metadata()
	if err != nil {
		return err
	}
	data := msg.Data()
	data = data[min(o.skip, int64(len(data))):]
	o.skip = 0
	o.held, o.last = data, meta.NumPending == 0
	return nil
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
		msg, err := o.r.next(o.ctx)
		if err != nil {
			return 0, fmt.Errorf("natsobj: read %s: %w", o.name, err)
		}
		if err := o.hold(msg); err != nil {
			return 0, fmt.Errorf("natsobj: read %s: %w", o.name, err)
		}
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
	o.closeOnce.Do(func() { o.r.stop(o.ctx) })
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
// A delete marker the library's own delete left — `nats object rm`, or any
// other client of the bucket's documented format — is purged the same way, so a
// name the listing visits for it ([Backend.List]) is cleared by the ordinary
// delete the collector makes of it; and so is metadata that cannot be read,
// whose pieces — if it names any — nothing can find any more.
func (b *Backend) Delete(ctx context.Context, name string) error {
	info, err := b.meta(ctx, name)
	switch {
	case errors.Is(err, objstore.ErrNotFound):
		return nil
	case err != nil && !errors.Is(err, objstore.ErrCorrupt):
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
// judge. A name holding only the delete marker the library's own delete left —
// `nats object rm`, or any other client of the bucket's documented format — is
// visited too, as an object of no bytes written when the marker was: the
// marker is a message the stream keeps for ever until something purges it,
// and the collector clears it with the same delete it makes of any name
// ([Backend.Delete]). So is a name whose metadata does not decode, which a
// stat and a get answer as [objstore.ErrCorrupt]: named by its subject, which
// spells the name, and dated when the broker stored the metadata — left out,
// it would be the one name in the bucket nothing ever judged or cleared.
func (b *Backend) List(ctx context.Context, visit func(objstore.Info) error) error {
	return b.eachMeta(ctx, func(info *jetstream.ObjectInfo) error {
		return visit(infoOf(info))
	})
}

// eachMeta hands visit the newest metadata on every name's subject, its
// ModTime the instant the broker stored it — a delete marker the library's
// own delete left included, and metadata it cannot decode as an object of no
// bytes and no pieces named by its subject. It logs and steps over only a
// subject that spells no name at all. See [Backend.List] for why it is a
// consumer of its own.
func (b *Backend) eachMeta(ctx context.Context, visit func(*jetstream.ObjectInfo) error) error {
	// NO STALL: a listing is waited on for as long as its caller allows,
	// since a visitor that is slow holds the next name back on purpose.
	r, msg, err := b.consume(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects:   []string{metaSubjects},
		DeliverPolicy:    jetstream.DeliverLastPerSubjectPolicy,
		MaxResetAttempts: listResets,
	}, walkAhead, 0)
	if err != nil {
		return fmt.Errorf("natsobj: list the bucket: %w", err)
	}
	if r == nil {
		return nil
	}
	defer r.stop(ctx)
	for {
		meta, err := msg.Metadata()
		if err != nil {
			return fmt.Errorf("natsobj: list the bucket: %w", err)
		}
		var info jetstream.ObjectInfo
		named := true
		if jerr := json.Unmarshal(msg.Data(), &info); jerr != nil {
			// NO OBJECT, AND STILL A NAME: what the subject spells, held
			// with no bytes and no pieces — see [Backend.List].
			name, ok := nameOfMeta(msg.Subject())
			if ok {
				info = jetstream.ObjectInfo{ObjectMeta: jetstream.ObjectMeta{Name: name}}
			} else {
				log.WarnContext(ctx, "object_metadata_unnamed", "subject", msg.Subject(),
					"error", jerr, "detail", "metadata on a subject that spells no name; "+
						"nothing can judge or clear it, so it is left where it is")
				named = false
			}
		}
		// STEPPED OVER, NEVER RETURNED FROM: the walk still advances below,
		// since the next message is read at the bottom of the loop and a
		// `continue` here would read this one again for ever.
		if named {
			info.ModTime = meta.Timestamp
			if verr := visit(&info); verr != nil {
				return verr
			}
		}
		if meta.NumPending == 0 {
			return nil
		}
		if msg, err = r.next(ctx); err != nil {
			return fmt.Errorf("natsobj: list the bucket: %w", err)
		}
	}
}

// pieceSubjects is every subject the bucket keeps an object's pieces on:
// `$O.<bucket>.C.<nuid>`, one per object.
const pieceSubjects = "$O." + Bucket + ".C.>"

// Pending implements [objstore.Backend]: every subject holding pieces that no
// object's metadata names, dated by its oldest piece.
//
// THE ONLY UPLOADS THE BROKER CAN LEAVE UNFINISHED, and nothing names them:
// the object store names an object's pieces only in the metadata message that
// completes it, so the pieces of a put that died before that message — a
// process killed mid-upload — and the pieces a delete interrupted between its
// two purges ([Backend.Delete]) are on a subject no name reaches, and a
// listing of the names never shows them. Each is [objstore.Pending] with no
// name and its piece identifier as the ID.
//
// IN THIS ORDER — the piece subjects, then the metadata, then each orphan's
// oldest piece — so a put that completes during the walk is never answered:
// its metadata, written after its pieces, is read after them too. One that is
// still running is answered, as every put in flight is pending; the collector
// abandons only what began longer ago than a put may take. Every read is the
// LEADER's: the subject listing is its stream info, and an orphan's oldest
// piece is its message read ([jsapi.Leader]) — a replica behind the quorum
// would answer a subject as holding nothing it in fact holds. A subject the
// leader holds nothing on by then was finished or purged in between, and is
// stepped over.
func (b *Backend) Pending(ctx context.Context, visit func(objstore.Pending) error) error {
	held, err := b.stream.Info(ctx, jetstream.WithSubjectFilter(pieceSubjects))
	if err != nil {
		return fmt.Errorf("natsobj: list the bucket's pieces: %w", err)
	}
	// THE CANDIDATES ARE THE SUBJECT LISTING ITSELF, whittled down by the
	// metadata walk, so the pass holds no more than the broker's answer
	// already did — never a second copy of the bucket's inventory.
	orphans := held.State.Subjects
	if len(orphans) == 0 {
		return nil
	}
	err = b.eachMeta(ctx, func(info *jetstream.ObjectInfo) error {
		// A DELETE MARKER NAMES NOTHING, whatever NUID it kept: the
		// delete that wrote it purged the pieces, and pieces still on
		// its subject are the leftovers of one that failed between.
		if !info.Deleted && info.NUID != "" {
			delete(orphans, pieceSubject(info.NUID))
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, subject := range slices.Sorted(maps.Keys(orphans)) {
		nuid, ok := strings.CutPrefix(subject, pieceSubject(""))
		if !ok {
			continue
		}
		first, err := b.leader.First(ctx, subject)
		switch {
		case errors.Is(err, jsapi.ErrNoMessage):
			continue
		case err != nil:
			return fmt.Errorf("natsobj: read the oldest piece on %s: %w", subject, err)
		}
		if err := visit(objstore.Pending{ID: nuid, Started: first.Time.UTC()}); err != nil {
			return err
		}
	}
	return nil
}

// Abandon implements [objstore.Backend]: a purge of the pieces' subject.
//
// THE ID IS A SINGLE SUBJECT TOKEN or it is refused: it is spliced into the
// subject the purge filters on, and one carrying a wildcard or a dot would
// purge the pieces of every object in the bucket.
func (b *Backend) Abandon(ctx context.Context, p objstore.Pending) error {
	if p.ID == "" || strings.ContainsAny(p.ID, ".*> \t\r\n") {
		return fmt.Errorf("natsobj: abandon %q: not a piece identifier", p.ID)
	}
	if err := b.stream.Purge(ctx, jetstream.WithPurgeSubject(pieceSubject(p.ID))); err != nil {
		return fmt.Errorf("natsobj: abandon the pieces %s: %w", p.ID, err)
	}
	return nil
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
