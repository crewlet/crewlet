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
// RESERVATION of its own, granted once by the broker at create time, and how
// close it is to full is what [Backend.Usage] answers and an alarm reads.
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
// retry and its messages out held to a WINDOW the connection's puts share
// ([window]: [linkWindow] across a leaf's link or to an external cluster,
// sized from the node's own lease heartbeat so a lease renewal queued behind
// it at the floor still lands inside a heartbeat; [memberWindow] on a
// member's own client); it is ended only when
// the broker has acknowledged nothing for [objstore.Stall] while messages are
// out ([objstore.ErrStalled]); and a put that fails, the caller giving up among
// the ways, drains what is out and purges what it stored under a context that
// does not end with the caller's ([objstore.ContextReader]) — the metadata
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
//   - A GET IS AN ORDERED CONSUMER OF THE OBJECT'S OWN MESSAGES, read one
//     message at a time under [objstore.Stall] and ended by its
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
//   - THE PUT FIGURES ABOVE ARE THE LIBRARY'S PUT, which [Backend.Put]
//     replaced. The two measured against each other, interleaved, on one
//     machine (four vCPUs of an Intel Xeon at 2.1 GHz, 2026-10-18, at
//     `stream.sync: always`): on a member's own client 64 KiB took
//     1.64–1.68 ms against 1.58–1.65, 1 MiB ran at 116–140 MB/s against
//     109–137 and 16 MiB at 146–167 against 161–174; through a leaf 64 KiB
//     took 2.43–2.61 ms against 2.26–2.73 and 16 MiB ran at 123–137 MB/s
//     against 120–144; at three replicas 64 KiB took 4.6–5.5 ms against
//     3.5–4.3 — the quorum round trip the metadata waits for its pieces'
//     acknowledgements in ([Backend.Put]) — and 16 MiB ran at 55–66 MB/s
//     against 72–76.
//   - THE WINDOW SHARED BY A CONNECTION'S PUTS ([window]) against one a put,
//     interleaved runs of each on one machine (2026-10-18, `stream.sync:
//     always`, the machine running other work): on a member's own client,
//     sixteen either way, a lone 16 MiB put ran at 147–167 MB/s against
//     132–171 and eight at once at 159–177 MB/s together against 164–183;
//     through a leaf, four shared against sixteen a put, a lone 16 MiB put
//     at 122–134 MB/s against 129–153, eight at once at 119–140 against
//     158, and 64 KiB in 2.2–2.7 ms against 2.0–2.7; at three replicas,
//     sixteen either way, a lone 16 MiB put at 59–63 MB/s against 62–63,
//     eight at once at 56–58 against 47–63 together, and 64 KiB in 5.8–6.0
//     ms against 5.8–6.2 — and sixty-four shared, beside sixteen in a run of
//     its own, 48–69 MB/s alone against 59–68 and 42–65 eight at once
//     against 50–62: nothing a member measured bought by a wider one.
//   - THE LINK'S WINDOW ON A SHORTER LEASE ([linkWindow] follows the
//     lease's heartbeat), through a leaf, interleaved runs of each on one
//     machine (2026-10-18, `stream.sync: always`, four vCPUs running other
//     work; four rounds, a fifth that overlapped another run left out): one
//     message out — a lease under 22.5 s — ran a lone 16 MiB put at
//     47–83 MB/s and eight at once at 56–78 together; two, at 109–125 and
//     102–122; four, the shipped lease's, at 120–139 and 130–135. A 64 KiB
//     put, one message whatever the window, took 2.3–4.0 ms in all three.
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
	// AcrossLink says the connection reaches the stream across a LINK the
	// node's leases and events ride too — a leaf's link to its members, or
	// a dial to an external cluster — rather than to the node's own member
	// in its own process: it decides the puts' [window] ([linkWindow],
	// [memberWindow]).
	AcrossLink bool
	// LeaseHeartbeat is how often the node renews the leases its connection
	// carries — the seat and presence leases' heartbeat, the lease TTL IN
	// FORCE over seat.HeartbeatRatio, never the shipped one — which sizes
	// the window across a link ([linkWindow]). Required with AcrossLink,
	// and refused as zero there rather than read as some default, because
	// a window sized for a lease the node does not hold is the one that
	// loses its seats; read nowhere else.
	LeaseHeartbeat time.Duration

	// MaxBytes is the byte reservation a bucket this node creates is given
	// (store.objects.nats_max_bytes, or the share the engine derives),
	// required, since a bucket with no ceiling is the failure the package
	// doc describes. A bucket that exists keeps the ceiling it holds: its
	// configuration has one writer, and a booting node is not it. The one
	// exception is a bucket holding NO ceiling, which takes an explicit
	// figure at boot (see [checkCeiling]).
	MaxBytes int64

	// Explicit says MaxBytes is what an operator wrote rather than what the
	// engine derived, which is when a bucket held at another ceiling is
	// worth a line: a derived figure follows the broker's free space and
	// differs from the one a bucket was created at on most boots.
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
	// acknowledge any of its messages out — a field so a test can watch a
	// stall end a put without waiting a minute for it.
	stall time.Duration
	// window is the places every put on the connection shares for the
	// messages it holds out ([window]).
	window *window
}

// Window is how many messages the puts on b's connection may hold out at
// once: [linkWindow] for [Config.LeaseHeartbeat] where [Config.AcrossLink]
// said the connection crosses a link to the stream, [memberWindow] where it
// did not. Whoever opens the bucket decides which, and the engine's choice —
// by the node's broker kind, at the heartbeat of the lease in force — is held
// by a test of its own that reads it here.
func (b *Backend) Window() int { return b.window.size }

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
// term while nobody answers ([jsprovision.ReadWith]) — and creates it only
// when told it is absent or when nobody answered, through
// [jsprovision.Timing.Place] as every replicated create at boot is. A create
// that is not refused outright may have landed, or found the name taken by a
// peer that won the race, so the bucket is then read back
// ([jsprovision.SettleWith]) rather than written again. Nothing here applies
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
	if cfg.MaxBytes <= 0 {
		return nil, fmt.Errorf("natsobj: the %s bucket needs a byte reservation, and "+
			"this node was given %d: a bucket with no ceiling grows into the broker "+
			"remainder the fleet's coordination records live in", Bucket, cfg.MaxBytes)
	}
	if cfg.AcrossLink && cfg.LeaseHeartbeat <= 0 {
		return nil, fmt.Errorf("natsobj: a bucket reached across a link needs the "+
			"node's lease heartbeat (Config.LeaseHeartbeat), which sizes the window "+
			"its uploads share; got %v", cfg.LeaseHeartbeat)
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
	// A PIECE'S REPLY IS GIVEN ITS CROSSING ONLY WHERE THERE IS A LINK TO
	// CROSS: a member's reads reach the leader over a cluster route, which
	// carries a piece in milliseconds, and a term sized for a leaf link at
	// the floor would hold a member's dropped piece read seven seconds
	// before it is asked again, where the read term asks it at two.
	size, pieces := memberWindow, leader
	if cfg.AcrossLink {
		size, pieces = linkWindow(cfg.LeaseHeartbeat), leader.Carrying(MessageBytes, objstore.MiBPace)
	}
	return &Backend{js: js, stream: stream, leader: leader, pieces: pieces,
		stall: objstore.Stall, window: newWindow(size)}, nil
}

// checkCeiling judges the ceiling the bucket holds against the one this node
// was configured with.
//
// # No ceiling is refused unless an operator wrote one, and a different one is kept
//
// A bucket holding NO ceiling is refused, for the package doc's reason, when
// the node's figure is one the engine derived: a derived share follows the
// broker's free space, so writing it would let whichever node booted last
// size the bucket. Every bucket a build before the reservation created holds
// none, as does one made by hand or by another tool. The refusal names both
// gestures that bound it: store.objects.nats_max_bytes, which [Open] applies
// to such a bucket at boot ([boundBucket]) and which is the only one an
// embedded broker that opens no client listener can take — a solo member's,
// or one serving only leaves — since nothing outside the engine reaches it;
// and the broker's own `nats stream update`, since a bucket IS a stream, run
// against an external cluster or through a clustered member's loopback client
// listener with the operator key `crewlet fleet broker credential` writes.
//
// A bucket holding a DIFFERENT ceiling is kept as it is, as a state log's
// stream is: the broker granted it once, and the capacity it reserves is the
// one an operator sized for. Said out loud where the operator wrote the other
// figure, since an edit to store.objects.nats_max_bytes reaches only a bucket
// that does not exist yet.
func checkCeiling(ctx context.Context, held int64, cfg Config) error {
	switch {
	case held <= 0:
		return fmt.Errorf("natsobj: the %s bucket holds no byte ceiling, so its "+
			"files grow into the broker remainder the fleet's leases and ledgers "+
			"live in, and a full remainder stops the fleet rather than an upload. "+
			"Give it one before this node starts: set store.objects.nats_max_bytes, "+
			"which a node applies at boot to a bucket holding no ceiling (%d is "+
			"the share this node derived), or run `nats stream update %s "+
			"--max-bytes=%d` against the broker", Bucket, cfg.MaxBytes, Stream, cfg.MaxBytes)
	case cfg.Explicit && held != cfg.MaxBytes:
		log.WarnContext(ctx, "natsobj_bucket_ceiling_kept", "bucket", Bucket,
			"holds", held, "configured", cfg.MaxBytes,
			"detail", "store.objects.nats_max_bytes sizes a bucket this node "+
				"creates, and this one exists: it keeps the ceiling the broker "+
				"granted it. Change a bucket that exists with `nats stream update "+
				Stream+" --max-bytes=<bytes>`")
	}
	return nil
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
// ([objstore.Full.Remedy]): three gestures, each an operator's.
//
// THE RESERVATION IS RAISED ON AN EXTERNAL BROKER ONLY, and the sentence says
// so: a bucket that exists keeps the ceiling it was created with
// ([checkCeiling]), and the `nats stream update` that changes it cannot reach
// an embedded broker, which has no client listener — so moving the files to a
// provider's bucket comes first, as the one gesture every deployment has.
const FullRemedy = "an operator makes room — moves the files to a bucket " +
	"(crewlet objects migrate -to s3), raises the bucket's byte reservation on an " +
	"external broker, or lets the collector remove what nothing names"

// Usage answers how much of the bucket's reservation its objects take, from
// the stream's own state and configuration: the ceiling the BROKER enforces,
// never the figure this node was configured with, since a bucket that exists
// keeps the ceiling it was created at.
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

// info is the bucket's stream info as the stream's LEADER answered it — its
// state and its configuration — asked as a metadata read is
// ([jsprovision.ReadBounded]).
//
// ASKED AGAIN WHILE NOBODY ANSWERS, because nobody but the leader does: a
// member that still believes in a leader its group has just lost takes the
// request and returns without a word (server/jetstream_api.go,
// jsStreamInfoRequest: "only the stream leader should answer"). Sent as one
// request it waited for its caller's deadline, or for the client's five
// seconds where there was none — the object gauge's reading on every turn of
// the collector's duty, which has none — for a reply nobody would send. A
// caller with no deadline keeps those five seconds, asked again inside them.
// THROUGH A HANDLE OF EACH REQUEST'S OWN, never [Backend]'s stream: the
// client caches an info reply on the handle that fetched it with no lock, the
// hedge can have two requests out, and the bucket's handle is the one every
// get and listing reads beside it.
func (b *Backend) info(ctx context.Context) (*jetstream.StreamInfo, error) {
	return jsprovision.ReadBounded(ctx, b.js.Options().DefaultTimeout,
		func(ctx context.Context) (*jetstream.StreamInfo, error) {
			stream, err := b.js.Stream(ctx, Stream)
			if err != nil {
				return nil, err
			}
			return stream.CachedInfo(), nil
		}, nil)
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
func (b *Backend) leaderFor(filter string) *jsapi.Leader {
	if strings.HasPrefix(filter, metaSubject("")) {
		return b.leader
	}
	return b.pieces
}

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
// consumer's first hand time to arrive inside [objstore.Stall]'s minute.
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
// nothing at the floor — the room [objstore.Stall] gives a mebibyte — and
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

// errBehind is a read the broker kept placing on a replica behind the
// stream's leader for as long as a read may wait for a message.
var errBehind = errors.New("natsobj: every consumer the broker made for the read was on " +
	"a replica behind the stream's leader")

// everything is the [reading.through] of a read every message of which the
// leader holds on its filter is its own: an object's pieces, every one of
// them stored before the metadata a get read from the leader.
const everything = math.MaxUint64

// reading is one ordered consumer being read — a get's, a seek's or a
// listing's — each message waited for no longer than stall, or for as long as
// the caller's context allows where stall is zero.
//
// # THE END OF A REPLICA'S COPY IS NOT THE END OF THE STREAM
//
// The broker makes an ordered consumer on ONE of the stream's replicas, chosen
// at random among those it counts as up — and a member just back from a
// restart, still catching up, is up. A consumer there counts what follows each
// message from that replica's copy, so its "nothing follows" is the end of
// what the replica has stored, and the messages it is still catching up are
// not delivered to a consumer made before they landed until somebody pulls
// again. Taken for the end of the stream, that answered an object a replica
// held none of as one with no pieces — read back as zero bytes, which the
// store calls DAMAGED, a definite answer — a ranged read past what it held as
// a range past the object's end, and a listing as one missing every object
// written while the member was down. So a consumer's end is taken as the end
// only where it is the end the read wanted (an object's last byte) or where
// the stream's LEADER agrees ([reading.caughtUp]); otherwise the read goes on
// from a consumer made again ([reading.resume]), which the broker places
// afresh.
type reading struct {
	b *Backend
	// filter is the subject the read is over, and cfg how its consumer is
	// made — the filter included — so a consumer made again is made alike.
	filter string
	// leader asks the stream's leader what follows on filter
	// ([Backend.leaderFor]).
	leader *jsapi.Leader
	cfg    jetstream.OrderedConsumerConfig
	batch  int
	stall  time.Duration
	// through is the newest sequence the read must reach on its filter:
	// what the leader held there when a listing began — a message stored
	// after it is no message the listing owes anybody — or [everything].
	through uint64

	cons jetstream.Consumer
	msgs jetstream.MessagesContext
}

// consume makes an ordered consumer over filter as cfg says and reads it,
// batch messages in hand, answering it with its first message — or no reading
// at all where the stream's leader holds nothing on filter that the read must
// reach (see [reading.open]).
func (b *Backend) consume(ctx context.Context, filter string, cfg jetstream.OrderedConsumerConfig,
	batch int, stall time.Duration, through uint64) (*reading, jetstream.Msg, error) {

	cfg.FilterSubjects = []string{filter}
	r := &reading{b: b, filter: filter, leader: b.leaderFor(filter), cfg: cfg, batch: batch,
		stall: stall, through: through}
	msg, err := r.open(ctx)
	if err != nil || msg == nil {
		return nil, nil, err
	}
	return r, msg, nil
}

// open makes r's consumer and answers its first message, or none where the
// stream's leader holds nothing on r's filter that r must reach.
//
// THE FIRST MESSAGE IS THE ONE A CLUSTER CAN DROP THE PULL FOR ([quietWait]):
// one that has not come in a second is asked about, and a consumer that never
// got the pull is dropped and made again, up to [firstPullRemakes] times —
// each with a whole stall of its own, since nothing the read made had begun.
//
// A CONSUMER WITH NOTHING TO DELIVER IS ASKED ABOUT TOO ([reading.caughtUp]):
// where the leader holds what it should have delivered, it was made on a
// replica behind the leader, and is dropped and made again — at once the
// first time, since the broker places a new consumer afresh and one replica
// behind is one of three, then a [quietWait] apart, while the replica catches
// up — until the read has been behind for [objstore.Stall], the longest it
// may wait for any one message, when it fails ([errBehind]).
func (r *reading) open(ctx context.Context) (jetstream.Msg, error) {
	var behind time.Time
	for remade := 0; ; {
		cons, err := r.b.stream.OrderedConsumer(ctx, r.cfg)
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
		msgs, err := cons.Messages(pulls(r.batch)...)
		if err != nil {
			r.b.drop(ctx, cons)
			return nil, err
		}
		r.cons, r.msgs = cons, msgs
		wait, cancel := r.waiting(ctx)
		msg, err := r.quietly(ctx, wait)
		if errors.Is(err, errQuiet) {
			if remade < firstPullRemakes && r.unpulled(wait) {
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
func (r *reading) caughtUp(ctx context.Context, after uint64) (bool, error) {
	if after >= r.through {
		return true, nil
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
func (r *reading) resume(ctx context.Context, after uint64) (jetstream.Msg, error) {
	r.stop(ctx)
	r.cfg.DeliverPolicy, r.cfg.OptStartSeq = jetstream.DeliverByStartSequencePolicy, after+1
	return r.open(ctx)
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
// to reap minutes later — once: a read made again stops the consumer it
// replaces, and has none at all where nothing followed.
func (r *reading) stop(ctx context.Context) {
	if r.msgs != nil {
		r.msgs.Stop()
	}
	if r.cons != nil {
		r.b.drop(ctx, r.cons)
	}
	r.cons, r.msgs = nil, nil
}

// Get implements [objstore.Backend].
//
// AN ORDERED CONSUMER OF THE OBJECT'S OWN MESSAGES, never the library's get —
// see the package doc. Each message is waited for under
// [objstore.Stall] and under ctx, so a read whose pieces have stopped
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
//
// ITS PIECES END WHERE THE LEADER'S DO: a consumer made on a replica still
// catching up says nothing follows where the leader holds more ([reading]),
// so a message its consumer calls the last that leaves the object short of
// the size its metadata states is asked about, and the read goes on from a
// consumer made again where the leader holds more. A healthy read asks
// nothing: its last message ends the object exactly.
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
	r, first, err := b.consume(ctx, subject, cfg, readAhead, objstore.Stall, everything)
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
	// read has no headers-only form, so asking it costs a piece.
	r, msg, err := b.consume(ctx, subject, jetstream.OrderedConsumerConfig{
		HeadersOnly:      true,
		MaxResetAttempts: listResets,
	}, walkAhead, objstore.Stall, everything)
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
		switch {
		case meta.NumPending > 0:
			msg, err = r.next(ctx)
		default:
			var done bool
			if done, err = r.caughtUp(ctx, meta.Sequence.Stream); err != nil || done {
				return 0, 0, false, err
			}
			msg, err = r.resume(ctx, meta.Sequence.Stream)
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
func (o *object) hold(msg jetstream.Msg) error {
	meta, err := msg.Metadata()
	if err != nil {
		return err
	}
	data := msg.Data()
	o.at += int64(len(data))
	o.seq = meta.Sequence.Stream
	data = data[min(o.skip, int64(len(data))):]
	o.skip = 0
	o.held, o.last, o.more = data, meta.NumPending == 0, false
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
		var msg jetstream.Msg
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
// A LISTING REACHES WHAT THE STREAM'S LEADER HELD WHEN IT BEGAN, or fails:
// its consumer is made on whichever replica the broker places it, and one
// still catching up lists the names it has stored ([reading]). So the
// listing first reads the leader's newest metadata, and a walk whose
// consumer says nothing follows short of it ends only where the leader holds
// nothing it missed — read from a replica behind, it fails, to be asked
// again, because a name it skipped is an object the caller takes for gone:
// [Backend.Pending] would hand the collector a finished upload's pieces to
// abandon. It cannot go on from a consumer made again, as a get does: a
// listing's consumer delivers each name's newest metadata, and one made from
// a sequence delivers every message after it, superseded ones among them.
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
	var through uint64
	switch newest, err := b.leaderFor(metaSubjects).Last(ctx, metaSubjects); {
	case errors.Is(err, jsapi.ErrNoMessage):
	case err != nil:
		return fmt.Errorf("natsobj: list the bucket: read its newest metadata from the stream's leader: %w", err)
	default:
		through = newest.Sequence
	}
	// NO STALL: a listing is waited on for as long as its caller allows,
	// since a visitor that is slow holds the next name back on purpose.
	r, msg, err := b.consume(ctx, metaSubjects, jetstream.OrderedConsumerConfig{
		DeliverPolicy:    jetstream.DeliverLastPerSubjectPolicy,
		MaxResetAttempts: listResets,
	}, walkAhead, 0, through)
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
			done, cerr := r.caughtUp(ctx, meta.Sequence.Stream)
			switch {
			case cerr != nil:
				return fmt.Errorf("natsobj: list the bucket: %w", cerr)
			case !done:
				return fmt.Errorf("natsobj: list the bucket: %w: it ended at sequence %d, short "+
					"of metadata the leader held when it began; list again", errBehind,
					meta.Sequence.Stream)
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
//
// THE SUBJECT LISTING IS ASKED A PAGE AT A TIME, each page asked again while
// nobody answers it ([jsapi.Leader.Subjects]): only the leader answers it, so
// a member that still believes in a leader its group has just lost drops it
// without a word, and one request held for the sweep's deadline failed the
// sweep over a reply nobody would send. A page holds a hundred thousand
// subjects — an object each — and every page costs the leader a sort of
// every piece subject the bucket holds, so a listing asked again whole would
// have asked that of it again for every page before the one that was slow.
// And the oldest piece is read as a piece is ([Backend]'s pieces): the
// leader's answer is the whole message.
//
// # What a sweep costs as the bucket grows
//
// The sort makes a listing grow faster than the bucket: measured on one solo
// member's file store over the bucket's own piece subjects (internal/jsapi's
// BenchmarkASubjectListing, one listing each on four cores), a page takes
// 0.13 s at a hundred thousand objects, 0.39 s at three hundred thousand,
// 1.26 s at a million and 4.22 s at three million, a whole listing 0.13,
// 1.36, 13.2 and 120 s — once an hour, holding one of the leader's API
// workers a page at a time. The sweep runs on its duty's lifetime with no
// deadline, so each page is held to the client's five seconds on its own,
// which by that growth a page outgrows at about three and a half million
// objects on those cores. Past it every sweep fails, and the uploads it would
// have abandoned stay billed, but after a node's first, each asks the page
// once ([jsapi.Leader.Subjects] keeps what a page nobody answered was asked
// for) rather than three sorts of every piece subject at once; a bucket that
// size has outgrown the broker, and the S3 backend lists its unfinished
// uploads itself at any size (`crewlet validate` warns of the nats bucket on
// a fleet).
//
// # Why not a walk of the pieces
//
// A walk of the piece subjects — an ordered consumer delivering each one's
// last piece, headers only, as the metadata walk does — is linear in the
// objects, and a replica serving it would cost nothing here, since every
// candidate is read again from the leader. It is not taken for what the walk
// reads: the broker finds each subject's last message in the file-store
// block holding it, and delivering it loads that block, so the walk's cost is
// the bucket's BYTES, about all of them for objects smaller than a block, where
// the listing reads only the per-subject index the store keeps in memory.
// Measured beside the listing on two thousand objects of one 128 KiB piece
// each, the walk took 103 ms where it took 6.3 ms over the same subjects on
// empty messages, and the listing 2.2 ms on either; over a million and three
// million empty messages it took 3.1 and 6.9 s, where the listing took 13.2
// and 120. But a million real objects is a hundred gigabytes and more to
// read for every sweep, beside the metadata walk's own read of the same
// blocks. And to start it the broker reads the per-subject state of every
// block whose state is not in memory, which it lets go of two minutes after
// the block was last read, holding the stream's store lock throughout (the
// vendored server's filestore.go, multiLastSeqsLocked), so an hourly walk
// would hold every append to the bucket on that member behind a read of the
// bucket.
func (b *Backend) Pending(ctx context.Context, visit func(objstore.Pending) error) error {
	// THE CANDIDATES ARE THE SUBJECT LISTING ITSELF, whittled down by the
	// metadata walk, so the pass holds no more than the broker's answers
	// already did — never a second copy of the bucket's inventory.
	orphans := make(map[string]uint64)
	err := b.leader.Subjects(ctx, pieceSubjects, func(page map[string]uint64) error {
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
