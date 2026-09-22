package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE THAT IS BEHIND KEEPS ITS SEATS.
//
// # The measured failure this is the regression for
//
// [stateLog.health] derived "has this node's copy ever been whole" from "is it
// level this instant" — one field assigned `lag == 0`, recomputed on every
// heartbeat. [statelog.Health.Healthy] reads the first and [Engine.SeatsServiceable]
// reads that, so a single record on the log this node had not applied YET made
// the node report its own rows WRONG: the sweep released every seat it held
// and reclaimed them at the next epoch about five seconds later. Running the
// Nimbus example with dummy model keys, one node shed all seven of its seats
// six times in eight minutes, each burst on a write to the tracker — which
// with a real model is every turn on the node interrupted by somebody filing
// a work item.
//
// # Why the appliers are halted rather than the timing raced
//
// Because the applier consumes a record in about a millisecond and the state
// under test is the gap between the append and the apply. Halting the loops
// makes that gap hold still: the record lands on the log, this node's
// checkpoint stays where it was, and the two gates are asked the same question
// they were asked during the bursts.
func TestANodeBehindOnItsLogKeepsTheSeatsItHolds(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	q, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", back.Queue)
	}
	spec := tracker.Domain{}.Stream()
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	s := e.native.log
	running := s.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	// AND FOR THE APPLIER'S OWN DRAIN, which is a separate instant from a
	// lag of zero: the loop learns the log is empty on the fetch AFTER it
	// commits the last record, up to [statelog.FetchWait] later. Halting
	// before then would stage a node that has genuinely never drained,
	// which is a different case from the one under test.
	waitUntil(t, 20*time.Second, "the applier to drain its log",
		running.runner.Drained)
	if ok, domain := e.SeatsServiceable(); !ok {
		t.Fatalf("a caught-up node cannot keep its seats: %s", domain)
	}

	// ONE RECORD THE APPLIER HAS NOT REACHED, which is what every tracker
	// write leaves behind between the append and the apply.
	s.haltAppliers()
	at := running.runner.Committed()
	body, err := tracker.EncodeBarrier(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind}, Gen: at.Generation,
		Scope: statelog.ScopeSet{Paths: []string{statelog.BarrierScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	seq, _, err := log.Append(t.Context(),
		spec.SubjectPrefix+"."+statelog.BarrierKind, "", nil, body)
	if err != nil {
		t.Fatalf("append a barrier: %v", err)
	}
	if seq != at.Seq+1 {
		t.Fatalf("the barrier landed at %d, want %d", seq, at.Seq+1)
	}

	health, err := s.health(t.Context(), running)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Lag == nil || *health.Lag != 1 {
		t.Fatalf("health reports a lag of %v, want 1 — the case below is about a "+
			"node with exactly one unapplied record", health.Lag)
	}
	if !health.Drained {
		t.Fatal("a node that caught up and then fell one record behind reports " +
			"that it has never drained its log — which is the conflation this " +
			"test exists for: the latch is the applier's own history and the " +
			"lag beside it is the instant")
	}

	// THE TWO GATES, AND THEY MUST DISAGREE. Admission withholds new
	// claims, because a seat attaching here would act on rows that are
	// behind; serviceability keeps what is held, because rows that are
	// behind catch up and dropping the work would be pure loss.
	if e.NativeHydrated() {
		t.Error("the node admits new seats with a record unapplied, so a seat " +
			"attaches to a copy that is behind and answers \"there is no such " +
			"item\" about work it was just handed")
	}
	if ok, domain := e.SeatsServiceable(); !ok {
		t.Fatalf("the node gave up every seat it holds over %s being one record "+
			"behind — on a company doing nothing but filing work items that is "+
			"the whole seat roster moving on every write", domain)
	}

	// AND THE FLEET VIEW STILL SAYS SO. The shed is what must not happen;
	// reporting the node as caught up would be the opposite error, and is
	// how an operator watching a node fall behind would see nothing.
	for _, row := range e.NativeStatus(t.Context()) {
		if row.Name != (tracker.Domain{}).Name() {
			continue
		}
		if row.Ready {
			t.Errorf("the fleet view reports the tracker ready with a record "+
				"unapplied: %+v", row)
		}
	}
}
