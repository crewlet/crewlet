package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A NODE THAT IS BEHIND KEEPS ITS SEATS.
//
// "Has this copy ever been whole" ([stateLog.health]'s Drained, the applier's
// latch) is not "is it level this instant". Conflated, one record not yet
// applied makes [Engine.SeatsServiceable] report the node's rows wrong, so it
// sheds every seat it holds on each tracker write and reclaims them an epoch
// later.
//
// The appliers are halted rather than raced: an apply takes about a
// millisecond, and halting holds the gap between append and apply still.
func TestANodeBehindOnItsLogKeepsTheSeatsItHolds(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
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
	spec := worklet.Domain{}.Stream()
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	s := e.core.Load().log
	running := s.Domain(worklet.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	// AND FOR THE APPLIER'S OWN DRAIN, up to [statelog.FetchWait] after a lag
	// of zero: halted before it, the node has never drained, another case.
	waitUntil(t, 20*time.Second, "the applier to drain its log",
		running.runner.Drained)
	if ok, domain := e.SeatsServiceable(t.Context()); !ok {
		t.Fatalf("a caught-up node cannot keep its seats: %s", domain)
	}

	// ONE RECORD THE APPLIER HAS NOT REACHED, which is what every tracker
	// write leaves behind between the append and the apply.
	s.haltAppliers()
	at := running.runner.Committed()
	body, err := worklet.EncodeBarrier(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind}, Gen: at.Generation,
		Scope: statelog.ScopeSet{Paths: []string{statelog.FrameworkScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	seq, _, err := log.Append(t.Context(),
		spec.SubjectPrefix+"."+statelog.BarrierKind, "", nil, running.signer.Seal(body))
	if err != nil {
		t.Fatalf("append a barrier: %v", err)
	}
	if seq != at.Seq+1 {
		t.Fatalf("the barrier landed at %d, want %d", seq, at.Seq+1)
	}

	health, err := s.health(t.Context(), running, s.readFloors(t.Context()))
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
	if e.StateLogHydrated(t.Context()) {
		t.Error("the node admits new seats with a record unapplied, so a seat " +
			"attaches to a copy that is behind and answers \"there is no such " +
			"item\" about work it was just handed")
	}
	if ok, domain := e.SeatsServiceable(t.Context()); !ok {
		t.Fatalf("the node gave up every seat it holds over %s being one record "+
			"behind — on a company doing nothing but filing work items that is "+
			"the whole seat roster moving on every write", domain)
	}

	// AND THE COPY STILL ANSWERS REQUESTS: a copy's fault takes it out of
	// service rather than shedding seats, so the regression could return as
	// the request gate. One record is inside a drained copy's snapshot slack,
	// so it answers while admitting no seat; judged otherwise, every request
	// would go to whichever peer was level, and a lone node's seats would wait
	// for the lagging pass.
	if v := s.judgeCopy(t.Context()); !v.serves(running.domain.Name()) || !v.answers(running.domain.Name()) {
		t.Fatalf("a copy one record behind is judged %+v, want sound and answering", v)
	}
	e.local.mu.Lock()
	e.local.slot = verdictSlot{} // judged afresh, at this instant
	e.local.mu.Unlock()
	served, serves := e.local.For(t.Context(), running.domain.Name())
	switch {
	case !serves:
		t.Fatal("a copy one record behind is not served")
	case served.Answers == nil || !served.Answers(t.Context()):
		t.Fatal("a copy one record behind answers no request")
	case served.Admits == nil || served.Admits(t.Context()):
		t.Fatal("a copy one record behind admits a seat, which the gate above refused")
	}
	if _, err := e.router.Work().Tasks(t.Context(), worklet.Query{Level: statelog.ReadStale},
		time.Now()); err != nil {
		t.Fatalf("a single node one record behind refused its own seat's read: %v", err)
	}

	// AND THE FLEET VIEW STILL SAYS SO. The shed is what must not happen;
	// reporting the node as caught up would be the opposite error, and is
	// how an operator watching a node fall behind would see nothing.
	for _, row := range e.StateLogStatus(t.Context()) {
		if row.Name != (worklet.Domain{}).Name() {
			continue
		}
		if row.Ready {
			t.Errorf("the fleet view reports the tracker ready with a record "+
				"unapplied: %+v", row)
		}
	}
}
