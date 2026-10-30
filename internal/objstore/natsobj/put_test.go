package natsobj_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat"
)

// suiteStall is the stall bound the suite's stalling backends are given: two
// seconds, long enough that a machine running the whole suite under the race
// detector does not take a healthy acknowledgement for a stall, and short
// enough that a case costs seconds rather than the production minute.
const suiteStall = 2 * time.Second

// acrossLink is how the engine opens the bucket on a node whose connection
// crosses a link to the stream — a leaf, an external cluster's client — at
// the shipped lease, renewed every [seat.HeartbeatInterval].
var acrossLink = natsobj.Config{MaxBytes: testCeiling, Replicas: 1, AcrossLink: true, LeaseHeartbeat: seat.HeartbeatInterval}

// stallingMember is the suite's stalling backend on a member reached over TCP:
// a client dialing the member through a [link], which the freeze stops in both
// directions — a broker that stopped acknowledging, as a node dialing an
// external cluster meets one.
//
// ITS CLEAN-UP MAY ADD TWO [natsobj.PurgeWait]s: the drain of what is out, and
// the purge of the pieces, sent into the frozen link and never answered — the
// metadata is never published, its pieces never having been acknowledged.
func stallingMember(t *testing.T) (objstore.Backend, time.Duration, time.Duration, func() func()) {
	ns := tcpMember(t)
	l := relay(t, ns.Addr().String())
	b := openWith(t, dialThrough(t, l), acrossLink)
	natsobj.SetStall(b, suiteStall)
	return b, suiteStall, 2 * natsobj.PurgeWait, func() func() { l.freeze(); return l.thaw }
}

// stallingLeaf is the same through a leaf whose link to its member runs
// through the [link]: a node without `data`, whose leaf link froze.
func stallingLeaf(t *testing.T) (objstore.Backend, time.Duration, time.Duration, func() func()) {
	member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: t.TempDir(),
		FleetAuth: linkFleetAuth()})
	if err != nil {
		t.Fatalf("start the member: %v", err)
	}
	t.Cleanup(member.Shutdown)
	l := relay(t, fmt.Sprintf("127.0.0.1:%d", member.LeafPort()))
	c := client(t, startLeaf(t, l.port()))
	awaitLink(t, c)
	b := openWith(t, c, acrossLink)
	natsobj.SetStall(b, suiteStall)
	return b, suiteStall, 2 * natsobj.PurgeWait, func() func() { l.freeze(); return l.thaw }
}

// A PUT ACROSS A LINK SLOWER THAN FIVE SECONDS A MESSAGE LANDS. Half a
// mebibyte crosses a link carrying the client's bytes at 64 KiB a second:
// four messages are in the client's socket at once, and the last is
// acknowledged eight seconds after it was published — which the library's
// own put, giving every acknowledgement five seconds from its publish,
// failed while the put was moving (`nats: timeout waiting for ack`; 2 MiB at
// 1.25× the floor failed at 10.0 s). The link's own time is what it takes.
func TestAPutAcrossALinkSlowerThanFiveSecondsAMessageLands(t *testing.T) {
	t.Parallel()
	ns := tcpMember(t)
	l := relay(t, ns.Addr().String())
	b := open(t, dialThrough(t, l))
	l.slow(64 << 10)
	data := patterned(512 << 10)
	started := time.Now()
	if err := b.Put(t.Context(), "slow", bytes.NewReader(data), objstore.PutMeta{}); err != nil {
		t.Fatalf("a put across a link at 64 KiB/s failed after %v: %v", time.Since(started), err)
	}
	// THE LINK WAS SLOW: half a mebibyte at 64 KiB/s is eight seconds, so a
	// put that took much less never met what this case stages.
	if took := time.Since(started); took < 7*time.Second {
		t.Fatalf("half a mebibyte crossed a link at 64 KiB/s in %v; the link was not slow", took)
	}
	l.slow(0)
	readBack(t, b, "slow", data)
}

// A PUT THE BROKER STOPS ACKNOWLEDGING FAILS AS STALLED a stall after its
// last acknowledgement, names the messages it has out, and — once the link
// moves again — leaves no object a stat or a listing finds.
//
// THE LINK FREEZES BOTH WAYS once the put's first message is acknowledged:
// the body waits on its second read until then, so the stall is measured from
// an acknowledgement rather than from the put's start. A freeze holds what the
// link was carrying, the clean-up's purge request among it, and delivers it
// after the thaw in an order the stream's ingest and its API do not keep, so a
// piece can land after the purge: an unfinished upload, which the collector
// abandons. So Pending is not held to be empty — only to name nothing but
// this put's own pieces, and to be cleared by abandoning them.
func TestAPutTheBrokerStopsAcknowledgingFailsAsStalled(t *testing.T) {
	t.Parallel()
	ns := tcpMember(t)
	l := relay(t, ns.Addr().String())
	direct := directClient(t, ns)
	pieces := watchPieces(t, direct)
	b := open(t, dialThrough(t, l))
	const stall = 2 * time.Second
	natsobj.SetStall(b, stall)

	body := &freezing{r: bytes.NewReader(patterned(4 << 20)), after: natsobj.MessageBytes,
		freeze: func() {
			pieces.await(t, 1)
			// THE FIRST PIECE'S ACKNOWLEDGEMENT crosses loopback in
			// microseconds once the broker has the piece.
			time.Sleep(200 * time.Millisecond)
			l.freeze()
		}}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	started := time.Now()
	err := b.Put(ctx, "frozen", body, objstore.PutMeta{})
	took := time.Since(started)
	if !errors.Is(err, objstore.ErrStalled) {
		t.Fatalf("a put whose broker stopped acknowledging = %v after %v; want objstore.ErrStalled", err, took)
	}
	if !strings.Contains(err.Error(), "of its messages out") {
		t.Fatalf("the stall = %q; want it to name the messages it had out", err)
	}
	// Its stall, the drain of what was out and one purge — the metadata was
	// never published — and a second for the machine.
	if most := stall + 2*natsobj.PurgeWait + time.Second; took > most {
		t.Fatalf("a put whose broker stopped acknowledging ended after %v, past %v", took, most)
	}
	l.thaw()

	if _, err := b.Stat(t.Context(), "frozen"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Stat after a stalled put = %v; want ErrNotFound", err)
	}
	if err := b.List(t.Context(), func(info objstore.Info) error {
		return fmt.Errorf("a listing after a stalled put named %+v", info)
	}); err != nil {
		t.Fatal(err)
	}
	nuid := pieces.nuid(t)
	if err := b.Pending(t.Context(), func(p objstore.Pending) error {
		if p.ID != nuid {
			return fmt.Errorf("Pending named %+v; the only upload begun was %s", p, nuid)
		}
		return b.Abandon(t.Context(), p)
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Pending(t.Context(), func(p objstore.Pending) error {
		return fmt.Errorf("Pending named %+v after it was abandoned", p)
	}); err != nil {
		t.Fatal(err)
	}
}

// A PUT WHOSE METADATA'S ACKNOWLEDGEMENT IS LOST LEAVES NO OBJECT. The
// metadata lands on the stream and its acknowledgement never comes back, so the
// put stalls not knowing whether the object exists — and it may: left there,
// the metadata names pieces the clean-up purges, an object a stat and a
// listing find and every read calls damaged. So the clean-up purges the
// metadata FIRST, where it was published; the library's purged the pieces
// alone.
func TestAPutWhoseMetadataAckIsLostLeavesNoObject(t *testing.T) {
	t.Parallel()
	ns := tcpMember(t)
	l := relay(t, ns.Addr().String())
	b := open(t, dialThrough(t, l))
	natsobj.SetStall(b, 2*time.Second)
	l.freezeDownAfter("$O." + natsobj.Bucket + ".M.")

	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	err := b.Put(ctx, "unacknowledged", bytes.NewReader(patterned(3*natsobj.MessageBytes/2)), objstore.PutMeta{})
	if !errors.Is(err, objstore.ErrStalled) {
		t.Fatalf("a put whose metadata was never acknowledged = %v; want objstore.ErrStalled", err)
	}
	l.thaw()
	if info, err := b.Stat(t.Context(), "unacknowledged"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Stat after a put whose metadata acknowledgement was lost = %+v, %v; "+
			"want ErrNotFound — the metadata names pieces the clean-up purged", info, err)
	}
}

// A FAILED PUT PURGES ONLY ONCE WHAT IT SENT IS ACKNOWLEDGED — or its drain
// runs out. The stream takes a piece on one queue and a purge on another, and
// keeps no order between the two, so a purge sent while pieces were still on
// their way could be applied before they were stored: they landed after it, an
// upload nobody finished, and through a leaf the suite's case of one failed in
// five runs of six. Held here by the order the link carried them in: with the
// broker's answers held back, the purge does not cross until they are let
// through — and then pending names nothing, every piece having been stored
// before the purge that removed it.
func TestAFailedPutPurgesOnlyOnceWhatItSentIsAcknowledged(t *testing.T) {
	t.Parallel()
	ns := tcpMember(t)
	l := relay(t, ns.Addr().String())
	b := open(t, dialThrough(t, l))
	purged := l.carried("STREAM.PURGE.")
	l.freezeDown()
	failed := make(chan struct{})
	body := &failing{r: bytes.NewReader(patterned(3 * natsobj.MessageBytes)), err: errors.New("the person went away"),
		failed: failed}
	put := make(chan error, 1)
	go func() { put <- b.Put(t.Context(), "failed", body, objstore.PutMeta{}) }()
	<-failed
	// HELD A SECOND PAST THE FAILURE — well inside the drain's [natsobj.PurgeWait]
	// — so a purge sent without waiting for the acknowledgements crosses
	// before they are let through.
	time.Sleep(time.Second)
	thawed := time.Now()
	l.thaw()
	if err := <-put; !errors.Is(err, body.err) {
		t.Fatalf("a put whose body failed = %v; want the body's own error", err)
	}
	select {
	case at := <-purged:
		if at.Before(thawed) {
			t.Fatalf("the failed put's purge crossed %v before its pieces' acknowledgements were let through",
				thawed.Sub(at))
		}
	default:
		t.Fatal("a failed put sent no purge")
	}
	if err := b.Pending(t.Context(), func(p objstore.Pending) error {
		return fmt.Errorf("a failed put left %+v pending: a piece was stored after the purge", p)
	}); err != nil {
		t.Fatal(err)
	}
}

// A PUT WAITING ON ITS CALLER IS NO STALL: nothing is out while the body has
// handed nothing, so the broker owes nothing, and a person's slow upload is
// never ended for being slow — only a broker that stops acknowledging what it
// was sent is.
func TestAPutWaitingOnItsCallerIsNoStall(t *testing.T) {
	t.Parallel()
	b := open(t, memberClient(t))
	const stall = time.Second
	natsobj.SetStall(b, stall)
	data := patterned(3 * natsobj.MessageBytes)
	body := &pausing{r: bytes.NewReader(data), after: natsobj.MessageBytes, pause: 3 * stall}
	if err := b.Put(t.Context(), "paused", body, objstore.PutMeta{}); err != nil {
		t.Fatalf("a put whose body paused %v past its %v stall: %v", 3*stall, stall, err)
	}
	readBack(t, b, "paused", data)
}

// A PUT IS THE LIBRARY'S FORMAT: the library's own object-store reader, which
// is what `nats object get` is, reads what this put stored — its size, its
// pieces, its digest and its content type, and the bytes whole — and the
// metadata message carries the rollup header that keeps one message a name.
func TestAPutIsTheLibrarysFormat(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	b := open(t, c)
	data := patterned(2*natsobj.MessageBytes + 1000)
	if err := b.Put(t.Context(), "formatted", bytes.NewReader(data),
		objstore.PutMeta{ContentType: "text/plain"}); err != nil {
		t.Fatal(err)
	}
	store, err := c.ObjectStore(t.Context(), natsobj.Bucket)
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.GetInfo(t.Context(), "formatted")
	if err != nil {
		t.Fatalf("the library's GetInfo: %v", err)
	}
	sum := sha256.Sum256(data)
	if want := "SHA-256=" + base64.URLEncoding.EncodeToString(sum[:]); info.Digest != want {
		t.Fatalf("the library read the digest %q, want %q", info.Digest, want)
	}
	if info.Size != uint64(len(data)) || info.Chunks != 3 || info.Opts == nil ||
		info.Opts.ChunkSize != natsobj.MessageBytes || info.Headers.Get("Content-Type") != "text/plain" {
		t.Fatalf("the library read %+v (options %+v)", info, info.Opts)
	}
	got, err := store.GetBytes(t.Context(), "formatted")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the library read back %d bytes, %v; want the %d put", len(got), err, len(data))
	}
	stream, err := c.Stream(t.Context(), natsobj.Stream)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := stream.GetLastMsgForSubject(t.Context(),
		"$O."+natsobj.Bucket+".M."+base64.URLEncoding.EncodeToString([]byte("formatted")))
	if err != nil {
		t.Fatal(err)
	}
	if got := raw.Header.Get(jetstream.MsgRollup); got != jetstream.MsgRollupSubject {
		t.Fatalf("the metadata carries %s %q, want %q", jetstream.MsgRollup, got, jetstream.MsgRollupSubject)
	}
}

// A PUT PUBLISHES EACH PIECE ONCE AND IN ORDER, from one buffer refilled as
// each publish returns — every piece but the last full, and the pieces in
// order the object's bytes. A piece published twice, or out of order, is what
// the library's retry made of its one reused buffer.
func TestAPutPublishesEachPieceOnceAndInOrder(t *testing.T) {
	t.Parallel()
	ns := tcpMember(t)
	pieces := watchPieces(t, directClient(t, ns))
	b := open(t, dialThrough(t, relay(t, ns.Addr().String())))
	data := patterned(16*natsobj.MessageBytes + 1000)
	if err := b.Put(t.Context(), "ordered", bytes.NewReader(data), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	pieces.await(t, 17)
	got := pieces.all()
	if len(got) != 17 {
		t.Fatalf("a put of %d bytes published %d pieces, want 17", len(data), len(got))
	}
	var joined []byte
	for i, p := range got {
		if i < len(got)-1 && len(p) != natsobj.MessageBytes {
			t.Fatalf("piece %d of %d holds %d bytes, want %d", i, len(got), len(p), natsobj.MessageBytes)
		}
		joined = append(joined, p...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("the pieces, in the order they were published, are not the object's bytes")
	}
}

// A PIECE THE STREAM REFUSES FAILS THE PUT AT ONCE: a publish no stream takes
// is answered "no responders", and the put fails then rather than publish it
// again — a piece re-sent late lands out of order, from whatever its buffer
// holds by then.
func TestAPieceTheStreamRefusesFailsThePutAtOnce(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	b := open(t, c)
	if err := c.DeleteStream(t.Context(), natsobj.Stream); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := b.Put(t.Context(), "refused", bytes.NewReader(patterned(1000)), objstore.PutMeta{})
	took := time.Since(started)
	if !errors.Is(err, jetstream.ErrNoStreamResponse) {
		t.Fatalf("a put no stream takes = %v; want the stream's no-responders error", err)
	}
	if took > 250*time.Millisecond {
		t.Fatalf("a put no stream takes failed after %v; want it failed at its first refusal, "+
			"not after the library's 250 ms retries", took)
	}
}

// A PUT LEAVES NO SUBSCRIPTION BEHIND: each one's publisher listens for its
// acknowledgements on a reply subscription of its own, and one left on the
// connection after every put is a leak a node grows by one an upload.
func TestAPutLeavesNoSubscriptionBehind(t *testing.T) {
	t.Parallel()
	c := memberClient(t)
	b := open(t, c)
	before := c.Conn().NumSubscriptions()
	for i := range 20 {
		if err := b.Put(t.Context(), fmt.Sprintf("p-%d", i), bytes.NewReader(patterned(1000)),
			objstore.PutMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	if after := c.Conn().NumSubscriptions(); after != before {
		t.Fatalf("20 puts took the connection from %d subscriptions to %d", before, after)
	}
}

// PUTS ON ONE CONNECTION SHARE ONE WINDOW: with the broker's answers stopped,
// three puts at once publish the connection's window of pieces BETWEEN them —
// counted where the broker takes them — and no more before they stall. What
// the window protects is the connection the node's leases and events ride, so a
// link that freezes holds one window of the node's uploads however many are
// under way, never one a put. Across a link it is [natsobj.LinkWindow] at the
// node's lease heartbeat — the shipped one, and one of a 15-second lease; on a
// member's own client, [natsobj.MemberWindow].
func TestPutsOnOneConnectionShareOneWindow(t *testing.T) {
	t.Parallel()
	shortLease := acrossLink
	shortLease.LeaseHeartbeat = 15 * time.Second / seat.HeartbeatRatio
	for _, c := range []struct {
		name string
		cfg  natsobj.Config
		want int
	}{
		{"on a member's own client", natsobj.Config{MaxBytes: testCeiling, Replicas: 1}, natsobj.MemberWindow},
		{"across a link", acrossLink, natsobj.LinkWindow(acrossLink.LeaseHeartbeat)},
		{"across a link on a short lease", shortLease, natsobj.LinkWindow(shortLease.LeaseHeartbeat)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ns := tcpMember(t)
			pieces := watchPieces(t, directClient(t, ns))
			l := relay(t, ns.Addr().String())
			b := openWith(t, dialThrough(t, l), c.cfg)
			if got := b.Window(); got != c.want {
				t.Fatalf("a backend opened with %+v has a window of %d, want %d", c.cfg, got, c.want)
			}
			natsobj.SetStall(b, time.Second)
			l.freezeDown()
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			const puts = 3
			errs := make([]error, puts)
			var wg sync.WaitGroup
			for i := range puts {
				wg.Go(func() {
					errs[i] = b.Put(ctx, fmt.Sprintf("windowed-%d", i), bytes.NewReader(patterned(4<<20)),
						objstore.PutMeta{})
				})
			}
			wg.Wait()
			for i, err := range errs {
				if !errors.Is(err, objstore.ErrStalled) {
					t.Fatalf("put %d of %d whose acknowledgements never came = %v; want objstore.ErrStalled",
						i, puts, err)
				}
			}
			if got := len(pieces.all()); got != c.want {
				t.Fatalf("%d puts at once with no acknowledgement published %d pieces between them, "+
					"want the connection's window of %d", puts, got, c.want)
			}
			l.thaw()
		})
	}
}

// THE LINK'S WINDOW CROSSES THE FLOOR WITHIN THE NODE'S OWN HEARTBEAT. A lease
// renewal queued behind a full window on a link at the floor every leg is
// promised ([objstore.MiBPace]) crosses within one heartbeat interval of the
// lease the node holds — the TTL over [seat.HeartbeatRatio], whatever
// `coordination.lease_ttl_seconds` made it — so a renewal sent an interval
// after the last still lands with a whole interval of the lease to spare, the
// margin the lease keeps for a missed beat. And the window is the WIDEST that
// does, up to the member's own: narrower is throughput thrown away on a lease
// that did not need it. Held over the shipped TTL and others, against the
// lease's and the store's own constants, so a window sized for one lease — the
// shipped one, as it was — fails the rows of every other.
//
// A lease whose heartbeat is shorter than one message's crossing keeps that
// margin behind no window at all; the window is one there, and the TTL is
// under the floor `crewlet validate` warns of ([config.LinkLeaseFloor]).
func TestTheLinkWindowCrossesTheFloorWithinAHeartbeat(t *testing.T) {
	t.Parallel()
	crossing := func(messages int) time.Duration {
		return time.Duration(int64(messages)*natsobj.MessageBytes) * objstore.MiBPace / objstore.MiB
	}
	if natsobj.MessageCrossing != crossing(1) {
		t.Fatalf("one message crosses a link at the floor in %v by natsobj's figure and %v by the "+
			"floor's own", natsobj.MessageCrossing, crossing(1))
	}
	for _, ttl := range []time.Duration{
		seat.SeatLeaseTTL, 15 * time.Second, 22500 * time.Millisecond, 30 * time.Second,
		90 * time.Second, 180 * time.Second, 10 * time.Minute,
		config.LinkLeaseFloor, 8 * time.Second, time.Second,
	} {
		t.Run(ttl.String(), func(t *testing.T) {
			t.Parallel()
			heartbeat := ttl / seat.HeartbeatRatio
			window := natsobj.LinkWindow(heartbeat)
			if window < 1 || window > natsobj.MemberWindow {
				t.Fatalf("a %v lease's link window is %d messages, outside 1..%d", ttl, window,
					natsobj.MemberWindow)
			}
			if heartbeat < natsobj.MessageCrossing {
				if window != 1 {
					t.Errorf("a %v lease, renewed every %v, under one message's %v crossing, has a "+
						"window of %d; want the one a put needs to move at all", ttl, heartbeat,
						natsobj.MessageCrossing, window)
				}
				if ttl >= config.LinkLeaseFloor {
					t.Errorf("a %v lease cannot keep its margin behind one message, and is not under "+
						"the %v floor crewlet validate warns of", ttl, config.LinkLeaseFloor)
				}
				return
			}
			if c := crossing(window); c > heartbeat {
				t.Errorf("a %v lease's window of %d messages crosses a link at the floor in %v, past its "+
					"heartbeat of %v: a renewal queued behind it misses its beat", ttl, window, c, heartbeat)
			}
			if spare := ttl - heartbeat - crossing(window); spare < heartbeat {
				t.Errorf("on a %v lease a renewal sent a heartbeat after the last and queued behind a full "+
					"window lands with %v of the lease to spare, under its heartbeat of %v", ttl, spare,
					heartbeat)
			}
			if window < natsobj.MemberWindow && crossing(window+1) <= heartbeat {
				t.Errorf("a %v lease's link window is %d messages, and %d still cross within its %v "+
					"heartbeat: throughput thrown away", ttl, window, window+1, heartbeat)
			}
			if c := crossing(window); c > objstore.Stall {
				t.Errorf("a message waits up to %v behind a full window at the floor, past the %v stall",
					c, objstore.Stall)
			}
		})
	}
}

// THE FLOOR VALIDATE WARNS OF IS THE WINDOW'S OWN ARITHMETIC: three heartbeats
// of one message crossing a link at the floor. config states it as a figure,
// importing none of the packages that make it, so this holds the two together.
func TestTheLinkLeaseFloorIsOneMessageAHeartbeat(t *testing.T) {
	t.Parallel()
	if want := seat.HeartbeatRatio * natsobj.MessageCrossing; config.LinkLeaseFloor != want {
		t.Fatalf("config.LinkLeaseFloor is %v; one message's crossing at the floor (%v) a heartbeat "+
			"is a lease of %v", config.LinkLeaseFloor, natsobj.MessageCrossing, want)
	}
}

// A BUCKET REACHED ACROSS A LINK NEEDS THE NODE'S HEARTBEAT, which sizes the
// window its uploads share: a zero is refused rather than read as some lease,
// since a window sized for a lease the node does not hold is the one that
// loses its seats.
func TestABucketAcrossALinkNeedsTheNodesHeartbeat(t *testing.T) {
	t.Parallel()
	_, err := natsobj.Open(t.Context(), memberClient(t), natsobj.Config{MaxBytes: testCeiling, Replicas: 1, AcrossLink: true})
	if err == nil || !strings.Contains(err.Error(), "LeaseHeartbeat") {
		t.Fatalf("a bucket opened across a link with no lease heartbeat = %v; want a refusal naming "+
			"Config.LeaseHeartbeat", err)
	}
}

// A PUT WAITING FOR A PLACE ENDS WHEN THE CONNECTION TAKES NOTHING: behind a
// window another put filled, and with the broker's answers stopped, it is
// ended as stalled a stall after the connection's last acknowledgement — not
// after the puts ahead of it have stalled, cleaned up and given their places
// back, which is a stall and two purges later, and then its own stall again.
// It publishes nothing: it never had a place.
func TestAPutWaitingForAPlaceEndsWhenTheConnectionTakesNothing(t *testing.T) {
	t.Parallel()
	ns := tcpMember(t)
	pieces := watchPieces(t, directClient(t, ns))
	l := relay(t, ns.Addr().String())
	b := open(t, dialThrough(t, l))
	const stall = 2 * time.Second
	natsobj.SetStall(b, stall)
	l.freezeDown()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() {
		first <- b.Put(ctx, "first", bytes.NewReader(patterned(4<<20)), objstore.PutMeta{})
	}()
	pieces.await(t, natsobj.MemberWindow)
	began := time.Now()
	err := b.Put(ctx, "behind", bytes.NewReader(patterned(1<<20)), objstore.PutMeta{})
	took := time.Since(began)
	if !errors.Is(err, objstore.ErrStalled) {
		t.Fatalf("a put waiting behind a window the broker never answered = %v after %v; want objstore.ErrStalled",
			err, took)
	}
	if !strings.Contains(err.Error(), "waiting for a place") {
		t.Fatalf("the stall = %q; want it to say the put was waiting for a place", err)
	}
	if most := stall + time.Second; took > most {
		t.Fatalf("a put waiting for a place on a connection that took nothing ended after %v, past %v", took, most)
	}
	if err := <-first; !errors.Is(err, objstore.ErrStalled) {
		t.Fatalf("the put that filled the window = %v; want objstore.ErrStalled", err)
	}
	if got := len(pieces.all()); got != natsobj.MemberWindow {
		t.Fatalf("the broker took %d pieces, want the first put's window of %d and none of the put "+
			"that waited", got, natsobj.MemberWindow)
	}
	l.thaw()
}

// A PUT WAITING FOR A PLACE IS NO STALL WHILE THE CONNECTION MOVES: four puts
// share a window of one message across a link that takes a message a second,
// so a put waits up to three seconds for its turn — longer than its stall —
// while the connection is acknowledging one message every second. Each lands:
// the waiter's clock is the connection's last acknowledgement, never how long
// it has waited.
func TestAPutWaitingForAPlaceIsNoStallWhileTheConnectionMoves(t *testing.T) {
	t.Parallel()
	ns := tcpMember(t)
	l := relay(t, ns.Addr().String())
	b := open(t, dialThrough(t, l))
	natsobj.SetWindow(b, 1)
	natsobj.SetStall(b, 2*time.Second)
	l.slow(natsobj.MessageBytes)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	const puts = 4
	data := make([][]byte, puts)
	errs := make([]error, puts)
	started := time.Now()
	var wg sync.WaitGroup
	for i := range puts {
		data[i] = patterned(2 * natsobj.MessageBytes)
		data[i][0] = byte(i)
		wg.Go(func() {
			errs[i] = b.Put(ctx, fmt.Sprintf("turn-%d", i), bytes.NewReader(data[i]), objstore.PutMeta{})
		})
	}
	wg.Wait()
	took := time.Since(started)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("put %d of %d sharing a window of one across a moving link = %v after %v", i, puts, err, took)
		}
	}
	// THE PUTS WAITED: eight pieces at a second each, one at a time.
	if took < 6*time.Second {
		t.Fatalf("%d puts of two pieces each crossed a link taking a piece a second in %v; "+
			"they did not wait their turns", puts, took)
	}
	l.slow(0)
	for i := range puts {
		readBack(t, b, fmt.Sprintf("turn-%d", i), data[i])
	}
}

// freezing hands its first after bytes, then calls freeze once, before the
// rest.
type freezing struct {
	r      io.Reader
	after  int
	read   int
	once   sync.Once
	freeze func()
}

func (f *freezing) Read(p []byte) (int, error) {
	if f.read >= f.after {
		f.once.Do(f.freeze)
	} else if len(p) > f.after-f.read {
		p = p[:f.after-f.read]
	}
	n, err := f.r.Read(p)
	f.read += n
	return n, err
}

// failing hands everything r holds, then fails with err — closing failed as it
// does.
type failing struct {
	r      io.Reader
	err    error
	failed chan struct{}
	once   sync.Once
}

func (f *failing) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		f.once.Do(func() { close(f.failed) })
		return n, f.err
	}
	return n, err
}

// pausing hands its first after bytes, then nothing for pause, then the rest.
type pausing struct {
	r      io.Reader
	after  int
	read   int
	pause  time.Duration
	paused bool
}

func (b *pausing) Read(p []byte) (int, error) {
	if b.read >= b.after && !b.paused {
		b.paused = true
		time.Sleep(b.pause)
	} else if b.read < b.after && len(p) > b.after-b.read {
		p = p[:b.after-b.read]
	}
	n, err := b.r.Read(p)
	b.read += n
	return n, err
}

// readBack reads name back whole from b, wanting want.
func readBack(t *testing.T, b *natsobj.Backend, name string, want []byte) {
	t.Helper()
	r, err := b.Get(deadline(t), name, 0, -1)
	if err != nil {
		t.Fatalf("Get %s: %v", name, err)
	}
	defer r.Close()
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s read back %d bytes, %v; want the %d put", name, len(got), err, len(want))
	}
}

func patterned(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*31 + i/251)
	}
	return out
}

// tcpMember is a member reached over TCP — JetStream on, the fleet's domain
// served, a client port on loopback — which no embedded member opens: a link
// can only stand between a client and a broker it dials.
func tcpMember(t *testing.T) *server.Server {
	t.Helper()
	ns, err := server.NewServer(&server.Options{ServerName: "member", Host: "127.0.0.1", Port: -1,
		JetStream: true, JetStreamDomain: jsapi.Domain, StoreDir: t.TempDir(), NoSigs: true})
	if err != nil {
		t.Fatalf("configure the member: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatal("the member did not become ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	return ns
}

// dialThrough is a client of the broker behind l, dialing it through l.
func dialThrough(t *testing.T, l *link) jetstream.JetStream {
	t.Helper()
	nc, err := nats.Connect(fmt.Sprintf("nats://127.0.0.1:%d", l.port()))
	if err != nil {
		t.Fatalf("connect through the link: %v", err)
	}
	t.Cleanup(nc.Close)
	c, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// directClient is a client of ns that no link stands in front of.
func directClient(t *testing.T, ns *server.Server) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect to the member: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// piecesSeen is every piece of every put published to the bucket, as a core
// subscription at the broker saw them — in the order the broker took them.
type piecesSeen struct {
	mu       sync.Mutex
	subjects []string
	data     [][]byte
	arrived  chan struct{}
}

func watchPieces(t *testing.T, nc *nats.Conn) *piecesSeen {
	t.Helper()
	w := &piecesSeen{arrived: make(chan struct{}, 1<<10)}
	sub, err := nc.Subscribe("$O."+natsobj.Bucket+".C.>", func(m *nats.Msg) {
		w.mu.Lock()
		w.subjects = append(w.subjects, m.Subject)
		w.data = append(w.data, m.Data)
		w.mu.Unlock()
		w.arrived <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return w
}

// await waits until n pieces have been seen.
func (w *piecesSeen) await(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		w.mu.Lock()
		got := len(w.data)
		w.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-w.arrived:
		case <-deadline:
			t.Fatalf("saw %d pieces, waiting for %d", got, n)
		}
	}
}

func (w *piecesSeen) all() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]byte(nil), w.data...)
}

// nuid is the piece identifier of the one put the watch saw.
func (w *piecesSeen) nuid(t *testing.T) string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.subjects) == 0 {
		t.Fatal("no piece was published")
	}
	id := strings.TrimPrefix(w.subjects[0], "$O."+natsobj.Bucket+".C.")
	for _, s := range w.subjects {
		if s != w.subjects[0] {
			t.Fatalf("pieces were published on %s and %s, want one put's", w.subjects[0], s)
		}
	}
	return id
}

// link is a TCP relay a case puts between a client and the broker it dials —
// the client's bytes UP, the broker's DOWN — which it can slow in the up
// direction and freeze in either. A frozen direction holds what it has read
// and carries nothing more until it is thawed, as a link that stopped
// carrying does: what was sent waits in the sockets' buffers on both sides.
//
// A RELAY, never a wrapper around the client's own writes: paced on the
// client's side, each publish blocks in the slowed write, its acknowledgement's
// timer starts late, and the library's put lands — the case meant to show it
// fail could not.
type link struct {
	ln     net.Listener
	target string
	up     valve
	down   valve
	rate   atomic.Int64
	done   chan struct{}

	watchMu sync.Mutex
	watches []upWatch

	mu    sync.Mutex
	shut  bool
	conns []net.Conn
}

// relay listens on a loopback port and carries every connection to target,
// until the case ends.
func relay(t *testing.T, target string) *link {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the link: %v", err)
	}
	l := &link{ln: ln, target: target, done: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			if !l.track(in) {
				_ = in.Close()
				continue
			}
			wg.Go(func() { l.pipe(in) })
		}
	})
	t.Cleanup(func() {
		close(l.done)
		_ = ln.Close()
		l.mu.Lock()
		l.shut = true
		conns := l.conns
		l.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		wg.Wait()
	})
	return l
}

func (l *link) port() int { return l.ln.Addr().(*net.TCPAddr).Port }

// slow holds the up direction to rate bytes a second; zero is loopback.
func (l *link) slow(rate int64) { l.rate.Store(rate) }

func (l *link) freeze()     { l.up.freeze(); l.down.freeze() }
func (l *link) freezeDown() { l.down.freeze() }
func (l *link) thaw()       { l.up.thaw(); l.down.thaw() }

// upWatch is something to do as the up direction carries pattern.
type upWatch struct {
	pattern string
	fire    func()
}

// onUp calls fire, once, as the up direction carries pattern: when the bytes
// holding it have passed the valve and before they are written on, so what
// fire does is in place before the broker can answer them.
func (l *link) onUp(pattern string, fire func()) {
	l.watchMu.Lock()
	defer l.watchMu.Unlock()
	l.watches = append(l.watches, upWatch{pattern: pattern, fire: fire})
}

// freezeDownAfter freezes the down direction as the up direction carries
// pattern, so the broker's answer to it is held.
func (l *link) freezeDownAfter(pattern string) { l.onUp(pattern, l.down.freeze) }

// carried answers the instant the up direction carried pattern on, once it
// has.
func (l *link) carried(pattern string) <-chan time.Time {
	at := make(chan time.Time, 1)
	l.onUp(pattern, func() { at <- time.Now() })
	return at
}

// fireWatches fires, and forgets, every watch whose pattern tail now holds.
func (l *link) fireWatches(tail []byte) {
	l.watchMu.Lock()
	var fired []func()
	kept := l.watches[:0]
	for _, w := range l.watches {
		if bytes.Contains(tail, []byte(w.pattern)) {
			fired = append(fired, w.fire)
			continue
		}
		kept = append(kept, w)
	}
	l.watches = kept
	l.watchMu.Unlock()
	for _, fire := range fired {
		fire()
	}
}

func (l *link) track(c net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shut {
		return false
	}
	l.conns = append(l.conns, c)
	return true
}

func (l *link) pipe(in net.Conn) {
	out, err := net.Dial("tcp", l.target)
	if err != nil {
		_ = in.Close()
		return
	}
	if !l.track(out) {
		_ = in.Close()
		_ = out.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() { l.carry(out, in, &l.up, true); done <- struct{}{} }()
	go func() { l.carry(in, out, &l.down, false); done <- struct{}{} }()
	<-done
	_ = in.Close()
	_ = out.Close()
	<-done
}

// watchTail is the longest pattern an [upWatch] may wait for.
const watchTail = 64

// carry copies src to dst a few kilobytes at a time, through v and — up — at
// the link's rate, firing the watches [link.onUp] set.
func (l *link) carry(dst io.Writer, src io.Reader, v *valve, up bool) {
	buf := make([]byte, 4<<10)
	carried := time.Now()
	var tail []byte
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if rate := l.rate.Load(); up && rate > 0 {
				carried = later(carried, time.Now()).Add(time.Duration(n) * time.Second / time.Duration(rate))
				time.Sleep(time.Until(carried))
			}
			if !v.pass(l.done) {
				return
			}
			if up {
				// THE TAIL KEEPS WHAT A PATTERN SPLIT ACROSS TWO READS
				// needs: the last watchTail bytes of the read before.
				//
				// FIRED BEFORE THE BYTES ARE WRITTEN, never after: a watch
				// that freezes the down direction must hold the broker's
				// answer to what it is about to carry, and fired once the
				// write had returned it raced that answer — on loopback
				// the broker acknowledged the metadata a put published
				// before this goroutine froze the link, under the race
				// detector on a loaded runner, and the put whose
				// acknowledgement the case had lost was told it landed.
				tail = append(tail, buf[:n]...)
				l.fireWatches(tail)
				if len(tail) > watchTail {
					tail = append(tail[:0], tail[len(tail)-watchTail:]...)
				}
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// valve is one direction of a [link]: open, or frozen until thawed.
type valve struct {
	mu     sync.Mutex
	frozen bool
	thawed chan struct{}
}

func (v *valve) freeze() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.frozen {
		v.frozen, v.thawed = true, make(chan struct{})
	}
}

func (v *valve) thaw() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.frozen {
		v.frozen = false
		close(v.thawed)
	}
}

// pass waits until v is open, or done is closed (false).
func (v *valve) pass(done <-chan struct{}) bool {
	v.mu.Lock()
	frozen, thawed := v.frozen, v.thawed
	v.mu.Unlock()
	if !frozen {
		return true
	}
	select {
	case <-thawed:
		return true
	case <-done:
		return false
	}
}
