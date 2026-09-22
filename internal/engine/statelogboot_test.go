package engine_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A FRESH NODE DOES NOT ASK THE FLEET FOR A SNAPSHOT.
//
// # The comparison, and its three zero cases
//
// A node asks the fleet for a snapshot when its checkpoint on some domain is
// below what that domain's log still holds, and what the log holds is read off
// the stream's first sequence — which is "zero" in three different ways:
//
//   - a stream NOBODY HAS WRITTEN TO reports a first sequence of 0, and a last
//     of 0;
//   - a stream that HAS BEEN WRITTEN TO reports 1 or more, and exactly 1 until
//     something trims it;
//   - a stream written to and then PURGED OF EVERYTHING reports one past its
//     last — as empty as the first case, and nowhere near 0.
//
// A node that has applied nothing has a checkpoint of 0, and the record it
// needs next is 1. It is behind only when the log's first sequence is PAST
// that next record, and each nearby reading of the comparison asks on one of
// the cases above: a join taken whenever the first sequence is above the
// checkpoint asks on the second — "the log starts at 1 and I am at 0" read as a
// missed record — and one taken at equality asks on the first, which is every
// brand-new company there is.
//
// This case is the brand-new company, whose logs have never been written;
// [TestANodeWithTheWholeLogAheadOfItDoesNotAsk] is the second case. Neither is
// a silent bug when it goes wrong, and where it costs is a FLEET: every member
// runs a donor, a donor with nothing usable stays silent, and so a node that
// asks wrongly spends the whole [statelog.OfferWindow] refusing every read and
// write — on every boot of every node a company adds. A node alone pays almost
// nothing, which is exactly why the wall clock could not see it; see below.
//
// # Why the ASK is what is observed, and not the wall clock
//
// This case used to bound the whole boot's wall clock under the window, and
// that was wrong in both directions. It failed in CI at 5.37s with the join
// correctly declined — no `statelog_below_the_floor` line in the log — because
// everything the boot does before the join is inside that interval too: the
// embedded broker, every coordination bucket, two migrated databases and three
// state-log streams, under the race detector on a contended four-core runner.
// And it could NOT fail on its own staging: a lone node has nothing listening
// on [statelog.SubjectOffer] when it joins, since its own donor starts later,
// so the broker answers an ask with "no responders" in a couple of
// milliseconds — and a join broken to ask on every boot still passed.
//
// So the assertion is the ask itself, heard by a tap on the one subject every
// snapshot ask crosses. A donor listens on nothing else, so the tap hears an
// ask whichever code path sends it; no load on the machine can produce one or
// hide one; and the tap is a SILENT RESPONDER, which is exactly what a fleet
// whose donors hold nothing looks like — an ask costs the whole window here,
// as it would in production, rather than the broker's shortcut. And the
// silence it reports is evidence only because
// [TestANodeTheFleetHasMovedPastAsksForASnapshotAtBoot] shows the same tap
// hearing an ask when the boot makes one.
func TestAFreshNodeDoesNotAskTheFleetForASnapshot(t *testing.T) {
	t.Parallel()
	boot, company, backends := joinBackends(t)
	asks := tapSnapshotAsks(t, backends)

	e := newEngine(t, engine.Options{Bootstrap: boot, Company: company, Backends: backends})
	if e.Tracker() == nil || e.TrackerWriter() == nil {
		t.Fatal("a default company got no tracker, so no state log ran and " +
			"there was no join to decline")
	}
	if got := asks.drain(t); len(got) != 0 {
		t.Errorf("a fresh node asked the fleet for a snapshot %d time(s) during "+
			"its boot (first ask needs %v) — a node whose logs have never been "+
			"written spent %s waiting on history nobody has written",
			len(got), got[0].Need, statelog.OfferWindow)
	}
}

// AND NOR DOES A NODE WITH THE WHOLE LOG AHEAD OF IT.
//
// The second zero case, and the one a reading of the comparison can actually
// get wrong: a log that has been written and never trimmed starts at 1, and a
// node at checkpoint 0 against it has missed nothing — its next record is the
// log's first. That node is a machine added to a company whose trim has not
// run yet, or one whose replicated estate was lost before it did. A brand-new
// company cannot stand in for it, because its logs are the first case, at 0:
// a join taken whenever the first sequence is above the checkpoint declines
// there and asks here.
func TestANodeWithTheWholeLogAheadOfItDoesNotAsk(t *testing.T) {
	t.Parallel()
	boot, company, backends := joinBackends(t)

	// A PEER HAS ALREADY WRITTEN to the tracker's log, so it starts at 1
	// and nothing has trimmed it. A barrier is the record any build reads
	// and that writes no rows, so what it changes is the log's bounds and
	// nothing this node's rows could say.
	q, ok := backends.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T", backends.Queue)
	}
	spec := tracker.Domain{}.Stream()
	if err := q.EnsureDomainStream(t.Context(), jetstream.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects,
		// AT THE FLOOR A BOOT SIZES A LOG TO, not at the ceiling the
		// domain declares. A ceiling is a reservation the broker grants
		// in full, and a stream that already exists keeps its own: the
		// declared sixteen gibibytes is nearly all of a test machine's
		// broker (three quarters of the volume's free space), so the
		// boot that follows had a few hundred megabytes left and was
		// refused the vectors log's reservation — a failure of this
		// staging, not of the join. The floor is the least the engine
		// itself creates a log at, so this is a log a peer could have
		// made on a small disk.
		MaxBytes:      engine.MinDomainCeiling,
		MaxPerSubject: spec.MaxPerSubject, MaxAge: spec.MaxAge,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the tracker's log: %v", err)
	}
	peerLog, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the tracker's log: %v", err)
	}
	body, err := tracker.EncodeBarrier(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind},
		Scope:   statelog.ScopeSet{Paths: []string{statelog.BarrierScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	if seq, _, err := peerLog.Append(t.Context(),
		spec.SubjectPrefix+"."+statelog.BarrierKind, "", nil, body); err != nil || seq != 1 {
		t.Fatalf("a peer's first record landed at (%d, %v), want 1", seq, err)
	}
	asks := tapSnapshotAsks(t, backends)

	newEngine(t, engine.Options{Bootstrap: boot, Company: company, Backends: backends})
	if got := asks.drain(t); len(got) != 0 {
		t.Errorf("a node at checkpoint 0 against a log starting at 1 asked the "+
			"fleet for a snapshot %d time(s) (first ask needs %v) — it has "+
			"missed nothing, the whole log is ahead of it", len(got), got[0].Need)
	}
}

// AND A NODE THE FLEET HAS MOVED PAST DOES ASK — which is what makes the
// silence of the two cases above mean anything.
//
// Without it the tap could be deaf and both would pass: the subject renamed on
// one side, the ask moved to a connection or a transport the tap cannot see,
// or a subscription the server never registered. So this is the same tap on
// the same boot with one fact changed to make the node genuinely behind, and
// the cheapest such fact: a node with no checkpoint joining a fleet whose trim
// has published a floor at generation 1. The records before a re-anchor are
// not on the log to be replayed, so the node must adopt — and, the tap
// answering nothing, it comes up on what it has one [statelog.OfferWindow]
// later.
//
// It is the TAP's control rather than the comparison's: a node with no
// checkpoint in a fleet past generation 0 is behind before any sequence is
// compared. The comparison's own positive case, a checkpoint the trim has
// purged past, is TestANodeBelowTheFloorAdoptsWhileRunning's.
func TestANodeTheFleetHasMovedPastAsksForASnapshotAtBoot(t *testing.T) {
	t.Parallel()
	boot, company, backends := joinBackends(t)
	name := tracker.Domain{}.Name()
	if err := backends.Fleet.PutFloor(t.Context(), coord.TrimFloor{
		Domain: name, Generation: 1, BlockedBy: "applied",
		At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish the fleet's floor: %v", err)
	}
	asks := tapSnapshotAsks(t, backends)

	newEngine(t, engine.Options{Bootstrap: boot, Company: company, Backends: backends})
	got := asks.drain(t)
	if len(got) == 0 {
		t.Fatal("a node with no checkpoint in a fleet at generation 1 booted " +
			"without a snapshot ask reaching the tap — so the silence the two " +
			"cases before this assert is the silence of a tap that cannot hear")
	}
	if gen := got[0].Generations[name]; gen != 1 {
		t.Errorf("the ask names generation %d for %s, want the fleet's 1", gen, name)
	}
}

// joinBackends opens the estate a boot's join runs against, so a test can
// reach the broker before [engine.New] does.
//
// SUPPLIED rather than engine-owned only for that reach: the join takes its
// connection from [engine.Backends.Queue] either way, so a boot on these is
// the boot `crewlet run` makes.
func joinBackends(t *testing.T) (*config.Bootstrap, *config.Company, *engine.Backends) {
	t.Helper()
	boot := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	company := parsedCompany(t, companyDoc)
	backends, err := engine.OpenBackends(t.Context(), boot, company)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { backends.Close(context.Background()) })
	return boot, company, backends
}

// snapshotTap is a subscription on the subject every snapshot ask crosses,
// held on a connection of its OWN — which is where a donor on another node
// hears the same request, rather than on the connection the asker publishes
// from.
type snapshotTap struct {
	nc  *nats.Conn
	sub *nats.Subscription
}

// tapSnapshotAsks attaches a [snapshotTap] to the broker behind backends, and
// returns only once the server holds its interest.
func tapSnapshotAsks(t *testing.T, backends *engine.Backends) *snapshotTap {
	t.Helper()
	q, ok := backends.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, and a join rides the JetStream broker's own "+
			"connection", backends.Queue)
	}
	nc, err := q.DialOwned()
	if err != nil {
		t.Fatalf("dial the broker: %v", err)
	}
	t.Cleanup(nc.Close)
	sub, err := nc.SubscribeSync(statelog.SubjectOffer)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", statelog.SubjectOffer, err)
	}
	// FLUSHED BEFORE THE BOOT, so the server holds the interest before
	// anything can ask: a SUB still in this connection's write buffer
	// misses an ask published meanwhile, and the case passes by being deaf.
	if err := nc.Flush(); err != nil {
		t.Fatalf("register the subscription: %v", err)
	}
	return &snapshotTap{nc: nc, sub: sub}
}

// drain returns every ask published before it was called, without waiting on
// a timer.
//
// TWO FLUSHES ARE THE BARRIER, the asker's and this one.
// [statelog.CollectOffers] flushes its request before it starts collecting,
// and a flush returns only once the server has answered a PING sent AFTER the
// publish — so by the time [engine.New] returns, the server has processed any
// ask the boot made and routed it to this tap, queued on this connection ahead
// of anything the server sends it later. The PONG answering the flush below is
// such a thing, and a connection's reads are in order, so once it is back
// every ask is already pending on the subscription. A timeout would be a guess
// at how long routing takes, and a guess too short passes a case by not having
// waited.
func (s *snapshotTap) drain(t *testing.T) []statelog.OfferRequest {
	t.Helper()
	if err := s.nc.Flush(); err != nil {
		t.Fatalf("flush the tap: %v", err)
	}
	pending, _, err := s.sub.Pending()
	if err != nil {
		t.Fatalf("read the tap: %v", err)
	}
	out := make([]statelog.OfferRequest, 0, pending)
	for range pending {
		// ALREADY DELIVERED, so this returns at once; the bound only
		// turns a count that lied into a failure rather than a hang.
		msg, err := s.sub.NextMsg(time.Second)
		if err != nil {
			t.Fatalf("read a pending ask: %v", err)
		}
		var req statelog.OfferRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			t.Fatalf("an ask on %s is not an offer request: %v", statelog.SubjectOffer, err)
		}
		out = append(out, req)
	}
	return out
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
