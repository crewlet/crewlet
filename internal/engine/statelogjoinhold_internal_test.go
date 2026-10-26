package engine

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A JOIN PINS THE TAIL IN THE ARTEFACT'S GENERATION, NEVER ITS OWN.
//
// A sequence names a number space, and the pin is only a pin when it is
// stated in the log's. The artefact's generation is the log's — the join
// refuses an offer at any other — while the joiner's own checkpoint is the one
// position a join knows to be wrong: a node that never ran has none, which
// read as generation 0, and a node a reanchor passed holds the old one. Stated
// in either, the hold sat below the log's generation, and the trim reads such
// a hold as an UNKNOWN term — it blocked every trim of the log for a minute
// and then, gone stale, pinned nothing at all.
//
// The joiner here has no estate open, as a node that never ran has none worth
// reading: the pin must not need one.
func TestAJoinPinsTheTailInTheArtefactsGeneration(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	s := &stateLog{nodeID: "node-join", fleet: fleet, tailHoldEvery: statelog.TrimHoldHeartbeat}
	at := map[string]statelog.DomainPosition{}
	for _, domain := range registeredDomains() {
		at[domain.Name()] = statelog.DomainPosition{
			Stream: domain.Stream().Name, Generation: 3, Seq: 2,
		}
	}
	release, err := s.holdTail(t.Context(), at)
	if err != nil {
		t.Fatalf("holdTail: %v", err)
	}
	defer release()

	holds, err := fleet.Holds(t.Context())
	if err != nil {
		t.Fatalf("read the holds: %v", err)
	}
	for _, domain := range registeredDomains() {
		in := statelog.TrimInputs{
			Generation: 3, Now: time.Now().UTC(),
			HoldsReadable: true, HoldStale: statelog.TrimHoldStale,
			Holds: holdsFor(holds, domain.Stream().Name),
		}
		var held statelog.Term
		for _, term := range in.Terms() {
			if term.Name == statelog.TermMinHold {
				held = term
			}
		}
		if !held.Known || held.Seq != 2 {
			t.Fatalf("on %s, a log at generation 3 reads the join's pin as %+v, "+
				"want it known at 2 — a pin in another number space blocks the "+
				"trim while it is fresh and pins nothing once it is stale",
				domain.Name(), held)
		}
	}
}

// joinHolds is the join pins the register holds for node.
func joinHolds(t *testing.T, register interface {
	Holds(context.Context) ([]coord.TrimHold, error)
}, node string) []coord.TrimHold {
	t.Helper()
	holds, err := register.Holds(t.Context())
	if err != nil {
		t.Fatalf("read the holds: %v", err)
	}
	var out []coord.TrimHold
	for _, h := range holds {
		if h.Owner == coord.HoldOwner(node, joinHoldPurpose) {
			out = append(out, h)
		}
	}
	return out
}

// THE JOIN'S PIN RENEWS ITSELF FOR AS LONG AS IT IS HELD, AND NOT AFTER.
//
// The trim ignores a hold older than its stale bound, which is what stops a
// crashed holder pinning the log for ever — and reads a pin nobody renews as
// exactly such a holder. The join's pin was put once and never renewed, so a
// minute into any transfer it pinned nothing, and a boot's join — which
// nothing else counts — could lose the very tail it was fetching. Here the
// node renews on a cadence a case can wait out, and the trim is asked at an
// instant that leaves the pin's first put past the stale bound and its latest
// renewal inside it: a pin put once is ignored there, a renewed one is not.
func TestAJoinsPinRenewsItselfUntilReleased(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	s := &stateLog{nodeID: "node-join", fleet: fleet, tailHoldEvery: time.Millisecond}
	at := map[string]statelog.DomainPosition{}
	for _, domain := range registeredDomains() {
		at[domain.Name()] = statelog.DomainPosition{
			Stream: domain.Stream().Name, Generation: 1, Seq: 2,
		}
	}
	release, err := s.holdTail(t.Context(), at)
	if err != nil {
		t.Fatalf("holdTail: %v", err)
	}
	defer release()
	taken := joinHolds(t, fleet, "node-join")
	if len(taken) != 1 {
		t.Fatalf("%d join pin(s) in the register once taken, want one", len(taken))
	}

	// A RENEWAL A WHOLE STALE BOUND AFTER THE FIRST PUT — fifty
	// milliseconds standing in for the minute, at a millisecond's cadence.
	const stale = 50 * time.Millisecond
	var renewed coord.TrimHold
	waitUntil(t, 10*time.Second, "a renewal of the join's pin", func() bool {
		holds := joinHolds(t, fleet, "node-join")
		if len(holds) != 1 || holds[0].At.Sub(taken[0].At) <= stale {
			return false
		}
		renewed = holds[0]
		return true
	})
	// Half the renewal's own age past it: the first put is half as old
	// again as the bound, and the renewal half as old as it.
	age := renewed.At.Sub(taken[0].At)
	for _, domain := range registeredDomains() {
		in := statelog.TrimInputs{
			Generation: 1, Now: renewed.At.Add(age / 2),
			HoldsReadable: true, HoldStale: age,
			Holds: holdsFor([]coord.TrimHold{renewed}, domain.Stream().Name),
		}
		var held statelog.Term
		for _, term := range in.Terms() {
			if term.Name == statelog.TermMinHold {
				held = term
			}
		}
		if !held.Known || held.Seq != 2 {
			t.Fatalf("on %s the trim reads the renewed join pin as %+v, want it "+
				"known at 2", domain.Name(), held)
		}
	}

	release()
	time.Sleep(20 * time.Millisecond)
	if n := len(joinHolds(t, fleet, "node-join")); n != 0 {
		t.Fatalf("%d join pin(s) in the register after the release — a renewal "+
			"that outlives its release puts the pin back for a whole stale window", n)
	}
}

// AN INSTALLED ARTEFACT'S PIN IS KEPT UNTIL A ROW OF THIS NODE'S OWN COUNTS IT.
//
// The trim counts a node by its published position, and a boot's join runs
// before the node has published one — before it holds a presence lease, too —
// so while it adopts, and after, until its first row lands, nothing but its
// own pin keeps the tail it is about to replay. Released at the end of the
// join, the pin left exactly that window open. So it is kept, and a published
// row releases it only when it counts the tail: on every log, a position no
// further along than the rows this node holds. A row naming more than the rows
// hold — a runner still keyed to the checkpoint an artefact replaced reports
// that, and it may be ahead on a log the node was current on — or naming
// nothing for a log, keeps it.
func TestAnInstalledArtefactsPinIsKeptUntilThisNodeIsCounted(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	at := map[string]statelog.DomainPosition{}
	for _, domain := range registeredDomains() {
		at[domain.Name()] = statelog.DomainPosition{Stream: domain.Stream().Name, Generation: 1, Seq: 10}
		if err := estate.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `
				INSERT INTO statelog_cursor
					(stream, generation, seq, stream_created_at, updated_at, applied_version)
				VALUES (?, 1, 10, 0, 0, 0)`, domain.Stream().Name)
			return err
		}); err != nil {
			t.Fatalf("seed %s's checkpoint: %v", domain.Name(), err)
		}
	}
	fleet := memory.NewFleet()
	ctx, cancel := context.WithCancel(t.Context())
	s := &stateLog{nodeID: "node-join", db: db, fleet: fleet,
		tailHoldEvery: statelog.TrimHoldHeartbeat, run: ctx, stop: cancel}
	rowAt := func(seq uint64, generation uint32) coord.NodePositions {
		row := coord.NodePositions{NodeID: "node-join", Domains: map[string]coord.DomainPosition{}}
		for _, domain := range registeredDomains() {
			row.Domains[domain.Name()] = coord.DomainPosition{Generation: generation, Seq: seq}
		}
		return row
	}
	adopt := func() *heldTail {
		t.Helper()
		release, err := s.holdTail(t.Context(), at)
		if err != nil {
			t.Fatalf("holdTail: %v", err)
		}
		s.holdUntilCounted(release)
		return s.waitingTail()
	}

	pin := adopt()
	ahead := rowAt(10, 1)
	ahead.Domains[worklet.Domain{}.Name()] = coord.DomainPosition{Generation: 1, Seq: 11}
	missing := rowAt(10, 1)
	delete(missing.Domains, worklet.Domain{}.Name())
	newer := rowAt(1, 2)
	for name, row := range map[string]coord.NodePositions{
		"a row ahead of the rows on one log": ahead,
		"a row naming nothing for one log":   missing,
		"a row in a later generation":        newer,
	} {
		s.releaseCountedTail(t.Context(), pin, row)
		if len(joinHolds(t, fleet, "node-join")) != 1 || s.waitingTail() != pin {
			t.Fatalf("%s released the pin: the trim would count this node past "+
				"records it has still to replay", name)
		}
	}

	// A ROW AT THE ROWS — or behind them, or at an earlier generation,
	// which the trim reads as unknown and keeps everything for — COUNTS.
	behind := rowAt(10, 1)
	behind.Domains[worklet.Domain{}.Name()] = coord.DomainPosition{Generation: 1, Seq: 9}
	behind.Domains[search.Domain{}.Name()] = coord.DomainPosition{Generation: 0, Seq: 99}
	s.releaseCountedTail(t.Context(), pin, behind)
	if n := len(joinHolds(t, fleet, "node-join")); n != 0 || s.waitingTail() != nil {
		t.Fatalf("a row that counts the tail left %d join pin(s) standing — the "+
			"trim stays pinned at the artefact for as long as this node runs", n)
	}

	// A LATER JOIN REPLACES AN EARLIER PIN, and a publish that captured the
	// earlier one does not release the later: that join's installed file is
	// one this publish's reading may predate.
	first := adopt()
	second := adopt()
	s.releaseCountedTail(t.Context(), first, rowAt(10, 1))
	if len(joinHolds(t, fleet, "node-join")) != 1 || s.waitingTail() != second {
		t.Fatal("a publish that captured an earlier join's pin released the pin " +
			"a later join took")
	}

	// AND THE STOP RELEASES WHAT NO ROW WILL NOW COUNT.
	s.Stop()
	if n := len(joinHolds(t, fleet, "node-join")); n != 0 {
		t.Fatalf("%d join pin(s) survive the state log's stop — each goes on "+
			"renewing itself, and pins the trim for as long as the process runs", n)
	}
}

// A REJOIN KEEPS THE PIN ON ITS ARTEFACT'S TAIL UNTIL A PUBLISH COUNTS IT.
//
// The whole path on a real broker: a running node below the floor adopts a
// donor's snapshot, and its pin on the tail is still in the register when the
// rejoin returns — the heartbeat is held off, so nothing has published since
// the install — and gone once a publish has written a row of the node's own.
func TestARejoinKeepsItsPinUntilAPublishCountsIt(t *testing.T) {
	t.Parallel()
	e, back, q := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats",
		func() bool { return e.StateLogHydrated(t.Context()) })
	s := e.core.Load().log
	quietHeartbeat(s)
	running, at, last := pushBelowTheFloor(t, e, q)
	rows := filepath.Join(t.TempDir(), "crewlet-replicated.db")
	copyAdvancedTo(t, back, running, at, last, rows)
	standUpDonor(t, e, q, rows, statelog.Position{
		Stream: at.Stream, Generation: at.Generation, Seq: last,
	}, running.runner.KeyedTo())

	// THE HEARTBEAT IS HELD OFF, so whatever is in the register when the
	// rejoin returns is the join's own doing.
	s.publishing.Lock()
	err := e.rejoin(s.run, s)
	pinned := joinHolds(t, back.Fleet, e.id)
	s.publishing.Unlock()
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if len(pinned) != 1 {
		t.Fatalf("%d join pin(s) in the register once the adoption installed, "+
			"want its one — nothing counts this node at the artefact yet", len(pinned))
	}
	if pos := pinned[0].Streams[at.Stream]; pos.Seq != last || pos.Generation != at.Generation {
		t.Fatalf("the pin holds %+v, want the artefact's %d@%d", pos, last, at.Generation)
	}

	s.publishPositions(t.Context())
	if n := len(joinHolds(t, back.Fleet, e.id)); n != 0 {
		t.Fatalf("%d join pin(s) survive a publish that counts this node at the "+
			"artefact — the trim stays pinned there for as long as it runs", n)
	}
}

// handoverFleet hands a pin over to the row that replaces it — the row first,
// then the release, the order every holder follows — in the instant after the
// trim's FIRST read of the register, whichever read that is.
type handoverFleet struct {
	fleetRegister
	once     sync.Once
	handover func()
}

func (f *handoverFleet) Positions(ctx context.Context) ([]coord.NodePositions, error) {
	rows, err := f.fleetRegister.Positions(ctx)
	f.once.Do(f.handover)
	return rows, err
}

func (f *handoverFleet) Holds(ctx context.Context) ([]coord.TrimHold, error) {
	holds, err := f.fleetRegister.Holds(ctx)
	f.once.Do(f.handover)
	return holds, err
}

// A HANDOVER BETWEEN THE TRIM'S TWO READS STILL KEEPS THE TAIL.
//
// A joiner writes the row that counts it and only then releases its pin. The
// trim read the positions register first and the holds second, so a handover
// that landed between them was in neither read — no row yet, no pin any more —
// and at boot no presence lease counted the joiner either: the tick saw only
// the donor, a thousand records on, and could purge the tail the joiner had
// just installed itself in front of. Holds read first, a pin the tick misses
// was released before that read began, so the row that replaced it is in the
// positions read that follows.
func TestATrimTickSeesAJoinsPinOrTheRowThatReplacedIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	register := memory.NewFleet()
	name, stream := worklet.Domain{}.Name(), worklet.Domain{}.Stream().Name
	row := func(node string, seq uint64) coord.NodePositions {
		return coord.NodePositions{NodeID: node, At: time.Now().UTC(),
			Domains: map[string]coord.DomainPosition{name: {Generation: 1, Seq: seq}}}
	}
	if err := register.PutPositions(ctx, row("node-donor", 1000)); err != nil {
		t.Fatalf("publish the donor's row: %v", err)
	}
	owner := coord.HoldOwner("node-join", joinHoldPurpose)
	if err := register.PutHold(ctx, coord.TrimHold{Owner: owner, At: time.Now().UTC(),
		Streams: map[string]coord.Position{stream: {Stream: stream, Generation: 1, Seq: 100}},
	}); err != nil {
		t.Fatalf("pin the joiner's tail: %v", err)
	}
	fleet := &handoverFleet{fleetRegister: register, handover: func() {
		if err := register.PutPositions(ctx, row("node-join", 100)); err != nil {
			t.Errorf("publish the joiner's row: %v", err)
		}
		if err := register.ReleaseHold(ctx, owner); err != nil {
			t.Errorf("release the joiner's pin: %v", err)
		}
	}}

	in, err := (&retention{fleet: fleet, nodeID: "node-a"}).read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	inputs := statelog.TrimInputs{
		Generation: 1, Now: in.at,
		Counted:         statelog.CountedSet(in.at, reportedPositions(in.positions, name), nil, nil),
		CountedReadable: in.readable,
		Holds:           holdsFor(in.holds, stream), HoldsReadable: true,
		HoldStale: statelog.TrimHoldStale,
	}
	kept := statelog.SeqUnbounded
	for _, term := range inputs.Terms() {
		if term.Name != statelog.TermApplied && term.Name != statelog.TermMinHold {
			continue
		}
		if !term.Known {
			t.Fatalf("the %s term is unknown: %s", term.Name, term.Detail)
		}
		kept = min(kept, term.Seq)
	}
	if kept > 100 {
		t.Fatalf("the tick's applied and min_hold terms let it remove through %d, "+
			"past the joiner's 100: it read the register on both sides of the "+
			"handover and saw neither the joiner's pin nor the row that replaced it",
			kept)
	}
}

// registerTape is the fleet's register with a tape, in the order the register
// took them, of what one node did there: its join's pin put, a positions row
// of its own written, its join's pin released. A pin and a row are taped once
// the register has taken them, a release as it is asked for — so a row taped
// before a release was written before the release began.
type registerTape struct {
	fleetRegister
	node string

	mu   sync.Mutex
	tape []tapeEntry
}

// tapeEntry is one step on a [registerTape]: "pin", "row" or "release", and
// the row a "row" wrote.
type tapeEntry struct {
	op  string
	row coord.NodePositions
}

// String names the step, and a row by where it puts the tracker — all a
// failure needs to show.
func (e tapeEntry) String() string {
	if e.op != "row" {
		return e.op
	}
	at := e.row.Domains[worklet.Domain{}.Name()]
	return fmt.Sprintf("row(tracker %d@%d)", at.Seq, at.Generation)
}

func (r *registerTape) note(e tapeEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tape = append(r.tape, e)
}

func (r *registerTape) entries() []tapeEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.tape)
}

func (r *registerTape) PutHold(ctx context.Context, h coord.TrimHold) error {
	err := r.fleetRegister.PutHold(ctx, h)
	if err == nil && h.Owner == coord.HoldOwner(r.node, joinHoldPurpose) {
		r.note(tapeEntry{op: "pin"})
	}
	return err
}

func (r *registerTape) ReleaseHold(ctx context.Context, owner string) error {
	if owner == coord.HoldOwner(r.node, joinHoldPurpose) {
		r.note(tapeEntry{op: "release"})
	}
	return r.fleetRegister.ReleaseHold(ctx, owner)
}

func (r *registerTape) PutPositions(ctx context.Context, p coord.NodePositions) error {
	err := r.fleetRegister.PutPositions(ctx, p)
	if err == nil && p.NodeID == r.node {
		r.note(tapeEntry{op: "row", row: p})
	}
	return err
}

// A BOOT'S JOIN KEEPS ITS PIN UNTIL THE NODE'S FIRST ROW COUNTS IT.
//
// The exposed path: a node that boots below the floor adopts before it holds a
// presence lease and before its heartbeat has written a row, so between the
// install and that first row nothing in the fleet counts it but its own pin. A
// pin released as the join returned left exactly that interval open to a trim
// tick, which could remove the tail the node had just installed itself in
// front of. So the same node boots a second time, below the floor, with a
// donor waiting, and the register's tape must show the join's pin put, then a
// row of the node's own naming the tracker at no more than the artefact, and
// only then the pin's release — which must come.
//
// THE ORDER ON THE TAPE rather than the register at the instant New returns,
// because the heartbeat publishes at once as the state log starts, before New
// returns: by then a correct boot has usually released the pin already, and a
// snapshot of the register would pass or fail on the race.
func TestABootJoinKeepsItsPinUntilItsFirstRowCountsIt(t *testing.T) {
	t.Parallel()
	n := openRejoinNode(t)
	e := n.start(t)
	presentAsDataNode(t, e, e.id)
	waitUntil(t, 20*time.Second, "the node to admit seats",
		func() bool { return e.StateLogHydrated(t.Context()) })
	quietHeartbeat(e.core.Load().log)
	running, at, last := pushBelowTheFloor(t, e, n.q)
	rows := filepath.Join(t.TempDir(), "crewlet-replicated.db")
	copyAdvancedTo(t, n.back, running, at, last, rows)
	standUpDonor(t, e, n.q, rows, statelog.Position{
		Stream: at.Stream, Generation: at.Generation, Seq: last,
	}, running.runner.KeyedTo())
	e.Stop(context.Background())

	tape := &registerTape{fleetRegister: n.back.Fleet, node: e.id}
	n.back.Fleet = tape
	again := n.start(t)
	if got := trackerCheckpoint(t, again); got.Seq < last {
		t.Fatalf("the boot left the tracker at %d, want the artefact's %d — it "+
			"did not adopt, so there was no pin to keep", got.Seq, last)
	}
	waitUntil(t, 3*statelog.PositionHeartbeat, "the boot join's pin to be released",
		func() bool {
			return slices.ContainsFunc(tape.entries(),
				func(x tapeEntry) bool { return x.op == "release" })
		})

	entries := tape.entries()
	pinned := slices.IndexFunc(entries, func(x tapeEntry) bool { return x.op == "pin" })
	released := slices.IndexFunc(entries, func(x tapeEntry) bool { return x.op == "release" })
	if pinned < 0 || pinned > released {
		t.Fatalf("the tape reads %v: the boot's join released a pin it never put", entries)
	}
	name := worklet.Domain{}.Name()
	counted := slices.ContainsFunc(entries[pinned:released], func(x tapeEntry) bool {
		pos, named := x.row.Domains[name]
		return x.op == "row" && named && pos.Generation == at.Generation && pos.Seq <= last
	})
	if !counted {
		t.Fatalf("the tape reads %v: the join's pin was released before a row of "+
			"this node's own named the tracker at or below the artefact's %d — "+
			"between the two, nothing in the fleet counted the tail it replays",
			entries, last)
	}
	if held := len(joinHolds(t, n.back.Fleet, e.id)); held != 0 {
		t.Fatalf("%d join pin(s) still in the register after the release", held)
	}
}
