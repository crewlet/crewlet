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
// # A byte reservation, and why it is not optional
//
// The bucket is created with a byte ceiling ([Config.MaxBytes]), and a bucket
// found without one is refused unless the operator wrote the figure to give it. The state logs reserve their ceilings against the
// broker's limit, and everything else on the broker (every seat's mailbox
// and every coordination bucket, where the leases, ledgers and budget
// counters live) shares what is left. A bucket with no ceiling grows into
// that remainder, and the collector does not bound it: it deletes garbage,
// never the files a company keeps. A full remainder refuses a lease heartbeat,
// which is a fleet outage rather than a file outage; a full bucket refuses an
// upload ([objstore.ErrFull]) and nothing else. So the bucket's growth is a
// RESERVATION of its own, granted by the broker at create time, and how
// close it is to full is what [Backend.Usage] answers, feeding the bucket
// gauges and objects_bucket_near_ceiling.
// [Backend.Raise] lifts it and never lowers it: the engine raises it to a
// larger store.objects.nats_max_bytes at a data node's boot, where the broker
// can grant the difference beside what the logs and coordination keep.
//
// A bucket found without a ceiling is refused naming the `nats stream update`
// that bounds it; [Open] keeps the ceiling an existing bucket holds, so a
// smaller figure in config never shrinks one. Unset, the engine derives the
// figure: the smaller of a quarter of the broker's limit and what the state
// logs' five eighths and a 1 GiB coordination floor leave. A raise the broker
// cannot grant without breaking that floor is refused, and the engine raises
// objects_bucket_raise_refused. A full bucket answers [objstore.ErrFull] inside
// an [objstore.Full] naming [FullRemedy]. [Backend.Seal] makes the bucket
// read-only for good once `crewlet objects migrate --to s3` has moved the
// fleet off it. The bucket advertises no presign: every byte goes through the
// node.
//
// # The bucket's own format, written and read through the stream
//
// The bucket is a NATS object store in that store's own documented layout:
// an object is a run of messages on `$O.<bucket>.C.<nuid>`, closed by one
// message on `$O.<bucket>.M.<name, base64url>` naming them — so every object
// stays readable by NATS's own tools (`nats object get`). The library only
// creates the bucket; the backend writes it ITSELF ([Backend.Put]) rather than
// through the library's put, which could not be made safe:
//
//   - IT FAILED A PUT THAT WAS MOVING. It gave each message's acknowledgement
//     five seconds from its publish, and a message published behind bytes the
//     link has not yet carried is acknowledged only once they have crossed —
//     so a put across any link slower than its queued bytes in five seconds
//     failed while it moved: 2 MiB at 1.25× the floor failed at 10.0 s, over
//     TCP and through a leaf alike, where the backend's own put stored it in
//     48.5 s, the link's own time. A node without `data` uploading across a
//     slow leaf link is exactly the floor every leg is promised
//     ([objstore.MiBPace]), broken by a timeout nobody chose. The same five
//     seconds failed a 16 MiB put on a member's own client while the logs
//     were busy and their flushes slow (spike S7): a put's success hung on
//     the disk's flush time rather than on whether the broker was still
//     taking its bytes.
//   - ON A FROZEN LINK IT TOOK THE CONNECTION DOWN. Its 4,000 outstanding
//     messages filled the socket until the connection's own write deadline
//     failed the node's whole connection — leases, events and subscriptions
//     with it (65.01 s, `i/o timeout`); and through a frozen leaf it failed in
//     five seconds on a metadata lookup a key that is never reused does not
//     need.
//   - ITS RETRY RE-SENT ANOTHER PIECE'S BYTES. A piece answered "no
//     responders" is published again 250 ms later from the library's one
//     reused message over its one reused buffer, which by then holds a later
//     piece (read from the code, not reproduced).
//
// The backend's put publishes FULL messages ([MessageBytes]) in order from one
// buffer, read through [objstore.Fill] so the object ends only where its
// reader answers io.EOF itself — never at a body cut short — with no publish
// retry and its messages out held to a WINDOW ([Backend.Window]: across a
// leaf's link or to an external cluster the link's one window, "The window
// across a link" below; [memberWindow] on a member's own client); it is ended
// only when the broker has acknowledged nothing for [objstore.Stall] while
// messages are out ([objstore.ErrStalled]), across a metered link only while
// the link carried nothing at the floor ([Backend.held]); and a put that
// fails, the caller giving up among the ways, drains what is out and purges
// what it stored under a context that does not end with the caller's
// ([objstore.ContextReader]) — the metadata
// first, so an object never names pieces that are gone. A piece the stream
// refuses because the bucket holds its whole reservation fails the put as
// [objstore.ErrFull].
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
//     and leave it there. Only the leader answers it, so it is ASKED AGAIN
//     while nobody does, as a metadata read is ([jsprovision.ReadBounded])
//     — a member that still believes in a leader its group has just lost
//     drops the request without a word, and one request waited out to the
//     client's five-second default failed the collector's first pass on a
//     loaded fleet — and so is every page of the sweep's piece listing,
//     the stream info [Backend.Pending] begins with, and the stream's own
//     info a usage reading and a seal begin with. A read whose caller set
//     no deadline keeps those five seconds, asked again inside them. A
//     read whose answer is a whole piece — the leader's next message for a
//     get or a ranged walk, the oldest piece of an unfinished upload — on a
//     node that reaches the bucket across a leaf link is presumed lost only
//     once a piece has had time to cross that link at the floor
//     ([jsapi.Leader.Carrying]), or each request asked again puts another
//     piece on a link the reply is still crossing; a member's reply crosses
//     a route, and keeps the read term.
//   - A STREAM WITH NO LEADER IS WAITED OUT, inside the caller's deadline:
//     every leader read, stream info and consumer create that a member
//     answers 503, err_code 10008 (its group, or the metadata group, knows it
//     has no leader) is asked again until an election ends it
//     ([jsprovision.ThroughElection]). A read whose deadline ends first fails
//     saying the stream had no leader.
//   - A GET IS A CONSUMER OF THE OBJECT'S OWN MESSAGES, read one message at
//     a time under [objstore.Stall] and ended by its context: on a member's
//     own client an ordered consumer the client pulls, and across a link one
//     whose pulls the reader asks itself, one piece a pull inside the link's
//     window (linkread.go). The library's own reader cannot be stopped once a
//     read blocks — the read holds the lock its close needs — so a download
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
//     skipped. And a consumer's end is its REPLICA's end, which the read
//     holds to the LEADER's ([reading]): the broker places a consumer on any
//     replica it counts as up, one still catching up included, so a get
//     ending short of its object's size, a walk short of its offset and a
//     listing short of the newest metadata the leader held when it began
//     ask the leader what follows — the get and the walk going on from a
//     consumer made again, the listing failing.
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
//   - A LISTING IS AN ORDERED CONSUMER OF ITS OWN over the metadata subjects,
//     never the library's List, which gathers the company's whole inventory
//     into one slice, nor its Watch, which never finishes when the newest
//     metadata cannot be decoded. Metadata that does not decode is no object
//     and still a NAME: a stat and a get answer [objstore.ErrCorrupt], and the
//     listing hands the name on, so the collector clears it like any other.
//     [Backend.Pending] lists piece SUBJECTS through the leader, never walks
//     the pieces, which reads the bucket's bytes where the subject index is
//     in memory (2,000 objects: 103 ms against 2.2 ms).
//
// # The window across a link
//
// Across a link every transfer takes its bytes from the link's ONE window
// ([Config.Window], internal/jsapi's Window), which the memory sync's replay
// pulls on the same link share (internal/learning/memsync): a put's piece
// until its acknowledgement, a download's pull of one piece until the piece is
// in, a walk's pull of a window of headers or names until it ends, and a
// leader's read of a whole piece, larger than the window, alone
// ([reading.caughtUp]). A link carries what a node sends in the order it was
// sent, and what comes back in the order it was sent back, so a request sent
// behind the node's transfers (at a first-in, first-out bottleneck, its
// lease link's too: ADR-0174) waits for what
// they hold out BOTH WAYS: held to one window, sized to what the floor
// carries in the request's term less its own round trip
// ([jsapi.WindowCrossing], four seconds, whatever lease the node holds), it
// is answered inside its term. One piece either way fills it, so a link
// carries one piece of the node's transfers at a time.
//
// NOT SIZED BY A MEASURED RATE, which would recover a fast link's throughput:
// a link that drops to the floor mid-transfer (a satellite's rain fade) then
// holds a request behind a window sized for the faster rate, past its term.
// The floor is a promise about the slowest link the engine allows, and the
// window keeps it there. It is also what keeps a stall honest across the
// link: no transfer waits behind more than the window of the node's own.
//
// What it does not hold is what the node's seats are sent: an event crosses
// whole, up to the transport's bound, and a request whose answer queues
// behind one waits for it. A transfer behind one is held while the link moves
// rather than stalled ([Backend.held]).
//
// What one window costs is throughput on a fast link, since the node's
// transfers move one piece a round trip between them: measured below ("What
// one window costs"). Every node across a link pays it: one without `data`
// through its leaf, a seat pod among them, and every node on an external
// broker, data nodes included.
//
// # What one window costs
//
// BenchmarkALinksTransfers, through a leaf (its tunnel included) whose link
// carries a rate and a round trip each way, random bytes, two runs each
// (2026-11-18, two cores of an Intel Core Ultra 7 265U running other work),
// in MB/s, one window against #1359's (a put one piece, a get four in hand):
//
//   - 100 Mbit/s across 100 ms: a lone download 1.04–1.05 against 2.27–2.32;
//     a lone upload 1.04–1.06 either way; both at once 0.53–0.55 down and
//     0.56–0.57 up, against 2.22–2.28 and 1.00–1.03.
//   - Across 20 ms: a lone download 3.62–3.67 against 6.42–6.53; an upload
//     3.35–3.45 either way; both at once 1.77–1.82 and 1.80–1.86, against
//     5.94–6.02 and 2.89–3.01.
//   - At the floor nothing is lost: 0.0346 a lone transfer either way, and
//     0.041 together against 0.043.
//   - On loopback, an in-cluster seat pod's link: a lone download 353–376
//     against 385–424, an upload 119–126 against 104–118, both at once 76–88
//     each way against 250–278 down and 112–116 up.
//
// So a download across a distant link moves about a piece a round trip, and
// transfers both ways at once share that: the price of a request answered
// inside its term behind them (ADR-0161).
//
// # What it costs, measured
//
// `make bench` (BenchmarkBackend, internal/objstore/objstoretest's Bench) at
// `stream.sync: always`, production's default, on a shared four-vCPU VM. The
// three-replica figures come from internal/objstore/natsobj/replicated, a
// package of tests alone: forming a three-member cluster puts a package in the
// solo half, and this package's whole suite would go with it.
//
//   - A member's own client: a 64 KiB put takes about 2 ms, every message
//     being fsynced, and larger puts run at 74–117 MB/s; a whole read at
//     450–740 MB/s, 200–250 verified ([objstore.Store.Open]), which is the
//     CPU's SHA-256 rate. A Stat is a leader read, about 10,000 a second from
//     one goroutine; a listing names 75,000–94,000 objects a second.
//   - Through a leaf, reads run at about half a member's, and a Stat from one
//     goroutine pays the link's round trip, which 32 in flight recover.
//   - At three replicas every message is a quorum append: puts run at a third
//     to three quarters of one member's. A small read pays for making its
//     consumer, a decision of the cluster's, and for the first pulls a cluster
//     drops at that moment ([quietWait]).
//   - A PAGE IS PAID FOR BY ITS OFFSET: 64 KiB at a random offset of a 128 MiB
//     object takes 32–36 ms, the walk of the headers before it, where the S3
//     backend's one ranged request takes half a millisecond. A page deep in a
//     large file costs that walk again every time it is read.
//   - [Backend.Put] matches the library's put on throughput, and costs a
//     quorum round trip more on a small put at three replicas, which the
//     metadata waits for its pieces' acknowledgements in.
//   - A window shared by a connection's puts ([memberWindow]) loses nothing
//     against one a put, and sixty-four shared bought nothing over sixteen.
//     Across a link a put's window of one message runs a lone 16 MiB put at
//     47–83 MB/s, two messages at 109–125, four at 120–139; the link's one
//     window is four.
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
	"math"
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
	// AcrossLink says the connection reaches the stream across a LINK — a
	// leaf's link to its members, or a dial to an external cluster — where
	// every request the node sends after a transfer, or whose answer comes
	// back after one, waits behind what the node's transfers hold out on the
	// link, rather than to the node's own member in its own process.
	AcrossLink bool
	// Window is the ONE window of that link (internal/queue/jetstream's
	// LinkWindow), required with AcrossLink and refused without it: every
	// put's piece, every download's pull of a piece and every walk's pull
	// takes its bytes from it, beside the memory replays' pulls on the same
	// link, so what the node holds out on the link both ways never crosses
	// the floor in more than a request's term leaves ([jsapi.LinkWindow]).
	// On a member's own client the puts share [memberWindow] of their own.
	Window *jsapi.Window
	// Link is the meter of that link (internal/queue/jetstream's LinkMeter),
	// read with AcrossLink alone: what tells a put or a read held behind the
	// link's other traffic from one whose broker stopped answering
	// ([Backend.held]). Nil reads no link, and time alone stalls a transfer;
	// a Link without AcrossLink is refused, since a member has none.
	Link Link

	// MaxBytes is the byte reservation a bucket this node creates is given
	// (store.objects.nats_max_bytes, or the share the engine derives),
	// required, since a bucket with no ceiling is the failure the package
	// doc describes. Open applies it to a bucket that exists only when that
	// bucket holds NO ceiling and the figure is explicit ([checkCeiling]);
	// raising one that holds a ceiling is [Backend.Raise]'s.
	MaxBytes int64

	// Explicit says MaxBytes is what an operator wrote rather than what the
	// engine derived: only a written figure bounds a bucket that holds no
	// ceiling, since a derived one follows the broker's free space.
	Explicit bool
}

// Backend is the objects in the broker's object store.
type Backend struct {
	// js is the client the bucket was opened on, whose connection a put
	// publishes on — see [Backend.Put] — and which a seal updates the
	// stream through.
	js jetstream.JetStream
	// stream is the stream behind the bucket, which a get and a listing
	// consume and a delete and a failed put purge.
	stream jetstream.Stream
	// leader reads the bucket's metadata from the stream's leader, and
	// pieces its pieces: the same reads, given — on a node that reaches the
	// bucket across a link — the time a whole piece takes to cross it at
	// the floor ([jsapi.Leader.Carrying]). Read through
	// [Backend.leaderFor], never by name.
	leader, pieces *jsapi.Leader
	// stall is [objstore.Stall], the longest a put waits for the broker to
	// acknowledge any of its messages out, and a get or a walk for its next
	// message — a field so a test can watch a stall end a transfer without
	// waiting a minute for it.
	stall time.Duration
	// beginPace is [listBeginPace], what a listing's beginning is given for
	// each mebibyte of the bucket ([Backend.listBegin]): a field so a test
	// can give a bucket of a few bytes the beginning a large one is given.
	beginPace time.Duration
	// window is what b's transfers take their bytes from: the link's one
	// window ([Config.Window]) where across reads, the puts' own
	// [memberWindow] where it does not.
	window *jsapi.Window
	// across says the connection crosses a link ([Config.AcrossLink]):
	// downloads and walks then read their consumers a pull at a time, each
	// taken from the window ([reading.fill]).
	across bool
	// link is the meter of that link ([Config.Link]), or nil.
	link Link
	// api is the JetStream API the connection speaks, and timing its
	// provisioning timing, which a read across a link asks its consumer's
	// pulls in ([jsapi.Pulls]).
	api    jsapi.API
	timing jsprovision.Timing
}

// Link is the node's link to the bucket as its meter reads it, at the link's
// own socket (internal/queue/jetstream's LinkMeter).
type Link interface {
	// Moving reports whether the link carried, in either direction, what a
	// link busy at the floor carries, over the last d.
	Moving(d time.Duration) bool
	// Carried is the bytes the far end acknowledged of what the node wrote
	// to the link, and the bytes the node read from it, since the meter was
	// made.
	Carried() (acked, read uint64)
}

// Window is what b's transfers take their bytes from: the link's one window
// where [Config.AcrossLink] said the connection crosses a link to the stream,
// and the puts' own [memberWindow] where it did not. The engine hands it the
// queue's, which a test of its own reads here.
func (b *Backend) Window() *jsapi.Window { return b.window }

// transport is the most bytes one pull's delivery may carry on b's
// connection: its largest message, its max_payload, and the room the broker
// counts beside it ([deliveryOverhead]) — what a pull of one message larger
// than the window is bounded at, let out of the window alone.
func (b *Backend) transport() int { return int(b.js.Conn().MaxPayload()) + deliveryOverhead }

// heldCap is the longest a transfer across a metered link may hear nothing
// while the link moves ([Backend.held]): its stall, and one message at the
// transport's bound crossing the link at the floor ahead of it
// ([jsapi.Delivering]), 308 s at the shipped stall. A link that moves for
// longer without carrying the transfer anything creeps below the floor, or
// carries more than one such message ahead of it, and the transfer ends.
func (b *Backend) heldCap() time.Duration {
	return b.stall + jsapi.Delivering(b.transport(), objstore.MiBPace)
}

// held reports whether a transfer that has heard nothing for quiet, at least
// its stall, is HELD behind the link's other traffic rather than stalled:
// its link is metered and carried, in either direction, what a link busy at
// the floor carries over the last stall, and quiet is short of [Backend.heldCap].
// A transfer across no metered link is stalled by time alone.
func (b *Backend) held(quiet time.Duration) bool {
	return b.link != nil && quiet < b.heldCap() && b.link.Moving(b.stall)
}

// deliveryOverhead is what the broker counts against a pull's bytes beside a
// message's payload: the subject, the reply subject carrying the message's
// position, and the header block. Four KiB, as internal/learning/memsync
// counts it: a piece's subject and reply are a few hundred bytes, and a
// walk's headers-only delivery carries its size in one header.
const deliveryOverhead = 4 << 10

// Open binds to the bucket, creating it first if it is not there: A LOOKUP,
// THEN A CREATE, NEVER AN UPDATE. The library's create-or-update sends an
// UPDATE first, and an update of a stream just created, its group not yet
// through its first election, reaches members none of which leads it while
// only a leader answers (processClusterUpdateStream): a booting fleet waits
// out the whole ask term silently. So Open READs ([jsprovision.ReadWith]) and
// creates only when told the bucket is absent or nobody answered
// ([jsprovision.Timing.Place]), then reads it back ([jsprovision.SettleWith])
// rather than write again: the create may have landed or lost to a peer. An
// existing bucket is never reconfigured. One replicated BELOW THIS NODE IS
// REFUSED, as the queue refuses it: an upload acknowledged there proves fewer
// copies than `stream.replicas` promises.
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
	if err := checkLink(cfg); err != nil {
		return nil, err
	}
	if cfg.MaxBytes <= 0 {
		return nil, fmt.Errorf("natsobj: the %s bucket needs a byte reservation, and "+
			"this node was given %d: a bucket with no ceiling grows into the broker "+
			"remainder the fleet's coordination records live in", Bucket, cfg.MaxBytes)
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

	_, err := lookupBucket(ctx, js, timing)
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
		if _, err = createBucket(ctx, js, timing, replicas, cfg.MaxBytes); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("natsobj: look up the %s bucket: %w", Bucket, err)
	}

	// THE STREAM BEHIND IT, which a get and a listing consume — read back
	// rather than read once, because a bucket a peer has just made can still
	// be not there on the member that answers.
	stream, err := jsprovision.SettleWith(ctx, timing,
		func(ctx context.Context) (jetstream.Stream, error) { return js.Stream(ctx, Stream) })
	if err != nil {
		return nil, fmt.Errorf("natsobj: bind the %s stream: %w", Stream, err)
	}
	held := stream.CachedInfo().Config
	if held.MaxBytes <= 0 && cfg.Explicit {
		bounded, boundErr := boundBucket(ctx, js, timing, held, cfg.MaxBytes)
		if boundErr != nil {
			return nil, boundErr
		}
		stream, held = bounded, bounded.CachedInfo().Config
	}
	if got := held.Replicas; got < replicas {
		return nil, fmt.Errorf("natsobj: the %s bucket is replicated %dx and this "+
			"node is configured for %dx (stream.replicas): an upload acknowledged "+
			"there would prove fewer copies than this node promises — raise the "+
			"bucket's replicas or align stream.replicas on every node",
			Bucket, got, replicas)
	}
	if err := checkCeiling(ctx, held.MaxBytes, cfg); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return nil, err
	}
	leader, err := jsapi.NewLeader(js, Stream)
	if err != nil {
		return nil, fmt.Errorf("natsobj: address the %s stream's leader: %w", Stream, err)
	}
	api, err := jsapi.Of(js)
	if err != nil {
		return nil, fmt.Errorf("natsobj: address the %s stream's consumers: %w", Stream, err)
	}
	// A PIECE'S REPLY IS GIVEN ITS CROSSING ONLY WHERE THERE IS A LINK TO
	// CROSS: a member's reads reach the leader over a cluster route, which
	// carries a piece in milliseconds, and a term sized for a leaf link at
	// the floor would hold a member's dropped piece read seven seconds
	// before it is asked again, where the read term asks it at two.
	b := &Backend{js: js, stream: stream, leader: leader, pieces: leader, stall: objstore.Stall,
		beginPace: listBeginPace, window: newMemberWindow(), api: api, timing: timing}
	if cfg.AcrossLink {
		b.across, b.window, b.link = true, cfg.Window, cfg.Link
		b.pieces = leader.Carrying(MessageBytes, objstore.MiBPace)
	}
	return b, nil
}

// checkLink refuses a [Config] whose link does not add up: a node across a
// link handed no window would take one of its own, and let its transfers out
// beside its link's other ones; a member handed a window or a meter has no
// link for either.
func checkLink(cfg Config) error {
	switch {
	case cfg.AcrossLink && cfg.Window == nil:
		return errors.New("natsobj: a node whose connection reaches the bucket across a link " +
			"transfers inside its link's one window, and none was handed it (Config.AcrossLink " +
			"is set without Config.Window)")
	case !cfg.AcrossLink && (cfg.Window != nil || cfg.Link != nil):
		return errors.New("natsobj: a node whose connection reaches the bucket on its own member " +
			"crosses no link, so it has none to window or meter (Config.Window or Config.Link is " +
			"set without Config.AcrossLink)")
	}
	return nil
}

// checkCeiling refuses a bucket holding NO ceiling where this node's figure is
// derived, naming store.objects.nats_max_bytes ([boundBucket]) and `nats stream
// update`; a bucket holding one is the engine's to raise ([Backend.Raise]).
func checkCeiling(_ context.Context, held int64, cfg Config) error {
	if held > 0 {
		return nil
	}
	return fmt.Errorf("natsobj: the %s bucket holds no byte ceiling, so its "+
		"files grow into the broker remainder the fleet's leases and ledgers "+
		"live in, and a full remainder stops the fleet rather than an upload. "+
		"Give it one before this node starts: set store.objects.nats_max_bytes, "+
		"which a node applies at boot to a bucket holding no ceiling (%d is "+
		"the share this node derived), or run `nats stream update %s "+
		"--max-bytes=%d` against the broker", Bucket, cfg.MaxBytes, Stream, cfg.MaxBytes)
}

// lookupBucket asks whether the bucket exists, as a read: at the read term, and
// under the lookup ceiling every read is held to ([jsprovision.ReadWith]).
func lookupBucket(ctx context.Context, js jetstream.JetStream, timing jsprovision.Timing) (jetstream.ObjectStore, error) {
	return jsprovision.ReadWith(ctx, timing,
		func(ctx context.Context) (jetstream.ObjectStore, error) { return js.ObjectStore(ctx, Bucket) }, nil)
}

// boundBucket gives a bucket that holds no ceiling the one an operator wrote
// in store.objects.nats_max_bytes, the one reservation gesture an embedded
// broker can take. Only the ceiling changes: everything else the bucket's
// stream holds is written back as it was read. The broker grants the
// reservation in full or refuses it, and a refusal stops the boot with the
// broker's reason, as a create the broker cannot grant does.
func boundBucket(ctx context.Context, js jetstream.JetStream, timing jsprovision.Timing,
	held jetstream.StreamConfig, maxBytes int64) (jetstream.Stream, error) {

	held.MaxBytes = maxBytes
	updateCtx, cancel := context.WithTimeout(ctx, timing.Budget)
	defer cancel()
	stream, err := js.UpdateStream(updateCtx, held)
	if err != nil {
		return nil, fmt.Errorf("natsobj: bound the %s bucket at %d bytes "+
			"(store.objects.nats_max_bytes): %w", Bucket, maxBytes, err)
	}
	log.InfoContext(ctx, "natsobj_bucket_bounded", "bucket", Bucket, "max_bytes", maxBytes,
		"detail", "the bucket held no byte ceiling and store.objects.nats_max_bytes "+
			"names one, so this node gave it that reservation")
	return stream, nil
}

// createBucket creates the bucket at replicas copies, or reads back the one a
// peer made.
//
// ctx IS OPEN'S, and the create's own deadline is derived here, because the
// read-back runs precisely when that deadline may have expired — see
// [jsprovision.SettleWith].
func createBucket(ctx context.Context, js jetstream.JetStream, timing jsprovision.Timing,
	replicas int, maxBytes int64) (jetstream.ObjectStore, error) {

	createCtx, cancel := context.WithTimeout(ctx, timing.Budget)
	defer cancel()
	var store jetstream.ObjectStore
	err := timing.Place(createCtx, func(ctx context.Context) error {
		var e error
		// CREATE, NOT CreateOrUpdate — see [Open].
		//
		// WITH A BYTE CEILING (MaxBytes), which the broker grants in full
		// as a reservation: see the package doc for why the bucket may not
		// grow into the remainder the coordination records share. The
		// library creates the stream to refuse a put past it rather than
		// discard an old object.
		store, e = js.CreateObjectStore(ctx, jetstream.ObjectStoreConfig{
			Bucket: Bucket,
			// WRITTEN ONCE, by whichever node creates the bucket: nothing
			// here updates a bucket that exists, so a later build's text
			// reaches only a fleet's first boot. It is read by nothing.
			Description: "Crewlet files, one object per upload",
			Storage:     jetstream.FileStorage,
			Replicas:    replicas,
			MaxBytes:    maxBytes,
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
	if jsprovision.OutOfCapacity(err) {
		// THE RESERVATION DOES NOT FIT, which the broker says without
		// naming a number: the figure and the two fields that move it are
		// what an operator needs, and the broker's own text is neither.
		return nil, fmt.Errorf("natsobj: create the %s bucket at %d copies with a "+
			"%d-byte reservation: %w. The broker cannot reserve it beside what it "+
			"has already granted. Lower store.objects.nats_max_bytes, raise "+
			"stream.store_max_bytes, or give the broker's volume more room",
			Bucket, replicas, maxBytes, err)
	}
	if jsprovision.Refused(err) {
		// REFUSED OUTRIGHT, so nothing was made and there is nothing to
		// read back — the refusal is the answer.
		return nil, fmt.Errorf("natsobj: create the %s bucket at %d copies: %w", Bucket, replicas, err)
	}
	// THE NAME WAS TAKEN — a peer won the race — or nobody said whether the
	// create landed: read back what is there, ON ctx, NOT createCtx.
	store, readErr := jsprovision.SettleWith(ctx, timing,
		func(ctx context.Context) (jetstream.ObjectStore, error) { return js.ObjectStore(ctx, Bucket) })
	if readErr != nil {
		// BOTH WRAPPED, as at every other create site: the create's error
		// says what went wrong and the read-back's whether the bucket is
		// really absent, and a caller asks either with [errors.Is].
		return nil, fmt.Errorf("natsobj: create the %s bucket at %d copies: %w "+
			"(and reading it back: %w)", Bucket, replicas, err, readErr)
	}
	return store, nil
}

// full reports a publish the broker refused because the bucket's stream holds
// its MaxBytes.
//
// BY THE SERVER'S OWN TEXT, [maxBytesExceeded]: a stream created to discard
// nothing answers a message past its ceiling with its store's ErrMaxBytes,
// carried back as the generic store failure (10077) every other store error
// shares, so the code alone cannot tell a full bucket from a failing disk and
// the description is the one part that does.
func full(err error) bool { return strings.Contains(err.Error(), maxBytesExceeded) }

// maxBytesExceeded is the pinned nats-server's ErrMaxBytes, verbatim
// (server/store.go).
const maxBytesExceeded = "maximum bytes exceeded"

// FullRemedy is the room the bucket needs once it holds its whole reservation
// ([objstore.Full.Remedy]): three gestures, each an operator's. A raised
// store.objects.nats_max_bytes reaches the bucket at a data node's boot
// ([Backend.Raise]), on every broker, the embedded one with no listener too.
const FullRemedy = "an operator makes room: raises store.objects.nats_max_bytes " +
	"and restarts a data node, moves the files to a bucket (crewlet objects " +
	"migrate -to s3), or lets the collector remove what nothing names"

// Usage answers how much of the bucket's reservation its objects take, from
// the stream's own state and configuration: the ceiling the BROKER enforces,
// never the figure this node was configured with, which a bucket that exists
// holds only once a data node has raised it to that figure.
//
// Bytes counts every message the stream holds (pieces and metadata, and the
// pieces of an upload in flight) because that is what the broker holds
// against MaxBytes and what a put is refused on.
func (b *Backend) Usage(ctx context.Context) (objstore.Usage, error) {
	info, err := b.info(ctx)
	if err != nil {
		return objstore.Usage{}, fmt.Errorf("natsobj: read the %s stream's usage: %w", Stream, err)
	}
	return objstore.Usage{Bytes: int64(info.State.Bytes), MaxBytes: info.Config.MaxBytes}, nil
}

// ErrNoRoom is a raise of the bucket's reservation the broker refused for want
// of room: nothing changed, and the bucket keeps the reservation it held.
var ErrNoRoom = errors.New("natsobj: the broker cannot reserve the raised byte ceiling")

// Raise lifts the bucket's reservation to maxBytes in one update of the stream's
// configuration as its leader holds it, only MaxBytes changed, and answers the
// one it holds after. It never lowers one; a broker refusal is [ErrNoRoom].
func (b *Backend) Raise(ctx context.Context, maxBytes int64) (int64, error) {
	info, err := b.info(ctx)
	if err != nil {
		return 0, fmt.Errorf("natsobj: read the %s stream before raising its reservation: %w", Stream, err)
	}
	cfg := info.Config
	held := cfg.MaxBytes
	switch {
	case held <= 0 || held >= maxBytes:
		return held, nil
	case cfg.Sealed:
		return held, fmt.Errorf("natsobj: the %s bucket is sealed, so its reservation "+
			"is never raised: its files moved to another store", Bucket)
	}
	cfg.MaxBytes = maxBytes
	stream, err := b.js.UpdateStream(ctx, cfg)
	switch {
	case jsprovision.OutOfCapacity(err):
		return held, fmt.Errorf("natsobj: raise the %s bucket from %d to %d bytes: %w: %w",
			Bucket, held, maxBytes, ErrNoRoom, err)
	case err != nil:
		return held, fmt.Errorf("natsobj: raise the %s bucket from %d to %d bytes: %w",
			Bucket, held, maxBytes, err)
	}
	return stream.CachedInfo().Config.MaxBytes, nil
}

// Seal makes the bucket read-only for good: the broker refuses every put and
// every delete after it, and every object stays readable.
//
// IT IS THE FENCE A MOVE TO ANOTHER STORE CLOSES BEHIND ITSELF. `crewlet
// objects migrate --to s3` seals the bucket once its final pass has copied
// every object and before it moves the fleet's record, so a node still
// running on this bucket (one not yet restarted onto the new store) has its
// uploads refused rather than stored where no node will look for them. A
// sealed stream cannot be unsealed, which is the point: the bucket is kept
// for an operator to read and delete, never to write again. Sealing a sealed
// bucket is not an error.
func (b *Backend) Seal(ctx context.Context) error {
	info, err := b.info(ctx)
	if err != nil {
		return fmt.Errorf("natsobj: read the %s stream before sealing it: %w", Stream, err)
	}
	if info.Config.Sealed {
		return nil
	}
	cfg := info.Config
	cfg.Sealed = true
	if _, err := b.js.UpdateStream(ctx, cfg); err != nil {
		return fmt.Errorf("natsobj: seal the %s stream: %w", Stream, err)
	}
	return nil
}

// info is the bucket's stream info as the stream's LEADER answered it, asked
// as a metadata read is ([jsprovision.ReadBounded]).
//
// ASKED AGAIN WHILE NOBODY ANSWERS, because only the leader does: a member
// that still believes in a leader its group has lost returns without a word
// (jsStreamInfoRequest), so one request would wait out the deadline, or the
// client's five seconds with none, for a reply nobody sends.
// THROUGH A HANDLE OF EACH REQUEST'S OWN, never [Backend]'s stream: the
// client caches an info reply on the handle with no lock, the hedge can have
// two requests out, and the bucket's handle serves every get and listing. A
// leaderless answer is waited out ([leaderReads]).
func (b *Backend) info(ctx context.Context) (*jetstream.StreamInfo, error) {
	return jsprovision.ThroughElection(ctx, Stream, func(ctx context.Context) (*jetstream.StreamInfo, error) {
		return jsprovision.ReadBounded(ctx, b.js.Options().DefaultTimeout,
			func(ctx context.Context) (*jetstream.StreamInfo, error) {
				stream, err := b.js.Stream(ctx, Stream)
				if err != nil {
					return nil, err
				}
				return stream.CachedInfo(), nil
			}, nil)
	})
}

// metaSubject is the subject the bucket keeps name's metadata on, and
// pieceSubject the subject an object's pieces are on — the object store's own
// documented layout, the name in base64url exactly as the library encodes it.
func metaSubject(name string) string {
	return "$O." + Bucket + ".M." + base64.URLEncoding.EncodeToString([]byte(name))
}

func pieceSubject(nuid string) string { return "$O." + Bucket + ".C." + nuid }

// leaderFor is the leader reads of what filter names: b's pieces for an
// object's pieces, every message of which may be a whole piece, and its
// leader for the metadata, whose messages are a few hundred bytes. Every
// leader MESSAGE read here takes its reader from this, so a read of a piece
// is never asked again while the piece is still crossing a link
// ([jsapi.Leader.Carrying]); a subject listing answers names and counts, and
// is the metadata leader's.
func (b *Backend) leaderFor(filter string) leaderReads {
	if strings.HasPrefix(filter, metaSubject("")) {
		return leaderReads{b.leader}
	}
	return leaderReads{b.pieces}
}

// leaderReads is the stream's leader reads, each waiting out an election
// inside its caller's deadline ([jsprovision.ThroughElection]): a member that
// knows the group has no leader answers so at once, and that is no answer
// about the object. The only way natsobj asks the stream's leader.
type leaderReads struct{ l *jsapi.Leader }

func (r leaderReads) Last(ctx context.Context, subject string) (jsapi.Message, error) {
	return jsprovision.ThroughElection(ctx, Stream, func(ctx context.Context) (jsapi.Message, error) {
		return r.l.Last(ctx, subject)
	})
}

func (r leaderReads) First(ctx context.Context, subject string) (jsapi.Message, error) {
	return jsprovision.ThroughElection(ctx, Stream, func(ctx context.Context) (jsapi.Message, error) {
		return r.l.First(ctx, subject)
	})
}

func (r leaderReads) Next(ctx context.Context, subject string, from uint64) (jsapi.Message, error) {
	return jsprovision.ThroughElection(ctx, Stream, func(ctx context.Context) (jsapi.Message, error) {
		return r.l.Next(ctx, subject, from)
	})
}

// Subjects lists filter's subjects a page at a time, asked again whole after a
// leaderless answer: visit must take a page it was handed before.
func (r leaderReads) Subjects(ctx context.Context, filter string, visit func(map[string]uint64) error) error {
	_, err := jsprovision.ThroughElection(ctx, Stream, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, r.l.Subjects(ctx, filter, visit)
	})
	return err
}

// Term is how long one read is given before it is asked again.
func (r leaderReads) Term() time.Duration { return r.l.Term() }

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
	msg, err := b.leaderFor(metaSubject(name)).Last(ctx, metaSubject(name))
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
// ENOUGH that the next message is usually there when a reader asks, so a
// download streams at the wire's pace rather than a round trip per message
// (the client asks for more once half the hand is read). NO MORE, because a
// read on a slow link waits on the hand WHOLE ([pullHeartbeat]), and a node
// serving several downloads holds a hand in memory for each, where the
// library's default of five hundred would be sixty-four mebibytes a download.
// A hand of a mebibyte takes thirty seconds at the floor, and no heartbeat
// could both outlast that and leave a replaced consumer's first hand time to
// arrive inside [objstore.Stall]'s minute.
const readAhead = 4

// walkAhead is how many messages a WALK holds in hand — a listing over the
// bucket's metadata, or a ranged get's walk over an object's message headers.
//
// FIVE HUNDRED, the client's default, stated rather than inherited: a walk's
// messages are metadata or headers, a few hundred bytes each, so a full hand
// is a hundred-odd kibibytes, where [readAhead] must be small because each of
// ITS messages is a whole [MessageBytes]. It is also where a walk's consumer
// can be lost unnoticed: what is in hand is read without asking the broker, so
// a consumer reaped while a slow visitor held the listing is found only at
// the next pull; [TestAListingWhoseConsumerIsLostFinishesOnItsReplacement]
// stages that past this many names.
const walkAhead = 500

// pullHeartbeat is how often the broker tells a get or a walk waiting on a
// pull that it is still being served; two missed and the client replaces the
// consumer from the last message read, the one way a read notices the broker
// took it away (reaped, or dropped in a leader change).
//
// DERIVED from the hand and the floor ([objstore.MiBPace]). A heartbeat comes
// only while a pull waits with nothing to deliver, and a compressed link
// carries a flush as one block decoded only whole, so a read hears nothing
// until its whole hand ([readAhead]) has arrived: fifteen seconds at the
// floor. Two heartbeats are thirty, so a consumer lost under a read costs its
// [quietWait], those thirty and a replacement's first hand: forty-six, inside
// [objstore.Stall]'s minute (TestAReadsHeartbeatOutlastsItsHandAtTheFloor).
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
// its first, by asking its consumer whether the pull ever arrived; at later
// ones, by waiting again.
//
// A CLUSTER DROPS A NEW CONSUMER'S FIRST PULL, routinely, silently. Heartbeats
// cannot tell that read from a first hand crossing a slow link, but its
// CONSUMER can: one holding no pull and having delivered nothing is made
// again; one holding the pull or having delivered is waited on. Asked, never
// guessed: a read remade while its hand is on the wire waits behind it. The
// client also skips a pull now and then (nats.go decides to ask only as a
// wait begins), so a wait begun again decides again. ONE SECOND: about twenty
// times the worst p99 `make bench` measured at three replicas (55 ms).
const quietWait = time.Second

// pulls is what every get and walk asks its consumer for: up to batch
// messages in hand, under the pull expiry and heartbeat above.
func pulls(batch int) []jetstream.PullMessagesOpt {
	return []jetstream.PullMessagesOpt{jetstream.PullMaxMessages(batch),
		jetstream.PullExpiry(pullExpiry), jetstream.PullHeartbeat(pullHeartbeat)}
}

// linkHeartbeat is how often the broker tells a read ACROSS A LINK that is
// waiting on a pull that the pull is still being served: one piece's crossing
// at the floor ([messageCrossing]), 3.75 seconds. A read across a link holds
// one pull out at a time, taken from the link's window ([reading.fill]), so
// the longest a healthy read hears nothing behind its own transfers is the
// window crossing, inside two heartbeats; two missed while the link carries
// nothing at the floor are the consumer lost, and it is made again
// ([reading.judge]). While the link moves, the beats are behind the link's
// other traffic, not lost.
const linkHeartbeat = messageCrossing

// messageCrossing is how long one [MessageBytes] message takes to cross a
// link at the floor every leg is promised ([objstore.MiBPace]): 3.75 seconds.
const messageCrossing = objstore.MiBPace * MessageBytes / objstore.MiB

// linkExpiry is how long one pull a read across a link sends waits for
// messages before the broker answers it as expired: twice [linkHeartbeat],
// the shortest the broker accepts beside it. It bounds nothing a reader
// waits for: an expired pull is asked again, its turn in the window taken
// again.
const linkExpiry = 2 * linkHeartbeat

// errQuiet is a wait for a message that ended with nothing arrived, before
// the read's own stall or its caller's context ended.
var errQuiet = errors.New("natsobj: no message yet")

// errUnpulled is a read's new consumer across a link that never had its first
// pull ([quietWait]), made again by [reading.open]; and errLost one whose pull
// went quiet for two heartbeats on a link that carried nothing, or for as much
// as the link carried a message at the transport's bound and a window
// meanwhile, made again past what it handed over ([reading.again]).
var (
	errUnpulled = errors.New("natsobj: the read's consumer never had its first pull")
	errLost     = errors.New("natsobj: the read's consumer went quiet")
)

// message is one message a read hands over, however its consumer was read: a
// member's ordered consumer, or a reader's own pulls across a link.
type message struct {
	subject string
	data    []byte
	header  nats.Header
	// seq is its stream sequence, pending how many its consumer holds after
	// it, and stamp when the stream stored it.
	seq, pending uint64
	stamp        time.Time
}

// fromConsumer is m, a message an ordered consumer delivered.
func fromConsumer(m jetstream.Msg) (*message, error) {
	meta, err := m.Metadata()
	if err != nil {
		return nil, err
	}
	return &message{subject: m.Subject(), data: m.Data(), header: m.Headers(),
		seq: meta.Sequence.Stream, pending: meta.NumPending, stamp: meta.Timestamp}, nil
}

// fromPull is ev, a delivery a reader's own pull took.
func fromPull(ev jsapi.Event) *message {
	return &message{subject: ev.Msg.Subject, data: ev.Msg.Data, header: ev.Msg.Header,
		seq: ev.Meta.Sequence.Stream, pending: ev.Meta.NumPending, stamp: ev.Meta.Timestamp}
}

// errBehind is a read the broker kept placing on a replica behind the
// stream's leader for as long as a read may wait for a message.
var errBehind = errors.New("natsobj: every consumer the broker made for the read was on " +
	"a replica behind the stream's leader")

// everything is the [reading.through] of a read every message of which the
// leader holds on its filter is its own: an object's pieces, every one of
// them stored before the metadata a get read from the leader.
const everything = math.MaxUint64

// reading is one ordered consumer being read (a get's, seek's or listing's),
// each message waited for no longer than stall, or as long as the caller's
// context allows where stall is zero.
//
// THE END OF A REPLICA'S COPY IS NOT THE END OF THE STREAM: the broker makes
// the consumer on ONE replica at random among those it counts as up, a member
// still catching up after a restart included, so its "nothing follows" may
// be a replica's end. Taken as the stream's, it reads an object as zero bytes
// (DAMAGED), a range as past the end, a listing as missing objects. So an end
// counts only where it is the one the read wanted (an object's last byte) or
// the LEADER agrees ([reading.caughtUp]); else the read goes on from a
// consumer made again ([reading.resume]).
type reading struct {
	b *Backend
	// filter is the subject the read is over, and cfg how its consumer is
	// made — the filter included — so a consumer made again is made alike.
	filter string
	// leader asks the stream's leader what follows on filter
	// ([Backend.leaderFor]).
	leader leaderReads
	cfg    jetstream.OrderedConsumerConfig
	batch  int
	stall  time.Duration
	// through is the newest sequence the read must reach on its filter:
	// what the leader held there when a listing began — a message stored
	// after it is no message the listing owes anybody — or [everything].
	through uint64
	// begin is how long the broker is given to make each of the read's
	// consumers: a listing's, which reads the bucket to begin
	// ([Backend.listBegin]), or zero for a read whose own deadline bounds it.
	begin time.Duration

	cons jetstream.Consumer
	msgs jetstream.MessagesContext

	// ACROSS A LINK ([Backend.across]) the consumer is read through the
	// reader's own pulls, one at a time, each taken from the link's window
	// ([reading.fill]); hand is what the last pull delivered and the read
	// has not yet handed over.
	pulls *jsapi.Pulls
	hand  []*message
	// want is the bytes of the piece a get reads next, which its pull takes
	// of the window ([pieceBound]); 0 for a walk, whose pull takes the window.
	want int
	// alone says the next pull is of one message at the transport's bound,
	// let out of the window alone: the last ended past its bytes with none.
	alone bool
	// fresh says the consumer has delivered nothing and may still be made
	// again for a first pull it never had; lost that it went quiet and is
	// made again once hand is handed over ([reading.again]).
	fresh, lost bool
	// word is when the read last had a message, which its stall is counted
	// from, and seq the stream sequence of the last it handed over.
	word time.Time
	seq  uint64
}

// consume makes and reads a consumer over filter as cfg says, begin to make it
// where not zero, batch messages in hand on a member's own client, want the
// bytes of a get's first piece across a link, answering its first message, or
// no reading where the leader holds nothing on filter the read must reach.
func (b *Backend) consume(ctx context.Context, filter string, cfg jetstream.OrderedConsumerConfig,
	batch, want int, stall time.Duration, through uint64, begin time.Duration) (*reading, *message, error) {

	cfg.FilterSubjects = []string{filter}
	r := &reading{b: b, filter: filter, leader: b.leaderFor(filter), cfg: cfg, batch: batch,
		stall: stall, through: through, begin: begin, want: want, word: time.Now()}
	msg, err := r.open(ctx)
	if err != nil || msg == nil {
		return nil, nil, err
	}
	return r, msg, nil
}

// open makes r's consumer and answers its first message, or none where the
// stream's leader holds nothing on r's filter that r must reach.
//
// THE FIRST MESSAGE IS THE ONE A CLUSTER CAN DROP THE PULL FOR ([quietWait],
// [reading.pulled]): a consumer that never got the pull is made again up to
// [jsapi.FirstPullRemakes] times, each with a whole stall of its own.
// A CONSUMER WITH NOTHING TO DELIVER IS ASKED ABOUT TOO ([reading.caughtUp]):
// where the leader holds what it should have delivered, the consumer was on a
// replica behind and is made again, at once the first time, then a
// [quietWait] apart, until the read has been behind for [objstore.Stall] and
// fails ([errBehind]).
func (r *reading) open(ctx context.Context) (*message, error) {
	var behind time.Time
	for remade := 0; ; {
		cons, err := r.create(ctx)
		if err != nil {
			return nil, err
		}
		if info := cons.CachedInfo(); info == nil || info.NumPending == 0 {
			r.b.drop(ctx, cons)
			done, cerr := r.caughtUp(ctx, r.after())
			if cerr != nil || done {
				return nil, cerr
			}
			if lerr := lagging(ctx, &behind); lerr != nil {
				return nil, lerr
			}
			continue
		}
		if r.b.across {
			msg, perr := r.pulled(ctx, cons, remade < jsapi.FirstPullRemakes)
			switch {
			case errors.Is(perr, errUnpulled):
				r.stop(ctx)
				remade++
				continue
			case perr != nil:
				r.stop(ctx)
				return nil, perr
			}
			return msg, nil
		}
		msgs, err := cons.Messages(pulls(r.batch)...)
		if err != nil {
			r.b.drop(ctx, cons)
			return nil, err
		}
		r.cons, r.msgs = cons, msgs
		wait, cancel := r.waiting(ctx)
		msg, err := r.quietly(ctx, wait)
		if errors.Is(err, errQuiet) {
			if remade < jsapi.FirstPullRemakes && r.unpulled(wait) {
				cancel()
				r.stop(ctx)
				remade++
				continue
			}
			msg, err = r.from(ctx, wait, wait)
		}
		cancel()
		if err != nil {
			r.stop(ctx)
			return nil, err
		}
		return msg, nil
	}
}

// create asks the broker for r's consumer, each ask for no longer than r.begin
// where r has one: under a context with no deadline the client holds the
// request to its own five seconds, which a large bucket's listing outgrows. A
// metadata group with no leader is waited out ([leaderReads]).
func (r *reading) create(ctx context.Context) (jetstream.Consumer, error) {
	return jsprovision.ThroughElection(ctx, Stream, r.createOnce)
}

// createOnce is one of [reading.create]'s asks.
func (r *reading) createOnce(ctx context.Context) (jetstream.Consumer, error) {
	if r.begin <= 0 {
		return r.made(ctx)
	}
	begin, cancel := context.WithTimeout(ctx, r.begin)
	defer cancel()
	cons, err := r.made(begin)
	if err != nil && ctx.Err() == nil && begin.Err() != nil {
		return nil, fmt.Errorf("the broker made no consumer for it in the %v it was given: %w",
			r.begin, err)
	}
	return cons, err
}

// made makes r's consumer: on a member's own client the ordered consumer the
// client reads with its own pulls; across a link a plain one made alike — no
// acknowledgements, in memory, one replica, reaped after five idle minutes as
// the client's ordered consumer is — whose pulls the reader asks itself, each
// taken from the link's window ([reading.fill]), since the client's iterator
// pulls on a threshold of its own that no window can admit, and its fetch makes
// the consumer again at every pull.
func (r *reading) made(ctx context.Context) (jetstream.Consumer, error) {
	if !r.b.across {
		return r.b.stream.OrderedConsumer(ctx, r.cfg)
	}
	cfg := jetstream.ConsumerConfig{DeliverPolicy: r.cfg.DeliverPolicy, OptStartSeq: r.cfg.OptStartSeq,
		FilterSubjects: r.cfg.FilterSubjects, HeadersOnly: r.cfg.HeadersOnly,
		AckPolicy: jetstream.AckNonePolicy, InactiveThreshold: consumerReap, Replicas: 1, MemoryStorage: true}
	return r.b.stream.CreateConsumer(ctx, cfg)
}

// consumerReap is how long the broker keeps a read's consumer across a link
// once nothing pulls it, should the reader die before it deletes it: five
// minutes, the client's own for the ordered consumer a member reads with.
const consumerReap = 5 * time.Minute

// lagging is the pause before a consumer is made again after one was made on
// a replica behind the stream's leader, since behind: none the first time,
// [quietWait] after that, and [errBehind] once the read has been behind for
// [objstore.Stall].
func lagging(ctx context.Context, behind *time.Time) error {
	if behind.IsZero() {
		*behind = time.Now()
		return nil
	}
	if time.Since(*behind) >= objstore.Stall {
		return fmt.Errorf("%w for %v", errBehind, objstore.Stall)
	}
	pause := time.NewTimer(quietWait)
	defer pause.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-pause.C:
		return nil
	}
}

// after is the sequence r's consumer begins past: the one before its start
// sequence, or none.
func (r *reading) after() uint64 {
	if r.cfg.DeliverPolicy == jetstream.DeliverByStartSequencePolicy && r.cfg.OptStartSeq > 0 {
		return r.cfg.OptStartSeq - 1
	}
	return 0
}

// caughtUp reports whether the stream's LEADER holds nothing on r's filter
// that r must reach past the sequence after — so that a consumer with nothing
// past after has come to the end of what r reads, rather than to the end of
// what a replica behind the leader holds. Asked only where a consumer says
// nothing follows and the read cannot tell that from what it read, so a read
// whose replica is current asks nothing.
//
// ACROSS A LINK, A PIECE'S ANSWER IS TAKEN FROM THE LINK'S WINDOW: the leader
// answers with the whole message, 175,788 bytes of reply for a piece, which
// crosses the floor in five seconds, more than the window. So it is let out
// alone ([jsapi.Window]), once the window is empty, and nothing else crosses
// beside it; it runs only on a replica behind the leader.
func (r *reading) caughtUp(ctx context.Context, after uint64) (bool, error) {
	if after >= r.through {
		return true, nil
	}
	if r.b.across && r.leader.l == r.b.pieces {
		reply := jsapi.ReplyBytes(MessageBytes)
		if err := r.b.window.Take(ctx, reply); err != nil {
			return false, err
		}
		defer r.b.window.Give(reply)
	}
	msg, err := r.leader.Next(ctx, r.filter, after+1)
	switch {
	case errors.Is(err, jsapi.ErrNoMessage):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("ask the stream's leader what follows %d on %s: %w", after, r.filter, err)
	}
	return msg.Sequence > r.through, nil
}

// resume makes r's consumer again, reading from past the sequence after: the
// end of what its replica held, where the leader holds more. It answers no
// message where the leader holds nothing past after by now — what followed
// was purged in between.
//
// A READ OF EVERY MESSAGE ONLY — a get's or a seek's. A listing's consumer
// delivers the last message on each subject, and made again from a sequence
// it would deliver every message from there, superseded ones included, so a
// listing behind the leader fails instead ([Backend.eachMeta]).
func (r *reading) resume(ctx context.Context, after uint64) (*message, error) {
	r.stop(ctx)
	r.cfg.DeliverPolicy, r.cfg.OptStartSeq = jetstream.DeliverByStartSequencePolicy, after+1
	return r.open(ctx)
}

// next is r's next message: waited for [quietWait], then once more for the
// rest of r's stall, so the client decides again whether to send a pull; and
// across a link, the next its own pulls hand over ([reading.handed]).
func (r *reading) next(ctx context.Context) (*message, error) {
	if r.b.across {
		return r.handed(ctx)
	}
	wait, cancel := r.waiting(ctx)
	defer cancel()
	msg, err := r.quietly(ctx, wait)
	if errors.Is(err, errQuiet) {
		msg, err = r.from(ctx, wait, wait)
	}
	return msg, err
}

// quietly is r's next message, waited for no longer than [quietWait].
func (r *reading) quietly(ctx, wait context.Context) (*message, error) {
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
func (r *reading) from(ctx, wait, until context.Context) (*message, error) {
	msg, err := r.msgs.Next(jetstream.NextContext(until))
	switch {
	case err == nil:
		return fromConsumer(msg)
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
// to reap minutes later — once: a read made again stops the consumer it
// replaces, and has none at all where nothing followed.
func (r *reading) stop(ctx context.Context) {
	if r.msgs != nil {
		r.msgs.Stop()
	}
	if r.pulls != nil {
		r.pulls.Stop()
	}
	if r.cons != nil {
		r.b.drop(ctx, r.cons)
	}
	r.cons, r.msgs, r.pulls, r.hand = nil, nil, nil, nil
}

// Get implements [objstore.Backend].
//
// AN ORDERED CONSUMER OF THE OBJECT'S OWN MESSAGES, never the library's get
// (package doc). Each is waited for under [objstore.Stall] and ctx; the FIRST
// here, so a consumer a cluster dropped the first pull for is made again
// ([quietWait]). A RANGE starts at the message holding its offset, found by walking message
// HEADERS ([Backend.seek]): the library has no ranged read and pieces cannot
// be addressed by place, so an offset costs a header per message before it.
// ITS PIECES END WHERE THE LEADER'S DO: a message its consumer calls the last
// that leaves the object short of its metadata's size is asked about
// ([reading]) and the read goes on from a consumer made again. A healthy read
// asks nothing.
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
	r, first, err := b.consume(ctx, subject, cfg, readAhead, nextPiece(size, off-skip), b.stall,
		everything, 0)
	switch {
	case err != nil:
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	case r == nil:
		return b.gone(ctx, name)
	}
	o := &object{ctx: ctx, name: name, r: r, skip: skip, left: n, at: off - skip, size: size}
	if err := o.hold(first); err != nil {
		r.stop(ctx)
		return nil, fmt.Errorf("natsobj: get %s: %w", name, err)
	}
	return o, nil
}

// nextPiece is the bytes of an object of size's piece that begins at at: a
// whole [MessageBytes], or the object's last piece, and never none, since a
// read asks a piece only where one follows. Across a link a get's pull of it
// takes that of the link's window ([pieceBound]); a piece some other tool cut
// larger ends that pull past its bytes and is pulled again alone.
func nextPiece(size, at int64) int { return int(max(min(MessageBytes, size-at), 1)) }

// gone answers a get whose object's pieces the stream's LEADER does not hold:
// an object that ends before it begins — which the store reads as one holding
// fewer bytes than its row says — unless the object itself went between its
// metadata's read and the pieces', which a second look tells apart.
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
// how the pieces were cut. A message its consumer calls the last, short of
// off, ends the walk only where the stream's leader holds nothing after it
// either ([reading]); otherwise the walk goes on from a consumer made again.
func (b *Backend) seek(ctx context.Context, subject string, off int64) (uint64, int64, bool, error) {
	// The leader's answer to what follows is the WHOLE message: a message
	// read has no headers-only form, so asking it costs a piece, which
	// across a link is let out of the window alone ([reading.caughtUp]).
	r, msg, err := b.consume(ctx, subject, jetstream.OrderedConsumerConfig{
		HeadersOnly:      true,
		MaxResetAttempts: listResets,
	}, walkAhead, 0, b.stall, everything, 0)
	if err != nil || r == nil {
		return 0, 0, false, err
	}
	defer r.stop(ctx)
	var at int64
	for {
		size, err := strconv.ParseInt(msg.header.Get(nats.MsgSize), 10, 64)
		if err != nil {
			return 0, 0, false, fmt.Errorf("message %d carries no size: %w", msg.seq, err)
		}
		if at+size > off {
			return msg.seq, off - at, true, nil
		}
		at += size
		switch seq := msg.seq; {
		case msg.pending > 0:
			msg, err = r.next(ctx)
		default:
			var done bool
			if done, err = r.caughtUp(ctx, seq); err != nil || done {
				return 0, 0, false, err
			}
			msg, err = r.resume(ctx, seq)
		}
		switch {
		case err != nil:
			return 0, 0, false, err
		case msg == nil:
			// NOTHING PAST IT AFTER ALL: what the leader held past the
			// replica's end was purged in between.
			return 0, 0, false, nil
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

	// at is where in the object the message after the current one begins,
	// size where the object ends by its metadata, and seq the current
	// message's stream sequence.
	at, size int64
	seq      uint64

	// held is the rest of the current message, and last whether it is the
	// object's final one. more says it is the last its consumer's replica
	// holds while the stream's leader holds more: the read goes on from a
	// consumer made again past it ([reading.resume]).
	held []byte
	last bool
	more bool

	closeOnce sync.Once
}

// hold makes msg the message o hands over next, past what of it comes
// before the range.
//
// A MESSAGE ITS CONSUMER CALLS THE LAST is the object's last where it ends
// the object — or the range ends inside it, so nothing after it is read — and
// otherwise only where the stream's leader holds nothing after it either: a
// consumer on a replica still catching up calls its last message the last
// whatever the leader holds ([reading]).
func (o *object) hold(msg *message) error {
	data := msg.data
	o.at += int64(len(data))
	o.seq = msg.seq
	o.r.want = nextPiece(o.size, o.at)
	data = data[min(o.skip, int64(len(data))):]
	o.skip = 0
	o.held, o.last, o.more = data, msg.pending == 0, false
	if !o.last || o.at >= o.size || (o.left >= 0 && o.left <= int64(len(data))) {
		return nil
	}
	end, err := o.r.caughtUp(o.ctx, o.seq)
	if err != nil {
		return err
	}
	o.last, o.more = end, !end
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
		var msg *message
		var err error
		if o.more {
			msg, err = o.r.resume(o.ctx, o.seq)
		} else {
			msg, err = o.r.next(o.ctx)
		}
		if err != nil {
			return 0, fmt.Errorf("natsobj: read %s: %w", o.name, err)
		}
		if msg == nil {
			// NOTHING PAST IT AFTER ALL: what the leader held past the
			// replica's end was purged in between, with the object.
			o.last = true
			return 0, io.EOF
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

// listBeginPace is what a listing's beginning is given a mebibyte of bucket:
// ten MiB a second, a quarter of the slowest beginning measured (580 MB in
// 13.6 s on a volume 98% full), since the broker first reads the blocks.
const listBeginPace = 100 * time.Millisecond

// listBegin is how long a listing of a bucket of bytes is given to begin: the
// client's own bound on one request, and [Backend.beginPace] for each
// mebibyte begun, which the broker reads before it makes the consumer.
func (b *Backend) listBegin(bytes uint64) time.Duration {
	return b.js.Options().DefaultTimeout +
		time.Duration((bytes+objstore.MiB-1)/objstore.MiB)*b.beginPace
}

// List implements [objstore.Backend].
//
// A CONSUMER OF ITS OWN OVER THE METADATA SUBJECTS, not the library's Watch,
// which knows the listing is complete only from a message it could DECODE and
// holds its caller for ever on unreadable newest metadata; here every message
// carries how many follow it. Streamed, as the library's List holds the whole
// inventory in one slice. A LISTING REACHES WHAT THE LEADER HELD WHEN IT BEGAN,
// or fails: a consumer on a replica still catching up lists only the names it
// stored ([reading]), and a skipped name is an object the caller takes for
// gone ([Backend.Pending] would hand the collector a finished upload's pieces
// to abandon). It cannot go on from a consumer made again as a get does: one
// made from a sequence delivers superseded messages too.
func (b *Backend) List(ctx context.Context, visit func(objstore.Info) error) error {
	return b.eachMeta(ctx, func(info *jetstream.ObjectInfo) error {
		return visit(infoOf(info))
	})
}

// eachMeta hands visit the newest metadata on every name's subject, its
// ModTime the instant the broker stored it. Every name is visited as it is: a
// delete marker another client of the bucket's format left is an object of no
// bytes, which the collector clears with the usual delete ([Backend.Delete]),
// and metadata that does not decode ([objstore.ErrCorrupt] to a stat or get)
// is one of no bytes named by its subject; left out, nothing would ever judge
// or clear it. Only a subject spelling no name is logged and stepped over. See
// [Backend.List] for why it is a consumer of its own, and [Backend.listBegin]
// for what its beginning is given: the broker reads every name's newest block
// first, beyond the client's five seconds.
func (b *Backend) eachMeta(ctx context.Context, visit func(*jetstream.ObjectInfo) error) error {
	var through uint64
	switch newest, err := b.leaderFor(metaSubjects).Last(ctx, metaSubjects); {
	case errors.Is(err, jsapi.ErrNoMessage):
	case err != nil:
		return fmt.Errorf("natsobj: list the bucket: read its newest metadata from the stream's leader: %w", err)
	default:
		through = newest.Sequence
	}
	info, err := b.info(ctx)
	if err != nil {
		return fmt.Errorf("natsobj: list the bucket: read its size from the stream's leader: %w", err)
	}
	// NO STALL: a listing is waited on for as long as its caller allows,
	// since a visitor that is slow holds the next name back on purpose.
	r, msg, err := b.consume(ctx, metaSubjects, jetstream.OrderedConsumerConfig{
		DeliverPolicy:    jetstream.DeliverLastPerSubjectPolicy,
		MaxResetAttempts: listResets,
	}, walkAhead, 0, 0, through, b.listBegin(info.State.Bytes))
	if err != nil {
		return fmt.Errorf("natsobj: list the bucket: %w", err)
	}
	if r == nil {
		return nil
	}
	defer r.stop(ctx)
	for {
		var info jetstream.ObjectInfo
		named := true
		if jerr := json.Unmarshal(msg.data, &info); jerr != nil {
			// NO OBJECT, AND STILL A NAME: what the subject spells, held
			// with no bytes and no pieces — see [Backend.List].
			name, ok := nameOfMeta(msg.subject)
			if ok {
				info = jetstream.ObjectInfo{ObjectMeta: jetstream.ObjectMeta{Name: name}}
			} else {
				log.WarnContext(ctx, "object_metadata_unnamed", "subject", msg.subject,
					"error", jerr, "detail", "metadata on a subject that spells no name; "+
						"nothing can judge or clear it, so it is left where it is")
				named = false
			}
		}
		// STEPPED OVER, NEVER RETURNED FROM: the walk still advances below,
		// since the next message is read at the bottom of the loop and a
		// `continue` here would read this one again for ever.
		if named {
			info.ModTime = msg.stamp
			if verr := visit(&info); verr != nil {
				return verr
			}
		}
		if msg.pending == 0 {
			done, cerr := r.caughtUp(ctx, msg.seq)
			switch {
			case cerr != nil:
				return fmt.Errorf("natsobj: list the bucket: %w", cerr)
			case !done:
				return fmt.Errorf("natsobj: list the bucket: %w: it ended at sequence %d, short "+
					"of metadata the leader held when it began; list again", errBehind,
					msg.seq)
			}
			return nil
		}
		if msg, err = r.next(ctx); err != nil {
			return fmt.Errorf("natsobj: list the bucket: %w", err)
		}
	}
}

// pieceSubjects is every subject the bucket keeps an object's pieces on:
// `$O.<bucket>.C.<nuid>`, one per object.
//
// [Backend.Pending] lists it a page at a time, each page asked again while
// nobody answers ([jsapi.Leader.Subjects]), since only the leader answers.
// Each page costs the leader a sort of every piece subject, so a listing asked
// again whole would repeat that for every page before the slow one. A page is
// 0.13 s at a hundred thousand objects, 1.26 s at a million, 4.22 s at three
// million (BenchmarkASubjectListing, four cores), held to the client's five
// seconds: outgrown at about three and a half million objects, past which
// every sweep fails. The S3 backend lists unfinished uploads at any size
// (`crewlet validate` warns of the nats bucket on a fleet).
const pieceSubjects = "$O." + Bucket + ".C.>"

// Pending implements [objstore.Backend]: every subject holding pieces that no
// object's metadata names, dated by its oldest piece. Pieces of a put that died
// before its completing metadata, or of a delete interrupted between its two
// purges ([Backend.Delete]), are on a subject no name reaches; each is
// [objstore.Pending] with its piece identifier as ID.
//
// IN THIS ORDER (piece subjects, metadata, each orphan's oldest piece) so a
// put completing during the walk is never answered: its metadata is written
// and read after its pieces. A put still running is answered; the collector
// abandons only what is older than a put may take. Every read is the
// LEADER's ([jsapi.Leader]): a replica behind would answer a subject as empty;
// one it then holds nothing on was finished or purged, and is stepped over.
func (b *Backend) Pending(ctx context.Context, visit func(objstore.Pending) error) error {
	// WHY NOT A WALK OF THE PIECES, headers only as the metadata walk does:
	// it is linear in the objects, but the broker loads the file-store block
	// holding each subject's last message, so it costs the bucket's BYTES (a
	// million real objects is a hundred gigabytes read per sweep), where the
	// listing reads only the in-memory per-subject index. Measured on two
	// thousand 128 KiB objects: 103 ms against the listing's 2.2. Starting it
	// also reads the state of every block not in memory while holding the
	// stream's store lock (filestore.go, multiLastSeqsLocked), stalling every
	// append on that member behind a read of the bucket.

	// THE CANDIDATES ARE THE SUBJECT LISTING ITSELF, whittled down by the
	// metadata walk, so the pass holds no more than the broker's answers
	// already did, never a second copy of the bucket's inventory.
	orphans := make(map[string]uint64)
	err := leaderReads{b.leader}.Subjects(ctx, pieceSubjects, func(page map[string]uint64) error {
		maps.Copy(orphans, page)
		return nil
	})
	if err != nil {
		return fmt.Errorf("natsobj: list the bucket's pieces: %w", err)
	}
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
		first, err := b.leaderFor(subject).First(ctx, subject)
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
