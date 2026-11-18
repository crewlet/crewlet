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
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A FRESH NODE DOES NOT ASK THE FLEET FOR A SNAPSHOT.
//
// A log's first sequence is 0 on a stream never written, 1 on one written and
// untrimmed, and last+1 on one purged. A node at checkpoint 0 needs record 1, so
// it is behind only when the first sequence is PAST 1: asking whenever it
// exceeds the checkpoint asks on the second case
// ([TestANodeWithTheWholeLogAheadOfItDoesNotAsk]); asking at equality asks on
// the first, every brand-new company, which is this case. On a fleet a wrong
// ask costs each boot a whole [statelog.OfferWindow] of refused reads and writes.
// The ask is asserted, not the wall clock (a lone node's ask fails fast on "no
// responders"), by a silent tap on [statelog.SubjectOffer], which
// [TestANodeTheFleetHasMovedPastAsksForASnapshotAtBoot] proves can hear.
func TestAFreshNodeDoesNotAskTheFleetForASnapshot(t *testing.T) {
	t.Parallel()
	boot, company, backends := joinBackends(t)
	asks := tapSnapshotAsks(t, backends)

	e := newEngine(t, engine.Options{Bootstrap: boot, Company: company, Backends: backends})
	if len(e.StateLogStatus(t.Context())) == 0 {
		t.Fatal("the node runs no state log, so there was no join to decline")
	}
	if got := asks.drain(t); len(got) != 0 {
		t.Errorf("a fresh node asked the fleet for a snapshot %d time(s) during "+
			"its boot (first ask needs %v) — a node whose logs have never been "+
			"written spent %s waiting on history nobody has written",
			len(got), got[0].Need, statelog.OfferWindow)
	}
}

// AND NOR DOES A NODE WITH THE WHOLE LOG AHEAD OF IT: a log written and never
// trimmed starts at 1, and a node at checkpoint 0 has missed nothing. That is a
// machine added before the trim ran, or one whose replicated estate was lost.
// A brand-new company cannot stand in for it: its logs start at 0, where a join
// taken whenever the first sequence exceeds the checkpoint still declines.
func TestANodeWithTheWholeLogAheadOfItDoesNotAsk(t *testing.T) {
	t.Parallel()
	boot, company, backends := joinBackends(t)

	// A PEER HAS ALREADY WRITTEN to the tracker's log, untrimmed, so it
	// starts at 1. A barrier is read by any build and writes no rows, so
	// it changes only the log's bounds.
	q, ok := backends.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T", backends.Queue)
	}
	spec := worklet.Domain{}.Stream()
	if err := q.EnsureDomainStream(t.Context(), jetstream.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects,
		// AT THE FLOOR A BOOT SIZES A LOG TO, a log a peer could make on
		// a small disk, not the domain's declared ceiling: an existing
		// stream keeps its own, and that beside a fresh node's floors is
		// more than a test broker holds (brokertest.StoreMaxBytes), so
		// the boot would be refused a log for this staging's sake.
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
	body, err := worklet.EncodeBarrier(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind},
		Scope:   statelog.ScopeSet{Paths: []string{statelog.FrameworkScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	// SIGNED UNDER THE FLEET'S KEYRING, as a peer's record is: an unsigned
	// one is a stranger's, which the applier refuses rather than applies.
	ring := recordseal.Keyring{ActiveID: boot.Secrets.ActiveKeyID}
	for _, key := range boot.Secrets.Keys {
		ring.Keys = append(ring.Keys, recordseal.Key{ID: key.ID, Material: key.Material})
	}
	signer, err := recordseal.NewSigner(worklet.Domain{}.Name(), ring)
	if err != nil {
		t.Fatalf("the peer's signer: %v", err)
	}
	if seq, _, err := peerLog.Append(t.Context(),
		spec.SubjectPrefix+"."+statelog.BarrierKind, "", nil,
		signer.Seal(body)); err != nil || seq != 1 {
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

// AND A NODE THE FLEET HAS MOVED PAST DOES ASK, which is what makes the two
// silences above mean anything: without it a deaf tap (a renamed subject, an
// ask on another transport, a subscription never registered) passes both.
// One fact changes: a node with no checkpoint joins a fleet whose trim has
// published a floor at generation 1, so it must adopt, and with the tap
// answering nothing it comes up one [statelog.OfferWindow] later. This is the
// TAP's control, not the comparison's; that positive case, a checkpoint the
// trim purged past, is TestANodeBelowTheFloorAdoptsWhileRunning's.
func TestANodeTheFleetHasMovedPastAsksForASnapshotAtBoot(t *testing.T) {
	t.Parallel()
	boot, company, backends := joinBackends(t)
	name := worklet.Domain{}.Name()
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
// reach the broker before [engine.New] does. The join takes its connection
// from [engine.Backends.Queue] either way, so a boot on these is the boot
// `crewlet run` makes.
func joinBackends(t *testing.T) (*config.Bootstrap, *config.Company, *engine.Backends) {
	t.Helper()
	boot := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	})
	company := parsedCompany(t, companyDoc)
	backends, err := engine.OpenBackends(t.Context(), boot, company, nil)
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

// drain returns every ask published before it was called, without a timer.
//
// TWO FLUSHES ARE THE BARRIER. [statelog.CollectOffers] flushes its request
// before collecting, and a flush returns only once the server answers a PING
// sent AFTER the publish, so when [engine.New] returns the server has routed
// any ask to this tap. The PONG answering the flush below queues behind it on
// this connection, whose reads are in order, so once it is back every ask is
// pending. A timeout would guess at routing time, and a short guess passes.
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

// EVERY REGISTERED DOMAIN REPORTS A ROW, including one that does not gate seat
// admission. Admission asks whether a seat's tools would answer wrongly, which
// the vector domain declines (stale embeddings make search worse, not the
// tracker wrong); the fleet view asks how far each copy is, and a domain
// missing from it is one an operator cannot watch fall behind.
func TestEveryDomainReportsItsOwnReplicationRow(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})

	rows := e.StateLogStatus(t.Context())
	byName := map[string]engine.ReplicationStatus{}
	for _, row := range rows {
		if _, twice := byName[row.Name]; twice {
			t.Errorf("two rows named %q — the count would double one loop", row.Name)
		}
		byName[row.Name] = row
	}
	// THE TRACKER'S, whose health DOES gate admission.
	if _, held := byName[worklet.Domain{}.Name()]; !held {
		t.Errorf("no row for the tracker's own log; rows = %+v", rows)
	}
	// AND THE VECTOR DOMAIN'S, whose health does not.
	if _, held := byName["vectors"]; !held {
		t.Errorf("no row for the vector domain, so an operator cannot see the "+
			"company's embeddings fall behind; rows = %+v", rows)
	}
	// AND THE KNOWLEDGE BASE'S, a domain's row like every other.
	if _, held := byName["pages"]; !held {
		t.Errorf("no row for the pages domain; rows = %+v", rows)
	}
	// AND THE USAGE DOMAIN'S, whose gap is a coverage number too — a node
	// whose spend history stopped replicating is one the fleet view has to
	// be able to name.
	if _, held := byName["usage"]; !held {
		t.Errorf("no row for the usage domain, so an operator cannot see a "+
			"node's spend history fall behind; rows = %+v", rows)
	}
	for _, row := range rows {
		switch {
		case !row.Ready && strings.TrimSpace(row.Detail) == "":
			t.Errorf("%s is not ready and says nothing about why — an operator "+
				"reading the fleet view has no next step", row.Name)
		case row.Ready && row.Detail != "":
			t.Errorf("%s is ready and still carries a detail (%q), which reads "+
				"as a warning on a healthy loop", row.Name, row.Detail)
		}
	}
}
