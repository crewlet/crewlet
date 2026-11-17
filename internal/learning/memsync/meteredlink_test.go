package memsync

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
	"github.com/crewlet/crewlet/internal/runtoken"
	seathost "github.com/crewlet/crewlet/internal/seat"
)

// A HYDRATION ACROSS A LEAF'S METERED LINK.
//
// The cases here stand a member serving leaves and a leaf joined to it as the
// engine stands them (internal/queue/jetstream), the leaf's data link crossing
// a first-in, first-out link (linktest.SharedFIFO) and the leaf's own meter
// reading it, and hydrate a seat through the leaf. The syncer's pace and
// window are scaled to the link's rate, and its client's request term to a
// second, so a stall is a second and a half and a download of half a MiB
// holds the link four times that.

const (
	// meteredRate is the pace of the link: 128 KiB a second each way.
	meteredRate = 128 << 10
	// meteredInflight is what the link lets each connection queue: half a
	// second of its rate, inside the ten seconds a member's leaf writes are
	// given.
	meteredInflight = 64 << 10
	// meteredPace is what a MiB takes to cross the link: the floor the
	// syncer plays at.
	meteredPace = time.Duration(int64(1<<20) * int64(time.Second) / meteredRate)
	// meteredWindow is the replay's window across the link.
	meteredWindow = 64 << 10
	// meteredTerm is the client's request term.
	meteredTerm = time.Second
	// downloadPieces of downloadPiece bytes are the download the hydration
	// waits behind: half a MiB, four seconds of the link.
	downloadPiece  = 128 << 10
	downloadPieces = 4
)

// meteredAuth is the fleet key the member and the leaf present to each other:
// a literal placeholder, never a key anybody deploys.
var meteredAuth = sync.OnceValue(func() js.FleetAuth {
	auth, err := js.FleetAuthFrom(runtoken.OneKey("memsync-leaf",
		"the memory sync's metered leaf cases' placeholder key material"))
	if err != nil {
		panic(err)
	}
	return auth
})

// meteredFleet is a member, a leaf joined to it across a first-in, first-out
// link, the leaf's meter of its data link, and a connection to each broker.
type meteredFleet struct {
	link   *linktest.Link
	meter  *js.LinkMeter
	member *nats.Conn
	leaf   *nats.Conn
}

// newMeteredFleet starts a metered fleet and waits until the leaf's clients
// reach the member's changelog.
func newMeteredFleet(t *testing.T) meteredFleet {
	t.Helper()
	ctx := t.Context()
	member, err := js.StartServer(ctx, js.Config{ServerName: "member",
		LeafHost: "127.0.0.1", LeafPort: js.AnyPort, StoreDir: brokertest.TempDir(t),
		StoreMaxBytes: brokertest.StoreMaxBytes, FleetAuth: meteredAuth()})
	if err != nil {
		t.Fatalf("start a member serving leaves: %v", err)
	}
	t.Cleanup(member.Shutdown)
	link := linktest.SharedFIFO(t, fmt.Sprintf("127.0.0.1:%d", member.LeafPort()), meteredRate,
		meteredInflight)
	leaf, err := js.StartServer(ctx, js.Config{ServerName: "leaf",
		LeafURLs: []string{"nats-leaf://" + link.Addr()}, FleetAuth: meteredAuth()})
	if err != nil {
		t.Fatalf("start the leaf across the link: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	client := func(name string, s *js.Server) *js.Queue {
		q, err := s.Client(ctx)
		if err != nil {
			t.Fatalf("a client of the %s: %v", name, err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
		return q
	}
	// THE MEMBER'S FIRST, which provisions the changelog.
	client("member", member)
	f := meteredFleet{link: link, meter: client("leaf", leaf).LinkMeter()}
	if f.meter == nil {
		t.Fatal("the leaf's queue has no meter of its link")
	}
	conn := func(name string, s *js.Server) *nats.Conn {
		c, err := s.Conn()
		if err != nil {
			t.Fatalf("connect to the %s: %v", name, err)
		}
		t.Cleanup(c.Close)
		return c
	}
	f.member, f.leaf = conn("member", member), conn("leaf", leaf)
	reachable(t, f.client(t))
	return f
}

// client is a JetStream client over the leaf's connection in the fleet's
// domain, its request term [meteredTerm].
func (f meteredFleet) client(t *testing.T) jetstream.JetStream {
	t.Helper()
	c, err := jetstream.NewWithDomain(f.leaf, jsapi.Domain, jetstream.WithDefaultTimeout(meteredTerm))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return c
}

// syncer is a syncer over db reaching the changelog through the leaf, across
// its link as the engine builds one, reading the leaf's meter, at the link's
// pace and window.
func (f meteredFleet) syncer(t *testing.T) *Syncer {
	t.Helper()
	s := mustNew(openStore(t), f.client(t), func(string) string { return seat.AgentID },
		Config{AcrossLink: true, LeaseHeartbeat: seathost.HeartbeatInterval, Clustered: true, Link: f.meter})
	s.pace, s.window = meteredPace, meteredWindow
	return s
}

// downloading subscribes the leaf's connection to what the member sends it,
// and waits until the member's sends reach it; the download is sent by the
// function it answers.
func (f meteredFleet) downloading(t *testing.T) (send func()) {
	t.Helper()
	if _, err := f.leaf.Subscribe("download.>", func(m *nats.Msg) {
		if m.Reply != "" {
			_ = m.Respond(nil)
		}
	}); err != nil {
		t.Fatalf("subscribe the leaf to the download: %v", err)
	}
	if err := f.leaf.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := f.member.Request("download.ready", nil, time.Second)
		if err == nil {
			break
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("the member's sends never reached the leaf: %v", err)
		case <-time.After(10 * time.Millisecond): //fixedwait:liveness
		}
	}
	piece := make([]byte, downloadPiece)
	if _, err := rand.Read(piece); err != nil {
		t.Fatal(err)
	}
	return func() {
		for range downloadPieces {
			if err := f.member.Publish("download.piece", piece); err != nil {
				t.Errorf("send a piece of the download: %v", err)
			}
		}
		// SENT ON before the pull: the member has queued every piece to
		// the leaf once its own connection's flush returns.
		if err := f.member.Flush(); err != nil {
			t.Errorf("flush the download: %v", err)
		}
	}
}

// diaryOn puts n notes of the seat on the changelog, through the member.
func (f meteredFleet) diaryOn(t *testing.T, n int) {
	t.Helper()
	diary := registered(t, "agent_diary")
	for i := range n {
		note := noteOfSize(t, fmt.Sprintf("n%02d", i), 512)
		publishOn(t, f.member, diary.subject(seat.AgentID, note.Values), note)
	}
}

// A HYDRATION BEHIND A DOWNLOAD ON ITS LINK IS NOT REFUSED.
//
// As the diary's replay sends its first pull, the member sends the leaf half
// a MiB — four seconds of the link — which the pull's rows queue behind on the
// link's one queue, as a satellite's replay queues behind the node's own
// download. The replay hears nothing from its copy for over twice its stall,
// while the leaf's link moves at its rate: it waits, and the hydration
// carries every note.
//
// Decided by bytes, never by a clock: the leaf's connection read at least the
// stall's worth of the link's rate between the pull's send and its first
// word, so the replay did go a stall without a word, and was not refused for
// it.
//
// Red where silence is judged by words alone, as before the meter: the seat
// is refused at the stall.
func TestAHydrationBehindADownloadOnItsLinkIsNotRefused(t *testing.T) {
	t.Parallel()
	f := newMeteredFleet(t)
	const notes = 10
	f.diaryOn(t, notes)
	send := f.downloading(t)
	s := f.syncer(t)
	spy := newSpyCopies(s)
	diary := tableFilter(seat.AgentID, "agent_diary")
	var (
		mu    sync.Mutex
		marks []uint64
	)
	mark := func() {
		mu.Lock()
		defer mu.Unlock()
		marks = append(marks, f.leaf.Stats().InBytes)
	}
	spy.beforeAsk = func(filter string, _ replayPulls) {
		if filter == diary {
			mark()
			send()
		}
	}
	spy.heard = func(filter string, _ jsapi.Event) {
		if filter == diary {
			mark()
		}
	}
	carried, err := s.Hydrate(t.Context(), seat.Handle)
	if err != nil || carried != notes {
		t.Fatalf("Hydrate = (%d, %v), want the %d notes: a replay behind a download on its link "+
			"moving at its rate is waited for", carried, err, notes)
	}
	stall := s.stall(s.pullBound())
	need := uint64(stall.Seconds() * meteredRate)
	mu.Lock()
	defer mu.Unlock()
	most := uint64(0)
	for i := 1; i < len(marks); i++ {
		most = max(most, marks[i]-marks[i-1])
	}
	if most < need {
		t.Errorf("the premise: the most the leaf read between the diary's pull and a word, or two "+
			"words, is %d bytes, want at least the %d its stall of %v carries at the link's rate",
			most, need, stall)
	}
}

// A HYDRATION ON A SILENT LINK IS REFUSED FOR ITS SILENCE.
//
// As the diary's replay sends its first pull, the leaf's data link is
// blackholed: the link reads nothing more of it, and carries nothing either
// way. The replay hears nothing and its link carries nothing, so the seat is
// refused for the link's silence, saying what the link carried, and never
// waits for the time cap a moving link would hold it to.
//
// Mutant: silence never judged by the link — the replay waits for its time
// cap, a minute and more, and is refused for it rather than for silence.
func TestAHydrationOnASilentLinkIsRefusedForItsSilence(t *testing.T) {
	t.Parallel()
	f := newMeteredFleet(t)
	f.diaryOn(t, 3)
	s := f.syncer(t)
	spy := newSpyCopies(s)
	diary := tableFilter(seat.AgentID, "agent_diary")
	t.Cleanup(func() { f.link.Mend(server.DEFAULT_GLOBAL_ACCOUNT) })
	spy.beforeAsk = func(filter string, _ replayPulls) {
		if filter == diary {
			f.link.Blackhole(server.DEFAULT_GLOBAL_ACCOUNT)
		}
	}
	carried, err := s.Hydrate(t.Context(), seat.Handle)
	if err == nil {
		t.Fatalf("Hydrate = (%d, nil) across a blackholed link, want the seat refused", carried)
	}
	if msg := err.Error(); !strings.Contains(msg, "nothing arrived for") ||
		!strings.Contains(msg, "the link carried nothing at the floor") {
		t.Errorf("the refusal = %v, want it refused for the replay's silence, saying its link "+
			"carried nothing", err)
	}
}
