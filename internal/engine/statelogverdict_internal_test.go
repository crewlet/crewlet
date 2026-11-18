package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY DOMAIN'S COPY IS JUDGED, whatever it declares about seats — ADR-0079.
//
// The router's verdict holds an entry for every registered domain, read from
// its own health, and carries separately the domain's declaration of whether a
// seat depends on it: one declaration answering both would leave the vectors
// and usage copies, which no seat depends on, unjudged and a wrong one served
// forever.
func TestEveryDomainsCopyIsJudged(t *testing.T) {
	t.Parallel()
	e, _, _ := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	s := e.core.Load().log
	v := s.judgeCopy(t.Context())
	if v.shut {
		t.Fatal("the premise: a running node's copy is open")
	}
	for _, domain := range registeredDomains() {
		name := domain.Name()
		if s.Domain(name) == nil {
			t.Fatalf("the premise: %s is not running on a data node", name)
		}
		d, judged := v.domains[name]
		switch {
		case !judged:
			t.Errorf("%s's copy was not judged", name)
		case d.unread:
			t.Errorf("%s's health could not be read on a running node", name)
		case d.wrong:
			t.Errorf("a freshly booted node judged its %s copy wrong", name)
		case d.gates != domain.GatesSeatAdmission():
			t.Errorf("%s's verdict says gates=%v and the domain declares %v",
				name, d.gates, domain.GatesSeatAdmission())
		}
	}
}

// wrongCopyOf leaves a running node's copy of one domain BELOW ITS LOG (its
// applier halted, the records it never applies purged: a hole no replay can
// fill, the one wrong state every domain can be in, compacted or not), and
// waits until the node takes it out of service.
//
// THE NODE'S OWN TRIM IS STOPPED, since its floors are not what a case is
// about, and its rejoin is RECORDED RATHER THAN OBEYED: a node below a log
// asks to adopt a snapshot, which would halt every applier mid-assertion and
// restart them over another copy.
func wrongCopyOf(t *testing.T, e *Engine, name string) {
	t.Helper()
	e.stopRetention()
	s := e.core.Load().log
	var rejoins atomic.Int64
	s.rejoinMu.Lock()
	s.rejoin = func(context.Context) error { rejoins.Add(1); return nil }
	s.rejoinMu.Unlock()

	running := s.Domain(name)
	if running == nil {
		t.Fatalf("the premise: the %s domain is not running", name)
	}
	s.haltApplier(name)
	at := running.runner.Committed()
	for _, probe := range []string{"a", "b"} {
		// ANY BYTES: the halted applier never reads them, and the purge
		// below removes the first before anything could.
		if _, _, err := running.log.Append(t.Context(),
			running.spec.SubjectPrefix+".probe."+probe, "", nil, []byte(`{}`)); err != nil {
			t.Fatalf("append to the %s log: %v", name, err)
		}
	}
	if err := running.log.Purge(t.Context(), at.Seq+2); err != nil {
		t.Fatalf("purge the %s log: %v", name, err)
	}
	health, err := s.health(t.Context(), running, s.readFloors(t.Context()))
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Floor.State != statelog.FloorBelow {
		t.Fatalf("the premise: the %s copy is %s, want below its log", name, health.Floor.State)
	}

	e.local.mu.Lock()
	e.local.slot = verdictSlot{} // judged afresh, at this instant
	e.local.mu.Unlock()
	waitUntil(t, 5*time.Second, "the "+name+" copy to go out of service", func() bool {
		_, serves := e.local.For(t.Context(), name)
		return !serves
	})
}

// ONE LOG'S SILENCE LEAVES NO OTHER DOMAIN UNREAD: the vectors stream's group
// does not answer for the whole reading deadline (mid-election), yet every
// other domain's health is read in that same reading, so a knowledge-base
// fault is seen at once rather than carried over as its old verdict. The
// reading takes one round trip's time, not the sum.
//
// ON THE BUBBLE'S CLOCK, which a loaded runner cannot skew as it does the wall
// clock: a reading that ends at its deadline takes exactly it.
//
// Mutation: give each log a deadline of its own past the reading's, and the
// reading overruns; read the logs serially, and those after the silent one
// meet an ended context and are left unread.
func TestOneLogsSilenceLeavesNoOtherDomainUnread(t *testing.T) {
	t.Parallel()
	synctest.Test(t, oneLogsSilenceLeavesNoOtherDomainUnread)
}

// oneLogsSilenceLeavesNoOtherDomainUnread is
// [TestOneLogsSilenceLeavesNoOtherDomainUnread] inside its bubble: its
// deadline and its measurement are the bubble's clock.
//
//fixedwait:bubble
func oneLogsSilenceLeavesNoOtherDomainUnread(t *testing.T) {
	var runs []*runningLog
	for _, d := range registeredDomains() {
		runs = append(runs, &runningLog{domain: d})
	}
	end := uint64(5)
	observe := func(ctx context.Context, running *runningLog) (statelog.Health, error) {
		if running.domain.Name() == vectorsName {
			<-ctx.Done()
			return statelog.Health{}, ctx.Err()
		}
		// A REAL REQUEST FAILS ON AN ENDED CONTEXT, which a serial reading
		// would hand every log after the silent one.
		if err := ctx.Err(); err != nil {
			return statelog.Health{}, err
		}
		first, lag := uint64(1), uint64(0)
		h := statelog.Health{Position: statelog.Position{Stream: running.domain.Name(),
			Generation: 1, Seq: end}, FirstSeq: &first, LastSeq: &end, Lag: &lag, Drained: true}
		if running.domain.Name() == pagesName {
			h.Err = "the applier halted at a page it cannot apply"
		}
		return h, nil
	}
	// EVERY DOMAIN'S FLOOR READ AND ABSENT — a fleet whose trim has not
	// run — so what this case judges is the logs alone: a domain the read
	// did not ask about is no floor at all, and its copy unread.
	floors := func(context.Context) publishedFloors {
		read := publishedFloors{domains: map[string]domainFloor{}, at: time.Now()}
		for _, name := range registeredNames() {
			read.domains[name] = domainFloor{}
		}
		return read
	}

	budget := 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()
	started := time.Now()
	v := judgeReads(time.Now(), readHealthsWith(ctx, runs, floors, observe))
	if took := time.Since(started); took != budget {
		t.Fatalf("the reading took %s on the bubble's clock, want exactly its %s "+
			"deadline: a reading that ends at its deadline takes no more", took, budget)
	}
	if !v.domains[vectorsName].unread {
		t.Fatal("the premise: the silent vectors log was read")
	}
	for _, name := range []string{trackerName, usage.Domain{}.Name()} {
		if d := v.domains[name]; d.unread || d.wrong || !d.answers {
			t.Errorf("the vectors log's silence left %s %+v", name, d)
		}
	}
	if d := v.domains[pagesName]; d.unread || !d.wrong || d.why != statelog.RefuseStalled {
		t.Errorf("a knowledge-base fault read beside a silent vectors log came back %+v", d)
	}
}

// A WRONG COPY OF ONE DOMAIN LEAVES THE OTHERS IN SERVICE, on a real node.
//
// With the vectors copy below its log (see wrongCopyOf), that copy goes out
// of service; the tracker's and the knowledge base's keep answering, the seat
// copy stays this node's with its seats and claims, and a single node's own
// tracker read is still answered from its own copy.
func TestAWrongCopyOfOneDomainLeavesTheOthersInService(t *testing.T) {
	t.Parallel()
	e, _, _ := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	// AND FOR THE TRACKER'S AND THE KNOWLEDGE BASE'S OWN DRAIN, which "keep
	// answering" is measured from. Admitting a seat proves only that a copy
	// was level at that instant, not SERVING (drained since its applier
	// started); the drained latch is set on the fetch AFTER
	// the last commit, and a copy without it reads as not answering whenever
	// a record (a linearizable read's barrier) is in flight as the verdict is
	// judged afresh below, whatever the vectors copy does.
	s := e.core.Load().log
	for _, name := range []string{trackerName, pagesName} {
		running := s.Domain(name)
		if running == nil {
			t.Fatalf("the premise: the %s domain is not running", name)
		}
		waitUntil(t, 20*time.Second, "the "+name+" applier to drain its log",
			running.runner.Drained)
	}
	wrongCopyOf(t, e, vectorsName)
	for _, name := range []string{trackerName, pagesName, estate.SeatCopy} {
		b, serves := e.local.For(t.Context(), name)
		if !serves {
			t.Fatalf("a vectors copy below its log took the %q copy out of service", name)
		}
		if name != estate.SeatCopy && (b.Answers == nil || !b.Answers(t.Context())) {
			t.Fatalf("a vectors copy below its log stopped the %s copy answering", name)
		}
	}
	if ok, reason := e.SeatsServiceable(t.Context()); !ok {
		t.Fatalf("a vectors copy below its log shed the node's seats: %s", reason)
	}
	if !e.StateLogHydrated(t.Context()) {
		t.Fatal("a vectors copy below its log withheld the node's seat claims")
	}
	if _, err := e.router.Work().Tasks(t.Context(), worklet.Query{Level: statelog.ReadLinearizable}, time.Now()); err != nil {
		t.Fatalf("a single node whose vectors copy is wrong refused its own tracker read: %v", err)
	}
}
