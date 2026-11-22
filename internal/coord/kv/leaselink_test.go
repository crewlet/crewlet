package kv

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// sharedLinkRate is the case's leaf link, both ways, shared fairly by every
// connection with bytes to send: 64 KiB a second, about twice the slowest
// allowed, so the upload below holds the data link for some sixteen seconds.
const sharedLinkRate = 64 << 10

// uploadBytes is the bulk the data link carries either way: half a mebibyte,
// random so the link's compression leaves it whole.
const uploadBytes = 512 << 10

// overSharedLink is a node's data connection and its lease connection across
// one fair shared link, and a connection on the far side that answers the
// data connection's bulk: a leaf's two leaf links to its member, or two dials
// to an external server.
type overSharedLink struct {
	link                *linktest.Link
	data, leases, local *nats.Conn
	// dataAccount is what the data connection binds on the link.
	dataAccount string
	// api is a JetStream client of a connection, in the broker's domain.
	api func(*nats.Conn) (jetstream.JetStream, error)
}

// newLeafOverSharedLink is a member serving leaves behind a fair shared link
// and a leaf joined across it: the leaf's data connection (the global
// account) and its lease connection ([js.LeaseAccount]), and the member's own.
func newLeafOverSharedLink(t *testing.T) overSharedLink {
	t.Helper()
	// A MEMBER THAT SERVES LEAVES PERSISTS, or it is refused.
	member, err := js.StartServer(t.Context(), js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: brokertest.TempDir(t),
		StoreMaxBytes: brokertest.StoreMaxBytes, FleetAuth: leafFleetAuth()})
	if err != nil {
		t.Fatalf("start a member serving leaves: %v", err)
	}
	t.Cleanup(member.Shutdown)
	link := linktest.SharedEachWay(t, fmt.Sprintf("127.0.0.1:%d", member.LeafPort()), sharedLinkRate)
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf",
		LeafURLs: []string{"nats-leaf://" + link.Addr()}, FleetAuth: leafFleetAuth()})
	if err != nil {
		t.Fatalf("start the leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	f := overSharedLink{link: link, dataAccount: server.DEFAULT_GLOBAL_ACCOUNT,
		api: jsapi.Embedded().Client}
	for _, c := range []struct {
		conn **nats.Conn
		dial func() (*nats.Conn, error)
		what string
	}{{&f.data, leaf.Conn, "the leaf's data connection"},
		{&f.leases, leaf.LeaseConn, "the leaf's lease connection"},
		{&f.local, member.Conn, "the member"}} {
		nc, err := c.dial()
		if err != nil {
			t.Fatalf("connect %s: %v", c.what, err)
		}
		t.Cleanup(nc.Close)
		*c.conn = nc
	}
	return f
}

// newExternalOverSharedLink is an external server behind a fair shared link,
// dialled across it twice, as an engine on `stream.type: nats` dials its queue
// and its leases, and once directly.
func newExternalOverSharedLink(t *testing.T) overSharedLink {
	t.Helper()
	srv := externaltest.Start(t, int32(queue.MaxPayloadBytes))
	link := linktest.SharedEachWay(t, srv.HostPort(), sharedLinkRate)
	f := overSharedLink{link: link, dataAccount: linktest.Client("queue"), api: jsapi.Account().Client}
	for _, c := range []struct {
		conn      **nats.Conn
		url, name string
	}{{&f.data, "nats://" + link.Addr(), "queue"},
		{&f.leases, "nats://" + link.Addr(), "leases"},
		{&f.local, srv.URL(), "far side"}} {
		nc, err := nats.Connect(c.url, nats.Token(externaltest.Token), nats.Name(c.name))
		if err != nil {
			t.Fatalf("dial the %s connection: %v", c.name, err)
		}
		t.Cleanup(nc.Close)
		*c.conn = nc
	}
	return f
}

// client is a JetStream client of nc in the broker's domain.
func (f overSharedLink) client(t *testing.T, nc *nats.Conn) jetstream.JetStream {
	t.Helper()
	c, err := f.api(nc)
	if err != nil {
		t.Fatalf("a client of the broker's domain: %v", err)
	}
	return c
}

// carried is what the link has carried of the data connection's bytes way.
func (f overSharedLink) carried(way linktest.Way) int64 {
	return f.link.Count(f.dataAccount, way).Carried
}

// answer has the far side answer every request on subject with nothing, once
// the data connection reaches it.
func (f overSharedLink) answer(t *testing.T, subject string) {
	t.Helper()
	sub, err := f.local.Subscribe(subject, func(m *nats.Msg) { _ = m.Respond(nil) })
	if err != nil {
		t.Fatalf("answer on %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := f.local.Flush(); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, "the far side's interest across the link", func() bool {
		_, err := f.data.Request(subject, nil, time.Second)
		return err == nil
	})
}

// bulk runs send, a request on the data connection, and answers on the
// channel it returns once send has, after waiting until the link carries the
// first sixteen kibibytes of the bulk way.
func (f overSharedLink) bulk(t *testing.T, way linktest.Way, send func() error) <-chan error {
	t.Helper()
	before := f.carried(way)
	done := make(chan error, 1)
	go func() { done <- send() }()
	pollUntil(t, "the bulk on the data link", func() bool { return f.carried(way) > before+16<<10 })
	return done
}

// upload sends uploadBytes from the data connection to the far side
// ([overSharedLink.bulk]).
func (f overSharedLink) upload(t *testing.T, subject string) <-chan error {
	t.Helper()
	f.answer(t, subject)
	body := randomBytes(t, uploadBytes)
	return f.bulk(t, linktest.ToBroker, func() error {
		_, err := f.data.Request(subject, body, time.Minute)
		return err
	})
}

// leaderRead has the data connection read a record of uploadBytes from its
// stream's leader on the far side, as the fleet store reads a record by key
// ([leaderReader]): a `STREAM.MSG.GET` whose reply, the record base64-encoded
// in JSON, crosses the link toward the node ([overSharedLink.bulk]). Half the
// spec's mebibyte, so each of a case's two reads crosses in some eleven
// seconds; the renewals it holds are answered in a few quanta whatever its size.
func (f overSharedLink) leaderRead(t *testing.T, subject string) <-chan error {
	t.Helper()
	ctx := t.Context()
	stream := strings.ReplaceAll(subject, ".", "_")
	far := f.client(t, f.local)
	if _, err := far.CreateStream(ctx, jetstream.StreamConfig{Name: stream, Subjects: []string{subject},
		Storage: jetstream.FileStorage, MaxBytes: 2 * uploadBytes}); err != nil {
		t.Fatalf("create the record's stream on the far side: %v", err)
	}
	if _, err := far.Publish(ctx, subject, randomBytes(t, uploadBytes)); err != nil {
		t.Fatalf("write the record on the far side: %v", err)
	}
	leader, err := jsapi.NewLeader(f.client(t, f.data), stream)
	if err != nil {
		t.Fatalf("address the record's leader from the data connection: %v", err)
	}
	// Given the reply's arrival across the link, as the fleet store's reader
	// of a large record is, or it is asked again mid-crossing.
	leader = leader.Carrying(uploadBytes, time.Duration((1<<20)/sharedLinkRate)*time.Second)
	return f.bulk(t, linktest.ToNode, func() error {
		_, err := leader.Last(ctx, subject)
		return err
	})
}

// randomBytes is n random bytes, which the link's compression leaves whole.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// pollUntil waits until cond holds, within a liveness bound.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	bound := time.NewTimer(time.Minute) //fixedwait:liveness
	defer bound.Stop()
	tick := time.NewTicker(20 * time.Millisecond) //fixedwait:liveness
	defer tick.Stop()
	for !cond() {
		select {
		case <-bound.C:
			t.Fatalf("waited a minute for %s", what)
		case <-tick.C:
		}
	}
}

// A RENEWAL CROSSES A LEAF'S LINK AHEAD OF AN UPLOAD ON IT (#1035): the
// lease store's holds ride the lease connection, a leaf link of their own, so
// at a fair bottleneck a renewal sent while half a mebibyte crosses the data
// link is answered before the upload has crossed. A store whose hold client is
// the data connection is answered only once the upload's bytes have, since
// its renewal is queued behind them on the one link. Decided by the link's own
// count of what it carried when each renewal was answered, never by a clock.
//
// Mutant: the lease store opened over the data client — the first arm's
// renewals wait for the upload, as the control's do.
func TestARenewalCrossesALeafsLinkAheadOfAnUploadOnIt(t *testing.T) {
	t.Parallel()
	f := newLeafOverSharedLink(t)
	renewalsCrossAhead(t, f, f.upload, linktest.ToBroker)
}

// A RENEWAL CROSSES A LEAF'S LINK AHEAD OF A LEADER READ ON IT: the bulk is
// the leaf's leader read of a large record on the member, so what the
// renewal's acknowledgement queues behind on the one link is the read's reply
// crossing hub to leaf.
//
// Mutant: the lease store opened over the data client — its acknowledgements
// wait for the reply, as the control's does.
func TestARenewalCrossesALeafsLinkAheadOfALeaderReadOnIt(t *testing.T) {
	t.Parallel()
	f := newLeafOverSharedLink(t)
	renewalsCrossAhead(t, f, f.leaderRead, linktest.ToNode)
}

// A RENEWAL CROSSES AN EXTERNAL LINK AHEAD OF AN UPLOAD (M3): on `stream.type:
// nats` the lease connection is a second dial, a TCP connection of its own
// that the node's uplink queues apart from the queue's, so a renewal sent
// while the queue's dial uploads is answered before the upload has crossed.
//
// Mutant: the lease store rides the queue's own connection — its renewals
// wait for the upload, as the control's do.
func TestARenewalCrossesAnExternalLinkAheadOfAnUpload(t *testing.T) {
	t.Parallel()
	f := newExternalOverSharedLink(t)
	renewalsCrossAhead(t, f, f.upload, linktest.ToBroker)
}

// renewalsCrossAhead renews a seat over f's lease connection three times while
// bulk crosses the data connection way, and holds each to being answered
// before the bulk has crossed and within a second of the link (the spec's
// bound: a few quanta and the leader); then, the control, renews one over the
// data connection behind a second bulk, which is answered only once that has.
func renewalsCrossAhead(t *testing.T, f overSharedLink,
	bulk func(*testing.T, string) <-chan error, way linktest.Way) {

	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))
	member, err := Open(ctx, f.client(t, f.local), f.client(t, f.local),
		Config{TTL: knownTTL, BucketPrefix: prefix, Reach: InProcess})
	if err != nil {
		t.Fatalf("open the far side's store: %v", err)
	}
	t.Cleanup(member.Close)
	open := func(hold *nats.Conn) *Store {
		s, err := Open(ctx, f.client(t, hold), f.client(t, f.data),
			Config{TTL: knownTTL, BucketPrefix: prefix, Reach: AcrossLink})
		if err != nil {
			t.Fatalf("open the node's store: %v", err)
		}
		t.Cleanup(s.Close)
		return s
	}

	leases, shared := open(f.leases), open(f.data)
	const owner = "node:1"
	onLeases := claimUngated(ctx, t, leases, knownSeat(0), owner, knownTTL)
	onShared := claimUngated(ctx, t, shared, knownSeat(1), owner, knownTTL)

	start := f.carried(way)
	done := bulk(t, "kv.bulk.leases")
	for i := range 3 {
		before := f.carried(way)
		if ok, err := leases.Renew(ctx, knownSeat(0), owner, onLeases.Epoch, knownTTL); err != nil || !ok {
			t.Fatalf("renewal %d over the lease connection = (%v, %v)", i+1, ok, err)
		}
		at := f.carried(way)
		if got := at - start; got >= uploadBytes {
			t.Errorf("renewal %d over the lease connection was answered once the data link had "+
				"carried %d bytes, the whole bulk's %d: it waited behind it", i+1, got, uploadBytes)
		}
		if during := at - before; during >= sharedLinkRate {
			t.Errorf("renewal %d over the lease connection was answered after the data link carried "+
				"%d bytes of the bulk, a second of the link or more: it queued behind part of it", i+1,
				during)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("the bulk: %v", err)
	}

	// THE CONTROL: the same renewal over the data connection, queued behind
	// the bulk, is answered only once the bulk has crossed, or not at all
	// inside its own budget.
	start = f.carried(way)
	done = bulk(t, "kv.bulk.shared")
	ok, err := shared.Renew(ctx, knownSeat(1), owner, onShared.Epoch, knownTTL)
	got := f.carried(way) - start
	switch {
	case err != nil && !errors.Is(err, coord.ErrUnavailable):
		t.Fatalf("the renewal over the data connection = %v, want an answer or unknown", err)
	case err == nil && ok && got < uploadBytes:
		t.Errorf("the control's renewal was answered with %d bytes of the %d-byte bulk carried: "+
			"the case cannot tell a renewal behind the bulk from one beside it", got, uploadBytes)
	}
	if err := <-done; err != nil {
		t.Fatalf("the control's bulk: %v", err)
	}
}

// lockedBytes is the publish that holds the queue client's lock: seven
// mebibytes, past all that can leave nats.go's write before the link carries
// it — a Linux sender's socket buffer (tcp_wmem's default ceiling, 4 MiB), the
// relay's pinned receive buffer (linktest.SocketBuffer, doubled by the kernel)
// and the link's inflight — so the write, made under the connection's lock,
// returns only once the link has carried some three mebibytes.
const lockedBytes = 7 << 20

// lockedRate is the case's first-in, first-out link: a mebibyte a second, so
// the lock is held for some three seconds of the publish's seven.
const lockedRate = 1 << 20

// lockedInflight is what the link reads of a connection ahead of carrying it,
// a congestion window: a renewal on another connection queues behind this
// much of the publish, 62.5 ms at lockedRate.
const lockedInflight = 64 << 10

// A RENEWAL IS NOT HELD BY THE QUEUE CLIENT'S LOCK: nats.go writes a publish
// to its socket under the connection's lock, so behind a first-in, first-out
// link every other call on that connection waits for the write. Renewals on
// the lease connection, a dial of its own, are answered while the queue's
// write is still blocked; the control, a renewal on the queue's connection, is
// sent only once the write returns and answered once the publish has crossed.
//
// Mutant: the external lease store rides the queue's connection — its
// renewals wait for the write, as the control's does.
func TestARenewalIsNotHeldByTheQueueClientsLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := externaltest.Start(t, int32(queue.MaxPayloadBytes))
	link := linktest.SharedFIFO(t, srv.HostPort(), lockedRate, lockedInflight)
	dial := func(url, name string) *nats.Conn {
		nc, err := nats.Connect(url, nats.Token(externaltest.Token), nats.Name(name))
		if err != nil {
			t.Fatalf("dial the %s connection: %v", name, err)
		}
		t.Cleanup(nc.Close)
		return nc
	}
	queueConn, leaseConn := dial("nats://"+link.Addr(), "queue"), dial("nats://"+link.Addr(), "leases")
	local := dial(srv.URL(), "far side")
	prefix := fmt.Sprintf("t%d", bucketSeq.Add(1))
	member, err := Open(ctx, jsOf(local), jsOf(local), Config{TTL: knownTTL, BucketPrefix: prefix,
		Reach: InProcess})
	if err != nil {
		t.Fatalf("open the far side's store: %v", err)
	}
	t.Cleanup(member.Close)
	open := func(hold *nats.Conn) *Store {
		s, err := Open(ctx, jsOf(hold), jsOf(queueConn), Config{TTL: knownTTL, BucketPrefix: prefix,
			Reach: AcrossLink})
		if err != nil {
			t.Fatalf("open the node's store: %v", err)
		}
		t.Cleanup(s.Close)
		return s
	}
	leases, shared := open(leaseConn), open(queueConn)
	const owner = "node:1"
	onLeases := claimUngated(ctx, t, leases, knownSeat(0), owner, knownTTL)
	onShared := claimUngated(ctx, t, shared, knownSeat(1), owner, knownTTL)

	body := randomBytes(t, lockedBytes)
	carried := func() int64 { return link.Count(linktest.Client("queue"), linktest.ToBroker).Carried }
	// publish writes body on the queue's connection and closes the channel it
	// returns once the write has returned, its lock released, after waiting
	// until the link carries the publish.
	publish := func(subject string) <-chan struct{} {
		before := carried()
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			if err := queueConn.Publish(subject, body); err != nil {
				t.Errorf("the publish: %v", err)
			}
		}()
		pollUntil(t, "the publish on the link", func() bool { return carried() > before+lockedInflight })
		return returned
	}
	crossed := func() {
		if err := queueConn.Flush(); err != nil {
			t.Fatalf("flush the publish: %v", err)
		}
	}

	returned := publish("kv.locked.leases")
	for i := range 3 {
		before := carried()
		if ok, err := leases.Renew(ctx, knownSeat(0), owner, onLeases.Epoch, knownTTL); err != nil || !ok {
			t.Fatalf("renewal %d over the lease connection = (%v, %v)", i+1, ok, err)
		}
		if during := carried() - before; during >= lockedRate {
			t.Errorf("renewal %d over the lease connection was answered after the link carried %d "+
				"bytes of the publish, a second of it or more: it queued behind more than the "+
				"publish's inflight", i+1, during)
		}
		select {
		case <-returned:
			t.Fatalf("the publish's write returned before renewal %d was answered: the case never "+
				"held the queue client's lock across it", i+1)
		default:
		}
	}
	<-returned
	crossed()

	// THE CONTROL: the same renewal on the queue's connection waits for the
	// lock, then behind the publish's bytes on the one connection.
	start := carried()
	returned = publish("kv.locked.shared")
	ok, err := shared.Renew(ctx, knownSeat(1), owner, onShared.Epoch, knownTTL)
	got := carried() - start
	switch {
	case err != nil && !errors.Is(err, coord.ErrUnavailable):
		t.Fatalf("the renewal over the queue's connection = %v, want an answer or unknown", err)
	case err == nil && ok && got < lockedBytes:
		t.Errorf("the control's renewal was answered with %d bytes of the %d-byte publish carried: "+
			"the case cannot tell a renewal behind the lock from one beside it", got, lockedBytes)
	}
	<-returned
	crossed()
}
