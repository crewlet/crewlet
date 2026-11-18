package natsobj_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A REQUEST BEHIND AN UPLOAD AND A DOWNLOAD IS ANSWERED INSIDE ITS TERM.
//
// An upload, a download and a memory replay's pulls (as memsync pulls, a
// window a pull) run across a leaf link at the floor each way, inside the
// link's ONE window, and a request is sent to the member as each of three
// upload pieces leaves the leaf. DECIDED BY ORDER: each side sees, in the
// order the link delivered them, what arrived after a marker it published to
// itself as the request was sent and before the request or its answer; that
// must cross the floor within what the term leaves after its round trip.
//
// Mutation: let a get, a put or a replay pull out without taking the window,
// or give each direction a window of its own, and this goes red.
func TestARequestBehindAnUploadAndADownloadIsAnsweredInsideItsTerm(t *testing.T) {
	t.Parallel()
	across := kvtest.SlowLeaf(t, floorRate)
	memberJS := client(t, across.MemberBroker)
	leafJS := client(t, across.LeafBroker)
	// THE MEMBER'S FIRST, which creates the bucket, and the object to
	// download, over the member's own client.
	// RANDOM BYTES EVERY WAY: a leaf's link compresses, and a patterned
	// transfer would cross it at a fraction of the bytes the case counts.
	seeded := randomBytes(t, 6*natsobj.MessageBytes)
	if err := open(t, memberJS).Put(t.Context(), "download", bytes.NewReader(seeded), objstore.PutMeta{}); err != nil {
		t.Fatalf("seed the download: %v", err)
	}
	window := jsapi.NewLinkWindow()
	b := openWith(t, leafJS, natsobj.Config{MaxBytes: testCeiling, Replicas: 1, AcrossLink: true,
		Window: window})
	rows := replayRows(t, memberJS, leafJS)

	pieces := "$O." + natsobj.Bucket + ".C.>"
	leaving := watchInOrder(t, memberConn(t, across.LeafBroker), pieces)
	up := watchInOrder(t, memberConn(t, across.MemberBroker), pieces, "probe", "mark.up.*")
	down := watchInOrder(t, memberConn(t, across.LeafBroker), "_INBOX.>", "mark.down.*")
	responder := memberConn(t, across.MemberBroker)
	answer := func(m *nats.Msg) { _ = m.Respond(append([]byte("answer:"), m.Data...)) }
	if _, err := responder.Subscribe("probe", answer); err != nil {
		t.Fatal(err)
	}
	if err := responder.Flush(); err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan struct{}, 3)
	upload := randomBytes(t, 8*natsobj.MessageBytes)
	go func() {
		defer func() { done <- struct{}{} }()
		_ = b.Put(ctx, "upload", bytes.NewReader(upload), objstore.PutMeta{})
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		// A LIVENESS BOUND, which a read must carry: the case ends it.
		read, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if r, err := b.Get(read, "download", 0, -1); err == nil {
			_, _ = io.Copy(io.Discard, r)
			_ = r.Close()
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		rows.pull(ctx, window)
	}()
	defer func() {
		stop()
		for range 3 {
			<-done
		}
	}()

	// ALL THREE UNDER WAY: a piece and a row crossed toward the leaf.
	down.await(t, func(m *nats.Msg) bool { return strings.HasPrefix(m.Subject, "$O.") })
	down.await(t, func(m *nats.Msg) bool { return strings.HasPrefix(m.Subject, "replay.") })

	marker := func(srv *js.Server, subject string) {
		nc := memberConn(t, srv)
		if err := nc.Publish(subject, nil); err != nil {
			t.Fatal(err)
		}
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	// SENT AS AN UPLOAD'S PIECE LEAVES THE LEAF, three times: the request
	// queues behind that piece on its way out, and its answer behind whatever
	// the node has coming back, the worst instant for a request behind both.
	for i, seen := 0, len(leaving.all()); i < 3; i, seen = i+1, len(leaving.all()) {
		leaving.awaitPast(t, seen)
		markUp, markDown, q := fmt.Sprintf("mark.up.%d", i), fmt.Sprintf("mark.down.%d", i), fmt.Sprintf("probe-%d", i)
		marker(across.MemberBroker, markUp)
		marker(across.LeafBroker, markDown)
		// A LIVENESS BOUND, deciding nothing: the claim is the order below.
		if _, err := memberConn(t, across.LeafBroker).Request("probe", []byte(q), objstore.Stall); err != nil {
			t.Fatalf("a request sent among an upload, a download and a replay on a link at the floor was "+
				"not answered within %v: %v", objstore.Stall, err)
		}
		upAhead := up.between(t, markUp, func(m *nats.Msg) bool { return m.Subject == "probe" && string(m.Data) == q })
		downAhead := down.between(t, markDown, func(m *nats.Msg) bool { return string(m.Data) == "answer:"+q })
		ahead := upAhead + downAhead
		c := time.Duration(int64(ahead) * int64(objstore.MiBPace) / objstore.MiB)
		t.Logf("request %d: %d bytes ahead, %d toward the member and %d back, %v at the floor", i, ahead,
			upAhead, downAhead, c)
		if c > requestBudget {
			t.Errorf("request %d: %d bytes crossed the link ahead of it and its answer (%d toward the member, "+
				"%d back), %v at the floor: answered past the %v its %v term leaves after its own round trip",
				i, ahead, upAhead, downAhead, c, requestBudget, jsapi.RequestTerm)
		}
	}
}

// A DOWNLOAD ACROSS A LINK HOLDS ONE PIECE AT A TIME.
//
// Across a link a get asks every pull itself, one piece each, bounded at that
// piece's bytes, and asks the next only once the last has ended: so the link
// carries at most one of its pieces at a time, which is what the link's
// window counts for it. Seen on the leaf, in the order its broker handled
// them, every pull the get sends and every piece the member delivers: never
// two pieces between two pulls, never a pull of more than a piece.
//
// Mutation: read across a link with a hand of four pieces, as a member's
// client does, and this goes red.
func TestADownloadAcrossALinkHoldsOnePieceAtATime(t *testing.T) {
	t.Parallel()
	across := kvtest.SlowLeaf(t, 32<<20)
	data := patterned(5*natsobj.MessageBytes + 1000)
	if err := open(t, client(t, across.MemberBroker)).Put(t.Context(), "pieces", bytes.NewReader(data),
		objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	b := openWith(t, client(t, across.LeafBroker), acrossLink())
	seen := watchInOrder(t, memberConn(t, across.LeafBroker), "_INBOX.>",
		"$JS."+jsapi.Domain+".API.CONSUMER.MSG.NEXT."+natsobj.Stream+".>")
	readBack(t, b, "pieces", data)

	pulls, pieces, between := 0, 0, 0
	for _, m := range seen.all() {
		switch {
		case strings.Contains(m.Subject, "CONSUMER.MSG.NEXT"):
			var req server.JSApiConsumerGetNextRequest
			if err := json.Unmarshal(m.Data, &req); err != nil {
				t.Fatalf("a pull's request does not decode: %v", err)
			}
			if req.Batch != 1 || req.MaxBytes > natsobj.PieceBound(natsobj.MessageBytes) {
				t.Fatalf("a get across a link asked a pull of %d messages and %d bytes, want one piece's "+
					"%d at most", req.Batch, req.MaxBytes, natsobj.PieceBound(natsobj.MessageBytes))
			}
			pulls++
			between = 0
		case strings.HasPrefix(m.Subject, "$O."+natsobj.Bucket+".C."):
			pieces++
			if between++; between > 1 {
				t.Fatalf("two pieces crossed the link toward the leaf on one pull")
			}
		}
	}
	if pieces != 6 || pulls < pieces {
		t.Fatalf("the get read %d pieces on %d pulls, want its 6 pieces a pull each", pieces, pulls)
	}
}

// A WALK ACROSS A LINK ENDS AT ITS CONSUMER'S END.
//
// A ranged get walks its object's headers to its offset, and a listing walks
// the metadata, on pulls of up to a walk's batch bounded at the whole window.
// A pull the server holds once its consumer has nothing more holds the window
// to the pull's expiry: every ranged read and listing across a link, however
// fast, was answered seconds late, every transfer on the link held behind it.
// So every pull ends at its consumer's end. Seen on the leaf: every pull asks
// no_wait, and none ends at its expiry (408), only at the consumer's end (404).
//
// Mutation: ask the pulls without no_wait, and the walks end at 408.
func TestAWalkAcrossALinkEndsAtItsConsumersEnd(t *testing.T) {
	t.Parallel()
	across := kvtest.SlowLeaf(t, 32<<20)
	data := patterned(3*natsobj.MessageBytes + 1000)
	if err := open(t, client(t, across.MemberBroker)).Put(t.Context(), "walked", bytes.NewReader(data),
		objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	b := openWith(t, client(t, across.LeafBroker), acrossLink())
	seen := watchInOrder(t, memberConn(t, across.LeafBroker), "_INBOX.>",
		"$JS."+jsapi.Domain+".API.CONSUMER.MSG.NEXT."+natsobj.Stream+".>")
	off := int64(2*natsobj.MessageBytes + 10)
	r, err := b.Get(deadline(t), "walked", off, -1)
	if err != nil {
		t.Fatalf("a ranged get across a link: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, data[off:]) {
		t.Fatalf("a ranged get from %d across a link read %d bytes, %v; want the %d after it", off, len(got),
			err, len(data)-int(off))
	}
	listed := 0
	if err := b.List(deadline(t), func(objstore.Info) error { listed++; return nil }); err != nil || listed != 1 {
		t.Fatalf("a listing across a link named %d objects, %v; want the one put", listed, err)
	}

	walks, ends := 0, 0
	for _, m := range seen.all() {
		switch {
		case strings.Contains(m.Subject, "CONSUMER.MSG.NEXT"):
			var req server.JSApiConsumerGetNextRequest
			if err := json.Unmarshal(m.Data, &req); err != nil {
				t.Fatalf("a pull's request does not decode: %v", err)
			}
			if req.Batch > 1 {
				walks++
			}
			if !req.NoWait {
				t.Errorf("a pull of %d messages across a link was asked to wait out its expiry at its "+
					"consumer's end", req.Batch)
			}
		case m.Header.Get("Status") == "408":
			t.Errorf("a pull across a link ended at its expiry, holding its take of the window that long")
		case m.Header.Get("Status") == "404":
			ends++
		}
	}
	if walks < 2 || ends < 2 {
		t.Fatalf("the ranged get and the listing asked %d walks' pulls, %d ended at their consumer's end; "+
			"want both to walk and end there", walks, ends)
	}
}

// movingLink is a link meter that says the link moves while the case holds it
// moving, and has read nothing: the link's other traffic, as the case stages
// it, without a meter's socket.
type movingLink struct{ moving atomic.Bool }

func (l *movingLink) Moving(time.Duration) bool { return l.moving.Load() }
func (l *movingLink) Carried() (uint64, uint64) { return 0, 0 }

// A TRANSFER BEHIND A MOVING LINK IS NOT STALLED.
//
// Across a metered link a put's acknowledgement and a get's next piece queue
// behind whatever else the link is carrying, and a message at the transport's
// bound ahead of them holds them past their stall at the floor. The link is
// moving meanwhile, so neither is stalled: a transfer's silence is judged by
// the link's, never by its own acknowledgements alone. Here the stall is a
// second and the link carries three seconds of a bulk message ahead of each.
//
// Mutation: stall a put, or a get, on its own acknowledgements or pieces
// whatever its link does, and this goes red.
func TestATransferBehindAMovingLinkIsNotStalled(t *testing.T) {
	t.Parallel()
	const rate = 1 << 20
	across := kvtest.SlowLeaf(t, rate)
	data := patterned(2 * natsobj.MessageBytes)
	if err := open(t, client(t, across.MemberBroker)).Put(t.Context(), "behind", bytes.NewReader(data),
		objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	link := &movingLink{}
	b := openWith(t, client(t, across.LeafBroker), natsobj.Config{MaxBytes: testCeiling, Replicas: 1,
		AcrossLink: true, Window: jsapi.NewLinkWindow(), Link: link})
	natsobj.SetStall(b, time.Second)
	bulk := make([]byte, 3*rate)
	if _, err := rand.Read(bulk); err != nil {
		t.Fatal(err)
	}
	// A BULK MESSAGE EACH WAY, with a subscriber at the far end so the link
	// carries it, flushed so it is on the link ahead of the transfer.
	send := func(from, to *js.Server, subject string) {
		t.Helper()
		sub := memberConn(t, to)
		if _, err := sub.Subscribe(subject, func(*nats.Msg) {}); err != nil {
			t.Fatal(err)
		}
		if err := sub.Flush(); err != nil {
			t.Fatal(err)
		}
		pub := memberConn(t, from)
		if err := pub.Publish(subject, bulk); err != nil {
			t.Fatal(err)
		}
		if err := pub.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	link.moving.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	send(across.LeafBroker, across.MemberBroker, "bulk.up")
	if err := b.Put(ctx, "put-behind", bytes.NewReader(patterned(1000)), objstore.PutMeta{}); err != nil {
		t.Fatalf("a put behind a bulk message on a moving link = %v; want it stored", err)
	}

	r, err := b.Get(ctx, "behind", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first := make([]byte, natsobj.MessageBytes)
	if _, err := io.ReadFull(r, first); err != nil {
		t.Fatal(err)
	}
	send(across.MemberBroker, across.LeafBroker, "bulk.down")
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("a get whose next piece queued behind a bulk message on a moving link = %v; want it read", err)
	}
	if got := append(first, rest...); !bytes.Equal(got, data) {
		t.Fatalf("the get read %d bytes, want the %d stored", len(got), len(data))
	}
}

// A LEADER'S READ OF A WHOLE PIECE ACROSS A LINK IS LET OUT ALONE.
//
// A read that asks the stream's leader what follows on an object's pieces is
// answered with a whole piece, more than the link's window crosses in a
// request's term. So it takes the window alone: it waits while anything else
// holds any of it, even a byte, and holds it whole until its answer is in. On
// a member's own client, which crosses no link, it takes nothing.
//
// Mutation: ask the leader without taking the window, and this goes red.
func TestCaughtUpAcrossALinkIsLetOutAlone(t *testing.T) {
	t.Parallel()
	across := kvtest.SlowLeaf(t, 32<<20)
	member := open(t, client(t, across.MemberBroker))
	if err := member.Put(t.Context(), "piece", bytes.NewReader(patterned(1000)), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	pieces := natsobj.PieceSubject("") + ">"
	window := jsapi.NewLinkWindow()
	b := openWith(t, client(t, across.LeafBroker), natsobj.Config{MaxBytes: testCeiling, Replicas: 1,
		AcrossLink: true, Window: window})
	if err := window.Take(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := natsobj.CaughtUp(t.Context(), b, pieces, 0)
		done <- err
	}()
	for {
		if _, waiting := window.Held(); waiting == 1 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("a leader's read of a piece across a link was answered (%v) beside a byte the "+
				"window held", err)
		default:
			runtime.Gosched()
		}
	}
	window.Give(1)
	if err := <-done; err != nil {
		t.Fatalf("a leader's read of a piece let out alone: %v", err)
	}
	if held, waiting := window.Held(); held != 0 || waiting != 0 {
		t.Fatalf("the read done, the window holds %d bytes with %d waiting", held, waiting)
	}

	whole := member.Window()
	if err := whole.Take(t.Context(), whole.Size()); err != nil {
		t.Fatal(err)
	}
	defer whole.Give(whole.Size())
	if _, err := natsobj.CaughtUp(t.Context(), member, pieces, 0); err != nil {
		t.Fatalf("a leader's read on a member's own client, its puts' window full: %v", err)
	}
}

// randomBytes is n bytes no link's compression shrinks.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// memberConn is a connection of srv's own, for a watcher, a responder or a
// publisher of a case's.
func memberConn(t *testing.T, srv *js.Server) *nats.Conn {
	t.Helper()
	nc, err := srv.Conn()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// ordered is what one connection's subscriptions saw, in the order its
// server sent it: every subscription feeds one channel, which nats.go fills
// from the connection's one read loop.
type ordered struct {
	msgs    chan *nats.Msg
	arrived chan struct{}
	seen    atomic.Pointer[[]*nats.Msg]
}

// watchInOrder subscribes nc to subjects, every one into one ordered record.
func watchInOrder(t *testing.T, nc *nats.Conn, subjects ...string) *ordered {
	t.Helper()
	w := &ordered{msgs: make(chan *nats.Msg, 1<<14), arrived: make(chan struct{}, 1)}
	w.seen.Store(&[]*nats.Msg{})
	for _, subject := range subjects {
		sub, err := nc.ChanSubscribe(subject, w.msgs)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case m := <-w.msgs:
				next := append(append([]*nats.Msg(nil), *w.seen.Load()...), m)
				w.seen.Store(&next)
				select {
				case w.arrived <- struct{}{}:
				default:
				}
			case <-stop:
				return
			}
		}
	}()
	return w
}

// all is every message w saw so far, in order.
func (w *ordered) all() []*nats.Msg { return *w.seen.Load() }

// await returns once w has seen a message match says is one.
func (w *ordered) await(t *testing.T, match func(*nats.Msg) bool) {
	t.Helper()
	deadline := time.After(time.Minute) //fixedwait:liveness
	for {
		for _, m := range w.all() {
			if match(m) {
				return
			}
		}
		select {
		case <-w.arrived:
		case <-deadline:
			t.Fatalf("the watched transfer never crossed the link; of %d messages seen, the subjects began %v",
				len(w.all()), w.kinds())
		}
	}
}

// kinds is the first two tokens of each subject w saw, and its reply's.
func (w *ordered) kinds() map[string]int {
	k := map[string]int{}
	for _, m := range w.all() {
		k[prefix(m.Subject)+" <- "+prefix(m.Reply)]++
	}
	return k
}

// prefix is a subject's first two tokens.
func prefix(subject string) string {
	tokens := strings.SplitN(subject, ".", 3)
	return strings.Join(tokens[:min(2, len(tokens))], ".")
}

// awaitPast returns once w has seen more than n messages.
func (w *ordered) awaitPast(t *testing.T, n int) {
	t.Helper()
	w.await(t, func(*nats.Msg) bool { return len(w.all()) > n })
}

// between is the bytes of everything w saw after the message on marker and
// before the first that end says ends the span.
func (w *ordered) between(t *testing.T, marker string, end func(*nats.Msg) bool) int {
	t.Helper()
	w.await(t, end)
	total, in := 0, false
	for _, m := range w.all() {
		switch {
		case m.Subject == marker:
			in = true
		case in && end(m):
			return total
		case in:
			total += m.Size()
		}
	}
	t.Fatalf("the marker %s was never seen before the span's end", marker)
	return 0
}

// replayStand is a stream of rows a case pulls as a memory replay does.
type replayStand struct {
	nc   *nats.Conn
	api  jsapi.API
	cons jetstream.Consumer
	// rows is how many the stream holds, and taken how many the pulls
	// delivered: a replay ends once it has them all.
	rows, taken int
}

// replayRows makes a stream of rows on the member, each 16 KiB, and a
// consumer of it the leaf reads: what a seat's hydration pulls across the
// link.
func replayRows(t *testing.T, member, leaf jetstream.JetStream) *replayStand {
	t.Helper()
	ctx := t.Context()
	if _, err := member.CreateStream(ctx, jetstream.StreamConfig{Name: "REPLAY", Subjects: []string{"replay.>"},
		Storage: jetstream.MemoryStorage}); err != nil {
		t.Fatal(err)
	}
	const rows = 64
	row := randomBytes(t, 16<<10)
	for range rows {
		if _, err := member.Publish(ctx, "replay.row", row); err != nil {
			t.Fatal(err)
		}
	}
	// KEPT while its pulls wait their turn in the window, past the five
	// idle seconds the broker otherwise reaps an ephemeral consumer at.
	cons, err := leaf.CreateConsumer(ctx, "REPLAY", jetstream.ConsumerConfig{
		AckPolicy: jetstream.AckNonePolicy, InactiveThreshold: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	api, err := jsapi.Of(leaf)
	if err != nil {
		t.Fatal(err)
	}
	return &replayStand{nc: leaf.Conn(), api: api, cons: cons, rows: rows}
}

// pull reads the rows as a memory replay across a link does, until it has
// them all or ctx ends: one pull at a time, each taking the window whole
// before it is asked and giving it back once it ends.
func (s *replayStand) pull(ctx context.Context, window *jsapi.Window) {
	p, err := jsapi.NewPulls(s.nc, s.api, "REPLAY", s.cons, jsprovision.Clustered(false).Timing())
	if err != nil {
		return
	}
	defer p.Stop()
	for ctx.Err() == nil && s.taken < s.rows {
		if window.Take(ctx, window.Size()) != nil {
			return
		}
		err := s.one(ctx, p, window.Size())
		window.Give(window.Size())
		if err != nil {
			return
		}
	}
}

// one asks a pull of bound bytes and reads it until it ends.
func (s *replayStand) one(ctx context.Context, p *jsapi.Pulls, bound int) error {
	if err := p.Ask(256, bound, 30*time.Second, 5*time.Second); err != nil {
		return err
	}
	for {
		ev, err := p.Next(ctx)
		switch {
		case err != nil:
			return err
		case ev.Kind == jsapi.Refused:
			return errors.New(ev.Description)
		}
		if ev.Kind == jsapi.Delivered {
			s.taken++
		}
		if ev.Ended {
			return nil
		}
	}
}
