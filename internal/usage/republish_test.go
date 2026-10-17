package usage_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// fleetLog is the usage log as two data nodes hold it: every record node-a
// publishes is applied into its own estate, and into the PEER's — the node
// that will re-anchor — only while the peer's applier follows the log.
//
// AND THE BROKER'S DUPLICATE WINDOW, which is what a republish has to get past:
// a record under an operation id the log already holds is collapsed onto that
// copy, applied nowhere new and answered as the copy's. Every id is held for
// the life of the case, which is the window at its widest — a republish
// minutes after the original is inside the real one.
type fleetLog struct {
	t          *testing.T
	own, peer  *store.DB
	mu         sync.Mutex
	seq        uint64
	peerFollow bool
	held       map[string]bool
	sent       []string
	unresolved int
}

func (l *fleetLog) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	decision, err := req.Decide(nil, statelog.Stamp{Writer: "node-a", Gen: 1})
	if err != nil {
		return statelog.Result{}, err
	}
	env, err := usage.Domain{}.Envelope(decision.Payload)
	if err != nil {
		return statelog.Result{}, err
	}
	l.sent = append(l.sent, env.Subject.String())
	if l.unresolved > 0 {
		l.unresolved--
		return statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: req.OpID}, nil
	}
	if l.held[req.OpID] {
		return statelog.Result{Outcome: statelog.OutcomeApplied, OpID: req.OpID,
			Collapsed: true}, nil
	}
	l.held[req.OpID] = true
	l.seq++
	rec := statelog.Record{Envelope: env, Payload: decision.Payload,
		Position: statelog.Position{Stream: usage.Domain{}.Stream().Name, Generation: 1, Seq: l.seq}}
	if err := applyRecord(ctx, l.own, rec); err != nil {
		return statelog.Result{}, err
	}
	if l.peerFollow {
		if err := applyRecord(ctx, l.peer, rec); err != nil {
			return statelog.Result{}, err
		}
	}
	return statelog.Result{Outcome: statelog.OutcomeApplied, OpID: req.OpID}, nil
}

// follow sets whether the peer's applier follows the log.
func (l *fleetLog) follow(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.peerFollow = on
}

// sentOn counts the records sent on subjects of one day since the last reset.
func (l *fleetLog) sentOn(day string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.sent {
		if strings.Contains(s, day) {
			n++
		}
	}
	return n
}

func (l *fleetLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = nil
}

// seenGenerations is the fleet's usage generations as a case sets them — what
// the engine's heartbeat would have read off the positions register.
type seenGenerations struct {
	mu   sync.Mutex
	g    usage.Generations
	read bool
}

func (s *seenGenerations) set(g usage.Generations) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.g, s.read = g, true
}

func (s *seenGenerations) get() (usage.Generations, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.g, s.read
}

// fleetPublisher is node-a's publisher over its own store, into l, holding the
// generations seen reports.
func fleetPublisher(t *testing.T, own *store.DB, l *fleetLog, c *clock,
	seen *seenGenerations) *usage.Publisher {

	t.Helper()
	return fleetPublisherLogging(t, own, l, c, seen, &refusalLines{})
}

// fleetPublisherLogging is [fleetPublisher] writing its lines to lines.
func fleetPublisherLogging(t *testing.T, own *store.DB, l *fleetLog, c *clock,
	seen *seenGenerations, lines *refusalLines) *usage.Publisher {

	t.Helper()
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: own, Log: l, Estate: own.Replicated(), NodeID: "node-a", Caught: caught,
		Generations: seen.get,
		Zone:        func() *time.Location { return santiago },
		Now:         c.now,
		FireHistory: fireHistory,
		Logger:      slog.New(slog.NewTextHandler(lines, nil)),
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	return p
}

// A RESTORED USAGE LOG WHOSE DAYS MOVED AFTER THE RESTORE CONVERGES AFTER THE
// REANCHOR.
//
// A broker restored from an older copy leaves the peer's rows ahead of the log;
// its applier stops, and while it waits for its reanchor node-a goes on
// publishing — yesterday's last turn arrives late, and a custody settle moves a
// day five back. The peer's reanchor follows the restored log from its END, so
// it never applies those records, and node-a sends neither day again: neither
// moves any more. Without more, the peer holds both at their pre-restore value
// for the 181 days the history keeps them.
//
// What reaches it is the rise in the peer's generation of the log: node-a
// publishes every day its event log holds again — under operation ids no copy
// before the rise carries, since the broker would collapse an unchanged day
// onto the copy the peer skipped — and records the generations it answered, so
// a restart does not answer the same rise again.
//
// Mutations, each checked red: no pass on a rise (the peer keeps both days
// stale); ids that do not cover the generations (every resend collapses);
// the pass recorded as answered before it settles (the restart's pass is
// skipped); a restart that does not read what was answered (it passes again).
func TestARestoredUsageLogConvergesAfterTheReanchor(t *testing.T) {
	t.Parallel()
	own, peer := openStore(t), openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	c := &clock{t: noon}
	yesterday, older := daysBack(noon, 1, 10), daysBack(noon, 5, 10)
	ev := &events{t: t, db: own}
	ev.phase(yesterday, "seat-1", "t1", "execute", "", 5, 1)
	ev.phase(older, "seat-2", "t2", "execute", "", 7, 1)

	l := &fleetLog{t: t, own: own, peer: peer, peerFollow: true, held: map[string]bool{}}
	seen := &seenGenerations{}
	seen.set(usage.Generations{"node-a": 0, "node-b": 0})
	p := fleetPublisher(t, own, l, c, seen)
	// THE FIRST PASS: a node that never answered any generations publishes
	// every day once — the older one included, which is what puts it in
	// the peer's rows before the restore.
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("the first flush: %v", err)
	}
	if got := spendOn(t, peer, older); got["seat-2"] != 7 {
		t.Fatalf("before the restore the peer holds %v five days back, want seat-2's 7", got)
	}

	// THE RESTORE: the peer's applier stops on a log that ends below its rows.
	l.follow(false)
	// AND NODE-A'S DAYS MOVE: yesterday's last turn written after midnight,
	// and a custody batch of five days back settled as kept.
	ev.phase(yesterday.Add(13*time.Hour), "seat-1", "t1b", "execute", "", 50, 1)
	keep(t.Context(), t, own, "late-batch", custodyPhase(t, "late", older.Add(time.Hour), "seat-2", 70))
	c.set(noon.Add(time.Minute))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("the flush while the peer waits: %v", err)
	}
	if got := spendOn(t, own, yesterday); got["seat-1"] != 55 {
		t.Fatalf("node-a holds %v yesterday, want 55 — the late turn was not published", got)
	}

	// THE REANCHOR: the peer follows the log from its end, and its row names
	// its next generation.
	l.follow(true)
	c.set(noon.Add(2 * time.Minute))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("a flush before the rise is seen: %v", err)
	}
	if got := spendOn(t, peer, yesterday); got["seat-1"] != 5 {
		t.Fatalf("the peer holds %v yesterday before node-a sees the rise, want the "+
			"pre-restore 5 — the case is not reaching the state it is about", got)
	}
	seen.set(usage.Generations{"node-a": 0, "node-b": 1})
	c.set(noon.Add(3 * time.Minute))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("the flush that sees the rise: %v", err)
	}
	if got := spendOn(t, peer, yesterday); got["seat-1"] != 55 {
		t.Errorf("after the reanchor the peer holds %v yesterday, want node-a's 55", got)
	}
	if got := spendOn(t, peer, older); got["seat-2"] != 77 {
		t.Errorf("after the reanchor the peer holds %v five days back, want node-a's 77", got)
	}
	recorded, ok, err := own.UsageRepublished(t.Context())
	if err != nil || !ok || recorded != "node-a@0,node-b@1" {
		t.Errorf("node-a recorded %q (%v, %v) as answered, want the generations of the rise",
			recorded, ok, err)
	}

	// A RESTART ANSWERS NOTHING IT ALREADY ANSWERED: the next start owes
	// today and yesterday by its watermark, and nothing older.
	l.reset()
	again := fleetPublisher(t, own, l, &clock{t: noon.Add(4 * time.Minute)}, seen)
	if err := again.Flush(t.Context()); err != nil {
		t.Fatalf("the restart's flush: %v", err)
	}
	if n := l.sentOn(older.Format(time.DateOnly)); n != 0 {
		t.Errorf("a restart sent %d record(s) five days back for a rise it had already "+
			"answered", n)
	}
}

// A PASS THAT DID NOT SETTLE IS ANSWERED AGAIN BY THE NEXT START.
//
// The generations a pass answers are recorded only once every day it derived
// has landed, so a process that stops with one publish unresolved leaves the
// rise unanswered, and its next start — seeing the same generations on its
// first beat — publishes every day again. Recorded at the start of the pass
// instead, a crash in the middle lost the rest of it for good.
func TestAPassThatDidNotSettleIsAnsweredAgain(t *testing.T) {
	t.Parallel()
	own, peer := openStore(t), openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	older := daysBack(noon, 5, 10)
	ev := &events{t: t, db: own}
	ev.phase(older, "seat-2", "t2", "execute", "", 7, 1)

	l := &fleetLog{t: t, own: own, peer: peer, peerFollow: true, held: map[string]bool{},
		unresolved: 1}
	seen := &seenGenerations{}
	seen.set(usage.Generations{"node-a": 0, "node-b": 1})
	first := fleetPublisher(t, own, l, &clock{t: noon}, seen)
	if err := first.Flush(t.Context()); err != nil {
		t.Fatalf("the first flush: %v", err)
	}
	if _, ok, err := own.UsageRepublished(t.Context()); err != nil || ok {
		t.Fatalf("a pass with a publish unresolved was recorded as answered (%v, %v)", ok, err)
	}

	l.reset()
	second := fleetPublisher(t, own, l, &clock{t: noon.Add(time.Minute)}, seen)
	if err := second.Flush(t.Context()); err != nil {
		t.Fatalf("the restart's flush: %v", err)
	}
	if n := l.sentOn(older.Format(time.DateOnly)); n == 0 {
		t.Error("the restart sent nothing five days back, so the rise the first " +
			"process never finished answering is lost")
	}
	if recorded, ok, err := own.UsageRepublished(t.Context()); err != nil || !ok ||
		recorded != "node-a@0,node-b@1" {
		t.Errorf("after the restart's pass node-a recorded %q (%v, %v)", recorded, ok, err)
	}
}

// A NODE JOINING IS NOT A REANCHOR.
//
// A data node that joins appears in the register at whatever generation its
// donor's snapshot held, and it lacks nothing a tail could hold — so a node
// first seen at zero starts no pass, and only a rise does: above the generation
// a node was last answered at, or above zero for one first seen. A change that
// rises nothing is recorded as answered, so the next start does not hold it
// against the old one.
//
// AND A FIRST START CLAIMS NO REANCHOR: a node whose store records no pass
// publishes every day once, which on a fresh node is nothing, and its line says
// so rather than sending an operator after a reanchor nobody ran. Mutation:
// log the reanchor's detail on every pass, and the first start's line claims one.
func TestANodeJoiningIsNotAReanchor(t *testing.T) {
	t.Parallel()
	own, peer := openStore(t), openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	older := daysBack(noon, 5, 10)
	ev := &events{t: t, db: own}
	ev.phase(older, "seat-2", "t2", "execute", "", 7, 1)

	l := &fleetLog{t: t, own: own, peer: peer, peerFollow: true, held: map[string]bool{}}
	seen := &seenGenerations{}
	seen.set(usage.Generations{"node-a": 0})
	c := &clock{t: noon}
	lines := &refusalLines{}
	p := fleetPublisherLogging(t, own, l, c, seen, lines)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("the first flush: %v", err)
	}
	if first := lines.text(); !strings.Contains(first, "msg=usage_republish_started") ||
		strings.Contains(first, "re-anchored") {
		t.Errorf("a first start's pass logged %q, want usage_republish_started "+
			"claiming no reanchor — none was run", first)
	}

	l.reset()
	seen.set(usage.Generations{"node-a": 0, "node-c": 0})
	c.set(noon.Add(time.Minute))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("the flush after a node joined: %v", err)
	}
	if n := l.sentOn(older.Format(time.DateOnly)); n != 0 {
		t.Errorf("a node joining at generation zero sent %d record(s) five days back", n)
	}
	if recorded, _, _ := own.UsageRepublished(t.Context()); recorded != "node-a@0,node-c@0" {
		t.Errorf("the join was recorded as %q, want the generations seen", recorded)
	}

	seen.set(usage.Generations{"node-a": 0, "node-c": 0, "node-d": 2})
	c.set(noon.Add(2 * time.Minute))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("the flush after a node first seen above zero: %v", err)
	}
	if n := l.sentOn(older.Format(time.DateOnly)); n == 0 {
		t.Error("a node first seen above generation zero re-anchored its copy, and " +
			"nothing five days back was sent again")
	}
	if got := lines.text(); !strings.Contains(got, "re-anchored") {
		t.Errorf("the pass a rise started logged %q, want it to name the reanchor", got)
	}
}

// GENERATIONS ROUND-TRIP, AND A RISE IS A RISE.
func TestGenerationsRoundTripAndRise(t *testing.T) {
	t.Parallel()
	g := usage.Generations{"node-b": 3, "node-a": 0}
	if got := g.String(); got != "node-a@0,node-b@3" {
		t.Fatalf("Generations.String = %q, want the nodes in order", got)
	}
	back, err := usage.ParseGenerations(g.String())
	if err != nil || back.String() != g.String() {
		t.Fatalf("ParseGenerations(%q) = %v, %v", g.String(), back, err)
	}
	if empty, err := usage.ParseGenerations(""); err != nil || len(empty) != 0 {
		t.Fatalf("ParseGenerations(\"\") = %v, %v", empty, err)
	}
	if _, err := usage.ParseGenerations("node-a"); err == nil {
		t.Error("a node with no generation was read")
	}
	for _, tc := range []struct {
		name      string
		seen, was usage.Generations
		rises     bool
	}{
		{"unchanged", usage.Generations{"a": 1}, usage.Generations{"a": 1}, false},
		{"a node re-anchored", usage.Generations{"a": 2}, usage.Generations{"a": 1}, true},
		{"a node joined at zero", usage.Generations{"a": 1, "b": 0}, usage.Generations{"a": 1}, false},
		{"a node first seen above zero", usage.Generations{"b": 1}, usage.Generations{}, true},
		{"a node gone from the register", usage.Generations{}, usage.Generations{"a": 4}, false},
	} {
		if got := tc.seen.Rises(tc.was); got != tc.rises {
			t.Errorf("%s: %v rises above %v = %v, want %v", tc.name, tc.seen, tc.was, got, tc.rises)
		}
	}
}
