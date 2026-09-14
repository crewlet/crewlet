package engine_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A FRESH NODE DOES NOT ASK THE FLEET FOR A SNAPSHOT.
//
// # Why this has to be asserted rather than reasoned about
//
// The join is off a comparison between two numbers whose zero cases do not
// line up: a node that has applied nothing has a checkpoint of 0, and a stream
// nobody has written to reports its first sequence as ONE PAST its last —
// which is 1, not 0. Read carelessly, "the log starts at 1 and I am at 0"
// looks exactly like a node that has missed a record.
//
// It is not a silent bug when it goes wrong. Every boot spends
// [statelog.OfferWindow] asking a fleet that has nothing to donate, and the
// first thing an operator sees of this engine is a five-second pause on the
// only path a quickstart takes.
//
// # Why the assertion is the WIRE and not the clock
//
// This measured the wall clock of the whole boot against [statelog.OfferWindow]
// itself, and that quantity is `boot_work + (0 or 5s)` compared against 5s.
// Everything else engine.New does — two Turso estates, an embedded NATS
// server, three streams and their consumers, the epoch, the MCP registry — is
// bounded by nothing and is measured by the same stopwatch. On a two-core CI
// runner shared with this package's t.Parallel() siblings, boot_work ALONE
// reached 11.04s and the case failed while asking nobody anything; on an idle
// box the same code takes 0.8s. No threshold fixes that: one high enough not
// to flake is necessarily above OfferWindow, and above OfferWindow it can no
// longer tell a boot that spent the window from one that did not.
//
// The old comment defended the clock as "the symptom, rather than an internal
// flag that would go on being true while the pause moved somewhere else". The
// bound WAS OfferWindow, though, so the only pause it could ever distinguish
// from ordinary boot work was this one — it never was a general anti-pause
// guard, and giving up the clock loses no coverage that existed.
//
// What replaces it is the gesture itself. [statelog.CollectOffers] publishes on
// [statelog.SubjectOffer] before it blocks, so asking the fleet is a message on
// a subject any subscriber on the same connection can see. That is not a flag
// somewhere in the engine: it is the fleet-visible act, and a path that enters
// the window cannot avoid it. Nothing here is compared against elapsed time.
func TestAFreshNodeDoesNotSpendTheOfferWindowAtBoot(t *testing.T) {
	t.Parallel()

	back, err := openBackends(t, bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	}))
	if err != nil {
		t.Fatalf("open backends: %v", err)
	}

	// THE CONNECTION THE JOIN ITSELF WOULD USE, reached by the same
	// assertion [Engine.join] makes on it. Same-connection is what makes
	// this deterministic without borrowing a guarantee from production
	// code: the test's flush sits behind the engine's publish in one
	// client's outbound stream, so a PONG cannot overtake an echoed
	// message.
	//
	// The t.Fatal is load-bearing. A queue exposing no connection is
	// EXACTLY the case in which join returns without joining, so silence
	// below would prove nothing at all.
	broker, ok := back.Queue.(interface{ Conn() *nats.Conn })
	if !ok || broker.Conn() == nil {
		t.Fatal("this queue exposes no broker connection, which is also the case " +
			"in which Engine.join returns early — so the silence this asserts " +
			"would be the absence of a join path rather than a join not taken")
	}
	nc := broker.Conn()

	asked := make(chan *nats.Msg, 1)
	sub, err := nc.ChanSubscribe(statelog.SubjectOffer, asked)
	if err != nil {
		t.Fatalf("subscribe %s: %v", statelog.SubjectOffer, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	// The subscription must be registered at the server before the engine
	// can publish past it, or this reads as silence because nobody was
	// listening yet.
	//
	// Flush rather than a context with a deadline of this test's choosing:
	// FlushWithContext REQUIRES a deadline, so using it would mean inventing
	// a duration, and the whole defect being fixed here was a duration
	// invented for work it did not bound. nc.Flush carries the vendored
	// client's own FlushTimeout instead, and what it bounds is an in-process
	// PING/PONG rather than any amount of work — orders of magnitude of
	// headroom even on a saturated two-core box, and nothing this test
	// asserts is derived from how long it took.
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	e := newEngine(t, engine.Options{Backends: back})

	if e.Tracker() == nil || e.TrackerWriter() == nil {
		t.Fatal("a default company got no tracker")
	}
	// Everything the engine published has reached the server and been
	// echoed back to this subscription by the time this returns.
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush after boot: %v", err)
	}

	select {
	case msg := <-asked:
		t.Errorf("a fresh node published on %s at boot: it asked the fleet for a "+
			"snapshot of history nobody has written, and every such boot spends "+
			"%s waiting for donors that cannot exist (%d-byte request)",
			statelog.SubjectOffer, statelog.OfferWindow, len(msg.Data))
	default:
	}

	// THE CONTROL, and without it the case above passes on a dead
	// subscription — which is the shape this package has been bitten by
	// before. It proves the wire this test watches is the wire a join
	// would use, using the engine's own connection rather than a second
	// one.
	if err := nc.Publish(statelog.SubjectOffer, []byte("control")); err != nil {
		t.Fatalf("publish the control: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush the control: %v", err)
	}
	// NON-BLOCKING, and that is a guarantee rather than a gamble: the flush
	// above round-trips PING/PONG on the SAME connection this subscription
	// lives on, so the server's delivery of the echo is ordered ahead of the
	// PONG and the message is already in the channel by the time Flush
	// returns. Waiting on a deadline here would reintroduce a duration; and
	// waiting on t.Context() alone HANGS to the package timeout when the
	// subscription is dead, which is the failure this control exists to
	// report and the worst way to report it.
	select {
	case <-asked:
	default:
		t.Fatal("the control message never arrived though the flush that carries it " +
			"completed, so this subscription was never live and the silence above " +
			"proved nothing")
	}
}

// EVERY REGISTERED DOMAIN REPORTS A ROW, and a domain that does not gate seat
// admission still reports one.
//
// The two are different questions. Seat admission asks whether a seat's tools
// would answer wrongly, and the vector domain deliberately says "that is not
// mine to say" — a company whose embeddings are behind has a search that is
// less good, not a tracker that lies. The fleet view asks how far along this
// node's copies are, which is a fact about every one of them: a domain missing
// from that count is one an operator cannot watch fall behind.
func TestEveryDomainReportsItsOwnReplicationRow(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})

	rows := e.NativeStatus(t.Context())
	byName := map[string]engine.ReplicationStatus{}
	for _, row := range rows {
		if _, twice := byName[row.Name]; twice {
			t.Errorf("two rows named %q — the count would double one loop", row.Name)
		}
		byName[row.Name] = row
	}
	// THE TRACKER'S, whose health DOES gate admission.
	if _, held := byName[tracker.Domain{}.Name()]; !held {
		t.Errorf("no row for the tracker's own log; rows = %+v", rows)
	}
	// AND THE VECTOR DOMAIN'S, whose health does not.
	if _, held := byName["vectors"]; !held {
		t.Errorf("no row for the vector domain, so an operator cannot see the "+
			"company's embeddings fall behind; rows = %+v", rows)
	}
	// AND THE WIKI'S, which is still a projector rather than a domain —
	// the count is over REPLICATION LOOPS, not over one mechanism.
	if _, held := byName["pages"]; !held {
		t.Errorf("no row for the wiki's projection; rows = %+v", rows)
	}
	for _, row := range rows {
		switch {
		case row.Kind != "projection" && row.Kind != "domain":
			t.Errorf("%s reports kind %q, which is neither", row.Name, row.Kind)
		case !row.Ready && strings.TrimSpace(row.Detail) == "":
			t.Errorf("%s is not ready and says nothing about why — an operator "+
				"reading the fleet view has no next step", row.Name)
		case row.Ready && row.Detail != "":
			t.Errorf("%s is ready and still carries a detail (%q), which reads "+
				"as a warning on a healthy loop", row.Name, row.Detail)
		}
	}
}
